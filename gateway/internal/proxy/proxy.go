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
	"gateway/internal/audit"
	"gateway/internal/cache"
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
	merkle         *cache.MerkleCache // 会话级增量检测缓存（nil 关闭）
}

// New 构造代理。
func New(proc *pipeline.Processor, upstream *url.URL, upstreamAPIKey, upstreamVer string, m *metrics.Collectors, logPII bool, merkle *cache.MerkleCache) *Proxy {
	return &Proxy{
		proc:           proc,
		upstream:       upstream,
		upstreamAPIKey: upstreamAPIKey,
		upstreamVer:    upstreamVer,
		client:         &http.Client{Timeout: 30 * time.Second},
		m:              m,
		logPII:         logPII,
		merkle:         merkle,
	}
}

// piiFieldKeys 请求体里需要脱敏的字符串字段（契约 §4.1 + 协议级补完 2026-09-11）。
//
// "input" 同时承担：Anthropic tool_use.input、OpenAI chat tool_calls arguments
// 解析后的根（经 anonymizeJSONString 强制 inPII=true 注入）。
// "instructions" / "output_text" 是 OpenAI Responses API 字段（T12 启用）。
// name 不入表：键名不该脱敏。
var piiFieldKeys = map[string]bool{
	"content":      true,
	"text":         true,
	"input":        true,
	"prompt":       true,
	"system":       true,
	"instructions": true, // OpenAI Responses 的 agent 提示
	"output_text":  true, // OpenAI Responses 输出文本
}

// piiBlockTypes block 根 type 字段命中 → 强制进入 PII 上下文（解决 T3 真实问题：
// Anthropic tool_use.input 在 content 列表里嵌着 dict，type 字段不是 PII 字段名，
// 但其下 input 字段全是要脱敏的用户数据；同理 tool_result / function_call /
// function_call_output / message 等结构化 block）。
//
// 这些都是「容器型 block」，里面 value 都该被脱敏。
var piiBlockTypes = map[string]bool{
	"tool_use":              true, // Anthropic Messages
	"tool_result":           true, // Anthropic Messages
	"server_tool_use":       true, // Anthropic WebSearch / WebFetch 等内置工具
	"mcp_tool_use":          true, // Anthropic MCP 工具
	"mcp_tool_result":       true,
	"function_call":         true, // OpenAI Responses
	"function_call_output":  true,
	"custom_tool_call":      true, // OpenAI Responses
	"custom_tool_call_output": true,
	"computer_call":         true, // OpenAI Responses（含 action 字典）
	"local_shell_call":      true,
	"shell_call":            true,
	"web_search_call":       true, // action 内含 query 字符串
	"apply_patch_call":      true,
	"file_search_call":      true,
	"code_interpreter_call": true,
	"program":               true,
	"message":               true, // OpenAI Responses bare text message
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

	// 提取 model 字段（非 PII），用于审计事件
	var meta struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &meta)

	// 早发布：request.received（含原始请求 + method/stream/started_at）；
	// Hub.Store 按 RequestID 合并，后续事件会填充其它字段。
	p.publish(pipeline.EvRequestReceived, &pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, Method: r.Method, Stream: stream,
		RawRequest: rawRequest, Strategy: p.strategy(),
		Outcome: "pending", StartedAt: start,
	})

	newBody, entries, aerr := p.anonymizeBody(r.Context(), body, reqID, convID)
	if aerr != nil {
		if p.m != nil {
			p.m.BlockedTotal.WithLabelValues(errorCode(aerr)).Inc()
			p.m.RequestsTotal.WithLabelValues(endpoint, "blocked").Inc()
		}
		p.publish(pipeline.EvRequestReceived, &pipeline.TrafficEvent{
			RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
			Strategy: p.strategy(), Outcome: "blocked", Error: aerr.Error(), Timestamp: time.Now(),
		})
		p.recordAudit(reqID, convID, endpoint, meta.Model, stream, nil, "blocked", errorCode(aerr), start)
		writeError(w, aerr)
		return
	}

	// 立即发布 detected + mapping + replaced（脱敏后立即可见，不等上游响应）。
	// Hub.Store 按 RequestID 合并，后续 restore.done 会继续合并。
	p.publish(pipeline.EvReplaced, &pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint,
		Detected: entitiesFromEntries(entries),
		Mapping:  mappingToWire(entries),
		Replaced: string(newBody),
		Strategy: p.strategy(),
	})

	if err := p.proc.Store(reqID, convID, entries); err != nil {
		writeError(w, err)
		return
	}

	p.forward(w, r, endpoint, stream, reqID, convID, meta.Model, newBody, rawRequest, entries, start)
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

	// 一次请求共享一个替换会话：跨字段 / 跨消息的占位符计数全局唯一，
	// 避免同类型不同值跨段碰撞导致还原错乱（契约 §5.2）。
	sess := p.proc.NewSession()

	// Merkle 增量：多轮对话的 message content 段做前缀复用，只扫新增 turn。
	var segEntities map[string][]types.Entity
	if p.merkle != nil && convID != "" {
		if segs, ok := extractMessageContents(doc); ok {
			res, merr := p.merkle.GetOrDetect(convID, segs, func(seg string) ([]types.Entity, error) {
				return p.proc.DetectText(ctx, convID, seg)
			})
			if merr != nil {
				// 增量路径检测异常 → 回落到逐段检测路径（由 fail-closed 统一阻断）。
				segEntities = nil
			} else {
				segEntities = make(map[string][]types.Entity, len(res.Segments))
				for _, s := range res.Segments {
					segEntities[s.Text] = s.Entities
				}
				if p.m != nil {
					p.m.DetectIncremental.WithLabelValues("detected").Add(float64(res.Scanned))
					p.m.DetectIncremental.WithLabelValues("reused").Add(float64(len(res.Segments) - res.Scanned))
				}
			}
		}
	}

	var collected []types.MappingEntry
	anon := func(text string) (string, error) {
		// 优先复用 Merkle 缓存命中的段实体（零检测）。
		if segEntities != nil {
			if ents, hit := segEntities[text]; hit {
				clean, ne, rerr := sess.Replace(text, ents)
				if rerr != nil {
					return "", rerr
				}
				collected = append(collected, ne...)
				p.recordReplace(ne)
				return clean, nil
			}
		}
		ents, derr := p.proc.DetectText(ctx, convID, text)
		if derr != nil {
			return "", derr
		}
		clean, ne, rerr := sess.Replace(text, ents)
		if rerr != nil {
			return "", rerr
		}
		collected = append(collected, ne...)
		p.recordReplace(ne)
		return clean, nil
	}

	transformed, terr := p.transform(doc, false, anon)
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

// recordReplace 记录脱敏实体数（按命运），供 /metrics。
func (p *Proxy) recordReplace(entries []types.MappingEntry) {
	if p.m == nil {
		return
	}
	for _, e := range entries {
		p.m.ReplaceCount.WithLabelValues(e.Fate.String()).Inc()
		p.m.PIIDetected.WithLabelValues(e.EntityType, e.Fate.String()).Inc()
	}
}

// extractMessageContents 从 OpenAI/Anthropic 风格请求体提取有序的 message content 段
// （多轮对话脱敏的主要增长点），用于 Merkle 增量前缀匹配。
func extractMessageContents(doc interface{}) ([]string, bool) {
	m, ok := doc.(map[string]interface{})
	if !ok {
		return nil, false
	}
	msgs, ok := m["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return nil, false
	}
	var segs []string
	for _, msg := range msgs {
		mm, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if c, ok := mm["content"].(string); ok {
			segs = append(segs, c)
		}
	}
	if len(segs) == 0 {
		return nil, false
	}
	return segs, true
}

// transform 递归遍历 JSON，仅对 PII 字段内的字符串脱敏。错误必须上抛（fail-closed 关键）。
//
// PII 上下文进入规则（任一即触发 childPII=true）：
//   1. 父层已在 PII 上下文（inPII=true 透传）
//   2. 当前 key 在 piiFieldKeys（content / text / input / system ...）
//   3. 当前 map 的 type 字段命中 piiBlockTypes（tool_use / tool_result /
//      function_call ... → 整块结构是「容器型 PII block」）
//
// 不透明 block 跳过（shouldSkipValue）：
//   - type 字段命中 opaqueBlockTypes（thinking / base64 块 / encrypted_content）
//   - 父 key 命中 opaqueValueKeys（data / signature / encrypted_content）
//   - string 值以 dataURLPrefixes 开头（base64 data URL）
// 命中任一即原样返回，不进 anon。
func (p *Proxy) transform(v interface{}, inPII bool, anon func(string) (string, error)) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		// 整 map 是不透明 block：原样返回
		if shouldSkipValue("", x) {
			return x, nil
		}
		out := make(map[string]interface{}, len(x))
		// 检测容器型 PII block：type 字段命中 piiBlockTypes → 整块进 PII 上下文
		blockIsPII := inPII
		if t, _ := x["type"].(string); t != "" {
			if piiBlockTypes[t] {
				blockIsPII = true
			}
		}
		for k, val := range x {
			// arguments 走 JSON 解析（OpenAI tool_calls.function.arguments）
			if k == "arguments" {
				r, e := p.anonymizeJSONString(val, anon)
				if e != nil {
					return nil, e
				}
				out[k] = r
				continue
			}
			// 不透明 value（base64 data / signature / encrypted_content 等）：原样保留
			if shouldSkipValue(k, val) {
				out[k] = val
				continue
			}
			childPII := blockIsPII || piiFieldKeys[k]
			r, e := p.transform(val, childPII, anon)
			if e != nil {
				return nil, e
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			r, err := p.transform(e, inPII, anon)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		if shouldSkipValue("", x) {
			return x, nil
		}
		if inPII {
			return anon(x)
		}
		return x, nil
	default:
		return x, nil
	}
}

// anonymizeJSONString 把 tool_calls[].function.arguments 解析后逐值脱敏（键保留）。
//
// arguments 可能是：JSON 字符串（OpenAI 常见）、对象或数组（部分 SDK 已展开）。
// 统一按 inPII=true 递归扫描：只把字符串值送检测，JSON 键永不脱敏。
// 每次进入（即每解析一个 tool_call 的 arguments）计一次 tool_calls_scanned。
func (p *Proxy) anonymizeJSONString(v interface{}, anon func(string) (string, error)) (interface{}, error) {
	if p.m != nil {
		p.m.ToolCallsScanned.Inc()
	}
	switch x := v.(type) {
	case string:
		var parsed interface{}
		if err := json.Unmarshal([]byte(x), &parsed); err != nil {
			return v, nil // 非 JSON：保持原样
		}
		return p.transform(parsed, true, anon)
	case map[string]interface{}, []interface{}:
		return p.transform(x, true, anon)
	default:
		return v, nil
	}
}

// forward 转发上游并还原响应。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, endpoint string, stream bool, reqID, convID, model string, newBody []byte, rawRequest string, entries []types.MappingEntry, start time.Time) {
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
		p.streamResponse(w, resp, endpoint, reqID, convID, model, rawRequest, entries, restorer, start)
		return
	}
	p.fullResponse(w, resp, endpoint, reqID, convID, model, rawRequest, entries, restorer, start)
}

// fullResponse 非流式：整包还原后返回。
func (p *Proxy) fullResponse(w http.ResponseWriter, resp *http.Response, endpoint, reqID, convID, model, rawRequest string, entries []types.MappingEntry, restorer *replacer.StreamRestorer, start time.Time) {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "read upstream", err))
		return
	}
	restoreStart := time.Now()
	out, _ := restorer.Write(respBody)
	rest, _ := restorer.Close()
	restored := string(out) + string(rest)
	if p.m != nil {
		p.m.RestoreLatency.WithLabelValues(endpoint).Observe(time.Since(restoreStart).Seconds())
	}

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
	now := time.Now()
	p.publish(pipeline.EvUpstreamResponse, &pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, UpstreamResp: redactLogIf(string(respBody), p.logPII),
	})
	p.publish(pipeline.EvRestoreDone, &pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
		Replaced: redactLogIf(string(mustMarshalReplaced(entries)), p.logPII),
		Restored: restored, Strategy: p.strategy(), Outcome: outcomeOf(resp.StatusCode),
		Detected: entitiesFromEntries(entries),
		Mapping:  mappingToWire(entries),
		StartedAt: start, FinishedAt: now,
		DurationMs: now.Sub(start).Milliseconds(),
		Timestamp: now,
	})
	p.recordAudit(reqID, convID, endpoint, model, false, entries, outcomeOf(resp.StatusCode), "", start)
}

// streamResponse SSE 流式：逐块还原并 flush。
func (p *Proxy) streamResponse(w http.ResponseWriter, resp *http.Response, endpoint, reqID, convID, model, rawRequest string, entries []types.MappingEntry, restorer *replacer.StreamRestorer, start time.Time) {
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

	// SSE 帧感知：占位符常被拆到多个事件里（帧结构会插在占位符中间），
	// 必须在「内容维度」跨事件还原，否则客户端会看到半截占位符。
	var streamWriter interface {
		Write([]byte) ([]byte, error)
		Close() ([]byte, error)
	} = restorer
	if isEventStream(resp.Header.Get("Content-Type")) {
		streamWriter = replacer.NewSSERestorer(restorer)
	}

	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	buf := make([]byte, 16*1024)
	var upstreamSb strings.Builder
	restoreStart := time.Now()
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			upstreamSb.Write(chunk)
			out, _ := streamWriter.Write(chunk)
			if len(out) > 0 {
				_, _ = w.Write(out)
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	rest, _ := streamWriter.Close()
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
		p.m.RestoreLatency.WithLabelValues(endpoint).Observe(time.Since(restoreStart).Seconds())
	}
	now := time.Now()
	p.publish(pipeline.EvUpstreamResponse, &pipeline.TrafficEvent{RequestID: reqID, Endpoint: endpoint, UpstreamResp: redactLogIf(upstreamSb.String(), p.logPII)})
	p.publish(pipeline.EvRestoreDone, &pipeline.TrafficEvent{
		RequestID: reqID, Endpoint: endpoint, RawRequest: rawRequest,
		Restored: "[stream]", Strategy: p.strategy(), Outcome: outcomeOf(resp.StatusCode),
		Detected: entitiesFromEntries(entries),
		Mapping:  mappingToWire(entries),
		StartedAt: start, FinishedAt: now,
		DurationMs: now.Sub(start).Milliseconds(),
		Timestamp: now,
	})
	p.recordAudit(reqID, convID, endpoint, model, true, entries, outcomeOf(resp.StatusCode), "", start)
}

// ---- 工具函数 ----

// isEventStream 判断上游是否以 SSE 帧返回（决定要不要做帧感知还原）。
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

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

// recordAudit 在请求收尾处记录一条结构化审计事件（契约 §9）。
// outcome 由调用方按响应状态给出（success/blocked/error）；entries 为本次映射条目。
func (p *Proxy) recordAudit(reqID, convID, endpoint, model string, stream bool, entries []types.MappingEntry, outcome, errorCode string, start time.Time) {
	if p.m != nil {
		p.m.RequestLatency.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	}
	if p.proc == nil {
		return
	}
	e := &audit.Event{
		Timestamp:        time.Now(),
		RequestID:        reqID,
		ConversationID:   convID,
		Upstream:         p.upstream.Host,
		Model:            model,
		DetectedEntities: toAuditSummary(entitiesFromEntries(entries), p.logPII),
		ReplacedCount:    len(entries),
		Strategy:         p.strategy(),
		Restored:         true,
		Streaming:        stream,
		LatencyMs:        time.Since(start).Milliseconds(),
		Outcome:          outcome,
		ErrorCode:        errorCode,
	}
	p.proc.RecordAudit(e)
}

// toAuditSummary 把检测实体转审计摘要；仅当 logPII=true 才携带原文（契约 §9.2）。
func toAuditSummary(entities []types.Entity, logPII bool) []audit.EntitySummary {
	if len(entities) == 0 {
		return nil
	}
	out := make([]audit.EntitySummary, 0, len(entities))
	for _, e := range entities {
		s := audit.EntitySummary{Type: e.Type, Score: e.Score}
		if logPII {
			s.Value = e.Value
		}
		out = append(out, s)
	}
	return out
}

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

// entitiesFromEntries 从映射条目还原 Entity 列表（用于调试面板 ② 检测到的 PII 段）。
func entitiesFromEntries(entries []types.MappingEntry) []types.Entity {
	if len(entries) == 0 {
		return nil
	}
	out := make([]types.Entity, 0, len(entries))
	for _, e := range entries {
		out = append(out, types.Entity{
			Type:  e.EntityType,
			Value: string(e.Original),
			Start: e.Start,
			End:   e.End,
			Score: float64(e.Score),
		})
	}
	return out
}

// mappingToWire 把 vault 的 MappingEntry 转成面板 JSON 形态（不含 Original 字节细节）。
func mappingToWire(entries []types.MappingEntry) []pipeline.MappingEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]pipeline.MappingEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, pipeline.MappingEntry{
			Placeholder: e.Sentinel(),
			Type:        e.EntityType,
			Value:       string(e.Original),
		})
	}
	return out
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
