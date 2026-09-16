package vault

import (
	"fmt"
	"testing"
	"time"

	"gateway/pkg/types"
)

// benchTable 构造一张典型规模的映射表（4 条实体）。
func benchTable(i int) *types.MappingTable {
	return &types.MappingTable{
		RequestID: fmt.Sprintf("bench-req-%d", i),
		Entries: []types.MappingEntry{
			{Placeholder: "«PHONE_1»", Original: []byte("13800138000"), EntityType: "zh_phone", Fate: types.FateReversible},
			{Placeholder: "«EMAIL_1»", Original: []byte("zhangsan@example.com"), EntityType: "email", Fate: types.FateReversible},
			{Placeholder: "«IDCARD_1»", Original: []byte("11010119900307867X"), EntityType: "zh_id_card", Fate: types.FateReversible},
			{Placeholder: "«CARD_1»", Original: []byte("6222021001234567"), EntityType: "zh_bank_card", Fate: types.FateReversible},
		},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

// BenchmarkVaultPutGet Put + Get 一张 4 条目映射表（每请求一次的稳态路径）。
func BenchmarkVaultPutGet(b *testing.B) {
	v, err := NewMemVault(time.Hour, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = v.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.Put(benchTable(i)); err != nil {
			b.Fatal(err)
		}
		if _, err := v.Get(fmt.Sprintf("bench-req-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVaultSealUnseal 加密落盘往返（AES-256-GCM + scrypt）。
func BenchmarkVaultSealUnseal(b *testing.B) {
	v, err := NewMemVault(time.Hour, []byte("bench-passphrase-32-bytes-123456"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = v.Close() })
	tbl := benchTable(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sealed, err := v.Seal(tbl)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := v.Unseal(sealed); err != nil {
			b.Fatal(err)
		}
	}
}
