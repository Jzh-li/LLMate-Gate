package registry

import (
	"context"

	"gateway/internal/detector"
	"gateway/pkg/types"
)

// Wrap 用登记表给内层检测器补召回，返回新的检测客户端。
//
// reg 为 nil 时原样返回内层，调用方不必在外面写 if——「没开登记表」和
// 「登记表为空」因此走的是同一条零成本路径。
//
// 位置很关键：登记命中在**内层检测器之后**合并，所以它不受 per-type 阈值、
// 也不受内层检测器的类型白名单（detector.WithEntityFilter）约束——这正是召回
// 保证的含义，用户显式声明的值不该再被阈值刷掉。同时也意味着它会流经下游的
// 检测缓存，于是登记表变更必须配套刷缓存（见 Registry.OnChange）。
func Wrap(inner detector.Client, reg *Registry) detector.Client {
	if inner == nil || reg == nil {
		return inner
	}
	return &wrapped{inner: inner, reg: reg}
}

type wrapped struct {
	inner detector.Client
	reg   *Registry
}

func (w *wrapped) Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error) {
	resp, err := w.inner.Detect(ctx, req)
	if err != nil {
		return nil, err // 内层错误语义原样上抛，fail-closed 由上层决定
	}
	if req == nil || resp == nil {
		return resp, nil
	}
	hits := w.reg.Scan(req.Text)
	if len(hits) == 0 {
		return resp, nil // 无命中就原样返回，不重建对象、不碰既有行为
	}
	return &types.DetectResponse{
		Entities:  Merge(hits, resp.Entities),
		LatencyMs: resp.LatencyMs,
	}, nil
}

func (w *wrapped) DetectBatch(ctx context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error) {
	resps, err := w.inner.DetectBatch(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for i, resp := range resps {
		if resp == nil || i >= len(reqs) || reqs[i] == nil {
			continue // 内层返回条数对不上时宁可少补，也不越界
		}
		hits := w.reg.Scan(reqs[i].Text)
		if len(hits) == 0 {
			continue
		}
		resps[i] = &types.DetectResponse{
			Entities:  Merge(hits, resp.Entities),
			LatencyMs: resp.LatencyMs,
		}
	}
	return resps, nil
}

func (w *wrapped) Health(ctx context.Context) error { return w.inner.Health(ctx) }

// Name 沿用内层引擎名。
//
// 这个名字会进审计事件与指标标签，改名会平白改变已有指标的取值分布；
// 登记表是叠加在引擎之上的补召回，不算新引擎。
func (w *wrapped) Name() string { return w.inner.Name() }

// 编译期接口断言。
var _ detector.Client = (*wrapped)(nil)
