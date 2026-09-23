// Package debug 实现内嵌调试面板：HTTP/WS 路由、流量事件广播、Playground（契约 §10 / UI设计 §0-4）。
//
// 设计原则：
//  1. 非阻塞 + recover 保护：Publish 永远不影响主代理流程（UI设计 §1.3）。
//  2. 环形缓冲：默认 200 条（可配），超出丢弃最旧（UI设计 §1.2 store.go）。
//  3. 127.0.0.1 绑定：默认仅本机访问（UI设计 §4.1 / 契约 §10.1）。
//  4. --no-debug 关闭后所有 debug 端点 404（UI设计 §5.1 D5）。
//
// 调用关系：
//
//	pipeline.Processor (Publisher) ──► Hub ──► WS subscribers
//	                               └► Store ──► GET /_api/traffic
package debug

import (
	"sync"

	"gateway/internal/pipeline"
	"gateway/pkg/types"
)

// MappingEntry 是 pipeline.MappingEntry 的别名，方便 debug/handler.go 直接用本地名构造。
type MappingEntry = pipeline.MappingEntry

// TrafficRecordStore 流量记录存储（按 RequestID 去重，最新覆盖）。
//
// 设计要点：
//   - 按 RequestID 去重：pipeline 会对同一请求发多个增量事件（request.received →
//     upstream.response → restore.done），最后一次发布应包含完整 5 段。
//   - 容量上限（默认 200）超出按插入顺序淘汰最旧。
//   - Snapshot 返回深拷贝，避免外部引用篡改。
type TrafficRecordStore struct {
	mu       sync.RWMutex
	byID     map[string]*pipeline.TrafficEvent // RequestID → 最新记录
	order    []string                          // 插入顺序（最旧 → 最新）
	capacity int
}

// NewTrafficStore 构造容量为 cap 的存储（cap<=0 时取默认 200）。
func NewTrafficStore(cap int) *TrafficRecordStore {
	if cap <= 0 {
		cap = 200
	}
	return &TrafficRecordStore{
		byID:     make(map[string]*pipeline.TrafficEvent, cap),
		order:    make([]string, 0, cap),
		capacity: cap,
	}
}

// Push 写入/合并一条流量事件；同 RequestID 会被最新覆盖（合并非空字段）。
func (s *TrafficRecordStore) Push(ev *pipeline.TrafficEvent) {
	if ev == nil || ev.RequestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byID[ev.RequestID]; ok {
		mergeTrafficEvent(existing, ev)
		return
	}
	// 容量满：淘汰最旧
	if len(s.order) >= s.capacity {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, old)
	}
	s.byID[ev.RequestID] = cloneTrafficEvent(ev)
	s.order = append(s.order, ev.RequestID)
}

// Snapshot 返回所有流量记录的深拷贝（按时间顺序：最旧 → 最新）。
func (s *TrafficRecordStore) Snapshot() []*pipeline.TrafficEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*pipeline.TrafficEvent, 0, len(s.order))
	for _, id := range s.order {
		if ev := s.byID[id]; ev != nil {
			out = append(out, cloneTrafficEvent(ev))
		}
	}
	return out
}

// Clear 清空所有记录（DELETE /_api/traffic 用）。
func (s *TrafficRecordStore) Clear() {
	s.mu.Lock()
	s.byID = make(map[string]*pipeline.TrafficEvent, s.capacity)
	s.order = make([]string, 0, s.capacity)
	s.mu.Unlock()
}

// Len 当前有效条目数。
func (s *TrafficRecordStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// Cap 容量上限。
func (s *TrafficRecordStore) Cap() int { return s.capacity }

// mergeTrafficEvent 把 src 的非空字段合并到 dst（增量发布：partial → full）。
func mergeTrafficEvent(dst, src *pipeline.TrafficEvent) {
	if src.Method != "" {
		dst.Method = src.Method
	}
	if src.Stream {
		dst.Stream = src.Stream
	}
	if src.RawRequest != "" {
		dst.RawRequest = src.RawRequest
	}
	if len(src.Detected) > 0 {
		dst.Detected = src.Detected
	}
	if src.Replaced != "" {
		dst.Replaced = src.Replaced
	}
	if src.UpstreamResp != "" {
		dst.UpstreamResp = src.UpstreamResp
	}
	if src.Restored != "" {
		dst.Restored = src.Restored
	}
	if len(src.Mapping) > 0 {
		dst.Mapping = src.Mapping
	}
	if src.Strategy != "" {
		dst.Strategy = src.Strategy
	}
	if src.Outcome != "" {
		dst.Outcome = src.Outcome
	}
	if src.Error != "" {
		dst.Error = src.Error
	}
	if src.DurationMs > 0 {
		dst.DurationMs = src.DurationMs
	}
	if !src.StartedAt.IsZero() {
		dst.StartedAt = src.StartedAt
	}
	if !src.FinishedAt.IsZero() {
		dst.FinishedAt = src.FinishedAt
	}
	if !src.Timestamp.IsZero() {
		dst.Timestamp = src.Timestamp
	}
}

// cloneTrafficEvent 深拷贝。
func cloneTrafficEvent(e *pipeline.TrafficEvent) *pipeline.TrafficEvent {
	c := *e
	if e.Detected != nil {
		c.Detected = make([]types.Entity, len(e.Detected))
		copy(c.Detected, e.Detected)
	}
	if e.Mapping != nil {
		c.Mapping = make([]pipeline.MappingEntry, len(e.Mapping))
		copy(c.Mapping, e.Mapping)
	}
	return &c
}
