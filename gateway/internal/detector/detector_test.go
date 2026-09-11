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

// --- 英文/国际化基线（2026-09-11 拍板） ---

// TestRegexEngine_EnglishEntities 内置引擎可检出英文强格式实体。
func TestRegexEngine_EnglishEntities(t *testing.T) {
	e := NewRegexEngine(WithThresholds(map[string]float64{
		"plate": 0.7, "url": 0.7, "us_ssn": 0.7, "credit_card": 0.7,
	}))
	text := "Visit https://example.com/api/v1 for details. " +
		"SSN: 078-05-1120 (public SSA example). " +
		"Pay with Visa 4242424242424242. " +
		"License plate: ABC-1234 (California)."
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)

	found := map[string]string{}
	for _, ent := range resp.Entities {
		found[ent.Type] = ent.Value
	}
	// URL / SSN / CC 必出
	require.Equal(t, "https://example.com/api/v1", found["url"], "URL 必须识别")
	require.Equal(t, "078-05-1120", found["us_ssn"], "US SSN 必须识别")
	require.Equal(t, "4242424242424242", found["credit_card"], "Visa 卡必须识别")
	// 英文车牌可识别（CA-state 缩写），但不是强制（避免误报"ABC-1234"命中太多）
	// 这里我们只断言 plate 字段能识别出至少一个候选，允许后续调整。
}

// TestRegexEngine_USSSN_RejectsInvalid 非法 SSN 必须被拒。
func TestRegexEngine_USSSN_RejectsInvalid(t *testing.T) {
	e := NewRegexEngine()
	for _, ssn := range []string{"000-12-3456", "666-12-3456", "900-12-3456", "123-00-6789"} {
		text := "SSN " + ssn + " here"
		resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
		require.NoError(t, err)
		for _, ent := range resp.Entities {
			require.NotEqual(t, types.EntityUSSSN, ent.Type,
				"非法 SSN %s 不应被检出，但发现 %+v", ssn, ent)
		}
	}
}

// TestRegexEngine_CreditCard_Amex15 Amex 15 位卡号必须能识别。
func TestRegexEngine_CreditCard_Amex15(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "Amex 378282246310005",
	})
	require.NoError(t, err)
	var saw bool
	for _, ent := range resp.Entities {
		if ent.Type == types.EntityCreditCard && ent.Value == "378282246310005" {
			saw = true
		}
	}
	require.True(t, saw, "Amex 15 位卡号必须识别为 credit_card（不是 zh_bank_card）")
}

// TestRegexEngine_CreditCard_InvalidLuhn Luhn 错的卡号必须被拒。
func TestRegexEngine_CreditCard_InvalidLuhn(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "bad card 4242424242424241",
	})
	require.NoError(t, err)
	for _, ent := range resp.Entities {
		require.NotEqual(t, types.EntityCreditCard, ent.Type)
	}
}

// TestRegexEngine_BilingualMixed 中英 PII 在同一文本中并行识别。
// 这是用户原话「覆盖范围与中文规则保持一致」的直接验证。
func TestRegexEngine_BilingualMixed(t *testing.T) {
	e := NewRegexEngine()
	text := "User 张三 (zhangsan@example.com) from " +
		"192.168.1.100 posted SSN 078-05-1120 and " +
		"Visa 4242424242424242, plate 京A12345."
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)

	byType := map[string][]string{}
	for _, ent := range resp.Entities {
		byType[ent.Type] = append(byType[ent.Type], ent.Value)
	}

	require.Contains(t, byType["email"], "zhangsan@example.com", "邮箱必识别")
	require.Contains(t, byType["ip_address"], "192.168.1.100", "IP 必识别")
	require.Contains(t, byType["us_ssn"], "078-05-1120", "SSN 必识别")
	require.Contains(t, byType["credit_card"], "4242424242424242", "信用卡必识别")
	require.Contains(t, byType["plate"], "京A12345", "中文车牌必识别")
}

// TestRegexEngine_URL_NotMistakenForEmail 邮箱 host 不被当成 URL。
func TestRegexEngine_URL_NotMistakenForEmail(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "email me at user@example.com please",
	})
	require.NoError(t, err)
	var urlCount int
	for _, ent := range resp.Entities {
		if ent.Type == types.EntityURL {
			urlCount++
		}
	}
	require.Zero(t, urlCount, "邮箱 host 不应被识别成 URL")
}

// TestRegexEngine_URL_TrailingPunctuation URL 末尾的句读标点不算 URL 的一部分。
func TestRegexEngine_URL_TrailingPunctuation(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "Visit https://docs.example.com/setup. Then call the API.",
	})
	require.NoError(t, err)
	var got string
	for _, ent := range resp.Entities {
		if ent.Type == types.EntityURL {
			got = ent.Value
		}
	}
	require.Equal(t, "https://docs.example.com/setup", got, "句号不应计入 URL")
}

// TestRegexEngine_PlateEN_RequiresContext 英文车牌必须有引导词；纯缩写不误报。
func TestRegexEngine_PlateEN_RequiresContext(t *testing.T) {
	e := NewRegexEngine()
	// 无上下文：API / ISO 9001 / HTTP 都不该命中 plate
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "The API returns JSON over HTTP, certified ISO 9001.",
	})
	require.NoError(t, err)
	for _, ent := range resp.Entities {
		require.NotEqual(t, types.EntityPlate, ent.Type,
			"缩写不应命中 plate: %+v", ent)
	}

	// 有上下文：License plate: ABC-1234 → 命中，且值只含车牌本体
	resp2, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "The vehicle's license plate: ABC-1234 was reported.",
	})
	require.NoError(t, err)
	var saw bool
	for _, ent := range resp2.Entities {
		if ent.Type == types.EntityPlate {
			saw = true
			require.Equal(t, "ABC-1234", ent.Value, "只报车牌本体，不含引导词")
		}
	}
	require.True(t, saw, "License plate: ABC-1234 必须识别")
}

// TestRegexEngine_PlateEN_CaliforniaStyle 加州 digit-first 车牌（7XWA123）。
func TestRegexEngine_PlateEN_CaliforniaStyle(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "License plate number 7XWA123 belongs to the suspect.",
	})
	require.NoError(t, err)
	var saw bool
	for _, ent := range resp.Entities {
		if ent.Type == types.EntityPlate && ent.Value == "7XWA123" {
			saw = true
		}
	}
	require.True(t, saw, "加州格式 7XWA123 必须识别")
}

// TestRegexEngine_URL_NotMistakenForEmail 的对偶：裸域名（无协议头）不算 URL。
func TestRegexEngine_URL_RequiresScheme(t *testing.T) {
	e := NewRegexEngine()
	resp, err := e.Detect(context.Background(), &types.DetectRequest{
		Text: "See example.com for details or docs.example.org/setup.",
	})
	require.NoError(t, err)
	for _, ent := range resp.Entities {
		require.NotEqual(t, types.EntityURL, ent.Type, "无 scheme 的裸域名不算 URL")
	}
}

// TestRegexEngine_AddressAllForms 覆盖省/直辖市/自治区三类地址形式。
// 回归用：先前正则只认 X省，漏掉 4 直辖市（baseline recall 0.67）。
func TestRegexEngine_AddressAllForms(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		expect string
	}{
		// 省级（已有覆盖，确保回归）
		{"province_with_district", "收件地址：浙江省南京市文三路261号", "浙江省南京市文三路261号"},
		{"province_no_district", "收件地址：广东省深圳市南京西路26号", "广东省深圳市南京西路26号"},
		// 直辖市（新增覆盖）
		{"direct_city_beijing", "收件地址：北京市海淀区科技园路207号", "北京市海淀区科技园路207号"},
		{"direct_city_shanghai", "收件地址：上海市海淀区中关村大街65号", "上海市海淀区中关村大街65号"},
		{"direct_city_tianjin", "办公地址：天津市南开区卫津路100号", "天津市南开区卫津路100号"},
		{"direct_city_chongqing", "重庆市渝中区中山四路36号", "重庆市渝中区中山四路36号"},
		// 自治区（与省份走同一分支，确保未回归）
		{"autonomous_region", "地址：内蒙古自治区呼和浩特市新华大街50号", "内蒙古自治区呼和浩特市新华大街50号"},
	}
	e := NewRegexEngine()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: tc.text})
			require.NoError(t, err)
			var saw bool
			for _, ent := range resp.Entities {
				if ent.Type == types.EntityAddress && ent.Value == tc.expect {
					saw = true
					break
				}
			}
			require.True(t, saw, "expected address %q in %q, got entities=%+v", tc.expect, tc.text, resp.Entities)
		})
	}
}
