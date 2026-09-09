package detector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// newMockSidecar 起一个返回预设 JSON 的 PII Engineer mock（测试规约 §1.2）。
func newMockSidecar(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *PIIEngineerClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewPIIEngineerClient(srv.URL, "/healthz", DetectTimeout,
		WithThresholds(map[string]float64{"zh_person_name": 0.5}))
	return srv, c
}

// TestDetect_Success 正常返回实体列表，按 start 升序（测试规约 §1.3）。
func TestDetect_Success(t *testing.T) {
	_, c := newMockSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/detect", r.URL.Path)
		_ = json.NewEncoder(w).Encode(types.DetectResponse{
			Entities: []types.Entity{
				{Type: "zh_phone", Value: "13800138000", Start: 8, End: 19, Score: 0.99},
				{Type: "zh_person_name", Value: "张三", Start: 0, End: 6, Score: 0.97},
			},
			LatencyMs: 180,
		})
	})
	resp, err := c.Detect(context.Background(), &types.DetectRequest{Text: "张三的手机号是13800138000"})
	require.NoError(t, err)
	require.Len(t, resp.Entities, 2)
	require.Equal(t, 0, resp.Entities[0].Start, "必须按 start 升序")
	require.Equal(t, 8, resp.Entities[1].Start)
}

// TestDetect_Timeout 超过 500ms → detector_timeout（测试规约 §1.3）。
func TestDetect_Timeout(t *testing.T) {
	_, c := newMockSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(700 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	_, err := c.Detect(context.Background(), &types.DetectRequest{Text: "张三"})
	require.Error(t, err)
	e, ok := gatewayerrors.As(err)
	require.True(t, ok)
	require.Equal(t, gatewayerrors.CodeDetectorTimeout, e.Code)
}

// TestDetect_SidecarDown sidecar 不可用 → detector_unavailable（测试规约 §1.3）。
func TestDetect_SidecarDown(t *testing.T) {
	srv, c := newMockSidecar(t, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // 主动关闭，模拟崩溃
	_, err := c.Detect(context.Background(), &types.DetectRequest{Text: "张三"})
	require.Error(t, err)
	e, _ := gatewayerrors.As(err)
	require.Equal(t, gatewayerrors.CodeDetectorUnavailable, e.Code)
	require.Error(t, c.Health(context.Background()))
}

// TestDetect_Threshold score 低于阈值 → 不返回该实体（测试规约 §1.3）。
func TestDetect_Threshold(t *testing.T) {
	_, c := newMockSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(types.DetectResponse{
			Entities: []types.Entity{
				{Type: "zh_person_name", Value: "张三", Start: 0, End: 6, Score: 0.3},
				{Type: "zh_person_name", Value: "李四", Start: 7, End: 13, Score: 0.9},
			},
		})
	})
	resp, err := c.Detect(context.Background(), &types.DetectRequest{Text: "张三和李四"})
	require.NoError(t, err)
	require.Len(t, resp.Entities, 1)
	require.Equal(t, "李四", resp.Entities[0].Value)
}

// TestDetectBatch_Order 批量结果顺序与输入一致（测试规约 §1.3）。
func TestDetectBatch_Order(t *testing.T) {
	_, c := newMockSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/detect/batch", r.URL.Path)
		var payload struct {
			Items []*types.DetectRequest `json:"items"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		out := make([]*types.DetectResponse, 0, len(payload.Items))
		for _, it := range payload.Items {
			out = append(out, &types.DetectResponse{
				Entities: []types.Entity{{Type: "zh_phone", Value: it.Text, Start: 0, End: len(it.Text), Score: 0.9}},
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	resps, err := c.DetectBatch(context.Background(), []*types.DetectRequest{
		{Text: "a"}, {Text: "b"}, {Text: "c"},
	})
	require.NoError(t, err)
	require.Len(t, resps, 3)
	require.Equal(t, "a", resps[0].Entities[0].Value)
	require.Equal(t, "c", resps[2].Entities[0].Value)
}

// --- 内置正则引擎 ---

// TestRegexEngine_ChineseEntities 内置引擎可检出中文强格式实体。
func TestRegexEngine_ChineseEntities(t *testing.T) {
	e := NewRegexEngine(WithThresholds(map[string]float64{
		"zh_person_name": 0.5, "zh_phone": 0.8, "zh_id_card": 0.9, "zh_bank_card": 0.9,
		"email": 0.7, "ip_address": 0.8, "date": 0.7, "api_key": 0.9, "token": 0.9,
	}))
	// 11010519491231002X 校验位合法（ISO 7064），是公开示例号码
	text := "我叫张三，手机13800138000，身份证11010519491231002X，" +
		"邮箱 zhangsan@example.com，服务器 192.168.1.100，生日 1990-01-01"
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)

	found := map[string]bool{}
	for _, ent := range resp.Entities {
		found[ent.Type] = true
		require.Equal(t, text[ent.Start:ent.End], ent.Value, "偏移必须与原文切片一致")
	}
	for _, want := range []string{"zh_person_name", "zh_phone", "zh_id_card", "email", "ip_address", "date"} {
		require.True(t, found[want], "未检出实体类型: %s", want)
	}
}

// TestRegexEngine_IDCardChecksum 身份证必须通过 ISO 7064 校验位。
func TestRegexEngine_IDCardChecksum(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "身份证11010519491231002X，另一个110105194912310021"})
	require.NoError(t, err)
	require.Len(t, resp.Entities, 1, "校验位非法的不应被检出")
	require.Equal(t, "11010519491231002X", resp.Entities[0].Value)
}

// TestRegexEngine_NoFalsePositiveOnPlainDigits 普通长数字串不应被误判为银行卡。
func TestRegexEngine_NoFalsePositiveOnPlainDigits(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: "订单编号是12345678901234567890"})
	require.NoError(t, err)
	for _, ent := range resp.Entities {
		require.NotEqual(t, types.EntityBankCard, ent.Type)
	}
}

// TestRegexEngine_Health 内置引擎永远健康。
func TestRegexEngine_Health(t *testing.T) {
	e := NewRegexEngine()
	require.NoError(t, e.Health(context.Background()))
	require.Equal(t, "regex", e.Name())
}

// TestRegexEngine_Secrets 凭证类实体被识别为不可逆类型。
func TestRegexEngine_Secrets(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "api_key: sk-abcdefghijklmnopqrstuvwx"})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Entities)
	sawSecret := false
	for _, ent := range resp.Entities {
		if types.IsIrreversible(ent.Type) {
			sawSecret = true
		}
	}
	require.True(t, sawSecret, "凭证应被识别为不可逆实体")
}
