package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"gateway/pkg/types"
)

// 本文件实现 OpenAI 兼容后端与自定义 HTTP 逃生口。
//
// 「BYOM 骨架」能否成立，全看这里的**没有 per-model 代码**：
// 无论对方是 Ollama、llama.cpp server、vLLM、LM Studio、LocalAI 还是用户自己
// 写的小服务，适配器只认三样东西 —— `base_url`、`model`、`schema_mode`。
// 新增一个模型不该新增一行 Go 代码。

// defaultMaxInputBytes 模型类后端的输入上限缺省值。
//
// 模型有上下文窗口，超长输入会被截断或报错——截断更危险（判断基于半条命令）。
// 因此宁可显式拒绝（→ 降级到下一个后端），也不截断。8192 足够容纳一条长命令行。
const defaultMaxInputBytes = 8192

// maxModelOutputTokens 判定输出的 token 上限。判定只需一个短 JSON，
// 给出上限是为了防止模型「解释太多」把时间预算吃光。
const maxModelOutputTokens = 256

// newLocalHTTPClient 构造一个「只能打本机/内网」的 HTTP 客户端。
//
// 三处刻意的设置，每一处都对应一个真实的绕过路径：
//
//	Proxy: nil           —— 默认的 ProxyFromEnvironment 会让请求走 http_proxy，
//	                        而代理在别的机器上。配了 localhost 后端却把数据
//	                        送到代理服务器，是「不联网」约束最容易被无声破坏的一处。
//	CheckRedirect        —— 默认跟随 302。一个 127.0.0.1 上的服务可以把请求
//	                        重定向到公网。配置期校验了 base_url，运行期也不能松。
//	DialContext timeout  —— 连不上时要快速失败（→ 降级），不能在热路径上等。
func newLocalHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   2 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("judge: refusing to follow redirect (backend must stay on the configured host)")
		},
	}
}

// modelOutput 模型/自定义服务的输出契约（闭集外的值一律视为无效输出）。
type modelOutput struct {
	Category   string   `json:"category"`
	Severity   float64  `json:"severity"`
	Confidence float64  `json:"confidence"`
	Reasons    []string `json:"reasons,omitempty"`
}

// toEvidence 把解析结果转成闭集内的 Evidence；任何越界都返回错误。
//
// 这里**不**做宽容：`category: "malware"` 不会被悄悄归到 unknown，
// 而是报 invalidOutput。理由是这个比例本身就是「用户选的模型合不合格」的
// 关键指标——静默修正会让它永远看不见。
func (o modelOutput) toEvidence(engine string) (types.Evidence, error) {
	cat := types.ActionCategory(strings.ToLower(strings.TrimSpace(o.Category)))
	if !types.IsKnownCategory(cat) {
		return types.Evidence{}, fmt.Errorf("category %q not in closed set", o.Category)
	}
	sev := clamp01(o.Severity)
	conf := clamp01(o.Confidence)
	if len(o.Reasons) > 4 {
		o.Reasons = o.Reasons[:4]
	}
	for i, r := range o.Reasons {
		o.Reasons[i] = truncate(r, 200)
	}
	return types.Evidence{
		Category: cat, Severity: sev, Confidence: conf,
		Reasons: o.Reasons, Engine: engine,
	}, nil
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractJSONObject 从模型输出里取出第一个 JSON 对象。
//
// 即使配了约束解码也保留这层：用户可能实际用的是 prompt_only（配置与实际
// 不一致是常态），此时模型爱把 JSON 包在 ```json fence 里或前后加解释。
// 剥掉包裹不等于放宽契约——真正的契约校验发生在 toEvidence（闭集）。
func extractJSONObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "```"); k >= 0 {
			rest = rest[:k]
		}
		if t := strings.TrimSpace(rest); strings.HasPrefix(t, "{") {
			s = t
		}
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内不数括号
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// —— 系统提示词 ——
//
// 提示词是骨架的一部分（它定义了模型该输出什么），因此内置且固定。它做三件事：
//  1. 把闭集**完整列出并逐条解释**——这是准确率的主要来源；
//  2. 明确 `unknown` 是合法且被鼓励的答案（防「被迫猜」）；
//  3. 给出 severity 的档位刻度，让不同模型的输出可比（虽然仍不可完全比）。
const judgeSystemPrompt = `你是行为风险判断器。你只输出一个 JSON 对象，不输出任何其它文字。

字段：
- "category": 以下闭集之一，必须原样使用：
    "unknown"        = 你无法确定这个动作的性质。**判不了时必须用它，不要猜。**
    "benign"         = 明确无害
    "pii"            = 内容本身就是个人敏感信息
    "credential"     = 触碰凭证/密钥（.ssh、.aws、.env、id_rsa、credentials.json 等）
    "bulk_read"      = 批量读取或递归遍历（读 .git、grep -r、find 全盘）
    "archive"        = 打包/归档动作（tar、zip、git archive、7z）
    "network_egress" = 把数据发往本机之外的地址（curl/wget/scp/rsync/ssh）
    "exfil"          = 把本机数据往外送：打包或读凭证之后紧接着外发
    "destructive"    = 删改类破坏动作（rm -rf、mkfs、git reset --hard）
- "severity": 0 到 1 之间的数字，按下表取值，不要取中间值：
    0    = 无风险
    0.6  = 可疑，需要人看一眼
    0.9  = 明确高危
    0.95 = 多个危险信号叠加（例如既打包又外发）
- "confidence": 0 到 1 之间的数字，表示你对自己这次判断的把握。
- "reasons": 最多 3 条短字符串，说明你依据的是命令里的哪个部分。

规则：
1. 只根据给出的动作本身判断，不要推测用户意图或后续步骤。
2. 命令是打包/外发等正常运维也可能做的事。只有它同时具备「覆盖范围大」
   （例如目标是 . / 家目录 / 整个仓库）或「目标凭证」或「发往外部地址」时，
   才给高危。单独一次 tar 备份到本目录属于 benign 或 bulk_read。
3. 拿不准就用 "unknown" —— 说不知道是允许的，猜错才是问题。

输出示例：{"category":"archive","severity":0.9,"confidence":0.85,"reasons":["tar 打包当前目录","输出到文件名而非 stdout"]}`

// OpenAICompat 任意 OpenAI 兼容服务后端（Ollama / llama.cpp server / vLLM /
// LM Studio / LocalAI / Xinference ...）。
type OpenAICompat struct {
	name          string
	baseURL       string
	model         string
	schemaMode    types.SchemaMode
	timeout       time.Duration
	maxInputBytes int
	client        *http.Client
}

// OpenAIOptions 构造参数。
type OpenAIOptions struct {
	Name          string
	BaseURL       string
	Model         string
	SchemaMode    types.SchemaMode
	Timeout       time.Duration
	MaxInputBytes int
	// Client 允许注入（测试用 httptest 假服务）；nil 时用内置的本地客户端。
	Client *http.Client
}

// NewOpenAICompat 构造 OpenAI 兼容后端。
func NewOpenAICompat(o OpenAIOptions) *OpenAICompat {
	mode := o.SchemaMode
	if mode == "" {
		mode = types.SchemaPromptOnly
	}
	maxIn := o.MaxInputBytes
	if maxIn <= 0 {
		maxIn = defaultMaxInputBytes
	}
	client := o.Client
	if client == nil {
		client = newLocalHTTPClient(o.Timeout)
	}
	return &OpenAICompat{
		name: o.Name, baseURL: strings.TrimRight(o.BaseURL, "/"), model: o.Model,
		schemaMode: mode, timeout: o.Timeout, maxInputBytes: maxIn, client: client,
	}
}

func (o *OpenAICompat) Name() string { return o.name }

// Capabilities 声明。
//
// GivesConfidence=true 是**保守**选择：它让 Mapper 启用置信地板（< 0.5 → review）。
// 本地模型的置信度普遍不可信，这个开关的意义是「不让一个没把握的结论触发动作」。
// Deterministic=false 是诚实的：即使 temperature=0，浮点与批处理也会让同一输入
// 在不同次运行间产生细微差异，不能对外宣称「同输入必同动作」。
func (o *OpenAICompat) Capabilities() types.Capabilities {
	return types.Capabilities{
		Categories:      nil, // 闭集由提示词约束，模型理论上可输出任意类别
		SchemaModes:     []types.SchemaMode{o.schemaMode},
		GivesConfidence: true,
		MaxInputBytes:   o.maxInputBytes,
		Deterministic:   false,
	}
}

// Health 轻量探测：只做 TCP 连通性检查，不发起推理请求。
//
// 为什么不在 Health 里推理：Health 会被周期性调用，每次推理都会占满一个
// 推理槽（本地模型通常串行），把真正需要判定的请求挤掉。
// 「模型是否真的能判」属于能力自检，走 Probe（probe.go），不在这里。
func (o *OpenAICompat) Health(ctx context.Context) error {
	u, err := parseEndpoint(o.baseURL)
	if err != nil {
		return unavailable(o.name, err)
	}
	ctx, cancel := withTimeout(ctx, o.timeout)
	defer cancel()
	conn, err := dialTCP(ctx, u)
	if err != nil {
		return unavailable(o.name, err)
	}
	_ = conn.Close()
	return nil
}

// Judge 发起一次判定。
func (o *OpenAICompat) Judge(ctx context.Context, d types.ActionDescriptor) (types.Evidence, error) {
	if err := d.Validate(); err != nil {
		return types.Evidence{}, err
	}
	if n := d.InputBytes(); n > o.maxInputBytes {
		return types.Evidence{}, fmt.Errorf("%w: %s: %d > %d", ErrInputTooLarge, o.name, n, o.maxInputBytes)
	}

	start := time.Now()
	ctx, cancel := withTimeout(ctx, o.timeout)
	defer cancel()

	body, err := json.Marshal(o.buildRequest(d))
	if err != nil {
		return types.Evidence{}, invalidOutput(o.name, err)
	}

	url := o.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return types.Evidence{}, unavailable(o.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return types.Evidence{}, unavailable(o.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return types.Evidence{}, unavailable(o.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return types.Evidence{}, unavailable(o.name,
			fmt.Errorf("http %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200)))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return types.Evidence{}, invalidOutput(o.name, fmt.Errorf("decode chat response: %v", err))
	}
	if parsed.Error != nil {
		return types.Evidence{}, unavailable(o.name, errors.New(truncate(parsed.Error.Message, 200)))
	}
	if len(parsed.Choices) == 0 {
		return types.Evidence{}, invalidOutput(o.name, errors.New("response has no choices"))
	}

	ev, err := o.parseContent(parsed.Choices[0].Message.Content)
	if err != nil {
		return types.Evidence{}, err
	}
	ev.LatencyMs = time.Since(start).Milliseconds()
	return ev, nil
}

// parseContent 把模型输出的文本解析成 Evidence。
func (o *OpenAICompat) parseContent(content string) (types.Evidence, error) {
	obj, ok := extractJSONObject(content)
	if !ok {
		return types.Evidence{}, invalidOutput(o.name,
			fmt.Errorf("no json object in model output: %s", truncate(strings.TrimSpace(content), 200)))
	}
	var out modelOutput
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return types.Evidence{}, invalidOutput(o.name, fmt.Errorf("decode model json: %v", err))
	}
	ev, err := out.toEvidence(o.name)
	if err != nil {
		return types.Evidence{}, invalidOutput(o.name, err)
	}
	return ev, nil
}

// buildRequest 组装 chat/completions 请求体。
func (o *OpenAICompat) buildRequest(d types.ActionDescriptor) map[string]any {
	req := map[string]any{
		"model":       o.model,
		"temperature": 0, // 判定要稳定，不要创造性
		"max_tokens":  maxModelOutputTokens,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "system", "content": judgeSystemPrompt},
			{"role": "user", "content": describeAction(d)},
		},
	}
	switch o.schemaMode {
	case types.SchemaJSONSchema:
		req["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "verdict",
				"strict": true,
				"schema": verdictJSONSchema(),
			},
		}
	case types.SchemaGBNF:
		// llama.cpp server 的 /v1/chat/completions 支持 grammar 字段。
		req["grammar"] = verdictGBNF()
	case types.SchemaFormat:
		req["response_format"] = map[string]any{"type": "json_object"}
	case types.SchemaPromptOnly:
		// 不加约束：模型全靠提示词自觉。解析失败会走 fail-safe（降级或 review）。
	}
	return req
}

// describeAction 把 ActionDescriptor 渲染成给模型看的一段说明。
//
// 只给判断需要的东西：工具、命令、观测位。不给会话历史、不给用户身份——
// 判断层不需要它们，多给只会引入偏差。
func describeAction(d types.ActionDescriptor) string {
	var b strings.Builder
	b.WriteString("待判断的动作：\n")
	if d.Tool != "" {
		b.WriteString("工具：" + truncate(d.Tool, 64) + "\n")
	}
	if cmd := d.CommandLine(); cmd != "" {
		b.WriteString("命令：" + truncate(cmd, 4096) + "\n")
	}
	if d.Target != "" && d.Target != d.CommandLine() {
		b.WriteString("目标：" + truncate(d.Target, 256) + "\n")
	}
	switch d.Phase {
	case types.PhaseProposal:
		b.WriteString("时序：proposal（该动作尚未执行，正在被提议）\n")
	case types.PhaseExecuted:
		b.WriteString("时序：executed（该动作已经发生）\n")
	}
	if len(d.Meta) > 0 {
		keys := make([]string, 0, len(d.Meta))
		for k := range d.Meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(k + "：" + truncate(d.Meta[k], 128) + "\n")
		}
	}
	b.WriteString("\n请输出 JSON。")
	return b.String()
}

// verdictJSONSchema 约束解码用的 JSON Schema（闭集锁死）。
func verdictJSONSchema() map[string]any {
	cats := make([]string, 0, len(types.AllCategories()))
	for _, c := range types.AllCategories() {
		cats = append(cats, string(c))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"category":   map[string]any{"type": "string", "enum": cats},
			"severity":   map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"reasons":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 3},
		},
		"required":             []string{"category", "severity", "confidence"},
		"additionalProperties": false,
	}
}

// verdictGBNF llama.cpp 语法：把 category 锁进闭集，数值限定为小数。
//
// 不上 JSON Schema 的运行时（老版本 llama.cpp）靠它兜底。文法刻意写得宽松
// （字段顺序自由、reasons 可选），因为过严的文法会让模型在生成中途卡住，
// 反而更容易输出半截 JSON。
func verdictGBNF() string {
	var b strings.Builder
	b.WriteString("root ::= \"{\" ws member (ws \",\" ws member)* ws \"}\"\n")
	b.WriteString("member ::= cat | sev | conf | reasons\n")
	b.WriteString("cat ::= \"\\\"category\\\"\" ws \":\" ws catval\n")
	b.WriteString("catval ::= ")
	for i, c := range types.AllCategories() {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString("\"\\\"" + string(c) + "\\\"\"")
	}
	b.WriteString("\n")
	b.WriteString("sev ::= \"\\\"severity\\\"\" ws \":\" ws num\n")
	b.WriteString("conf ::= \"\\\"confidence\\\"\" ws \":\" ws num\n")
	b.WriteString("reasons ::= \"\\\"reasons\\\"\" ws \":\" ws \"[\" ws str (ws \",\" ws str)* ws \"]\"\n")
	b.WriteString("num ::= (\"0\" | \"1\") (\".\" [0-9]+)?\n")
	b.WriteString("str ::= \"\\\"\" [^\"\\\\]* \"\\\"\"\n")
	b.WriteString("ws ::= [ \\t\\n]*\n")
	return b.String()
}

// dialTCP 建立一条 TCP 连接，供 Health 的连通性探测使用。
func dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// parseEndpoint 把 base_url 拆成 host:port 供连通性探测。
func parseEndpoint(raw string) (string, error) {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "", fmt.Errorf("empty endpoint")
	}
	if !strings.Contains(s, ":") {
		s += ":80"
	}
	return s, nil
}
