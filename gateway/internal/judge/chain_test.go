package judge

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

// 降级链的语义核心：`unknown` 是「没答」，不是「命中」。
// 若把 unknown 当命中，一个恒返回 unknown 的坏模型会堵死整条链，
// 后面的确定性规则永远轮不到——这正是 D3 要防的事。
func TestChain_UnknownIsNotAHit(t *testing.T) {
	ctx := context.Background()
	weak := unknownStub("weak")
	rules := catStub("rules", types.CatArchive, 0.9, 0)

	c := NewChain(weak, rules)
	ev, degraded, err := c.judgeWithMeta(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.CatArchive, ev.Category)
	assert.Equal(t, "rules", ev.Engine)
	assert.True(t, degraded, "首个后端没给出结论 ⇒ 结论来自降级")
	assert.Equal(t, 1, weak.calls)
}

func TestChain_FirstHitWins(t *testing.T) {
	ctx := context.Background()
	m := catStub("model", types.CatArchive, 0.8, 0.9)
	rules := catStub("rules", types.CatCredential, 0.9, 0)

	c := NewChain(m, rules)
	ev, degraded, err := c.judgeWithMeta(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, "model", ev.Engine, "首个给出结论的后端胜出")
	assert.False(t, degraded)
	assert.Zero(t, rules.calls, "命中后不应继续询问后续后端")
}

func TestChain_ErrorFallsThrough(t *testing.T) {
	ctx := context.Background()
	bad := failingStub("bad", errBoom)
	rules := catStub("rules", types.CatArchive, 0.9, 0)

	ev, degraded, err := NewChain(bad, rules).judgeWithMeta(ctx, sampleDesc())
	require.NoError(t, err, "单个后端失败不应让整链失败")
	assert.Equal(t, "rules", ev.Engine)
	assert.True(t, degraded)
}

func TestChain_AllUnknownReturnsLast(t *testing.T) {
	ctx := context.Background()
	ev, degraded, err := NewChain(unknownStub("a"), unknownStub("b")).judgeWithMeta(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.CatUnknown, ev.Category)
	assert.True(t, degraded)
	// 全部 unknown 不是 error：它是一条正常结论（判不了），由 Mapper 转 review。
	v := NewMapper("m", types.DefaultThresholds(), false).Map(ev)
	assert.Equal(t, types.ActionReview, v.Action)
}

func TestChain_AllFailedReturnsError(t *testing.T) {
	ctx := context.Background()
	_, _, err := NewChain(failingStub("a", errBoom), failingStub("b", errBoom)).judgeWithMeta(ctx, sampleDesc())
	require.Error(t, err, "所有后端都失败才是 error")

	// 无后端也是 error（而不是静默返回空证据）。
	_, _, err = NewChain().judgeWithMeta(ctx, sampleDesc())
	require.Error(t, err)
}

func TestChain_TimeoutFallsThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	slow := &stubJudge{name: "slow", delay: 500 * time.Millisecond, ev: types.Evidence{Category: types.CatArchive}}
	rules := catStub("rules", types.CatBenign, 0, 0)

	ev, degraded, err := NewChain(slow, rules).judgeWithMeta(ctx, sampleDesc())
	require.NoError(t, err, "超时的时间预算由调用方的 ctx 决定；慢后端被跳过")
	assert.Equal(t, "rules", ev.Engine)
	assert.True(t, degraded)
}

// Health：链的可用性 = 至少一个后端可用。这条保证的正是「rules 永不缺席」。
func TestChain_HealthNeedsOnlyOne(t *testing.T) {
	ctx := context.Background()
	alive := benignStub("alive")
	require.NoError(t, NewChain(failingStub("dead", errBoom), alive).Health(ctx),
		"有一个后端活着，链就还能作答")
	require.Error(t, NewChain(failingStub("dead", errBoom)).Health(ctx))
}

func TestChain_ImplementJudgeInterface(t *testing.T) {
	// Chain 本身也是 Judge：可以嵌套、可以当作单后端塞进 Evaluator。
	var _ Judge = NewChain(NewRules(Whitelist{}))
	ev, err := NewChain(NewRules(Whitelist{})).Judge(context.Background(), sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.CatArchive, ev.Category)
	assert.Equal(t, "rules", ev.Engine)

	c := NewChain(benignStub("x"), NewRules(Whitelist{}))
	assert.Equal(t, "chain[x,rules]", c.Name())
	assert.Equal(t, []string{"x", "rules"}, c.Names())
}

func TestChain_CapabilitiesUnion(t *testing.T) {
	c := NewChain(
		&stubJudge{name: "a", cap: types.Capabilities{Categories: []types.ActionCategory{types.CatArchive}, GivesConfidence: true}},
		&stubJudge{name: "b", cap: types.Capabilities{Categories: []types.ActionCategory{types.CatCredential}, Deterministic: true}},
	)
	cap := c.Capabilities()
	assert.ElementsMatch(t, []types.ActionCategory{types.CatArchive, types.CatCredential}, cap.Categories)
	assert.True(t, cap.GivesConfidence)
	assert.False(t, cap.Deterministic, "有后端未声明确定性 ⇒ 并集不是确定性")

	// 任一后端声明「支持全部」（Categories 为空）⇒ 整体视为支持全部。
	all := NewChain(&stubJudge{name: "r", cap: types.Capabilities{Deterministic: true}}, benignStub("b"))
	assert.Empty(t, all.Capabilities().Categories)
}

// S2 的关键性质：从原始命令行到最终裁决，整条路可复现。
func TestChain_RulesOnly_EndToEnd(t *testing.T) {
	ctx := context.Background()
	e, err := NewEvaluator([]Judge{NewRules(Whitelist{})}, EvaluatorOptions{FailClosed: true})
	require.NoError(t, err)

	v, err := e.Evaluate(ctx, sampleDesc())
	require.NoError(t, err)
	assert.Equal(t, types.ActionBlock, v.Action)
	assert.Equal(t, types.CatArchive, v.Category)
	assert.Equal(t, "rules", v.Engine)
	assert.False(t, v.Degraded)

	// 良性动作走 allow，且不产生噪声。
	v, err = e.Evaluate(ctx, types.ActionDescriptor{Kind: types.KindToolCall, Command: "ls -la"})
	require.NoError(t, err)
	assert.Equal(t, types.ActionAllow, v.Action)
}
