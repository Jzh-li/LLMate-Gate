package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// TestConfig_Valid 合法 YAML 加载成功（测试规约 §1.3 config）。
func TestConfig_Valid(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
  request_timeout: "30s"
detection:
  engine: "regex"
  cache:
    enabled: true
    ttl: "30m"
replacement:
  strategy: "placeholder"
`)
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, ":8400", c.Gateway.Listen)
	require.Equal(t, "https://api.openai.com", c.Gateway.Upstream)
	require.Equal(t, 30*time.Second, c.Gateway.RequestTimeout)
	require.True(t, c.Detection.Cache.Enabled)
}

// TestConfig_Default 无配置文件时使用默认配置（零配置启动）。
func TestConfig_Default(t *testing.T) {
	c, err := Load("")
	require.NoError(t, err)
	require.Equal(t, ":8400", c.Gateway.Listen)
	require.True(t, c.Policy.FailClosed, "fail-closed 必须默认开启")
	require.True(t, c.Detection.Cache.BindConversation, "缓存键必须绑定 conversation")
	require.False(t, c.Audit.LogPII, "审计默认不含原文")
}

// TestConfig_MissingUpstream 必填缺失 → invalid_config（测试规约 §1.3）。
func TestConfig_MissingUpstream(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: ""
`)
	_, err := Load(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream is required")
}

// TestConfig_EnumInvalid 非法 strategy → 校验错误（测试规约 §1.3）。
func TestConfig_EnumInvalid(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
replacement:
  strategy: "fake"
`)
	_, err := Load(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown replacement.strategy")
}

// TestConfig_ListenFormat listen 必须以冒号开头。
func TestConfig_ListenFormat(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: "8400"
  upstream: "https://api.openai.com"
`)
	_, err := Load(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must start with ':'")
}

// TestConfig_EnvExpansion ${ENV} 与 ${ENV:-default} 展开。
func TestConfig_EnvExpansion(t *testing.T) {
	t.Setenv("LMGATE_TEST_TOKEN", "tok-abc")
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
  auth_token: "${LMGATE_TEST_TOKEN}"
  upstream_api_key: "${LMGATE_TEST_MISSING:-fallback-key}"
`)
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, "tok-abc", c.Gateway.AuthToken)
	require.Equal(t, "fallback-key", c.Gateway.UpstreamAPIKey)
}

// TestConfig_ThresholdFor 未配置类型回落到保守默认阈值。
func TestConfig_ThresholdFor(t *testing.T) {
	c := Default()
	require.InDelta(t, 0.8, c.ThresholdFor("zh_phone"), 1e-9)
	require.InDelta(t, 0.5, c.ThresholdFor("unknown_type"), 1e-9)
}

// TestConfig_StringNeverLeaksToken 配置摘要不得泄露令牌。
func TestConfig_StringNeverLeaksToken(t *testing.T) {
	c := Default()
	c.Gateway.AuthToken = "super-secret"
	require.NotContains(t, c.String(), "super-secret")
}
