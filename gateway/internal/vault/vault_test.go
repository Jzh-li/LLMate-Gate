package vault

import (
	"context"
	"testing"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

func newTestVault(t *testing.T, ttl time.Duration) *MemVault {
	t.Helper()
	v, err := NewMemVault(ttl, []byte("unit-test-passphrase"), false, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func sampleTable(id string) *types.MappingTable {
	return &types.MappingTable{
		RequestID:      id,
		ConversationID: "conv_1",
		Entries: []types.MappingEntry{
			{Placeholder: "<<zh_person_name_1>>", Original: []byte("张三"), EntityType: "zh_person_name", Fate: types.FateReversible, Start: 0, End: 20},
			{Placeholder: "<<zh_phone_1>>", Original: []byte("13800138000"), EntityType: "zh_phone", Fate: types.FateReversible, Start: 21, End: 35},
		},
		CreatedAt: time.Now(),
	}
}

// TestSealUnseal_RoundTrip 序列化→加密→解密→反序列化，字段一致（测试规约 §1.3）。
func TestSealUnseal_RoundTrip(t *testing.T) {
	v := newTestVault(t, 30*time.Minute)
	in := sampleTable("req_rt")
	blob, err := v.Seal(in)
	require.NoError(t, err)
	require.Equal(t, "LMGV", string(blob[:4]), "落盘文件必须有 magic 头")

	out, err := v.Unseal(blob)
	require.NoError(t, err)
	require.Equal(t, in.RequestID, out.RequestID)
	require.Equal(t, in.ConversationID, out.ConversationID)
	require.Len(t, out.Entries, len(in.Entries))
	require.Equal(t, in.Entries[0].Placeholder, out.Entries[0].Placeholder)
	require.Equal(t, string(in.Entries[0].Original), string(out.Entries[0].Original))
	require.Equal(t, in.Entries[0].Fate, out.Entries[0].Fate)
}

// TestSealUnseal_Tamper 篡改 ciphertext 必须解密失败（AES-GCM 认证加密）。
func TestSealUnseal_Tamper(t *testing.T) {
	v := newTestVault(t, 30*time.Minute)
	blob, err := v.Seal(sampleTable("req_t"))
	require.NoError(t, err)
	blob[len(blob)-1] ^= 0xFF
	_, err = v.Unseal(blob)
	require.Error(t, err)
}

// TestTTL_Expired 过期后 Get → not_found（测试规约 §1.3）。
func TestTTL_Expired(t *testing.T) {
	v := newTestVault(t, time.Minute)
	tbl := sampleTable("req_ttl")
	tbl.CreatedAt = time.Now().Add(-2 * time.Minute)
	tbl.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, v.Put(tbl))

	_, err := v.Get("req_ttl")
	require.Error(t, err)
	e, ok := gatewayerrors.As(err)
	require.True(t, ok)
	require.Equal(t, gatewayerrors.CodeNotFound, e.Code)
}

// TestPutGetAndSweep 正常读写 + 过期清理。
func TestPutGetAndSweep(t *testing.T) {
	v := newTestVault(t, time.Minute)
	require.NoError(t, v.Put(sampleTable("req_a")))
	got, err := v.Get("req_a")
	require.NoError(t, err)
	require.Equal(t, "req_a", got.RequestID)
	require.Equal(t, 1, v.Len())

	expired := sampleTable("req_b")
	expired.CreatedAt = time.Now().Add(-5 * time.Minute)
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, v.Put(expired))
	n, err := v.Sweep()
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, v.Len())
}

// TestMemzero 明文 Original 处理后清零（测试规约 §1.3）。
func TestMemzero(t *testing.T) {
	e := types.MappingEntry{Original: []byte("张三"), FakeValue: []byte("李雷")}
	e.Zeroize()
	for _, b := range e.Original {
		require.Equal(t, byte(0), b)
	}
	for _, b := range e.FakeValue {
		require.Equal(t, byte(0), b)
	}
}

// TestDelete_Zeroizes 删除映射表时清零明文。
func TestDelete_Zeroizes(t *testing.T) {
	v := newTestVault(t, time.Minute)
	tbl := sampleTable("req_z")
	require.NoError(t, v.Put(tbl))
	require.NoError(t, v.Delete("req_z"))
	for _, e := range tbl.Entries {
		for _, b := range e.Original {
			require.Equal(t, byte(0), b, "删除后明文必须清零")
		}
	}
	_, err := v.Get("req_z")
	require.Error(t, err)
}

// TestStartSweeper 后台清理协程可被 ctx 取消。
func TestStartSweeper(t *testing.T) {
	v := newTestVault(t, time.Minute)
	require.NoError(t, v.Put(sampleTable("req_s")))
	ctx, cancel := context.WithCancel(context.Background())
	v.StartSweeper(ctx, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	cancel()
}

// TestPut_RequiresRequestID 缺少 request_id 直接报错。
func TestPut_RequiresRequestID(t *testing.T) {
	v := newTestVault(t, time.Minute)
	require.Error(t, v.Put(&types.MappingTable{}))
}
