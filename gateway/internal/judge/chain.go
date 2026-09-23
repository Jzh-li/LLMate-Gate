package judge

import (
	"context"
	"fmt"
	"strings"

	"gateway/pkg/types"
)

// Chain 组合多个后端，按序询问、首个给出结论者胜出（契约 §12 后端矩阵 kind=chain）。
//
// ⚠️ 语义是**降级链，不是优先级链**。顺序应按「语义能力降序」排列：用户自选的
// 模型在前，确定性 rules 在最后兜底。区别在于「首个命中」的定义——
//
//	优先级链：取第一个后端的结论，无论它说什么；
//	降级链：  取第一个**给出非 unknown 结论**的后端，unknown 视为「没答」继续往下问。
//
// 这个区别是 D3 的落点：如果 unknown 也算命中，那么一个恒返回 unknown 的坏模型
// 会把整条链堵死，后面的 rules 永远轮不到。
type Chain struct {
	backends []Judge
}

// NewChain 构造降级链。
func NewChain(backends ...Judge) *Chain {
	return &Chain{backends: backends}
}

// Names 后端名（按降级顺序）。
func (c *Chain) Names() []string {
	out := make([]string, 0, len(c.backends))
	for _, b := range c.backends {
		out = append(out, b.Name())
	}
	return out
}

// Name 链的标识（进指标与日志）。
func (c *Chain) Name() string {
	return "chain[" + strings.Join(c.Names(), ",") + "]"
}

// Capabilities 链的能力声明 = 各后端能力的并集。
//
// 用于面板展示与「这条链整体能判什么」的粗判；真正的 per-backend 能力判断发生在
// 各后端内部（Chain 不做能力过滤——它靠 unknown 语义自然表达「这个后端答不了」）。
func (c *Chain) Capabilities() types.Capabilities {
	cap := types.Capabilities{Deterministic: true}
	// 任一后端声明「支持全部类别」（Categories 为空即此意）→ 整体也视为支持全部。
	supportsAllCats := false
	catSet := map[types.ActionCategory]bool{}
	modeSet := map[types.SchemaMode]bool{}
	for _, b := range c.backends {
		bc := b.Capabilities()
		if !bc.Deterministic {
			cap.Deterministic = false
		}
		if bc.GivesConfidence {
			cap.GivesConfidence = true
		}
		if bc.MaxInputBytes > cap.MaxInputBytes {
			cap.MaxInputBytes = bc.MaxInputBytes
		}
		if len(bc.Categories) == 0 {
			supportsAllCats = true
		} else {
			for _, x := range bc.Categories {
				catSet[x] = true
			}
		}
		for _, x := range bc.SchemaModes {
			modeSet[x] = true
		}
	}
	if !supportsAllCats {
		for _, x := range types.AllCategories() {
			if catSet[x] {
				cap.Categories = append(cap.Categories, x)
			}
		}
	}
	for _, x := range types.AllSchemaModes() {
		if modeSet[x] {
			cap.SchemaModes = append(cap.SchemaModes, x)
		}
	}
	return cap
}

// Health 链是否至少有一个后端可用。
//
// 只要有一个能答，链就不算挂——这正是「rules 永不缺席」在健康检查上的体现。
func (c *Chain) Health(ctx context.Context) error {
	var lastErr error
	for _, b := range c.backends {
		if err := b.Health(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return lastErr
}

// Judge 实现 Judge 接口（丢弃 degraded 细节）。
func (c *Chain) Judge(ctx context.Context, d types.ActionDescriptor) (types.Evidence, error) {
	ev, _, err := c.judgeWithMeta(ctx, d)
	return ev, err
}

// judgeWithMeta 按降级链求证据，并报告该结论是否来自降级。
//
// 返回 error 的唯一条件是**所有后端都失败**（网络/解析/超时）。单条描述判不出来
// 不算失败：那是 Category=unknown，会作为正常结论返回，由 Mapper 转成 review。
func (c *Chain) judgeWithMeta(ctx context.Context, d types.ActionDescriptor) (types.Evidence, bool, error) {
	var firstErr error
	// last 记录「最后一个给出了结论（含 unknown）的后端」的证据：
	// 全部 unknown 时它是唯一能说明「谁看过了、看到了什么」的线索。
	var last types.Evidence
	hasLast := false

	for i, b := range c.backends {
		ev, err := b.Judge(ctx, d)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ev.Engine == "" {
			ev.Engine = b.Name()
		}
		if ev.Category == types.CatUnknown {
			last, hasLast = ev, true
			continue
		}
		// 命中：首个给出非 unknown 结论的后端。
		// i > 0 说明前面有后端没给出结论，本次裁决来自降级。
		return ev, i > 0, nil
	}

	if hasLast {
		return last, true, nil
	}
	if firstErr == nil {
		// 空链：没有任何后端能给出结论。这不是「没命中」，而是「这一层不存在」。
		firstErr = fmt.Errorf("%w: chain has no backends", ErrUnavailable)
	}
	return types.Evidence{}, false, firstErr
}
