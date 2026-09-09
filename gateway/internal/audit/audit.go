// Package audit 结构化审计日志（契约 §9）。
//
// 规则：默认不含原文（log_pii=false）；JSON Lines 一行一条；支持合规导出。
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"
)

// EntitySummary 审计中记录的实体摘要（不含完整偏移）。
type EntitySummary struct {
	Type  string  `json:"type"`
	Score float64 `json:"score"`
	// 默认不填原文；仅当 log_pii=true 时填充
	Value string `json:"value,omitempty"`
}

// Event 审计事件（契约 §9.1）。
type Event struct {
	Timestamp         time.Time       `json:"timestamp"`
	SchemaVersion     string          `json:"schema_version"`
	RequestID         string          `json:"request_id"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	ClientID          string          `json:"client_id,omitempty"`
	Upstream          string          `json:"upstream"`
	Model             string          `json:"model,omitempty"`
	DetectedEntities  []EntitySummary `json:"detected_entities"`
	ReplacedCount     int             `json:"replaced_count"`
	Strategy          string          `json:"strategy"`
	Restored          bool            `json:"restored"`
	Streaming         bool            `json:"streaming"`
	LatencyMs         int64           `json:"latency_ms"`
	DetectorLatencyMs int64           `json:"detector_latency_ms"`
	Outcome           string          `json:"outcome"`
	ErrorCode         string          `json:"error_code,omitempty"`
	SampleText        string          `json:"sample_text,omitempty"`
}

// Logger 审计日志写入器（契约 §9.2：JSON Lines）。
type Logger struct {
	mu       sync.Mutex
	file     *os.File
	writer   *bufio.Writer
	logPII   bool
	disabled bool
}

// NewLogger 构造审计 logger。
// path 为空或 enabled=false 时仅丢弃（不报错），保证代理可独立运行。
func NewLogger(path string, enabled, logPII bool) (*Logger, error) {
	l := &Logger{logPII: logPII}
	if !enabled {
		l.disabled = true
		return l, nil
	}
	if path == "" {
		l.disabled = true
		return l, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "open audit log", err)
	}
	l.file = f
	l.writer = bufio.NewWriter(f)
	return l, nil
}

// Write 追加一条审计事件（线程安全）。
func (l *Logger) Write(e *Event) error {
	if l == nil || l.disabled {
		return nil
	}
	if e.SchemaVersion == "" {
		e.SchemaVersion = "1"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.writer.Write(append(b, '\n')); err != nil {
		return err
	}
	return l.writer.Flush()
}

// Close 刷新并关闭文件。
func (l *Logger) Close() error {
	if l == nil || l.disabled {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writer != nil {
		_ = l.writer.Flush()
	}
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// ToSummary 从检测实体构造摘要（受 logPII 控制是否带原文）。
func (l *Logger) ToSummary(entities []types.Entity) []EntitySummary {
	out := make([]EntitySummary, 0, len(entities))
	for _, e := range entities {
		s := EntitySummary{Type: e.Type, Score: e.Score}
		if l != nil && l.logPII {
			s.Value = e.Value
		}
		out = append(out, s)
	}
	return out
}

// Exporter 合规导出（契约 §9.2）。
type Exporter struct {
	logPII bool
}

// NewExporter 构造导出器。
func NewExporter(logPII bool) *Exporter { return &Exporter{logPII: logPII} }

// Export 将审计事件导出为指定合规格式的 JSON。
//
// 支持：pip（个人信息保护法字段集）、gdpr（GDPR 第 30 条）、dsl、csl、等保2.0。
// 未知格式回退为 pip 字段集。
func (x *Exporter) Export(format string, events []Event) ([]byte, error) {
	records := make([]map[string]interface{}, 0, len(events))
	for _, e := range events {
		records = append(records, x.project(format, e))
	}
	return json.MarshalIndent(map[string]interface{}{
		"format": format,
		"count":  len(records),
		"records": records,
	}, "", "  ")
}

func (x *Exporter) project(format string, e Event) map[string]interface{} {
	base := map[string]interface{}{
		"event_ts":        e.Timestamp.Format(time.RFC3339),
		"request_id":      e.RequestID,
		"conversation_id": e.ConversationID,
		"upstream":        e.Upstream,
		"model":           e.Model,
		"strategy":        e.Strategy,
		"replaced_count":  e.ReplacedCount,
		"restored":        e.Restored,
		"outcome":         e.Outcome,
		"error_code":      e.ErrorCode,
	}
	switch format {
	case "gdpr":
		// GDPR 第 30 条：处理活动记录
		base["subject_categories"] = entityTypes(e.DetectedEntities)
		base["lawful_basis"] = "consent"
		base["retention"] = "session"
	case "dsl", "csl", "等保2.0":
		// 网络安全等级保护 / 数据安全法：强调分类分级与流向
		base["data_class"] = dataClass(e.DetectedEntities)
		base["flow"] = e.Upstream
	default: // pip
		base["pii_count"] = len(e.DetectedEntities)
		base["pii_types"] = entityTypes(e.DetectedEntities)
	}
	return base
}

func entityTypes(s []EntitySummary) []string {
	out := make([]string, 0, len(s))
	for _, e := range s {
		out = append(out, e.Type)
	}
	return out
}

// dataClass 依据实体类型粗分数据级别（演示用）。
func dataClass(s []EntitySummary) string {
	for _, e := range s {
		switch e.Type {
		case "zh_id_card", "zh_bank_card", "api_key", "password", "token":
			return "敏感/重要数据"
		}
	}
	if len(s) > 0 {
		return "一般个人信息"
	}
	return "非敏感"
}
