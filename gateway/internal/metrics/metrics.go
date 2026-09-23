// Package metrics 定义 LLMate Gate 的 Prometheus 指标（契约 §9 / §5.3）。
//
// ✅ 2026-09-11 已裁决（SPEC_ALIGNMENT.md C7 / Q7）：采用「方案②——改 spec 承认现状」。
// 本文件的指标名/单位为权威口径（Specs/00 §14.2 已回写同步），不再变更：
//   - llmate_blocked_total 即 spec 原称的 fail_closed_total；
//   - 延迟类指标单位统一为秒（Prometheus 惯例单位，优于 ms）；
//   - spec 原列的 pii_detected_total / tool_calls_scanned_total / request_total_latency /
//     response_restore_latency 四项已于 2026-09-12 在本文件实现（命名对齐 spec，单位用秒）。
//
// 指标名属公共接口，改名会破坏已对接的 Grafana/告警，故保持现状。
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Collectors 网关运行指标集合（统一在 /metrics 暴露）。
type Collectors struct {
	RequestsTotal  *prometheus.CounterVec
	DetectLatency  *prometheus.HistogramVec
	ReplaceCount   *prometheus.CounterVec
	RestoredTotal  *prometheus.CounterVec
	BlockedTotal   *prometheus.CounterVec
	UpstreamErrors *prometheus.CounterVec
	StreamOrphans  *prometheus.CounterVec
	CacheHits      *prometheus.CounterVec
	CacheMisses    *prometheus.CounterVec
	// DetectIncremental Merkle 增量缓存：本请求实际「送检测器」vs「复用缓存」的段数。
	DetectIncremental *prometheus.CounterVec
	VaultSize         prometheus.Gauge
	ActiveConns       prometheus.Gauge

	// —— 以下 4 项 2026-09-12 补齐（spec §14.2 原「规划中」→「已实现」）——

	// PIIDetected 检出的 PII 实体数（按类型与命运），覆盖请求+响应双向脱敏。
	PIIDetected *prometheus.CounterVec
	// ToolCallsScanned 被递归扫描参数的 tool_call 数量（arguments 解析一次计一次）。
	ToolCallsScanned prometheus.Counter
	// RequestLatency 端到端请求耗时（ingress→审计收尾），单位秒。
	RequestLatency *prometheus.HistogramVec
	// RestoreLatency 响应还原（de-anonymize）处理耗时，单位秒。
	RestoreLatency *prometheus.HistogramVec

	// —— 以下 3 项为行为判断层（契约 §12，2026-09-23）——

	// VerdictTotal 判断层裁决数（按动作/类别/后端）。
	//
	// 这是判断层最重要的一个指标：`action="review"` 与 `action="block"` 的
	// 比例直接回答「这个后端是不是在乱判」，而 `engine` 标签回答「是哪一层
	// 在判」——降级频繁说明首选后端不可用或总在说 unknown。
	VerdictTotal *prometheus.CounterVec
	// JudgeLatency 单次判定耗时（按后端），单位秒。
	JudgeLatency *prometheus.HistogramVec
	// JudgeUnavailable 判定失败数（按后端与原因类别）。
	//
	// 与 VerdictTotal 分开：失败不是「判了一个结果」，它意味着这次
	// **没有看到**动作，属于覆盖缺口，不能和正常裁决混在同一个计数里。
	JudgeUnavailable *prometheus.CounterVec
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
		PIIDetected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_pii_detected_total",
			Help: "检出的 PII 实体数（按类型与命运）",
		}, []string{"entity_type", "fate"}),
		ToolCallsScanned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "llmate_tool_calls_scanned_total",
			Help: "被递归扫描参数的 tool_call 数量",
		}),
		RequestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmate_request_total_latency_seconds",
			Help:    "端到端请求耗时分布（ingress→审计收尾）",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"endpoint"}),
		RestoreLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llmate_response_restore_latency_seconds",
			Help:    "响应还原（de-anonymize）处理耗时分布",
			Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1},
		}, []string{"endpoint"}),
		VerdictTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_verdict_total",
			Help: "判断层裁决数（按动作/类别/后端）",
		}, []string{"action", "category", "engine"}),
		JudgeLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmate_judge_latency_seconds",
			Help: "单次行为判定耗时分布（按后端）",
			// 桶按判断层的预算设：热路径 300ms，所以重点刻度在毫秒到百毫秒。
			Buckets: []float64{0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.3, 0.5, 1},
		}, []string{"engine"}),
		JudgeUnavailable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmate_judge_unavailable_total",
			Help: "判断层判定失败次数（按后端与原因），表示这段时间内的覆盖缺口",
		}, []string{"engine", "reason"}),
	}
	reg.MustRegister(
		c.RequestsTotal, c.DetectLatency, c.ReplaceCount, c.RestoredTotal,
		c.BlockedTotal, c.UpstreamErrors, c.StreamOrphans,
		c.CacheHits, c.CacheMisses, c.DetectIncremental, c.VaultSize, c.ActiveConns,
		c.PIIDetected, c.ToolCallsScanned, c.RequestLatency, c.RestoreLatency,
		c.VerdictTotal, c.JudgeLatency, c.JudgeUnavailable,
	)
	return c
}
