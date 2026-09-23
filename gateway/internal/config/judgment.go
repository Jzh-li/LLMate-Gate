// 判断层配置（契约 §12 / 方案 §8）。
//
// 独立成文件的理由：judgment 是 config 里唯一「会去访问另一个进程」的段落，
// 它的校验规则（尤其 base_url 必须在本机/内网）与其余部分性质不同，值得单独可读。

package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"
)

// 判断层缺省值。
const (
	// DefaultJudgmentTimeout 单次判定超时。300ms 是方案的预算值：
	// 判断层挂在请求热路径上，它的耗时上限必须是「用户感知不到」的量级。
	DefaultJudgmentTimeout = 300 * time.Millisecond
	// MaxJudgmentTimeout 超时上限。超过 1s 的判定不该挂在热路径上，
	// 该改用 shadow / 离线模式，所以配置层面直接拒绝，避免用户把自己拖死。
	MaxJudgmentTimeout = 1 * time.Second
	// MaxJudgmentBackends 后端数量上限（防呆：链过长意味着单请求最坏耗时线性增长）。
	MaxJudgmentBackends = 4
)

// JudgmentConfig 行为判断层配置。
type JudgmentConfig struct {
	// Enabled 是否启用判断层。false 时其余字段全部不生效（不装配、不拦截、零开销）。
	Enabled bool `yaml:"enabled"`
	// Mode shadow（只记不拦）| enforce（按 verdict 动作生效）。
	//
	// enforce 需要 J6 评测数据支撑才可开启——没有漏报/误报口径之前，
	// 拦住的东西是否该拦是不可知的。
	Mode string `yaml:"mode"`
	// FailClosed 判断层整体不可用时的姿态（true → review，false → allow）。
	//
	// ⚠️ 与 policy.fail_closed 语义一致：默认 true。判断层挂了并不意味着
	// 「没有任何可疑行为」，只意味着「这次没能看」，不能当成放行理由。
	FailClosed bool `yaml:"fail_closed"`
	// Timeout 单次判定超时（超时 ⇒ 降级到下个后端，不是放行）。
	Timeout time.Duration `yaml:"timeout"`
	// Backends 后端列表，顺序即降级顺序（语义能力强者在前、确定性规则在最后兜底）。
	Backends []JudgmentBackendConfig `yaml:"backends"`
	// Thresholds 全局缺省阈值表；每个后端可用自己的 thresholds 覆盖。
	//
	// per-backend 是硬要求：confidence 跨后端不可比，同一套阈值相当于假定
	// 各后端的 severity 分布相同，这个假定不成立。
	Thresholds *types.JudgmentThresholds `yaml:"thresholds"`
	// Points 四个判断点的分开关（灰度上线用）。
	Points JudgmentPointsConfig `yaml:"points"`
	// Whitelist 先于一切判断的放行名单。
	Whitelist JudgmentWhitelistConfig `yaml:"whitelist"`
}

// JudgmentBackendConfig 单个判断后端。
type JudgmentBackendConfig struct {
	// Name 后端名（进 Evidence.Engine 与指标标签），非空且全局唯一。
	Name string `yaml:"name"`
	// Kind rules | openai | http（见 judge.Kind）。
	Kind string `yaml:"kind"`
	// BaseURL 服务地址，kind=openai/http 必填，且**必须指向本机或内网**。
	//
	// 这是硬约束而非建议：一个用来判断「数据会不会外泄」的组件，自己把数据
	// 发到公网上，是自相矛盾的。配置期就拒绝，不给运行期留口子。
	BaseURL string `yaml:"base_url"`
	// Model 模型名（kind=openai 必填）。
	Model string `yaml:"model"`
	// SchemaMode 约束解码模式 json_schema|gbnf|format|prompt_only。
	//
	// 空值按 prompt_only 处理（最保守假设）：不假定用户的服务受约束，
	// 于是解析失败会走 review 而不是被当成合法输出。
	SchemaMode string `yaml:"schema_mode"`
	// Timeout 该后端单次超时（0 → 用全局 Timeout）。
	Timeout time.Duration `yaml:"timeout"`
	// MaxInputBytes 输入上限（0 → 用后端能力声明的上限）。
	MaxInputBytes int `yaml:"max_input_bytes"`
	// Thresholds 该后端专属阈值表（nil → 用全局 Thresholds）。
	Thresholds *types.JudgmentThresholds `yaml:"thresholds"`
}

// JudgmentPointsConfig 判断点的分开关。
//
// 默认全 false：判断层引入时默认不产生任何行为，由使用方逐点打开。
type JudgmentPointsConfig struct {
	// InboundText 请求入站文本整段判定（v1 仅预留）。
	InboundText bool `yaml:"inbound_text"`
	// ToolParams 工具调用参数判定（P2，本次主战场）。
	ToolParams bool `yaml:"tool_params"`
	// MCPScan MCP 门面参数扫描（P3）。
	MCPScan bool `yaml:"mcp_scan"`
	// Endpoint /v1/judge 控制面端点（P4）。
	Endpoint bool `yaml:"endpoint"`
}

// JudgmentWhitelistConfig 白名单。
type JudgmentWhitelistConfig struct {
	// Hosts 允许的目标主机（精确匹配；以 "." 开头表示后缀匹配）。
	Hosts []string `yaml:"hosts"`
	// Tools 允许的工具名（精确匹配）。
	Tools []string `yaml:"tools"`
}

// AnyPointEnabled 是否有任一判断点打开（enabled=true 但全部 point 关闭 = 空转）。
func (p JudgmentPointsConfig) AnyPointEnabled() bool {
	return p.InboundText || p.ToolParams || p.MCPScan || p.Endpoint
}

// String 输出已开启的判断点（启动日志用）。全关时返回 "none"——
// enabled=true 但一个点都没开是「看起来开了、其实什么都没做」，日志要能直接看出来。
func (p JudgmentPointsConfig) String() string {
	on := make([]string, 0, 4)
	if p.InboundText {
		on = append(on, "inbound_text")
	}
	if p.ToolParams {
		on = append(on, "tool_params")
	}
	if p.MCPScan {
		on = append(on, "mcp_scan")
	}
	if p.Endpoint {
		on = append(on, "endpoint")
	}
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ",")
}

// Validate 判断层配置校验（启动期，失败即退出）。
func (c *JudgmentConfig) Validate() error {
	// 规范化：把「没写」补成显式缺省，让下游只需处理确定值。
	if c.Mode == "" {
		c.Mode = "shadow"
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultJudgmentTimeout
	}

	switch c.Mode {
	case "shadow":
	case "enforce":
		// 空转的配置项比没有更危险：如果这里收下 enforce 却什么都不拦，
		// 用户会以为「我已经开了拦截」。v1 只做观测，因此直接拒绝这个值，
		// 等 J6 评测集给出漏报/误报口径之后再实现。
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig,
			"judgment.mode=enforce is not implemented in v1: verdicts are observational only. "+
				"Enforcement needs a measured false-positive/false-negative rate first; use mode: shadow")
	default:
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"unknown judgment.mode: %q (want shadow)", c.Mode)
	}
	if c.Timeout < 0 || c.Timeout > MaxJudgmentTimeout {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"judgment.timeout must be in (0, %s], got %s", MaxJudgmentTimeout, c.Timeout)
	}
	if c.Thresholds != nil {
		if err := c.Thresholds.Validate(); err != nil {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "judgment.thresholds: %v", err)
		}
	}
	if len(c.Backends) > MaxJudgmentBackends {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"judgment.backends supports at most %d entries, got %d", MaxJudgmentBackends, len(c.Backends))
	}

	seen := make(map[string]bool, len(c.Backends))
	for i := range c.Backends {
		b := &c.Backends[i]
		where := fmt.Sprintf("judgment.backends[%d]", i)
		if strings.TrimSpace(b.Name) == "" {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "%s.name is required", where)
		}
		where = fmt.Sprintf("judgment.backends[%d](%s)", i, b.Name)
		if seen[b.Name] {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
				"%s: duplicate backend name %q", where, b.Name)
		}
		seen[b.Name] = true
		if err := b.Validate(where, c.Timeout); err != nil {
			return err
		}
	}

	// enabled 但没有任何判断点：配置看起来开着、实际什么都不做。
	// 空转的配置项比没有更危险——它会让人以为「我已经防住了」。
	if c.Enabled && len(c.Backends) == 0 {
		return gatewayerrors.New(gatewayerrors.CodeInvalidConfig,
			"judgment.enabled is true but judgment.backends is empty (add at least one backend, or set enabled: false)")
	}
	return nil
}

// Validate 单个后端校验。where 用于把错误定位到具体条目，globalTimeout 是全局超时。
func (b *JudgmentBackendConfig) Validate(where string, globalTimeout time.Duration) error {
	switch b.Kind {
	case "rules", "openai", "http":
	default:
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"%s.kind is required and must be rules|openai|http, got %q", where, b.Kind)
	}
	switch b.Kind {
	case "rules":
		// 规则后端是进程内的，任何连接参数都是误解。静默忽略会让用户以为
		// 「我配了个远端规则服务」——报错比忽略安全。
		if strings.TrimSpace(b.BaseURL) != "" {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
				"%s.kind=rules has no base_url (rules run in-process); remove it or use kind=openai", where)
		}
		if strings.TrimSpace(b.Model) != "" {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
				"%s.kind=rules has no model; remove it or use kind=openai", where)
		}
	case "openai", "http":
		if strings.TrimSpace(b.BaseURL) == "" {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "%s.base_url is required for kind=%s", where, b.Kind)
		}
		if err := validateLocalEndpoint(b.BaseURL); err != nil {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "%s.base_url: %v", where, err)
		}
		if b.Kind == "openai" && strings.TrimSpace(b.Model) == "" {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
				"%s.model is required for kind=openai", where)
		}
	}

	if b.SchemaMode == "" {
		b.SchemaMode = string(types.SchemaPromptOnly)
	}
	if !types.IsKnownSchemaMode(types.SchemaMode(b.SchemaMode)) {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"%s.schema_mode invalid %q (want json_schema|gbnf|format|prompt_only)", where, b.SchemaMode)
	}
	if b.Timeout < 0 || b.Timeout > MaxJudgmentTimeout {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"%s.timeout must be in (0, %s], got %s", where, MaxJudgmentTimeout, b.Timeout)
	}
	if b.Timeout == 0 {
		b.Timeout = globalTimeout
	}
	if b.MaxInputBytes < 0 {
		return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig,
			"%s.max_input_bytes must be >= 0, got %d", where, b.MaxInputBytes)
	}
	if b.Thresholds != nil {
		if err := b.Thresholds.Validate(); err != nil {
			return gatewayerrors.Errorf(gatewayerrors.CodeInvalidConfig, "%s.thresholds: %v", where, err)
		}
	}
	return nil
}

// EffectiveThresholds 该后端最终使用的阈值表（自身 > 全局 > 内置缺省）。
func (b *JudgmentBackendConfig) EffectiveThresholds(global *types.JudgmentThresholds) types.JudgmentThresholds {
	switch {
	case b.Thresholds != nil:
		return *b.Thresholds
	case global != nil:
		return *global
	default:
		return types.DefaultThresholds()
	}
}

// validateLocalEndpoint 要求 base_url 指向本机或内网。
//
// 允许：localhost / *.localhost / 回环 / 私网（10/8、172.16/12、192.168/16）/
// 链路本地 / 未指定（0.0.0.0，等价本机监听）。
//
// 拒绝两类：
//   - 公网 IP —— 明确违反「数据不出机」；
//   - 非 IP 的主机名（除 localhost）—— 配置期无法判断它解析到哪里，而 DNS
//     可以被改。要让用户用主机名，就得在运行期信任一次解析结果，等于把一个
//     安全属性交给不可控的外部状态。写 IP 是明确的替代路径。
func validateLocalEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("missing host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("host %q is not a literal IP: judging backends must be on this machine or a private network, "+
			"and hostnames cannot be verified at config time (use the IP instead of a DNS name)", host)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return nil
	}
	return fmt.Errorf("address %s is not loopback/private: the judgment layer must not send data to the public internet", ip)
}
