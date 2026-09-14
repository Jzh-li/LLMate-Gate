package simulator

import (
	"testing"

	"gateway/pkg/cn"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// TestExpandIdentityCard_StableAndFormatPreserving 身份卡生成的仿真值格式保持且跨调用稳定。
func TestExpandIdentityCard_StableAndFormatPreserving(t *testing.T) {
	card := map[string]string{
		types.EntityPersonName: "张三",
		types.EntityPhone:      "13800138000",
	}
	first, err := ExpandIdentityCard(card)
	require.NoError(t, err)
	require.NotEmpty(t, first[types.EntityPersonName]["张三"])

	// 姓名：非空且不等于原值
	name := first[types.EntityPersonName]["张三"]
	require.NotEmpty(t, name)
	require.NotEqual(t, "张三", name)

	// 手机号：11 位、号段合法
	phone := first[types.EntityPhone]["13800138000"]
	require.Len(t, phone, 11)
	require.Contains(t, cn.PhonePrefixes(), phone[:3])

	// 跨调用稳定：同一真实值恒映射到同一仿真值
	second, err := ExpandIdentityCard(card)
	require.NoError(t, err)
	require.Equal(t, name, second[types.EntityPersonName]["张三"])
	require.Equal(t, phone, second[types.EntityPhone]["13800138000"])
}

// TestExpandIdentityCard_RejectsNonSimulatable 不可仿真/不可逆类型与空值被拒绝。
func TestExpandIdentityCard_RejectsNonSimulatable(t *testing.T) {
	_, err := ExpandIdentityCard(map[string]string{types.EntityAPIKey: "sk-secret"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not simulatable")

	_, err = ExpandIdentityCard(map[string]string{types.EntityPersonName: "   "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty value")
}

// TestExpandIdentityCard_Empty 空身份卡返回空结果，不报错。
func TestExpandIdentityCard_Empty(t *testing.T) {
	out, err := ExpandIdentityCard(nil)
	require.NoError(t, err)
	require.Nil(t, out)
}
