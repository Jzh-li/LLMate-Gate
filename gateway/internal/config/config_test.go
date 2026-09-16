package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gateway/internal/simulator"
	"gateway/pkg/types"

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

// ---------- 仿真词典 ----------

// TestValidateSimulateDictionary 词典校验规则（配置加载期与面板热加载共用）。
func TestValidateSimulateDictionary(t *testing.T) {
	t.Run("合法", func(t *testing.T) {
		require.NoError(t, ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityPersonName: {"张三": "王五", "李四": "赵六"},
			types.EntityAddress:    {"北京市朝阳区": "上海市浦东新区"},
		}))
	})
	t.Run("空词典", func(t *testing.T) {
		require.NoError(t, ValidateSimulateDictionary(nil))
		require.NoError(t, ValidateSimulateDictionary(map[string]map[string]string{}))
	})
	t.Run("未知类型", func(t *testing.T) {
		err := ValidateSimulateDictionary(map[string]map[string]string{
			"zh_person": {"张三": "王五"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-simulatable")
	})
	t.Run("不可逆类型被拒", func(t *testing.T) {
		// api_key 走 redact、不参与仿真；列进词典会让用户误以为能控制它的假值
		err := ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityAPIKey: {"sk-real": "sk-fake"},
		})
		require.Error(t, err)
	})
	t.Run("同类型仿真值重复", func(t *testing.T) {
		err := ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityPersonName: {"张三": "王五", "李四": "王五"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "restore would break")
	})
	t.Run("跨类型仿真值重复同样被拒", func(t *testing.T) {
		// 还原表是平表、不分类型分桶，跨类型撞车一样会互相覆盖
		err := ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityPersonName: {"张三": "王五"},
			types.EntityAddress:    {"北京市": "王五"},
		})
		require.Error(t, err)
	})
	t.Run("空真实值", func(t *testing.T) {
		require.Error(t, ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityPersonName: {"   ": "王五"},
		}))
	})
	t.Run("空仿真值", func(t *testing.T) {
		require.Error(t, ValidateSimulateDictionary(map[string]map[string]string{
			types.EntityPersonName: {"张三": "  "},
		}))
	})
}

// TestSimulateDictionary_LoadedFromYAML 词典能从配置读进来。
func TestSimulateDictionary_LoadedFromYAML(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "http://127.0.0.1:15721"
replacement:
  strategy: "simulate"
  simulate_zh:
    dictionary:
      zh_person_name:
        "张三": "王五"
      zh_address:
        "北京市朝阳区": "上海市浦东新区"
`)
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, "王五", c.Replacement.SimulateZH.Dictionary["zh_person_name"]["张三"])
	require.Equal(t, "上海市浦东新区", c.Replacement.SimulateZH.Dictionary["zh_address"]["北京市朝阳区"])
}

// TestSimulateDictionary_BadYAMLFailsLoad 非法词典必须在加载期拦下，不能带病启动。
func TestSimulateDictionary_BadYAMLFailsLoad(t *testing.T) {
	p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "http://127.0.0.1:15721"
replacement:
  strategy: "simulate"
  simulate_zh:
    dictionary:
      zh_person_name:
        "张三": "王五"
        "李四": "王五"
`)
	_, err := Load(p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "restore would break")
}

// TestSimulatableTypes 面板下拉用的类型清单：非空、无重复、与校验口径一致、返回副本。
//
// 注意断言方向：清单的权威来源是 simulator 的能力登记表，config 只是转发。
// 因此这里检验的是「清单里的每一项都能通过词典校验」（正向），以及「表外类型被
// 校验拒绝」（反向）——任何一头断裂都说明清单与真实仿真能力脱节了。
func TestSimulatableTypes(t *testing.T) {
	got := SimulatableTypes()
	require.NotEmpty(t, got)
	seen := map[string]bool{}
	for _, tp := range got {
		require.False(t, seen[tp], "重复类型 %s", tp)
		seen[tp] = true
		require.NoError(t, ValidateSimulateDictionary(map[string]map[string]string{
			tp: {"真实值X": "假值Y"},
		}), "%s 应可通过校验", tp)
	}

	// 反向：不在清单里的类型必须被拒。zh_plate 有实体类型但无仿真实现。
	require.Error(t, ValidateSimulateDictionary(map[string]map[string]string{
		"zh_plate": {"京A12345": "沪B67890"},
	}), "表外类型必须被校验拒绝")

	// 清单必须与 simulator 的能力登记表逐项对齐（防止有人只改一边）。
	require.ElementsMatch(t, simulator.SimulatableTypes(), got)

	got[0] = "mutated"
	require.NotEqual(t, "mutated", SimulatableTypes()[0], "返回的必须是副本")
	require.NotEqual(t, "mutated", simulator.SimulatableTypes()[0], "simulator 侧同样必须是副本")
}

// ---------- 鉴权令牌解析（Specs/07 阶段 A）----------
//
// 这一组测试钉住的核心不变量：**「没配 auth_token」不再等于「不鉴权」**。
// 它必须等于「自动生成一个稳定令牌」——稳定是关键，每次重启换令牌会把用户
// 推回「干脆关掉鉴权」，那比原来更不安全。

// TestResolveAuthToken_Configured 显式配置优先，且不落盘任何令牌文件。
func TestResolveAuthToken_Configured(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Gateway.AuthToken = "explicit-token"
	c.Gateway.AuthTokenFile = filepath.Join(dir, "auth_token")

	tok, generated, err := c.ResolveAuthToken()
	require.NoError(t, err)
	require.Equal(t, "explicit-token", tok)
	require.False(t, generated)
	_, statErr := os.Stat(c.Gateway.AuthTokenFile)
	require.True(t, os.IsNotExist(statErr), "显式配置令牌时不该再落盘文件")
}

// TestResolveAuthToken_GeneratesOnceAndStaysStable 未配置时生成一次、落盘 0600、重启复用。
func TestResolveAuthToken_GeneratesOnceAndStaysStable(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Gateway.AuthTokenFile = filepath.Join(dir, "auth_token")

	tok1, generated, err := c.ResolveAuthToken()
	require.NoError(t, err)
	require.True(t, generated, "首次调用应报告「本次新生成」，调用方据此决定是否打印令牌")
	require.Len(t, tok1, 64, "32 字节随机 → hex 64 字符")
	require.False(t, c.Gateway.AllowUnauthenticated)

	fi, err := os.Stat(c.Gateway.AuthTokenFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "令牌等价于访问凭据，必须 0600")

	// 模拟重启：新的 Config 对象读同一个文件
	c2 := Default()
	c2.Gateway.AuthTokenFile = c.Gateway.AuthTokenFile
	tok2, generated2, err := c2.ResolveAuthToken()
	require.NoError(t, err)
	require.False(t, generated2)
	require.Equal(t, tok1, tok2, "重启后令牌必须不变，否则客户端每次都要改配置")
}

// TestResolveAuthToken_FillsEmptyFile 内容为空的令牌文件按「尚未生成」处理。
func TestResolveAuthToken_FillsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth_token")
	require.NoError(t, os.WriteFile(p, []byte("\n"), 0o600))

	c := Default()
	c.Gateway.AuthTokenFile = p
	tok, generated, err := c.ResolveAuthToken()
	require.NoError(t, err)
	require.True(t, generated)
	require.NotEmpty(t, tok)
}

// TestResolveAuthToken_ExplicitlyDisabled 显式声明免鉴权时不生成令牌。
func TestResolveAuthToken_ExplicitlyDisabled(t *testing.T) {
	c := Default()
	c.Gateway.AllowUnauthenticated = true
	tok, generated, err := c.ResolveAuthToken()
	require.NoError(t, err)
	require.Empty(t, tok)
	require.False(t, generated)
}

// TestValidate_AuthConfigConflicts 鉴权相关的自相矛盾配置必须在启动期拦下。
func TestValidate_AuthConfigConflicts(t *testing.T) {
	t.Run("同时给 token 与 allow_unauthenticated", func(t *testing.T) {
		// 用户很可能以为这是「本地免密、外部要鉴权」，实际语义只能二选一。
		// 与其让它静默按某一种解释生效，不如拒绝启动。
		c := Default()
		c.Gateway.AuthToken = "t"
		c.Gateway.AllowUnauthenticated = true
		err := c.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "allow_unauthenticated must not be true")
	})
	t.Run("未配 token 又没给落盘路径", func(t *testing.T) {
		c := Default()
		c.Gateway.AuthTokenFile = ""
		err := c.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "auth_token_file is required")
	})
	t.Run("显式免鉴权合法", func(t *testing.T) {
		c := Default()
		c.Gateway.AllowUnauthenticated = true
		require.NoError(t, c.Validate())
	})
	t.Run("默认配置合法且默认不是免鉴权", func(t *testing.T) {
		c := Default()
		require.NoError(t, c.Validate())
		require.False(t, c.Gateway.AllowUnauthenticated, "免鉴权必须是显式选择")
		require.Equal(t, "./auth_token", c.Gateway.AuthTokenFile)
	})
}

// TestValidate_RejectsVaultPersist persist 是「看起来能用、实际骗人」的开关，必须拒绝。
func TestValidate_RejectsVaultPersist(t *testing.T) {
	c := Default()
	c.Vault.Persist = true
	err := c.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "vault.persist is not supported")
}

// TestValidate_AuditRotation 审计轮转参数：负值拒绝，0 表示关闭轮转。
func TestValidate_AuditRotation(t *testing.T) {
	c := Default()
	c.Audit.MaxSizeMB = -1
	require.Error(t, c.Validate())

	c = Default()
	c.Audit.MaxSizeMB = 0
	require.NoError(t, c.Validate(), "0 表示关闭轮转，是合法取值")
	require.Equal(t, 100, Default().Audit.MaxSizeMB, "默认应开启轮转")
	require.Equal(t, 3, Default().Audit.MaxBackups)
}

// TestAuthState_NeverLeaksToken 配置摘要可以报鉴权状态，但绝不能带出令牌。
func TestAuthState_NeverLeaksToken(t *testing.T) {
	c := Default()
	c.Gateway.AuthToken = "super-secret-token-value"
	require.Contains(t, c.String(), "auth=configured")
	require.NotContains(t, c.String(), "super-secret-token-value")

	c = Default()
	c.Gateway.AllowUnauthenticated = true
	require.Contains(t, c.String(), "auth=none(explicit)")

	c = Default()
	require.Contains(t, c.String(), "auth=auto")
}

// TestExampleConfig_Loads 随仓库发布的示例配置必须能被加载。
//
// 示例配置是文档的一部分，最容易在加字段时忘了同步、或者写出 schema 里不存在的
// 键（yaml.v3 默认不报未知字段，写错了会静默失效，用户以为配上了其实没有）。
// 这条测试把示例配置钉在 CI 里，改 config 结构时它会先响。
func TestExampleConfig_Loads(t *testing.T) {
	p := filepath.Join("..", "..", "configs", "config.example.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("示例配置应当随仓库发布: %v", err)
	}
	c, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, ":8400", c.Gateway.Listen)
	require.Equal(t, "placeholder", c.Replacement.Strategy)
}

// ---------- 登记表 ----------

// TestRegistryConfig 登记表配置只有开关 + 路径，值留在独立文件里。
func TestRegistryConfig(t *testing.T) {
	t.Run("加载", func(t *testing.T) {
		p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "http://127.0.0.1:15721"
detection:
  registry:
    enabled: true
    path: "./registry.yaml"
`)
		c, err := Load(p)
		require.NoError(t, err)
		require.True(t, c.Detection.Registry.Enabled)
		require.Equal(t, "./registry.yaml", c.Detection.Registry.Path)
	})
	t.Run("启用但没给路径", func(t *testing.T) {
		// 面板里加的值若不落盘，重启就静默消失，而用户以为已经登记好了
		p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "http://127.0.0.1:15721"
detection:
  registry:
    enabled: true
`)
		_, err := Load(p)
		require.Error(t, err)
		require.Contains(t, err.Error(), "detection.registry.path is required")
	})
	t.Run("默认关闭且不要求路径", func(t *testing.T) {
		c, err := Load("")
		require.NoError(t, err)
		require.False(t, c.Detection.Registry.Enabled)
		require.Empty(t, c.Detection.Registry.Path)
	})
	t.Run("路径支持环境变量展开", func(t *testing.T) {
		t.Setenv("LMGATE_TEST_REGISTRY_PATH", "/tmp/lmgate-registry.yaml")
		p := writeConfig(t, `
gateway:
  listen: ":8400"
  upstream: "http://127.0.0.1:15721"
detection:
  registry:
    enabled: true
    path: "${LMGATE_TEST_REGISTRY_PATH}"
`)
		c, err := Load(p)
		require.NoError(t, err)
		require.Equal(t, "/tmp/lmgate-registry.yaml", c.Detection.Registry.Path)
	})
	t.Run("结构上不许放登记值", func(t *testing.T) {
		// 登记值是明文 PII，一旦进了 config 就会顺着配置摘要进日志、顺着
		// /metrics 与面板配置视图扩散出去。这条测试把这个不变量钉住：以后真要
		// 往这里加字段，必须先想清楚「它会不会带用户 PII」。
		typ := reflect.TypeOf(RegistryConfig{})
		require.Equal(t, 2, typ.NumField(), "RegistryConfig 只该有 开关 + 路径")
		require.Equal(t, "Enabled", typ.Field(0).Name)
		require.Equal(t, "Path", typ.Field(1).Name)
	})
}
