package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

// TestSimulatableTypes 面板下拉用的类型清单：非空、无重复、与校验集合一致、返回副本。
func TestSimulatableTypes(t *testing.T) {
	got := SimulatableTypes()
	require.NotEmpty(t, got)
	seen := map[string]bool{}
	for _, tp := range got {
		require.False(t, seen[tp], "重复类型 %s", tp)
		seen[tp] = true
		require.True(t, simulatableSet[tp], "%s 应在校验集合内", tp)
		require.NoError(t, ValidateSimulateDictionary(map[string]map[string]string{
			tp: {"真实值X": "假值Y"},
		}), "%s 应可通过校验", tp)
	}
	require.Len(t, seen, len(simulatableSet), "Types() 与校验集合必须一一对应")

	got[0] = "mutated"
	require.NotEqual(t, "mutated", SimulatableTypes()[0], "返回的必须是副本")
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
