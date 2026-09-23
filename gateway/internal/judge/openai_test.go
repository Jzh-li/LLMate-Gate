package judge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

// fakeOpenAI 起一个本地假 OpenAI 服务（S3：真模型之前先把协议与边界测完）。
//
// 这不是「简陋的替身」：S4 接上真模型之后如果表现异常，有这层就能立刻区分
// 「是骨架/协议错」还是「是模型差」。没有它，两类问题会混成一个现象。
func fakeOpenAI(t *testing.T, status int, body string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// chatJSON 包一层 chat/completions 响应外壳。
func chatJSON(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	return string(b)
}

func newTestBackend(t *testing.T, srv *httptest.Server, mode types.SchemaMode) *OpenAICompat {
	t.Helper()
	return NewOpenAICompat(OpenAIOptions{
		Name: "local", BaseURL: srv.URL, Model: "test-model",
		SchemaMode: mode, Timeout: 2 * time.Second,
	})
}

func TestOpenAICompat_HappyPath(t *testing.T) {
	srv, seen := fakeOpenAI(t, 200, chatJSON(`{"category":"archive","severity":0.9,"confidence":0.8,"reasons":["打包当前目录"]}`))
	b := newTestBackend(t, srv, types.SchemaPromptOnly)

	ev, err := b.Judge(context.Background(), sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.CatArchive, ev.Category)
	assert.InDelta(t, 0.9, ev.Severity, 1e-9)
	assert.InDelta(t, 0.8, ev.Confidence, 1e-9)
	assert.Equal(t, "local", ev.Engine)
	assert.NotEmpty(t, ev.Reasons)

	// 请求体必须带上提示词与动作描述——模型看不到动作就无从判断。
	assert.Equal(t, "test-model", (*seen)["model"])
	assert.Equal(t, float64(0), (*seen)["temperature"], "判定必须用温度 0")
	msgs, ok := (*seen)["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 2)
	user := msgs[1].(map[string]any)["content"].(string)
	assert.Contains(t, user, "tar -czf repo.tar.gz .")
}

func TestOpenAICompat_ConstraintModes(t *testing.T) {
	cases := []struct {
		mode      types.SchemaMode
		check     func(t *testing.T, req map[string]any)
		expectKey string
	}{
		{
			mode: types.SchemaJSONSchema,
			check: func(t *testing.T, req map[string]any) {
				rf, ok := req["response_format"].(map[string]any)
				require.True(t, ok, "json_schema 模式必须带 response_format")
				assert.Equal(t, "json_schema", rf["type"])
				js, ok := rf["json_schema"].(map[string]any)
				require.True(t, ok)
				schema, ok := js["schema"].(map[string]any)
				require.True(t, ok)
				props := schema["properties"].(map[string]any)
				enum := props["category"].(map[string]any)["enum"].([]any)
				assert.Len(t, enum, len(types.AllCategories()), "枚举必须与闭集一致")
			},
		},
		{
			mode: types.SchemaGBNF,
			check: func(t *testing.T, req map[string]any) {
				g, ok := req["grammar"].(string)
				require.True(t, ok, "gbnf 模式必须带 grammar")
				assert.Contains(t, g, "archive")
				assert.Contains(t, g, "unknown")
			},
		},
		{
			mode: types.SchemaFormat,
			check: func(t *testing.T, req map[string]any) {
				rf := req["response_format"].(map[string]any)
				assert.Equal(t, "json_object", rf["type"])
			},
		},
		{
			mode: types.SchemaPromptOnly,
			check: func(t *testing.T, req map[string]any) {
				_, hasRF := req["response_format"]
				_, hasGram := req["grammar"]
				assert.False(t, hasRF, "prompt_only 不应加 response_format")
				assert.False(t, hasGram)
			},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			srv, seen := fakeOpenAI(t, 200, chatJSON(`{"category":"benign","severity":0,"confidence":0.9}`))
			b := newTestBackend(t, srv, tc.mode)
			_, err := b.Judge(context.Background(), sampleDesc())
			require.NoError(t, err)
			tc.check(t, *seen)
		})
	}
}

// 边界矩阵：这些是「模型胡说」与「服务异常」的分界线，各自对应一条不同的降级路径。
func TestOpenAICompat_BoundaryMatrix(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"闭集外的 category", 200, chatJSON(`{"category":"malware","severity":0.9}`), ErrInvalidOutput},
		{"缺 category", 200, chatJSON(`{"severity":0.9}`), ErrInvalidOutput},
		{"完全不是 JSON", 200, chatJSON("我不知道该怎么办"), ErrInvalidOutput},
		{"空 choices", 200, `{"choices":[]}`, ErrInvalidOutput},
		{"服务端报错字段", 200, `{"error":{"message":"model not found"}}`, ErrUnavailable},
		{"HTTP 500", 500, `{"error":"boom"}`, ErrUnavailable},
		{"HTTP 404", 404, `not found`, ErrUnavailable},
		{"响应不是 JSON", 200, `<html>oops</html>`, ErrInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fakeOpenAI(t, tc.status, tc.body)
			b := newTestBackend(t, srv, types.SchemaPromptOnly)
			_, err := b.Judge(context.Background(), sampleDesc())
			require.Error(t, err)
			assert.Truef(t, errors.Is(err, tc.wantErr), "want %v, got %v", tc.wantErr, err)
		})
	}
}

// fenced / 夹带解释的输出必须仍能被解析——prompt_only 模式下这是常态。
func TestOpenAICompat_TolerantParsing(t *testing.T) {
	contents := []string{
		"```json\n{\"category\":\"archive\",\"severity\":0.9,\"confidence\":0.8}\n```",
		"好的，我的判断如下：\n{\"category\":\"archive\",\"severity\":0.9,\"confidence\":0.8}\n希望有帮助。",
		"{\"category\":\"archive\",\"severity\":0.9,\"confidence\":0.8,\"reasons\":[\"含 {大括号} 的说明\"]}",
	}
	for _, c := range contents {
		srv, _ := fakeOpenAI(t, 200, chatJSON(c))
		b := newTestBackend(t, srv, types.SchemaPromptOnly)
		ev, err := b.Judge(context.Background(), sampleDesc())
		require.NoErrorf(t, err, "content=%q", c)
		assert.Equal(t, types.CatArchive, ev.Category)
	}
}

func TestOpenAICompat_ClampsOutOfRangeNumbers(t *testing.T) {
	srv, _ := fakeOpenAI(t, 200, chatJSON(`{"category":"archive","severity":7,"confidence":-3}`))
	b := newTestBackend(t, srv, types.SchemaPromptOnly)
	ev, err := b.Judge(context.Background(), sampleDesc())
	require.NoError(t, err)
	assert.InDelta(t, 1, ev.Severity, 1e-9, "越界数值是钳制而不是报错：模型普遍会写 1.5 这类值")
	assert.InDelta(t, 0, ev.Confidence, 1e-9)
}

func TestOpenAICompat_InputTooLarge(t *testing.T) {
	srv, seen := fakeOpenAI(t, 200, chatJSON(`{"category":"benign"}`))
	b := NewOpenAICompat(OpenAIOptions{
		Name: "local", BaseURL: srv.URL, Model: "m",
		Timeout: time.Second, MaxInputBytes: 16,
	})
	_, err := b.Judge(context.Background(), sampleDesc())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInputTooLarge))
	assert.Nil(t, *seen, "超长输入必须在发出请求之前就被拒绝，不能靠上游截断")
}

func TestOpenAICompat_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(chatJSON(`{"category":"benign"}`)))
	}))
	defer srv.Close()

	b := NewOpenAICompat(OpenAIOptions{
		Name: "slow", BaseURL: srv.URL, Model: "m", Timeout: 50 * time.Millisecond,
	})
	_, err := b.Judge(context.Background(), sampleDesc())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnavailable), "超时必须降级，不能变成「没发现风险」")
}

func TestOpenAICompat_Health(t *testing.T) {
	srv, _ := fakeOpenAI(t, 200, "{}")
	b := newTestBackend(t, srv, types.SchemaPromptOnly)
	require.NoError(t, b.Health(context.Background()))

	// 端口上没有服务 → Health 必须报不可用。
	dead := NewOpenAICompat(OpenAIOptions{
		Name: "dead", BaseURL: "http://127.0.0.1:1/v1", Model: "m", Timeout: 100 * time.Millisecond,
	})
	require.Error(t, dead.Health(context.Background()))
}

// 判断层是「用来防数据外泄的组件」，它自己的出网路径必须被钉死。
func TestNewLocalHTTPClient_Hardening(t *testing.T) {
	c := newLocalHTTPClient(time.Second)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Nil(t, tr.Proxy, "必须显式禁用代理：http_proxy 会把「本机后端」的请求带到别的机器上")

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	require.Error(t, c.CheckRedirect(req, nil), "不得跟随重定向：否则 127.0.0.1 上的服务可以把请求转到公网")
}

func TestParseEndpoint(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:11434/v1": "127.0.0.1:11434",
		"http://localhost:8080":     "localhost:8080",
		"127.0.0.1:9000":            "127.0.0.1:9000",
		"http://example.com":        "example.com:80",
	}
	for in, want := range cases {
		got, err := parseEndpoint(in)
		require.NoErrorf(t, err, "in=%s", in)
		assert.Equal(t, want, got)
	}
	_, err := parseEndpoint("http://")
	require.Error(t, err)
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"a":1}`, `{"a":1}`, true},
		{"前缀 {\"a\":1} 后缀", `{"a":1}`, true},
		{"```json\n{\"a\":{\"b\":2}}\n```", `{"a":{"b":2}}`, true},
		{`{"s":"带 } 的字符串"}`, `{"s":"带 } 的字符串"}`, true},
		{`no json here`, "", false},
		{`{"unclosed":1`, "", false},
	}
	for _, tc := range cases {
		got, ok := extractJSONObject(tc.in)
		assert.Equalf(t, tc.ok, ok, "in=%q", tc.in)
		if tc.ok {
			assert.Equalf(t, tc.want, got, "in=%q", tc.in)
		}
	}
}

func TestHTTPEndpoint_ProtocolAndContract(t *testing.T) {
	var got types.ActionDescriptor
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"category":"exfil","severity":0.95,"confidence":0.9,"reasons":["打包后外发"]}`))
	}))
	defer srv.Close()

	b := NewHTTPEndpoint(HTTPOptions{Name: "custom", URL: srv.URL, Timeout: time.Second})
	ev, err := b.Judge(context.Background(), sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.CatExfil, ev.Category)
	assert.Equal(t, "tar -czf repo.tar.gz .", got.Command, "逃生口收到的是 ActionDescriptor 原文")

	// 逃生口放宽的是协议，不是契约：闭集外一样拒。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"category":"whatever","severity":1}`))
	}))
	defer srv2.Close()
	_, err = NewHTTPEndpoint(HTTPOptions{Name: "bad", URL: srv2.URL, Timeout: time.Second}).
		Judge(context.Background(), sampleDesc())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidOutput))
}

func TestDescribeAction(t *testing.T) {
	d := sampleDesc()
	d.Meta = map[string]string{"file_count": "120", "entropy": "7.9"}
	s := describeAction(d)
	assert.Contains(t, s, "tar -czf repo.tar.gz .")
	assert.Contains(t, s, "executed")
	assert.Contains(t, s, "file_count")
	assert.True(t, strings.HasPrefix(s, "待判断的动作："))
}
