package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gateway/internal/detector"
	"gateway/pkg/types"
)

// benchSegments 模拟一轮 Agent 多轮对话的历史段（5 段，含中英 PII）。
var benchSegments = []string{
	"用户张三的手机号是13800138000，邮箱 zhangsan@example.com。",
	"His Visa card is 4111 1111 1111 1111 and SSN is 521-45-6789.",
	"身份证号11010119900307867X，收款卡 6222 0210 0123 4567。",
	"Server logs him in from 203.0.113.50; site is https://zhang.example.io.",
	"请把以上信息整理成英文摘要发给 bob_smith@test.org。",
}

// realDetect 用真实 RegexEngine 检测（避免基准只量到 stub）。
func realDetect(seg string) ([]types.Entity, error) {
	eng := detector.NewRegexEngine()
	resp, err := eng.Detect(context.Background(), &types.DetectRequest{Text: seg})
	if err != nil {
		return nil, err
	}
	return resp.Entities, nil
}

// BenchmarkMerkleFullMiss 新会话首轮：5 段全部送真实检测器（冷缓存上界）。
func BenchmarkMerkleFullMiss(b *testing.B) {
	m := NewMerkle(time.Minute)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conv := fmt.Sprintf("bench-full-%d", i)
		res, err := m.GetOrDetect(conv, benchSegments, realDetect)
		if err != nil {
			b.Fatal(err)
		}
		if res.Scanned != len(benchSegments) {
			b.Fatalf("expected %d scanned, got %d", len(benchSegments), res.Scanned)
		}
	}
}

// BenchmarkMerkleIncremental 稳态增量：历史 5 段全部缓存命中，仅 1 个新段送检测器
// —— Agent 多轮对话的典型形态（O(1) 检测量而非 O(n) 全量重扫）。
func BenchmarkMerkleIncremental(b *testing.B) {
	m := NewMerkle(time.Hour)
	conv := "bench-incr"
	segs := append([]string{}, benchSegments...)
	if _, err := m.GetOrDetect(conv, segs, realDetect); err != nil {
		b.Fatal(err)
	}
	// 预生成新段变体，保证每轮迭代都恰好 1 个未缓存段。
	variants := make([]string, 1024)
	for i := range variants {
		variants[i] = fmt.Sprintf("新请求 %d：联系电话 139%08d，问题是怎么开发票？", i, i)
	}
	segs6 := append(segs, "")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		segs6[len(segs6)-1] = variants[i%len(variants)]
		res, err := m.GetOrDetect(conv, segs6, realDetect)
		if err != nil {
			b.Fatal(err)
		}
		if res.Scanned != 1 {
			b.Fatalf("expected 1 scanned segment, got %d", res.Scanned)
		}
	}
}
