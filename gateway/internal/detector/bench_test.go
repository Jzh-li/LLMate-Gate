package detector

import (
	"context"
	"testing"

	"gateway/pkg/types"
)

// benchText 中英混合、含多类 PII 的典型 Agent 请求负载（~340 字节）。
const benchText = "用户张三的手机号是13800138000，身份证号11010119900307867X，" +
	"邮箱 zhangsan@example.com，收款卡 6222 0210 0123 4567。" +
	"His Visa card is 4111 1111 1111 1111 and SSN is 521-45-6789. " +
	"Server logs him in from 203.0.113.50; his site is https://zhang.example.io/about. " +
	"请把以上信息整理成英文摘要发给 bob_smith@test.org，并在下周二前同步到 192.168.1.10 的数据库。"

// BenchmarkRegexEngineDetect 全量检测热路径：双语混合文本一次 Detect。
func BenchmarkRegexEngineDetect(b *testing.B) {
	e := NewRegexEngine()
	req := &types.DetectRequest{Text: benchText}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := e.Detect(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if len(resp.Entities) == 0 {
			b.Fatal("expected entities")
		}
	}
}

// BenchmarkRegexEngineDetectBatch 批量检测（一次往返的 embedding/多段场景）。
func BenchmarkRegexEngineDetectBatch(b *testing.B) {
	e := NewRegexEngine()
	reqs := make([]*types.DetectRequest, 8)
	for i := range reqs {
		reqs[i] = &types.DetectRequest{Text: benchText}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.DetectBatch(context.Background(), reqs); err != nil {
			b.Fatal(err)
		}
	}
}
