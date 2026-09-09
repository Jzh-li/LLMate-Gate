// Package cache 实现 detection_cache（契约 §8）。
//
// 硬约束：缓存键必须包含 conversation_id（契约 §8.2），否则跨会话复用会导致实体错乱。
package cache

import (
	"container/list"
	"crypto/sha256"
	"sync"
	"time"

	"gateway/pkg/types"
)

// Cache 检测结果缓存（契约 §8.1）。
type Cache interface {
	Get(conversationID string, textHash [32]byte) (*types.DetectResponse, bool)
	Put(conversationID string, textHash [32]byte, resp *types.DetectResponse) error
	Invalidate(conversationID string) error
	// Sweep 清理过期条目。
	Sweep() int
}

type entry struct {
	key        string
	resp       *types.DetectResponse
	expiresAt  time.Time
	conversationID string
	listElem   *list.Element
}

// LRU 进程内 LRU + TTL 缓存（契约 §8.2：v1 用 sync.Map + LRU，max 10000 条）。
type LRU struct {
	mu         sync.Mutex
	items      map[string]*entry
	order      *list.List // 最近使用在队首
	maxEntries int
	ttl        time.Duration
	bindConv   bool

	hits   int64
	misses int64
}

// NewLRU 构造 LRU 缓存。
func NewLRU(maxEntries int, ttl time.Duration, bindConversation bool) *LRU {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &LRU{
		items:      make(map[string]*entry),
		order:      list.New(),
		maxEntries: maxEntries,
		ttl:        ttl,
		bindConv:   bindConversation,
	}
}

// buildKey 缓存键 = conversation_id + ":" + hex(textHash)。
// bindConv 关闭时用 "-" 占位（仅单会话调试场景允许）。
func (c *LRU) buildKey(conversationID string, h [32]byte) string {
	conv := conversationID
	if !c.bindConv {
		conv = "-"
	}
	b := make([]byte, 0, len(conv)+1+64)
	b = append(b, conv...)
	b = append(b, ':')
	const hexTable = "0123456789abcdef"
	for _, v := range h {
		b = append(b, hexTable[v>>4], hexTable[v&0x0f])
	}
	return string(b)
}

// Get 命中返回缓存结果（契约 §8.1）。
func (c *LRU) Get(conversationID string, textHash [32]byte) (*types.DetectResponse, bool) {
	key := c.buildKey(conversationID, textHash)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		c.order.Remove(e.listElem)
		delete(c.items, key)
		c.misses++
		return nil, false
	}
	c.order.MoveToFront(e.listElem)
	c.hits++
	return e.resp, true
}

// Put 写入缓存，超出容量淘汰最久未使用（契约 §8.1）。
func (c *LRU) Put(conversationID string, textHash [32]byte, resp *types.DetectResponse) error {
	key := c.buildKey(conversationID, textHash)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.resp = resp
		e.expiresAt = time.Now().Add(c.ttl)
		c.order.MoveToFront(e.listElem)
		return nil
	}
	e := &entry{
		key:            key,
		resp:           resp,
		expiresAt:      time.Now().Add(c.ttl),
		conversationID: conversationID,
	}
	e.listElem = c.order.PushFront(e)
	c.items[key] = e
	for c.order.Len() > c.maxEntries {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		if old, ok := back.Value.(*entry); ok {
			delete(c.items, old.key)
		}
	}
	return nil
}

// Invalidate 清空某会话的全部缓存（契约 §8.1）。
func (c *LRU) Invalidate(conversationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.bindConv {
		// 未绑定会话时无法按会话清理，整体失效以保证正确性。
		c.items = make(map[string]*entry)
		c.order.Init()
		return nil
	}
	for k, e := range c.items {
		if e.conversationID == conversationID {
			c.order.Remove(e.listElem)
			delete(c.items, k)
		}
	}
	return nil
}

// Stats 命中/未命中计数，供 /metrics。
func (c *LRU) Stats() (hits, misses int64, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.order.Len()
}

// Sweep 清理过期条目。
func (c *LRU) Sweep() int {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, e := range c.items {
		if now.After(e.expiresAt) {
			c.order.Remove(e.listElem)
			delete(c.items, k)
			n++
		}
	}
	return n
}

// HashText 文本哈希（契约 §8.2：SHA-256(text)）。
func HashText(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// 编译期接口断言。
var _ Cache = (*LRU)(nil)
