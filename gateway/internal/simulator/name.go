package simulator

import (
	mrand "math/rand"
	"strings"

	"gateway/pkg/types"
)

// fakePersonName 生成中文仿真姓名（契约 §6.1）。
//
// 语义一致规则：
//   - 复姓 → 复姓（欧阳娜娜 → 司马文文）
//   - 长度对齐（3 字名 → 3 字名，2 字名 → 2 字名）
//   - 含少数民族分隔符「·」时保留结构
func (g *Generator) fakePersonName(v string, key []byte) string {
	if strings.Contains(v, "·") {
		// 少数民族姓名：保留「名·姓」结构，两段分别仿真
		parts := strings.Split(v, "·")
		out := make([]string, 0, len(parts))
		for i, p := range parts {
			out = append(out, g.fakePlainName(p, key, i))
		}
		return strings.Join(out, "·")
	}
	return g.fakePlainName(v, key, 0)
}

// fakePlainName 生成不含分隔符的单段姓名。
func (g *Generator) fakePlainName(v string, key []byte, salt int) string {
	runes := []rune(v)
	r := rngFor(key, types.EntityPersonName, []byte(v+string(rune('0'+salt))))

	// 复姓识别：原值前 2 字命中复姓表 → 生成复姓
	if len(runes) >= 3 && compoundSurnames[string(runes[:2])] {
		sur := compoundSurnameList[r.Intn(len(compoundSurnameList))]
		givenLen := len(runes) - 2
		if givenLen < 1 {
			givenLen = 1
		}
		return sur + randomGiven(r, givenLen)
	}
	// 长度对齐（2-4 字）
	n := len(runes)
	if n < 2 {
		n = 2
	}
	if n > 4 {
		n = 4
	}
	sur := singleSurnameList[r.Intn(len(singleSurnameList))]
	out := sur + randomGiven(r, n-1)
	if out == v {
		// 极小概率撞回原值 → 换一个姓
		sur = singleSurnameList[(r.Intn(len(singleSurnameList))+1)%len(singleSurnameList)]
		out = sur + randomGiven(r, n-1)
	}
	return out
}

// randomGiven 生成指定长度的常用人名用字串。
func randomGiven(r *mrand.Rand, n int) string {
	var sb strings.Builder
	pool := givenChars
	for i := 0; i < n; i++ {
		sb.WriteRune(pool[r.Intn(len(pool))])
	}
	return sb.String()
}
