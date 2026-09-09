// Command mock-llm 是 e2e 测试的回声上游：把收到的内容再原样返回（让网关层
// 检测「脱敏后向上游发什么」、「还原后向客户端发什么」两个语义都被覆盖）。
//
// 端点覆盖：
//
//	POST /v1/chat/completions  OpenAI 兼容；支持 stream=true 返回 SSE
//	POST /v1/completions       OpenAI 兼容
//	POST /v1/embeddings        OpenAI 兼容
//	POST /v1/responses         OpenAI 兼容
//	POST /v1/messages          Anthropic 兼容
//	POST /v1/models            列举
//	GET  /_received            回显 server 收到的最近一条原始请求体（--record）
//	GET  /healthz              健康
//
// 行为约定：
//
//   - 默认把请求体中的 content/text/input/prompt 字段提取出来，作为回复 content。
//   - SSE 流（chat）把回复切成 token 段推送。
//   - Anthropic /v1/messages 同样回声 content，格式对齐 Anthropic 响应。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// requestRecord 记录最近一次收到的请求，供 e2e.sh 断言「网关层发了什么」。
type requestRecord struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
	At     time.Time       `json:"at"`
}

func main() {
	var (
		listen       = flag.String("listen", ":8999", "mock LLM listen address")
		record       = flag.Bool("record", false, "record last request body to /_received")
		recordFile   = flag.String("record-file", "", "append-mode file path to record every received body (JSON lines)")
		dumpRequest  = flag.Bool("dump-headers", false, "echo all request headers in response")
	)
	flag.Parse()

	var (
		lastMu sync.RWMutex
		last   requestRecord
		reqSeq atomic.Int64
		f      *os.File
	)
	if *recordFile != "" {
		var err error
		f, err = os.OpenFile(*recordFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("[mock-llm] open record-file: %v", err)
		}
		defer f.Close()
	}

	commonHandler := func(label string, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if *record {
			lastMu.Lock()
			last = requestRecord{Method: r.Method, Path: r.URL.Path, Body: json.RawMessage(body), At: time.Now()}
			lastMu.Unlock()
		}
		if f != nil {
			seq := reqSeq.Add(1)
			_, _ = fmt.Fprintf(f, `{"seq":%d,"path":%q,"body":%s}`+"\n", seq, r.URL.Path, body)
		}
		log.Printf("[mock-llm] %s %s body=%d bytes", label, r.URL.Path, len(body))

		switch r.URL.Path {
		case "/v1/chat/completions":
			handleChat(w, r, body, *dumpRequest)
		case "/v1/completions":
			handleCompletion(w, body)
		case "/v1/embeddings":
			handleEmbedding(w, body)
		case "/v1/responses":
			handleResponses(w, body)
		case "/v1/messages":
			handleAnthropic(w, r, body)
		case "/v1/models":
			handleModels(w)
		default:
			http.Error(w, "not found: "+r.URL.Path, http.StatusNotFound)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/_received", func(w http.ResponseWriter, r *http.Request) {
		lastMu.RLock()
		defer lastMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(last)
	})
	mux.HandleFunc("/_received/all", func(w http.ResponseWriter, r *http.Request) {
		// 永远返回当前 latest；reset=1 时清空
		if r.URL.Query().Get("reset") == "1" {
			lastMu.Lock()
			last = requestRecord{}
			lastMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		lastMu.RLock()
		_ = json.NewEncoder(w).Encode(last)
		lastMu.RUnlock()
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		commonHandler("recv", w, r)
	})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("[mock-llm] listening on %s record=%v file=%q", *listen, *record, *recordFile)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// extractContent 递归从 OpenAI/Anthropic 请求体里抠出第一个 content/text/input/prompt 字符串。
// 用于回显生成。找到就停。
func extractContent(raw json.RawMessage) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return findString(v)
}

func findString(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]interface{}:
		// 优先看几个字段顺序
		prio := []string{"content", "text", "input", "prompt"}
		for _, k := range prio {
			if s := findString(x[k]); s != "" {
				return s
			}
		}
		// 其它字段按 key 顺序
		for _, k := range []string{"messages", "input", "system"} {
			if s := findString(x[k]); s != "" {
				return s
			}
		}
		for _, vv := range x {
			if s := findString(vv); s != "" {
				return s
			}
		}
	case []interface{}:
		for _, vv := range x {
			if s := findString(vv); s != "" {
				return s
			}
		}
	}
	return ""
}

// writeJSON 安全写 JSON 响应，关闭 HTML 转义（占位符必须字面保留）。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// jsonLiteral 序列化为单行 JSON 字面（关闭 HTML 转义、去掉尾部换行）。
// 注意：json.Marshal 会把 < > 转成 \u003c \u003e，占位符 <<...>> 会被破坏。
func jsonLiteral(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// handleChat OpenAI /v1/chat/completions。
// stream=false: 一次性返回 {choices:[{message:{role:"assistant", content:回显}}]}
// stream=true:  返回 SSE 事件序列，每行一段增量 + DONE
func handleChat(w http.ResponseWriter, r *http.Request, body []byte, dumpHeaders bool) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	echo := extractContent(body)
	if echo == "" {
		echo = "(empty)"
	}
	id := fmt.Sprintf("chatcmpl-mock-%d", time.Now().UnixNano())

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 切 token 流：逐字 + 模拟跨块
		tokens := chunkTokens(echo, 8)
		for _, t := range tokens {
			evt := map[string]interface{}{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   req.Model,
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]string{"content": t}, "finish_reason": ""},
				},
			}
			// 标准 SSE 帧：data: <json>\n\n（此前这里只写裸 JSON 行，
			// 与真实上游不一致，导致 E3 测的是假场景）
			b, err := jsonLiteral(evt)
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_ = dumpHeaders
		return
	}

	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": echo}, "finish_reason": "stop"},
		},
		"usage": map[string]int{"prompt_tokens": len(echo), "completion_tokens": len(echo), "total_tokens": len(echo) * 2},
	}
	writeJSON(w, http.StatusOK, resp)
}

// chunkTokens 朴素分词：按 rune 切块 size，每块作为一个 SSE delta。
func chunkTokens(s string, size int) []string {
	if size <= 0 {
		size = 1
	}
	out := make([]string, 0, (len([]rune(s))+size-1)/size)
	runes := []rune(s)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

func handleCompletion(w http.ResponseWriter, body []byte) {
	var req struct {
		Model string `json:"model"`
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &req)
	echo := req.Prompt
	if echo == "" {
		echo = "(empty)"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-mock-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{"text": echo, "index": 0, "finish_reason": "stop"},
		},
	})
}

func handleEmbedding(w http.ResponseWriter, body []byte) {
	var req struct {
		Input interface{} `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	items := []map[string]interface{}{}
	if arr, ok := req.Input.([]interface{}); ok {
		items = make([]map[string]interface{}, len(arr))
		for i, x := range arr {
			items[i] = map[string]interface{}{"object": "embedding", "index": i, "embedding": []float64{0.1, 0.2, 0.3}}
			_ = x
		}
	} else {
		items = []map[string]interface{}{{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list", "data": items, "model": "mock-embed", "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
	})
}

func handleResponses(w http.ResponseWriter, body []byte) {
	echo := extractContent(body)
	if echo == "" {
		echo = "(empty)"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":     fmt.Sprintf("resp-mock-%d", time.Now().UnixNano()),
		"object": "response",
		"output": []map[string]interface{}{
			{"role": "assistant", "content": []map[string]interface{}{{"type": "output_text", "text": echo}}},
		},
	})
}

// handleAnthropic /v1/messages 回声消息 content，支持 stream=true（SSE event:content_block_delta）。
func handleAnthropic(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Messages []struct {
			Role  string `json:"role"`
			Content interface{} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	echo := extractContent(body)
	if echo == "" {
		echo = "(empty)"
	}
	id := fmt.Sprintf("msg-mock-%d", time.Now().UnixNano())
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Anthropic-Version", "2023-06-01")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		writeAnthropicSSE(w, flusher, id, req.Model, chunkTokens(echo, 8))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         id,
		"type":       "message",
		"role":       "assistant",
		"model":      req.Model,
		"stop_reason": "end_turn",
		"content":    []map[string]interface{}{{"type": "text", "text": echo}},
	})
}

func writeAnthropicSSE(w http.ResponseWriter, flusher http.Flusher, id, model string, tokens []string) {
	send := func(event string, data interface{}) {
		b, _ := jsonLiteral(data) // json.Marshal 会转义 < >，破坏占位符
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": id, "type": "message", "role": "assistant", "model": model}})
	send("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	for _, t := range tokens {
		send("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": t},
		})
		time.Sleep(5 * time.Millisecond)
	}
	send("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}})
	send("message_stop", map[string]interface{}{"type": "message_stop"})
}

func handleModels(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data": []map[string]string{
			{"id": "mock-1", "object": "model", "owned_by": "mock"},
			{"id": "mock-2", "object": "model", "owned_by": "mock"},
		},
	})
}

// 保留 bufio 以备未来按行探测；当前未直接使用。
var _ = bufio.NewScanner
var _ = strings.HasPrefix
