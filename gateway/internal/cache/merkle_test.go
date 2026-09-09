package cache

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

// countingDetect 返回一个 detectFn：每调用一次计数 +1，并对每段吐出一个实体（value=段文本）。
func countingDetect(calls *int) func(seg string) ([]types.Entity, error) {
	return func(seg string) ([]types.Entity, error) {
		*calls++
		return []types.Entity{
			{Type: "seg", Value: seg, Start: 0, End: len(seg), Score: 0.9},
		}, nil
	}
}

func TestMerkle_AppendOnlyScansNewTail(t *testing.T) {
	m := NewMerkle(0)
	calls := 0
	detect := countingDetect(&calls)

	// turn 1：单段
	res1, err := m.GetOrDetect("c1", []string{"A"}, detect)
	require.NoError(t, err)
	require.Equal(t, 1, res1.Scanned)
	require.Len(t, res1.Segments, 1)
	require.Equal(t, 1, calls, "turn1 应只检测 1 段")

	// turn 2：追加 B，前缀 A 应命中缓存，只检测 B
	res2, err := m.GetOrDetect("c1", []string{"A", "B"}, detect)
	require.NoError(t, err)
	require.Equal(t, 1, res2.Scanned, "turn2 只应检测新增的 B")
	require.Len(t, res2.Segments, 2)
	require.Equal(t, 2, calls, "两轮共应只检测 2 段（A 复用缓存）")

	// turn 3：完全相同序列 → 全缓存命中，零检测
	res3, err := m.GetOrDetect("c1", []string{"A", "B"}, detect)
	require.NoError(t, err)
	require.Equal(t, 0, res3.Scanned)
	require.Equal(t, 2, calls, "第三次请求不应再触发检测")
}

func TestMerkle_PrefixDivergenceRetects(t *testing.T) {
	m := NewMerkle(0)
	calls := 0
	detect := countingDetect(&calls)

	_, err := m.GetOrDetect("c1", []string{"A", "B"}, detect)
	require.NoError(t, err)
	require.Equal(t, 2, calls)

	// 历史被改写：第二段从 B 变 X → 前缀 A 命中，B 失效，只重检 X
	res, err := m.GetOrDetect("c1", []string{"A", "X"}, detect)
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned)
	require.Len(t, res.Segments, 2)
	require.Equal(t, 3, calls, "B 已被 X 替换，不应复用")

	// 验证实体内容已跟随新序列
	require.Equal(t, "A", res.Segments[0].Entities[0].Value)
	require.Equal(t, "X", res.Segments[1].Entities[0].Value)
}

func TestMerkle_TruncationResets(t *testing.T) {
	m := NewMerkle(0)
	calls := 0
	detect := countingDetect(&calls)

	_, err := m.GetOrDetect("c1", []string{"A", "B", "C"}, detect)
	require.NoError(t, err)
	require.Equal(t, 3, calls)

	// 段数变少 → 截断到前缀，后续不再复用被删段
	res, err := m.GetOrDetect("c1", []string{"A"}, detect)
	require.NoError(t, err)
	require.Equal(t, 0, res.Scanned, "A 仍在前缀，应零检测")
	require.Len(t, res.Segments, 1)
	require.Equal(t, 3, calls, "截断后不应重新检测任何段")
}

func TestMerkle_Invalidate(t *testing.T) {
	m := NewMerkle(0)
	calls := 0
	detect := countingDetect(&calls)

	_, _ = m.GetOrDetect("c1", []string{"A"}, detect)
	require.Equal(t, 1, calls)
	m.Invalidate("c1")
	// 失效后重新检测
	_, err := m.GetOrDetect("c1", []string{"A"}, detect)
	require.NoError(t, err)
	require.Equal(t, 2, calls, "失效后应重新检测 A")
	require.Equal(t, 1, m.Size())
}
