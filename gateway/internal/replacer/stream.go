package replacer

import (
	"bytes"
	"sync/atomic"

	"gateway/pkg/types"
)

// StreamOrphanTotal 流结束时仍未闭合的哨兵串计数（契约 §5.3 metric: stream_orphan_placeholder）。
var StreamOrphanTotal atomic.Int64

// StreamRestorer 流式还原器：接收上游 SSE 分块，跨块拼接哨兵串后还原（契约 §5.3）。
//
// 规则：
//   - 闭合后查映射表还原；查不到 → 保留原样（fail-safe，不报错）
//   - Close() 时缓冲仍不完整 → 记录 metric，原样输出
type StreamRestorer struct {
	trie *sentinelTrie
	buf  []byte
}

// NewStreamRestorer 基于映射条目构造流式还原器。
func NewStreamRestorer(entries []entryPair) *StreamRestorer {
	t := newSentinelTrie()
	for _, e := range entries {
		t.Insert(e.sentinel, e.restoreTo)
	}
	return &StreamRestorer{trie: t}
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
// 不可逆（redact）条目不参与还原；mask 条目保留遮盖形态。
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

// Write 接收上游分块，返回可立即输出的已还原块（契约 §5.3）。
func (s *StreamRestorer) Write(chunk []byte) ([]byte, error) {
	if s.trie == nil {
		return chunk, nil
	}
	s.buf = append(s.buf, chunk...)
	var out []byte
	for len(s.buf) > 0 {
		// 跳过不可能成为哨兵串起点的字节
		i := 0
		for i < len(s.buf) && !s.trie.hasStart(s.buf[i]) {
			i++
		}
		if i > 0 {
			out = append(out, s.buf[:i]...)
			s.buf = s.buf[i:]
		}
		if len(s.buf) == 0 {
			break
		}
		n, val, ok := s.trie.longestMatch(s.buf)
		if ok {
			out = append(out, val...)
			s.buf = s.buf[n:]
			continue
		}
		if s.trie.isPrefix(s.buf) {
			// 需要更多数据才能判定，留缓冲等待下一块
			break
		}
		// 不构成任何哨兵串前缀：吐掉首字节继续
		out = append(out, s.buf[0])
		s.buf = s.buf[1:]
	}
	return out, nil
}

// Close 刷新剩余缓冲；残留不完整哨兵串计入 metric（契约 §5.3）。
func (s *StreamRestorer) Close() ([]byte, error) {
	if s.trie == nil {
		return nil, nil
	}
	out := s.buf
	if bytes.Contains(out, []byte("<<")) {
		StreamOrphanTotal.Add(1)
	}
	s.buf = nil
	return out, nil
}
