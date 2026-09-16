package pipeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/vault"
	"gateway/pkg/types"
)

// TestAssertRestorableViaAPI 锁定「按 request_id 还原原文」的权限边界。
//
// 这条不变式之前不存在：映射表只按 ID 索引，而数据面的 ID 来自客户端可控的
// X-Request-ID。于是任何能调控制面 /v1/privacy/restore 的调用方，只要猜到（或从
// 日志里读到）另一个请求的 ID，就能把它脱敏掉的原文读回来——一条绕过 vault 加密、
// 绕过 TTL 语义、也不经过任何内容审计的读取路径。
//
// 现在还原前必须通过来源校验：
//   - OriginControl（由 /v1/privacy/redact 建立）→ 放行
//   - OriginData（由 /v1/* LLM 请求建立）→ 拒绝
//   - Origin 为空（旧数据 / 未标注）→ 拒绝（缺省取收紧的一侧）
//   - 不存在 / 已过期 → 拒绝
//
// 四种拒绝统一为 not_found：区分开就等于给调用方一个「这个 ID 存不存在」的探针。
func TestAssertRestorableViaAPI(t *testing.T) {
	v, err := vault.NewMemVault(time.Minute, []byte("pipeline-test-passphrase"))
	require.NoError(t, err)
	p := New(Config{Vault: v})

	future := time.Now().Add(time.Minute)
	put := func(id, origin string) {
		t.Helper()
		require.NoError(t, v.Put(&types.MappingTable{
			RequestID: id, Origin: origin, ExpiresAt: future,
		}))
	}
	put("data-plane-id", types.OriginData)
	put("control-plane-id", types.OriginControl)
	put("legacy-id", "") // 空 origin：升级前遗留 / 未标注

	t.Run("控制面自建的表可还原", func(t *testing.T) {
		require.NoError(t, p.AssertRestorableViaAPI("control-plane-id"))
	})

	t.Run("数据面的表不可经 API 还原", func(t *testing.T) {
		err := p.AssertRestorableViaAPI("data-plane-id")
		require.ErrorIs(t, err, gatewayerrors.ErrNotFound,
			"数据面表被控制面 API 还原 = 越权读取")
	})

	t.Run("未标注来源的表按数据面处理（缺省收紧）", func(t *testing.T) {
		require.ErrorIs(t, p.AssertRestorableViaAPI("legacy-id"), gatewayerrors.ErrNotFound)
	})

	t.Run("不存在的 id 与越权的 id 返回同一个错误", func(t *testing.T) {
		missing := p.AssertRestorableViaAPI("no-such-id")
		denied := p.AssertRestorableViaAPI("data-plane-id")
		require.ErrorIs(t, missing, gatewayerrors.ErrNotFound)
		require.ErrorIs(t, denied, gatewayerrors.ErrNotFound)
		// 同码同文案：调用方无法据此判断 ID 是否存在
		require.Equal(t, missing.Error(), denied.Error())
	})

	t.Run("过期后不可还原", func(t *testing.T) {
		require.NoError(t, v.Put(&types.MappingTable{
			RequestID: "expired-ctrl", Origin: types.OriginControl,
			ExpiresAt: time.Now().Add(-time.Second),
		}))
		// Put 只在 ExpiresAt 为零时才补默认值，显式传入的过去时间会被保留
		require.ErrorIs(t, p.AssertRestorableViaAPI("expired-ctrl"), gatewayerrors.ErrNotFound)
	})
}

// TestStore_SetsOrigin 两个写入入口必须落到不同的来源标记上。
//
// 这一条防的是「入口调错了」：如果 /v1/privacy/redact 误用 Store（而不是 StoreControl），
// 它自己返回的 request_id 就还原不了——功能立刻坏掉，属于容易发现的错。反过来，
// 数据面误用 StoreControl 则没有任何症状，却在静默地把每条 LLM 请求的原文都变成
// 可经 API 还原的。两者都值得一条断言。
func TestStore_SetsOrigin(t *testing.T) {
	v, err := vault.NewMemVault(time.Minute, []byte("pipeline-test-passphrase"))
	require.NoError(t, err)
	p := New(Config{Vault: v})
	entries := []types.MappingEntry{{Placeholder: "<<PHONE_1>>", Original: []byte("13800138000")}}

	require.NoError(t, p.Store("d1", "", entries))
	require.NoError(t, p.StoreControl("c1", "", entries))

	d, err := v.Get("d1")
	require.NoError(t, err)
	require.Equal(t, types.OriginData, d.Origin)
	require.False(t, d.IsRestorableViaAPI())

	c, err := v.Get("c1")
	require.NoError(t, err)
	require.Equal(t, types.OriginControl, c.Origin)
	require.True(t, c.IsRestorableViaAPI())

	// 空 entries 不建表（保持原语义：没脱敏就没有可还原的东西）
	require.NoError(t, p.Store("empty", "", nil))
	_, err = v.Get("empty")
	require.Error(t, err)
}
