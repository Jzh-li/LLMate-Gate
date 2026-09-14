package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAllTypes 权威类型表（契约 §6.1）：非空、无重复、与 IsKnownType 一致、返回副本。
//
// 面板下拉、登记表校验都吃这张表，它一旦漏掉某个类型，那个类型就会在面板里
// 消失、在登记时被判非法。
func TestAllTypes(t *testing.T) {
	all := AllTypes()
	require.NotEmpty(t, all)

	seen := map[string]bool{}
	for _, tp := range all {
		require.False(t, seen[tp], "重复类型 %s", tp)
		seen[tp] = true
		require.True(t, IsKnownType(tp), "%s 应被 IsKnownType 认可", tp)
		require.NotEmpty(t, tp)
	}

	require.False(t, IsKnownType(""), "空类型不合法")
	require.False(t, IsKnownType("zh_person"), "拼错/不存在的类型不该被放行")
	require.False(t, IsKnownType("zh_person_name "), "不做 trim，带空白即非法")

	all[0] = "mutated"
	require.NotEqual(t, "mutated", AllTypes()[0], "返回的必须是副本，调用方改不坏权威表")
}

// TestIrreversibleTypesInAllTypes 不可逆类型必须都在权威表内，否则登记/词典校验
// 与替换阶段对同一类型的判断会不一致。
func TestIrreversibleTypesInAllTypes(t *testing.T) {
	for _, tp := range IrreversibleTypes() {
		require.True(t, IsKnownType(tp), "%s 不在权威表内", tp)
		require.True(t, IsIrreversible(tp))
	}
	require.False(t, IsIrreversible(EntityPersonName))
}
