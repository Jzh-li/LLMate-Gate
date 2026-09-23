// Package types —— 行为判断层（Judgment Layer）契约。
//
// 本段是《接口与数据契约规范.md》§12 的权威实现，与 detect.go 对称。
// 任何字段变更必须先更新规范。
//
// 设计红线（不可妥协，见方案 §7）：
//
//	模型 / 规则后端只产出 Evidence（category + severity + confidence）；
//	Action 一律由确定性 Mapper 按 per-backend 阈值表算出。
//
// 后果是「同输入必同 action」可审计、可回归，而模型质量只影响 severity 的取值，
// 不影响裁决路径本身。任何把 action 交给模型的改动都违反本契约。
package types

import "fmt"

// ActionKind 待判断对象的来源形态。
type ActionKind string

const (
	// KindToolCall 一次工具调用（OpenAI tool_calls / Anthropic tool_use）。
	// 这是本层最主要、也是唯一「客户端通用」的观测对象。
	KindToolCall ActionKind = "tool_call"
	// KindEgressText 一段疑似外发的文本（如出站请求体摘要）。v1 仅预留。
	KindEgressText ActionKind = "egress_text"
	// KindMCPAsk MCP 门面转发过来的参数扫描请求（P3）。
	KindMCPAsk ActionKind = "mcp_ask"
)

// AllActionKinds 全部来源形态（闭集）。
func AllActionKinds() []ActionKind {
	return []ActionKind{KindToolCall, KindEgressText, KindMCPAsk}
}

// IsKnownActionKind 来源形态是否在闭集内。
func IsKnownActionKind(k ActionKind) bool {
	for _, x := range AllActionKinds() {
		if x == k {
			return true
		}
	}
	return false
}

// ObservationPhase 观测位——决定这条描述是「事前」还是「事后」。
//
// 时序差异是判断层最重要的一条事实：只有 PhaseProposal 能在动作发生前拦截，
// 而它需要流式中断；PhaseExecuted 一定看得见，但看到时动作已经发生。
type ObservationPhase string

const (
	// PhaseProposal ① 当轮响应流里模型提议的 tool_call（执行前）。
	PhaseProposal ObservationPhase = "proposal"
	// PhaseExecuted ② 下一轮回灌的请求体（含工具执行结果，执行后）。
	PhaseExecuted ObservationPhase = "executed"
)

// ActionCategory 行为类别闭集。
//
// ⚠️ 这是契约级闭集：新增/删除都要同步 Specs/02 §12 与本文件，并考虑阈值表与
// 探针集（probe.go）是否仍能覆盖。`unknown` 是一等公民——后端有权说「我判不了」，
// 该结论会让 Mapper 走 review（交人），而不是被当成 benign 放行。
type ActionCategory string

const (
	// CatUnknown 后端无法判定。一等公民：弱模型 / 规则覆盖不到的地方必须能表达它，
	// 否则只能被迫给出一个假的 benign，那正是「自信地错」。
	CatUnknown ActionCategory = "unknown"
	// CatBenign 明确无害。
	CatBenign ActionCategory = "benign"
	// CatPII 内容级 PII（与 detector 重叠，用于对整段文本做判断的场合）。
	CatPII ActionCategory = "pii"
	// CatCredential 凭证 / 密钥类（.ssh / .aws / .env / id_rsa ...）。
	CatCredential ActionCategory = "credential"
	// CatBulkRead 批量读取（含读 .git、递归遍历仓库）。
	CatBulkRead ActionCategory = "bulk_read"
	// CatArchive 打包归档（tar / zip / git archive / 7z ...）。
	//
	// 相对方案 §4 的闭集新增：本次讨论的原始场景（ZCode 打包整个代码仓库）
	// 在 bulk_read / exfil 里表达不准确——「读了整仓」与「把整仓压成一个文件」
	// 是两个不同动作，后者才是外发的前一步，规则侧也可精确识别。
	CatArchive ActionCategory = "archive"
	// CatExfil 外传意图（把本机数据发往外部）。
	CatExfil ActionCategory = "exfil"
	// CatEgress 非白名单出站（网络层语义，v1 仅由规则后端按 host 判定）。
	CatEgress ActionCategory = "network_egress"
	// CatDestructive 删改类破坏动作（rm -rf / git reset --hard / 覆盖写入）。
	CatDestructive ActionCategory = "destructive"
)

// AllCategories 全部行为类别（闭集，顺序与 Specs/02 §12 表格一致）。
func AllCategories() []ActionCategory {
	return []ActionCategory{
		CatUnknown, CatBenign, CatPII, CatCredential,
		CatBulkRead, CatArchive, CatExfil, CatEgress, CatDestructive,
	}
}

// IsKnownCategory 行为类别是否在闭集内。
func IsKnownCategory(c ActionCategory) bool {
	for _, x := range AllCategories() {
		if x == c {
			return true
		}
	}
	return false
}

// Action 最终裁决动作闭集。
//
// ⚠️ 只有确定性 Mapper 能产出 Action。后端返回的 Evidence 里没有这个字段——
// 这是编译期就成立的隔离，不是纪律要求。
type Action string

const (
	// ActionAllow 放行。
	ActionAllow Action = "allow"
	// ActionRedact 抹掉描述里的敏感部分后放行。
	ActionRedact Action = "redact"
	// ActionReview 交人 / 交上层 agent 确认（fail-safe 的落点）。
	ActionReview Action = "review"
	// ActionBlock 阻断。
	ActionBlock Action = "block"
)

// AllActions 全部裁决动作（闭集）。
func AllActions() []Action {
	return []Action{ActionAllow, ActionRedact, ActionReview, ActionBlock}
}

// IsKnownAction 裁决动作是否在闭集内。
func IsKnownAction(a Action) bool {
	for _, x := range AllActions() {
		if x == a {
			return true
		}
	}
	return false
}

// ActionDescriptor 一次待判断的行为。
//
// 判断层只吃这个结构：它没有 URL、没有文件句柄、没有执行能力，
// 因此「判断层本身成为新的攻击面」这条风险被结构性地压住了。
//
// Target / Argv / Meta 的填充责任在采集侧（proxy / mcp），不在判断侧：
// 分词与归一化是确定性的，做一次给所有后端复用，避免每个后端各写一遍。
type ActionDescriptor struct {
	// Kind 来源形态。
	Kind ActionKind `json:"kind"`
	// Phase 观测位（proposal 事前 / executed 事后）。
	Phase ObservationPhase `json:"phase,omitempty"`
	// Tool 工具名（Bash / shell / read_file ...）。
	Tool string `json:"tool,omitempty"`
	// Target host / 路径 / 命令首词——规则后端的主要判据。
	Target string `json:"target,omitempty"`
	// Command 原始命令行文本。**这是判据的权威来源**，采集侧应原样填入。
	//
	// 为什么要原文而不是只用分词结果：`bash -c "tar -czf x ."` 这类包装命令，
	// 一旦只保留 token 列表，引号边界就丢了，判断侧再也无法还原出内层命令。
	// 分词（Argv）是给展示与快速匹配用的派生数据，原文才是事实。
	//
	// 隐私：命令文本与 Tool/Argv 的内容重叠，不引入额外的暴露面；是否落日志
	// 由 audit.log_pii 闸控制（与其它字段一致）。
	Command string `json:"command,omitempty"`
	// Argv 分词结果（无 Command 时的回落；也供面板展示与快速匹配）。
	Argv []string `json:"argv,omitempty"`
	// Size 相关体量（字节数 / 文件数，由采集侧填，0 表示未知）。
	Size int `json:"size,omitempty"`
	// Seq 会话内序号（序列型判定的输入，0 表示未编号）。
	Seq int `json:"seq,omitempty"`
	// ConversationID 会话标识（序列特征的键；不含用户内容）。
	ConversationID string `json:"conversation_id,omitempty"`
	// RequestID 请求标识。
	RequestID string `json:"request_id,omitempty"`
	// Meta 扩展信号（batch_count / entropy / history_hits ...）。
	// 采集侧与行为链上下文（J5）通过它注入派生特征，判断侧只读。
	Meta map[string]string `json:"meta,omitempty"`
}

// Validate 校验描述本身的合法性（不含任何语义判断）。
func (d ActionDescriptor) Validate() error {
	if !IsKnownActionKind(d.Kind) {
		return fmt.Errorf("judge: unknown action kind %q", d.Kind)
	}
	if d.Phase != "" && d.Phase != PhaseProposal && d.Phase != PhaseExecuted {
		return fmt.Errorf("judge: unknown observation phase %q", d.Phase)
	}
	return nil
}

// InputBytes 描述的可判长度估算，供 MaxInputBytes 闸使用。
func (d ActionDescriptor) InputBytes() int {
	n := len(d.Target) + len(d.Tool) + len(d.Command)
	for _, a := range d.Argv {
		n += len(a) + 1
	}
	for k, v := range d.Meta {
		n += len(k) + len(v) + 2
	}
	return n
}

// CommandLine 返回用于匹配的命令行文本：原始命令 > 分词结果 > Target。
//
// 规则后端与自检探针都通过它取「要匹配的那串字符」，避免各处各拼一遍
// （拼接方式不一致会让同一条命令在不同后端眼里变成两个动作）。
func (d ActionDescriptor) CommandLine() string {
	if d.Command != "" {
		return d.Command
	}
	if len(d.Argv) > 0 {
		return joinArgv(d.Argv)
	}
	return d.Target
}

// joinArgv 以空格连接 argv。刻意不做引号还原：这里只需要「可匹配的文本」，
// 还原引号反而会让子串匹配在小概率下失效。
func joinArgv(argv []string) string {
	n := 0
	for _, a := range argv {
		n += len(a) + 1
	}
	if n == 0 {
		return ""
	}
	b := make([]byte, 0, n-1)
	for i, a := range argv {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, a...)
	}
	return string(b)
}

// Evidence 后端给出的「证据」——注意：这里没有 action。
type Evidence struct {
	// Category 行为类别（闭集）。
	Category ActionCategory `json:"category"`
	// Severity 严重度 0..1。
	Severity float64 `json:"severity"`
	// Confidence 置信度 0..1。是否可信由后端在 Capabilities 里声明；
	// 未声明 GivesConfidence 的后端，该字段在 Mapper 里不参与加权。
	Confidence float64 `json:"confidence"`
	// Reasons 人类可读的判据（进审计日志，便于事后解释）。
	Reasons []string `json:"reasons,omitempty"`
	// Engine 产出该证据的后端名（配置里 backends[].name）。
	Engine string `json:"engine"`
	// LatencyMs 后端耗时（毫秒）。
	LatencyMs int64 `json:"latency_ms,omitempty"`
}

// Validate 校验证据的闭集与取值区间。
func (e Evidence) Validate() error {
	if !IsKnownCategory(e.Category) {
		return fmt.Errorf("judge: unknown category %q", e.Category)
	}
	if e.Severity < 0 || e.Severity > 1 {
		return fmt.Errorf("judge: severity out of range [0,1]: %v", e.Severity)
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("judge: confidence out of range [0,1]: %v", e.Confidence)
	}
	return nil
}

// Verdict 最终裁决（确定性代码算出来的）。
type Verdict struct {
	// Action 由 Mapper 判定，后端无法直接设置。
	Action Action `json:"action"`
	// Evidence 产生该裁决的证据。
	Evidence
	// Degraded 该裁决是否来自降级（首个后端未给出结论、由后备后端给出）。
	Degraded bool `json:"degraded,omitempty"`
}

// Validate 校验裁决的闭集与内部一致性。
func (v Verdict) Validate() error {
	if !IsKnownAction(v.Action) {
		return fmt.Errorf("judge: unknown action %q", v.Action)
	}
	return v.Evidence.Validate()
}

// SchemaMode 后端对结构化输出的约束能力。
//
// 这是 BYOM 的准入条件：约束越强，「模型吐出闭集外的值」的概率越低。
// prompt_only 不是禁止使用，而是要求在解析失败时走 review（fail-safe 到人）。
type SchemaMode string

const (
	// SchemaJSONSchema JSON Schema 约束（Ollama format / OpenAI response_format）。
	SchemaJSONSchema SchemaMode = "json_schema"
	// SchemaGBNF llama.cpp 语法约束（GBNF 文法锁死枚举）。
	SchemaGBNF SchemaMode = "gbnf"
	// SchemaFormat 仅约束「输出是 JSON」，不锁枚举。
	SchemaFormat SchemaMode = "format"
	// SchemaPromptOnly 无约束解码，纯靠提示词。解析失败率最高。
	SchemaPromptOnly SchemaMode = "prompt_only"
)

// AllSchemaModes 全部约束模式（闭集）。
func AllSchemaModes() []SchemaMode {
	return []SchemaMode{SchemaJSONSchema, SchemaGBNF, SchemaFormat, SchemaPromptOnly}
}

// IsKnownSchemaMode 约束模式是否在闭集内。
func IsKnownSchemaMode(m SchemaMode) bool {
	for _, x := range AllSchemaModes() {
		if x == m {
			return true
		}
	}
	return false
}

// Constrained 是否受结构化约束（prompt_only 之外都算）。
func (m SchemaMode) Constrained() bool {
	return m != "" && m != SchemaPromptOnly
}

// Capabilities 一个后端的能力声明。
//
// 这五项不是装饰：每一项各自对应一条降级路径，缺一项就意味着遇到该边界时只能猜。
//
//	Categories 为空   → 视为支持全部类别
//	SchemaModes 为空  → 视为仅 prompt_only（最保守假设）
//	GivesConfidence   → false 时 Mapper 不做 confidence 加权
//	MaxInputBytes 为0 → 不限长
//	Deterministic     → false 时不参与「同输入必同动作」的可复现性宣称
type Capabilities struct {
	Categories      []ActionCategory `json:"categories,omitempty"`
	SchemaModes     []SchemaMode     `json:"schema_modes,omitempty"`
	GivesConfidence bool             `json:"gives_confidence"`
	MaxInputBytes   int              `json:"max_input_bytes,omitempty"`
	Deterministic   bool             `json:"deterministic"`
}

// SupportsCategory 该后端是否声称能判这个类别。
func (c Capabilities) SupportsCategory(cat ActionCategory) bool {
	if len(c.Categories) == 0 {
		return true
	}
	for _, x := range c.Categories {
		if x == cat {
			return true
		}
	}
	return false
}

// HasSchemaMode 该后端是否支持某种约束模式。
func (c Capabilities) HasSchemaMode(m SchemaMode) bool {
	if len(c.SchemaModes) == 0 {
		return m == SchemaPromptOnly
	}
	for _, x := range c.SchemaModes {
		if x == m {
			return true
		}
	}
	return false
}

// BestSchemaMode 在候选模式里挑约束最强、且该后端支持的一个。
//
// 顺序即强度：json_schema > gbnf > format > prompt_only。都不支持时返回 prompt_only，
// 由调用方的解析失败路径兜底（而不是假定它受约束）。
func (c Capabilities) BestSchemaMode(want ...SchemaMode) SchemaMode {
	strength := func(m SchemaMode) int {
		switch m {
		case SchemaJSONSchema:
			return 3
		case SchemaGBNF:
			return 2
		case SchemaFormat:
			return 1
		default:
			return 0
		}
	}
	best := SchemaMode("")
	for _, m := range want {
		if !c.HasSchemaMode(m) {
			continue
		}
		if best == "" || strength(m) > strength(best) {
			best = m
		}
	}
	if best == "" {
		return SchemaPromptOnly
	}
	return best
}

// JudgmentThresholds 确定性 Mapper 的阈值表。
//
// ⚠️ 必须 per-backend（契约 §12 / 评审 D4）：confidence 在不同后端之间**不可比**
// （Laya 自承高棉语准确率 0.000 而置信度 95.2%），同一套阈值套到所有后端上，
// 等于假定它们的 severity 分布相同——这个假定不成立。
type JudgmentThresholds struct {
	// Block severity ≥ Block → block。
	Block float64 `json:"block" yaml:"block"`
	// Review severity ≥ Review → review。
	Review float64 `json:"review" yaml:"review"`
	// Redact severity ≥ Redact → redact；低于 Redact → allow。
	Redact float64 `json:"redact" yaml:"redact"`
}

// DefaultThresholds 保守缺省：宁可多交人复核，也不默认放行。
func DefaultThresholds() JudgmentThresholds {
	return JudgmentThresholds{Block: 0.85, Review: 0.55, Redact: 0.30}
}

// Validate 校验阈值单调性与取值范围。三条必须严格递减，否则档位相互吞没。
func (t JudgmentThresholds) Validate() error {
	for name, v := range map[string]float64{"block": t.Block, "review": t.Review, "redact": t.Redact} {
		if v < 0 || v > 1 {
			return fmt.Errorf("judge: threshold %s out of range [0,1]: %v", name, v)
		}
	}
	if !(t.Block > t.Review && t.Review > t.Redact) {
		return fmt.Errorf("judge: thresholds must be strictly decreasing (block > review > redact), got block=%v review=%v redact=%v",
			t.Block, t.Review, t.Redact)
	}
	return nil
}
