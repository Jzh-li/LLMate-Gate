package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllCategories_ClosedSetUniqueAndKnown(t *testing.T) {
	seen := map[ActionCategory]bool{}
	for _, c := range AllCategories() {
		require.Falsef(t, seen[c], "duplicate category %q", c)
		seen[c] = true
		assert.Truef(t, IsKnownCategory(c), "category %q must be known", c)
	}
	assert.True(t, IsKnownCategory(CatUnknown), "unknown 必须是一等公民")
	assert.False(t, IsKnownCategory("not_a_category"))
}

func TestAllActions_ClosedSetUniqueAndKnown(t *testing.T) {
	seen := map[Action]bool{}
	for _, a := range AllActions() {
		require.Falsef(t, seen[a], "duplicate action %q", a)
		seen[a] = true
		assert.Truef(t, IsKnownAction(a), "action %q must be known", a)
	}
	assert.False(t, IsKnownAction("deny"))
}

func TestAllActionKinds(t *testing.T) {
	for _, k := range AllActionKinds() {
		assert.Truef(t, IsKnownActionKind(k), "kind %q must be known", k)
	}
	assert.False(t, IsKnownActionKind("file_write"))
}

func TestSchemaMode_Constrained(t *testing.T) {
	// prompt_only 是唯一的「不受约束」模式：它决定解析失败必须走 review。
	assert.False(t, SchemaPromptOnly.Constrained())
	for _, m := range []SchemaMode{SchemaJSONSchema, SchemaGBNF, SchemaFormat} {
		assert.Truef(t, m.Constrained(), "%q 应视为受约束", m)
	}
	// 空值按未声明处理 → 不受约束（最保守假设）。
	assert.False(t, SchemaMode("").Constrained())
	assert.False(t, IsKnownSchemaMode(""))
	assert.True(t, IsKnownSchemaMode(SchemaGBNF))
}

func TestCapabilities_SupportsCategory(t *testing.T) {
	// 空声明 = 支持全部（「我不限制」），不是「什么都不支持」。
	all := Capabilities{}
	assert.True(t, all.SupportsCategory(CatArchive))

	limited := Capabilities{Categories: []ActionCategory{CatArchive, CatBulkRead}}
	assert.True(t, limited.SupportsCategory(CatArchive))
	assert.False(t, limited.SupportsCategory(CatExfil))
}

func TestCapabilities_HasSchemaMode(t *testing.T) {
	// 空声明 = 仅 prompt_only：不假定用户的服务受约束。
	none := Capabilities{}
	assert.True(t, none.HasSchemaMode(SchemaPromptOnly))
	assert.False(t, none.HasSchemaMode(SchemaJSONSchema))

	some := Capabilities{SchemaModes: []SchemaMode{SchemaFormat, SchemaGBNF}}
	assert.True(t, some.HasSchemaMode(SchemaGBNF))
	assert.False(t, some.HasSchemaMode(SchemaJSONSchema))
}

func TestCapabilities_BestSchemaMode(t *testing.T) {
	// 多候选取约束最强者；都不支持时落到 prompt_only（而不是假定受约束）。
	cap := Capabilities{SchemaModes: []SchemaMode{SchemaFormat, SchemaGBNF}}
	assert.Equal(t, SchemaGBNF, cap.BestSchemaMode(SchemaPromptOnly, SchemaFormat, SchemaGBNF, SchemaJSONSchema))

	assert.Equal(t, SchemaPromptOnly, Capabilities{}.BestSchemaMode(SchemaJSONSchema, SchemaFormat))
	assert.Equal(t, SchemaJSONSchema, Capabilities{SchemaModes: []SchemaMode{SchemaJSONSchema}}.BestSchemaMode(SchemaJSONSchema))
}

func TestThresholds_Validate(t *testing.T) {
	require.NoError(t, DefaultThresholds().Validate())
	require.NoError(t, JudgmentThresholds{Block: 1, Review: 0.5, Redact: 0}.Validate())

	// 必须严格递减：相等意味着后一档永远不可达。
	assert.Error(t, JudgmentThresholds{Block: 0.5, Review: 0.5, Redact: 0.1}.Validate())
	assert.Error(t, JudgmentThresholds{Block: 0.4, Review: 0.5, Redact: 0.1}.Validate())
	// 越界。
	assert.Error(t, JudgmentThresholds{Block: 1.5, Review: 0.5, Redact: 0.1}.Validate())
	assert.Error(t, JudgmentThresholds{Block: 0.9, Review: 0.5, Redact: -0.1}.Validate())
}

func TestEvidence_Validate(t *testing.T) {
	ok := Evidence{Category: CatArchive, Severity: 0.9, Confidence: 0.8, Engine: "rules"}
	require.NoError(t, ok.Validate())

	// 闭集外的类别必须被拒：这是「模型吐 schema 外的值」的最后一道路障。
	assert.Error(t, Evidence{Category: "malware", Severity: 0.5}.Validate())
	assert.Error(t, Evidence{Category: CatBenign, Severity: 1.2}.Validate())
	assert.Error(t, Evidence{Category: CatBenign, Severity: -0.1}.Validate())
	assert.Error(t, Evidence{Category: CatBenign, Confidence: 1.1}.Validate())
	// 零值是合法输入（severity=0 confidence=0 = 完全无害但说不清）。
	assert.NoError(t, Evidence{Category: CatBenign}.Validate())
}

func TestActionDescriptor_ValidateAndInputBytes(t *testing.T) {
	d := ActionDescriptor{Kind: KindToolCall, Phase: PhaseProposal, Tool: "Bash", Target: "tar",
		Argv: []string{"tar", "-czf", "repo.tar.gz", "."}}
	require.NoError(t, d.Validate())
	assert.Equal(t, len("tar")+len("Bash")+len("tar")+1+len("-czf")+1+len("repo.tar.gz")+1+len(".")+1, d.InputBytes())

	// 来源形态与观测位都在闭集内校验。
	assert.Error(t, ActionDescriptor{Kind: "file_write"}.Validate())
	assert.Error(t, ActionDescriptor{Kind: KindToolCall, Phase: "later"}.Validate())
	// 空 Phase 合法（采集侧未必知道时序）。
	assert.NoError(t, ActionDescriptor{Kind: KindToolCall}.Validate())
}

func TestVerdict_Validate(t *testing.T) {
	v := Verdict{Action: ActionReview, Evidence: Evidence{Category: CatArchive, Severity: 0.7}}
	require.NoError(t, v.Validate())
	assert.Error(t, Verdict{Action: "deny", Evidence: Evidence{Category: CatArchive}}.Validate())
	// 内嵌 Evidence 的校验同样生效。
	assert.Error(t, Verdict{Action: ActionAllow, Evidence: Evidence{Category: "nope"}}.Validate())
}
