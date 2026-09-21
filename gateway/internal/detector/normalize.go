package detector

import (
	"strings"
	"unicode/utf8"
)

// 本文件实现「形态容忍」：把同一实体的不同书写形态（分隔符、全角数字）归一化后
// 再检测一次，并把结果精确映射回原文偏移。
//
// # 背景（实测，不是推测）
//
// 真对抗矩阵语料（`cn-pii-bench/fixtures/cases_adversarial_ext.jsonl`）显示：检测器
// 对「同一个值的不同书写形态」几乎零覆盖 —— 只要值**内部**出现分隔符或全角数字就漏检。
// 其中 dashed / spaced / grouped / fullwidth / country 五种形态共 173 个 GT 全部漏检，
// 矩阵召回因此停在 0.4231。缺口与检测能力无关（裸写形态检出率 100%），纯粹是形态问题。
//
// # 三条硬约束（都是实测逼出来的，不是保守起见）
//
//  1. **只做加法，绝不替换原文。** `us_ssn` 规则要求 3-2-4 的连字符（`123-45-6789`），
//     `date` 规则要求 `-` `/` `.` `年` `月`；若把文本换成归一化版本再扫，这两类会
//     **静默退化**（把 `2024-09-17` 变成 `20240917` 就再也匹配不上了）。所以原文照旧
//     扫一遍，归一化文本只是**追加**一遍。见 regex.go 的 scan。
//
//  2. **不删数字间的 `.`。** 删掉它确实能带出点分手机号（`138.0013.8000`），但也会
//     打散 IPv4（`192.168.1.100`）—— 实测净亏：IP 少 40 个 GT，只补回 30 个。
//     点分手机号另有专用规则（regex.go 的 rePhoneDotted），不必在此冒险。
//
//  3. **偏移必须精确，且宁可漏检也不许错位。** 归一化会改变字节长度（全角数字 3 字节
//     → 1 字节；分隔符被整段删除），所以映射表按「每个归一化字节在原文中占用的字节
//     区间」精确记录，而不是估算。落地前还有两道验收（区间一致性自检 + 分隔符一致性），
//     不符就丢弃整条实体 —— 脱敏改错地方比漏检严重得多。

// normSep 是「夹在两个数字之间即删除」的分隔符集合。
//
// 这些都是纯视觉分组符，删除不改变数字本身的语义。刻意**不含** `.`（理由见文件头
// 第 2 条）。全角变体（`－`、`＿`、表意空格）不必单列：它们先被 foldWidth 折成半角，
// 再走同一判据。
var normSep = map[rune]bool{
	'-': true, '_': true, ' ': true,
	'\u00a0': true, // 不换行空格
	'·':      true, '•': true,
	'–': true, '—': true,
}

// foldWidth 全角 → 半角。
//
// 只处理「全角 ASCII 变体」（U+FF01..U+FF5E，与 ASCII 0x21..0x7E 一一对应，含全角
// 数字与全角连字符）与表意空格 U+3000；其余字符原样返回。
//
// 刻意不引入 golang.org/x/text：内置正则引擎的定位就是**零依赖**，而这里真正需要的
// 只是这一个映射区间。NFKC 的其余部分（上下标、连字、罗马数字、圈号…）对 PII 形态
// 没有意义，引进来只会扩大行为面。
func foldWidth(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	case r == 0x3000:
		return ' '
	}
	return r
}

// foldDigit 返回 rune 折叠后的 ASCII 数字；非数字返回 ok=false。
//
// 「数字之间」这个判据必须在折叠后的语义上成立，否则 `１３８ ００１３` 这种全角写法
// 里的空格会被漏掉。
func foldDigit(r rune) (byte, bool) {
	switch {
	case r >= '0' && r <= '9':
		return byte(r), true
	case r >= 0xFF10 && r <= 0xFF19:
		return byte(r - 0xFEE0), true
	}
	return 0, false
}

// sepBetweenDigits 判断 s[i:i+size) 这个字符（折叠后是分隔符）是否夹在两个数字之间。
func sepBetweenDigits(s string, i, size int) bool {
	if i == 0 || i+size >= len(s) {
		return false
	}
	if _, ok := foldDigit(runeBefore(s, i)); !ok {
		return false
	}
	_, ok := foldDigit(runeAt(s, i+size))
	return ok
}

func runeBefore(s string, i int) rune {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func runeAt(s string, i int) rune {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// needsNormalize 快速判断文本是否会因归一化而改变。
//
// 只有两种特征会让归一化起作用：出现全角 ASCII 区字符，或出现「数字夹分隔符」。
// 其余文本（纯 ASCII 标识符、常规中文）在这里直接返回 false，从而完全跳过归一化的
// 分配与拷贝 —— 归一化只对数字型固定格式实体的**书写形态**负责，不是通用预处理。
func needsNormalize(s string) bool {
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			// 纯 ASCII 快路径：不解 rune。
			if normSep[rune(s[i])] && sepBetweenDigits(s, i, 1) {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r >= 0xFF01 && r <= 0xFF5E) || r == 0x3000 {
			return true
		}
		if normSep[r] && sepBetweenDigits(s, i, size) {
			return true
		}
		i += size
	}
	return false
}

// normWindow 是一段「可能因为形态而归一化才有救」的文本窗口。
type normWindow struct {
	start int    // 窗口在原文中的起始字节偏移
	text  string // 窗口的原文
}

// windows 切出所有「值得二次检查」的窗口。
//
// # 为什么不整篇归一化再扫一遍
//
// 因为那样开销与文本长度成正比，而**绝大多数文本根本不含需要归一化的片段**。
// 实测（`BenchmarkRegexEngineDetect`，340 字节典型 Agent 负载）：整篇归一化会把
// Detect 耗时拉到 1.39×、每次分配从 4.1KB 涨到 14.3KB，直接顶穿 CI 的 1.25×
// 性能回退守门（`scripts/bench-guard.sh`）。改成窗口后，开销只与「真的像号码」
// 的片段长度成正比；不含这类片段的文本一次分配都不产生。
//
// 窗口的定义：以数字（或 `+` + 数字，国际号码写法）起、只由数字、分隔符与 `.`
// 构成、**以数字结尾**的最长片段。窗口不是「需要归一化」，而是「值得看一眼」：
//   - 含数字间分隔符或有全角字符 → 归一化后才可能匹配（`138-0013-8000`）；
//   - 以 `+` 起 → 国际号码写法（`+8613800138000`，无需归一化但形态专用规则要跑）；
//   - 数字间有点 → 点分号码（`138.0013.8000`，`.` 不在删除集里，同样无需归一化）。
//
// 窗口同时是「归一化把不相干 token 拼起来」的最大范围（见 uniformSeparators）。
func windows(text string) []normWindow {
	var out []normWindow
	start, end := -1, -1
	flush := func() {
		if start >= 0 && end > start {
			if w := text[start:end]; worthWindow(w) {
				out = append(out, normWindow{start: start, text: w})
			}
		}
		start, end = -1, -1
	}
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case isDigitRune(r):
			if start < 0 {
				start = i
			}
			end = i + size // 窗口必须以数字结尾，故只有数字推进 end
		case start < 0 && r == '+' && nextIsDigit(text, i+size):
			start, end = i, i+size
		case start >= 0 && (normSep[foldWidth(r)] || r == '.'):
			// 窗口内的分隔符 / 点号：不断开窗口，但也不收进窗口（尾部靠 end 裁掉）
		default:
			flush()
		}
		i += size
	}
	flush()
	return out
}

// worthWindow 判断窗口是否值得二次检查（三类形态，见 windows 注释）。
func worthWindow(w string) bool {
	if w[0] == '+' {
		return true
	}
	if needsNormalize(w) {
		return true
	}
	return isDottedPhoneShape(w)
}

// isDottedPhoneShape 窗口是否为「点分号码」形态：以 1 开头的 3-4-4 分组。
//
// 刻意卡到这么窄，因为每多一个窗口就要多跑一整套窗口规则，而正则扫描正是热路径的
// 大头（实测 `windows()` 自身只要 3µs，但每个窗口会带来一整套规则扫描）。
// IPv4（`192.168.1.100`）、版本号、日志里的十进制数都不满足这个形态。
// 反过来，3-4-4 里存在**四位组**，而合法 IPv4 的每一组至多三位 —— 两者天然互斥，
// 所以这条形态判据与 reIPv4 不会互相抢。
func isDottedPhoneShape(w string) bool {
	if len(w) != 13 || w[0] != '1' || w[3] != '.' || w[8] != '.' {
		return false
	}
	for i := 0; i < len(w); i++ {
		if i == 3 || i == 8 {
			continue
		}
		if !isASCIIDigit(w[i]) {
			return false
		}
	}
	return true
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// isDigitRune 半角或全角数字。
func isDigitRune(r rune) bool {
	_, ok := foldDigit(r)
	return ok
}

// nextIsDigit 从 s[i:] 起是否紧跟一个数字。
func nextIsDigit(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return isDigitRune(runeAt(s, i))
}

// normalize 返回归一化文本，以及「归一化字节 → 原文字节区间」的映射表。
//
// 文本无需归一化时返回 (text, nil, nil)，调用方据此跳过第二次扫描。
// 两个切片长度一致，与归一化文本逐字节对应：starts[i]/ends[i] 是归一化第 i 个字节
// 在原文中占用的区间 [starts[i], ends[i])。
//
// 为什么是「区间」而不是「一个下标」：折叠会把 3 字节的全角字符变成 1 字节半角，
// 删除会把多字节分隔符整段吃掉。记录区间后，任意 [ns, ne) 映射回原文都只需取
// starts[ns] 与 ends[ne-1] —— 于是映射是精确的，且**天然覆盖被删掉的分隔符**，
// 脱敏替换不会留下残渣。
func normalize(text string) (string, []int, []int) {
	if !needsNormalize(text) {
		return text, nil, nil
	}

	var (
		b      strings.Builder
		starts []int
		ends   []int
	)
	b.Grow(len(text))
	starts = make([]int, 0, len(text))
	ends = make([]int, 0, len(text))
	emit := func(v byte, os, oe int) {
		b.WriteByte(v)
		starts = append(starts, os)
		ends = append(ends, oe)
	}

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			// 非法 UTF-8：原样保留，不猜测也不丢弃（丢字节会让偏移更难对齐）。
			emit(text[i], i, i+1)
			i++
			continue
		}
		folded := foldWidth(r)
		if normSep[folded] && sepBetweenDigits(text, i, size) {
			i += size // 数字之间的分隔符：整段删除，不产生输出字节
			continue
		}
		if folded == r {
			// 未折叠：逐字节记各自的原位。**不能**整段记成 (i, i+size) ——
			// 那样从 rune 中间开始的区间会带回整个 rune，往返自检就会失配。
			for k := 0; k < size; k++ {
				emit(text[i+k], i+k, i+k+1)
			}
			i += size
			continue
		}
		// 折叠：全角字符（3 字节）压成半角（1 字节），输出字节对应原文整个 rune。
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], folded)
		for k := 0; k < n; k++ {
			emit(buf[k], i, i+size)
		}
		i += size
	}

	out := b.String()
	if out == text {
		return text, nil, nil
	}
	return out, starts, ends
}

// normMap 把归一化文本的字节区间映射回原文。
type normMap struct {
	starts []int
	ends   []int
}

// toOrig 把归一化区间 [ns, ne) 映射为原文区间 [os, oe)。
//
// 接收者为 nil 表示该窗口无需归一化（恒等映射）—— 调用方不必为此分叉。
// 区间必须非空（ns < ne）：空区间没有「最后一个字节」可以取终点。
func (m *normMap) toOrig(ns, ne int) (int, int, bool) {
	if ns < 0 || ns >= ne {
		return 0, 0, false
	}
	if m == nil {
		return ns, ne, true
	}
	if ne > len(m.starts) {
		return 0, 0, false
	}
	return m.starts[ns], m.ends[ne-1], true
}

// uniformSeparators 检查原文区间内被删除的分隔符是否只有一种。
//
// 用途：把「形态」和「凑巧」区分开。同一个人写分组号码只会用**一种**分隔符
// （`138-0013-8000` / `138 0013 8000`）；而 `2024-09-17 1234 5678` 这种
// 「日期 + 两个四位数」被归一化拼成 16 位数字的情形，分隔符必然是**混用**的。
//
// 归一化把相邻 token 合并成更长数字串，是它唯一引入的新误报面（拼出来的串还要
// 恰好通过 Luhn / 校验位才会被报出）。混用即丢弃，就是这道闸门。
func uniformSeparators(text string, os, oe int) bool {
	var seen rune
	for i := os; i < oe; {
		r, size := utf8.DecodeRuneInString(text[i:])
		folded := foldWidth(r)
		if normSep[folded] && sepBetweenDigits(text, i, size) {
			if seen != 0 && seen != folded {
				return false
			}
			seen = folded
		}
		i += size
	}
	return true
}
