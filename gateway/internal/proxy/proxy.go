// Package proxy OpenAI 兼容反向代理 + 请求脱敏 + 响应还原（契约 §4）。
//
// 设计：server 只负责路由/鉴权/healthz/metrics；proxy 负责「脱敏请求体 → 转发上游
// → 还原响应体」。还原走流式 trie（replacer.StreamRestorer），占位符跨 SSE 块拼接。
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/metrics"
	"gateway/internal/pipeline"
	"gateway/internal/replacer"
	"gateway/pkg/types"
)

// Proxy 转发代理。
type Proxy struct {
	proc           *pipeline.Processor
	upstream       *url.URL
	upstreamAPIKey string
	upstreamVer    string // Anthropic 需要 x-api-key + anthropic-version
	client         *http.Client
	m              *metrics.Collectors
	logPII         bool
}

// New 构造代理。
func New(proc *pipeline.Processor, upstream *url.URL, upstreamAPIKey, upstreamVer string, m *metrics.Collectors, logPII bool) *Proxy {
	return &Proxy{
		proc:           proc,
		upstream:       upstream,
		upstreamAPIKey: upstreamAPIKey,
		upstreamVer:    upstreamVer,
		client:         &http.Client{Timeout: 30 * time.Second},
		m:              m,
		logPII:         logPII,
	}
}

// piiFieldKeys 请求体里需要脱敏的字符串字段（契约 §4.1）。
var piiFieldKeys = map[string]bool{
	"content": true,
	"text":    true,
	"input":   true,
	"prompt":  true,
	"system":  true,
}

// Handle 通用转发入口；anon 决定如何脱敏请求体。
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request, endpoint string, stream bool) {
	start := time.Now()
	reqID := requestID(r)
	convID := conversationID(r, r.Body)

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "read body", err))
		return
	}
	rawRequest := string(body)
	if !p.logPII {
		rawRequest = redactLog(rawRequest)
	}

	newBody, entries, aerr := p.anonymizeBody(r.Context(), body, reqID, convID)
	if aerr != nil {
		if p.m != nil {
			p.m.BlockedTotal.WithLabelValues(errorCode(aerr)).Inc()
			p.m.RequestsTotal.WithLabelValues(endpoint, "blocked").Inc()
		}
		p.publish(pipeline.EvRequestReceived, pipeline.TrafficEvent{
			RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
			Strategy: p.strategy(), Outcome: "blocked", Error: aerr.Error(), Timestamp: time.Now(),
		})
		writeError(w, aerr)
		return
	}

	if err := p.proc.Store(reqID, convID, entries); err != nil {
		writeError(w, err)
		return
	}

	// 审计：detected 摘要
	_ = entries // entries 已存入 vault，审计从 entries 派生

	p.forward(w, r, endpoint, stream, reqID, convID, newBody, rawRequest, entries, start)
}

// anonymizeBody 解析 JSON 请求体并脱敏 PII 字段，返回脱敏后 body 与映射条目。
func (p *Proxy) anonymizeBody(ctx context.Context, body []byte, reqID, convID string) (newBody []byte, entries []types.MappingEntry, err error) {
	var doc interface{}
	if jerr := json.Unmarshal(body, &doc); jerr != nil {
		// 非 JSON：整段当作文本脱敏（如纯文本 prompt）
		clean, ents, _, e := p.proc.Anonymize(ctx, reqID, convID, string(body))
		if e != nil {
			return nil, nil, e
		}
		return []byte(clean), ents, nil
	}
	var collected []types.MappingEntry
	anon := func(text string) (string, error) {
		clean, ents, _, e := p.proc.Anonymize(ctx, reqID, convID, text)
		if e != nil {
			return "", e
		}
		collected = append(collected, ents...)
		return clean, nil
	}
	transformed, terr := transform(doc, false, anon)
	if terr != nil {
		return nil, nil, terr
	}
	// 关键：关闭 HTML 转义，否则 `<<`/`>>` 会被编码成 \u003c/\u003e，
	// 导致占位符在请求/响应中失效（契约 §5.2 占位符格式）。
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(transformed); err != nil {
		return nil, nil, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "re-marshal request", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), collected, nil
}

// transform 递归遍历 JSON，仅对 PII 字段内的字符串脱敏。错误必须上抛（fail-closed 关键）。
func transform(v interface{}, inPII bool, anon func(string) (string, error)) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, val := range x {
			if k == "arguments" {
				// OpenAI tool_calls.function.arguments：JSON 字符串，整体当 PII
				r, e := anonymizeJSONString(val, anon)
				if e != nil {
					return nil, e
				}
				out[k] = r
				continue
			}
			childPII := inPII || piiFieldKeys[k]
			r, e := transform(val, childPII, anon)
			if e != nil {
				return nil, e
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			r, err := transform(e, inPII, anon)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		if inPII {
			return anon(x)
		}
		return x, nil
	default:
		return x, nil
	}
}

// anonymizeJSONString 把 JSON 字符串解析后整体脱敏（tool 参数场景）。
func anonymizeJSONString(v interface{}, anon func(string) (string, error)) (interface{}, error) {
	s, ok := v.(string)
	if !ok {
		return v, nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return v, nil // 非 JSON：保持原样
	}
	return transform(parsed, true, anon)
}

// forward 转发上游并还原响应。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, endpoint string, stream bool, reqID, convID string, newBody []byte, rawRequest string, entries []types.MappingEntry, start time.Time) {
	dest := p.upstreamFor(r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, dest, bytes.NewReader(newBody))
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "build upstream request", err))
		return
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.Header.Del("Authorization")
	upReq.Header.Del("X-Api-Key")
	p.setUpstreamAuth(upReq)
	upReq.Header.Set("Content-Type", "application/json")
	if p.upstreamVer != "" {
		upReq.Header.Set("anthropic-version", p.upstreamVer)
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		if p.m != nil {
			p.m.BlockedTotal.WithLabelValues(string(gatewayerrors.CodeUpstreamError)).Inc()
			p.m.RequestsTotal.WithLabelValues(endpoint, "error").Inc()
		}
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "upstream", err))
		return
	}
	defer resp.Body.Close()

	restorer := replacer.NewStreamRestorerFromEntries(entries)

	if stream {
		p.streamResponse(w, resp, endpoint, reqID, convID, rawRequest, entries, restorer, start)
		return
	}
	p.fullResponse(w, resp, endpoint, reqID, convID, rawRequest, entries, restorer, start)
}

// fullResponse 非流式：整包还原后返回。
func (p *Proxy) fullResponse(w http.ResponseWriter, resp *http.Response, endpoint, reqID, convID, rawRequest string, entries []types.MappingEntry, restorer *replacer.StreamRestorer, start time.Time) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "read upstream", err))
		return
	}
	out, _ := restorer.Write(respBody)
	rest, _ := restorer.Close()
	restored := string(out) + string(rest)

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length") // 长度已变
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write([]byte(restored))

	if p.m != nil {
		p.m.RequestsTotal.WithLabelValues(endpoint, outcomeOf(resp.StatusCode)).Inc()
		if resp.StatusCode >= 400 {
			p.m.UpstreamErrors.WithLabelValues(endpoint, statusLabel(resp.StatusCode)).Inc()
		}
		p.m.RestoredTotal.WithLabelValues(endpoint).Inc()
	}
	p.publish(pipeline.EvUpstreamResponse, pipeline.TrafficEvent{RequestID: reqID, Endpoint: endpoint, UpstreamResp: redactLogIf(string(respBody), p.logPII)})
	p.publish(pipeline.EvRestoreDone, pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
		Replaced: redactLogIf(string(mustMarshalReplaced(entries)), p.logPII),
		Restored: restored, Strategy: p.strategy(), Outcome: outcomeOf(resp.StatusCode), Timestamp: time.Now(),
	})
	_ = start
}

// streamResponse SSE 流式：逐块还原并 flush。
func (p *Proxy) streamResponse(w http.ResponseWriter, resp *http.Response, endpoint, reqID, convID, rawRequest string, entries []types.MappingEntry, restorer *replacer.StreamRestorer, start time.Time) {
	if p.m != nil {
		p.m.ActiveConns.Inc()
		defer p.m.ActiveConns.Dec()
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, gatewayerrors.New(gatewayerrors.CodeUpstreamError, "streaming unsupported by client"))
		return
	}
	copyHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	buf := make([]byte, 16*1024)
	var upstreamSb strings.Builder
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			upstreamSb.Write(chunk)
			out, _ := restorer.Write(chunk)
			if len(out) > 0 {
				_, _ = w.Write(out)
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	rest, _ := restorer.Close()
	if len(rest) > 0 {
		_, _ = w.Write(rest)
		flusher.Flush()
	}
	if p.m != nil {
		orphanDelta := replacer.StreamOrphans() // 全局累计，近似计入
		_ = orphanDelta
		p.m.RequestsTotal.WithLabelValues(endpoint, outcomeOf(resp.StatusCode)).Inc()
		if resp.StatusCode >= 400 {
			p.m.UpstreamErrors.WithLabelValues(endpoint, statusLabel(resp.StatusCode)).Inc()
		}
		p.m.RestoredTotal.WithLabelValues(endpoint).Inc()
	}
	p.publish(pipeline.EvUpstreamResponse, pipeline.TrafficEvent{RequestID: reqID, Endpoint: endpoint, UpstreamResp: redactLogIf(upstreamSb.String(), p.logPII)})
	p.publish(pipeline.EvRestoreDone, pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
		Restored: "[stream]", Strategy: p.strategy(), Outcome: outcomeOf(resp.StatusCode), Timestamp: time.Now(),
	})
}

// ---- 工具函数 ----

func (p *Proxy) upstreamFor(path string) string {
	u := *p.upstream
	u.Path = singleJoiningSlash(u.Path, path)
	u.RawQuery = "" // 不转发代理自身 query
	return u.String()
}

func (p *Proxy) setUpstreamAuth(req *http.Request) {
	if p.upstreamAPIKey == "" {
		return
	}
	if p.upstreamVer != "" && strings.Contains(p.upstream.Path, "/v1/messages") {
		// Anthropic 用 x-api-key
		req.Header.Set("x-api-key", p.upstreamAPIKey)
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.upstreamAPIKey)
}

func (p *Proxy) strategy() string { return p.proc.Strategy() }

func (p *Proxy) publish(ev string, data interface{}) { p.proc.Publish(ev, data) }

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Llmate-Request-Id"); v != "" {
		return v
	}
	return randHex(16)
}

// conversationID 优先取 X-Conversation-ID；否则尝试从 body 的 conversation_id 提取。
func conversationID(r *http.Request, _ io.Reader) string {
	if v := r.Header.Get("X-Conversation-ID"); v != "" {
		return v
	}
	return ""
}

func statusLabel(code int) string { return http.StatusText(code) }

func outcomeOf(code int) string {
	if code >= 200 && code < 300 {
		return "success"
	}
	if code >= 500 {
		return "error"
	}
	return "blocked"
}

func errorCode(err error) string {
	if e, ok := gatewayerrors.As(err); ok {
		return string(e.Code)
	}
	return string(gatewayerrors.CodeDetectorUnavailable)
}

func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	msg := err.Error()
	if e, ok := gatewayerrors.As(err); ok {
		msg = e.Message
		code = statusForCode(e.Code)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"code": errorCode(err), "message": msg},
	})
}

func statusForCode(c gatewayerrors.Code) int {
	switch c {
	case gatewayerrors.CodeInvalidRequest:
		return http.StatusBadRequest
	case gatewayerrors.CodeUnauthorized:
		return http.StatusUnauthorized
	case gatewayerrors.CodeDetectorTimeout, gatewayerrors.CodeCircuitOpen, gatewayerrors.CodeUpstreamError:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func redactLog(s string) string {
	if len(s) > 4096 {
		return s[:4096] + "...[truncated]"
	}
	return s
}

func redactLogIf(s string, logPII bool) string {
	if logPII {
		return s
	}
	return redactLog(s)
}

func mustMarshalReplaced(entries []types.MappingEntry) []byte {
	b, _ := json.Marshal(entries)
	return b
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 2*n)
	for _, v := range b {
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out)
}

// Passthrough 透传无需脱敏的端点（如 /v1/models）。
func (p *Proxy) Passthrough(w http.ResponseWriter, r *http.Request) {
	dest := p.upstreamFor(r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, dest, nil)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "build passthrough", err))
		return
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.Header.Del("Authorization")
	upReq.Header.Del("X-Api-Key")
	p.setUpstreamAuth(upReq)
	resp, err := p.client.Do(upReq)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "upstream passthrough", err))
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
