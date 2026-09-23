package judge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

func TestMapper_ThresholdBands(t *testing.T) {
	m := NewMapper("m", types.DefaultThresholds(), false) // block .85 review .55 redact .30
	cases := []struct {
		sev  float64
		want types.Action
	}{
		{0, types.ActionAllow},
		{0.29, types.ActionAllow},
		{0.30, types.ActionRedact},
		{0.549, types.ActionRedact},
		{0.55, types.ActionReview},
		{0.849, types.ActionReview},
		{0.85, types.ActionBlock},
		{1, types.ActionBlock},
	}
	for _, tc := range cases {
		v := m.Map(types.Evidence{Category: types.CatBulkRead, Severity: tc.sev})
		assert.Equalf(t, tc.want, v.Action, "severity=%.3f 应落 %s（实际 %s）", tc.sev, tc.want, v.Action)
	}
}

// unknown 是一等公民：后端说「判不了」时，落点是交人，不是放行。
func TestMapper_UnknownAlwaysReview(t *testing.T) {
	m := NewMapper("m", types.DefaultThresholds(), false)
	for _, sev := range []float64{0, 0.5, 1} {
		v := m.Map(types.Evidence{Category: types.CatUnknown, Severity: sev})
		assert.Equalf(t, types.ActionReview, v.Action, "unknown(sev=%.1f) 必须交人复核", sev)
	}
}

// 置信地板：不可信的高危结论不能触发不可逆动作，不可信的低危结论也不能被放行。
func TestMapper_LowConfidenceFallsBackToReview(t *testing.T) {
	m := NewMapper("m", types.DefaultThresholds(), true)

	v := m.Map(types.Evidence{Category: types.CatArchive, Severity: 0.99, Confidence: 0.4})
	assert.Equal(t, types.ActionReview, v.Action, "低置信的高危结论不能直接 block")

	v = m.Map(types.Evidence{Category: types.CatBenign, Severity: 0.0, Confidence: 0.1})
	assert.Equal(t, types.ActionReview, v.Action, "低置信的低危结论也不能被当成放行")

	// 达到地板后 confidence 才参与加权：0.9 * 0.6 = 0.54 → redact 档。
	v = m.Map(types.Evidence{Category: types.CatArchive, Severity: 0.9, Confidence: 0.6})
	assert.Equal(t, types.ActionRedact, v.Action)
}

func TestMapper_ConfidenceIgnoredWhenNotDeclared(t *testing.T) {
	// 后端没声明 GivesConfidence（如 rules）→ confidence 不参与任何计算。
	m := NewMapper("m", types.DefaultThresholds(), false)
	v := m.Map(types.Evidence{Category: types.CatArchive, Severity: 0.9, Confidence: 0})
	assert.Equal(t, types.ActionBlock, v.Action)
}

// benign 不做特例：自相矛盾的输出（benign + 高 severity）应取更严的一侧。
func TestMapper_BenignNotSpecialCased(t *testing.T) {
	m := NewMapper("m", types.DefaultThresholds(), false)
	assert.Equal(t, types.ActionBlock, m.Map(types.Evidence{Category: types.CatBenign, Severity: 0.9}).Action)
	assert.Equal(t, types.ActionAllow, m.Map(types.Evidence{Category: types.CatBenign, Severity: 0}).Action)
}

func TestMapper_Deterministic(t *testing.T) {
	// 同输入必同输出：这是可进 CI 回归的前提。
	m := NewMapper("m", types.DefaultThresholds(), true)
	ev := types.Evidence{Category: types.CatBulkRead, Severity: 0.7, Confidence: 0.9}
	first := m.Map(ev)
	for i := 0; i < 50; i++ {
		assert.Equal(t, first.Action, m.Map(ev).Action)
	}
}

func TestMapper_FillsEngine(t *testing.T) {
	m := NewMapper("engine-default", types.DefaultThresholds(), false)
	v := m.Map(types.Evidence{Category: types.CatBenign})
	assert.Equal(t, "engine-default", v.Engine)
	// 后端已填的 Engine 不被覆盖。
	v = m.Map(types.Evidence{Category: types.CatBenign, Engine: "explicit"})
	assert.Equal(t, "explicit", v.Engine)
}

func TestNewEvaluator_Validation(t *testing.T) {
	_, err := NewEvaluator(nil, EvaluatorOptions{})
	require.Error(t, err, "没有后端的求值器无意义")

	_, err = NewEvaluator([]Judge{benignStub("dup"), benignStub("dup")}, EvaluatorOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate backend name")

	_, err = NewEvaluator([]Judge{benignStub("")}, EvaluatorOptions{})
	require.Error(t, err)
}

func TestEvaluator_WhitelistShortCircuitsBackends(t *testing.T) {
	b := catStub("local", types.CatArchive, 0.99, 0)
	e, err := NewEvaluator([]Judge{b}, EvaluatorOptions{Whitelist: Whitelist{Tools: []string{"Bash"}}})
	require.NoError(t, err)

	v, err := e.Evaluate(context.Background(), sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.ActionAllow, v.Action)
	assert.Equal(t, "whitelist", v.Engine)
	assert.Zero(t, b.calls, "白名单命中时不应调用后端")
}

func TestEvaluator_PerBackendThresholds(t *testing.T) {
	ctx := context.Background()
	// 同一份证据（archive, 0.6），两个后端用各自的阈值表得到不同动作。
	// 这正是 per-backend 阈值的意义：confidence 跨后端不可比，阈值也不能共用。
	strict, err := NewEvaluator([]Judge{catStub("strict", types.CatArchive, 0.6, 0)}, EvaluatorOptions{})
	require.NoError(t, err)
	strict.SetMapper("strict", types.JudgmentThresholds{Block: 0.9, Review: 0.7, Redact: 0.1}, false)
	v, err := strict.Evaluate(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.ActionRedact, v.Action, "0.6 低于该后端的 review 阈值")

	lax, err := NewEvaluator([]Judge{catStub("lax", types.CatArchive, 0.6, 0)}, EvaluatorOptions{})
	require.NoError(t, err)
	lax.SetMapper("lax", types.JudgmentThresholds{Block: 0.5, Review: 0.3, Redact: 0.1}, false)
	v, err = lax.Evaluate(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.ActionBlock, v.Action, "0.6 高于该后端的 block 阈值")
}

func TestEvaluator_FallbackVerdict(t *testing.T) {
	ctx := context.Background()

	failClosed, err := NewEvaluator([]Judge{failingStub("bad", errBoom)}, EvaluatorOptions{FailClosed: true})
	require.NoError(t, err)
	_, err = failClosed.Evaluate(ctx, sampleDesc())
	require.Error(t, err, "全部后端失败必须冒泡成 error，由调用方决定姿态")

	v := failClosed.FallbackVerdict(err)
	assert.Equal(t, types.ActionReview, v.Action, "fail-closed ⇒ 交人")

	failOpen, err := NewEvaluator([]Judge{failingStub("bad", errBoom)}, EvaluatorOptions{FailClosed: false})
	require.NoError(t, err)
	v = failOpen.FallbackVerdict(errBoom)
	assert.Equal(t, types.ActionAllow, v.Action)
	assert.Equal(t, "fallback", v.Engine, "即使放行也要在审计里留下「判断层没跑起来」的痕迹")
	assert.NotEmpty(t, v.Reasons)
}

func TestEvaluator_RejectsInvalidDescriptor(t *testing.T) {
	e, err := NewEvaluator([]Judge{benignStub("b")}, EvaluatorOptions{})
	require.NoError(t, err)
	_, err = e.Evaluate(context.Background(), types.ActionDescriptor{Kind: "file_write"})
	require.Error(t, err, "闭集外的 kind 必须在入口被拦下")
}

func TestEvaluator_BackendsAndCapabilities(t *testing.T) {
	e, err := NewEvaluator([]Judge{
		&stubJudge{name: "m", cap: types.Capabilities{GivesConfidence: true}},
		NewRules(Whitelist{}),
	}, EvaluatorOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"m", "rules"}, e.Backends())
	cap := e.Capabilities()
	assert.False(t, cap.Deterministic, "链里有非确定性后端 ⇒ 整体非确定性")
	assert.True(t, cap.GivesConfidence)
}
