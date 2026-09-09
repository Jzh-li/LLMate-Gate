// Package simulator 实现中文格式保持仿真替换（契约 §6、技术方案 §7）。
//
// 三条铁律：
//  1. 格式一致：身份证 18 位且校验位合法、手机号段合法、银行卡过 Luhn；
//  2. 语义一致：人名长度/复姓与原值对齐；
//  3. 会话内双射：同一 (entity_type, value) 恒定映射到同一仿真值
//     （HMAC-SHA256 派生，无需持久化明文映射）。
package simulator

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	mrand "math/rand"
	"strconv"
	"strings"

	"gateway/pkg/cn"
	"gateway/pkg/types"
)

// ErrNoSimulation 该实体类型不提供仿真（应走 redact）。
var ErrNoSimulation = errors.New("entity type has no simulation")

// SimulateZHConfig 中文仿真开关（契约 §3.1 replacement.simulate_zh）。
type SimulateZHConfig struct {
	PersonName bool
	Phone      bool
	IDCard     bool
	BankCard   bool
}

// Simulator 仿真接口（契约 §6.4）。
type Simulator interface {
	// Fake 生成仿真值；同一会话内 f(type, value) 恒定。
	Fake(entityType string, value []byte, sessionKey []byte) ([]byte, error)
}

// Generator 仿真生成器。sessionKey 为空时随机生成（进程内恒定）。
type Generator struct {
	sessionKey []byte
	// Cfg 控制哪些中文实体启用仿真。
	Cfg SimulateZHConfig
}

// New 构造生成器；sessionKey 为空时随机生成 32 字节。
func New(sessionKey []byte) *Generator {
	if len(sessionKey) == 0 {
		sessionKey = make([]byte, 32)
		_, _ = rand.Read(sessionKey)
	}
	return &Generator{sessionKey: sessionKey, Cfg: SimulateZHConfig{
		PersonName: true, Phone: true, IDCard: true, BankCard: true,
	}}
}

// Fake 生成仿真值（契约 §6.4）。
func (g *Generator) Fake(entityType string, value []byte, sessionKey []byte) ([]byte, error) {
	if types.IsIrreversible(entityType) {
		return nil, ErrNoSimulation
	}
	key := sessionKey
	if len(key) == 0 {
		key = g.sessionKey
	}
	switch entityType {
	case types.EntityPersonName:
		if !g.Cfg.PersonName {
			return nil, ErrNoSimulation
		}
		return []byte(g.fakePersonName(string(value), key)), nil
	case types.EntityPhone:
		if !g.Cfg.Phone {
			return nil, ErrNoSimulation
		}
		return []byte(g.fakePhone(string(value), key)), nil
	case types.EntityIDCard:
		if !g.Cfg.IDCard {
			return nil, ErrNoSimulation
		}
		return []byte(g.fakeIDCard(string(value), key)), nil
	case types.EntityBankCard:
		if !g.Cfg.BankCard {
			return nil, ErrNoSimulation
		}
		return []byte(g.fakeBankCard(string(value), key)), nil
	case types.EntityAddress:
		return []byte(g.fakeAddress(string(value), key)), nil
	case types.EntityEmail:
		return []byte(g.fakeEmail(string(value), key)), nil
	case types.EntityIPAddress:
		return []byte(g.fakeIP(string(value), key)), nil
	case types.EntityDate:
		return []byte(g.fakeDate(string(value), key)), nil
	default:
		return nil, ErrNoSimulation
	}
}

// derive 用 HMAC-SHA256 派生确定性字节流（契约 §6.4 双射保证）。
func derive(key []byte, entityType string, value []byte, n int) []byte {
	out := make([]byte, 0, n)
	var counter uint32
	for len(out) < n {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(entityType))
		mac.Write([]byte{0})
		mac.Write(value)
		var cb [4]byte
		binary.BigEndian.PutUint32(cb[:], counter)
		mac.Write(cb[:])
		out = mac.Sum(out)
		counter++
	}
	return out[:n]
}

// rngFor 派生一个确定性的伪随机数发生器。
func rngFor(key []byte, entityType string, value []byte) *mrand.Rand {
	seed := derive(key, entityType, value, 8)
	return mrand.New(mrand.NewSource(int64(binary.BigEndian.Uint64(seed)))) //nolint:gosec // 确定性 seed 是双射要求
}

// 编译期接口断言。
var _ Simulator = (*Generator)(nil)

// helpers ------------------------------------------------------------------

func digits(n int, r *mrand.Rand) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(byte('0' + r.Intn(10)))
	}
	return sb.String()
}

// fakePhone 生成号段合法的仿真手机号（契约 §6.1/§6.3）。
func (g *Generator) fakePhone(v string, key []byte) string {
	r := rngFor(key, types.EntityPhone, []byte(v))
	prefixes := cn.PhonePrefixes()
	out := prefixes[r.Intn(len(prefixes))] + digits(8, r)
	if out == v {
		// 极小概率与原值相同 → 再派生一次，保证"看起来不同"
		out = prefixes[(r.Intn(len(prefixes))+1)%len(prefixes)] + digits(8, r)
	}
	return out
}

// fakeIDCard 生成校验位合法的仿真身份证（契约 §6.2）。
//
// 行政区划仅用公开省级代码；顺序码避开 000/999 等敏感值；
// 生成的号码绝不对应真实自然人，禁止用于真实身份核验。
func (g *Generator) fakeIDCard(v string, key []byte) string {
	r := rngFor(key, types.EntityIDCard, []byte(v))
	province := cn.ProvinceCodes[r.Intn(len(cn.ProvinceCodes))]
	city := digits(2, r)
	district := digits(2, r)

	year := 1950 + r.Intn(70) // 1950..2019
	month := 1 + r.Intn(12)
	day := 1 + r.Intn(28) // 固定 ≤28，避免月末边界
	seq := 1 + r.Intn(998) // 避开 000/999
	body := province + city + district +
		strconv.Itoa(year) +
		pad2(month) + pad2(day) +
		pad3(seq)
	return body + string(cn.IDCardChecksum(body))
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func pad3(n int) string {
	switch {
	case n < 10:
		return "00" + strconv.Itoa(n)
	case n < 100:
		return "0" + strconv.Itoa(n)
	default:
		return strconv.Itoa(n)
	}
}

// fakeBankCard 生成 BIN + Luhn 合法的仿真银行卡（契约 §6.1）。
func (g *Generator) fakeBankCard(v string, key []byte) string {
	r := rngFor(key, types.EntityBankCard, []byte(v))
	length := len([]rune(v))
	if length < 16 || length > 19 {
		length = 19
	}
	bin := bankBINs[r.Intn(len(bankBINs))]
	partial := bin + digits(length-len(bin)-1, r)
	out := partial + string(cn.LuhnCheckDigit(partial))
	if out == v {
		partial = bin + digits(length-len(bin)-1, r)
		out = partial + string(cn.LuhnCheckDigit(partial))
	}
	return out
}

// fakeEmail 保留域名，本地部分由 HMAC 派生（契约 §6.1）。
func (g *Generator) fakeEmail(v string, key []byte) string {
	at := strings.LastIndex(v, "@")
	domain := "example.com"
	local := v
	if at > 0 {
		domain = v[at+1:]
		local = v[:at]
	}
	r := rngFor(key, types.EntityEmail, []byte(v))
	// 保持本地部分的结构：按分隔符切分后逐段替换
	parts := strings.FieldsFunc(local, func(c rune) bool { return c == '.' || c == '_' || c == '-' })
	sep := ""
	if strings.Contains(local, ".") {
		sep = "."
	} else if strings.Contains(local, "_") {
		sep = "_"
	} else if strings.Contains(local, "-") {
		sep = "-"
	}
	newParts := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		pool := emailWords
		newParts = append(newParts, pool[r.Intn(len(pool))])
	}
	if len(newParts) == 0 {
		newParts = append(newParts, emailWords[r.Intn(len(emailWords))])
	}
	return strings.Join(newParts, sep) + "@" + domain
}

// fakeIP 保留私网/公网类别，在同类地址空间内派生（契约 §6.1）。
func (g *Generator) fakeIP(v string, key []byte) string {
	r := rngFor(key, types.EntityIPAddress, []byte(v))
	parts := strings.Split(v, ".")
	if len(parts) != 4 {
		return "10.0.0.1"
	}
	private := false
	switch {
	case parts[0] == "10":
		private = true
	case parts[0] == "172" && len(parts[1]) > 0:
		if n, err := strconv.Atoi(parts[1]); err == nil && n >= 16 && n <= 31 {
			private = true
		}
	case parts[0] == "192" && parts[1] == "168":
		private = true
	}
	if private {
		if parts[0] == "192" {
			return "192.168." + itoa(r.Intn(256)) + "." + itoa(1+r.Intn(254))
		}
		if parts[0] == "172" {
			return "172." + itoa(16+r.Intn(16)) + "." + itoa(r.Intn(256)) + "." + itoa(1+r.Intn(254))
		}
		return "10." + itoa(r.Intn(256)) + "." + itoa(r.Intn(256)) + "." + itoa(1+r.Intn(254))
	}
	// 公网：保留首位，其余派生；避开保留段
	a := 1 + r.Intn(223)
	for a == 10 || a == 127 || a == 0 {
		a = 1 + r.Intn(223)
	}
	return itoa(a) + "." + itoa(r.Intn(256)) + "." + itoa(r.Intn(256)) + "." + itoa(1+r.Intn(254))
}

func itoa(n int) string { return strconv.Itoa(n) }

// fakeDate 保持原始格式，派生一个同量级的日期（契约 §6.1）。
func (g *Generator) fakeDate(v string, key []byte) string {
	r := rngFor(key, types.EntityDate, []byte(v))
	sep1, sep2 := "-", "-"
	hasCN := strings.Contains(v, "年")
	switch {
	case strings.Contains(v, "/"):
		sep1, sep2 = "/", "/"
	case strings.Contains(v, "."):
		sep1, sep2 = ".", "."
	case hasCN:
		sep1, sep2 = "年", "月"
	}
	suffix := ""
	if strings.HasSuffix(v, "日") {
		suffix = "日"
	}
	y := 1950 + r.Intn(70)
	m := 1 + r.Intn(12)
	d := 1 + r.Intn(28)
	ms, ds := pad2(m), pad2(d)
	if hasCN && !strings.Contains(v, "0") {
		// 中文格式若原文未补零（如 1990年1月1日），保持不补零
		ms, ds = itoa(m), itoa(d)
	}
	return itoa(y) + sep1 + ms + sep2 + ds + suffix
}
