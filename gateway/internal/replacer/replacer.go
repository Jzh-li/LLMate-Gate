// Package replacer 实现占位符替换与流式还原（契约 §5）。
package replacer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/policy"
	"gateway/internal/simulator"
	"gateway/internal/vault"
	"gateway/pkg/types"
)

// Replacer 脱敏/还原抽象（契约 §5.1）。
type Replacer interface {
	Replace(ctx context.Context, req *ReplaceRequest) (*ReplaceResult, error)
	Restore(ctx context.Context, req *RestoreRequest) (string, error)
	// Strategy 当前替换策略（placeholder | simulate | bypass）。
	Strategy() string
	// SetStrategy 热加载替换策略（线程安全）。
	SetStrategy(string) error
	// SetDictionary 热加载仿真词典（线程安全）；nil 等效清空。
	SetDictionary(map[string]map[string]string)
	// Dictionary 返回当前仿真词典的深拷贝。
	Dictionary() map[string]map[string]string
	// NewSession 构造一次请求内的替换会话：跨多个字段 / 消息共享占位符计数与
	// (type,value) 复用，保证全局占位符唯一、跨段指代不崩（契约 §5.2 规则 1-3）。
	NewSession() *Session
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
	// Policy 逐类型命运策略（per-type fate）；nil 时使用内置默认。
	Policy *policy.Policy
}

// impl 默认实现。
type impl struct {
	// mu 保护 cfg 整块的热加载（含 Strategy 与 Simulate——后者内嵌 map 头，
	// 锁外按值拷贝整个结构体同样会与 SetStrategy 的写入竞争）。
	mu    sync.RWMutex
	cfg   Config
	sim   *simulator.Generator
	vault vault.Vault
}

// New 构造替换器。
func New(cfg Config, v vault.Vault) Replacer {
	g := simulator.New(cfg.SessionKey)
	g.SetSimulateConfig(cfg.Simulate)
	return &impl{cfg: cfg, sim: g, vault: v}
}

// normalizeStrategy 空策略等价 placeholder（契约 §5.2 默认值）。
func normalizeStrategy(s string) string {
	if s == "" {
		return "placeholder"
	}
	return s
}

// Strategy 返回当前替换策略。
//
// 必须在锁内读：SetStrategy 会并发改写 r.cfg.Strategy（面板 PUT），
// 而本方法是**每个请求**都会调的（proxy.strategy()），锁外读字符串头是真实竞态。
func (r *impl) Strategy() string {
	r.mu.RLock()
	s := r.cfg.Strategy
	r.mu.RUnlock()
	return normalizeStrategy(s)
}

// SetStrategy 热加载替换策略（线程安全）。仅允许 placeholder / simulate / bypass / 空（=placeholder）。
func (r *impl) SetStrategy(s string) error {
	switch s {
	case "", "placeholder", "simulate", "bypass":
	default:
		return fmt.Errorf("invalid strategy %q", s)
	}
	r.mu.Lock()
	r.cfg.Strategy = s
	r.mu.Unlock()
	return nil
}

// SetDictionary 热加载仿真词典；会话与生成器共享同一个 Generator，故一次设置全局生效。
func (r *impl) SetDictionary(d map[string]map[string]string) { r.sim.SetDictionary(d) }

// Dictionary 返回当前仿真词典的深拷贝。
func (r *impl) Dictionary() map[string]map[string]string { return r.sim.Dictionary() }

// Replace 对单段文本做脱敏（契约 §5.1/§5.2）。
//
// 单段调用等价于一次独立会话；多字段 / 多消息的请求应改用 NewSession() 以共享占位符计数，
// 避免同一类型不同值跨段产生重复占位符导致还原错乱。
func (r *impl) Replace(ctx context.Context, req *ReplaceRequest) (*ReplaceResult, error) {
	if req == nil {
		return nil, gatewayerrors.New(gatewayerrors.CodeInvalidRequest, "nil replace request")
	}
	sess := r.NewSession()
	if req.Strategy != "" {
		_ = sess.SetStrategy(req.Strategy)
	}
	clean, entries, err := sess.Replace(req.Text, req.Entities)
	if err != nil {
		return nil, err
	}
	return &ReplaceResult{Text: clean, Entries: entries}, nil
}

// NewSession 构造一次请求内的替换会话（共享占位符计数与 (type,value) 复用）。
//
// r.cfg 在锁内整体快照：它含 Simulate（内嵌 map 头）与 Strategy 两个会热加载的字段，
// 锁外拷贝整个结构体会与 SetStrategy 的写入竞争。
//
// 注意：不要在持锁期间调 r.Strategy() —— RWMutex 的 RLock 不可重入，
// 若有写者排在两次 RLock 之间会死锁。这里改成先在锁内取快照，再在锁外归一化策略。
func (r *impl) NewSession() *Session {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	return &Session{
		cfg:          cfg,
		sim:          r.sim,
		vault:        r.vault,
		strategy:     normalizeStrategy(cfg.Strategy),
		typeCounters: map[string]int{},
		valueIndex:   map[string]int{},
	}
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

// Session 一次请求内的替换会话：跨多个字段 / 消息共享占位符计数与 (type,value) 复用，
// 保证全局占位符唯一、跨段指代不崩（契约 §5.2 规则 1-3）。同时修复「不同段同类型不同值
// 各自从 _1 编号导致占位符碰撞、还原错乱」的隐患。
type Session struct {
	mu           sync.Mutex
	cfg          Config
	sim          *simulator.Generator
	vault        vault.Vault
	strategy     string
	typeCounters map[string]int
	valueIndex   map[string]int
	entries      []types.MappingEntry
}

// SetStrategy 热加载替换策略（线程安全）。
func (s *Session) SetStrategy(str string) error {
	switch str {
	case "", "placeholder", "simulate", "bypass":
	default:
		return fmt.Errorf("invalid strategy %q", str)
	}
	s.mu.Lock()
	s.strategy = str
	s.mu.Unlock()
	return nil
}

// Replace 对单段文本做脱敏，并累积到本次会话的占位符序列（契约 §5.1/§5.2）。
func (s *Session) Replace(text string, ents []types.Entity) (string, []types.MappingEntry, error) {
	if text == "" {
		return "", nil, nil
	}
	strategy := s.strategy
	ents = sanitizeEntities(text, ents)
	if len(ents) == 0 {
		return text, nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var sb strings.Builder
	prev := 0
	for _, e := range ents {
		if e.Start < prev {
			continue // 与已处理区间重叠，跳过
		}
		sb.WriteString(text[prev:e.Start])

		fate := s.fateFor(e.Type, strategy)
		key := e.Type + "\x00" + e.Value
		if idx, seen := s.valueIndex[key]; seen {
			// 同 (type, value) → 复用同一上游可见串，保证 LLM 跨句指代不崩
			sb.WriteString(s.entries[idx].Sentinel())
			prev = e.End
			continue
		}

		s.typeCounters[e.Type]++
		placeholder := fmt.Sprintf("<<%s_%d>>", e.Type, s.typeCounters[e.Type])
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
				if fake, err := s.sim.Fake(e.Type, []byte(e.Value), nil); err == nil && len(fake) > 0 {
					entry.FakeValue = fake
					emitted = string(fake)
				}
			}
		}
		start := sb.Len()
		sb.WriteString(emitted)
		entry.Start = start
		entry.End = sb.Len()
		s.valueIndex[key] = len(s.entries)
		s.entries = append(s.entries, entry)
		prev = e.End
	}
	sb.WriteString(text[prev:])

	return sb.String(), s.entries, nil
}

// Entries 返回本次会话累积的全部映射条目（供调用方落盘 vault / 外部集成点使用）。
func (s *Session) Entries() []types.MappingEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.MappingEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// fateFor 决定实体命运（技术方案 §5 per-type fate + 契约 §6.1）。
func (s *Session) fateFor(entityType, strategy string) types.Fate {
	if s.cfg.Policy != nil {
		return s.cfg.Policy.FateFor(entityType, strategy)
	}
	// 内置默认：历史 irreversible 列表 → redact；其余回落 Type 默认。
	for _, t := range s.cfg.Irreversible {
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
