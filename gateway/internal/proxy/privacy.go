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
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/replacer"
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
type privacyRedactReq struct {
	JSON           json.RawMessage `json:"json,omitempty"`
	Text           string          `json:"text,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Strategy       string          `json:"strategy,omitempty"` // placeholder（默认）| simulate
}

// privacyRedactResp POST /v1/privacy/redact 出参。
type privacyRedactResp struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	RequestID string          `json:"request_id"`
	Changed   bool            `json:"changed"`
	Strategy  string          `json:"strategy"`
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
func (p *Proxy) redactValue(ctx context.Context, sess *replacer.Session, convID string, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case string:
		return p.redactText(ctx, sess, convID, x)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
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
func (p *Proxy) restoreValue(ctx context.Context, reqID string, v interface{}) (interface{}, error) {
	switch x := v.(type) {
	case string:
		return p.proc.Restore(ctx, reqID, x)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
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
