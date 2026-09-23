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
// 本用例只守「必须保留原文扫描」这一条性质。date 的跨度曾被截断，因而一度只能断言
// 「类型被检出」；2026-09-23 修掉后恢复断言完整值，跨度另有 TestRegexEngine_DateSpan 守。
func TestRegexEngine_ShapeToleranceKeepsSeparatorRules(t *testing.T) {
	cases := []struct {
		name, text, want string
		notWant          []string
	}{
		{"date 依赖连字符", "会议时间 2024-09-17，地点北京。", "date=2024-09-17", nil},
		{"date 依赖年月日", "签约时间 2024年9月17日 生效", "date=2024年9月17日", nil},
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

// TestRegexEngine_DateSpan 守住日期的跨度不被截断（2026-09-23 修复的回归闸门）。
//
// 缺陷形态：`reDate` 的日交替写成「短优先」（`(?:0?[1-9]|[12][0-9]|3[01])`），
// 而日后面跟的是**可选**的 `日?`。Go regexp 取 leftmost-first，返回最先到达接受态
// 的分支，于是日 ≥ 10 只报到第一位数字：`2024-09-17` → `2024-09-1`。
// 脱敏只替换 span 内的字节，输出里就残留一个 `7`（`2024年12月31日` 残留 `1日`）。
//
// 判据不是「哪种写法好看」，而是**交替后面跟必需元素还是可选元素**：
//   - 日后面是 `日?`（可选）→ 短优先必错，必须长优先；
//   - 月后面是 `[-/.月]`（必需）→ 短优先也会因整体失败而回退纠偏，故保持原样。
//
// 所以本用例同时覆盖月与日取满，并特意保留「短分支恰好够用」的对照样本
// （`2024-01-05` / `2024-09-1`）—— 只看双位日会以为长优先是唯一写法。
func TestRegexEngine_DateSpan(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"hyphen 双位日", "会议时间 2024-09-17，地点北京。", "date=2024-09-17"},
		{"hyphen 日 31", "签约于 2024-12-31 生效", "date=2024-12-31"},
		{"hyphen 日 05（短分支恰好够用）", "生效日 2024-01-05 无异议", "date=2024-01-05"},
		{"hyphen 单位日", "今天是 2024-09-1 星期天", "date=2024-09-1"},
		{"斜杠", "归档 2024/09/17 完毕", "date=2024/09/17"},
		{"点分", "记录 2024.09.17 完毕", "date=2024.09.17"},
		{"中文年月日", "签署于 2024年9月17日 当天", "date=2024年9月17日"},
		{"中文年月日 31", "截止 2024年12月31日 止", "date=2024年12月31日"},
		{"20 世纪", "出生于 1999-11-30 上午", "date=1999-11-30"},
		{"单月位", "生效 2024-9-17 起", "date=2024-9-17"},
	}
	for _, tc := range cases {
		got := detectSet(t, tc.text)
		if !got[tc.want] {
			t.Errorf("%s：未检出 %s（原文 %q），实得 %v", tc.name, tc.want, tc.text, keysOf(got))
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
