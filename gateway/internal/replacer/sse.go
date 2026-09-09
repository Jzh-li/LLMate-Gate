package replacer

import (
	"bytes"
	"encoding/json"
)

// SSE 帧边界：事件以空行（\n\n）分隔。
var sseBoundary = []byte("\n\n")

// 承载「模型输出文本」的 JSON 键——只还原这些键的字符串值。
// 其余字段（id/model/index/finish_reason…）必须原样透传。
var sseTextKeys = map[string]bool{
	"content":           true, // OpenAI chat delta.content / Anthropic content_block
	"text":              true, // /v1/completions choices[].text、Anthropic delta.text
	"reasoning_content": true,
	"reasoning":         true,
	"refusal":           true,
	"output_text":       true,
}

// SSERestorer 在 SSE 帧之上做还原（契约 §5.3、技术方案 §5 流式还原）。
//
// 为什么不能直接对原始字节流还原：占位符是「内容维度」连续的，但 SSE 会在中间
// 插入帧结构（data: 前缀、\n\n 分隔符、以及下一帧的 JSON 结构），导致占位符被
// 切成 <<email 与 _1>> 分处两个事件——原始字节流上永远匹配不到完整哨兵。
//
// 本实现按帧解析：只把 data 负载里 sseTextKeys 对应的字符串交给 StreamRestorer，
// 而 StreamRestorer 的跨块缓冲在「内容维度」上跨事件保持，因此被拆开的占位符
// 仍能在后续事件到达时拼回并还原。非 data 行（event:/id:/retry:/注释/[DONE]）
// 原样透传。
type SSERestorer struct {
	inner *StreamRestorer
	pend  []byte // 尚未构成完整事件的尾部字节
}

// NewSSERestorer 包装既有 StreamRestorer，增加 SSE 帧感知能力。
func NewSSERestorer(inner *StreamRestorer) *SSERestorer {
	return &SSERestorer{inner: inner}
}

// Write 接收上游分块，返回可立即下发的已还原 SSE 字节。
func (s *SSERestorer) Write(chunk []byte) ([]byte, error) {
	if s.inner == nil {
		return chunk, nil
	}
	s.pend = append(s.pend, chunk...)
	var out []byte
	for {
		idx := bytes.Index(s.pend, sseBoundary)
		if idx < 0 {
			break
		}
		event := s.pend[:idx+len(sseBoundary)]
		s.pend = s.pend[idx+len(sseBoundary):]
		out = append(out, s.processEvent(event)...)
	}
	return out, nil
}

// Close 冲刷残余：先处理未闭合的最后一帧，再让 inner 收尾（残留不完整哨兵计入 orphan）。
func (s *SSERestorer) Close() ([]byte, error) {
	if s.inner == nil {
		return nil, nil
	}
	var out []byte
	if len(s.pend) > 0 {
		out = append(out, s.processEvent(s.pend)...)
		s.pend = nil
	}
	rest, _ := s.inner.Close()
	return append(out, rest...), nil
}

// processEvent 处理单个完整事件（含结尾 \n\n）。
func (s *SSERestorer) processEvent(ev []byte) []byte {
	suffix := []byte{}
	body := ev
	if bytes.HasSuffix(body, sseBoundary) {
		suffix = sseBoundary
		body = body[:len(body)-len(sseBoundary)]
	}
	lines := bytes.Split(body, []byte("\n"))
	for i, ln := range lines {
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue // event:/id:/retry:/:/[DONE] 之外的行原样透传
		}
		payload := bytes.TrimSpace(ln[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if restored, ok := s.restorePayload(payload); ok {
			lines[i] = append([]byte("data: "), restored...)
		}
	}
	return append(bytes.Join(lines, []byte("\n")), suffix...)
}

// restorePayload 对单个 data 负载做还原；ok=false 表示不是 JSON 或无变化（保持原样）。
func (s *SSERestorer) restorePayload(payload []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber() // 保留数字字面，避免 int64 精度与 1.7e9 这类科学计数法变形
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if !sseRestoreWalk(v, s.inner) {
		return nil, false
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 必须：否则 << >> 被转成 \u003c \u003e
	if err := enc.Encode(v); err != nil {
		return nil, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// sseRestoreWalk 递归遍历 JSON，仅还原 sseTextKeys 对应的字符串值；返回是否发生改动。
func sseRestoreWalk(v interface{}, inner *StreamRestorer) bool {
	changed := false
	switch node := v.(type) {
	case map[string]interface{}:
		for k, val := range node {
			if str, ok := val.(string); ok && sseTextKeys[k] {
				out, _ := inner.Write([]byte(str))
				if string(out) != str {
					node[k] = string(out)
					changed = true
				}
				continue
			}
			if sseRestoreWalk(val, inner) {
				changed = true
			}
		}
	case []interface{}:
		for _, item := range node {
			if sseRestoreWalk(item, inner) {
				changed = true
			}
		}
	}
	return changed
}
