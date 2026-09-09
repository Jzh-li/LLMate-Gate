// Package cache 实现 detection_cache（契约 §8）。
//
// MerkleCache 是会话级「增量检测」缓存（技术方案 §10.3 / Phase 2 阶段 2）：
// 把一次多轮对话视为「有序文本段序列」，每段（通常是某条 message 的 content）检测一次后
// 按其在序列中的位置缓存。下一轮请求到达时只需检测「新增的尾部段」，命中前缀的段直接复用
// 缓存结果 —— 即「只扫新增 turn」，避免 Agent 多轮重扫全量历史（PrivAiTe 实测：不开缓存
// 42s/请求，开缓存 1-3s）。
//
// 正确性：缓存键绑定 conversation_id（契约 §8.2），并以每段 SHA-256 构成的前缀链标识
// 「前缀完整性」——历史被改写（某段文本变化 / 段数变少）时，前缀匹配在首个分叉处截断，
// 后续段重新检测，不会复用错误结果。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"gateway/pkg/types"
)

// MerkleCache 会话级有序段增量检测缓存。
type MerkleCache struct {
	mu   sync.Mutex
	conv map[string]*convEntry
	ttl  time.Duration
}

// convEntry 某会话的完整缓存：有序段 + 整体过期时间。
type convEntry struct {
	segs      []merkleSeg
	expiresAt time.Time
}

// merkleSeg 单段缓存：文本哈希（用于前缀匹配）+ 检测出的实体（偏移相对本段文本）。
type merkleSeg struct {
	hash     [32]byte
	entities []types.Entity
}

// SegmentResult 单段的检测结果（偏移相对本段文本）。
type SegmentResult struct {
	Text     string
	Entities []types.Entity
}

// ConvResult 一次 GetOrDetect 的结果。
type ConvResult struct {
	Segments []SegmentResult
	Scanned  int // 本次实际送检测器的段数（0 表示全部缓存命中）
}

// NewMerkle 构造 Merkle 增量缓存。
func NewMerkle(ttl time.Duration) *MerkleCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &MerkleCache{
		conv: map[string]*convEntry{},
		ttl:  ttl,
	}
}

// GetOrDetect 给定有序段序列，返回每段的检测结果（旧段缓存、新段检测）。
//
// 前缀匹配：从首段起逐段比对 SHA-256，连续相等的段直接复用缓存实体（零检测）。
// 首个分叉 / 段数变少 → 截断后续，仅检测分叉之后的尾部段。
// detectFn 仅在「新增尾部段」上被调用；任一新段检测失败整体返回 error，由调用方回落非增量路径。
func (m *MerkleCache) GetOrDetect(convID string, segments []string, detect func(seg string) ([]types.Entity, error)) (*ConvResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var stored []merkleSeg
	if ce, ok := m.conv[convID]; ok && time.Now().Before(ce.expiresAt) {
		stored = ce.segs
	}

	// 1) 计算匹配前缀长度 k：连续段哈希相等的最大前缀。
	k := 0
	for k < len(stored) && k < len(segments) {
		if stored[k].hash != sha256.Sum256([]byte(segments[k])) {
			break
		}
		k++
	}
	// 历史被改写（分叉或缩短）→ 截断到 k。
	if k < len(stored) {
		stored = stored[:k]
	}

	res := &ConvResult{Segments: make([]SegmentResult, 0, len(segments))}
	for i := 0; i < k; i++ {
		res.Segments = append(res.Segments, SegmentResult{
			Text:     segments[i],
			Entities: stored[i].entities,
		})
	}

	// 2) 检测新增尾部段，写回缓存。
	scanned := 0
	for i := k; i < len(segments); i++ {
		seg := segments[i]
		ents, err := detect(seg)
		if err != nil {
			return nil, err
		}
		scanned++
		stored = append(stored, merkleSeg{
			hash:     sha256.Sum256([]byte(seg)),
			entities: ents,
		})
		res.Segments = append(res.Segments, SegmentResult{Text: seg, Entities: ents})
	}

	m.conv[convID] = &convEntry{segs: stored, expiresAt: time.Now().Add(m.ttl)}
	res.Scanned = scanned
	return res, nil
}

// Root 返回某会话当前前缀链的 Merkle 根（便于日志 / 调试标识）。
func (m *MerkleCache) Root(convID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ce, ok := m.conv[convID]
	if !ok {
		return ""
	}
	h := sha256.New()
	for _, s := range ce.segs {
		h.Write(s.hash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Invalidate 清空某会话的全部缓存（契约 §8.1）。
func (m *MerkleCache) Invalidate(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.conv, convID)
}

// Sweep 清理过期会话。
func (m *MerkleCache) Sweep() int {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for cid, ce := range m.conv {
		if now.After(ce.expiresAt) {
			delete(m.conv, cid)
			n++
		}
	}
	return n
}

// Size 当前缓存的会话数（调试/指标）。
func (m *MerkleCache) Size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conv)
}
