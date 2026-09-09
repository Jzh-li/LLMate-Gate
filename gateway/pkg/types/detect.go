// Package types 定义跨包共享的数据类型。
//
// 本包是《接口与数据契约规范.md》§2.3 的权威实现，任何字段变更必须先更新规范。
package types

// DetectRequest 检测引擎入参（契约 §2.1/§2.3）。
type DetectRequest struct {
	Text           string   `json:"text"`
	ConversationID string   `json:"conversation_id,omitempty"`
	Entities       []string `json:"entities,omitempty"`
}

// DetectResponse 检测引擎出参（契约 §2.1/§2.3）。
type DetectResponse struct {
	Entities  []Entity `json:"entities"`
	LatencyMs int64    `json:"latency_ms"`
}

// Entity 一个被检出的 PII 实体。
//
// Start/End 为 UTF-8 字节偏移，半开区间 [Start, End)。
type Entity struct {
	Type  string  `json:"type"`
	Value string  `json:"value"`
	Start int     `json:"start"`
	End   int     `json:"end"`
	Score float64 `json:"score"`
}

// 实体类型常量（契约 §6.1 权威表）。
const (
	EntityPersonName = "zh_person_name"
	EntityPhone      = "zh_phone"
	EntityIDCard     = "zh_id_card"
	EntityBankCard   = "zh_bank_card"
	EntityAddress    = "zh_address"
	EntityEmail      = "email"
	EntityIPAddress  = "ip_address"
	EntityDate       = "date"
	EntityAPIKey     = "api_key"
	EntityPassword   = "password"
	EntityToken      = "token"
)

// IrreversibleTypes 走不可逆 redact 的实体类型（契约 §6.1）。
func IrreversibleTypes() []string {
	return []string{EntityAPIKey, EntityPassword, EntityToken}
}

// IsIrreversible 该实体类型是否为不可逆（redact）。
func IsIrreversible(entityType string) bool {
	for _, t := range IrreversibleTypes() {
		if t == entityType {
			return true
		}
	}
	return false
}
