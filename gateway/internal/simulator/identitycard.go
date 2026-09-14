package simulator

import (
	"fmt"
	"strings"
)

// identityCardKey 身份卡的固定派生密钥：同一真实值跨重启、跨安装恒映射到同一仿真值。
//
// 这与会话密钥刻意不同——会话密钥是进程随机的，身份卡要的是「内置身份」：把仿真名
// 提前告知下游后，无论何时重启都还是那个名。密钥写死也意味着身份卡的映射是公开可复现的，
// 与手写 dictionary 的「明文 real→fake」同一保密等级，不再承诺更强的不可逆。
var identityCardKey = []byte("llmate-gate/identity-card/v1")

// ExpandIdentityCard 把身份卡（entity_type → 真实值）展开成仿真词典条目。
//
// 身份卡是 dictionary 的糖：把「我的真实值」按类型声明一遍，网关用固定密钥为其生成
// 格式保持、跨重启稳定的仿真值，等价于手写 (真实值 → 仿真值) 对。展开结果由
// config.ValidateSimulateDictionary 做全局唯一校验，与手写词典同一套规则。
//
// 返回结构为 entity_type → (真实值 → 仿真值)，可直接并入 Dictionary。真实值先去首尾
// 空白；未知/不可仿真的类型（含不可逆实体）返回 error。
func ExpandIdentityCard(card map[string]string) (map[string]map[string]string, error) {
	if len(card) == 0 {
		return nil, nil
	}
	gen := New(identityCardKey)
	out := make(map[string]map[string]string, len(card))
	for entityType, real := range card {
		real = strings.TrimSpace(real)
		if real == "" {
			return nil, fmt.Errorf("identity_card[%s]: empty value", entityType)
		}
		fake, err := gen.Fake(entityType, []byte(real), identityCardKey)
		if err != nil {
			return nil, fmt.Errorf("identity_card: type %q is not simulatable", entityType)
		}
		if out[entityType] == nil {
			out[entityType] = make(map[string]string)
		}
		out[entityType][real] = string(fake)
	}
	return out, nil
}
