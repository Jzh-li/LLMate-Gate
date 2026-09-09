package cache

import (
	"testing"
	"time"

	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// TestCache_ConversationBound 不同 conversation_id 同文本 → 不命中（测试规约 §1.3）。
func TestCache_ConversationBound(t *testing.T) {
	c := NewLRU(100, time.Minute, true)
	h := HashText("张三的手机号是13800138000")
	resp := &types.DetectResponse{Entities: []types.Entity{{Type: "zh_phone", Value: "13800138000"}}}

	require.NoError(t, c.Put("conv_A", h, resp))
	got, ok := c.Get("conv_A", h)
	require.True(t, ok)
	require.Equal(t, resp, got)

	_, ok = c.Get("conv_B", h)
	require.False(t, ok, "缓存键必须绑定 conversation_id，否则跨会话实体错乱")
}

// TestCache_TTL 过期后需重新检测（测试规约 §1.3）。
func TestCache_TTL(t *testing.T) {
	c := NewLRU(100, 20*time.Millisecond, true)
	h := HashText("13800138000")
	require.NoError(t, c.Put("conv_A", h, &types.DetectResponse{}))

	_, ok := c.Get("conv_A", h)
	require.True(t, ok)

	time.Sleep(40 * time.Millisecond)
	_, ok = c.Get("conv_A", h)
	require.False(t, ok, "过期后必须重新检测")
}

// TestCache_LRUEviction 超出容量淘汰最久未使用。
func TestCache_LRUEviction(t *testing.T) {
	c := NewLRU(2, time.Minute, true)
	require.NoError(t, c.Put("conv", HashText("a"), &types.DetectResponse{}))
	require.NoError(t, c.Put("conv", HashText("b"), &types.DetectResponse{}))
	_, _ = c.Get("conv", HashText("a")) // a 变为最近使用
	require.NoError(t, c.Put("conv", HashText("c"), &types.DetectResponse{}))

	_, ok := c.Get("conv", HashText("b"))
	require.False(t, ok, "最久未使用的 b 应被淘汰")
	_, ok = c.Get("conv", HashText("a"))
	require.True(t, ok, "最近使用的 a 应保留")
}

// TestCache_Invalidate 按会话失效。
func TestCache_Invalidate(t *testing.T) {
	c := NewLRU(100, time.Minute, true)
	require.NoError(t, c.Put("conv_A", HashText("x"), &types.DetectResponse{}))
	require.NoError(t, c.Put("conv_B", HashText("x"), &types.DetectResponse{}))
	require.NoError(t, c.Invalidate("conv_A"))

	_, ok := c.Get("conv_A", HashText("x"))
	require.False(t, ok)
	_, ok = c.Get("conv_B", HashText("x"))
	require.True(t, ok, "其他会话的缓存不应被误清")
}

// TestCache_Stats 命中率统计。
func TestCache_Stats(t *testing.T) {
	c := NewLRU(100, time.Minute, true)
	h := HashText("x")
	require.NoError(t, c.Put("conv", h, &types.DetectResponse{}))
	_, _ = c.Get("conv", h)
	_, _ = c.Get("conv", HashText("y"))
	hits, misses, size := c.Stats()
	require.Equal(t, int64(1), hits)
	require.Equal(t, int64(1), misses)
	require.Equal(t, 1, size)
}
