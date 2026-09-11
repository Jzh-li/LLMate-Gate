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
//
// 命名约定（2026-09-11 拍板）：
//   - zh_*：仅中文语料有效（如身份证/手机号/地址）
//   - 不带前缀：国际通用（email/ip_address/date/api_key/...）
//   - 英文专属实体（iban_code / us_ssn / credit_card / url / plate）：
//     沿用 privaite / Microsoft Presidio 的国际惯例命名，不带 en_ 前缀。
//     这是国际化基线，不属于「支持更多语言」（见 DECISION.md §8.6 注脚）。
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
	// 英文/国际化基线（2026-09-11）：
	EntityPlate      = "plate"       // 中英车牌统一（中文见 rePlate，英文见 rePlateEN）
	EntityURL        = "url"         // URL（HTTP/HTTPS/FTP）
	EntityUSSSN      = "us_ssn"      // 美国社会安全号 AAA-GG-SSSS
	EntityCreditCard = "credit_card" // 国际信用卡（Luhn，跟 zh_bank_card 共用校验）
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
