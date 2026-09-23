package judge

import (
	"context"
	"fmt"
	"strings"

	"gateway/pkg/types"
)

// LowConfidenceFloor 置信度地板。
//
// 声明了 GivesConfidence 的后端，若单次结论的 confidence 低于该值，则该结论
// **不参与任何档位判定**，直接交人复核。理由是两个方向都要防：
//
//	不可信的高危结论 → 若放行到 block，就是拿不可靠部件做不可逆操作；
//	不可信的低危结论 → 若放行到 allow，等于用「我没把握」当作「没事」。
//
// 两侧都交人是唯一安全的落点，代价只是多几条 review。
const LowConfidenceFloor = 0.50

// Mapper 把 Evidence 确定性映射为 Action（契约 §7 核心红线的实现）。
//
// 纯函数 + 不可变配置：给定同一 Evidence 与同一阈值表，输出必定相同。
// 这是「同输入必同 action」可进 CI 回归的技术基础，也是模型永远拿不到
// block 权限的技术基础——Evidence 类型里根本没有 Action 字段。
type Mapper struct {
	engine          string
	thresholds      types.JudgmentThresholds
	givesConfidence bool
}

// NewMapper 构造 Mapper。engine 用于填充兜底 Evidence 的来源标签。
func NewMapper(engine string, th types.JudgmentThresholds, givesConfidence bool) *Mapper {
	return &Mapper{engine: engine, thresholds: th, givesConfidence: givesConfidence}
}

// Thresholds 返回该 Mapper 使用的阈值表（只读用途，如面板展示）。
func (m *Mapper) Thresholds() types.JudgmentThresholds { return m.thresholds }

// Map 计算最终裁决。
//
// 判定顺序（前两条是「不落档」的短路，第三条才做数值映射）：
//
//  1. category == unknown —— 后端自认判不了 → review（交人，不是放行）
//  2. confidence 低于地板 —— 结论不可信     → review（两个方向都防）
//  3. score = severity（若 GivesConfidence 则再乘 confidence）落阈值表
//
// 注意 benign 不做特例：它只影响统计与探针，不影响动作档位。否则「benign +
// 高 severity」这种自相矛盾的输出会被静默放行——取更严的一侧才是 fail-safe。
func (m *Mapper) Map(ev types.Evidence) types.Verdict {
	if ev.Engine == "" {
		ev.Engine = m.engine
	}
	if ev.Category == types.CatUnknown {
		return types.Verdict{Action: types.ActionReview, Evidence: ev}
	}
	score := ev.Severity
	if m.givesConfidence {
		if ev.Confidence < LowConfidenceFloor {
			return types.Verdict{Action: types.ActionReview, Evidence: ev}
		}
		score = ev.Severity * ev.Confidence
	}
	switch {
	case score >= m.thresholds.Block:
		return types.Verdict{Action: types.ActionBlock, Evidence: ev}
	case score >= m.thresholds.Review:
		return types.Verdict{Action: types.ActionReview, Evidence: ev}
	case score >= m.thresholds.Redact:
		return types.Verdict{Action: types.ActionRedact, Evidence: ev}
	default:
		return types.Verdict{Action: types.ActionAllow, Evidence: ev}
	}
}

// Whitelist 先于一切判断的放行名单（方案 §8：默认拒绝 + 白名单，ZCode 教训）。
//
// 白名单命中即 allow 且不进后端——「agent 天天读 .git」这类正常开发动作必须
// 有确定性出口，否则规则后端一定会把它判成 bulk_read 而抬高噪声。
type Whitelist struct {
	// Hosts 允许的目标主机（精确匹配，或以 "." 开头表示后缀匹配）。
	Hosts []string
	// Tools 允许的工具名（精确匹配）。
	Tools []string
}

// Hit 该描述是否命中白名单；命中时返回命中的条目标签（便于审计）。
func (w Whitelist) Hit(d types.ActionDescriptor) (string, bool) {
	for _, t := range w.Tools {
		if t != "" && t == d.Tool {
			return "tool:" + t, true
		}
	}
	for _, h := range w.Hosts {
		if h == "" {
			continue
		}
		if matchHost(d.Target, h) {
			return "host:" + h, true
		}
	}
	return "", false
}

// matchHost 主机名匹配：剥端口后精确比对；条目以 "." 开头时做后缀匹配。
func matchHost(target, pattern string) bool {
	host := target
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(host, pattern) || host == strings.TrimPrefix(pattern, ".")
	}
	return host == pattern
}

// Evaluator 判断层的唯一对外入口：白名单 → 降级链 → per-backend Mapper。
//
// 它把「顺序」与「阈值」这两件确定性的事固定下来，让后端只负责出证据。
type Evaluator struct {
	chain     *Chain
	mappers   map[string]*Mapper
	fallback  *Mapper
	whitelist Whitelist
	// strict 为 true 时，白名单/后端都不适用一律走 review（fail-closed）。
	strict bool
}

// EvaluatorOptions 构造参数。
type EvaluatorOptions struct {
	Whitelist Whitelist
	// FailClosed 后端全部失败时的姿态：true → review，false → allow。
	// 与 policy.fail_closed 同语义，由 config.judgment.fail_closed 投影。
	FailClosed bool
}

// NewEvaluator 由一组后端构造求值器。backends 顺序即降级顺序（可靠者在前）。
//
// 每个后端按自己的阈值表与能力声明各建一个 Mapper——per-backend 阈值在这里落地。
func NewEvaluator(backends []Judge, opts EvaluatorOptions) (*Evaluator, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("judge: evaluator needs at least one backend")
	}
	mappers := make(map[string]*Mapper, len(backends))
	for _, b := range backends {
		name := b.Name()
		if name == "" {
			return nil, fmt.Errorf("judge: backend name must not be empty")
		}
		if _, dup := mappers[name]; dup {
			return nil, fmt.Errorf("judge: duplicate backend name %q", name)
		}
		// 缺省：保守阈值 + 按后端自身的能力声明决定 confidence 是否参与加权。
		// 具体阈值由装配层用 SetMapper 按配置覆盖（per-backend）。
		mappers[name] = NewMapper(name, types.DefaultThresholds(), b.Capabilities().GivesConfidence)
	}
	return &Evaluator{
		chain:     NewChain(backends...),
		mappers:   mappers,
		fallback:  NewMapper("default", types.DefaultThresholds(), false),
		whitelist: opts.Whitelist,
		strict:    opts.FailClosed,
	}, nil
}

// SetMapper 为一个后端登记专属的 Mapper（阈值表 + confidence 语义）。
//
// 分离于构造函数，是为了让「后端怎么构造」与「阈值从哪来」各自独立：
// 前者由 Spec 决定，后者由配置决定，测试可以只换后者。
func (e *Evaluator) SetMapper(engine string, th types.JudgmentThresholds, givesConfidence bool) *Evaluator {
	e.mappers[engine] = NewMapper(engine, th, givesConfidence)
	return e
}

// mapperFor 取后端专属 Mapper；未登记则用兜底（默认阈值 + 不做 confidence 加权）。
func (e *Evaluator) mapperFor(engine string) *Mapper {
	if m, ok := e.mappers[engine]; ok {
		return m
	}
	return e.fallback
}

// Capabilities 暴露降级链的能力声明（链的能力 = 各后端能力的并集）。
func (e *Evaluator) Capabilities() types.Capabilities { return e.chain.Capabilities() }

// Backends 后端名列表（按降级顺序）。
func (e *Evaluator) Backends() []string { return e.chain.Names() }

// Evaluate 得到最终裁决。
//
// 返回 error 只发生在「链里所有后端都失败」这一种情况——此时调用方按
// FailClosed 决定姿态（e.FallbackVerdict 给的就是这个映射）。
// 单条描述判不出来 **不是** error：那是 CatUnknown，会正常走到 review。
func (e *Evaluator) Evaluate(ctx context.Context, d types.ActionDescriptor) (types.Verdict, error) {
	if err := d.Validate(); err != nil {
		return types.Verdict{}, err
	}
	if label, ok := e.whitelist.Hit(d); ok {
		return types.Verdict{
			Action: types.ActionAllow,
			Evidence: types.Evidence{
				Category: types.CatBenign,
				Engine:   "whitelist",
				Reasons:  []string{"whitelist hit " + label},
			},
		}, nil
	}
	ev, degraded, err := e.chain.judgeWithMeta(ctx, d)
	if err != nil {
		return types.Verdict{}, err
	}
	v := e.mapperFor(ev.Engine).Map(ev)
	v.Degraded = degraded
	return v, nil
}

// FallbackVerdict 后端全部不可用时的裁决。
//
// fail_closed=true → review（交人）；false → allow（放行，但会带 reasons 说明）。
// 注意这里**没有**「静默放行」这一档：即便 fail_closed=false，审计里也一定留下
// engine=fallback 的记录，不会让「判断层没跑起来」这件事无声无息。
func (e *Evaluator) FallbackVerdict(err error) types.Verdict {
	reason := "judge layer unavailable"
	if err != nil {
		reason += ": " + err.Error()
	}
	action := types.ActionReview
	if !e.strict {
		action = types.ActionAllow
	}
	return types.Verdict{
		Action: action,
		Evidence: types.Evidence{
			Category: types.CatUnknown,
			Engine:   "fallback",
			Reasons:  []string{reason},
		},
	}
}

// FailClosed 报告当前姿态（调用方据此决定要不要把 FallbackVerdict 当成失败处理）。
func (e *Evaluator) FailClosed() bool { return e.strict }
