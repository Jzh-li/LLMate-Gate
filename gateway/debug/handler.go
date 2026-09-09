package debug

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gateway/internal/config"
	"gateway/internal/detector"
	"gateway/internal/replacer"
	"gateway/pkg/types"
)

//go:embed assets/*
var assetsFS embed.FS

// Assets FS 返回内嵌的静态资源（测试可用）。
func Assets() embed.FS { return assetsFS }

// Handler 调试面板路由聚合（契约 §10 / UI设计 §0-4）。
type Handler struct {
	hub      *Hub
	store    *TrafficRecordStore
	cfg      *config.Config
	det      detector.Client
	repl     replacer.Replacer
	// 规则热加载钩子（外部设置）：PUT /_api/rules 时调用
	ruleHook func(strategy string) error
	rulesMu  sync.Mutex
}

// NewHandler 构造 debug Handler；ruleHook 可选（nil 时 /_api/rules 不可用）。
func NewHandler(cfg *config.Config, det detector.Client, repl replacer.Replacer, hub *Hub, store *TrafficRecordStore, ruleHook func(string) error) *Handler {
	return &Handler{
		hub:      hub,
		store:    store,
		cfg:      cfg,
		det:      det,
		repl:     repl,
		ruleHook: ruleHook,
	}
}

// Mount 把 debug 路由挂到 mux（受 --no-debug 门控由调用方决定）。
//
// 路由：
//   GET    /_debug         → HTML（embed）
//   GET    /_debug/...     → 静态资源（JS/CSS）
//   WS     /ws/events      → 流量事件推送
//   GET    /_api/traffic   → 拉取环形缓冲
//   DELETE /_api/traffic   → 清空
//   POST   /_api/detect    → Playground: 仅检测
//   POST   /_api/replace   → Playground: 检测 + 替换
//   GET    /_api/rules     → 当前规则
//   PUT    /_api/rules     → 更新策略（热加载）
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/_debug", h.serveIndex)
	mux.HandleFunc("/_debug/", h.serveAsset)
	mux.HandleFunc("/ws/events", h.serveWS)
	mux.HandleFunc("/_api/traffic", h.handleTraffic)
	mux.HandleFunc("/_api/detect", h.handleDetect)
	mux.HandleFunc("/_api/replace", h.handleReplace)
	mux.HandleFunc("/_api/rules", h.handleRules)
}

// ---------- handlers ----------

// serveIndex 返回面板 HTML（embed）。
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/_debug" {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板不缓存，便于开发期热更新（UI设计 §1）。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveAsset 返回内嵌静态文件（JS/CSS）。
func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[len("/_debug/"):]
	if name == "" {
		http.NotFound(w, r)
		return
	}
	// 简单安全：禁止路径穿越（embed FS 本身已禁止 ..）
	if strings.Contains(name, "..") || strings.ContainsAny(name, "\\") {
		http.NotFound(w, r)
		return
	}
	data, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "text/plain; charset=utf-8"
	switch {
	case len(name) >= 3 && name[len(name)-3:] == ".js":
		ctype = "application/javascript; charset=utf-8"
	case len(name) >= 4 && name[len(name)-4:] == ".css":
		ctype = "text/css; charset=utf-8"
	case len(name) >= 5 && name[len(name)-5:] == ".html":
		ctype = "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// serveWS 把 HTTP 升级为 WebSocket。
func (h *Handler) serveWS(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		http.Error(w, "hub not configured", http.StatusServiceUnavailable)
		return
	}
	unsub, err := h.hub.Subscribe(w, r)
	if err != nil {
		// Upgrade 已写入响应，只能记日志
		log.Printf("[debug] ws upgrade failed: %v", err)
		return
	}
	_ = unsub // readPump 负责调用
}

// handleTraffic GET/DELETE 流量缓冲。
func (h *Handler) handleTraffic(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		// 注意：占位符 << / >> 在 JSON 序列化时默认转义为 \u003c/\u003e。
		// 用 json.Encoder + SetEscapeHTML(false) 序列化 records，让占位符字面保留。
		var recBuf bytes.Buffer
		recEnc := json.NewEncoder(&recBuf)
		recEnc.SetEscapeHTML(false)
		if err := recEnc.Encode(h.store.Snapshot()); err != nil {
			writeErr(w, http.StatusInternalServerError, "marshal_failed", err.Error())
			return
		}
		// recEnc.Encode 末尾会加 \n，去掉它
		recJSON := bytes.TrimRight(recBuf.Bytes(), "\n")
		body := fmt.Sprintf(`{"records":%s,"size":%d,"cap":%d}`, recJSON, h.store.Len(), h.store.Cap())
		_, _ = w.Write([]byte(body))
	case http.MethodDelete:
		h.store.Clear()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// detectReq POST /_api/detect 入参（契约 §10.3）。
type detectReq struct {
	Text     string   `json:"text"`
	Entities []string `json:"entities,omitempty"`
}

// detectResp POST /_api/detect 出参。
type detectResp struct {
	Entities  []entityResp `json:"entities"`
	LatencyMs int64        `json:"latency_ms"`
	Error     string       `json:"error,omitempty"`
}

type entityResp struct {
	Type       string  `json:"type"`
	Value      string  `json:"value"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Confidence float64 `json:"confidence"`
}

// handleDetect Playground: 纯本地检测（不出站，UI设计 §3.3）。
func (h *Handler) handleDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req detectReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text is empty")
		return
	}
	// 限流：单次文本不超过 64KB（Playground 是手动测试，不是生产流量）
	if len(req.Text) > 64*1024 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text too large (max 64KB)")
		return
	}
	// 直接用主 detector；detector.Client 接口未暴露运行时 entity filter，
	// 因此 Playground 的 entities 字段在本版本仅作为占位保留（文档说明）。
	_ = req.Entities

	start := time.Now()
	resp, err := h.det.Detect(r.Context(), &types.DetectRequest{Text: req.Text})
	latency := time.Since(start).Milliseconds()

	out := detectResp{LatencyMs: latency}
	if err != nil {
		out.Error = err.Error()
	} else {
		for _, e := range resp.Entities {
			out.Entities = append(out.Entities, entityResp{
				Type:       e.Type,
				Value:      e.Value,
				Start:      e.Start,
				End:        e.End,
				Confidence: float64(e.Score),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// replaceReq POST /_api/replace 入参（契约 §10.3）。
type replaceReq struct {
	Text     string   `json:"text"`
	Strategy string   `json:"strategy,omitempty"` // placeholder | simulate
	Entities []string `json:"entities,omitempty"`
}

// replaceResp POST /_api/replace 出参。
type replaceResp struct {
	Replaced string         `json:"replaced"`
	Mapping  []MappingEntry `json:"mapping"`
	Strategy string         `json:"strategy"`
}

// handleReplace Playground: 纯本地检测 + 替换（不出站，UI设计 §3.3）。
//
// 不写 vault：Playground 数据不落盘（UI设计 §3.3 安全约束）。
func (h *Handler) handleReplace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req replaceReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text is empty")
		return
	}
	if len(req.Text) > 64*1024 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "text too large (max 64KB)")
		return
	}
	if req.Strategy != "" && req.Strategy != "placeholder" && req.Strategy != "simulate" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "strategy must be placeholder|simulate")
		return
	}
	_ = req.Entities // 同 handleDetect

	resp, err := h.det.Detect(r.Context(), &types.DetectRequest{Text: req.Text})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "detector_failed", err.Error())
		return
	}

	rr, err := h.repl.Replace(r.Context(), &replacer.ReplaceRequest{
		Text:           req.Text,
		ConversationID: "playground",
		Entities:       resp.Entities,
		Strategy:       req.Strategy,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "replace_failed", err.Error())
		return
	}

	out := replaceResp{
		Replaced: rr.Text,
		Mapping:  make([]MappingEntry, 0, len(rr.Entries)),
		Strategy: h.repl.Strategy(),
	}
	for _, e := range rr.Entries {
		out.Mapping = append(out.Mapping, MappingEntry{
			Placeholder: e.Sentinel(),
			Type:        e.EntityType,
			Value:       string(e.Original),
		})
	}
	// 关键：与 proxy.go 一致，关闭 HTML 转义，否则占位符的 << / >> 会被编码为 \u003c/\u003e。
	w.Header().Set("Content-Type", "application/json")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
	_, _ = w.Write(buf.Bytes())
}

// rulesResp GET /_api/rules 出参。
type rulesResp struct {
	Strategy     string   `json:"strategy"`
	Irreversible []string `json:"irreversible"`
}

// rulesReq PUT /_api/rules 入参（只支持改 strategy，irreversible 需重启）。
type rulesReq struct {
	Strategy string `json:"strategy"`
}

// handleRules GET 读取当前策略；PUT 热更新 strategy。
func (h *Handler) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		strategy := "placeholder"
		if h.repl != nil {
			strategy = h.repl.Strategy()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rulesResp{
			Strategy:     strategy,
			Irreversible: h.cfg.Replacement.Irreversible,
		})
	case http.MethodPut:
		var req rulesReq
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Strategy != "placeholder" && req.Strategy != "simulate" {
			writeErr(w, http.StatusBadRequest, "invalid_request", "strategy must be placeholder|simulate")
			return
		}
		h.rulesMu.Lock()
		hook := h.ruleHook
		h.rulesMu.Unlock()
		if hook == nil {
			writeErr(w, http.StatusNotImplemented, "not_implemented", "rule hot-reload not wired")
			return
		}
		if err := hook(req.Strategy); err != nil {
			writeErr(w, http.StatusInternalServerError, "rule_apply_failed", err.Error())
			return
		}
		// 广播 rule.changed（UI设计 §3 / §4.2）
		h.hub.Publish(string(EventRuleChanged), map[string]string{"strategy": req.Strategy})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- helpers ----------

// decodeJSON 读 body 并解析；限制最大 1MB。
func decodeJSON(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// writeErr 写统一 JSON 错误（与 server.writeError 风格一致）。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"code": code, "message": msg},
	})
}