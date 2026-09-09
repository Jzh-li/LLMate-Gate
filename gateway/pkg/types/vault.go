package types

import "time"

// Fate 实体命运：决定该实体在脱敏后是否可还原（契约 §2.3）。
type Fate int

const (
	// FateReversible 占位符/仿真替换，可在响应中还原。
	FateReversible Fate = iota
	// FateMask 部分遮盖，如 138****8000。
	FateMask
	// FateRedact 不可逆移除，响应中也抹除。
	FateRedact
)

// String 返回 Fate 的可读名称，用于审计日志与配置序列化。
func (f Fate) String() string {
	switch f {
	case FateReversible:
		return "reversible"
	case FateMask:
		return "mask"
	case FateRedact:
		return "redact"
	default:
		return "unknown"
	}
}

// MarshalJSON 序列化为 snake_case 字符串，保证审计日志可读。
func (f Fate) MarshalJSON() ([]byte, error) {
	return []byte(`"` + f.String() + `"`), nil
}

// UnmarshalJSON 从字符串反序列化。
func (f *Fate) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case `"reversible"`:
		*f = FateReversible
	case `"mask"`:
		*f = FateMask
	case `"redact"`:
		*f = FateRedact
	default:
		*f = FateReversible
	}
	return nil
}

// MappingTable 一次请求的完整映射表（契约 §7.1）。
type MappingTable struct {
	RequestID      string         `json:"request_id"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Entries        []MappingEntry `json:"entries"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
}

// MappingEntry 单条占位符 ↔ 原值映射（契约 §7.1）。
//
// Original 为明文字段：仅在内存驻留，落盘时由 Seal 经 AES-256-GCM
// 加密为密文（不是明文），使用完毕必须调用 Zeroize 清零内存。
type MappingEntry struct {
	Placeholder string  `json:"placeholder"`
	Original    []byte  `json:"original"`
	EntityType  string  `json:"entity_type"`
	Score       float32 `json:"score"`
	Fate        Fate    `json:"fate"`
	Start       int     `json:"start_offset"`
	End         int     `json:"end_offset"`
	FakeValue   []byte  `json:"fake_value,omitempty"`
}

// Sentinel 返回该实体在上游侧可见的字符串：
// simulate 模式下是仿真值，placeholder 模式下是占位符。
// 响应还原时用它做 trie 匹配。
func (e *MappingEntry) Sentinel() string {
	if len(e.FakeValue) > 0 {
		return string(e.FakeValue)
	}
	return e.Placeholder
}

// Zeroize 将明文字段清零（契约 §7.4 内存安全）。
func (e *MappingEntry) Zeroize() {
	for i := range e.Original {
		e.Original[i] = 0
	}
	for i := range e.FakeValue {
		e.FakeValue[i] = 0
	}
}

// Zeroize 清空整张映射表的明文。
func (t *MappingTable) Zeroize() {
	for i := range t.Entries {
		t.Entries[i].Zeroize()
	}
}

// Expired 映射表是否已过期。
func (t *MappingTable) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && now.After(t.ExpiresAt)
}
