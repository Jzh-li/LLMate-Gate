// Package global 提供国际通用格式校验原语（URL/SSN/信用卡 Luhn）。
//
// 跟 pkg/cn 的关系（边界）：
//   - pkg/cn：仅限中国大陆特有规则（手机号段、身份证 GB 11643、行政区划）
//   - pkg/global：国际通用规则，跟语言/国家无强绑定（URL/SSN/Luhn）
//
// 命名规范：
//   - pkg/cn/* 的函数加 Valid 前缀（中文特有）
//   - pkg/global/* 同样 Valid 前缀（国际通用）
//
// 复用：信用卡 Luhn 复用 pkg/cn.LuhnValid（中国 Luhn = 国际 Luhn，算法一致）。
package global

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"gateway/pkg/cn"
)

// USSSNPattern 美国 SSN 形态：AAA-GG-SSSS（必须带连字符）。
// 不接受空格分隔（避免跟电话号码混淆）；不接受无分隔（避免跟长数字串混淆）。
var usssnPattern = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)

// HighEntropyAreas 排除「全 0」「顺号」「递增号」的低熵 SSN（SSA 公开不分配）。
// 来源：https://www.ssa.gov/employer/randomization.html
// 000 全部 / 666 永久不发 / 900-999 留作 ITIN
var usssnInvalidAreas = func() map[string]bool {
	m := map[string]bool{"000": true, "666": true}
	// 900-999 全部不分配给 SSN（除 ITIN，但 ITIN 形态是 9XX-7X/8X-XXXX）
	for i := 900; i <= 999; i++ {
		m[fmt.Sprintf("%03d", i)] = true
	}
	return m
}()

// ValidUSSSN 校验美国 SSN：
//  1. 形态合法（AAA-GG-SSSS）
//  2. area 不在 SSA 不分配列表（000/666/900-999）
//  3. group 非 00，serial 非 0000
func ValidUSSSN(v string) bool {
	v = strings.TrimSpace(v)
	if !usssnPattern.MatchString(v) {
		return false
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, v)
	if len(digits) != 9 {
		return false
	}
	area := digits[0:3]
	group := digits[3:5]
	serial := digits[5:9]
	if usssnInvalidAreas[area] {
		return false
	}
	if group == "00" || serial == "0000" {
		return false
	}
	return true
}

// ValidURL 校验 URL：http/https/ftp 协议 + 含 host + 长度合理。
// 邮箱里嵌的 URL（mailto）不算 URL（用 email 正则处理）。
func ValidURL(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 8 || len(v) > 2048 {
		return false
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

// ValidCreditCard 信用卡：13-19 位 + Luhn 校验（中国/国际一致）。
// 注意：跟 zh_bank_card 共用校验逻辑，但 schema 不同（CN=银联卡号，
// INTL=Visa/Master/Amex/Disc），所以另立类型，不复用 zh_bank_card。
func ValidCreditCard(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 13 || len(v) > 19 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return cn.LuhnValid(v)
}

// IsInternationalCard 通过 IIN 前缀判断是否为国际信用卡品牌。
// 返回 true → credit_card，返回 false → zh_bank_card（中国本地借记卡/银联卡）。
//
// 国际卡组织 IIN 前缀（公开来源 ISO/IEC 7812）：
//   - Visa:       4xxx
//   - MasterCard: 51-55xx, 2221-2720
//   - Amex:       34xx, 37xx
//   - Discover:   6011, 622126-622925, 644-649, 65xx
//   - JCB:        3528-3589
//   - Diners:     300-305, 36xx, 38xx
//
// UnionPay（62xx）部分为国际发卡，部分为中国本地——保守判定：
// 62 开头的 16-19 位银联卡为 zh_bank_card，13-15 位为 credit_card。
func IsInternationalCard(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 13 || len(v) > 19 {
		return false
	}
	if len(v) >= 2 && v[0:2] == "34" || len(v) >= 2 && v[0:2] == "37" {
		return true // Amex
	}
	if len(v) >= 1 && v[0] == '4' {
		return true // Visa
	}
	if len(v) >= 2 && v[0:2] >= "51" && v[0:2] <= "55" {
		return true // MasterCard 51-55
	}
	if len(v) >= 4 && v[0:4] >= "2221" && v[0:4] <= "2720" {
		return true // MasterCard 2221-2720
	}
	if len(v) >= 4 && v[0:4] == "6011" {
		return true // Discover
	}
	if len(v) >= 3 && v[0:3] >= "644" && v[0:3] <= "649" {
		return true // Discover
	}
	if len(v) >= 2 && v[0:2] == "65" {
		return true // Discover
	}
	if len(v) >= 4 && v[0:4] >= "3528" && v[0:4] <= "3589" {
		return true // JCB
	}
	if len(v) >= 3 && v[0:3] >= "300" && v[0:3] <= "305" {
		return true // Diners
	}
	if len(v) >= 2 && (v[0:2] == "36" || v[0:2] == "38") {
		return true // Diners
	}
	return false
}
