package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/cache"
	"gateway/internal/detector"
	"gateway/internal/pipeline"
	"gateway/internal/replacer"
	"gateway/internal/simulator"
	"gateway/internal/vault"
	"gateway/pkg/types"
)

const testTTL = 30 * time.Minute

// testProxy 自带一个「回声」上游，把收到的（已脱敏）内容原样返回，便于验证还原。
type testProxy struct {
	px          *Proxy
	closeUp     func()
	lastUpBody  string
	mu          sync.Mutex
}

// flushingRecorder 让 httptest.ResponseRecorder 支持 http.Flusher（流式响应必需）。
type flushingRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushingRecorder) Flush() {}

func newTestProxy(t *testing.T) *testProxy {
	t.Helper()
	v, err := vault.NewMemVault(testTTL, []byte("unit-test-passphrase"), false, "")
	require.NoError(t, err)
	det := detector.NewRegexEngine(detector.WithThresholds(map[string]float64{
		"zh_person_name": 0.5, "zh_phone": 0.8,
	}))
	repl := replacer.New(replacer.Config{
		Strategy:     "placeholder",
		Irreversible: []string{"api_key", "password", "token"},
		Simulate:     simulator.SimulateZHConfig{},
		SessionKey:   []byte("session-key-32-bytes-long!!!"),
	}, v)
	proc := pipeline.New(pipeline.Config{Detector: det, Replacer: repl, Vault: v, FailClosed: true})

	tp := &testProxy{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tp.mu.Lock()
		tp.lastUpBody = string(b)
		tp.mu.Unlock()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Input string `json:"input"`
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(b, &req)

		if req.Stream {
			// 把脱敏内容切成两段合法的 SSE 事件。占位符（<<...>>）保持完整落在第一块，
			// 真实 SSE 下单个 data 行内占位符字节是连续的；跨事件内容由客户端拼接。
			// 网关逐事件还原：第一块内占位符被还原，第二块透传；客户端拼接 content 即得原文。
			content := req.Messages[0].Content
			// 必须在 rune 边界切分（content 是中文，UTF-8 多字节），用 []rune + string() 转换
			// 避免切到码点中间导致 json.Marshal 插入 U+FFFD 替换字符。
			rc := []rune(content)
			mid := len(rc) / 2
			c1 := string(rc[:mid])
			c2 := string(rc[mid:])
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			// 第一块 + 第二块 + DONE，各自完整 JSON（必须用 "\n\n" 双引号，
			// 反引号 raw string 里 \n 是字面两字节，不是真换行）。
			_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":` + jsonQuote(c1) + `}}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":` + jsonQuote(c2) + `}}]}` + "\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		content := req.Input
		if len(req.Messages) > 0 {
			content = req.Messages[0].Content
		}
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"role": "assistant", "content": content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false) // 模拟真实 LLM：占位符以字面量返回，不被 HTML 转义
		_ = enc.Encode(resp)
	}))
	tp.px = New(proc, mustParse(t, up.URL), "", "", nil, false, nil)
	tp.closeUp = up.Close
	t.Cleanup(up.Close)
	return tp
}

func jsonQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// collectStreamContent 模拟 SSE 客户端：按行解析 data: 事件，拼接各事件 choices[].delta.content。
func collectStreamContent(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if len(ev.Choices) > 0 {
			sb.WriteString(ev.Choices[0].Delta.Content)
		}
	}
	return sb.String()
}

func writeSSE(w http.ResponseWriter, f http.Flusher, payload string) {
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
	if f != nil {
		f.Flush()
	}
}
func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestProxy_ChatCompletions_Restore 非流式：请求脱敏 + 响应还原（Phase 1 验收）。
func TestProxy_ChatCompletions_Restore(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()

	body := `{"messages":[{"role":"user","content":"我叫张三，手机13800138000"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	tp.px.Handle(rec, req, "chat.completions", false)

	require.Equal(t, 200, rec.Code)
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	content := out.Choices[0].Message.Content
	require.Contains(t, content, "张三")
	require.Contains(t, content, "13800138000")
	require.NotContains(t, content, "<<", "占位符必须被还原")
}

// TestProxy_ChatCompletions_StreamRestore 流式：占位符跨 SSE 事件还原，客户端拼接 content 拼回原文。
func TestProxy_ChatCompletions_StreamRestore(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()

	body := `{"messages":[{"role":"user","content":"张三的订单已处理"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := &flushingRecorder{httptest.NewRecorder()}

	tp.px.Handle(rec, req, "chat.completions", true)

	require.Equal(t, 200, rec.Code)
	full := rec.Body.String()
	require.NotContains(t, full, "<<", "流式不得残留占位符")
	// 客户端按 SSE 协议拼接各事件 content delta，须还原为原文。
	require.Equal(t, "张三的订单已处理", collectStreamContent(t, full), "流式还原必须拼回原文")
}

// TestProxy_Embeddings_Anonymize embeddings.input 必须脱敏后再转发上游。
func TestProxy_Embeddings_Anonymize(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()

	body := `{"input":"联系人张三的邮箱"}`
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	rec := httptest.NewRecorder()

	tp.px.Handle(rec, req, "embeddings", false)
	require.Equal(t, 200, rec.Code)

	tp.mu.Lock()
	up := tp.lastUpBody
	tp.mu.Unlock()
	require.Contains(t, up, "<<zh_person_name_1>>", "上游收到的 input 必须已脱敏")
	require.NotContains(t, up, "张三", "上游不得看到明文")
}

// failDetector 永远返回错误的检测器，用于验证 fail-closed 阻断。
type failDetector struct{}

func (failDetector) Detect(ctx context.Context, _ *types.DetectRequest) (*types.DetectResponse, error) {
	return nil, gatewayerrors.New(gatewayerrors.CodeDetectorUnavailable, "injected failure")
}
func (failDetector) DetectBatch(ctx context.Context, _ []*types.DetectRequest) ([]*types.DetectResponse, error) {
	return nil, gatewayerrors.New(gatewayerrors.CodeDetectorUnavailable, "injected failure")
}
func (failDetector) Health(ctx context.Context) error { return nil }
func (failDetector) Name() string                              { return "fail" }

// TestProxy_FailClosed_Blocks fail_closed=true 时检测异常必须阻断（502）。
func TestProxy_FailClosed_Blocks(t *testing.T) {
	v, err := vault.NewMemVault(testTTL, []byte("unit-test-passphrase"), false, "")
	require.NoError(t, err)
	repl := replacer.New(replacer.Config{
		Strategy: "placeholder", Irreversible: []string{"api_key", "password", "token"},
		Simulate: simulator.SimulateZHConfig{}, SessionKey: []byte("session-key-32-bytes-long!!!"),
	}, v)
	proc := pipeline.New(pipeline.Config{
		Detector: failDetector{}, Replacer: repl, Vault: v, FailClosed: true,
	})
	px := New(proc, mustParse(t, "http://127.0.0.1:1"), "", "", nil, false, nil)

	body := `{"messages":[{"role":"user","content":"我叫张三"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	px.Handle(rec, req, "chat.completions", false)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "fail-closed 必须阻断 (契约 §0.3: detector_unavailable=500)")
	var out map[string]map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Equal(t, string(gatewayerrors.CodeDetectorUnavailable), out["error"]["code"])
}

// lastUp 线程安全地读取上游收到的（已脱敏）请求体。
func (tp *testProxy) lastUp() string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.lastUpBody
}

// reAnonPlaceholder 匹配脱敏后的占位符（仅校验形态，不依赖编号顺序）。
var reAnonPlaceholder = regexp.MustCompile(`^<<(zh_phone|email|zh_person_name)_\d+>>$`)

// TestProxy_ToolCall_ArgumentsString tool_calls.function.arguments 为 JSON 字符串：
// 键必须保留，值逐值脱敏；api_key 等不可逆类型 → [REDACTED]，可逆类型 → 占位符。
func TestProxy_ToolCall_ArgumentsString(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()

	body := `{"messages":[{"role":"user","content":"请调用 lookup 工具"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"api_key\":\"sk-1234567890abcdef\",\"phone\":\"13800138000\",\"email\":\"zhangsan@example.com\"}"}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	tp.px.Handle(rec, req, "chat.completions", false)

	require.Equal(t, 200, rec.Code)

	var up map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(tp.lastUp()), &up))
	tcs := up["tool_calls"].([]interface{})
	fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
	args, ok := fn["arguments"].(map[string]interface{})
	require.True(t, ok, "arguments 应为脱敏后的对象")

	// 键保留
	require.Contains(t, args, "api_key")
	require.Contains(t, args, "phone")
	require.Contains(t, args, "email")

	// 值脱敏：不可逆 → [REDACTED]
	require.Equal(t, replacer.RedactedText, args["api_key"])
	// 可逆 → 占位符（编号顺序不敏感）
	require.Regexp(t, reAnonPlaceholder, args["phone"])
	require.Regexp(t, reAnonPlaceholder, args["email"])

	// 明文绝不上游
	raw := tp.lastUp()
	require.NotContains(t, raw, "sk-1234567890abcdef")
	require.NotContains(t, raw, "13800138000")
	require.NotContains(t, raw, "zhangsan@example.com")
}

// TestProxy_ToolCall_ArgumentsObject arguments 已是对象（部分 SDK 展开）：同样键保留、值脱敏。
func TestProxy_ToolCall_ArgumentsObject(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()

	body := `{"messages":[{"role":"user","content":"请调用 lookup 工具"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":{"api_key":"sk-1234567890abcdef","phone":"13800138000","email":"zhangsan@example.com"}}}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	tp.px.Handle(rec, req, "chat.completions", false)

	require.Equal(t, 200, rec.Code)

	var up map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(tp.lastUp()), &up))
	tcs := up["tool_calls"].([]interface{})
	fn := tcs[0].(map[string]interface{})["function"].(map[string]interface{})
	args := fn["arguments"].(map[string]interface{})

	require.Equal(t, replacer.RedactedText, args["api_key"])
	require.Regexp(t, reAnonPlaceholder, args["phone"])
	require.Regexp(t, reAnonPlaceholder, args["email"])

	raw := tp.lastUp()
	require.NotContains(t, raw, "sk-1234567890abcdef")
	require.NotContains(t, raw, "13800138000")
	require.NotContains(t, raw, "zhangsan@example.com")
}

// countingDetector 包装真实检测器，统计实际检测调用次数（验证 Merkle 增量只扫新增段）。
type countingDetector struct {
	detector.Client
	mu    sync.Mutex
	calls int
}

func (c *countingDetector) Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.Client.Detect(ctx, req)
}

func (c *countingDetector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newIncrementalProxy 构造带 Merkle 增量缓存 + 计数检测器的代理（用于多轮对话验收）。
// 上游直接把收到的（已脱敏）请求体原样回显，便于断言；同时捕获该 body。
func newIncrementalProxy(t *testing.T) (*Proxy, *cache.MerkleCache, *countingDetector, func() string) {
	t.Helper()
	v, err := vault.NewMemVault(testTTL, []byte("unit-test-passphrase"), false, "")
	require.NoError(t, err)
	det := &countingDetector{Client: detector.NewRegexEngine(detector.WithThresholds(map[string]float64{
		"zh_person_name": 0.5, "zh_phone": 0.8,
	}))}
	repl := replacer.New(replacer.Config{
		Strategy: "placeholder", Irreversible: []string{"api_key", "password", "token"},
		Simulate: simulator.SimulateZHConfig{}, SessionKey: []byte("session-key-32-bytes-long!!!"),
	}, v)
	proc := pipeline.New(pipeline.Config{Detector: det, Replacer: repl, Vault: v, FailClosed: true})
	mk := cache.NewMerkle(testTTL)

	var (
		mu     sync.Mutex
		lastUp string
	)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastUp = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	px := New(proc, mustParse(t, up.URL), "", "", nil, false, mk)
	return px, mk, det, func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastUp
	}
}

// TestProxy_ConversationIncremental 多轮对话：仅新增 turn 走检测，命中前缀的旧段零检测；
// 全局占位符唯一，不同值跨段不碰撞。
func TestProxy_ConversationIncremental(t *testing.T) {
	px, _, det, lastUp := newIncrementalProxy(t)
	const conv = "conv-inc-1"

	// Turn 1：单条 user 消息
	body1 := `{"messages":[{"role":"user","content":"我叫李雷，手机13900139000"}],"conversation_id":"conv-inc-1"}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body1))
	req1.Header.Set("X-Conversation-ID", conv)
	rec1 := httptest.NewRecorder()
	px.Handle(rec1, req1, "chat.completions", false)
	require.Equal(t, 200, rec1.Code)
	require.Equal(t, 1, det.count(), "turn1 只检测 1 段")

	// Turn 2：在尾部追加 2 条新消息（前缀段完全复用）
	body2 := `{"messages":[{"role":"user","content":"我叫李雷，手机13900139000"},{"role":"assistant","content":"好的"},{"role":"user","content":"我叫韩梅梅，手机13700137000"}],"conversation_id":"conv-inc-1"}`
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body2))
	req2.Header.Set("X-Conversation-ID", conv)
	rec2 := httptest.NewRecorder()
	px.Handle(rec2, req2, "chat.completions", false)
	require.Equal(t, 200, rec2.Code)

	// 增量验证：turn2 只新增检测 2 段（前缀段零检测），累计 = 3
	require.Equal(t, 3, det.count(), "Merkle 增量：仅扫新增 turn")

	// 上游收到的脱敏体：两段手机号都被脱敏且占位符唯一不碰撞
	up := lastUp()
	require.Contains(t, up, "<<zh_phone_1>>")
	require.Contains(t, up, "<<zh_phone_2>>")
	require.NotContains(t, up, "13900139000")
	require.NotContains(t, up, "13700137000")
}



