// Package policy 实现 per-type fate 策略引擎（技术方案 §5 per-type fate + 契约 §6.1）。
//
// 「命运（fate）」决定一个被检出的实体在脱敏后的可还原性：
//   - reversible：占位符 / 仿真替换，响应中可还原（默认，人名为代表）
//   - mask：部分遮盖，如手机号 138****8000（银行卡在占位符模式下默认）
//   - redact：不可逆移除，响应中也抹除（密钥 / 密码 / token）
//
// 配置 replacement.per_type_fate 可逐类型覆盖默认命运；未配置的类型回落到内置默认。
package policy

import (
	"fmt"

	"gateway/pkg/types"
)

// Policy 决定每个实体类型的最终命运。
type Policy struct {
	overrides map[string]types.Fate
}

// New 从 YAML 的 per_type_fate（字符串 → Fate）构造策略；非法 fate 字符串直接报错。
func New(perTypeFate map[string]string) (*Policy, error) {
	ov := make(map[string]types.Fate, len(perTypeFate))
	for t, s := range perTypeFate {
		f, err := parseFate(s)
		if err != nil {
			return nil, fmt.Errorf("policy: invalid fate for %s: %w", t, err)
		}
		ov[t] = f
	}
	return &Policy{overrides: ov}, nil
}

// WithIrreversible 把一批「强制不可逆」类型并入覆盖表（未显式指定的类型 → redact）。
// 用于兼容 replacement.irreversible 历史字段。
func (p *Policy) WithIrreversible(force []string) *Policy {
	for _, t := range force {
		if _, ok := p.overrides[t]; !ok {
			p.overrides[t] = types.FateRedact
		}
	}
	return p
}

// FateFor 返回实体类型的最终命运（strategy: placeholder | simulate）。
//
// 决议优先级：逐类型覆盖 > 内置不可逆类型 > 银行卡占位符模式例外 > 默认可逆。
func (p *Policy) FateFor(entityType, strategy string) types.Fate {
	if f, ok := p.overrides[entityType]; ok {
		return f
	}
	if types.IsIrreversible(entityType) {
		return types.FateRedact
	}
	// 银行卡在占位符模式下遮盖（保留后 4 位，可部分展示）；仿真模式下走可逆仿真。
	if entityType == types.EntityBankCard && strategy != "simulate" {
		return types.FateMask
	}
	return types.FateReversible
}

// HasOverride 报告该类型是否被显式配置（用于调试日志）。
func (p *Policy) HasOverride(entityType string) bool {
	_, ok := p.overrides[entityType]
	return ok
}

func parseFate(s string) (types.Fate, error) {
	switch s {
	case "reversible", "":
		return types.FateReversible, nil
	case "mask":
		return types.FateMask, nil
	case "redact":
		return types.FateRedact, nil
	default:
		return types.FateReversible, fmt.Errorf("unknown fate %q (want reversible|mask|redact)", s)
	}
}

// String 便于日志/调试。
func (p *Policy) String() string {
	return fmt.Sprintf("policy{overrides=%d}", len(p.overrides))
}
