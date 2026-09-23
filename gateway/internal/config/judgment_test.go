package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gateway/pkg/types"
)

// 默认配置（Judgment.Enabled=false、无后端）必须能过校验：
// 否则每一个没写 judgment 块的老配置都会启动失败。
func TestJudgmentValidate_DefaultConfigPasses(t *testing.T) {
	c := Default()
	require.False(t, c.Judgment.Enabled)
	require.NoError(t, c.Validate())
}

func TestJudgmentValidate_NormalizesEmptyFields(t *testing.T) {
	j := JudgmentConfig{Enabled: true, Backends: []JudgmentBackendConfig{{Name: "r", Kind: "rules"}}}
	require.NoError(t, j.Validate())
	assert.Equal(t, "shadow", j.Mode, "空 mode 必须规范化为 shadow（enforce 需显式声明）")
	assert.Equal(t, DefaultJudgmentTimeout, j.Timeout)
	assert.Equal(t, string(types.SchemaPromptOnly), j.Backends[0].SchemaMode,
		"空 schema_mode 必须规范化为 prompt_only（不假定用户的服务受约束）")
}

func TestJudgmentValidate_EnabledWithoutBackends(t *testing.T) {
	j := JudgmentConfig{Enabled: true, Mode: "shadow", Timeout: time.Second}
	err := j.Validate()
	require.Error(t, err, "enabled 但没有后端 = 空转配置，必须拦下")
	assert.Contains(t, err.Error(), "backends is empty")
}

func TestJudgmentValidate_ModeClosedSet(t *testing.T) {
	j := JudgmentConfig{Mode: "monitor"}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "judgment.mode")
}

func TestJudgmentValidate_TimeoutBounds(t *testing.T) {
	j := JudgmentConfig{Timeout: 5 * time.Second}
	require.Error(t, j.Validate(), "超时上限必须存在：判断层挂在热路径上")

	j = JudgmentConfig{Timeout: -time.Millisecond}
	require.Error(t, j.Validate())

	j = JudgmentConfig{Timeout: MaxJudgmentTimeout}
	require.NoError(t, j.Validate())
}

// 这是本文件最要紧的一条：判断层是「用来防数据外泄的组件」，
// 它自己把数据发到公网是自相矛盾的，配置期就必须拒绝。
func TestJudgmentValidate_RejectsNonLocalBaseURL(t *testing.T) {
	cases := []string{
		"https://api.openai.com/v1",
		"http://8.8.8.8:11434/v1",
		"https://judge.example.com",
		"http://203.0.113.7:8000",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			j := JudgmentConfig{Enabled: true, Backends: []JudgmentBackendConfig{
				{Name: "m", Kind: "openai", BaseURL: raw, Model: "qwen2.5"},
			}}
			err := j.Validate()
			require.Errorf(t, err, "公网地址 %s 必须被拒", raw)
			assert.Contains(t, err.Error(), "judgment.backends[0](m).base_url")
		})
	}
}

func TestJudgmentValidate_AcceptsLocalEndpoints(t *testing.T) {
	cases := []string{
		"http://127.0.0.1:11434/v1",
		"http://localhost:8080/v1",
		"http://[::1]:11434/v1",
		"http://192.168.1.5:11434/v1",
		"http://10.0.0.7:8000",
		"http://172.16.3.9:1234",
		"http://0.0.0.0:11434/v1",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			j := JudgmentConfig{Enabled: true, Backends: []JudgmentBackendConfig{
				{Name: "m", Kind: "openai", BaseURL: raw, Model: "qwen2.5"},
			}}
			require.NoError(t, j.Validate())
		})
	}
}

func TestJudgmentValidate_RejectsNonHTTPScheme(t *testing.T) {
	j := JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "m", Kind: "openai", BaseURL: "ftp://127.0.0.1/v1", Model: "x"},
	}}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scheme must be http or https")
}

func TestJudgmentValidate_RulesRejectsConnectionFields(t *testing.T) {
	// kind=rules 是进程内的，给了 base_url 说明用户误解了语义。
	// 静默忽略会让人以为「我配了个远端规则服务」，所以直接报错。
	j := JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "r", Kind: "rules", BaseURL: "http://127.0.0.1:9000"},
	}}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind=rules has no base_url")

	j = JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "r", Kind: "rules", Model: "gpt"},
	}}
	err = j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind=rules has no model")
}

func TestJudgmentValidate_OpenAIRequiresModel(t *testing.T) {
	j := JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "m", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1"},
	}}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model is required")
}

func TestJudgmentValidate_UnknownKindAndSchemaMode(t *testing.T) {
	j := JudgmentConfig{Backends: []JudgmentBackendConfig{{Name: "x", Kind: "laya"}}}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind is required and must be rules|openai|http")

	j = JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "m", Kind: "openai", BaseURL: "http://127.0.0.1:1", Model: "x", SchemaMode: "wild"},
	}}
	err = j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_mode invalid")
}

func TestJudgmentValidate_DuplicateBackendName(t *testing.T) {
	j := JudgmentConfig{Backends: []JudgmentBackendConfig{
		{Name: "same", Kind: "rules"},
		{Name: "same", Kind: "rules"},
	}}
	err := j.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate backend name")
}

func TestJudgmentValidate_Thresholds(t *testing.T) {
	j := JudgmentConfig{
		Backends:   []JudgmentBackendConfig{{Name: "r", Kind: "rules"}},
		Thresholds: &types.JudgmentThresholds{Block: 0.5, Review: 0.6, Redact: 0.1},
	}
	err := j.Validate()
	require.Error(t, err, "全局阈值必须严格递减")
	assert.Contains(t, err.Error(), "judgment.thresholds")

	j.Thresholds = &types.JudgmentThresholds{Block: 0.9, Review: 0.6, Redact: 0.1}
	// per-backend 阈值用一组非递减的值（review > block），必须同样被拦下。
	j.Backends[0].Thresholds = &types.JudgmentThresholds{Block: 0.6, Review: 0.7, Redact: 0.1}
	err = j.Validate()
	require.Error(t, err, "per-backend 阈值同样要校验")
	assert.Contains(t, err.Error(), "judgment.backends[0](r).thresholds")
}

func TestJudgmentValidate_BackendTimeoutInheritsGlobal(t *testing.T) {
	j := JudgmentConfig{
		Timeout:  400 * time.Millisecond,
		Backends: []JudgmentBackendConfig{{Name: "r", Kind: "rules"}},
	}
	require.NoError(t, j.Validate())
	assert.Equal(t, 400*time.Millisecond, j.Backends[0].Timeout,
		"后端未声明超时时应继承全局值，而不是留 0（0 在下游意味着「无期限」，与 fail-safe 相反）")

	j.Backends[0].Timeout = 2 * time.Second
	require.Error(t, j.Validate())
}

func TestJudgmentValidate_TooManyBackends(t *testing.T) {
	j := JudgmentConfig{}
	for i := 0; i < MaxJudgmentBackends+1; i++ {
		j.Backends = append(j.Backends, JudgmentBackendConfig{Name: string(rune('a' + i)), Kind: "rules"})
	}
	require.Error(t, j.Validate())
}

func TestJudgmentBackend_EffectiveThresholds(t *testing.T) {
	global := &types.JudgmentThresholds{Block: 0.9, Review: 0.6, Redact: 0.2}
	own := &types.JudgmentThresholds{Block: 0.95, Review: 0.7, Redact: 0.3}

	assert.Equal(t, *own, (&JudgmentBackendConfig{Thresholds: own}).EffectiveThresholds(global))
	assert.Equal(t, *global, (&JudgmentBackendConfig{}).EffectiveThresholds(global))
	assert.Equal(t, types.DefaultThresholds(), (&JudgmentBackendConfig{}).EffectiveThresholds(nil))
}

func TestJudgmentConfig_LoadedFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// 最小可用配置 + judgment 块。gateway 段落给出，避免其它校验项干扰。
	require.NoError(t, os.WriteFile(path, []byte(`
gateway:
  upstream: "https://api.openai.com"
  auth_token: "t"
judgment:
  enabled: true
  mode: shadow
  timeout: 250ms
  backends:
    - name: local
      kind: openai
      base_url: "http://127.0.0.1:11434/v1"
      model: "qwen2.5:7b-instruct"
      schema_mode: json_schema
      timeout: 200ms
    - name: rules
      kind: rules
  thresholds:
    block: 0.85
    review: 0.55
    redact: 0.30
  points:
    tool_params: true
  whitelist:
    hosts: ["127.0.0.1", "localhost"]
`), 0o600))

	c, err := Load(path)
	require.NoError(t, err)
	require.True(t, c.Judgment.Enabled)
	assert.Equal(t, "shadow", c.Judgment.Mode)
	assert.Equal(t, 250*time.Millisecond, c.Judgment.Timeout)
	require.Len(t, c.Judgment.Backends, 2)
	assert.Equal(t, "local", c.Judgment.Backends[0].Name)
	assert.Equal(t, "json_schema", c.Judgment.Backends[0].SchemaMode)
	assert.Equal(t, 200*time.Millisecond, c.Judgment.Backends[0].Timeout)
	// rules 后端没写 timeout → 继承全局 250ms。
	assert.Equal(t, 250*time.Millisecond, c.Judgment.Backends[1].Timeout)
	assert.Equal(t, string(types.SchemaPromptOnly), c.Judgment.Backends[1].SchemaMode)
	assert.True(t, c.Judgment.Points.ToolParams)
	assert.False(t, c.Judgment.Points.Endpoint)
	assert.Equal(t, []string{"127.0.0.1", "localhost"}, c.Judgment.Whitelist.Hosts)
}

// 未知键必须在 judgment 段同样报错（KnownFields 严格模式对新增结构也生效）。
func TestJudgmentConfig_UnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
gateway:
  upstream: "https://api.openai.com"
  auth_token: "t"
judgment:
  enabled: true
  backend: []
`), 0o600))
	_, err := Load(path)
	require.Error(t, err, "拼错的键名必须报错，不能静默丢弃")
	assert.Contains(t, err.Error(), "not found in type config.JudgmentConfig")
	assert.NotContains(t, err.Error(), "enabled: true", "错误消息里不应回显配置值")
}

func TestValidateLocalEndpoint_Direct(t *testing.T) {
	require.NoError(t, validateLocalEndpoint("http://127.0.0.1:11434/v1"))
	require.NoError(t, validateLocalEndpoint("http://localhost/v1"))
	require.NoError(t, validateLocalEndpoint("http://[fd00::1]:8080/v1"))

	require.Error(t, validateLocalEndpoint("http://1.1.1.1"))
	require.Error(t, validateLocalEndpoint("http://judge.internal:8080"))
	require.Error(t, validateLocalEndpoint("://nope"))
	require.Error(t, validateLocalEndpoint("http://"))
}
