// Package metrics 定义 LLMate Gate 的 Prometheus 指标（契约 §9 / §5.3）。
//
// ⚠️ 冲突标注（2026-09-10，待裁决，见 SPEC_ALIGNMENT.md C7 / Q7）。
// 本文件指标命名/单位与 Specs/00-整体技术方案.md §14.2 不一致：
// llmate_blocked_total 对应 spec 的 llmate_fail_closed_total；
// llmate_detect_latency_seconds 对应 spec 的 llmate_detection_latency_ms（单位 seconds ≠ ms）；
// 缺失 llmate_pii_detected_total、llmate_tool_calls_scanned_total、
// llmate_request_total_latency_ms、llmate_response_restore_latency_ms。
// 方案①：改代码对齐 spec —— 规范统一，但指标名是公共接口，会破坏已对接的 Grafana/告警。
// 方案②：改 spec 承认现状 —— 零破坏，且 seconds 是 Prometheus 惯例单位，但需补录缺失项定义。
// 当前未改动任何指标名（属公共接口变更，需人工裁决）。
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Collectors 网关运行指标集合（统一在 /metrics 暴露）。
type Collectors struct {
	RequestsTotal       *prometheus.CounterVec
	DetectLatency       *prometheus.HistogramVec
	ReplaceCount        *prometheus.CounterVec
	RestoredTotal       *prometheus.CounterVec
	BlockedTotal        *prometheus.CounterVec
	UpstreamErrors      *prometheus.CounterVec
	StreamOrphans       *prometheus.CounterVec
	CacheHits           *prometheus.CounterVec
	CacheMisses         *prometheus.CounterVec
	// DetectIncremental Merkle 增量缓存：本请求实际「送检测器」vs「复用缓存」的段数。
	DetectIncremental   *prometheus.CounterVec
	VaultSize           prometheus.Gauge
	ActiveConns         prometheus.Gauge
}

// New 构造并注册全部指标。
func New(reg prometheus.Registerer) *Collectors {
	c := &Collectors{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_requests_total",
			Help: "处理的请求总数（按端点与结果）",
		}, []string{"endpoint", "outcome"}),
		DetectLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmate_detect_latency_seconds",
			Help:    "检测引擎耗时分布",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}, []string{"engine"}),
		ReplaceCount: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_replace_total",
			Help: "脱敏替换的实体总数（按命运）",
		}, []string{"fate"}),
		RestoredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_restore_total",
			Help: "响应还原次数（按端点）",
		}, []string{"endpoint"}),
		BlockedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_blocked_total",
			Help: "fail-closed 阻断的请求数（检测异常）",
		}, []string{"reason"}),
		UpstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_upstream_errors_total",
			Help: "上游返回非 2xx 的次数",
		}, []string{"endpoint", "status"}),
		StreamOrphans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_stream_orphan_placeholders_total",
			Help: "流式还原残留的不完整占位符数（契约 §5.3）",
		}, []string{"endpoint"}),
		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_detect_cache_hits_total",
			Help: "检测缓存命中数",
		}, []string{"conversation"}),
		CacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_detect_cache_misses_total",
			Help: "检测缓存未命中数",
		}, []string{"conversation"}),
		DetectIncremental: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_detect_incremental_segments_total",
			Help: "Merkle 增量检测：本请求实际送检测器的段数（detected）vs 复用缓存的段数（reused）",
		}, []string{"action"}),
		VaultSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmate_vault_size",
			Help: "当前存活映射表数量",
		}),
		ActiveConns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "llmate_active_streams",
			Help: "当前活跃的流式连接数",
		}),
	}
	reg.MustRegister(
		c.RequestsTotal, c.DetectLatency, c.ReplaceCount, c.RestoredTotal,
		c.BlockedTotal, c.UpstreamErrors, c.StreamOrphans,
		c.CacheHits, c.CacheMisses, c.DetectIncremental, c.VaultSize, c.ActiveConns,
	)
	return c
}
