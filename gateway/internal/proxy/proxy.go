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
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gateway/internal/audit"
	"gateway/internal/cache"
	gatewayerrors "gateway/internal/errors"
	"gateway/internal/judge"
	"gateway/internal/metrics"
	"gateway/internal/pipeline"
	"gateway/internal/replacer"
	"gateway/pkg/types"
)

// Upstream 描述一个协议上游的转发目标（配置驱动，见 config.UpstreamConfig）。
//
// 各家厂商适配：OpenAI 兼容上游用 Authorization: Bearer，Anthropic 兼容上游
// 用 x-api-key + anthropic-version。两者只是鉴权与 base URL 不同，转发/脱敏/还原共用一套。
type Upstream struct {
	URL        *url.URL
	APIKey     string
	APIVersion string // Anthropic anthropic-version；OpenAI 协议留空
	PathPrefix string // 替换入口 /v1 段（如智谱 /v4、DashScope /compatible-mode/v1）；空则透传 /v1
}

// Proxy 转发代理。
type Proxy struct {
	proc      *pipeline.Processor
	openai    *Upstream // 默认 OpenAI 兼容上游（必填）
	anthropic *Upstream // Anthropic 上游（可选；nil 时 /v1/messages 回退 openai）
	client    *http.Client
	m         *metrics.Collectors
	logPII    bool
	merkle    *cache.MerkleCache // 会话级增量检测缓存（nil 关闭）

	// judgment 行为判断层（契约 §12）。nil = 未启用，此时热路径上只有一次
	// 指针判空的开销。
	judgment *judge.Evaluator
	// judgmentMode 判断层模式。v1 只可能是 shadow —— 见 WithJudge 注释。
	judgmentMode string
}

// WithJudge 挂上行为判断层。
//
// 为什么用 setter 而不是往 New 的签名里加参数：New 已有 6 个参数，再加两个会
// 让所有调用点（server、一堆单测）跟着改，而判断层本身是**可选旁挂**能力，
// 没配它的部署不该被牵连。
//
// mode 在当前版本只有 shadow 一种合法值（配置校验会拒绝 enforce）：
// 判断结果**不进入拦截路径**——`observeToolCall` 只写指标与日志，不改变
// 请求体、不改写映射条目、不影响响应字节。
func (p *Proxy) WithJudge(e *judge.Evaluator, mode string) *Proxy {
	p.judgment = e
	p.judgmentMode = mode
	return p
}

// JudgeEnabled 判断层是否已挂上（装配层用它决定要不要打启动日志）。
func (p *Proxy) JudgeEnabled() bool { return p.judgment != nil }

// New 构造代理。openai 必填；anthropic 可选。
func New(proc *pipeline.Processor, openai, anthropic *Upstream, m *metrics.Collectors, logPII bool, merkle *cache.MerkleCache) *Proxy {
	return &Proxy{
		proc:      proc,
		openai:    openai,
		anthropic: anthropic,
		client:    &http.Client{Timeout: 30 * time.Second},
		m:         m,
		logPII:    logPII,
		merkle:    merkle,
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
	"tool_use":                true, // Anthropic Messages
	"tool_result":             true, // Anthropic Messages
	"server_tool_use":         true, // Anthropic WebSearch / WebFetch 等内置工具
	"mcp_tool_use":            true, // Anthropic MCP 工具
	"mcp_tool_result":         true,
	"function_call":           true, // OpenAI Responses
	"function_call_output":    true,
	"custom_tool_call":        true, // OpenAI Responses
	"custom_tool_call_output": true,
	"computer_call":           true, // OpenAI Responses（含 action 字典）
	"local_shell_call":        true,
	"shell_call":              true,
	"web_search_call":         true, // action 内含 query 字符串
	"apply_patch_call":        true,
	"file_search_call":        true,
	"code_interpreter_call":   true,
	"program":                 true,
	"message":                 true, // OpenAI Responses bare text message
}

// Handle 通用转发入口；anon 决定如何脱敏请求体。
// 本方法自行读取请求体，等价于 HandleWithBody(..., body)，保留它让单测与外部
// 调用方能用最朴素的方式驱动代理。
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request, endpoint string, stream bool) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "read body", err))
		return
	}
	p.HandleWithBody(w, r, endpoint, stream, body)
}

// HandleWithBody 以「已读出的请求体」驱动转发。
//
// server.handleLLM 必须先把 body 读出来才能判定 stream 字段；若这里再 io.ReadAll
// 一次，同一个请求就要在内存里完整拷贝两遍（大上下文请求下是实打实的开销与 GC 压力）。
// HandleWithBody 让上层把已读出的切片直接交进来，消除这次重复拷贝。
func (p *Proxy) HandleWithBody(w http.ResponseWriter, r *http.Request, endpoint string, stream bool, body []byte) {
	start := time.Now()
	reqID := requestID(r)
	convID := conversationID(r)

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
	// bypass：整条请求原样透传。不写映射表（entries 为空 → 响应侧还原器空转）。
	if p.proc.Strategy() == "bypass" {
		return body, nil, nil
	}
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

	// observe 是 P2 观测位的入口：transform 每遇到一层 map 都会调它一次，
	// 由 observeToolCall 自行判断这层是不是工具调用。闭包捕获 ctx/reqID/convID，
	// 这样 transform 本身不用背 4 个额外参数。
	observe := func(x map[string]interface{}) {
		p.observeToolCall(ctx, x, reqID, convID)
	}

	transformed, terr := p.transform(doc, false, anon, observe)
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
//  1. 父层已在 PII 上下文（inPII=true 透传）
//  2. 当前 key 在 piiFieldKeys（content / text / input / system ...）
//  3. 当前 map 的 type 字段命中 piiBlockTypes（tool_use / tool_result /
//     function_call ... → 整块结构是「容器型 PII block」）
//
// 不透明 block 跳过（shouldSkipValue）：
//   - type 字段命中 opaqueBlockTypes（thinking / base64 块 / encrypted_content）
//   - 父 key 命中 opaqueValueKeys（data / signature / encrypted_content）
//   - string 值以 dataURLPrefixes 开头（base64 data URL）
//
// 命中任一即原样返回，不进 anon。
func (p *Proxy) transform(v interface{}, inPII bool, anon func(string) (string, error), observe func(map[string]interface{})) (interface{}, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		// 整 map 是不透明 block：原样返回
		if shouldSkipValue("", x) {
			return x, nil
		}
		// P2 观测位：这层 map 若是一次工具调用，先做行为判定。
		// 旁挂——判定结果不参与下面的脱敏，也不改返回值。
		if observe != nil {
			observe(x)
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
				r, e := p.anonymizeJSONString(val, anon, observe)
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
			r, e := p.transform(val, childPII, anon, observe)
			if e != nil {
				return nil, e
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, e := range x {
			r, err := p.transform(e, inPII, anon, observe)
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
func (p *Proxy) anonymizeJSONString(v interface{}, anon func(string) (string, error), observe func(map[string]interface{})) (interface{}, error) {
	if p.m != nil {
		p.m.ToolCallsScanned.Inc()
	}
	switch x := v.(type) {
	case string:
		var parsed interface{}
		if err := json.Unmarshal([]byte(x), &parsed); err != nil {
			return v, nil // 非 JSON：保持原样
		}
		transformed, err := p.transform(parsed, true, anon, observe)
		if err != nil {
			return nil, err
		}
		// arguments 为 JSON 字符串（OpenAI 常见形态）时必须序列化回字符串：
		// 直接返回 map 会把线上 wire format 从 string 改成 object，上游（如
		// DeepSeek）按 string 反序列化会报 invalid type: map, expected a string。
		buf, err := marshalNoEscape(transformed)
		if err != nil {
			return nil, err
		}
		return string(bytes.TrimRight(buf, "\n")), nil
	case map[string]interface{}, []interface{}:
		return p.transform(x, true, anon, observe)
	default:
		return v, nil
	}
}

// commandFields 工具参数里承载「要执行的东西」的键名，按优先级排列。
//
// 覆盖的是「命令/脚本/目标」三类语义：Bash 类工具用 command，代码执行类用
// code/script，抓取类用 url，文件类用 path/file_path，检索类用 pattern。
// 判断层只吃这些字段——别的参数（超时、编码、重试次数）对行为判定没有信息量。
var commandFields = []string{
	"command", "cmd", "script", "code", "shell",
	"url", "file_path", "path", "pattern",
}

// observeToolCall 在 P2 观测位对一次工具调用做行为判定（契约 §12.8）。
//
// **位置与时序**：这是「下一轮回灌的请求体」，即 PhaseExecuted —— 看到时动作
// 已经发生。它的价值是审计、序列升级与 shadow 观测；事前拦截需要响应流位置
// （P5），本版本未接。把这条说清楚很重要：它决定了本函数**不该**有任何
// 「阻断请求」的语义。
//
// **不改行为**：函数无返回值，不修改 block，不写映射表，不影响脱敏结果。
// 这是 S2 阶段的硬约束——既有 e2e 与 L2 泄漏级对等性测试会立刻抓到任何偏移。
func (p *Proxy) observeToolCall(ctx context.Context, block map[string]interface{}, reqID, convID string) {
	if p.judgment == nil {
		return
	}
	tool, _ := block["name"].(string)
	if tool == "" {
		return
	}
	var raw interface{}
	switch {
	case block["arguments"] != nil:
		// OpenAI chat：tool_calls[].function.{name,arguments}
		raw = block["arguments"]
	case block["input"] != nil:
		// Anthropic Messages：type=tool_use 时 input 才是参数对象。
		// 不加这个 type 判断会把普通消息体的 input 字段误当成工具调用。
		switch t, _ := block["type"].(string); t {
		case "tool_use", "server_tool_use", "mcp_tool_use":
			raw = block["input"]
		default:
			return
		}
	default:
		return
	}
	cmd := commandFromArgs(raw)
	if cmd == "" {
		return
	}

	d := types.ActionDescriptor{
		Kind: types.KindToolCall, Phase: types.PhaseExecuted,
		Tool: tool, Command: cmd, Target: judge.FirstWord(cmd),
		ConversationID: convID, RequestID: reqID,
	}

	start := time.Now()
	verdict, err := p.judgment.Evaluate(ctx, d)
	elapsed := time.Since(start).Seconds()

	if p.m != nil {
		p.m.JudgeLatency.WithLabelValues(engineLabel(verdict, err)).Observe(elapsed)
	}
	if err != nil {
		if p.m != nil {
			p.m.JudgeUnavailable.WithLabelValues("chain", reasonLabel(err)).Inc()
		}
		// 判定失败意味着「这次没能看」，不是「这次没问题」——必须留下痕迹，
		// 否则判断层静默失效会被误读成「最近没有可疑行为」。
		log.Printf("[judge] tool=%s unavailable: %v", tool, err)
		return
	}
	if p.m != nil {
		p.m.VerdictTotal.WithLabelValues(string(verdict.Action), string(verdict.Category), verdict.Engine).Inc()
	}
	if verdict.Action != types.ActionAllow {
		// 只记录，不改变任何请求语义：影子模式的全部价值就在于
		// 「先看到真实分布，再决定阈值」。
		log.Printf("[judge] tool=%s action=%s category=%s severity=%.2f confidence=%.2f engine=%s degraded=%v reasons=%v",
			tool, verdict.Action, verdict.Category, verdict.Severity, verdict.Confidence,
			verdict.Engine, verdict.Degraded, verdict.Reasons)
	}
}

// engineLabel 取指标用的后端标签（判定失败时没有 verdict.Engine）。
func engineLabel(v types.Verdict, err error) string {
	if err != nil || v.Engine == "" {
		return "chain"
	}
	return v.Engine
}

// reasonLabel 把判定错误映射成稳定的指标标签（避免把错误消息原文当标签）。
func reasonLabel(err error) string {
	switch {
	case errors.Is(err, judge.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, judge.ErrInvalidOutput):
		return "invalid_output"
	case errors.Is(err, judge.ErrInputTooLarge):
		return "input_too_large"
	default:
		return "other"
	}
}

// commandFromArgs 从工具参数里取出「要执行的命令/目标」。
//
// arguments 有两种形态：JSON 字符串（OpenAI 常见，部分 SDK 已展开成对象）。
// 非 JSON 的字符串原样返回——有些工具直接把命令放在字符串参数里。
func commandFromArgs(raw interface{}) string {
	switch a := raw.(type) {
	case string:
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(a), &m); err != nil {
			return a
		}
		return firstStringField(m)
	case map[string]interface{}:
		return firstStringField(a)
	case []interface{}:
		// 有些工具的参数是命令数组（如 ["tar","-czf","x",".]）。
		if parts := stringSlice(a); len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	}
	return ""
}

// firstStringField 按 commandFields 的优先级取第一个非空字符串字段。
func firstStringField(m map[string]interface{}) string {
	for _, k := range commandFields {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// stringSlice 尝试把 []interface{} 全部读成字符串；含非字符串元素时返回 nil。
func stringSlice(v []interface{}) []string {
	if len(v) == 0 {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, e := range v {
		s, ok := e.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// forward 转发上游并还原响应。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, endpoint string, stream bool, reqID, convID, model string, newBody []byte, rawRequest string, entries []types.MappingEntry, start time.Time) {
	dest := p.upstreamFor(endpoint, r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, dest, bytes.NewReader(newBody))
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "build upstream request", err))
		return
	}
	copyHeaders(upReq.Header, r.Header)
	p.setUpstreamAuth(endpoint, upReq)
	upReq.Header.Set("Content-Type", "application/json")
	if ver := p.targetFor(endpoint).APIVersion; ver != "" {
		upReq.Header.Set("anthropic-version", ver)
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
	defer func() { _ = resp.Body.Close() }()

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
		if orphan := restorer.Orphans(); orphan > 0 {
			p.m.StreamOrphans.WithLabelValues(endpoint).Add(float64(orphan))
		}
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
		Detected:  entitiesFromEntries(entries),
		Mapping:   mappingToWire(entries),
		StartedAt: start, FinishedAt: now,
		DurationMs: now.Sub(start).Milliseconds(),
		Timestamp:  now,
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
		// 按请求记账：读本还原器（=本请求）的残留数，而不是全局累计量。
		if orphan := restorer.Orphans(); orphan > 0 {
			p.m.StreamOrphans.WithLabelValues(endpoint).Add(float64(orphan))
		}
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
		Detected:  entitiesFromEntries(entries),
		Mapping:   mappingToWire(entries),
		StartedAt: start, FinishedAt: now,
		DurationMs: now.Sub(start).Milliseconds(),
		Timestamp:  now,
	})
	p.recordAudit(reqID, convID, endpoint, model, true, entries, outcomeOf(resp.StatusCode), "", start)
}

// ---- 工具函数 ----

// isEventStream 判断上游是否以 SSE 帧返回（决定要不要做帧感知还原）。
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// isAnthropicEndpoint 判定 endpoint 是否走 Anthropic Messages 协议。
func isAnthropicEndpoint(endpoint string) bool {
	return endpoint == "messages" || strings.HasSuffix(endpoint, "/v1/messages")
}

// targetFor 按 endpoint 协议选择上游（配置驱动：openai / anthropic）。
func (p *Proxy) targetFor(endpoint string) *Upstream {
	if isAnthropicEndpoint(endpoint) && p.anthropic != nil {
		return p.anthropic
	}
	return p.openai
}

func (p *Proxy) upstreamFor(endpoint, path string) string {
	t := p.targetFor(endpoint)
	u := *t.URL
	rel := path
	if t.PathPrefix != "" {
		rel = t.PathPrefix + strings.TrimPrefix(path, "/v1")
	}
	u.Path = singleJoiningSlash(u.Path, rel)
	u.RawQuery = "" // 不转发代理自身 query
	return u.String()
}

func (p *Proxy) setUpstreamAuth(endpoint string, req *http.Request) {
	t := p.targetFor(endpoint)
	if t.APIKey == "" {
		// 透传模式：上游没配 key 时，保留客户端原样鉴权头（gate 前置拓扑，
		// 真实鉴权由下一跳 ccswitch 等完成，gate 只做脱敏、不持有凭据）。
		return
	}
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	if isAnthropicEndpoint(endpoint) {
		// Anthropic 用 x-api-key
		req.Header.Set("x-api-key", t.APIKey)
		return
	}
	req.Header.Set("Authorization", "Bearer "+t.APIKey)
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
		Upstream:         p.targetFor(endpoint).URL.Host,
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

// requestIDPattern 客户端可提供的请求标识白名单：字母 / 数字 / - / _，8~64 字符。
//
// 网关侧的 request_id 有两个真实用途——vault 映射表主键与调试面板索引键——所以它
// 必须是网关能约束的值。客户端提供的任意字符串直接进键空间会带来两类问题：
//   - 超长 / 含特殊字符的值污染审计日志、面板与（历史版本里的）落盘文件名
//   - 可预测的值（固定值、时间戳、用户 ID 派生）让「按 request_id 还原原文」变成
//     一条可猜的通道
//
// 说清楚定位：客户端提供的 ID 只是为了让客户端日志能与网关日志对齐，它从来不是
// 身份凭据，网关的任何授权判断都不该建立在它之上（还原能力由控制面令牌把关）。
//
// 不合规时静默改用服务端随机值而不是报错：ID 对客户端是便利设施，不是契约字段，
// 为一个格式不合规的便利值让整个请求失败不值得。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// conversationIDPattern 会话 ID 白名单，长度放宽到 1（会话 ID 短是常见写法）。
var conversationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func requestID(r *http.Request) string {
	for _, h := range []string{"X-Request-ID", "X-Llmate-Request-Id"} {
		if v := r.Header.Get(h); requestIDPattern.MatchString(v) {
			return v
		}
	}
	return randHex(16)
}

// conversationID 优先取 X-Conversation-ID 头，用于多轮对话的 Merkle 增量检测。
//
// 原签名带一个 io.Reader 参数却从未使用（历史遗留的「从 body 里掏 conversation_id」
// 设想，从未实现）。已删除，避免调用方误以为 body 会被读取。
//
// 这里同样校验收紧：该值会成为 Merkle 缓存的键，而缓存保存的是「上一轮扫到哪里」的
// 增量状态。若两个不相干的会话共用同一个 ID，后者的新增文本会被当成「已扫过」而跳过
// 检测——那是直接漏检。ID 不合规时按「无会话」处理（每次全量扫），宁可多扫一遍。
func conversationID(r *http.Request) string {
	if v := r.Header.Get("X-Conversation-ID"); conversationIDPattern.MatchString(v) {
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
	case gatewayerrors.CodeNotFound:
		// 映射表不存在 / 已过期 / 不属于本调用面，对外统一 404（契约 §7.3）。
		return http.StatusNotFound
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
	dest := p.upstreamFor("", r.URL.Path)
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, dest, nil)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "build passthrough", err))
		return
	}
	copyHeaders(upReq.Header, r.Header)
	p.setUpstreamAuth("", upReq)
	resp, err := p.client.Do(upReq)
	if err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeUpstreamError, "upstream passthrough", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
