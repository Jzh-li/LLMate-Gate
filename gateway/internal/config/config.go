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
	"gateway/internal/simulator"
	"gateway/pkg/types"

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
	Listen         string           `yaml:"listen"`
	Upstream       string           `yaml:"upstream"`
	AuthToken      string           `yaml:"auth_token"`
	UpstreamAPIKey string           `yaml:"upstream_api_key"`
	Upstreams      []UpstreamConfig `yaml:"upstreams"`
	RequestTimeout time.Duration    `yaml:"request_timeout"`
	Debug          bool             `yaml:"debug"`
	LogLevel       string           `yaml:"log_level"`
}

// UpstreamConfig 按协议路由的上游（各家厂商适配，配置驱动而非写死代码）。
//
// 网关入口同时支持 OpenAI 兼容（/v1/chat/completions 等）与 Anthropic
// 兼容（/v1/messages）两类协议；不同厂商的这两种端点地址与鉴权方式不同
// （如 DeepSeek 的 Anthropic 端点是 https://api.deepseek.com/anthropic，
// 用 x-api-key + anthropic-version）。通过 upstreams 列表声明各协议的目标即可。
type UpstreamConfig struct {
	Protocol   string `yaml:"protocol"`    // openai | anthropic
	BaseURL    string `yaml:"base_url"`    // 该协议上游的 base URL
	APIKey     string `yaml:"api_key"`     // 上游鉴权 key（Anthropic → x-api-key，OpenAI → Bearer）
	APIVersion string `yaml:"api_version"` // Anthropic anthropic-version；默认 2023-06-01
	// PathPrefix 替换入口路径里的 /v1 段。多数厂商用 /v1；智谱 GLM 是 /v4，
	// 通义千问 DashScope 是 /compatible-mode/v1。留空则原样透传 /v1。
	PathPrefix string `yaml:"path_prefix"`
}

// DetectionConfig 检测引擎配置。
type DetectionConfig struct {
	Engine     string             `yaml:"engine"`
	Sidecar    SidecarConfig      `yaml:"sidecar"`
	Thresholds map[string]float64 `yaml:"thresholds"`
	Cache      CacheConfig        `yaml:"cache"`
	Registry   RegistryConfig     `yaml:"registry"`
	// FallbackRegex 为格式固定的实体提供正则加速通道（不经过模型）。
	FallbackRegex bool `yaml:"fallback_regex"`
}

// RegistryConfig 登记表配置（用户自报真实 PII 值的检测补召回层）。
//
// 这里只有「开关 + 文件路径」——登记值本身留在独立文件里。这样配置摘要、
// 审计事件、面板的配置视图统统碰不到明文 PII，只有加载登记表的那一处读文件。
// 文件是明文，权限收在 0600（见 registry.SaveFile）。
type RegistryConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
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
	// Dictionary 用户自定义词典：entity_type → (真实值 → 仿真值)。
	// 命中者用你的仿真值，未命中回落内置词表 + HMAC 派生。
	Dictionary map[string]map[string]string `yaml:"dictionary"`
	// IdentityCard 身份卡：entity_type → 真实值（每种类型一个）。加载期用固定密钥
	// 为其生成格式保持、跨重启稳定的仿真值，展开进 Dictionary（手写词典优先）。
	// 它是 dictionary 的糖，不引入第二条运行时路径。
	IdentityCard map[string]string `yaml:"identity_card"`
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
	for i := range c.Gateway.Upstreams {
		c.Gateway.Upstreams[i].BaseURL = expandEnv(c.Gateway.Upstreams[i].BaseURL)
		c.Gateway.Upstreams[i].APIKey = expandEnv(c.Gateway.Upstreams[i].APIKey)
		c.Gateway.Upstreams[i].APIVersion = expandEnv(c.Gateway.Upstreams[i].APIVersion)
		c.Gateway.Upstreams[i].PathPrefix = expandEnv(c.Gateway.Upstreams[i].PathPrefix)
	}
	c.Vault.Path = expandEnv(c.Vault.Path)
	c.Audit.Path = expandEnv(c.Audit.Path)
	c.Detection.Sidecar.Endpoint = expandEnv(c.Detection.Sidecar.Endpoint)
	c.Detection.Sidecar.Command = expandEnv(c.Detection.Sidecar.Command)
	c.Detection.Registry.Path = expandEnv(c.Detection.Registry.Path)
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
	if err := c.expandIdentityCard(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// expandIdentityCard 把 identity_card 展开进 Dictionary（身份卡 → 词典的糖）。
//
// 在 Validate 之前做：展开后的词典与手写词典一起过全局唯一校验，身份卡的仿真值与
// 手写仿真值撞车同样能在启动期被拦下。手写词典优先——同一 (类型, 真实值) 若两边都
// 写了，以手写为准（用户显式给出的仿真值不被身份卡覆盖）。
func (c *Config) expandIdentityCard() error {
	card := c.Replacement.SimulateZH.IdentityCard
	if len(card) == 0 {
		return nil
	}
	expanded, err := simulator.ExpandIdentityCard(card)
	if err != nil {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"expand replacement.simulate_zh.identity_card: %v", err)
	}
	if c.Replacement.SimulateZH.Dictionary == nil {
		c.Replacement.SimulateZH.Dictionary = make(map[string]map[string]string)
	}
	for entityType, pairs := range expanded {
		if c.Replacement.SimulateZH.Dictionary[entityType] == nil {
			c.Replacement.SimulateZH.Dictionary[entityType] = make(map[string]string)
		}
		for real, fake := range pairs {
			if _, exists := c.Replacement.SimulateZH.Dictionary[entityType][real]; !exists {
				c.Replacement.SimulateZH.Dictionary[entityType][real] = fake
			}
		}
	}
	return nil
}

// Validate 启动期一次性校验（契约 §3.3）。失败即退出，不做降级。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Gateway.Upstream) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "upstream is required")
	}
	if !strings.HasPrefix(c.Gateway.Listen, ":") {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "gateway.listen must start with ':' (e.g. :8400)")
	}
	for _, u := range c.Gateway.Upstreams {
		switch u.Protocol {
		case "openai", "anthropic":
		default:
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "unknown upstreams[].protocol: %q", u.Protocol)
		}
		if strings.TrimSpace(u.BaseURL) == "" {
			return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "upstreams[].base_url is required")
		}
	}
	if c.Gateway.RequestTimeout <= 0 {
		c.Gateway.RequestTimeout = 30 * time.Second
	}
	switch c.Replacement.Strategy {
	case "placeholder", "simulate", "bypass":
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
	// 登记表必须有落盘位置：面板里加的值若不落盘，重启就静默消失，
	// 而用户会以为已经登记好了。
	if c.Detection.Registry.Enabled && strings.TrimSpace(c.Detection.Registry.Path) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "detection.registry.path is required when detection.registry.enabled is true")
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
	if err := ValidateSimulateDictionary(c.Replacement.SimulateZH.Dictionary); err != nil {
		return err
	}
	if c.Detection.Cache.MaxEntries <= 0 {
		c.Detection.Cache.MaxEntries = 10000
	}
	return nil
}

// simulatableTypeOrder 支持仿真替换的实体类型，顺序固定（供面板下拉与文档直接使用），
// 与 simulator.Generator.Fake 的 switch 保持一致。
var simulatableTypeOrder = []string{
	types.EntityPersonName,
	types.EntityPhone,
	types.EntityIDCard,
	types.EntityBankCard,
	types.EntityAddress,
	types.EntityEmail,
	types.EntityIPAddress,
	types.EntityDate,
}

var simulatableSet = func() map[string]bool {
	m := make(map[string]bool, len(simulatableTypeOrder))
	for _, t := range simulatableTypeOrder {
		m[t] = true
	}
	return m
}()

// SimulatableTypes 返回可仿真的实体类型副本。
func SimulatableTypes() []string {
	out := make([]string, len(simulatableTypeOrder))
	copy(out, simulatableTypeOrder)
	return out
}

// ValidateSimulateDictionary 校验仿真词典（配置加载期与面板热加载共用同一套规则）。
//
// 最要紧的一条是「仿真值全局唯一」——注意是全局，不是同类型内。仿真值就是上游可见串，
// 而响应侧还原表是 `map[哨兵串]原值` 一张平表（见 replacer.EntryPairs），不分类型分桶。
// 所以两个真实值（哪怕类型不同）共用一个仿真值，就会在还原表里同键互相覆盖，
// 其中一个永久还原不回来、还被错认成另一个——静默损坏，必须在入口拦下。
func ValidateSimulateDictionary(dict map[string]map[string]string) error {
	seen := make(map[string]string) // 仿真值 → "类型/真实值"
	for entityType, pairs := range dict {
		if !simulatableSet[entityType] {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
				"replacement.simulate_zh.dictionary: unknown or non-simulatable entity type %q", entityType)
		}
		for real, fake := range pairs {
			if strings.TrimSpace(real) == "" {
				return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
					"replacement.simulate_zh.dictionary[%s]: empty real value", entityType)
			}
			if strings.TrimSpace(fake) == "" {
				return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
					"replacement.simulate_zh.dictionary[%s][%s]: empty fake value", entityType, real)
			}
			where := entityType + "/" + real
			if prev, dup := seen[fake]; dup {
				return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
					"replacement.simulate_zh.dictionary: fake value %q is used by both %s and %s (restore would break)",
					fake, prev, where)
			}
			seen[fake] = where
		}
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
