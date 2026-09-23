// Package config 负责配置加载、${ENV} 展开与启动期一次性校验（契约 §3）。
//
// 校验失败一律退出（exit code 2），绝不进入降级服务状态。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/simulator"

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
	Judgment    JudgmentConfig    `yaml:"judgment"`
}

// GatewayConfig 网关监听与上游配置。
type GatewayConfig struct {
	Listen    string `yaml:"listen"`
	Upstream  string `yaml:"upstream"`
	AuthToken string `yaml:"auth_token"`
	// AuthTokenFile：auth_token 未配置时，网关自动生成的令牌存放位置。
	//
	// 为什么需要它：无鉴权 + 已配 upstream_api_key 意味着「任何能连到端口的人都能
	// 用你的凭据调上游」，所以不能再让「忘写 auth_token」静默变成裸奔。但只把令牌
	// 生成在内存里同样不行——每次重启换一个令牌，客户端要一直改配置，最后用户会
	// 干脆关掉鉴权，反而更不安全。写成 0600 的文件是让默认安全与可用性同时成立的最
	// 小代价：生成一次，之后重启复用。
	AuthTokenFile string `yaml:"auth_token_file"`
	// AllowUnauthenticated：显式声明「我知道本网关不鉴权，且接受」。
	//
	// 存在的意义是把「没配」与「明确不要」分开。本地基准测试、CI 守门这类场景确实
	// 需要零鉴权（见 configs/bench-gate.yaml），但它们应当把这个意图写出来，而不是
	// 依赖「auth_token 恰好为空」——否则同一个空值既表示「我还没配」也表示「我不要」，
	// 网关无从区分，只能永远选择更弱的那个解释。
	AllowUnauthenticated bool `yaml:"allow_unauthenticated"`
	// ControlAuthToken：控制面（/v1/privacy/*，可还原原文）的独立令牌。
	//
	// 控制面能按 request_id 还原原文，数据面只是转发。两者权限级别不同：
	// 给 Claude Code hooks 的数据面令牌不该同时具备「还原任意请求原文」的能力。
	// 留空则回退到 auth_token（保持单令牌部署的兼容性）。
	ControlAuthToken string           `yaml:"control_auth_token"`
	UpstreamAPIKey   string           `yaml:"upstream_api_key"`
	Upstreams        []UpstreamConfig `yaml:"upstreams"`
	RequestTimeout   time.Duration    `yaml:"request_timeout"`
	Debug            bool             `yaml:"debug"`
	LogLevel         string           `yaml:"log_level"`
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
	Strategy     string           `yaml:"strategy"`
	SimulateZH   SimulateZHConfig `yaml:"simulate_zh"`
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
//
// 三个字段里只有 FailClosed 是真的开关（`main.go` 读它构造 processor）。
// 另外两个是**历史遗留的假开关**：被定义了、被写进默认配置 / 示例 YAML /
// Specs，但代码里从没有任何一处读它们，对应行为始终无条件执行。
// 也就是说 `tool_call_scan: false` 与 `stream_restore: false` 过去都是
// 一句无声的谎话。收尾见 validateAlwaysOn：不删键（删了会让已发布配置在
// 严格解析下拒绝启动），但把合法取值收窄成 true，写 false 在启动期报错。
type PolicyConfig struct {
	FailClosed bool `yaml:"fail_closed"`
	// ToolCallScan 只能为真。它声称控制的「对 tool_calls 参数的递归扫描」
	// （transform / anonymizeJSONString）无条件执行。
	//
	// 不给它真开关的理由：tool_calls 的 arguments 承载命令、路径、文件名和
	// 模型自造的字面量，是整份请求里 PII 密度最高、也最容易被外发的位置。
	ToolCallScan bool `yaml:"tool_call_scan"`
	// StreamRestore 只能为真。它声称控制的「SSE trie 缓冲还原」同样无条件
	// 执行（proxy 侧一律用 pipeline.StreamRestorer 造还原器）。
	//
	// 不给它真开关的理由：不还原，客户端拿到的是 <<zh_phone_1>> 而不是真实值，
	// 用户自己的应用当场就坏了——这是正确性问题，不是可选项。
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
	// MaxSizeMB 单个审计文件大小上限（MiB），超过则轮转为 path.1 / path.2…；
	// 0 表示关闭轮转。默认 100。
	MaxSizeMB int `yaml:"max_size_mb"`
	// MaxBackups 保留的历史审计文件份数。默认 3。
	MaxBackups int `yaml:"max_backups"`
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
			AuthTokenFile:  "./auth_token",
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
		Audit: AuditConfig{Enabled: true, Path: "./audit.log", Export: []string{"pip", "gdpr"}, LogPII: false, MaxSizeMB: 100, MaxBackups: 3},
		// 判断层默认关闭：新增一个「能拦请求」的组件，默认姿态必须是「不生效」。
		// 打开它需要显式配 enabled + backends + points，三者缺一就是空转配置。
		Judgment: JudgmentConfig{
			Enabled:    false,
			Mode:       "shadow",
			FailClosed: true,
			Timeout:    DefaultJudgmentTimeout,
		},
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
	c.Gateway.AuthTokenFile = expandEnv(c.Gateway.AuthTokenFile)
	c.Gateway.ControlAuthToken = expandEnv(c.Gateway.ControlAuthToken)
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
	for i := range c.Judgment.Backends {
		c.Judgment.Backends[i].BaseURL = expandEnv(c.Judgment.Backends[i].BaseURL)
		c.Judgment.Backends[i].Model = expandEnv(c.Judgment.Backends[i].Model)
	}
}

// backtickRe 匹配 yaml.v3 错误消息里回显的值片段。
var backtickRe = regexp.MustCompile("`[^`]*`")

// describeYAMLError 把 yaml.v3 的解析错误压成一行可定位的提示。
//
// 严格模式下最常见的是「未知键」：yaml 会给出形如
// `line 5: field upstream not found in type config.Config` 的明细，
// 这是用户唯一能定位键名拼错的线索，必须带出来——旧实现只报一句
// "parse config yaml"，用户完全不知道哪里写错了。
//
// 与 registry.redactYAMLError 同款处理：抹掉反引号内回显的值，避免把配置里的
// 明文（如 upstream_api_key）写进启动日志。键名与行号保留，排查够用。
func describeYAMLError(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		parts := make([]string, 0, len(te.Errors))
		for _, e := range te.Errors {
			parts = append(parts, backtickRe.ReplaceAllString(e, "`…`"))
		}
		return strings.Join(parts, "; ")
	}
	return backtickRe.ReplaceAllString(err.Error(), "`…`")
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
		//
		// 用严格模式（KnownFields）：出现结构体里不存在的键就直接报错。
		// 默认的宽松解析会静默丢弃拼错的键——用户按安装脚本提示写了
		// upstream.api_key（正确键是 gateway.upstream_api_key），网关一声不吭
		// 地用空 key 转发、上游 401，而启动日志与配置摘要里看不出任何异常。
		// 本项目其余校验一律「失败即退出、不降级」，未知键不应例外。
		//
		// 空文件按「全部走默认值」处理（与旧行为一致，首次运行时常见）。
		expanded := expandEnv(string(raw))
		if strings.TrimSpace(expanded) != "" {
			dec := yaml.NewDecoder(strings.NewReader(expanded))
			dec.KnownFields(true)
			if err := dec.Decode(c); err != nil {
				return nil, gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
					"parse config yaml: %s", describeYAMLError(err))
			}
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

// validateAlwaysOn 校验一个「历史遗留的假开关」：键存在于已发布的配置里、
// 但代码从未读它，对应行为始终无条件执行，因此合法取值只有 true（或缺省）。
//
// 为什么是报错而不是静默忽略：这类键最坏的地方不是它没用，而是它**看起来有用**。
// 运维把 tool_call_scan 改成 false，会以为自己关掉了某个扫描，实际上什么都没
// 发生；日志、指标、配置摘要全都显示不出这件事。同 mode: enforce 的处理一致——
// 空转的配置项比没有更危险。
//
// 为什么不是直接删字段：Load 用严格解析（KnownFields），删了会让所有已发布的、
// 带这一行的配置在升级后拒绝启动。改个空转字段不值得付这个代价。
func validateAlwaysOn(key string, v bool) error {
	if v {
		return nil
	}
	return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
		"policy.%s is always on and cannot be disabled; "+
			"remove the key (it defaults to true) or set it to true", key)
}

// Validate 启动期一次性校验（契约 §3.3）。失败即退出，不做降级。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Gateway.Upstream) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "upstream is required")
	}
	if !strings.HasPrefix(c.Gateway.Listen, ":") {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "gateway.listen must start with ':' (e.g. :8400)")
	}
	// 两个字段同时给出是自相矛盾的配置：用户很可能以为「配了 token 又开了
	// allow_unauthenticated」等于「本地免密、外部要鉴权」，而实际语义只能是二选一。
	// 本项目其余校验一律「失败即退出、不降级」，这种会让人误解安全状态的组合不例外。
	if strings.TrimSpace(c.Gateway.AuthToken) != "" && c.Gateway.AllowUnauthenticated {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig,
			"gateway.allow_unauthenticated must not be true when gateway.auth_token is set (pick one)")
	}
	// 未配置 token 且未显式声明免鉴权时，网关要自动生成令牌并落盘 —— 此时路径必须可用。
	if strings.TrimSpace(c.Gateway.AuthToken) == "" && !c.Gateway.AllowUnauthenticated &&
		strings.TrimSpace(c.Gateway.AuthTokenFile) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig,
			"gateway.auth_token_file is required when auth_token is unset and allow_unauthenticated is false")
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
	// policy 下的两个「假开关」——它们从来没被代码读过，行为始终无条件执行。
	// 详见 PolicyConfig 的注释：这里把合法取值收窄成 true，谎话变成一道闸。
	if err := validateAlwaysOn("tool_call_scan", c.Policy.ToolCallScan); err != nil {
		return err
	}
	if err := validateAlwaysOn("stream_restore", c.Policy.StreamRestore); err != nil {
		return err
	}
	// 登记表必须有落盘位置：面板里加的值若不落盘，重启就静默消失，
	// 而用户会以为已经登记好了。
	if c.Detection.Registry.Enabled && strings.TrimSpace(c.Detection.Registry.Path) == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "detection.registry.path is required when detection.registry.enabled is true")
	}
	if c.Vault.RequestTTL < 5*time.Minute {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig, "vault.request_ttl must be >= 5m")
	}
	// vault.persist 在旧实现里是单向无效功能：落盘用本次进程的随机密钥（passphrase
	// 恒为 nil），而 Get 只查内存 map、从不读磁盘。于是配了 persist 的结果是——
	// 文件永远解不开、也永不会被读到，只在磁盘上持续累积不可解密的 .lmgv。
	//
	// 这比「不能持久化」更危险：用户据此认为「我已持久化」，实际重启即丢。而
	// VaultConfig 里没有任何密钥字段，所以当前也不存在「配好密钥让它生效」的路径。
	// 因此在密钥管理（轮换 / 备份 / 谁能读）想清楚之前直接拒绝这个开关，而不是
	// 留一条看起来能用、实际骗人的路。
	if c.Vault.Persist {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig,
			"vault.persist is not supported: mapping tables are memory-only "+
				"(sealed files cannot be read back because the key is per-process and never stored); "+
				"remove vault.persist from your config, or state your persistence requirement as an issue")
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
	if c.Audit.MaxSizeMB < 0 {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"audit.max_size_mb must be >= 0 (0 disables rotation), got %d", c.Audit.MaxSizeMB)
	}
	if c.Audit.MaxBackups <= 0 {
		c.Audit.MaxBackups = 3
	}
	if c.Detection.Cache.MaxEntries <= 0 {
		c.Detection.Cache.MaxEntries = 10000
	}
	if err := c.Judgment.Validate(); err != nil {
		return err
	}
	return nil
}

// ResolveAuthToken 解析数据面实际使用的访问令牌。
//
// 三种来源，优先级从高到低：
//  1. gateway.auth_token —— 用户显式配置
//  2. gateway.auth_token_file —— 上次自动生成并落盘的令牌（重启复用，客户端不必改配置）
//  3. 新生成 32 字节随机令牌并写入 gateway.auth_token_file（0600）
//
// 返回空令牌表示「本次启动明确不鉴权」（allow_unauthenticated=true）。第二个返回值
// 表示令牌是否为本次新生成，调用方据此决定要不要把令牌值打印出来（只需要一次）。
func (c *Config) ResolveAuthToken() (string, bool, error) {
	if tok := strings.TrimSpace(c.Gateway.AuthToken); tok != "" {
		return tok, false, nil
	}
	if c.Gateway.AllowUnauthenticated {
		return "", false, nil
	}
	path := strings.TrimSpace(c.Gateway.AuthTokenFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			return tok, false, nil
		}
		// 文件存在但内容为空：当作「尚未生成」，走下面的覆写分支。
	case os.IsNotExist(err):
		// 首次运行：生成。
	default:
		return "", false, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "read auth_token_file", err)
	}
	tok, err := generateAuthToken()
	if err != nil {
		return "", false, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "generate auth token", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", false, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "create auth_token_file dir", err)
		}
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, gatewayerrors.Wrap(gatewayerrors.CodeInvalidConfig, "write auth_token_file", err)
	}
	return tok, true, nil
}

// generateAuthToken 生成 32 字节随机令牌（hex 编码，64 字符）。
func generateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// authState 返回可安全落日志的鉴权状态（绝不输出令牌本身）。
func (c *Config) authState() string {
	switch {
	case c.Gateway.AllowUnauthenticated:
		return "none(explicit)"
	case strings.TrimSpace(c.Gateway.AuthToken) != "":
		return "configured"
	case strings.TrimSpace(c.Gateway.ControlAuthToken) != "":
		return "auto+control"
	default:
		return "auto"
	}
}

// SimulatableTypes 返回可仿真的实体类型副本。
//
// 权威来源是 simulator.simulatable 登记表（simulator.Fake 的派发依据）。此处刻意
// 不再维护第二份清单——早期本文件有一份与 simulator 的 switch 手工对齐的列表，
// 一旦有人只改一边，面板下拉与词典校验就会和实际仿真能力脱节。
func SimulatableTypes() []string {
	return simulator.SimulatableTypes()
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
		if !simulator.Simulatable(entityType) {
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

// EffectiveUpstream 一条「实际生效」的上游路由。
//
// 与 UpstreamConfig 的区别：APIKey 是明文（供建连使用），因此它的 String() 只输出
// set / none(passthrough)，绝不把 key 本身写进日志或配置摘要。
type EffectiveUpstream struct {
	Protocol   string
	BaseURL    string
	APIKey     string
	APIVersion string
	PathPrefix string
}

// String 输出可安全落日志的形态：只报「有没有 key」，不报 key 内容。
func (u EffectiveUpstream) String() string {
	key := "key=none(passthrough)"
	if strings.TrimSpace(u.APIKey) != "" {
		key = "key=set"
	}
	s := u.Protocol + "=" + u.BaseURL
	if u.PathPrefix != "" {
		s += " path_prefix=" + u.PathPrefix
	}
	if u.APIVersion != "" {
		s += " api_version=" + u.APIVersion
	}
	return s + " " + key
}

// EffectiveUpstreams 返回合并后实际生效的上游路由（openai 在前，anthropic 其后）。
//
// 合并语义是「默认值 + 同协议覆盖」，而这一点很容易踩坑：
// gateway.upstream 是必需的默认 OpenAI 上游，upstreams 里 protocol=openai 的条目会
// 静默覆盖它。于是只打印 gateway.upstream 会把已被覆盖的旧值当成生效值——用户改了
// upstreams 发现「没生效」，看日志却被带偏。
//
// 本方法是这层合并语义的**唯一实现**：启动日志（Config.String）与真实建连
// （cmd/llmate-gate）都走它，二者不可能再各说各话。
func (c *Config) EffectiveUpstreams() []EffectiveUpstream {
	openai := EffectiveUpstream{
		Protocol: "openai",
		BaseURL:  c.Gateway.Upstream,
		APIKey:   c.Gateway.UpstreamAPIKey,
	}
	var anthropic *EffectiveUpstream
	for _, u := range c.Gateway.Upstreams {
		// 直接类型转换，而不是逐字段写字面量：两个结构体字段一一对应（同名同类型同序），
		// 转换让「必须保持同构」成为**编译期约束** —— 将来给任一侧加字段，
		// 这里会立刻编译失败，而不是静默漏传一个字段。
		// （staticcheck S1016 也正是要求这么写。）
		e := EffectiveUpstream(u)
		switch u.Protocol {
		case "openai":
			openai = e
		case "anthropic":
			if e.APIVersion == "" {
				e.APIVersion = "2023-06-01"
			}
			cp := e
			anthropic = &cp
		}
	}
	out := []EffectiveUpstream{openai}
	if anthropic != nil {
		out = append(out, *anthropic)
	}
	return out
}

// String 打印脱敏后的配置摘要（绝不打印 token / key）。
//
// upstreams 打印的是**合并后的生效值**而非 gateway.upstream 字面量，理由见
// EffectiveUpstreams 的注释。
func (c *Config) String() string {
	routes := make([]string, 0, 2)
	for _, u := range c.EffectiveUpstreams() {
		routes = append(routes, u.String())
	}
	return fmt.Sprintf("listen=%s upstreams=[%s] engine=%s strategy=%s fail_closed=%v debug=%v auth=%s",
		c.Gateway.Listen, strings.Join(routes, "; "), c.Detection.Engine, c.Replacement.Strategy,
		c.Policy.FailClosed, c.Gateway.Debug, c.authState())
}
