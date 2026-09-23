package global

import "testing"

func TestIsInternationalCard(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// Visa
		{"4242424242424242", true},
		{"4111111111111111", true},
		// MasterCard
		{"5555555555554444", true},
		{"5105105105105100", true},
		{"2223003122003222", true}, // 2221-2720 段
		// Amex
		{"378282246310005", true},
		{"371449635398431", true},
		// Discover
		{"6011111111111117", true},
		{"6500000000000002", true},
		// JCB
		{"3530111333300000", true},
		// Diners
		{"30569309025904", true},
		// 中国本地（不属国际品牌）
		{"6222021234567890123", false}, // 银联 62 + 19 位（中国借记卡）
		{"6212345678901234", false},    // 银联 62 + 16 位（典型中国银联）
		// 长度异常
		{"411111111111", false},         // 12 位
		{"41111111111111111111", false}, // 20 位
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			if got := IsInternationalCard(tc.v); got != tc.want {
				t.Errorf("IsInternationalCard(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestValidUSSSN(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// 合法形态
		{"123-45-6789", true},
		{"078-05-1120", true}, // SSA 公开示例
		// 形态不合法
		{"12345-6789", false},
		{"12-345-6789", false},
		{"abc-12-3456", false},
		{"078 05 1120", false}, // 不接受空格分隔（避免与电话混淆）
		// SSA 不分配的 area
		{"000-12-3456", false},
		{"666-12-3456", false},
		{"900-12-3456", false},
		{"999-12-3456", false},
		// group/serial 零值
		{"123-00-6789", false},
		{"123-45-0000", false},
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			if got := ValidUSSSN(tc.v); got != tc.want {
				t.Errorf("ValidUSSSN(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestValidURL(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"https://example.com", true},
		{"http://api.example.com/v1/users", true},
		{"https://example.com:8443/path?q=1&r=2", true},
		{"ftp://files.example.com/pub/data.zip", true},
		// 协议不对
		{"javascript:alert(1)", false},
		{"mailto:user@example.com", false},
		{"file:///etc/passwd", false},
		// 无 host
		{"http://", false},
		// 太长 / 太短
		{"http://a.b", true}, // 8 字符，刚到下界
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			if got := ValidURL(tc.v); got != tc.want {
				t.Errorf("ValidURL(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestValidCreditCard(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// Visa 测试卡（Stripe 公开测试卡号）
		{"4242424242424242", true}, // Visa 16
		{"4000000000000002", true}, // Visa 16（declined 卡片，Luhn 仍合法）
		// MasterCard
		{"5555555555554444", true}, // MC 16
		// Amex（15 位）
		{"378282246310005", true},
		{"371449635398431", true},
		// Discover
		{"6011111111111117", true},
		// JCB
		{"3530111333300000", true},
		// 不合法（Luhn 错）
		{"4242424242424241", false},
		{"1234567890123456", false},
		// 长度错
		{"123456789012", false},         // 12 位
		{"12345678901234567890", false}, // 20 位
	}
	for _, tc := range cases {
		t.Run(tc.v, func(t *testing.T) {
			if got := ValidCreditCard(tc.v); got != tc.want {
				t.Errorf("ValidCreditCard(%q) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}
