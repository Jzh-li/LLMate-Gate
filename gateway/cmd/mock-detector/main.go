// Command mock-detector 是 PII Engineer sidecar 的替身：实现契约 §2.1 的 HTTP
// 接口，供 e2e 测试触发「detector 超时/不可用」分支，验证代理 fail-closed 行为。
//
// 实现细节：
//
//	POST /api/detect          返回 types.DetectResponse（--sleep 会阻塞该时长）
//	POST /api/detect/batch    顺序处理 N 个请求
//	GET  /healthz             返回 ok（除非 --fail 模式）
//
// 选项：
//
//	--listen=":18000"             监听地址
//	--sleep=0                     /api/detect 处理前 sleep N 秒（用于触发 502 detector_timeout）
//	--respond-fail                立即返回 500（用于触发 500 detector_unavailable）
//	--engine="mock-pii-engineer"  Name() 返回
//	--fixtures="fixtures/*.json"  可选：从 fixtures 目录加载预置样本（实体类型 → 起止偏移）
//
// 命令示例：
//
//	mock-detector --listen :18000 --sleep 5 &
//	mock-detector --listen :18000 --respond-fail &
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"gateway/pkg/types"
)

func main() {
	var (
		listen      = flag.String("listen", ":18000", "mock sidecar listen address")
		sleep       = flag.Duration("sleep", 0, "artificial processing delay per request (used to trigger timeout)")
		respondFail = flag.Bool("respond-fail", false, "immediately return 500 (used to trigger unavailable)")
		engineName  = flag.String("engine", "mock-pii-engineer", "engine name reported in Health")
	)
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if *respondFail {
			http.Error(w, "deliberately unhealthy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "engine": *engineName})
	})

	detect := func(w http.ResponseWriter, r *http.Request, reqs []*types.DetectRequest) {
		if *sleep > 0 {
			time.Sleep(*sleep)
		}
		if *respondFail {
			http.Error(w, "deliberate failure", http.StatusInternalServerError)
			return
		}
		results := make([]*types.DetectResponse, len(reqs))
		for i, req := range reqs {
			results[i] = &types.DetectResponse{
				Entities: regexSimulate(req.Text),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		// 关闭 HTML 转义：占位符/JSON 字段保持字面
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(results)
	}

	mux.HandleFunc("/api/detect", func(w http.ResponseWriter, r *http.Request) {
		var req types.DetectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		detect(w, r, []*types.DetectRequest{&req})
	})

	mux.HandleFunc("/api/detect/batch", func(w http.ResponseWriter, r *http.Request) {
		var reqs []*types.DetectRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		detect(w, r, reqs)
	})

	log.Printf("[mock-detector] listening on %s sleep=%v respond_fail=%v", *listen, *sleep, *respondFail)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// regexSimulate 用最简规则（在 sidecar 服务中也跑一遍只是为了 round-trip 通路，
// 真实检测逻辑由内置 regex 完成；网关层先 receive 真实实体，存在 cache 即可）。
//
// 这里我们只返回 nil entities —— 主要目的是「侧车存活但无附加 PII」以验证：
//  1. cache 与 sidecar 的 round-trip
//  2. 不同 sidecar 实现的协议一致性
//
// 如果要触发超时，--sleep 已足够；不需要真实内容。
func regexSimulate(text string) []types.Entity {
	_ = text
	out := make([]types.Entity, 0)
	// 可选：暴露最小 inspector（mask 样例里的身份证号）以确认 payload 路径打通
	for _, token := range strings.Fields(text) {
		if len(token) == 18 && allDigits(token) {
			out = append(out, types.Entity{Type: "zh_id_card", Value: token, Start: 0, End: len(token), Score: 0.99})
		}
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
