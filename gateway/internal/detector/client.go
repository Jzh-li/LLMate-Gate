// Package detector 封装 PII 检测能力（契约 §2）。
//
// 设计要点：检测与代理解耦（技术方案 §2 原则 5）——代理层只依赖 Client 接口，
// 底层实现可在「内置中文正则引擎」与「PII Engineer sidecar」之间替换。
package detector

import (
	"context"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"
)

// Client 是检测引擎的客户端抽象（契约 §2.2）。
type Client interface {
	// Detect 对单段文本做 PII 检测。
	Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error)

	// DetectBatch 批量检测，顺序与输入一致。
	DetectBatch(ctx context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error)

	// Health 健康检查。
	Health(ctx context.Context) error

	// Name 引擎名称，写入审计日志。
	Name() string
}

// Sidecar 管理 PII Engineer 子进程（契约 §2.2）。
type Sidecar interface {
	Start() error
	Stop() error
	WaitHealthy(timeout time.Duration) error
}

// Option 配置检测客户端。
type Option func(*options)

type options struct {
	thresholds  map[string]float64
	defaultThr  float64
	enabledOnly map[string]bool
}

// WithThresholds 设置 per-type 置信度阈值（契约 §2.4）。
func WithThresholds(m map[string]float64) Option {
	return func(o *options) { o.thresholds = m }
}

// WithEntityFilter 限定只检测给定实体类型；空表示全部。
func WithEntityFilter(entities []string) Option {
	return func(o *options) {
		if len(entities) == 0 {
			return
		}
		o.enabledOnly = make(map[string]bool, len(entities))
		for _, e := range entities {
			o.enabledOnly[e] = true
		}
	}
}

func newOptions(opts ...Option) *options {
	o := &options{defaultThr: 0.5, thresholds: map[string]float64{}}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func (o *options) thresholdFor(t string) float64 {
	if v, ok := o.thresholds[t]; ok {
		return v
	}
	return o.defaultThr
}

// filterAndSort 统一后置处理：按阈值过滤 + 按 start 升序 + 去重重叠。
//
// 契约 §2.1 要求 entities 按 start 升序；重叠区间保留分数更高者，
// 避免替换阶段出现区间交叉导致文本错乱。
func filterAndSort(entities []types.Entity, o *options) []types.Entity {
	out := make([]types.Entity, 0, len(entities))
	for _, e := range entities {
		if o.enabledOnly != nil && !o.enabledOnly[e.Type] {
			continue
		}
		if e.Score < o.thresholdFor(e.Type) {
			continue
		}
		if e.Start < 0 || e.End > len(e.Value)+e.Start {
			// 偏移非法：宁可丢弃，也不要在替换阶段 panic。
			continue
		}
		out = append(out, e)
	}
	// 插入排序即可：模型输出通常近乎有序。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Start < out[j-1].Start ||
			(out[j].Start == out[j-1].Start && out[j].End > out[j-1].End)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	// 去掉重叠：保留分数高的
	res := make([]types.Entity, 0, len(out))
	for _, e := range out {
		if len(res) > 0 && e.Start < res[len(res)-1].End {
			if e.Score > res[len(res)-1].Score {
				res[len(res)-1] = e
			}
			continue
		}
		res = append(res, e)
	}
	return res
}

// ErrUnavailable 快速构造"检测引擎不可用"错误，供 fail-closed 使用。
func ErrUnavailable(cause error) error {
	return gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "detector unavailable", cause)
}
