package pipeline

import (
	"time"

	"gateway/pkg/types"
)

// 调试事件类型（契约 §10.2）。pipeline 通过 EventPublisher 广播。
const (
	EvRequestReceived  = "request.received"
	EvDetectionDone    = "detection.done"
	EvReplacementDone  = "replacement.done"
	EvReplaced         = "replaced" // proxy 在脱敏后立即发布的"含完整 detected+replaced"事件
	EvUpstreamResponse = "upstream.response"
	EvRestoreDone      = "restore.done"
	EvRuleChanged      = "rule.changed"
)

// MappingEntry 脱敏映射（UI设计 §3.2 / 契约 §10.3）—— 占位符 → 原始值（面板 JSON 形态）。
//
// 由 proxy 在 publish 时构造，避免把 types.MappingEntry（带 Original []byte）的 JSON shape
// 直接暴露给前端；面板只关心 placeholder/type/value 三字段。
type MappingEntry struct {
	Placeholder string `json:"placeholder"`
	Type        string `json:"type"`
	Value       string `json:"value"` // 前端默认打码
}

// DetectionPayload detection.done 载荷。
type DetectionPayload struct {
	RequestID string         `json:"request_id"`
	Cached    bool           `json:"cached"`
	Entities  []types.Entity `json:"entities"`
}

// ReplacementPayload replacement.done 载荷。
type ReplacementPayload struct {
	RequestID string `json:"request_id"`
	Replaced  string `json:"replaced"`
	Count     int    `json:"count"`
}

// TrafficEvent 一条流量的完整 5 段对比（request.received / upstream.response / restore.done 复用）。
//
// 5 段对应 UI设计 §2：原始请求 / 检测 / 脱敏后请求 / 上游响应 / 还原后响应。
// pipeline 通过 EventPublisher 广播给 debug.Hub 存储与 WS 推送。
type TrafficEvent struct {
	RequestID    string               `json:"request_id"`
	Endpoint     string               `json:"endpoint"`
	Method       string               `json:"method,omitempty"`
	Stream       bool                 `json:"stream,omitempty"`
	RawRequest   string               `json:"raw_request,omitempty"`
	Detected     []types.Entity       `json:"detected,omitempty"`
	Replaced     string               `json:"replaced,omitempty"`
	UpstreamResp string               `json:"upstream_response,omitempty"`
	Restored     string               `json:"restored,omitempty"`
	Mapping      []MappingEntry       `json:"mapping,omitempty"`
	Strategy     string               `json:"strategy"`
	Outcome      string               `json:"outcome"`
	Error        string               `json:"error,omitempty"`
	DurationMs   int64                `json:"duration_ms,omitempty"`
	StartedAt    time.Time            `json:"started_at,omitempty"`
	FinishedAt   time.Time            `json:"finished_at,omitempty"`
	Timestamp    time.Time            `json:"timestamp"`
}
