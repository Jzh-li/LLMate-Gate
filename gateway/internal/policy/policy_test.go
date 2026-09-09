package policy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

func TestPolicy_Defaults(t *testing.T) {
	p, err := New(nil)
	require.NoError(t, err)

	// 内置不可逆类型默认 redact。
	require.Equal(t, types.FateRedact, p.FateFor(types.EntityAPIKey, "placeholder"))
	require.Equal(t, types.FateRedact, p.FateFor(types.EntityPassword, "placeholder"))
	require.Equal(t, types.FateRedact, p.FateFor(types.EntityToken, "placeholder"))

	// 银行卡占位符模式默认 mask（保留后 4 位）。
	require.Equal(t, types.FateMask, p.FateFor(types.EntityBankCard, "placeholder"))
	// 银行卡仿真模式走可逆仿真。
	require.Equal(t, types.FateReversible, p.FateFor(types.EntityBankCard, "simulate"))

	// 其余默认可逆占位符。
	require.Equal(t, types.FateReversible, p.FateFor(types.EntityPhone, "placeholder"))
	require.Equal(t, types.FateReversible, p.FateFor(types.EntityPersonName, "placeholder"))
	require.Equal(t, types.FateReversible, p.FateFor(types.EntityAddress, "placeholder"))
}

func TestPolicy_Override(t *testing.T) {
	p, err := New(map[string]string{
		"zh_phone":  "mask",
		"zh_id_card": "redact",
	})
	require.NoError(t, err)

	require.Equal(t, types.FateMask, p.FateFor(types.EntityPhone, "placeholder"))
	require.Equal(t, types.FateRedact, p.FateFor(types.EntityIDCard, "placeholder"))
	// 未覆盖的类型回落默认。
	require.Equal(t, types.FateReversible, p.FateFor(types.EntityPersonName, "placeholder"))
	require.True(t, p.HasOverride(types.EntityPhone))
	require.False(t, p.HasOverride(types.EntityEmail))
}

func TestPolicy_WithIrreversible(t *testing.T) {
	p, err := New(nil)
	require.NoError(t, err)
	p = p.WithIrreversible([]string{"api_key", "secret_x"})
	require.Equal(t, types.FateRedact, p.FateFor("api_key", "placeholder"))
	require.Equal(t, types.FateRedact, p.FateFor("secret_x", "placeholder"))
	// 显式覆盖优先于 irreversible 并入。
	p2, err := New(map[string]string{"api_key": "reversible"})
	require.NoError(t, err)
	p2 = p2.WithIrreversible([]string{"api_key"})
	require.Equal(t, types.FateReversible, p2.FateFor("api_key", "placeholder"))
}

func TestPolicy_InvalidFate(t *testing.T) {
	_, err := New(map[string]string{"zh_phone": "bogus"})
	require.Error(t, err)
}
