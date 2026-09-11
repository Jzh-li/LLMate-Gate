package replacer

import (
	"context"
	"fmt"
	"testing"

	"gateway/internal/detector"
	"gateway/internal/vault"
	"gateway/pkg/types"
)

// benchTextReplace 与检测基准同源的典型负载。
const benchTextReplace = "用户张三的手机号是13800138000，邮箱 zhangsan@example.com，" +
	"身份证号11010119900307867X，收款卡 6222 0210 0123 4567。" +
	"His Visa card is 4111 1111 1111 1111 and SSN is 521-45-6789."

// benchEntities 用真实 RegexEngine 得到实体列表（与生产路径一致）。
func benchEntities(b *testing.B) []types.Entity {
	eng := detector.NewRegexEngine()
	resp, err := eng.Detect(context.Background(), &types.DetectRequest{Text: benchTextReplace})
	if err != nil {
		b.Fatal(err)
	}
	if len(resp.Entities) < 4 {
		b.Fatalf("expected >=4 entities, got %d", len(resp.Entities))
	}
	return resp.Entities
}

// BenchmarkReplacerSessionReplace 会话内占位符替换热路径（每请求一次）。
func BenchmarkReplacerSessionReplace(b *testing.B) {
	v, err := vault.NewMemVault(0, nil, false, "")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = v.Close() })
	r := New(Config{Strategy: "placeholder"}, v)
	ents := benchEntities(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := r.NewSession()
		if _, _, err := s.Replace(benchTextReplace, ents); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReplacerFullReplace 完整 Replace（含 vault Put 的请求级路径）。
func BenchmarkReplacerFullReplace(b *testing.B) {
	v, err := vault.NewMemVault(0, nil, false, "")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = v.Close() })
	r := New(Config{Strategy: "placeholder"}, v)
	ents := benchEntities(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := &ReplaceRequest{
			Text:           benchTextReplace,
			RequestID:      fmt.Sprintf("bench-req-%d", i),
			ConversationID: "bench-conv",
			Entities:       ents,
		}
		if _, err := r.Replace(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
