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

// 映射表来源（MappingTable.Origin）。区分「谁建的这张表」是还原权限的判据：
//
//	vault.Get 按 request_id 取值，而 request_id 在数据面是客户端指定的头。
//	若不记录来源，任何能调控制面 /v1/privacy/restore 的调用方，只要猜到（或
//	从日志里看到）另一个请求的 X-Request-ID，就能把那次请求的原文还原出来。
//	来源标记把两个面在存储层就分开，还原时按面校验。
const (
	// OriginData 数据面：由 /v1/* 的 LLM 请求建立，仅供该请求自身的响应还原使用，
	// 不可通过控制面 API 还原。
	OriginData = "data"
	// OriginControl 控制面：由 /v1/privacy/redact 建立，是可被 /v1/privacy/restore 还原的表。
	OriginControl = "control"
)

// MappingTable 一次请求的完整映射表（契约 §7.1）。
type MappingTable struct {
	RequestID      string         `json:"request_id"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Entries        []MappingEntry `json:"entries"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      time.Time      `json:"expires_at"`
	// Origin 建立该表的调用面，取值见 OriginData / OriginControl。
	//
	// 零值（空串）按 OriginData 处理——即「不可经控制面还原」。缺省必须是收紧的那一侧：
	// 兼容旧数据时若把空值当成控制面，就等于给所有历史条目开了还原口子。
	Origin string `json:"origin,omitempty"`
}

// IsRestorableViaAPI 该表是否允许经控制面 /v1/privacy/restore 还原。
func (t *MappingTable) IsRestorableViaAPI() bool {
	return t != nil && t.Origin == OriginControl
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
