// privacy_test.go —— 常驻隐私端点（/v1/privacy/redact）与不透明 block 跳过
// （block allowlist）的单测。覆盖 §8.2「架构对齐 · Block allowlist」。
package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gateway/internal/detector"
	"gateway/internal/pipeline"
	"gateway/internal/replacer"
	"gateway/internal/simulator"
	"gateway/internal/vault"
)

// newPrivacyTestProxy 构造一个最小可用的 *Proxy，仅供 redactValue / restoreValue 单测。
// 不启 HTTP 服务、不挂路由——只复用其内部组件。
func newPrivacyTestProxy(t *testing.T) *Proxy {
	t.Helper()
	v, err := vault.NewMemVault(testTTL, []byte("unit-test-passphrase"))
	require.NoError(t, err)
	det := detector.NewRegexEngine()
	repl := replacer.New(replacer.Config{
		Strategy:   "placeholder",
		Simulate:   simulator.SimulateZHConfig{},
		SessionKey: []byte("session-key-32-bytes-long!!!"),
	}, v)
	proc := pipeline.New(pipeline.Config{Detector: det, Replacer: repl, Vault: v, FailClosed: true})
	return &Proxy{proc: proc}
}

// TestShouldSkipValue 覆盖三类不透明子树判定。
func TestShouldSkipValue(t *testing.T) {
	t.Run("byType: 整 subtree 是 thinking 块", func(t *testing.T) {
		v := map[string]interface{}{"type": "thinking", "thinking": "用户手机 13800138000"}
		require.True(t, shouldSkipValue("", v))
	})
	t.Run("byKey: data 字段是 base64", func(t *testing.T) {
		v := "iVBORw0KGgoAAAANSUhEUgAA..."
		require.True(t, shouldSkipValue("data", v))
	})
	t.Run("byPrefix: data URL 字符串", func(t *testing.T) {
		v := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA..."
		require.True(t, shouldSkipValue("", v))
	})
	t.Run("负面: 普通 PII 文本不应跳过", func(t *testing.T) {
		v := "我的手机 13800138000"
		require.False(t, shouldSkipValue("", v))
	})
	t.Run("负面: 普通 dict 不应跳过", func(t *testing.T) {
		v := map[string]interface{}{"role": "user", "content": "电话 13800138000"}
		require.False(t, shouldSkipValue("", v))
	})
	t.Run("负面: image_url 是 dict（非字符串）也不命中 byPrefix", func(t *testing.T) {
		v := map[string]interface{}{"url": "data:image/png;base64,xxx"}
		// map 本身不命中 byType（无 type 字段），内部走递归命中 image_url 键
		require.False(t, shouldSkipValue("", v))
	})
}

// TestRedactValue_OpBlocks cover:
//  1. base64 字符串（data 字段）原样返回
//  2. thinking 块整 subtree 原样返回（即使内部含 PII 也不脱敏）
//  3. image data URL 原样返回
//  4. 普通文本里的 PII 仍被脱敏
//  5. 通透结构（content 字段）仍递归脱敏
func TestRedactValue_OpBlocks(t *testing.T) {
	px := newPrivacyTestProxy(t)
	ctx := context.Background()
	sess := px.proc.NewSession()
	convID := ""

	// Case 1: 顶层 data 字段（base64）
	t.Run("data 字段 base64 跳过", func(t *testing.T) {
		in := map[string]interface{}{
			"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
		}
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		got, _ := json.Marshal(out)
		// 整段未变（不进 redactText，无占位符被写入）
		require.Equal(t, json.RawMessage(got), json.RawMessage(mustMarshal(t, in)))
	})

	// Case 2: thinking 整块不脱敏（即使 thinking 字段内含 PII 也不动）
	t.Run("thinking 整块不脱敏", func(t *testing.T) {
		in := map[string]interface{}{
			"type":     "thinking",
			"thinking": "用户提到 13800138000，这是 PII。",
		}
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		got, _ := json.Marshal(out)
		require.Equal(t, json.RawMessage(got), json.RawMessage(mustMarshal(t, in)))
	})

	// Case 3: image data URL 字符串（原样）
	t.Run("data URL 字符串原样", func(t *testing.T) {
		in := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA"
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		require.Equal(t, in, out)
	})

	// Case 4: 普通 PII 仍被脱敏
	t.Run("普通 PII 文本脱敏", func(t *testing.T) {
		in := "我的手机 13800138000"
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		s, _ := out.(string)
		require.NotEqual(t, in, s)
		require.NotContains(t, s, "13800138000")
	})

	// Case 5: 通透字段 content 仍递归脱敏
	t.Run("content 字段递归脱敏", func(t *testing.T) {
		in := map[string]interface{}{
			"role":    "user",
			"content": "联系张三，手机 13800138000",
		}
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		m := out.(map[string]interface{})
		// 键保留
		require.Equal(t, "user", m["role"])
		// 值脱敏
		s, _ := m["content"].(string)
		require.NotContains(t, s, "13800138000")
	})

	// Case 6: mcp_list_tools 块整 subtree 不动
	t.Run("mcp_list_tools 块不动", func(t *testing.T) {
		in := map[string]interface{}{
			"type":   "mcp_list_tools",
			"server": "github",
			"tools":  []interface{}{"create_issue", "list_repos"},
		}
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		got, _ := json.Marshal(out)
		require.Equal(t, json.RawMessage(got), json.RawMessage(mustMarshal(t, in)))
	})

	// Case 7: 嵌套 — OpenAI chat 多模态 content 数组里 image_url 跳过
	t.Run("多模态 content 数组里 image_url 跳过", func(t *testing.T) {
		in := map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "请描述这张图"},
				map[string]interface{}{
					"type":     "image_url",
					"image_url": map[string]interface{}{
						"url": "data:image/png;base64,abc",
					},
				},
			},
		}
		out, err := px.redactValue(ctx, sess, convID, in)
		require.NoError(t, err)
		m := out.(map[string]interface{})
		arr := m["content"].([]interface{})
		// 第一个 part（text）原样保留（"请描述这张图" 无 PII）
		first := arr[0].(map[string]interface{})
		require.Equal(t, "请描述这张图", first["text"])
		// 第二个 part（image_url）原样保留
		second := arr[1].(map[string]interface{})
		require.Equal(t, "image_url", second["type"])
		// 内层 image_url.url 仍是 data URL 字符串，未被改
		inner := second["image_url"].(map[string]interface{})
		require.Equal(t, "data:image/png;base64,abc", inner["url"])
	})
}

// TestRedactValue_RegFunc 测试 /v1/privacy/redact 端点的脱敏功能。
// 注意：privacy.redactValue 走无差别脱敏（任意 string 都扫）——与 proxy.transform
// 按 PII 字段表的精细脱敏不同。这里测的是端点行为。
func TestRedactValue_RegFunc(t *testing.T) {
	px := newPrivacyTestProxy(t)
	ctx := context.Background()
	sess := px.proc.NewSession()
	convID := ""

	in := map[string]interface{}{
		"content":  "联系张三，手机 13800138000",
		"role":     "user",
		"api_key":  "sk-1234567890abcdef",
		"messages": []interface{}{
			map[string]interface{}{"content": "邮箱 zhangsan@example.com"},
		},
	}
	out, err := px.redactValue(ctx, sess, convID, in)
	require.NoError(t, err)
	m := out.(map[string]interface{})
	// 键保留
	require.Contains(t, m, "content")
	require.Contains(t, m, "role")
	require.Contains(t, m, "api_key")
	require.Contains(t, m, "messages")
	// content 字段被脱敏（zh_person_name + zh_phone 都被识别）
	cs, _ := m["content"].(string)
	require.NotEqual(t, "联系张三，手机 13800138000", cs)
	require.NotContains(t, cs, "13800138000")
	// 嵌套 content 字段（list of messages）也脱敏
	msgs := m["messages"].([]interface{})
	firstMsg := msgs[0].(map[string]interface{})
	firstContent, _ := firstMsg["content"].(string)
	require.NotContains(t, firstContent, "zhangsan@example.com")
	// api_key 走不可逆 redact（privacy.go 走无差别脱敏，所有 string 都进检测器）
	require.Equal(t, "[REDACTED]", m["api_key"])
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestPrivacyRedact_GateOnly 通过 HTTP 路径测 gate_only=true 模式：
// 入参是含 PII 的 JSON/text，返回 has_pii + entities，原文应原样保留。
func TestPrivacyRedact_GateOnly(t *testing.T) {
	t.Run("text 模式：返回 has_pii=true，原文保留", func(t *testing.T) {
		px := newPrivacyTestProxy(t)
		req := newReqJSON("POST", "/v1/privacy/redact", `{
			"text": "联系张三，手机 13800138000",
			"gate_only": true
		}`)
		rec := httptest.NewRecorder()
		px.PrivacyRedact(rec, req)
		require.Equal(t, 200, rec.Code)

		var resp struct {
			Text     string  `json:"text"`
			HasPII   bool    `json:"has_pii"`
			Entities []struct {
				Type  string  `json:"type"`
				Value string  `json:"value"`
				Score float64 `json:"score"`
			} `json:"entities"`
			Blocked  bool   `json:"blocked"`
			Changed  bool   `json:"changed"`
			RequestID string `json:"request_id"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.True(t, resp.HasPII)
		require.NotEmpty(t, resp.Entities)
		require.True(t, resp.Blocked) // 默认 block_types 为空 = 命中即 block
		require.False(t, resp.Changed)
		require.Equal(t, "", resp.RequestID) // gate_only 不创建 request_id
		require.Equal(t, "联系张三，手机 13800138000", resp.Text) // 原文原样
	})

	// 注：JSON 嵌套模式（req.json 字段作为嵌套 JSON object）暂未实现，
	// 当前 gate_only 的 json 字段按 string 接收。text 模式已覆盖 gate_only 核心语义：
	// 1) has_pii=true / entities 准确；2) 原文不改写。
	// 见 .workbuddy/TODO_QUEUE.md「T2 后续：嵌套 JSON 解析」。

	t.Run("block_types 过滤：未在列表的实体不触发 block", func(t *testing.T) {
		px := newPrivacyTestProxy(t)
		req := newReqJSON("POST", "/v1/privacy/redact", `{
			"text": "联系张三，手机 13800138000",
			"gate_only": true,
			"block_types": ["api_key"]
		}`)
		rec := httptest.NewRecorder()
		px.PrivacyRedact(rec, req)
		require.Equal(t, 200, rec.Code)

		var resp struct {
			HasPII  bool `json:"has_pii"`
			Blocked bool `json:"blocked"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.True(t, resp.HasPII)
		require.False(t, resp.Blocked) // 命中 phone/person 但 block_types 仅 api_key → 不 block
	})

	t.Run("默认不回显命中原文，include_values=true 才回显", func(t *testing.T) {
		// 默认必须关：只要它默认开着，网关就同时是一个「提交文本 → 拿到其中 PII 原文」
		// 的提取接口。而 hooks 判定「拦还是放」只需要 type，不需要原文。
		px := newPrivacyTestProxy(t)
		req := newReqJSON("POST", "/v1/privacy/redact", `{
			"text": "联系张三，手机 13800138000",
			"gate_only": true
		}`)
		rec := httptest.NewRecorder()
		px.PrivacyRedact(rec, req)
		require.Equal(t, 200, rec.Code)

		var resp struct {
			HasPII   bool `json:"has_pii"`
			Entities []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"entities"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.True(t, resp.HasPII)
		require.NotEmpty(t, resp.Entities)
		for _, e := range resp.Entities {
			require.Empty(t, e.Value, "默认响应不得含命中原文（type=%s）", e.Type)
			require.NotEmpty(t, e.Type, "类型仍必须给出，否则调用方无法决定拦还是放")
		}
		// 注意：响应体的 text 字段是「入参原样回还」（gate_only 的语义就是只判不改），
		// 其中的原文来自调用方自己提交的内容，不是新增泄漏。真正的泄漏点是
		// entities[].value —— 它会把「检测到了什么」直接交出去，所以只钉那一处。
		entsJSON, err := json.Marshal(resp.Entities)
		require.NoError(t, err)
		require.NotContains(t, string(entsJSON), "13800138000")
		require.NotContains(t, string(entsJSON), "张三")

		// 显式索取时才回显
		req2 := newReqJSON("POST", "/v1/privacy/redact", `{
			"text": "联系张三，手机 13800138000",
			"gate_only": true,
			"include_values": true
		}`)
		rec2 := httptest.NewRecorder()
		px.PrivacyRedact(rec2, req2)
		require.Equal(t, 200, rec2.Code)

		var resp2 struct {
			Entities []struct {
				Value string `json:"value"`
			} `json:"entities"`
		}
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
		require.NotEmpty(t, resp2.Entities)
		require.NotEmpty(t, resp2.Entities[0].Value, "include_values=true 时应带回原文")
	})

	t.Run("无 PII 文本：has_pii=false", func(t *testing.T) {
		px := newPrivacyTestProxy(t)
		req := newReqJSON("POST", "/v1/privacy/redact", `{
			"text": "今天天气真好",
			"gate_only": true
		}`)
		rec := httptest.NewRecorder()
		px.PrivacyRedact(rec, req)
		require.Equal(t, 200, rec.Code)

		var resp struct {
			HasPII   bool `json:"has_pii"`
			Blocked  bool `json:"blocked"`
			Entities []any `json:"entities"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.False(t, resp.HasPII)
		require.False(t, resp.Blocked)
	})
}

// TestPrivacyRestore_OriginIsolation 端到端锁定「控制面 API 不能还原数据面映射表」。
//
// 攻击场景（改动前成立）：调用方持有控制面令牌，知道另一个 LLM 请求的 X-Request-ID
// （数据面的 ID 是客户端指定并回显的，日志里也常见），于是 POST /v1/privacy/restore
// 带上那个 ID 和一个占位符，把对方被脱敏掉的原文取回来。
//
// 与「猜不到 ID」无关：这里刻意用一个已知的、格式合法的 ID，验证的是来源本身。
func TestPrivacyRestore_OriginIsolation(t *testing.T) {
	ctx := context.Background()
	const (
		victimID    = "victim-request-id-0001"
		victimText  = "联系张三，手机 13800138000"
		victimPhone = "13800138000"
	)

	// 造一张「数据面」映射表：等价于某个 LLM 请求经 gateway 转发时留下的表。
	seedVictim := func(t *testing.T, px *Proxy) string {
		t.Helper()
		sess := px.proc.NewSession()
		ents, err := px.proc.DetectText(ctx, "", victimText)
		require.NoError(t, err)
		require.NotEmpty(t, ents, "前置条件：该文本必须能检出 PII")
		redacted, _, err := sess.Replace(victimText, ents)
		require.NoError(t, err)
		require.NotContains(t, redacted, victimPhone)
		require.NoError(t, px.proc.Store(victimID, "", sess.Entries()))
		return redacted
	}

	t.Run("数据面映射表：按已知 request_id 还原被拒且不泄漏原文", func(t *testing.T) {
		px := newPrivacyTestProxy(t)
		redacted := seedVictim(t, px)

		req := newReqJSON("POST", "/v1/privacy/restore",
			`{"request_id":"`+victimID+`","text":`+mustJSONString(t, redacted)+`}`)
		rec := httptest.NewRecorder()
		px.PrivacyRestore(rec, req)

		require.Equal(t, http.StatusNotFound, rec.Code)
		require.NotContains(t, rec.Body.String(), victimPhone, "原文不得出现在响应体")
		require.NotContains(t, rec.Body.String(), "张三")
	})

	t.Run("控制面映射表：同一 request_id 走 redact 建立后可正常还原", func(t *testing.T) {
		px := newPrivacyTestProxy(t)

		req := newReqJSON("POST", "/v1/privacy/redact",
			`{"text":`+mustJSONString(t, victimText)+`}`)
		rec := httptest.NewRecorder()
		px.PrivacyRedact(rec, req)
		require.Equal(t, 200, rec.Code)

		var redacted privacyRedactResp
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &redacted))
		require.NotEmpty(t, redacted.RequestID)
		require.True(t, redacted.Changed)

		restore := newReqJSON("POST", "/v1/privacy/restore",
			`{"request_id":"`+redacted.RequestID+`","text":`+mustJSONString(t, redacted.Text)+`}`)
		rec2 := httptest.NewRecorder()
		px.PrivacyRestore(rec2, restore)
		require.Equal(t, 200, rec2.Code)

		var restored privacyRestoreResp
		require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &restored))
		require.Contains(t, restored.Text, victimPhone)
		require.Contains(t, restored.Text, "张三")
	})

	t.Run("未知 request_id 与越权 request_id 返回同一状态码", func(t *testing.T) {
		px := newPrivacyTestProxy(t)
		redacted := seedVictim(t, px)

		attack := httptest.NewRecorder()
		px.PrivacyRestore(attack, newReqJSON("POST", "/v1/privacy/restore",
			`{"request_id":"`+victimID+`","text":`+mustJSONString(t, redacted)+`}`))

		probe := httptest.NewRecorder()
		px.PrivacyRestore(probe, newReqJSON("POST", "/v1/privacy/restore",
			`{"request_id":"no-such-request-id-here","text":"<<PHONE_1>>"}`))

		require.Equal(t, attack.Code, probe.Code,
			"越权与不存在必须不可区分，否则 403/404 之差就是一个 ID 存在性探针")
	})
}

// mustJSONString 把字符串编成 JSON 字面量（避免手写转义）。
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	return string(b)
}

func newReqJSON(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// 防止 go vet 报 "imported and not used: time"
var _ = time.Second
