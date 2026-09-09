// Package circuit 提供 fail-closed 熔断器（技术方案 §6.1：连续 5 次失败进入半开探测）。
package circuit

import (
	"context"
	"sync"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"

	"gateway/internal/detector"
)

// Breaker 检测调用熔断器：连续失败达阈值后快速失败，冷却后进入半开态探测。
type Breaker struct {
	mu            sync.Mutex
	threshold     int
	cooldown      time.Duration
	consecFails   int
	openedAt      time.Time
	open          bool
	onStateChange func(open bool)
}

// New 构造熔断器；threshold<=0 时取默认 5，cooldown<=0 时取 10s。
func New(threshold int, cooldown time.Duration, onStateChange func(open bool)) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, onStateChange: onStateChange}
}

// IsOpen 当前是否处于熔断态（半开探测时返回 false，允许一次请求通过）。
func (b *Breaker) IsOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open && time.Since(b.openedAt) >= b.cooldown {
		return false // 冷却结束 → 半开，放一个请求探测
	}
	return b.open
}

func (b *Breaker) recordSuccess() {
	b.mu.Lock()
	wasOpen := b.open
	b.consecFails = 0
	b.open = false
	b.mu.Unlock()
	if wasOpen && b.onStateChange != nil {
		b.onStateChange(false)
	}
}

func (b *Breaker) recordFailure() {
	b.mu.Lock()
	wasOpen := b.open
	b.consecFails++
	if b.consecFails >= b.threshold && !b.open {
		b.open = true
		b.openedAt = time.Now()
	}
	b.mu.Unlock()
	if !wasOpen && b.open && b.onStateChange != nil {
		b.onStateChange(true)
	}
}

// Stats 暴露内部状态，供 /healthz 与 /metrics 使用。
func (b *Breaker) Stats() (open bool, consecFails int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open, b.consecFails
}

// GuardedClient 在 detector.Client 之外包一层熔断 + fail-closed 语义。
type GuardedClient struct {
	inner   detector.Client
	breaker *Breaker
}

// NewGuardedClient 包装任意检测客户端。
func NewGuardedClient(inner detector.Client, b *Breaker) *GuardedClient {
	return &GuardedClient{inner: inner, breaker: b}
}

// Name 引擎名称。
func (g *GuardedClient) Name() string { return g.inner.Name() }

// Detect 带熔断的检测；熔断打开时返回 circuit_open（调用方据此 fail-closed 阻断）。
func (g *GuardedClient) Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error) {
	if g.breaker != nil && g.breaker.IsOpen() {
		return nil, gatewayerrors.New(gatewayerrors.CodeCircuitOpen, "detector circuit breaker is open")
	}
	resp, err := g.inner.Detect(ctx, req)
	if g.breaker != nil {
		if err != nil {
			g.breaker.recordFailure()
		} else {
			g.breaker.recordSuccess()
		}
	}
	return resp, err
}

// DetectBatch 带熔断的批量检测。
func (g *GuardedClient) DetectBatch(ctx context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error) {
	if g.breaker != nil && g.breaker.IsOpen() {
		return nil, gatewayerrors.New(gatewayerrors.CodeCircuitOpen, "detector circuit breaker is open")
	}
	resp, err := g.inner.DetectBatch(ctx, reqs)
	if g.breaker != nil {
		if err != nil {
			g.breaker.recordFailure()
		} else {
			g.breaker.recordSuccess()
		}
	}
	return resp, err
}

// Health 健康检查（不参与熔断计数）。
func (g *GuardedClient) Health(ctx context.Context) error { return g.inner.Health(ctx) }
