package simulator

import (
	"strings"
	"testing"

	"gateway/pkg/cn"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

func newGen() *Generator {
	return New([]byte("unit-test-session-key"))
}

// TestIDCard_Checksum 生成号码第 18 位通过 ISO 7064 校验（测试规约 §1.3）。
func TestIDCard_Checksum(t *testing.T) {
	g := newGen()
	for i := 0; i < 200; i++ {
		id := g.fakeIDCard("11010519491231002X"+string(rune('a'+i%26)), g.sessionKey)
		require.Len(t, id, 18)
		require.True(t, cn.ValidIDCard(id), "生成的身份证必须校验位合法: %s", id)
	}
}

// TestIDCard_NoRealPerson 生成号码不复用输入原值，且行政区划来自公开省级代码（测试规约 §1.3）。
func TestIDCard_NoRealPerson(t *testing.T) {
	g := newGen()
	src := "11010519491231002X"
	seen := map[string]int{}
	for i := 0; i < 1000; i++ {
		id := g.fakeIDCard(src+string(rune(i)), g.sessionKey)
		seen[id]++
		require.NotEqual(t, src, id, "仿真值不得等于原值")
		require.Contains(t, cn.ProvinceCodes, id[:2], "必须使用公开省级行政区划代码")
	}
	// 顺序码不得为 000/999（避免特定敏感值）
	for id := range seen {
		seq := id[14:17]
		require.NotEqual(t, "000", seq)
		require.NotEqual(t, "999", seq)
	}
}

// TestPhone_Prefix 生成号码前缀在合法号段内（测试规约 §1.3）。
func TestPhone_Prefix(t *testing.T) {
	g := newGen()
	prefixes := cn.PhonePrefixes()
	set := map[string]bool{}
	for _, p := range prefixes {
		set[p] = true
	}
	for i := 0; i < 500; i++ {
		p := g.fakePhone("13800138000"+string(rune(i)), g.sessionKey)
		require.Len(t, p, 11)
		require.True(t, set[p[:3]], "号段必须合法: %s", p)
	}
}

// TestBankCard_Luhn 生成号码通过 Luhn 校验（测试规约 §1.3）。
func TestBankCard_Luhn(t *testing.T) {
	g := newGen()
	for i := 0; i < 200; i++ {
		card := g.fakeBankCard("6222021234567890123"+string(rune(i)), g.sessionKey)
		require.True(t, cn.LuhnValid(card), "银行卡必须通过 Luhn: %s", card)
		require.GreaterOrEqual(t, len(card), 16)
		require.LessOrEqual(t, len(card), 19)
	}
}

// TestDoubleMapping 同会话 f(name, 张三) 恒定（测试规约 §1.3）。
func TestDoubleMapping(t *testing.T) {
	g := newGen()
	a1, err := g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	a2, err := g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	require.Equal(t, string(a1), string(a2), "同一输入必须恒定输出（会话内双射）")

	b, err := g.Fake(types.EntityPersonName, []byte("李四"), nil)
	require.NoError(t, err)
	require.NotEqual(t, string(a1), string(b), "不同输入应产生不同仿真值")
}

// TestDoubleMapping_CrossSession 不同会话密钥 → 不同仿真值。
func TestDoubleMapping_CrossSession(t *testing.T) {
	g1 := New([]byte("session-a"))
	g2 := New([]byte("session-b"))
	a, _ := g1.Fake(types.EntityPhone, []byte("13800138000"), nil)
	b, _ := g2.Fake(types.EntityPhone, []byte("13800138000"), nil)
	require.NotEqual(t, string(a), string(b))
}

// TestIrreversible_Redact api_key/password/token 走 redact，无仿真值（测试规约 §1.3）。
func TestIrreversible_Redact(t *testing.T) {
	g := newGen()
	for _, t0 := range []string{types.EntityAPIKey, types.EntityPassword, types.EntityToken} {
		_, err := g.Fake(t0, []byte("secret-value"), nil)
		require.ErrorIs(t, err, ErrNoSimulation, "%s 必须不可仿真", t0)
	}
}

// TestPersonName_LengthAndCompound 人名长度对齐 + 复姓 → 复姓（契约 §6.1）。
func TestPersonName_LengthAndCompound(t *testing.T) {
	g := newGen()
	for i := 0; i < 50; i++ {
		out := g.fakePersonName("张三", g.sessionKey)
		require.Len(t, []rune(out), 2, "2 字名必须生成 2 字名: %s", out)

		// 复姓 4 字名：欧阳 + 2 个随 i 变化的常用字，总长恒为 4 rune
		givenA := string(givenChars[i%len(givenChars)])
		givenB := string(givenChars[(i*7)%len(givenChars)])
		out3 := g.fakePersonName("欧阳"+givenA+givenB, g.sessionKey)
		require.Len(t, []rune(out3), 4, "复姓 4 字名必须保持 4 字: %s", out3)
		require.True(t, compoundSurnames[string([]rune(out3)[:2])], "复姓必须生成复姓: %s", out3)
	}
}

// TestPersonName_MinorityStructure 少数民族姓名保留「·」结构。
func TestPersonName_MinorityStructure(t *testing.T) {
	g := newGen()
	out := g.fakePersonName("阿迪力·买买提", g.sessionKey)
	require.Contains(t, out, "·")
	require.Len(t, strings.Split(out, "·"), 2)
}

// TestEmail_KeepsDomain 邮箱保留域名结构（契约 §6.1）。
func TestEmail_KeepsDomain(t *testing.T) {
	g := newGen()
	out := g.fakeEmail("zhang.san@example.com", g.sessionKey)
	require.True(t, strings.HasSuffix(out, "@example.com"))
	require.Contains(t, out, ".")
}

// TestIP_KeepsClass IP 保留私网/公网类别（契约 §6.1）。
func TestIP_KeepsClass(t *testing.T) {
	g := newGen()
	priv := g.fakeIP("192.168.1.100", g.sessionKey)
	require.True(t, strings.HasPrefix(priv, "192.168."))
	pub := g.fakeIP("8.8.8.8", g.sessionKey)
	require.False(t, strings.HasPrefix(pub, "10."))
	require.False(t, strings.HasPrefix(pub, "192.168."))
}

// TestAddress_Generated 地址按省→市→区→街道→门牌生成。
func TestAddress_Generated(t *testing.T) {
	g := newGen()
	out := g.fakeAddress("北京市海淀区中关村大街88号", g.sessionKey)
	require.True(t, strings.HasSuffix(out, "号"))
	require.NotEqual(t, "北京市海淀区中关村大街88号", out)
}

// TestDate_KeepsFormat 日期保持格式。
func TestDate_KeepsFormat(t *testing.T) {
	g := newGen()
	require.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, g.fakeDate("1990-01-01", g.sessionKey))
	require.Regexp(t, `^\d{4}/\d{1,2}/\d{1,2}$`, g.fakeDate("1990/1/1", g.sessionKey))
}

// TestSimulator_Interface 契约 §6.4 接口可用性。
func TestSimulator_Interface(t *testing.T) {
	var s Simulator = newGen()
	out, err := s.Fake(types.EntityPhone, []byte("13800138000"), []byte("explicit-key"))
	require.NoError(t, err)
	require.Len(t, string(out), 11)
}

// ---------- 用户自定义词典 ----------
//
// 词典里的仿真值刻意用「假名A」这类不可能出现在内置词表里的字串，
// 这样 NotEqual / NotContains 断言不会因内置词表恰好生成同值而偶发失败。

// TestDictionary_Hit 词典命中的实体直接返回用户指定的仿真值。
func TestDictionary_Hit(t *testing.T) {
	g := newGen()
	g.SetDictionary(map[string]map[string]string{
		types.EntityPersonName: {"张三": "假名A"},
	})
	out, err := g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	require.Equal(t, "假名A", string(out))
}

// TestDictionary_MissFallsBack 未命中的值回落内置词表 + HMAC 派生，且回落路径同样确定性。
func TestDictionary_MissFallsBack(t *testing.T) {
	g := newGen()
	g.SetDictionary(map[string]map[string]string{
		types.EntityPersonName: {"张三": "假名A"},
	})
	a, err := g.Fake(types.EntityPersonName, []byte("李四"), nil)
	require.NoError(t, err)
	require.NotEmpty(t, a)
	require.NotEqual(t, "假名A", string(a))

	b, err := g.Fake(types.EntityPersonName, []byte("李四"), nil)
	require.NoError(t, err)
	require.Equal(t, string(a), string(b), "回落路径也必须确定性")
}

// TestDictionary_DoesNotOverrideIrreversible 词典不能把不可逆实体变成可还原值。
func TestDictionary_DoesNotOverrideIrreversible(t *testing.T) {
	g := newGen()
	g.SetDictionary(map[string]map[string]string{
		types.EntityAPIKey: {"sk-real-secret": "sk-fake-secret"},
	})
	_, err := g.Fake(types.EntityAPIKey, []byte("sk-real-secret"), nil)
	require.ErrorIs(t, err, ErrNoSimulation)
}

// TestDictionary_HotReloadAndIsolation SetDictionary 即时生效；Dictionary() 返回深拷贝。
func TestDictionary_HotReloadAndIsolation(t *testing.T) {
	g := newGen()
	g.SetDictionary(map[string]map[string]string{
		types.EntityPersonName: {"张三": "假名A"},
	})

	// 改外部快照不得影响生成器
	snap := g.Dictionary()
	snap[types.EntityPersonName]["张三"] = "假名B"
	got, err := g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	require.Equal(t, "假名A", string(got))

	// 整体替换后旧条目立即失效
	g.SetDictionary(map[string]map[string]string{
		types.EntityPersonName: {"张三": "假名C"},
	})
	got, err = g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	require.Equal(t, "假名C", string(got))

	// 清空 → 回落内置，不再等于任何自定义值
	g.SetDictionary(nil)
	got, err = g.Fake(types.EntityPersonName, []byte("张三"), nil)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.NotContains(t, []string{"假名A", "假名C"}, string(got))
}

// TestDictionary_ConcurrentReload 热加载与在途请求并发（配合 go test -race）。
func TestDictionary_ConcurrentReload(t *testing.T) {
	g := newGen()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			g.SetDictionary(map[string]map[string]string{
				types.EntityPersonName: {"张三": "假名A"},
			})
			_ = g.Dictionary()
		}
	}()
	for i := 0; i < 200; i++ {
		_, err := g.Fake(types.EntityPersonName, []byte("李四"), nil)
		require.NoError(t, err)
	}
	<-done
}
