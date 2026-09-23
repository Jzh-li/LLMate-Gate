package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestNew_AllCollectorsRegistered 验证 New 注册所有 16 个指标（含 2026-09-12 补齐的 4 项）。
//
// 注意：prometheus 的 MetricVec 在没有任何子样本时不会出现在 Gather() 输出里，
// 因此测试先为每个 vec 预创建一个 dummy 子样本，保证能被 Gather 枚举。
func TestNew_AllCollectorsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := New(reg)

	// 预实例化每个 vec（Gauge / Counter 不需要）。
	c.RequestsTotal.WithLabelValues("_", "_").Inc()
	c.DetectLatency.WithLabelValues("_").Observe(0)
	c.ReplaceCount.WithLabelValues("_").Inc()
	c.RestoredTotal.WithLabelValues("_").Inc()
	c.BlockedTotal.WithLabelValues("_").Inc()
	c.UpstreamErrors.WithLabelValues("_", "_").Inc()
	c.StreamOrphans.WithLabelValues("_").Inc()
	c.CacheHits.WithLabelValues("_").Inc()
	c.CacheMisses.WithLabelValues("_").Inc()
	c.DetectIncremental.WithLabelValues("_").Inc()
	c.VaultSize.Set(0) // 让 Gauge 进入 Gather
	c.ActiveConns.Set(0)
	c.PIIDetected.WithLabelValues("_", "_").Inc()
	c.RequestLatency.WithLabelValues("_").Observe(0)
	c.RestoreLatency.WithLabelValues("_").Observe(0)

	families, err := reg.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}

	expected := []string{
		"llmate_requests_total",
		"llmate_detect_latency_seconds",
		"llmate_replace_total",
		"llmate_restore_total",
		"llmate_blocked_total",
		"llmate_upstream_errors_total",
		"llmate_stream_orphan_placeholders_total",
		"llmate_detect_cache_hits_total",
		"llmate_detect_cache_misses_total",
		"llmate_detect_incremental_segments_total",
		"llmate_vault_size",
		"llmate_active_streams",
		// 2026-09-12 补齐
		"llmate_pii_detected_total",
		"llmate_tool_calls_scanned_total",
		"llmate_request_total_latency_seconds",
		"llmate_response_restore_latency_seconds",
	}
	for _, n := range expected {
		require.True(t, names[n], "缺少指标: %s", n)
	}
	require.Equal(t, len(expected), len(names), "指标数量不符（含额外项）")
}

// TestNew_CountersUsable 冒烟：4 个新指标能正常写入并被读出（CounterVec 多 label）。
func TestNew_CountersUsable(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := New(reg)

	c.PIIDetected.WithLabelValues("zh_phone", "reversible").Inc()
	c.PIIDetected.WithLabelValues("zh_phone", "reversible").Inc()
	c.PIIDetected.WithLabelValues("api_key", "redact").Inc()
	c.ToolCallsScanned.Inc()
	c.ToolCallsScanned.Add(2)
	c.RequestLatency.WithLabelValues("/v1/chat/completions").Observe(0.012)
	c.RestoreLatency.WithLabelValues("/v1/chat/completions").Observe(0.0008)

	require.Equal(t, 2.0, testutil.ToFloat64(c.PIIDetected.WithLabelValues("zh_phone", "reversible")))
	require.Equal(t, 1.0, testutil.ToFloat64(c.PIIDetected.WithLabelValues("api_key", "redact")))
	require.Equal(t, 3.0, testutil.ToFloat64(c.ToolCallsScanned))
	require.Equal(t, 1, testutil.CollectAndCount(c.RequestLatency))
	require.Equal(t, 1, testutil.CollectAndCount(c.RestoreLatency))
}
