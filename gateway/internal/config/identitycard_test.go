package config

import (
	"testing"

	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// TestConfig_IdentityCardExpands 身份卡在加载期展开进词典，且仿真值格式保持、非空。
func TestConfig_IdentityCardExpands(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
replacement:
  strategy: "simulate"
  simulate_zh:
    identity_card:
      zh_person_name: "张三"
`)
	c, err := Load(p)
	require.NoError(t, err)

	fake := c.Replacement.SimulateZH.Dictionary[types.EntityPersonName]["张三"]
	require.NotEmpty(t, fake)
	require.NotEqual(t, "张三", fake)
}

// TestConfig_IdentityCardExplicitDictWins 同一 (类型, 真实值) 两边都写时，手写词典优先。
func TestConfig_IdentityCardExplicitDictWins(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
replacement:
  strategy: "simulate"
  simulate_zh:
    identity_card:
      zh_person_name: "张三"
    dictionary:
      zh_person_name:
        "张三": "王晓明"
`)
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, "王晓明", c.Replacement.SimulateZH.Dictionary[types.EntityPersonName]["张三"],
		"身份卡不得覆盖用户显式给出的仿真值")
}

// TestConfig_IdentityCardInvalidType 身份卡声明不可仿真类型 → 加载期拒绝。
func TestConfig_IdentityCardInvalidType(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
replacement:
  strategy: "simulate"
  simulate_zh:
    identity_card:
      api_key: "sk-secret"
`)
	_, err := Load(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not simulatable")
}
