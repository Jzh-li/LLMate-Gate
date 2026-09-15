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
	"sync"

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
	// Dictionary 用户自定义词典：entity_type → (真实值 → 仿真值)。
	// 命中者直接返回自定义仿真值，未命中回落内置词表 + HMAC 派生。
	// 由 config 层保证仿真值全局唯一（还原表是平表，跨类型撞车同样会串，
	// 见 config.ValidateSimulateDictionary）。
	Dictionary map[string]map[string]string
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
	// mu 保护 Cfg.Dictionary 的热加载：面板 PUT 与在途请求会并发读写。
	mu sync.RWMutex
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

// SetDictionary 热加载用户自定义词典（线程安全）。传 nil 等效清空。
//
// 词典整体替换而非增量合并——调用方（面板 / config）持有完整视图，
// 增量合并会让「删掉一条」无法表达。
func (g *Generator) SetDictionary(d map[string]map[string]string) {
	g.mu.Lock()
	g.Cfg.Dictionary = cloneDict(d)
	g.mu.Unlock()
}

// Dictionary 返回当前词典的深拷贝（线程安全），调用方可放心改。
func (g *Generator) Dictionary() map[string]map[string]string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return cloneDict(g.Cfg.Dictionary)
}

// dictLookup 在锁内查词典。
func (g *Generator) dictLookup(entityType, value string) (string, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	fake, ok := g.Cfg.Dictionary[entityType][value]
	return fake, ok
}

// cloneDict 深拷贝两层 map；nil 进 nil 出。
func cloneDict(d map[string]map[string]string) map[string]map[string]string {
	if d == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(d))
	for t, pairs := range d {
		cp := make(map[string]string, len(pairs))
		for k, v := range pairs {
			cp[k] = v
		}
		out[t] = cp
	}
	return out
}

// simulatable 仿真能力登记表——**唯一权威来源**。
//
// 为什么用表而不是 switch：这张表同时被三处消费——
//  1. Fake 的派发（本文件）；
//  2. config.SimulatableTypes()（面板下拉的候选清单）；
//  3. config.ValidateSimulateDictionary()（词典里出现表外类型即判非法）。
//
// 用 switch 时这三处只能靠「记得同步改」维系，而漏改不会编译失败：在 switch 里
// 加了 case、却忘了补 config 的清单，词典校验就会把合法配置判成非法（面板保存
// 时用户看到「unknown or non-simulatable entity type」却无从下手）。表驱动让
// 三者不可能漂移——新增一种仿真只要在这张表里加一行。
var simulatable = []struct {
	typ string
	// on 为 nil 表示该类型无开关、恒开（如邮箱/IP）；否则以 SimulateZHConfig 为准。
	on  func(SimulateZHConfig) bool
	gen func(*Generator, string, []byte) string
}{
	{types.EntityPersonName, func(c SimulateZHConfig) bool { return c.PersonName }, (*Generator).fakePersonName},
	{types.EntityPhone, func(c SimulateZHConfig) bool { return c.Phone }, (*Generator).fakePhone},
	{types.EntityIDCard, func(c SimulateZHConfig) bool { return c.IDCard }, (*Generator).fakeIDCard},
	{types.EntityBankCard, func(c SimulateZHConfig) bool { return c.BankCard }, (*Generator).fakeBankCard},
	{types.EntityAddress, nil, (*Generator).fakeAddress},
	{types.EntityEmail, nil, (*Generator).fakeEmail},
	{types.EntityIPAddress, nil, (*Generator).fakeIP},
	{types.EntityDate, nil, (*Generator).fakeDate},
}

// SimulatableTypes 返回支持仿真的实体类型（副本，顺序稳定）。
// config.SimulatableTypes() 直接委托到这里，面板下拉消费同一顺序。
func SimulatableTypes() []string {
	out := make([]string, 0, len(simulatable))
	for _, e := range simulatable {
		out = append(out, e.typ)
	}
	return out
}

// Simulatable 报告该实体类型是否有内置仿真实现。
func Simulatable(entityType string) bool {
	for _, e := range simulatable {
		if e.typ == entityType {
			return true
		}
	}
	return false
}

// Fake 生成仿真值（契约 §6.4）。
func (g *Generator) Fake(entityType string, value []byte, sessionKey []byte) ([]byte, error) {
	if types.IsIrreversible(entityType) {
		return nil, ErrNoSimulation
	}
	// 用户自定义词典优先：命中即返回，未命中回落内置词表。
	// 放在不可逆判定之后——词典不得把 api_key 之类的实体变成可还原值。
	if fake, ok := g.dictLookup(entityType, string(value)); ok {
		return []byte(fake), nil
	}
	key := sessionKey
	if len(key) == 0 {
		key = g.sessionKey
	}
	for _, e := range simulatable {
		if e.typ != entityType {
			continue
		}
		if e.on != nil && !e.on(g.Cfg) {
			return nil, ErrNoSimulation
		}
		return []byte(e.gen(g, string(value), key)), nil
	}
	return nil, ErrNoSimulation
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
