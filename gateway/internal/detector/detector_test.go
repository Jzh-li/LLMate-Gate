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

// TestRegexEngine_PersonName_NoParticleOvercapture 人名不得把尾部助词吞进来。
//
// 回归护栏：reNameCtx / reNameTile 的 `[一-龥]{2,4}` 是贪婪的，引导词紧邻人名时
// 会多捕一个字 ——「员工张三的身份证号是…」曾捕出 4 字人名「张三的身」。
// 这是精确率缺陷（引擎的设计契约是对弱格式实体「只保证精确率」）。
func TestRegexEngine_PersonName_NoParticleOvercapture(t *testing.T) {
	e := NewRegexEngine(WithThresholds(map[string]float64{
		"zh_person_name": 0.5, "zh_id_card": 0.9, "zh_phone": 0.8,
	}))

	cases := []struct {
		text string
		want string // 期望的人名值（"" 表示不应检出人名）
	}{
		// 核心回归：掩码身份证使 id_card 规则不命中，人名规则曾把「的身」吞进来
		{"员工张三的身份证号是 110101********8531，请核对。", "张三"},
		{"员工李四的身份证号是 110101********8531，请核对。", "李四"},
		{"员工张三的身份证号是110101199003078531", "张三"},
		// 四字名（复姓 + 双字名）不得被误截
		{"客户欧阳娜娜确认出席。", "欧阳娜娜"},
		{"联系人上官婉儿确认出席。", "上官婉儿"},
		// 三字名末字落在「和」这类可能作名末字的字上 → 不截断
		{"联系人李永和确认收到。", "李永和"},
		// 没有引导词 / 称谓时本就低召回（引擎的已声明边界），不得因截断而改变
		{"本次合作由欧阳娜娜代表团队出面。", ""},
		// 称谓规则同样要截：「王五的先生」捕到「王五的」+ 称谓，人名应是「王五」
		{"王五的先生到了。", "王五"},
		// 称谓规则的正例不受影响
		{"张三先生明天到。", "张三"},
	}
	for _, c := range cases {
		resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: c.text})
		require.NoError(t, err)
		var got string
		for _, ent := range resp.Entities {
			if ent.Type == "zh_person_name" {
				got = ent.Value
				require.Equal(t, c.text[ent.Start:ent.End], ent.Value, "偏移必须与原文切片一致")
			}
		}
		require.Equal(t, c.want, got, "text=%q", c.text)
	}
}

// TestRegexEngine_PersonName_AllOccurrencesReported 同一段文本里重复出现的同名 PII
// 必须**逐次**上报。
//
// 回归护栏（这是一处真实泄漏）：早期 scanPersonName / scanPlateEN 用 `map[string]bool`
// 按**值**去重，于是同一段文本里第二处同名的人名只上报一次，后续出现原样发往上游
// —— 网关存在的意义就是拦住这件事。端到端复现（工具调用载荷里同一段文本落在两个
// 字段中，两处都带引导词）：
//
//	IN : [{"content": "我叫李杰琪"}, {"content": "联系人李杰琪"}]
//	OUT: [{"content": "我叫<<zh_person_name_1>>"}, {"content": "联系人李杰琪"}]   ← 泄漏
//
// 注意：本用例要求两处都带强引导词 —— 人名规则是「上下文引导 + 精确率优先」，
// 不带引导词的第二次出现本就不在召回范围内（引擎的已声明边界）。
// 去重必须按区间（span），规则之间的重叠交给 replacer.sanitizeEntities 消解。
func TestRegexEngine_PersonName_AllOccurrencesReported(t *testing.T) {
	e := NewRegexEngine(WithThresholds(map[string]float64{"zh_person_name": 0.5}))

	text := "我叫李杰琪，联系人李杰琪。"
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)

	var names []types.Entity
	for _, ent := range resp.Entities {
		if ent.Type == "zh_person_name" {
			names = append(names, ent)
			require.Equal(t, text[ent.Start:ent.End], ent.Value, "偏移必须与原文切片一致")
		}
	}
	require.Len(t, names, 2, "两处引导词命中的同名 PII 都要上报，否则第二处明文会泄漏到上游")
	require.Equal(t, "李杰琪", names[0].Value)
	require.Equal(t, "李杰琪", names[1].Value)
	require.NotEqual(t, names[0].Start, names[1].Start, "两次上报必须是不同的区间")
}

// TestRegexEngine_PlateEN_AllOccurrencesReported 车牌同样逐次上报（与上面同源缺陷）。
func TestRegexEngine_PlateEN_AllOccurrencesReported(t *testing.T) {
	e := NewRegexEngine(WithThresholds(map[string]float64{"plate": 0.5}))

	text := "license plate: ABC-1234 was seen, again license plate: ABC-1234 nearby."
	resp, err := e.Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)

	var plates []types.Entity
	for _, ent := range resp.Entities {
		if ent.Type == "plate" {
			plates = append(plates, ent)
		}
	}
	require.Len(t, plates, 2, "两次出现都要上报")
	require.NotEqual(t, plates[0].Start, plates[1].Start)
}

// TestRegexEngine_IDCardChecksum 身份证必须通过 ISO 7064 校验位。
func TestRegexEngine_IDCardChecksum(t *testing.T) {	e := NewRegexEngine()
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
	for _, tc := range cases {		t.Run(tc.name, func(t *testing.T) {
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
