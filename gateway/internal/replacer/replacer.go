// Package replacer 实现占位符替换与流式还原（契约 §5）。
package replacer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"

	"gateway/internal/simulator"
	"gateway/internal/vault"
)

// Replacer 脱敏/还原抽象（契约 §5.1）。
type Replacer interface {
	Replace(ctx context.Context, req *ReplaceRequest) (*ReplaceResult, error)
	Restore(ctx context.Context, req *RestoreRequest) (string, error)
	// Strategy 当前替换策略（placeholder | simulate）。
	Strategy() string
	// SetStrategy 热加载替换策略（线程安全）。
	SetStrategy(string) error
}

// ReplaceRequest 脱敏入参（契约 §5.1）。
type ReplaceRequest struct {
	Text           string
	RequestID      string
	ConversationID string
	Entities       []types.Entity
	// Strategy 覆盖全局策略：placeholder | simulate，空表示沿用配置。
	Strategy string
}

// ReplaceResult 脱敏结果（契约 §5.1）。
type ReplaceResult struct {
	Text    string
	Entries []types.MappingEntry
}

// RestoreRequest 还原入参（契约 §5.1）。
type RestoreRequest struct {
	Text      string
	RequestID string
	Entries   []types.MappingEntry
}

// RedactedText 不可逆 redact 的可见文本（契约 §6.1）。
const RedactedText = "[REDACTED]"

// Config 替换器配置。
type Config struct {
	Strategy     string // placeholder | simulate
	Irreversible []string
	Simulate     simulator.SimulateZHConfig
	SessionKey   []byte
}

// impl 默认实现。
type impl struct {
	mu   sync.Mutex // 保护 cfg.Strategy 热加载
	cfg  Config
	sim  *simulator.Generator
	vault vault.Vault
}

// New 构造替换器。
func New(cfg Config, v vault.Vault) Replacer {
	g := simulator.New(cfg.SessionKey)
	g.Cfg = cfg.Simulate
	return &impl{cfg: cfg, sim: g, vault: v}
}

// Strategy 返回当前替换策略。
func (r *impl) Strategy() string {
	if r.cfg.Strategy == "" {
		return "placeholder"
	}
	return r.cfg.Strategy
}

// SetStrategy 热加载替换策略（线程安全）。仅允许 placeholder / simulate / 空（=placeholder）。
func (r *impl) SetStrategy(s string) error {
	switch s {
	case "", "placeholder", "simulate":
	default:
		return fmt.Errorf("invalid strategy %q", s)
	}
	r.mu.Lock()
	r.cfg.Strategy = s
	r.mu.Unlock()
	return nil
}

// Replace 对文本做脱敏（契约 §5.1/§5.2）。
//
// 协议保证：
//  1. 同 (type, value) → 同占位符（跨句指代不崩）
//  2. 不同类型即使值相同 → 不同占位符
//  3. index 按 type 独立计数
//  4. 记录占位符在**脱敏文本**中的 [Start, End)
func (r *impl) Replace(ctx context.Context, req *ReplaceRequest) (*ReplaceResult, error) {
	if req == nil {
		return nil, gatewayerrors.New(gatewayerrors.CodeInvalidRequest, "nil replace request")
	}
	text := req.Text
	if text == "" {
		return &ReplaceResult{Text: "", Entries: nil}, nil
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = r.cfg.Strategy
	}

	// 1. 排序 + 去重重叠（防御性：检测引擎已保证，但替换阶段再兜一次）
	ents := sanitizeEntities(text, req.Entities)
	if len(ents) == 0 {
		return &ReplaceResult{Text: text, Entries: nil}, nil
	}

	typeCounters := map[string]int{}
	valueIndex := map[string]int{} // (type \x00 value) → 在 entries 中的下标
	var entries []types.MappingEntry

	var sb strings.Builder
	prev := 0
	for _, e := range ents {
		if e.Start < prev {
			continue // 与已处理区间重叠，跳过
		}
		sb.WriteString(text[prev:e.Start])

		fate := r.fateFor(e.Type, strategy)
		key := e.Type + "\x00" + e.Value
		if idx, seen := valueIndex[key]; seen {
			// 同 (type, value) → 复用同一上游可见串，保证 LLM 跨句指代不崩
			sb.WriteString(entries[idx].Sentinel())
			prev = e.End
			continue
		}

		typeCounters[e.Type]++
		placeholder := fmt.Sprintf("<<%s_%d>>", e.Type, typeCounters[e.Type])
		entry := types.MappingEntry{
			Placeholder: placeholder,
			Original:    []byte(e.Value),
			EntityType:  e.Type,
			Score:       float32(e.Score),
			Fate:        fate,
		}
		var emitted string
		switch fate {
		case types.FateRedact:
			emitted = RedactedText
		case types.FateMask:
			emitted = maskValue(e.Value)
		case types.FateReversible:
			emitted = placeholder
			if strategy == "simulate" {
				if fake, err := r.sim.Fake(e.Type, []byte(e.Value), nil); err == nil && len(fake) > 0 {
					entry.FakeValue = fake
					emitted = string(fake)
				}
			}
		}
		start := sb.Len()
		sb.WriteString(emitted)
		entry.Start = start
		entry.End = sb.Len()
		valueIndex[key] = len(entries)
		entries = append(entries, entry)
		prev = e.End
	}
	sb.WriteString(text[prev:])

	return &ReplaceResult{Text: sb.String(), Entries: entries}, nil
}

// Restore 在响应文本中还原（契约 §5.1）。
// 优先使用传入的 Entries，否则按 RequestID 从 vault 加载。
func (r *impl) Restore(ctx context.Context, req *RestoreRequest) (string, error) {
	if req == nil {
		return "", gatewayerrors.New(gatewayerrors.CodeInvalidRequest, "nil restore request")
	}
	entries := req.Entries
	if len(entries) == 0 && req.RequestID != "" && r.vault != nil {
		t, err := r.vault.Get(req.RequestID)
		if err != nil {
			// 映射表过期/丢失 → 保持原样，不报错（fail-safe）
			return req.Text, nil
		}
		entries = t.Entries
	}
	if len(entries) == 0 {
		return req.Text, nil
	}
	sr := NewStreamRestorerFromEntries(entries)
	out, err := sr.Write([]byte(req.Text))
	if err != nil {
		return "", gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "stream restore", err)
	}
	rest, err := sr.Close()
	if err != nil {
		return "", gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "stream restore flush", err)
	}
	return string(out) + string(rest), nil
}

// fateFor 决定实体命运（技术方案 §5 per-type fate + 契约 §6.1）。
func (r *impl) fateFor(entityType, strategy string) types.Fate {
	for _, t := range r.cfg.Irreversible {
		if t == entityType {
			return types.FateRedact
		}
	}
	if types.IsIrreversible(entityType) {
		return types.FateRedact
	}
	// 银行卡：占位符模式下遮盖（保留后 4 位）；仿真模式下走可逆仿真。
	if entityType == types.EntityBankCard && strategy != "simulate" {
		return types.FateMask
	}
	return types.FateReversible
}

// maskValue 保留后 4 位，其余用 * 填充，长度保持不变。
func maskValue(v string) string {
	runes := []rune(v)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	return strings.Repeat("*", len(runes)-4) + string(runes[len(runes)-4:])
}

// sanitizeEntities 按 start 升序排序、剔除越界与重叠实体。
func sanitizeEntities(text string, in []types.Entity) []types.Entity {
	out := make([]types.Entity, 0, len(in))
	for _, e := range in {
		if e.Start < 0 || e.End > len(text) || e.Start >= e.End {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End > out[j].End
	})
	// 剔除重叠，保留更长（信息更全）的实体
	res := make([]types.Entity, 0, len(out))
	for _, e := range out {
		if len(res) > 0 && e.Start < res[len(res)-1].End {
			continue
		}
		res = append(res, e)
	}
	return res
}

// 编译期接口断言。
var _ Replacer = (*impl)(nil)
