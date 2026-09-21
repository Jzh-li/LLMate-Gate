package detector

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFoldWidth 全角 ASCII 区与表意空格折叠成半角。
func TestFoldWidth(t *testing.T) {
	cases := map[rune]rune{
		'１': '1', '９': '9', '０': '0',
		'－': '-', '＿': '_', '：': ':', 'Ａ': 'A',
		0x3000: ' ', // 表意空格
		'中':    '中', // 不在折叠区，原样
		'a':    'a',
	}
	for in, want := range cases {
		require.Equalf(t, want, foldWidth(in), "foldWidth(%q)", in)
	}
}

// TestNormalize_DigitSeparators 数字之间的分隔符被删除；`.` 保留。
func TestNormalize_DigitSeparators(t *testing.T) {
	cases := []struct{ in, want string }{
		{"138-0013-8000", "13800138000"},
		{"138 0013 8000", "13800138000"},
		{"110101 1990 0307 8531", "110101199003078531"},
		{"6222 0212 3456 7890 128", "6222021234567890128"},
		{"+86-138-0013-8000", "+8613800138000"},
		{"138_0013_8000", "13800138000"},
		{"138\u00a00013\u00a08000", "13800138000"},
		{"１３８ ００１３ ８０００", "13800138000"}, // 全角数字之间的空格同样算「数字之间」
		// `.` 必须保留：删它会把 IPv4 打散（normalize.go 文件头第 2 条）。
		{"138.0013.8000", "138.0013.8000"},
		{"192.168.1.100", "192.168.1.100"},
		// 不在数字之间的分隔符不动。
		{"abc-def", "abc-def"},
		{"sk-live-0011223344556677", "sk-live-0011223344556677"},
		{"南大街 5 号", "南大街 5 号"},
		{
			// 全角标点也在折叠区内（U+FF01..FF5E 与 ASCII 一一对应）。
			// 归一化文本只用于匹配、从不外露（实体的值取原文切片），所以折叠标点无害；
			// 这里把行为固定下来，避免以后误以为是漏改。
			"身份证 11010519491231002X，另一个 110105194912310021",
			"身份证 11010519491231002X,另一个 110105194912310021",
		},
	}
	for _, tc := range cases {
		got, _, _ := normalize(tc.in)
		require.Equalf(t, tc.want, got, "normalize(%q)", tc.in)
	}
}

// TestNormalize_IdentitySkipsMap 无需归一化时必须返回 nil 映射表 ——
// 调用方（scan）据此跳过第二次扫描，也是「常规文本零开销」的实现依据。
func TestNormalize_IdentitySkipsMap(t *testing.T) {
	for _, s := range []string{
		"13800138000",
		"张三的手机号是13800138000",
		"10.0.0.1:8080",
		"",
	} {
		_, starts, ends := normalize(s)
		require.Nilf(t, starts, "normalize(%q) 不应产生映射表", s)
		require.Nilf(t, ends, "normalize(%q) 不应产生映射表", s)
	}
	// 反向：确有形态可归一化时必须触发，否则调用方会跳过第二次扫描、漏掉形态变体。
	for _, s := range []string{"2024-09-17", "138-0013-8000", "１３８００１３８０００"} {
		norm, starts, _ := normalize(s)
		require.NotNilf(t, starts, "normalize(%q) 应当触发", s)
		require.NotEqualf(t, s, norm, "normalize(%q) 应当改变文本", s)
	}
}

// TestWindows 只切出「值得二次检查」的片段：含数字间分隔符、含全角、`+` 开头、
// 或数字间有点。
//
// 这条边界同时决定了性能与精度：不含这类片段的文本一次分配都不产生（见 windows 注释），
// 而窗口同时限制了「归一化把不相干 token 拼起来」的最大范围。
func TestWindows(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		// 需要归一化才可能匹配
		{"同事在群里说：138-0013-8000，记得回一下。", []string{"138-0013-8000"}},
		{"卡号 6222 0212 3456 7890 128 请核对", []string{"6222 0212 3456 7890 128"}},
		{"联系电话：+86-186-1234-5678，仅工作时段接听。", []string{"+86-186-1234-5678"}},
		{"同事在群里说：１３８００１３８０００，记得回一下。", []string{"１３８００１３８０００"}},
		{"同段落两处：138-0013-8000 与 110101 1990 0307 8531",
			[]string{"138-0013-8000", "110101 1990 0307 8531"}},
		// 无需归一化，但形态专用规则要跑：`+` 开头的国际号码、数字间带点的点分号码
		{"联系电话：+8618612345678，仅工作时段接听。", []string{"+8618612345678"}},
		{"同事在群里说：138.0013.8000，记得回一下。", []string{"138.0013.8000"}},
		{"服务器 192.168.1.100 已就绪", nil}, // IPv4 不是 3-4-4 点分形态
		// 以下都不产生窗口：原文那一遍已能覆盖，二次检查纯属浪费。
		{"联系方式:13800138000", nil},
		{"客户资料：李雷，电话 13912345678，邮箱 lilei@example.com。", nil},
		{"", nil},
	}
	for _, tc := range cases {
		ws := windows(tc.text)
		var got []string
		for _, w := range ws {
			// 窗口偏移必须精确，否则映射回原文会整体位移。
			require.Equalf(t, w.text, tc.text[w.start:w.start+len(w.text)],
				"窗口偏移错位：windows(%q) 的 %q 起点 %d", tc.text, w.text, w.start)
			got = append(got, w.text)
		}
		require.Equalf(t, tc.want, got, "windows(%q)", tc.text)
	}
}

// TestNormMap_RoundTrip 映射表的核心不变式：
// 任意归一化区间映射回原文后，原文切片重新归一化必须与归一化切片逐字节相同。
//
// 这条不变式就是 regex.go scan 里那道自检；它成立，偏移就不可能错位。
func TestNormMap_RoundTrip(t *testing.T) {
	texts := []string{
		"同事在群里说：138-0013-8000，记得回一下。",
		"联系电话：+86-186-1234-5678，仅工作时段接听。",
		"身份证 １１０１０１１９９００３０７８５３１ 请核对",
		"卡号 6222 0212 3456 7890 128 到期 2024-09-17",
		"混合：138 0013 8000 与 138-0013-8000 同时出现",
	}
	for _, text := range texts {
		norm, starts, ends := normalize(text)
		require.NotNilf(t, starts, "normalize(%q) 应当触发", text)
		require.Len(t, starts, len(norm))
		require.Len(t, ends, len(norm))
		m := &normMap{starts: starts, ends: ends}
		for ns := 0; ns < len(norm); ns++ {
			for ne := ns + 1; ne <= len(norm); ne++ {
				os, oe, ok := m.toOrig(ns, ne)
				require.Truef(t, ok, "toOrig(%d,%d) 应当成立", ns, ne)
				require.GreaterOrEqualf(t, os, 0, "toOrig(%d,%d)", ns, ne)
				require.LessOrEqualf(t, oe, len(text), "toOrig(%d,%d)", ns, ne)
				back, _, _ := normalize(text[os:oe])
				require.Equalf(t, norm[ns:ne], back,
					"区间 [%d,%d) → 原文 [%d,%d) 的往返不一致（原文 %q）", ns, ne, os, oe, text)
			}
		}
	}
}

// TestNormMap_EmptyRangeRejected 空区间没有「最后一个字节」，必须明确拒绝而不是猜。
func TestNormMap_EmptyRangeRejected(t *testing.T) {
	_, starts, ends := normalize("138-0013-8000")
	m := &normMap{starts: starts, ends: ends}
	_, _, ok := m.toOrig(3, 3)
	require.False(t, ok)
	_, _, ok = m.toOrig(0, len(starts)+1)
	require.False(t, ok, "越界区间必须拒绝")
	_, _, ok = m.toOrig(-1, 2)
	require.False(t, ok, "负下标必须拒绝")
}

// TestUniformSeparators 分隔符混用 ⇒ 判定为「拼出来的数字串」。
func TestUniformSeparators(t *testing.T) {
	uniform := []string{
		"138-0013-8000",
		"138 0013 8000",
		"+86-186-1234-5678",
		"6222 0212 3456 7890 128",
		"１３８００１３８０００", // 无分隔符，恒为真
	}
	for _, s := range uniform {
		require.Truef(t, uniformSeparators(s, 0, len(s)), "uniformSeparators(%q)", s)
	}
	// 同一串数字，分隔符混用：归一化会把它们拼起来，但这不该被当成一个实体。
	mixed := strings.Replace("4111 1111 1111 1111", " ", "-", 1) // "4111-1111 1111 1111"
	require.False(t, uniformSeparators(mixed, 0, len(mixed)), "uniformSeparators(%q)", mixed)
}
