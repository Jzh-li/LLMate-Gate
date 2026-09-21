package replacer

import (
	"context"
	"fmt"
	"testing"

	"gateway/internal/vault"
	"gateway/pkg/types"
)

// benchTextReplace 与检测基准同源的典型负载。
const benchTextReplace = "用户张三的手机号是13800138000，邮箱 zhangsan@example.com，" +
	"身份证号11010119900307867X，收款卡 6222 0210 0123 4567。" +
	"His Visa card is 4111 1111 1111 1111 and SSN is 521-45-6789."

// benchEntities 返回**写死的**实体列表，故意不现场调用检测器。
//
// 这里原本是 `eng := detector.NewRegexEngine(); eng.Detect(...)`，理由是「与生产
// 路径一致」。那个写法有一个隐蔽缺陷：**本基准的工作量会随检测能力漂移**，
// 于是「检测变强」会被性能门禁读成「replacer 变慢」。
//
// 2026-09-21 就是这么爆的：形态容忍落地后，同一段文本多检出一个分组卡号
// （`4111 1111 1111 1111` —— `6222 0210 0123 4567` 因校验位不通过仍不检出），
// 实体数 4 → 5。两个基准的 ns/op 与 allocs/op 同步上升约 25%，CI 的 perf 门禁
// （阈值 1.25×）因此判「replacer 回退」—— 实际检测器是变强了，replacer 的
// **单位工作量成本没有任何变化**。这是假阳性，根因是基准的工作量定义耦合了
// 被测对象之外的东西。
//
// 两个关注点必须解耦：**检测器能力由 BenchmarkRegexEngineDetect 度量，
// 本基准只度量「给定实体列表时的替换吞吐」。** 否则每新增一类可检出实体，
// 都会给 perf 门禁投一次假阳性。
//
// ⚠️ 改动本列表（或改 benchTextReplace 的偏移）= 改动基准工作量，
// 必须同步复算 `gateway/bench_baseline.txt`，否则门禁会误报。
func benchEntities(b *testing.B) []types.Entity {
	ents := []types.Entity{
		{Type: types.EntityPersonName, Value: "张三", Start: 6, End: 12, Score: 0.75},
		{Type: types.EntityPhone, Value: "13800138000", Start: 27, End: 38, Score: 0.95},
		{Type: types.EntityEmail, Value: "zhangsan@example.com", Start: 48, End: 68, Score: 0.90},
		{Type: types.EntityUSSSN, Value: "521-45-6789", Start: 184, End: 195, Score: 0.90},
	}
	// 防线：列表必须与 benchTextReplace 的偏移逐字一致，否则基准测的是别的东西。
	for _, e := range ents {
		if got := benchTextReplace[e.Start:e.End]; got != e.Value {
			b.Fatalf("基准实体列表与文本不符：%s [%d,%d) = %q，期望 %q",
				e.Type, e.Start, e.End, got, e.Value)
		}
	}
	return ents
}

// BenchmarkReplacerSessionReplace 会话内占位符替换热路径（每请求一次）。
func BenchmarkReplacerSessionReplace(b *testing.B) {
	v, err := vault.NewMemVault(0, nil)
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
	v, err := vault.NewMemVault(0, nil)
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
