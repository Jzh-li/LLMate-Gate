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
	EvUpstreamResponse = "upstream.response"
	EvRestoreDone      = "restore.done"
	EvRuleChanged      = "rule.changed"
)

// DetectionPayload detection.done 载荷。
type DetectionPayload struct {
	RequestID string          `json:"request_id"`
	Cached    bool            `json:"cached"`
	Entities  []types.Entity  `json:"entities"`
}

// ReplacementPayload replacement.done 载荷。
type ReplacementPayload struct {
	RequestID string `json:"request_id"`
	Replaced  string `json:"replaced"`
	Count     int    `json:"count"`
}

// TrafficEvent 一条流量的完整 5 段对比（request.received / upstream.response / restore.done 复用）。
type TrafficEvent struct {
	RequestID    string          `json:"request_id"`
	Endpoint     string          `json:"endpoint"`
	RawRequest   string          `json:"raw_request,omitempty"`
	Detected     []types.Entity  `json:"detected,omitempty"`
	Replaced     string          `json:"replaced,omitempty"`
	UpstreamResp string          `json:"upstream_response,omitempty"`
	Restored     string          `json:"restored,omitempty"`
	Strategy     string          `json:"strategy"`
	Outcome      string          `json:"outcome"`
	Error        string          `json:"error,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
}
