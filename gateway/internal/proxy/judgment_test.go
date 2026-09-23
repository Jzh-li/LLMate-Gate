package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/internal/judge"
	"gateway/internal/metrics"
)

// attachJudge 给测试代理挂上判断层（默认 rules 后端）与指标收集器。
func attachJudge(t *testing.T, tp *testProxy, mode string) *metrics.Collectors {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	tp.px.m = m
	eval, err := judge.NewFromSpecs(
		[]judge.Spec{{Name: "rules", Kind: judge.KindRules}},
		judge.EvaluatorOptions{FailClosed: true},
	)
	require.NoError(t, err)
	tp.px.WithJudge(eval, mode)
	return m
}

const archiveToolCallBody = `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"tar -czf repo.tar.gz .\"}"}}]}]}`

const benignToolCallBody = `{"model":"m","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls -la\"}"}}]}]}`

func runProxy(t *testing.T, tp *testProxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	tp.px.Handle(rec, req, "chat.completions", false)
	return rec
}

// S2 的硬约束：判断层挂在热路径上，但**绝不改变现有行为**。
//
// 这条测试用最直接的方式守住它：同一请求跑两遍（挂 / 不挂判断层），
// 上游收到的字节与客户端收到的字节都必须逐字节相同。
func TestProxy_JudgmentShadow_DoesNotChangeTraffic(t *testing.T) {
	base := newTestProxy(t)
	defer base.closeUp()
	recBase := runProxy(t, base, archiveToolCallBody)
	require.Equal(t, 200, recBase.Code)

	withJudge := newTestProxy(t)
	defer withJudge.closeUp()
	m := attachJudge(t, withJudge, "shadow")
	recJudge := runProxy(t, withJudge, archiveToolCallBody)
	require.Equal(t, 200, recJudge.Code)

	assert.Equal(t, base.lastUp(), withJudge.lastUp(),
		"影子模式绝不能改变上游看到的字节")
	assert.Equal(t, recBase.Body.String(), recJudge.Body.String(),
		"影子模式绝不能改变客户端看到的响应")

	// 指标必须真的有数据，否则「不改变行为」等于「什么都没做」。
	got := testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules"))
	assert.Equal(t, float64(1), got, "打包整仓应被判为 block")
}

func TestProxy_JudgmentRecordsBenignWithoutNoise(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	runProxy(t, tp, benignToolCallBody)

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("allow", "benign", "rules")))
}

// Anthropic 形态：tool_use 的 input 才是参数对象。这条覆盖 observe 分支里
// 「type 必须命中 tool_use」的判断——不判 type 会把普通消息体的 input 误当工具调用。
func TestProxy_JudgmentAnthropicToolUse(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	body := `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"tar -czf repo.tar.gz ."}}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	tp.px.Handle(rec, req, "messages", false)
	require.Equal(t, 200, rec.Code)

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")))
}

// 没有工具调用的请求不该产生任何裁决 —— 判断层的触发面必须精确，
// 否则「每个请求都判一次」会把开销与噪声一起放大。
func TestProxy_JudgmentIgnoresPlainRequests(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	rec := runProxy(t, tp, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	require.Equal(t, 200, rec.Code)

	assert.Equal(t, 0, testutil.CollectAndCount(m.VerdictTotal),
		"普通消息不该触发判断")
	assert.Equal(t, 0, testutil.CollectAndCount(m.JudgeLatency))
}

// 未挂判断层时，热路径上不该有任何判定开销（连指标都不该出现）。
func TestProxy_JudgmentDisabledProducesNothing(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := metrics.New(prometheus.NewRegistry())
	tp.px.m = m
	require.False(t, tp.px.JudgeEnabled())

	runProxy(t, tp, archiveToolCallBody)

	assert.Equal(t, 0, testutil.CollectAndCount(m.VerdictTotal))
	assert.Equal(t, 0, testutil.CollectAndCount(m.JudgeUnavailable))
}

// 后端整体不可用时，判断层要留下痕迹而不是静默通过：
// 「没看到」和「没问题」在运维上是两件事。
func TestProxy_JudgmentUnavailableIsVisible(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := metrics.New(prometheus.NewRegistry())
	tp.px.m = m

	// 指向一个没有服务的地址的超时后端，且**不放 rules 兜底**，
	// 这样链上唯一后端必然失败。
	eval, err := judge.NewFromSpecs([]judge.Spec{{
		Name: "dead", Kind: judge.KindOpenAI,
		BaseURL: "http://127.0.0.1:1/v1", Model: "none", Timeout: 50 * 1e6, // 50ms
	}}, judge.EvaluatorOptions{FailClosed: true})
	require.NoError(t, err)
	tp.px.WithJudge(eval, "shadow")

	rec := runProxy(t, tp, archiveToolCallBody)
	require.Equal(t, 200, rec.Code, "判定失败不影响转发：这是旁挂层")

	require.Equal(t, 1, testutil.CollectAndCount(m.JudgeUnavailable))
	assert.Equal(t, 0, testutil.CollectAndCount(m.VerdictTotal),
		"失败不是「判了一个结果」，不能混进裁决计数")
}

// arguments 为对象形态（部分 SDK 已展开）时同样要判到。
func TestProxy_JudgmentWithObjectArguments(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":{"command":"tar -czf repo.tar.gz ."}}}]}]}`
	runProxy(t, tp, body)

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")))
}

// arguments 是非 JSON 的纯字符串（有些工具直接传命令）时也要能判。
func TestProxy_JudgmentWithRawStringArguments(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"shell","arguments":"tar -czf repo.tar.gz ."}}]}]}`
	runProxy(t, tp, body)

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")))
}

// 参数是字符串数组（命令数组形态）时也要能判。
func TestProxy_JudgmentWithArrayArguments(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":["tar","-czf","repo.tar.gz","."]}}]}]}`
	runProxy(t, tp, body)

	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")))
}

// 多轮对话里旧轮次的 tool_call 会被反复看到：每次请求都会重判一次。
// 这不是 bug（判断层无状态），但要在测试里钉住，避免以后有人误以为它做了去重。
func TestProxy_JudgmentIsStatelessAcrossTurns(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	runProxy(t, tp, archiveToolCallBody)
	runProxy(t, tp, archiveToolCallBody)

	assert.Equal(t, float64(2),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")),
		"判断层无状态：每轮请求各判一次")
}

// 判定的输入必须来自 arguments，而不是被脱敏后的占位符文本。
// 这条验证接线位置正确：observe 在脱敏之前跑，看到的是原文。
func TestProxy_JudgmentSeesOriginalCommand(t *testing.T) {
	tp := newTestProxy(t)
	defer tp.closeUp()
	m := attachJudge(t, tp, "shadow")

	// 命令里带一个手机号（会被脱敏），但命令结构必须仍被看清。
	body := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"tar -czf repo.tar.gz . # 13800138000\"}"}}]}]}`
	runProxy(t, tp, body)

	var up map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(tp.lastUp()), &up))
	// 上游侧手机号已被替换（脱敏生效）。
	require.NotContains(t, tp.lastUp(), "13800138000")
	// 判断结果仍正确（命令结构被看清）。
	assert.Equal(t, float64(1),
		testutil.ToFloat64(m.VerdictTotal.WithLabelValues("block", "archive", "rules")))
}
