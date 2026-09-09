// Package config 负责配置加载、${ENV} 展开与启动期一次性校验（契约 §3）。
//
// 校验失败一律退出（exit code 2），绝不进入降级服务状态。
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"

	"gopkg.in/yaml.v3"
)

// Config 是 config.yaml 的完整结构（契约 §3.1）。
type Config struct {
	Gateway     GatewayConfig     `yaml:"gateway"`
	Detection   DetectionConfig   `yaml:"detection"`
	Replacement ReplacementConfig `yaml:"replacement"`
	Policy      PolicyConfig      `yaml:"policy"`
	Vault       VaultConfig       `yaml:"vault"`
	Audit       AuditConfig       `yaml:"audit"`
}

// GatewayConfig 网关监听与上游配置。
type GatewayConfig struct {
	Listen         string        `yaml:"listen"`
	Upstream       string        `yaml:"upstream"`
	AuthToken      string        `yaml:"auth_token"`
	UpstreamAPIKey string        `yaml:"upstream_api_key"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	Debug          bool          `yaml:"debug"`
	DebugBind      string        `yaml:"debug_bind"`
	LogLevel       string        `yaml:"log_level"`
}

// DetectionConfig 检测引擎配置。
type DetectionConfig struct {
	Engine     string             `yaml:"engine"`
	Sidecar    SidecarConfig      `yaml:"sidecar"`
	Thresholds map[string]float64 `yaml:"thresholds"`
	Cache      CacheConfig        `yaml:"cache"`
	// FallbackRegex 为格式固定的实体提供正则加速通道（不经过模型）。
	FallbackRegex bool `yaml:"fallback_regex"`
}

// SidecarConfig PII Engineer sidecar 进程管理配置。
type SidecarConfig struct {
	Command      string        `yaml:"command"`
	Endpoint     string        `yaml:"endpoint"`
	Healthz      string        `yaml:"healthz"`
	StartTimeout time.Duration `yaml:"start_timeout"`
	RestartLimit int           `yaml:"restart_limit"`
	// AutoStart 为 false 时不拉起子进程，仅连接既有 endpoint（Docker/手动部署场景）。
	AutoStart bool `yaml:"auto_start"`
}

// CacheConfig detection_cache 配置（契约 §8.2）。
type CacheConfig struct {
	Enabled          bool          `yaml:"enabled"`
	BindConversation bool          `yaml:"bind_conversation"`
	TTL              time.Duration `yaml:"ttl"`
	MaxEntries       int           `yaml:"max_entries"`
}

// ReplacementConfig 替换策略配置。
type ReplacementConfig struct {
	Strategy    string            `yaml:"strategy"`
	SimulateZH  SimulateZHConfig  `yaml:"simulate_zh"`
	Irreversible []string         `yaml:"irreversible"`
	// PerTypeFate 逐类型命运覆盖：entity_type -> reversible|mask|redact（Phase 2 阶段 2）。
	PerTypeFate map[string]string `yaml:"per_type_fate"`
}

// SimulateZHConfig 中文仿真替换开关（v1.1）。
type SimulateZHConfig struct {
	PersonName bool `yaml:"person_name"`
	Phone      bool `yaml:"phone"`
	IDCard     bool `yaml:"id_card"`
	BankCard   bool `yaml:"bank_card"`
}

// PolicyConfig 安全策略。
type PolicyConfig struct {
	FailClosed    bool `yaml:"fail_closed"`
	ToolCallScan  bool `yaml:"tool_call_scan"`
	StreamRestore bool `yaml:"stream_restore"`
}

// VaultConfig 映射表加密存储配置。
type VaultConfig struct {
	Path          string        `yaml:"path"`
	Encryption    string        `yaml:"encryption"`
	KeyDerivation string        `yaml:"key_derivation"`
	RequestTTL    time.Duration `yaml:"request_ttl"`
	Persist       bool          `yaml:"persist"`
}

// AuditConfig 审计日志配置（契约 §9.2）。
type AuditConfig struct {
	Enabled bool     `yaml:"enabled"`
	Path    string   `yaml:"path"`
	Export  []string `yaml:"export"`
	LogPII  bool     `yaml:"log_pii"`
}

// Default 返回默认配置，保证零配置文件也能启动。
func Default() *Config {
	return &Config{
		Gateway: GatewayConfig{
			Listen:         ":8400",
			Upstream:       "https://api.openai.com",
			RequestTimeout: 30 * time.Second,
			Debug:          true,
			DebugBind:      "127.0.0.1",
			LogLevel:       "info",
		},
		Detection: DetectionConfig{
			Engine: "regex",
			Sidecar: SidecarConfig{
				Endpoint:     "http://127.0.0.1:8000",
				Healthz:      "/healthz",
				StartTimeout: 60 * time.Second,
				RestartLimit: 3,
				AutoStart:    false,
			},
			Thresholds: map[string]float64{
				"zh_person_name": 0.5,
				"zh_phone":       0.8,
				"zh_id_card":     0.9,
				"zh_bank_card":   0.9,
				"zh_address":     0.6,
				"email":          0.7,
			},
			Cache: CacheConfig{
				Enabled:          true,
				BindConversation: true,
				TTL:              30 * time.Minute,
				MaxEntries:       10000,
			},
			FallbackRegex: true,
		},
		Replacement: ReplacementConfig{
			Strategy:     "placeholder",
			SimulateZH:   SimulateZHConfig{PersonName: true, Phone: true, IDCard: true, BankCard: true},
			Irreversible: []string{"api_key", "password", "token"},
		},
		Policy: PolicyConfig{FailClosed: true, ToolCallScan: true, StreamRestore: true},
		Vault: VaultConfig{
			Path:          "./vault_data",
			Encryption:    "aes-256-gcm",
			KeyDerivation: "scrypt",
			RequestTTL:    30 * time.Minute,
			Persist:       false,
		},
		Audit: AuditConfig{Enabled: true, Path: "./audit.log", Export: []string{"pip", "gdpr"}, LogPII: false},
	}
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv 展开 ${VAR} 与 ${VAR:-default}。
func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(m string) string {
		groups := envPattern.FindStringSubmatch(m)
		name := groups[1]
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		if len(groups) > 2 && groups[2] != "" {
			return groups[2]
		}
		return ""
	})
}

// expandEnvDeep 对配置中所有字符串字段做 ${ENV} 展开。
func expandEnvDeep(c *Config) {
	c.Gateway.Listen = expandEnv(c.Gateway.Listen)
	c.Gateway.Upstream = expandEnv(c.Gateway.Upstream)
	c.Gateway.AuthToken = expandEnv(c.Gateway.AuthToken)
	c.Gateway.UpstreamAPIKey = expandEnv(c.Gateway.UpstreamAPIKey)
	c.Gateway.DebugBind = expandEnv(c.Gateway.DebugBind)
	c.Vault.Path = expandEnv(c.Vault.Path)
	c.Audit.Path = expandEnv(c.Audit.Path)
	c.Detection.Sidecar.Endpoint = expandEnv(c.Detection.Sidecar.Endpoint)
	c.Detection.Sidecar.Command = expandEnv(c.Detection.Sidecar.Command)
}

// Load 从 YAML 文件加载配置；path 为空时返回默认配置。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "read config file", err)
		}
		// 先展开再解析，保证 ${ENV} 在 YAML 语义之前生效。
		if err := yaml.Unmarshal([]byte(expandEnv(string(raw))), c); err != nil {
			return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "parse config yaml", err)
		}
	}
	expandEnvDeep(c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate 启动期一次性校验（契约 §3.3）。失败即退出，不做降级。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Gateway.Upstream) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "upstream is required")
	}
	if !strings.HasPrefix(c.Gateway.Listen, ":") {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "gateway.listen must start with ':' (e.g. :8400)")
	}
	if c.Gateway.RequestTimeout <= 0 {
		c.Gateway.RequestTimeout = 30 * time.Second
	}
	switch c.Replacement.Strategy {
	case "placeholder", "simulate":
	default:
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "unknown replacement.strategy: %q", c.Replacement.Strategy)
	}
	switch c.Detection.Engine {
	case "pii-engineer", "regex":
	default:
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "unknown detection.engine: %q", c.Detection.Engine)
	}
	if c.Detection.Cache.TTL > 0 && c.Detection.Cache.TTL < time.Minute {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "detection.cache.ttl must be >= 1m")
	}
	if c.Vault.RequestTTL < 5*time.Minute {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "vault.request_ttl must be >= 5m")
	}
	if c.Vault.Encryption != "" && c.Vault.Encryption != "aes-256-gcm" {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "unsupported vault.encryption: %q", c.Vault.Encryption)
	}
	for k, v := range c.Detection.Thresholds {
		if v < 0 || v > 1 {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "threshold %s out of range [0,1]: %v", k, v)
		}
	}
	for t, f := range c.Replacement.PerTypeFate {
		switch f {
		case "reversible", "mask", "redact":
		default:
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "replacement.per_type_fate[%s] invalid fate %q (want reversible|mask|redact)", t, f)
		}
	}
	if c.Detection.Cache.MaxEntries <= 0 {
		c.Detection.Cache.MaxEntries = 10000
	}
	return nil
}

// ThresholdFor 返回实体类型的置信度阈值，未配置时使用保守默认 0.5。
func (c *Config) ThresholdFor(entityType string) float64 {
	if v, ok := c.Detection.Thresholds[entityType]; ok {
		return v
	}
	return 0.5
}

// String 打印脱敏后的配置摘要（绝不打印 token / key）。
func (c *Config) String() string {
	return fmt.Sprintf("listen=%s upstream=%s engine=%s strategy=%s fail_closed=%v debug=%v",
		c.Gateway.Listen, c.Gateway.Upstream, c.Detection.Engine, c.Replacement.Strategy,
		c.Policy.FailClosed, c.Gateway.Debug)
}
