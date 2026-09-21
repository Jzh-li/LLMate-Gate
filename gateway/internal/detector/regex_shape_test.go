package detector

import (
	"context"
	"strings"
	"testing"

	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// detectSet 跑一次检测，返回 "type=value" 集合。
//
// 同时校验偏移不变式：Value 必须**恰好**等于原文的 [Start, End) 切片。
// 这是形态容忍最要紧的性质 —— 归一化会改字节长度，偏移一旦错位，
// 脱敏就会改到别的字符上，比漏检严重得多。
func detectSet(t *testing.T, text string) map[string]bool {
	t.Helper()
	resp, err := NewRegexEngine().Detect(context.Background(), &types.DetectRequest{Text: text})
	require.NoError(t, err)
	got := make(map[string]bool, len(resp.Entities))
	for _, e := range resp.Entities {
		require.GreaterOrEqualf(t, e.End, e.Start, "区间必须有序（%s=%q）", e.Type, e.Value)
		require.LessOrEqualf(t, e.End, len(text), "区间越界（%s=%q）", e.Type, e.Value)
		require.Equalf(t, e.Value, text[e.Start:e.End],
			"偏移与值必须一致（%s：值 %q，原文切片 %q）", e.Type, e.Value, text[e.Start:e.End])
		got[e.Type+"="+e.Value] = true
	}
	return got
}

// TestRegexEngine_ShapeTolerance 同一实体的不同书写形态都应被检出，
// 且上报的值与偏移**保留原文形态**（真对抗日志的 GT 就是这么标的）。
//
// 用例取自 `cn-pii-bench/fixtures/cases_adversarial_ext.jsonl` 的 surface_matrix 子集。
func TestRegexEngine_ShapeTolerance(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"dashed 手机号", "同事在群里说：138-0013-8000，记得回一下。", "zh_phone=138-0013-8000"},
		{"spaced 手机号", "同事在群里说：138 0013 8000，记得回一下。", "zh_phone=138 0013 8000"},
		{"dotted 手机号", "同事在群里说：138.0013.8000，记得回一下。", "zh_phone=138.0013.8000"},
		{"fullwidth 手机号", "同事在群里说：１３８００１３８０００，记得回一下。", "zh_phone=１３８００１３８０００"},
		{"country 手机号", "同事在群里说：+86-138-0013-8000，记得回一下。", "zh_phone=+86-138-0013-8000"},
		{"country 手机号（无分隔符）", "联系电话：+8618612345678，仅工作时段接听。", "zh_phone=+8618612345678"},
		{"grouped 身份证", "同事在群里说：110101 1990 0307 8531，记得回一下。", "zh_id_card=110101 1990 0307 8531"},
		{"grouped 银行卡", "同事在群里说：6222 0212 3456 7890 128，记得回一下。", "zh_bank_card=6222 0212 3456 7890 128"},
		{"统一空格的信用卡", "卡号 4111 1111 1111 1111 请核对", "credit_card=4111 1111 1111 1111"},
	}
	for _, tc := range cases {
		if !detectSet(t, tc.text)[tc.want] {
			t.Errorf("%s：未检出 %s（原文 %q）", tc.name, tc.want, tc.text)
		}
	}
}

// TestRegexEngine_ShapeToleranceKeepsSeparatorRules 是形态容忍的**回归闸门**。
//
// date 依赖 `-` / `/` / `.` / `年`，us_ssn 依赖 3-2-4 的连字符。若实现成「把文本换成
// 归一化版本再扫」，这两类会静默退化（`2024-09-17` → `20240917` 就再也匹配不上）。
// 所以 scan 必须保留原文那一遍，本用例守的就是这条。
//
// 两类用例的断言粒度不同，是刻意的：
//   - date 只断言**类型被检出**。`reDate` 的日/月分支写成了「短优先」的交替
//     （`(?:0?[1-9]|[12][0-9]|3[01])`），匹配 `2024-09-17` 时先命中 `1` 就收，
//     实际报出的是 `2024-09-1`。这是**本轮之前就存在的缺陷**，与形态容忍无关，
//     修它要动 reDate 并牵动合成语料的 F1=1.0 基线，故单独立项，不夹带在本轮。
//     这里只断言「date 仍被检出」，避免把缺陷固化成期望值。
//   - 其余用例断言**完整值**（它们的值本来就是完整的）。
func TestRegexEngine_ShapeToleranceKeepsSeparatorRules(t *testing.T) {
	// wantType 非空时只断言该类型出现过。
	typeCases := []struct{ name, text, wantType string }{
		{"date 依赖连字符", "会议时间 2024-09-17，地点北京。", "date"},
		{"date 依赖年月日", "签约时间 2024年9月17日 生效", "date"},
	}
	for _, tc := range typeCases {
		got := detectSet(t, tc.text)
		if !hasType(got, tc.wantType) {
			t.Errorf("%s：未检出 %s 类型（原文 %q），实得 %v",
				tc.name, tc.wantType, tc.text, keysOf(got))
		}
	}

	cases := []struct {
		name, text, want string
		notWant          []string
	}{
		{"us_ssn 依赖连字符", "Record SSN 123-45-6789 for the case.", "us_ssn=123-45-6789", nil},
		{
			"IP 不得被点分手机号规则吃掉", "服务器 192.168.1.100 已就绪",
			"ip_address=192.168.1.100", []string{"zh_phone="},
		},
	}
	for _, tc := range cases {
		got := detectSet(t, tc.text)
		if !got[tc.want] {
			t.Errorf("%s：未检出 %s（原文 %q），实得 %v", tc.name, tc.want, tc.text, keysOf(got))
		}
		for _, bad := range tc.notWant {
			for k := range got {
				if strings.HasPrefix(k, bad) {
					t.Errorf("%s：不应检出 %s（原文 %q）", tc.name, k, tc.text)
				}
			}
		}
	}
}

// hasType 判断结果集里是否存在某个类型的实体。
func hasType(got map[string]bool, typ string) bool {
	for k := range got {
		if strings.HasPrefix(k, typ+"=") {
			return true
		}
	}
	return false
}

// TestRegexEngine_ShapeToleranceRejectsMixedSeparators 归一化会把相邻 token 拼成
// 更长的数字串 —— 这是它唯一引入的新误报面。分隔符混用即判定为「拼出来的」并丢弃：
// 同一串 Luhn 合法的数字，分隔符一致时报出，混用时不报。
func TestRegexEngine_ShapeToleranceRejectsMixedSeparators(t *testing.T) {
	const uniform = "卡号 4111 1111 1111 1111 请核对"
	const mixed = "卡号 4111-1111 1111 1111 请核对" // 同一串数字，连字符与空格混用

	require.True(t, detectSet(t, uniform)["credit_card=4111 1111 1111 1111"],
		"分隔符一致的 16 位卡号应当报出")
	got := detectSet(t, mixed)
	for k := range got {
		require.NotContainsf(t, k, "credit_card",
			"分隔符混用的数字串不应报为银行卡（实得 %v）", keysOf(got))
	}
}

// TestRegexEngine_ShapeToleranceNoDoubleReport 归一化路与原文路命中同一区间时，
// 只能出现一条实体（合并规则保证不与已有区间相交）。
func TestRegexEngine_ShapeToleranceNoDoubleReport(t *testing.T) {
	// 同一段文本里同一实体只出现一次，但两条路径都能命中。
	const text = "联系电话：+86-186-1234-5678，仅工作时段接听。"
	got := detectSet(t, text)
	n := 0
	for k := range got {
		if strings.HasPrefix(k, "zh_phone=") {
			n++
		}
	}
	require.Equal(t, 1, n, "同一区间不应被两条路径各报一次：%v", keysOf(got))
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
