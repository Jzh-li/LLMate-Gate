package replacer

import (
	"bytes"
	"sync/atomic"

	"gateway/pkg/types"
)

// StreamOrphanTotal 流结束时仍未闭合/未匹配的哨兵串累计数（契约 §5.3 metric: stream_orphan_placeholder）。
//
// 这是**进程级**累计量，仅用于测试断言与运维粗看；不要把它当成本次请求的增量
// 写进 Prometheus —— 它与请求无关，并发下会把别的请求的残留算到自己头上。
// 按请求归属请用 StreamRestorer.Orphans()。
var StreamOrphanTotal atomic.Int64

// StreamOrphans 返回全局残留占位符累计数（进程级）。
func StreamOrphans() int64 { return StreamOrphanTotal.Load() }

// StreamRestorer 流式还原器：接收上游分块（可能是 SSE 原始字节），在缓冲中跨块拼接
// 哨兵串后还原为原文（契约 §5.3）。
//
// 设计要点：
//   - 哨兵串既可能是占位符模式的 `<<type_index>>`，也可能是仿真模式的仿真值
//     （如「叶谈」），二者都不含统一分隔符，因此本实现对「哨兵串」做统一的多模式流式匹配，
//     而非仅识别 `<<`/`>>` 定界符。
//   - SSE 事件分隔符（\n\n）落在 data 字段之外，占位符字节在流中是连续的；本实现通过
//     缓冲「可能是某哨兵串前缀」的尾部字节来保证跨块拼接，哨兵串被完整到达即还原，
//     否则保留缓冲等待后续分块。查不到（不匹配且无前缀关系）→ 原样透传（fail-safe）。
type StreamRestorer struct {
	table   map[string]string // 哨兵串 → 原值
	buf     []byte            // 跨块缓冲，仅保留「可能成为哨兵前缀」的尾部
	orphans int64             // 本还原器（即本请求）累计的残留哨兵数
}

// Orphans 返回**本还原器**（通常是单个请求）累计的残留哨兵数。
//
// 之所以要按实例计数而不是读全局 StreamOrphanTotal：Prometheus 计数器要的是
// 增量，而全局量是累计值；更关键的是并发流式请求下，全局量会把 A 请求的残留
// 记到 B 请求的 endpoint 标签上。本方法保证归属精确。
func (s *StreamRestorer) Orphans() int64 { return s.orphans }

// NewStreamRestorer 基于映射条目构造流式还原器。
func NewStreamRestorer(entries []entryPair) *StreamRestorer {
	if len(entries) == 0 {
		// table 留 nil → Write 直接原样透传（bypass 模式每次请求都走这里）。
		return &StreamRestorer{}
	}
	t := make(map[string]string, len(entries))
	for _, e := range entries {
		t[e.sentinel] = e.restoreTo
	}
	return &StreamRestorer{table: t}
}

// entryPair 上游可见串 → 还原目标。
type entryPair struct {
	sentinel  string
	restoreTo string
}

// NewStreamRestorerFromEntries 直接从映射条目构造还原器（pipeline 主路径）。
func NewStreamRestorerFromEntries(entries []types.MappingEntry) *StreamRestorer {
	return NewStreamRestorer(EntryPairs(entries))
}

// EntryPairs 把映射条目转为 (哨兵串 → 原值) 对，供 StreamRestorer 使用。
// 不可逆（redact）条目不参与还原；mask 条目保留遮盖形态（不在此还原）。
func EntryPairs(entries []types.MappingEntry) []entryPair {
	out := make([]entryPair, 0, len(entries))
	for _, e := range entries {
		if e.Fate != types.FateReversible {
			continue
		}
		out = append(out, entryPair{sentinel: e.Sentinel(), restoreTo: string(e.Original)})
	}
	return out
}

// Write 接收上游分块，返回可立即输出的已还原字节（契约 §5.3）。
func (s *StreamRestorer) Write(chunk []byte) ([]byte, error) {
	if s.table == nil {
		return chunk, nil
	}
	s.buf = append(s.buf, chunk...)
	var out []byte
	i := 0
	for i < len(s.buf) {
		// 1) 是否有哨兵从位置 i 开始（取最长匹配，避免「叶」与「叶谈」冲突）。
		if val, slen := s.matchAt(i); slen > 0 {
			out = append(out, val...)
			i += slen
			continue
		}
		// 2) buf[i:] 是某哨兵串的严格前缀 → 必须留在缓冲，等后续分块补全后再还原。
		if s.isPrefixOfAny(s.buf[i:]) {
			break
		}
		// 3) 普通字节，安全透传。
		out = append(out, s.buf[i])
		i++
	}
	s.buf = s.buf[i:]
	return out, nil
}

// matchAt 返回从 i 开始的最长匹配哨兵的原值与长度（slen==0 表示无匹配）。
func (s *StreamRestorer) matchAt(i int) (string, int) {
	best := ""
	bestLen := 0
	for k, v := range s.table {
		if len(k) > bestLen && bytes.HasPrefix(s.buf[i:], []byte(k)) {
			bestLen = len(k)
			best = v
		}
	}
	return best, bestLen
}

// isPrefixOfAny 判断 suffix 是否为某个哨兵串的严格前缀（长度更短且为其前缀）。
func (s *StreamRestorer) isPrefixOfAny(suffix []byte) bool {
	if len(suffix) == 0 {
		return false
	}
	for k := range s.table {
		if len(k) > len(suffix) && bytes.HasPrefix([]byte(k), suffix) {
			return true
		}
	}
	return false
}

// Close 刷新剩余缓冲；残留不完整哨兵串计入 metric（契约 §5.3）。
func (s *StreamRestorer) Close() ([]byte, error) {
	if s.table == nil {
		return nil, nil
	}
	var out []byte
	if val, slen := s.matchAt(0); slen > 0 {
		out = append(out, val...)
	} else if s.isPrefixOfAny(s.buf) {
		// 残留为某哨兵前缀，无法还原 → 计入 orphan，原样吐出（fail-safe，不丢数据）。
		s.orphans++
		StreamOrphanTotal.Add(1)
		out = append(out, s.buf...)
	} else {
		out = append(out, s.buf...)
	}
	s.buf = nil
	return out, nil
}
