// Package proxy OpenAI 兼容反向代理 + 请求脱敏 + 响应还原（契约 §4）。
//
// privacy.go 提供常驻（不受 --no-debug 门控）的隐私 API，供 Claude Code hooks、
// VS Code 扩展等外部集成点调用：
//   POST /v1/privacy/redact   —— 对一段 JSON / 文本递归脱敏（键保留、值脱敏），返回占位符 + request_id
//   POST /v1/privacy/restore  —— 按 request_id 把占位符还原为原文
//
// 与调试面板 /_api/replace 的区别：这里常驻可用、auth 门控、且支持任意嵌套 JSON 的递归扫描，
// 并复用与代理层完全一致的核心（pipeline.DetectText / replacer.Session / vault），保证占位符一致。
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/replacer"
	"gateway/pkg/types"
)

var (
	errEmptyPrivacyInput = errors.New("json or text is required")
	errBadStrategy       = errors.New("strategy must be placeholder|simulate")
	errMissingRequestID  = errors.New("request_id is required for restore")
)

// privacyRedactReq POST /v1/privacy/redact 入参。
//
// 两种形态任选其一：
//   - json：任意嵌套 JSON（推荐，键保留、字符串值脱敏）
//   - text：纯文本（整体脱敏）
//
// conversation_id 可选，用于 Merkle/LRU 缓存命中（外部集成点通常留空，每次独立检测）。
// gate_only=true 时只扫描不改写（用于 Claude Code hooks / MCP-server 边界「先看 PII
// 再决定拦/放」），返回 has_pii + entities 列表，不返回 request_id（无可还原映射）。
type privacyRedactReq struct {
	JSON           json.RawMessage `json:"json,omitempty"`
	Text           string          `json:"text,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Strategy       string          `json:"strategy,omitempty"`      // placeholder（默认）| simulate
	GateOnly       bool            `json:"gate_only,omitempty"`     // true=只判不改；默认 false
	BlockTypes     []string        `json:"block_types,omitempty"`   // gate_only 模式下，命中即标记的 PII 类型（默认全部）
}

// privacyRedactResp POST /v1/privacy/redact 出参。
type privacyRedactResp struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	RequestID string          `json:"request_id,omitempty"` // gate_only 时为空
	Changed   bool            `json:"changed"`
	Strategy  string          `json:"strategy"`
	// GateOnly 模式专属字段
	HasPII   bool             `json:"has_pii,omitempty"`
	Entities []entitySummary  `json:"entities,omitempty"`
	Blocked  bool             `json:"blocked,omitempty"` // 是否因 block_types 命中而拦截
}

// entitySummary 极简 PII 实体摘要（gate_only 模式用，不含 start/end 避免泄漏结构）
type entitySummary struct {
	Type  string  `json:"type"`
	Value string  `json:"value,omitempty"` // 命中字符串本身（用于 hooks 决定「拦还是脱敏后放」）
	Score float64 `json:"score"`
}

// privacyRestoreReq POST /v1/privacy/restore 入参。
type privacyRestoreReq struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	RequestID string          `json:"request_id"` // 来自 redact 返回的 request_id
}

// privacyRestoreResp POST /v1/privacy/restore 出参。
type privacyRestoreResp struct {
	JSON json.RawMessage `json:"json,omitempty"`
	Text string          `json:"text,omitempty"`
}

// PrivacyRedact 递归脱敏任意 JSON / 文本，返回占位符与 request_id。
//
// 设计要点（契约 §5.2 规则 1-3）：
//   - 整个请求只用「一个」replacer.Session，保证跨字段/跨层占位符编号全局唯一、同 (type,value) 复用同一哨兵；
//   - JSON 键永不脱敏（递归时只对字符串值调用脱敏）；
//   - 脱敏结果（MappingEntry）写入 vault，键为本次生成的 request_id，供后续 restore 还原。
//
// gate_only=true 时只扫描不改写：
//   - 不调用 redactText，输出原样返回（json/text 字段等于入参）
//   - 不创建 request_id（无可还原映射）
//   - 返回 has_pii + entities 摘要 + blocked（block_types 命中即 true）
//   - 用于 Claude Code hooks / MCP-server 边界「先看 PII 再决定拦/放」
func (p *Proxy) PrivacyRedact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req privacyRedactReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "decode", err))
		return
	}
	if len(req.JSON) == 0 && req.Text == "" {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "empty", errEmptyPrivacyInput))
		return
	}
	if req.Strategy != "" && req.Strategy != "placeholder" && req.Strategy != "simulate" {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "strategy", errBadStrategy))
		return
	}

	// gate_only 分支：只扫描不改写
	if req.GateOnly {
		p.handleGateOnly(w, r, &req)
		return
	}

	// 整个请求共享一个会话，保证占位符全局唯一。
	sess := p.proc.NewSession()
	if req.Strategy != "" {
		_ = sess.SetStrategy(req.Strategy)
	}

	var (
		outJSON json.RawMessage
		outText string
		changed bool
	)

	if len(req.JSON) > 0 {
		var root interface{}
		dec := json.NewDecoder(bytes.NewReader(req.JSON))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "json decode", err))
			return
		}
		redacted, err := p.redactValue(r.Context(), sess, req.ConversationID, root)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "redact", err))
			return
		}
		buf, err := marshalNoEscape(redacted)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "marshal", err))
			return
		}
		outJSON = buf
		changed = !jsonEqual(buf, req.JSON)
	} else {
		redacted, err := p.redactText(r.Context(), sess, req.ConversationID, req.Text)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "redact", err))
			return
		}
		outText = redacted
		changed = redacted != req.Text
	}

	// 整段递归脱敏统一在会话里累积映射条目；生成一次 request_id 并落盘，供后续 restore 还原。
	reqID := newRequestID()
	if err := p.proc.Store(reqID, req.ConversationID, sess.Entries()); err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "store mapping", err))
		return
	}

	resp := privacyRedactResp{
		JSON:      outJSON,
		Text:      outText,
		RequestID: reqID,
		Changed:   changed,
		Strategy:  firstNonEmpty(req.Strategy, "placeholder"),
	}
	writePrivacyJSON(w, resp)
}

// handleGateOnly gate_only=true 分支：只扫描不改写，返回 has_pii + entities 摘要。
func (p *Proxy) handleGateOnly(w http.ResponseWriter, r *http.Request, req *privacyRedactReq) {
	// 构造 block set：空表示所有 PII 命中都「block」
	blockSet := make(map[string]bool, len(req.BlockTypes))
	for _, t := range req.BlockTypes {
		blockSet[t] = true
	}

	var entities []entitySummary
	blocked := false

	// gate_only 模式：原样回传入参（不解组/重 marshal，避免双重引号 / 字段顺序变化）
	var outJSON json.RawMessage
	var outText string
	if len(req.JSON) > 0 {
		outJSON = req.JSON
		// 走 gateDocumentOnly：递归但不替换
		ents := p.gateDocumentJSON(r.Context(), req.ConversationID, req.JSON)
		for _, e := range ents {
			summary := entitySummary{Type: e.Type, Value: e.Value, Score: e.Score}
			entities = append(entities, summary)
			if len(blockSet) == 0 || blockSet[e.Type] {
				blocked = true
			}
		}
	} else {
		outText = req.Text
		// text 模式：单次扫描
		ents, err := p.proc.DetectText(r.Context(), req.ConversationID, req.Text)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "gate detect", err))
			return
		}
		for _, e := range ents {
			summary := entitySummary{Type: e.Type, Value: e.Value, Score: e.Score}
			entities = append(entities, summary)
			if len(blockSet) == 0 || blockSet[e.Type] {
				blocked = true
			}
		}
	}

	resp := privacyRedactResp{
		JSON:     outJSON,
		Text:     outText,
		Changed:  false,
		Strategy: firstNonEmpty(req.Strategy, "placeholder"),
		HasPII:   len(entities) > 0,
		Entities: entities,
		Blocked:  blocked,
	}
	writePrivacyJSON(w, resp)
}

// PrivacyRestore 按 request_id 把占位符还原为原文。
//
// 递归遍历 JSON / 文本，对每个字符串调用 pipeline.Restore（底层按 sentinel 从 vault 取原文，
// 失败 fail-safe 保留占位符不报错）。一次 redact 的映射表全局共享，故同一 request_id 可跨
// 多次调用还原。
func (p *Proxy) PrivacyRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req privacyRestoreReq
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "decode", err))
		return
	}
	if req.RequestID == "" {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "request_id", errMissingRequestID))
		return
	}
	if len(req.JSON) == 0 && req.Text == "" {
		writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "empty", errEmptyPrivacyInput))
		return
	}

	var (
		outJSON json.RawMessage
		outText string
	)
	if len(req.JSON) > 0 {
		var root interface{}
		dec := json.NewDecoder(bytes.NewReader(req.JSON))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "json decode", err))
			return
		}
		restored, err := p.restoreValue(r.Context(), req.RequestID, root)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "restore", err))
			return
		}
		buf, err := marshalNoEscape(restored)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "marshal", err))
			return
		}
		outJSON = buf
	} else {
		restored, err := p.proc.Restore(r.Context(), req.RequestID, req.Text)
		if err != nil {
			writeError(w, gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "restore", err))
			return
		}
		outText = restored
	}

	writePrivacyJSON(w, privacyRestoreResp{JSON: outJSON, Text: outText})
}

// redactValue 递归脱敏：map / slice 下钻，字符串值送脱敏，其它类型原样返回。
// JSON 键永不脱敏（map 分支只对 value 递归，key 不动）。
//
// 不透明 block 跳过（见 shouldSkipValue）：base64 数据 / thinking 块 / 加密内容 /
// mcp_list_tools 等子树原样保留，不进 redactText，避免重写损坏载荷或误判。
func (p *Proxy) redactValue(ctx context.Context, sess *replacer.Session, convID string, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case string:
		// string 单值（top-level 或 list 元素）：按 byPrefix 判定 data URL
		if shouldSkipValue("", x) {
			return x, nil
		}
		return p.redactText(ctx, sess, convID, x)
	case map[string]interface{}:
		// 整子树是已知不透明 block：原样返回
		if shouldSkipValue("", x) {
			return x, nil
		}
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			if shouldSkipValue(k, val) {
				m[k] = val
				continue
			}
			r, err := p.redactValue(ctx, sess, convID, val)
			if err != nil {
				return nil, err
			}
			m[k] = r // 键 k 保留，不脱敏
		}
		return m, nil
	case []interface{}:
		s := make([]interface{}, len(x))
		for i, val := range x {
			r, err := p.redactValue(ctx, sess, convID, val)
			if err != nil {
				return nil, err
			}
			s[i] = r
		}
		return s, nil
	default:
		return v, nil // number / bool / nil 原样
	}
}

// restoreValue 递归还原占位符（同 redactValue 结构，但调用 Restore）。
// 同步跳过不透明 block：这些子树在 redact 时未被替换，restore 时也不应改写。
func (p *Proxy) restoreValue(ctx context.Context, reqID string, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case string:
		// data URL 之类不透明 string：原样返回，不进 Restore
		if shouldSkipValue("", x) {
			return x, nil
		}
		return p.proc.Restore(ctx, reqID, x)
	case map[string]interface{}:
		if shouldSkipValue("", x) {
			return x, nil
		}
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			if shouldSkipValue(k, val) {
				m[k] = val
				continue
			}
			r, err := p.restoreValue(ctx, reqID, val)
			if err != nil {
				return nil, err
			}
			m[k] = r
		}
		return m, nil
	case []interface{}:
		s := make([]interface{}, len(x))
		for i, val := range x {
			r, err := p.restoreValue(ctx, reqID, val)
			if err != nil {
				return nil, err
			}
			s[i] = r
		}
		return s, nil
	default:
		return v, nil
	}
}

// redactText 对单段文本：检测 + 替换，返回脱敏文本（映射条目累积进会话）。
func (p *Proxy) redactText(ctx context.Context, sess *replacer.Session, convID, text string) (string, error) {
	if text == "" {
		return text, nil
	}
	ents, err := p.proc.DetectText(ctx, convID, text)
	if err != nil {
		return "", err
	}
	out, _, err := sess.Replace(text, ents)
	if err != nil {
		return "", err
	}
	return out, nil
}

// gateDocumentJSON gate_only 模式的递归扫描：把 JSON 树按 PII 字段规则走一遍，
// 收集所有 Entity，但不替换、不写 vault。返回的 entity 不带 start/end 偏移
// （外层只关心 type / value / score）。
func (p *Proxy) gateDocumentJSON(ctx context.Context, convID string, raw json.RawMessage) []types.Entity {
	var root interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	var all []types.Entity
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case string:
			if x == "" {
				return
			}
			ents, err := p.proc.DetectText(ctx, convID, x)
			if err != nil {
				return
			}
			all = append(all, ents...)
		case map[string]interface{}:
			// 跳过不透明 block
			if shouldSkipValue("", x) {
				return
			}
			for k, val := range x {
				// arguments 字段按 JSON 字符串解析后递归
				if k == "arguments" {
					if s, ok := val.(string); ok {
						var parsed interface{}
						if err := json.Unmarshal([]byte(s), &parsed); err == nil {
							walk(parsed)
						}
					} else {
						walk(val)
					}
					continue
				}
				// 不透明 value 跳过
				if shouldSkipValue(k, val) {
					continue
				}
				walk(val)
			}
		case []interface{}:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(root)
	return all
}

// marshalNoEscape 与 writePrivacyJSON 保持一致：禁用 HTML 转义，保证占位符 <<type_index>>
// 以字面量输出而非 \u003c。
func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- block allowlist（不动这些子树）----------
//
// 背景：通用 LLM API 的请求体里有多种「不透明 block」——重写它们会损坏载荷，
// 但里面也没有 PII 文本可被检测器发现。直接送进 redactValue 会让 base64 /
// 思考块 / 加密内容被错误处理。
//
// 借鉴思路参考 PrivAiTe（privaite/gateway/scrub.py 的 _THINKING_TYPES /
// _BINARY_PART_TYPES / _RESPONSES_OPAQUE_TYPES 等 allowlist），但**具体集合按
// 我们自己的契约与字段名实现**，与上游命名无关。
//
// 三类不透明子树（见 §8.2「架构对齐 · Block allowlist」）：
//   - byType：block 根的 `type` 字段是已知不透明类型 → 整子树跳过
//   - byKey：JSON 对象的 key 是已知不透明字段 → 该 value 子树跳过
//   - byPrefix：字符串值是 data URL（base64）前缀 → 整串跳过
//
// 调用方（`redactValue`）按"先 byType / byKey，再递归"的顺序判断；遇到任一
// 命中即原样返回，**不进入** redactText。

// opaqueBlockTypes block 根 type 字段命中 → 整子树跳过（不透出，结构保留）。
// 覆盖 OpenAI Responses / Anthropic Messages / MCP 三类不透明 part。
var opaqueBlockTypes = map[string]struct{}{
	// OpenAI Responses API（不透明 / 二进制 / 指针）
	"reasoning":             {},
	"compaction":            {},
	"compaction_trigger":    {},
	"computer_call_output":  {}, // 屏幕截图等二进制
	"image_generation_call": {},
	"item_reference":        {}, // 仅有 id，无内容
	"mcp_list_tools":        {}, // 工具列表，结构无 PII
	"tool_search_call":      {},
	"tool_search_output":    {},
	"additional_tools":      {},
	// OpenAI / Anthropic 多模态二进制 part
	"input_image":  {},
	"input_file":   {},
	"input_audio":  {},
	"image":        {},
	"output_image": {},
	"computer_screenshot": {},
	// Anthropic 思考块（重写会被拒）
	"thinking":           {},
	"redacted_thinking":  {},
	// Anthropic 上传容器指针
	"container_upload": {},
	// Anthropic / OpenAI web 工具结果
	"web_search_call":   {}, // query 已在外层 actions 扫过；call 本身是 action 描述
	"web_fetch_result":  {},
}

// opaqueValueKeys JSON 对象的 key 命中 → 整个 value 子树跳过。
// 这些字段要么是 base64 / 加密内容（重写即损坏），要么是上游校验用的 id / 签名。
var opaqueValueKeys = map[string]struct{}{
	// 通用：二进制 / base64 负载（OpenAI file_id 引用、Anthropic document source.data）
	"data":               {},
	"image_url":          {}, // OpenAI 多模态 {type:image_url, image_url:{url:data:...}}
	"input_image":        {},
	"input_audio":        {},
	"file_id":            {},
	"file":               {},
	// Anthropic 思考 / 加密内容
	"signature":          {}, // thinking.signature
	"encrypted_content":  {}, // Anthropic web_search / web_fetch
	"redacted_data":      {},
	// OpenAI Responses 加密 / 内部
	"summary":            {},
}

// dataURLPrefixes 字符串值以这些前缀开头 → 整串跳过（base64 图像 / 音频）。
// 注意：dataURLPrefixes 只在 string 分支判断（map / slice 走 byKey / byType）。
var dataURLPrefixes = [...]string{
	"data:image/",
	"data:audio/",
	"data:video/",
	"data:application/octet-stream",
	"data:application/pdf",
}

// shouldSkipValue 判断给定 (parent key, node) 是否需要被跳过（不脱敏、不递归）。
// k 仅在 map[string]interface{} 分支传入；top-level 与 list 元素传 ""。
func shouldSkipValue(k string, v interface{}) bool {
	// 1) byKey：父 key 命中
	if k != "" {
		if _, ok := opaqueValueKeys[k]; ok {
			return true
		}
	}
	// 2) byType：node 是 dict 且 type 字段是已知不透明类型
	if m, ok := v.(map[string]interface{}); ok {
		if t, _ := m["type"].(string); t != "" {
			if _, ok := opaqueBlockTypes[t]; ok {
				return true
			}
		}
	}
	// 3) byPrefix：string 以 data: 开头（base64 图像 / 音频 / 视频）
	if s, ok := v.(string); ok {
		for _, p := range dataURLPrefixes {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
	}
	return false
}

// ---------- helpers ----------

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极小概率：回退到时间戳（仍具唯一性，仅丧失随机性）
		return "req-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "req-" + hex.EncodeToString(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func writePrivacyJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 占位符 << / >> 不能编码成 \u003c/\u003e
	_ = enc.Encode(v)
	_, _ = w.Write(buf.Bytes())
}

func decodeJSONBody(r *http.Request, v interface{}) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func jsonEqual(a, b json.RawMessage) bool {
	var ja, jb interface{}
	if err := json.Unmarshal(a, &ja); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &jb); err != nil {
		return false
	}
	return jsonEqualValue(ja, jb)
}

func jsonEqualValue(a, b interface{}) bool {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v1 := range av {
			v2, ok := bv[k]
			if !ok || !jsonEqualValue(v1, v2) {
				return false
			}
		}
		return true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqualValue(av[i], bv[i]) {
				return false
			}
		}
		return true
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av.String() == bv.String()
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	default:
		return a == b
	}
}
