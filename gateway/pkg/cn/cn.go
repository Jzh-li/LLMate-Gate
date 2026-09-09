// Package cn 提供中国大陆格式校验原语（手机号段、身份证校验位、Luhn）。
//
// 该包被 detector（检测）与 simulator（仿真生成）共用，因此放在 pkg 下，
// 避免 detector ↔ simulator 之间产生横向依赖（契约 §1.2 依赖方向）。
package cn

// PhonePrefixes 返回 2026 年现行合法手机号前三位号段（契约 §6.3）。
func PhonePrefixes() []string {
	return []string{
		"130", "131", "132", "133", "134", "135", "136", "137", "138", "139",
		"145", "146", "147", "148", "149",
		"150", "151", "152", "153", "155", "156", "157", "158", "159",
		"166", "167",
		"170", "171", "172", "173", "174", "175", "176", "177", "178", "179",
		"180", "181", "182", "183", "184", "185", "186", "187", "188", "189",
		"190", "191", "192", "193", "195", "196", "197", "198", "199",
	}
}

// ValidPhonePrefix 判断手机号前三位是否在合法号段内。
func ValidPhonePrefix(v string) bool {
	if len(v) < 3 {
		return false
	}
	p := v[:3]
	for _, x := range PhonePrefixes() {
		if x == p {
			return true
		}
	}
	return false
}

// ValidPhone 校验 11 位手机号：1 开头 + 合法号段。
func ValidPhone(v string) bool {
	if len(v) != 11 || v[0] != '1' {
		return false
	}
	for i := 0; i < 11; i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return ValidPhonePrefix(v)
}

// IDCardChecksum ISO 7064 / GB 11643 加权校验，返回第 18 位校验字符（契约 §6.2）。
//
//	权重 W = [7,9,10,5,8,4,2,1,6,3,7,9,10,5,8,4,2]
//	校验码表 R = "10X98765432"（index = sum % 11）
func IDCardChecksum(body17 string) byte {
	if len(body17) != 17 {
		return 0
	}
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	const checkMap = "10X98765432"
	sum := 0
	for i := 0; i < 17; i++ {
		c := body17[i]
		if c < '0' || c > '9' {
			return 0
		}
		sum += int(c-'0') * weights[i]
	}
	return checkMap[sum%11]
}

// ValidIDCard 校验身份证号（18 位校验位 / 15 位老式）。
func ValidIDCard(v string) bool {
	if len(v) == 15 {
		for i := 0; i < 15; i++ {
			if v[i] < '0' || v[i] > '9' {
				return false
			}
		}
		return true
	}
	if len(v) != 18 {
		return false
	}
	for i := 0; i < 17; i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	got := v[17]
	if got >= 'a' && got <= 'z' {
		got -= 32 // 小写 x 归一为 X
	}
	return got == IDCardChecksum(v[:17])
}

// LuhnValid Luhn 校验（银行卡）。
func LuhnValid(v string) bool {
	if len(v) == 0 {
		return false
	}
	sum := 0
	alt := false
	for i := len(v) - 1; i >= 0; i-- {
		c := v[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

// LuhnCheckDigit 计算使号码通过 Luhn 的校验位。
func LuhnCheckDigit(partial string) byte {
	sum := 0
	alt := true // 校验位本身为 alt=false，因此从右侧第一个 body 位开始 alt=true
	for i := len(partial) - 1; i >= 0; i-- {
		d := int(partial[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return byte('0' + (10-sum%10)%10)
}

// ValidBankCard 银行卡：16-19 位 + Luhn，且不得是合法身份证号（避免误判）。
func ValidBankCard(v string) bool {
	if len(v) < 16 || len(v) > 19 {
		return false
	}
	if len(v) == 18 && ValidIDCard(v) {
		return false
	}
	return LuhnValid(v)
}

// ProvinceCodes 公开行政区划省级代码（契约 §6.2：仅用公开省级代码，不对应真实自然人）。
var ProvinceCodes = []string{
	"11", "12", "13", "14", "15", // 京津冀晋蒙
	"21", "22", "23", // 辽吉黑
	"31", "32", "33", "34", "35", "36", "37", // 沪苏浙皖闽赣鲁
	"41", "42", "43", "44", "45", "46", // 豫鄂湘粤桂琼
	"50", "51", "52", "53", "54", // 渝川贵云藏
	"61", "62", "63", "64", "65", // 陕甘青宁新
}
