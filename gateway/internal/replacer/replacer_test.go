package replacer

import (
	"context"
	"strings"
	"testing"

	"gateway/internal/simulator"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// entAt 按 UTF-8 字节偏移构造实体，避免手写偏移出错。
func entAt(text, typ, value string, score float64) types.Entity {
	i := strings.Index(text, value)
	if i < 0 {
		panic("value not found in text: " + value)
	}
	return types.Entity{Type: typ, Value: value, Start: i, End: i + len(value), Score: score}
}

// simulatorAll 全量开启中文仿真。
func simulatorAll() simulator.SimulateZHConfig {
	return simulator.SimulateZHConfig{PersonName: true, Phone: true, IDCard: true, BankCard: true}
}

func newTestReplacer(strategy string) Replacer {
	return New(Config{
		Strategy:     strategy,
		Irreversible: []string{"api_key", "password", "token"},
		Simulate:     simulatorAll(),
		SessionKey:   []byte("test-session-key"),
	}, nil)
}

// TestNewReplacer_VaultNil 无 vault 时仍可工作（映射表仅内存）。
func TestNewReplacer_VaultNil(t *testing.T) {
	r := newTestReplacer("placeholder")
	out, err := r.Restore(context.Background(), &RestoreRequest{Text: "无映射表", RequestID: "missing"})
	require.NoError(t, err)
	require.Equal(t, "无映射表", out, "映射表缺失时 fail-safe 原样返回")
}

// TestReplace_DoubleMapping 同值同占位符（测试规约 §1.3）。
func TestReplace_DoubleMapping(t *testing.T) {
	r := newTestReplacer("placeholder")
	text := "张三给张三打电话"
	first := strings.Index(text, "张三")
	second := strings.LastIndex(text, "张三")
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      text,
		RequestID: "req1",
		Entities: []types.Entity{
			entAt(text, "zh_person_name", "张三", 0.9),
			{Type: "zh_person_name", Value: "张三", Start: second, End: second + len("张三"), Score: 0.9},
		},
	})
	require.Greater(t, second, first)
	require.NoError(t, err)
	require.Equal(t, "<<zh_person_name_1>>给<<zh_person_name_1>>打电话", res.Text)
	require.Len(t, res.Entries, 1, "同值必须复用同一占位符")
}

// TestReplace_DiffTypeSameValue 不同类型即使值相同 → 不同占位符（契约 §5.2 规则 2）。
func TestReplace_DiffTypeSameValue(t *testing.T) {
	r := newTestReplacer("placeholder")
	text := "110101 与 110101"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      text,
		RequestID: "req1",
		Entities: []types.Entity{
			entAt(text, "zh_person_name", "110101", 0.9),
			{Type: "zh_id_card", Value: "110101", Start: strings.LastIndex(text, "110101"), End: strings.LastIndex(text, "110101") + 6, Score: 0.9},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	require.NotEqual(t, res.Entries[0].Placeholder, res.Entries[1].Placeholder)
	require.Equal(t, "<<zh_person_name_1>> 与 <<zh_id_card_1>>", res.Text)
}

// TestReplace_OffsetPreserved 占位符在脱敏文本中的偏移正确（契约 §5.2 规则 4）。
func TestReplace_OffsetPreserved(t *testing.T) {
	r := newTestReplacer("placeholder")
	text := "联系张三，手机13800138000"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      text,
		RequestID: "req1",
		Entities: []types.Entity{
			entAt(text, "zh_person_name", "张三", 0.9),
			entAt(text, "zh_phone", "13800138000", 0.95),
		},
	})
	require.NoError(t, err)
	for _, e := range res.Entries {
		require.Equal(t, e.Sentinel(), res.Text[e.Start:e.End], "偏移必须能定位到占位符")
	}
	require.Equal(t, "联系<<zh_person_name_1>>，手机<<zh_phone_1>>", res.Text)
}

// TestRestore_ByOffset 正文恰含 <<x>> 不被误还原（测试规约 §1.3）。
func TestRestore_ByOffset(t *testing.T) {
	r := newTestReplacer("placeholder")
	entries := []types.MappingEntry{
		{Placeholder: "<<zh_phone_1>>", Original: []byte("13800138000"), EntityType: "zh_phone", Fate: types.FateReversible},
	}
	// 正文中本来就存在形如占位符的文本，但不在映射表里 → 必须原样保留
	out, err := r.Restore(context.Background(), &RestoreRequest{
		Text:    "字面量 <<not_mapped_1>> 与 <<zh_phone_1>>",
		Entries: entries,
	})
	require.NoError(t, err)
	require.Equal(t, "字面量 <<not_mapped_1>> 与 13800138000", out)
}

// TestStreamRestore_Trie SSE 分块跨块拼接占位符后完整还原（测试规约 §1.3）。
func TestStreamRestore_Trie(t *testing.T) {
	entries := []types.MappingEntry{
		{Placeholder: "<<zh_person_name_1>>", Original: []byte("张三"), EntityType: "zh_person_name", Fate: types.FateReversible},
		{Placeholder: "<<zh_phone_1>>", Original: []byte("13800138000"), EntityType: "zh_phone", Fate: types.FateReversible},
	}
	sr := NewStreamRestorerFromEntries(entries)
	full := "好的，已联系 <<zh_person_name_1>>（<<zh_phone_1>>），请确认。"

	var got string
	// 逐字节喂入，模拟最恶劣的 SSE 切分
	for i := 0; i < len(full); i++ {
		out, err := sr.Write([]byte{full[i]})
		require.NoError(t, err)
		got += string(out)
	}
	rest, err := sr.Close()
	require.NoError(t, err)
	got += string(rest)

	require.Equal(t, "好的，已联系 张三（13800138000），请确认。", got)
	require.NotContains(t, got, "<<", "还原后不得残留占位符")
}

// TestStreamRestore_Orphan 流结束仍有不完整占位符 → 计入 metric 且原样输出（契约 §5.3）。
func TestStreamRestore_Orphan(t *testing.T) {
	before := StreamOrphanTotal.Load()
	sr := NewStreamRestorerFromEntries([]types.MappingEntry{
		{Placeholder: "<<zh_person_name_1>>", Original: []byte("张三"), Fate: types.FateReversible},
	})
	out, err := sr.Write([]byte("结尾是 <<zh_person_na"))
	require.NoError(t, err)
	rest, err := sr.Close()
	require.NoError(t, err)
	require.Equal(t, "结尾是 <<zh_person_na", string(out)+string(rest))
	require.Equal(t, before+1, StreamOrphanTotal.Load())
}

// TestStreamRestorer_OrphansPerInstance 孤儿数必须按实例归属，不能被全局量串号。
//
// 这是 P1-6 的回归护栏：proxy 曾经拿全局 StreamOrphans() 当增量写 Prometheus，
// 并发流式请求下会把 A 的残留记到 B 的 endpoint 标签上，且累计值当增量会指数虚增。
func TestStreamRestorer_OrphansPerInstance(t *testing.T) {
	pairs := []entryPair{{sentinel: "<<email_1>>", restoreTo: "a@b.com"}}
	victim := NewStreamRestorer(pairs)
	bystander := NewStreamRestorer(pairs)

	// victim 制造一个未闭合哨兵；bystander 全程干净。
	_, err := victim.Write([]byte("见 <<e"))
	require.NoError(t, err)
	_, err = victim.Close()
	require.NoError(t, err)

	require.Equal(t, int64(1), victim.Orphans(), "victim 应记 1 次残留")
	require.Equal(t, int64(0), bystander.Orphans(), "bystander 不得被 victim 的残留污染")

	// Close 幂等：重复 Close 不得重复计数（buf 已清空）。
	_, err = victim.Close()
	require.NoError(t, err)
	require.Equal(t, int64(1), victim.Orphans(), "Close 必须幂等，不重复计数")
}

// TestStreamRestorer_OrphansZeroOnCleanStream 正常流（含被拆开的完整占位符）不得计 orphan。
func TestStreamRestorer_OrphansZeroOnCleanStream(t *testing.T) {
	sr := NewStreamRestorer([]entryPair{{sentinel: "<<email_1>>", restoreTo: "a@b.com"}})
	var got strings.Builder
	out, err := sr.Write([]byte("邮箱 <<e"))
	require.NoError(t, err)
	got.Write(out)
	out, err = sr.Write([]byte("mail_1>> 已收到"))
	require.NoError(t, err)
	got.Write(out)
	rest, err := sr.Close()
	require.NoError(t, err)
	got.Write(rest)

	require.Equal(t, "邮箱 a@b.com 已收到", got.String())
	require.Equal(t, int64(0), sr.Orphans(), "跨块拼回的完整占位符不算残留")
}

// TestReplace_Redact 不可逆类型被替换成 [REDACTED] 且不参与还原。
func TestReplace_Redact(t *testing.T) {
	r := newTestReplacer("placeholder")
	text := "token 是 sk-abcdefghijklmnopqrstuvwx"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      text,
		RequestID: "req1",
		Entities:  []types.Entity{entAt(text, "api_key", "sk-abcdefghijklmnopqrstuvwx", 0.95)},
	})
	require.NoError(t, err)
	require.Contains(t, res.Text, RedactedText)
	require.Len(t, res.Entries, 1)
	require.Equal(t, types.FateRedact, res.Entries[0].Fate)

	out, err := r.Restore(context.Background(), &RestoreRequest{Text: res.Text, Entries: res.Entries})
	require.NoError(t, err)
	require.Equal(t, res.Text, out, "redact 不可逆，还原后保持不变")
}

// TestReplace_Mask 银行卡在占位符模式下保留后 4 位。
func TestReplace_Mask(t *testing.T) {
	r := newTestReplacer("placeholder")
	text := "卡号6222021234567890123"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      text,
		RequestID: "req1",
		Entities:  []types.Entity{entAt(text, "zh_bank_card", "6222021234567890123", 0.95)},
	})
	require.NoError(t, err)
	require.Equal(t, "卡号***************0123", res.Text)
	require.Equal(t, types.FateMask, res.Entries[0].Fate)
}

// TestReplaceAndRestore_RoundTrip 替换→还原必须字节级一致（测试规约 §2.1）。
func TestReplaceAndRestore_RoundTrip(t *testing.T) {
	r := newTestReplacer("placeholder")
	orig := "张三的手机号是13800138000，邮箱 zhangsan@example.com"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      orig,
		RequestID: "req1",
		Entities: []types.Entity{
			entAt(orig, "zh_person_name", "张三", 0.9),
			entAt(orig, "zh_phone", "13800138000", 0.95),
			entAt(orig, "email", "zhangsan@example.com", 0.9),
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 3)

	out, err := r.Restore(context.Background(), &RestoreRequest{Text: res.Text, Entries: res.Entries})
	require.NoError(t, err)
	require.Equal(t, orig, out)
}

// TestReplace_Simulate 仿真模式下上游看到的是仿真值，还原后回到原文。
func TestReplace_Simulate(t *testing.T) {
	r := newTestReplacer("simulate")
	orig := "张三的手机号是13800138000"
	res, err := r.Replace(context.Background(), &ReplaceRequest{
		Text:      orig,
		RequestID: "req1",
		Entities: []types.Entity{
			entAt(orig, "zh_person_name", "张三", 0.9),
			entAt(orig, "zh_phone", "13800138000", 0.95),
		},
	})
	require.NoError(t, err)
	require.NotContains(t, res.Text, "张三")
	require.NotContains(t, res.Text, "13800138000")
	require.NotEmpty(t, res.Entries[0].FakeValue, "仿真模式下必须生成仿真值")

	out, err := r.Restore(context.Background(), &RestoreRequest{Text: res.Text, Entries: res.Entries})
	require.NoError(t, err)
	require.Equal(t, orig, out)
}
