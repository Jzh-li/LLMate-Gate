package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/internal/config"
	"gateway/pkg/types"
)

// 本文件只测一件事：buildJudgment 的「配置 → Spec」投影。
//
// 为什么值得单独测：这条投影是 config 层与 judge 层之间**唯一**的接缝，而它刻意
// 被放在 cmd 层（好让 judge 包不依赖 config）。代价是它既不在 config 的测试范围内，
// 也不在 judge 的测试范围内——两侧都测不到，只能在这里测。
//
// 测试策略：全部通过 Evaluator 的公开面观察（Evaluate / Capabilities / Backends /
// FailClosed），不读内部字段。理由是这些才是网关实际依赖的行为；投影错了、但内部
// 字段对得上，依然是坏投影。

// tarNarrow 触发 rules 的「打包归档（窄目标）」→ severity = 0.60。
//
// 选它当探针的理由：0.60 正好卡在缺省阈值表的 review 档（0.60 ≥ 0.55），而在
// 抬高后的阈值表下落到 allow。于是「后端最终用了哪张阈值表」不必读内部状态，
// 看最终 Action 就能判定。
const tarNarrow = "tar -czf backup.tar.gz /tmp/data"

func archiveDesc() types.ActionDescriptor {
	return types.ActionDescriptor{
		Kind: types.KindToolCall, Phase: types.PhaseExecuted,
		Tool: "Bash", Command: tarNarrow, Target: "tar",
	}
}

// raised 三道闸全部抬高，使 0.60 落到 allow。
func raised() *types.JudgmentThresholds {
	return &types.JudgmentThresholds{Block: 0.99, Review: 0.95, Redact: 0.90}
}

// rulesBackend 一个最小可用的规则后端配置。
func rulesBackend(name string) config.JudgmentBackendConfig {
	return config.JudgmentBackendConfig{Name: name, Kind: "rules"}
}

// evaluate 装配后判一条描述，返回 Action。
func evaluate(t *testing.T, jc config.JudgmentConfig, d types.ActionDescriptor) types.Verdict {
	t.Helper()
	e, err := buildJudgment(jc)
	require.NoError(t, err)
	v, err := e.Evaluate(context.Background(), d)
	require.NoError(t, err)
	return v
}

func TestBuildJudgment_RejectsEmptyAndUnknown(t *testing.T) {
	// 空后端列表：配置层的 Validate 会先拦掉（enabled=true + backends 为空），
	// 但 buildJudgment 自己也不能接受——它是被独立调用的，不该依赖调用方先校验过。
	_, err := buildJudgment(config.JudgmentConfig{})
	require.Error(t, err, "没有任何后端时不该构造出一个「永远答不出来」的判断层")

	// 未知 kind：必须在装配期炸掉。否则用户会得到一个静默空转的判断层。
	_, err = buildJudgment(config.JudgmentConfig{
		Backends: []config.JudgmentBackendConfig{{Name: "x", Kind: "telepathy"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "telepathy")
}

func TestBuildJudgment_ThresholdDefaults(t *testing.T) {
	// 不配任何阈值 → 内置缺省（0.60 → review）。
	v := evaluate(t, config.JudgmentConfig{Backends: []config.JudgmentBackendConfig{rulesBackend("rules")}}, archiveDesc())
	assert.Equal(t, types.ActionReview, v.Action, "缺省阈值下 0.60 落 review 档")
}

func TestBuildJudgment_GlobalThresholdsReachTheBackend(t *testing.T) {
	// 只配全局阈值 → 后端必须用上它（0.60 < 0.90 → allow）。
	v := evaluate(t, config.JudgmentConfig{
		Backends:   []config.JudgmentBackendConfig{rulesBackend("rules")},
		Thresholds: raised(),
	}, archiveDesc())
	assert.Equal(t, types.ActionAllow, v.Action,
		"judgment.thresholds 是全局缺省，必须真的作用到后端上；不生效等于配置空转")
}

// TestBuildJudgment_ThresholdsAreKeyedByBackendName 是本文件最重要的一条。
//
// 阈值表是按**后端名**登记的，而判决时用的键是 Evidence.Engine——即后端自己
// 盖在证据上的名字。这两个名字必须恒等。一旦不等，配置里的阈值就会被静默丢弃、
// 悄悄退回内置缺省，而用户看到的是「我调了阈值但没有任何变化」。
//
// 名字取 "policy" 而不是 "rules" 是有意的：它模拟用户给兜底后端起个语义化名字
// 的常见写法（"policy" / "fallback" / "local-rules" 都可能）。
func TestBuildJudgment_ThresholdsAreKeyedByBackendName(t *testing.T) {
	v := evaluate(t, config.JudgmentConfig{
		Backends:   []config.JudgmentBackendConfig{rulesBackend("policy")},
		Thresholds: raised(),
	}, archiveDesc())
	assert.Equal(t, types.ActionAllow, v.Action,
		"后端名与阈值表的键对不上时，配置的阈值被静默忽略——用户调了阈值却毫无效果")
}

func TestBuildJudgment_BackendThresholdsOverrideGlobal(t *testing.T) {
	// 后端自带阈值 > 全局阈值：全局抬高、后端缺省 → 回到 review（缺省表）。
	v := evaluate(t, config.JudgmentConfig{
		Backends: []config.JudgmentBackendConfig{
			{Name: "rules", Kind: "rules", Thresholds: ptr(types.DefaultThresholds())},
		},
		Thresholds: raised(),
	}, archiveDesc())
	assert.Equal(t, types.ActionReview, v.Action, "per-backend 阈值优先于全局阈值")

	// 反向：全局缺省、后端抬高 → allow。两个方向都测，避免「最后一个赋值者胜出」
	// 这种恰好通过一侧的实现。
	v = evaluate(t, config.JudgmentConfig{
		Backends: []config.JudgmentBackendConfig{
			{Name: "rules", Kind: "rules", Thresholds: raised()},
		},
	}, archiveDesc())
	assert.Equal(t, types.ActionAllow, v.Action, "per-backend 阈值在没有全局阈值时同样生效")
}

func TestBuildJudgment_FailClosedProjection(t *testing.T) {
	e, err := buildJudgment(config.JudgmentConfig{
		Backends:   []config.JudgmentBackendConfig{rulesBackend("rules")},
		FailClosed: true,
	})
	require.NoError(t, err)
	assert.True(t, e.FailClosed())
	assert.Equal(t, types.ActionReview, e.FallbackVerdict(nil).Action, "fail-closed 的兜底是 review")

	e, err = buildJudgment(config.JudgmentConfig{
		Backends:   []config.JudgmentBackendConfig{rulesBackend("rules")},
		FailClosed: false,
	})
	require.NoError(t, err)
	assert.False(t, e.FailClosed())
	assert.Equal(t, types.ActionAllow, e.FallbackVerdict(nil).Action)
	// 即便 fail_closed=false 也不许「无声放行」：兜底裁决必须带来源标签。
	assert.Equal(t, "fallback", e.FallbackVerdict(nil).Engine)
}

func TestBuildJudgment_WhitelistProjection(t *testing.T) {
	// Tools 白名单：命中即放行，且不进后端（Engine=whitelist 是证据）。
	jc := config.JudgmentConfig{
		Backends:   []config.JudgmentBackendConfig{rulesBackend("rules")},
		Thresholds: raised(),
		Whitelist:  config.JudgmentWhitelistConfig{Tools: []string{"Bash"}},
	}
	v := evaluate(t, jc, archiveDesc())
	assert.Equal(t, types.ActionAllow, v.Action)
	assert.Equal(t, "whitelist", v.Engine)

	// Hosts 白名单：白名单是在一切判断之前生效的短路，与后端判出什么无关。
	jc = config.JudgmentConfig{
		Backends:  []config.JudgmentBackendConfig{rulesBackend("rules")},
		Whitelist: config.JudgmentWhitelistConfig{Hosts: []string{"api.example.com"}},
	}
	d := archiveDesc()
	d.Target = "api.example.com"
	v = evaluate(t, jc, d)
	assert.Equal(t, types.ActionAllow, v.Action)
	assert.Equal(t, "whitelist", v.Engine)
}

func TestBuildJudgment_BackendOrderIsDegradationOrder(t *testing.T) {
	// 链的顺序 = 配置顺序。它不是优先级顺序，是**降级**顺序：
	// 能力强的（模型）在前，确定性兜底（rules）在最后。
	e, err := buildJudgment(config.JudgmentConfig{
		Backends: []config.JudgmentBackendConfig{
			{Name: "local-model", Kind: "openai", BaseURL: "http://127.0.0.1:11434", Model: "qwen2.5:7b", Timeout: time.Second},
			rulesBackend("rules"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"local-model", "rules"}, e.Backends())
}

func TestBuildJudgment_SchemaModeProjection(t *testing.T) {
	// schema_mode 经过 config → Spec → 后端三层，最后体现在后端的能力声明里。
	// 这条投影错了，用户就以为自己在用受约束解码，其实退回了 prompt_only。
	for _, tc := range []struct {
		configured string
		want       types.SchemaMode
	}{
		{"json_schema", types.SchemaJSONSchema},
		{"gbnf", types.SchemaGBNF},
		{"format", types.SchemaFormat},
		{"prompt_only", types.SchemaPromptOnly},
		{"", types.SchemaPromptOnly}, // 空值 = 最保守假设，不是最宽松
	} {
		t.Run("configured="+tc.configured, func(t *testing.T) {
			e, err := buildJudgment(config.JudgmentConfig{
				Backends: []config.JudgmentBackendConfig{{
					Name: "m", Kind: "openai", BaseURL: "http://127.0.0.1:11434",
					Model: "qwen2.5:7b", SchemaMode: tc.configured, Timeout: time.Second,
				}},
			})
			require.NoError(t, err)
			assert.Equal(t, []types.SchemaMode{tc.want}, e.Capabilities().SchemaModes)
		})
	}
}

func TestBuildJudgment_MaxInputBytesProjection(t *testing.T) {
	e, err := buildJudgment(config.JudgmentConfig{
		Backends: []config.JudgmentBackendConfig{{
			Name: "m", Kind: "openai", BaseURL: "http://127.0.0.1:11434",
			Model: "qwen2.5:7b", MaxInputBytes: 4096, Timeout: time.Second,
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, 4096, e.Capabilities().MaxInputBytes)
}

// ptr 返回 v 的地址（阈值表在配置里是可选字段）。
func ptr(v types.JudgmentThresholds) *types.JudgmentThresholds { return &v }
