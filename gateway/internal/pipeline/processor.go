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

// Store 把一次请求的全部映射条目写入 vault（按 RequestID 还原）。
func (p *Processor) Store(reqID, convID string, entries []types.MappingEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tbl := &types.MappingTable{
		RequestID:      reqID,
		ConversationID: convID,
		Entries:        entries,
		CreatedAt:      time.Now(),
	}
	if err := p.vault.Put(tbl); err != nil {
		return gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "store mapping", err)
	}
	if p.m != nil {
		p.m.VaultSize.Set(float64(p.vault.Len()))
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
