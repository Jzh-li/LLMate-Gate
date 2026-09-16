// Package pipeline 串联检测→替换→映射表→还原 的请求处理流程（契约 §4.1）。
//
// 进程内编排层：server 只负责 HTTP 与上游转发，pipeline 负责「对一段文本脱敏」
// 与「按 RequestID 还原一段文本」这两个原子能力，并写入审计与发布调试事件。
package pipeline

import (
	"context"
	"errors"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/internal/audit"
	"gateway/internal/cache"
	"gateway/internal/detector"
	"gateway/internal/metrics"
	"gateway/internal/replacer"
	"gateway/internal/vault"
	"gateway/pkg/types"
)

// EventPublisher 非阻塞调试事件发布（internal/debug 实现；nil 安全）。
type EventPublisher interface {
	Publish(eventType string, data interface{})
}

// NopPublisher 默认空实现：debug 关闭时由 main 注入，避免每处判 nil。
type NopPublisher struct{}

// Publish 空实现。
func (NopPublisher) Publish(string, interface{}) {}

// Processor 请求处理编排器。
type Processor struct {
	det       detector.Client
	repl      replacer.Replacer
	vault     vault.Vault
	dc        cache.Cache
	audit     *audit.Logger
	m         *metrics.Collectors
	pub       EventPublisher
	useCache  bool
	failClosed bool
}

// Config 编排器配置。
type Config struct {
	Detector   detector.Client
	Replacer   replacer.Replacer
	Vault      vault.Vault
	Cache      cache.Cache
	Audit      *audit.Logger
	Metrics    *metrics.Collectors
	Publisher  EventPublisher
	UseCache   bool
	FailClosed bool
}

// New 构造编排器。
func New(cfg Config) *Processor {
	return &Processor{
		det:       cfg.Detector,
		repl:      cfg.Replacer,
		vault:     cfg.Vault,
		dc:        cfg.Cache,
		audit:     cfg.Audit,
		m:         cfg.Metrics,
		pub:       cfg.Publisher,
		useCache:  cfg.UseCache,
		failClosed: cfg.FailClosed,
	}
}

// Anonymize 对单段文本做「检测 + 替换」，返回脱敏文本与映射条目。
//
// 不写 vault（一次请求可能含多段，由调用方统一 Store）。
// fail_closed=false 时检测失败则原文透传（放行）；否则返回错误由 server 阻断。
func (p *Processor) Anonymize(ctx context.Context, reqID, convID, text string) (clean string, entries []types.MappingEntry, detMs int64, err error) {
	if text == "" {
		return "", nil, 0, nil
	}
	var resp *types.DetectResponse
	if p.useCache && p.dc != nil && convID != "" {
		if r, ok := p.dc.Get(convID, cache.HashText(text)); ok {
			resp = r
			if p.m != nil {
				p.m.CacheHits.WithLabelValues(convID).Inc()
			}
			p.publish(EvDetectionDone, DetectionPayload{RequestID: reqID, Cached: true, Entities: r.Entities})
		}
	}
	if resp == nil {
		start := time.Now()
		r, derr := p.det.Detect(ctx, &types.DetectRequest{Text: text, ConversationID: convID})
		detMs = time.Since(start).Milliseconds()
		if p.m != nil {
			p.m.DetectLatency.WithLabelValues(p.det.Name()).Observe(time.Since(start).Seconds())
		}
		if derr != nil {
			if !p.failClosed {
				return text, nil, detMs, nil // 放行
			}
			return "", nil, detMs, classifyDetectErr(derr)
		}
		resp = r
		if p.useCache && p.dc != nil && convID != "" {
			_ = p.dc.Put(convID, cache.HashText(text), resp)
			if p.m != nil {
				p.m.CacheMisses.WithLabelValues(convID).Inc()
			}
		}
		p.publish(EvDetectionDone, DetectionPayload{RequestID: reqID, Cached: false, Entities: resp.Entities})
	}

	rr, rerr := p.repl.Replace(ctx, &replacer.ReplaceRequest{
		Text:           text,
		RequestID:      reqID,
		ConversationID: convID,
		Entities:       resp.Entities,
	})
	if rerr != nil {
		if !p.failClosed {
			return text, nil, detMs, nil
		}
		return "", nil, detMs, gatewayerrors.Wrap(gatewayerrors.CodeReplaceFailed, "replace", rerr)
	}
	if p.m != nil {
		for _, e := range rr.Entries {
			p.m.ReplaceCount.WithLabelValues(e.Fate.String()).Inc()
		}
	}
	p.publish(EvReplacementDone, ReplacementPayload{RequestID: reqID, Replaced: rr.Text, Count: len(rr.Entries)})
	return rr.Text, rr.Entries, detMs, nil
}

// DetectText 仅做检测（带 per-text LRU 缓存 + 指标 + fail-closed），返回实体。
//
// 用于 Merkle 增量路径：proxy 在会话内对每段文本调用，命中 Merkle 前缀的旧段根本不会
// 进入此函数（零检测），只有新增尾部段会真正送检测器（契约 §10.3）。
func (p *Processor) DetectText(ctx context.Context, convID, text string) ([]types.Entity, error) {
	if text == "" {
		return nil, nil
	}
	var resp *types.DetectResponse
	if p.useCache && p.dc != nil && convID != "" {
		if r, ok := p.dc.Get(convID, cache.HashText(text)); ok {
			resp = r
			if p.m != nil {
				p.m.CacheHits.WithLabelValues(convID).Inc()
			}
			p.publish(EvDetectionDone, DetectionPayload{RequestID: "", Cached: true, Entities: r.Entities})
			return r.Entities, nil
		}
	}
	start := time.Now()
	r, derr := p.det.Detect(ctx, &types.DetectRequest{Text: text, ConversationID: convID})
	if p.m != nil {
		p.m.DetectLatency.WithLabelValues(p.det.Name()).Observe(time.Since(start).Seconds())
	}
	if derr != nil {
		if !p.failClosed {
			return nil, nil
		}
		return nil, classifyDetectErr(derr)
	}
	resp = r
	if p.useCache && p.dc != nil && convID != "" {
		_ = p.dc.Put(convID, cache.HashText(text), resp)
		if p.m != nil {
			p.m.CacheMisses.WithLabelValues(convID).Inc()
		}
	}
	p.publish(EvDetectionDone, DetectionPayload{RequestID: "", Cached: false, Entities: resp.Entities})
	return resp.Entities, nil
}

// NewSession 构造一次请求内的替换会话（共享占位符计数，保证全局唯一）。
func (p *Processor) NewSession() *replacer.Session {
	return p.repl.NewSession()
}

// Store 写入一次「数据面」请求的映射表；仅供该请求自身的响应还原使用。
//
// 数据面的 request_id 来自客户端指定的 X-Request-ID（见 proxy.requestID），因此
// 这个命名空间对外是**半公开**的——同一台机器上的另一个客户端可能从日志/自身配置里
// 见到它。Origin=OriginData 保证这类表不会被控制面 API 还原。
func (p *Processor) Store(reqID, convID string, entries []types.MappingEntry) error {
	return p.store(reqID, convID, types.OriginData, entries)
}

// StoreControl 写入一次「控制面」/v1/privacy/redact 的映射表，允许后续经
// /v1/privacy/restore 还原。
//
// 与控制面自建 ID（proxy.newRequestID，128 位随机）配套：ID 不可猜 + 来源可校验，
// 两个条件都成立才还原得出原文。
func (p *Processor) StoreControl(reqID, convID string, entries []types.MappingEntry) error {
	return p.store(reqID, convID, types.OriginControl, entries)
}

func (p *Processor) store(reqID, convID, origin string, entries []types.MappingEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tbl := &types.MappingTable{
		RequestID:      reqID,
		ConversationID: convID,
		Entries:        entries,
		CreatedAt:      time.Now(),
		Origin:         origin,
	}
	if err := p.vault.Put(tbl); err != nil {
		return gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "store mapping", err)
	}
	if p.m != nil {
		p.m.VaultSize.Set(float64(p.vault.Len()))
	}
	return nil
}

// AssertRestorableViaAPI 校验 request_id 对应的映射表允许经控制面 API 还原。
//
// 存在的意义是把「按 ID 取原文」的权限从「知道 ID」抬高到「ID 由控制面产生」。
// 在此之前，任何持有控制面令牌的调用方只要拿到另一个请求的 request_id（数据面的
// 那个是客户端可指定的），就能还原出它的原文——一条与「谁能调网关」无关的越权读路径。
//
// 失败一律返回 not_found，不区分「不存在」「已过期」「属于别的调用面」：
// 区分开就等于提供了一个「这个 ID 是否存在」的探针，而调用方本来就不该知道别人
// 的 ID 空间长什么样。
func (p *Processor) AssertRestorableViaAPI(reqID string) error {
	tbl, err := p.vault.Get(reqID)
	if err != nil {
		return err
	}
	if !tbl.IsRestorableViaAPI() {
		return gatewayerrors.ErrNotFound
	}
	return nil
}

// Restore 整包还原（非流式）。
func (p *Processor) Restore(ctx context.Context, reqID, text string) (string, error) {
	out, err := p.repl.Restore(ctx, &replacer.RestoreRequest{Text: text, RequestID: reqID})
	if err != nil {
		return "", gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "restore", err)
	}
	return out, nil
}

// StreamRestorer 构造基于本请求映射表的流式还原器（SSE 用）。
func (p *Processor) StreamRestorer(reqID string) (*replacer.StreamRestorer, error) {
	tbl, err := p.vault.Get(reqID)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeRestoreFailed, "load mapping for stream", err)
	}
	return replacer.NewStreamRestorerFromEntries(tbl.Entries), nil
}

// publish 非阻塞发布调试事件（nil 安全）。
func (p *Processor) publish(eventType string, data interface{}) {
	if p.pub != nil {
		p.pub.Publish(eventType, data)
	}
}

// Publish 对外暴露的非阻塞事件发布（proxy 调用）。
func (p *Processor) Publish(eventType string, data interface{}) {
	p.publish(eventType, data)
}

// Strategy 返回当前替换策略（placeholder | simulate），供审计/调试使用。
func (p *Processor) Strategy() string {
	if p.repl == nil {
		return "placeholder"
	}
	return p.repl.Strategy()
}

// RecordAudit 记录一条审计事件（契约 §9）；audit 为 nil 时安全跳过。
func (p *Processor) RecordAudit(e *audit.Event) {
	if p.audit != nil {
		_ = p.audit.Write(e)
	}
}

// classifyDetectErr 把检测错误映射为统一的阻断错误码（契约 §0.3）。
func classifyDetectErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return gatewayerrors.Wrap(gatewayerrors.CodeDetectorTimeout, "detect", err)
	}
	if e, ok := gatewayerrors.As(err); ok {
		switch e.Code {
		case gatewayerrors.CodeCircuitOpen, gatewayerrors.CodeDetectorTimeout:
			return err
		}
	}
	return gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "detect", err)
}
