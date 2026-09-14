// Package registry 实现「登记表」——用户自报真实 PII 值的检测补召回层。
//
// 定位：检测层的一次**精确匹配前置**，不是第二条运行时路径。用户把真实存在、但
// 模型 / 正则必然漏检的值（中文姓名、自定义地址、内部工号式邮箱……）登记进来，
// 命中即产出该类型的实体，与内层检测结果合并后进入同一条替换 / 还原链路（契约 §5）。
//
// 为什么需要它：这类值没有格式约束，统计模型给不出召回保证；而「我刚看到自己的
// 名字被原样发出去了」是最原始的诉求，用户需要能当场声明、立刻生效。
//
// 与仿真词典的分工：词典管「真值 → 你指定的假值」（改的是替换结果），
// 登记表管「这个值必须被认出来」（改的是检测结果）。登记表是更基础的一层，
// 词典要生效也得先检测得到。二者可以叠加：登记 + 词典 = 指定类型、指定假值。
//
// 安全约定：
//   - 登记值只存在于本包与独立的登记表文件里，不进 config.Config、不进配置摘要、
//     不进审计（audit.log_pii 默认 false 是另一道闸）。
//   - 校验错误只报类型 + 下标，绝不回显值——启动期校验失败是要写日志的。
//   - 匹配对 ASCII 大小写不敏感（邮箱 / URL 的常见写法），CJK 逐字节精确；
//     折叠函数长度不变，因此偏移能 1:1 映射回原文。
package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"gateway/pkg/types"

	"gopkg.in/yaml.v3"
)

// entry 一条待匹配的登记值。
type entry struct {
	typeName string
	folded   string // foldASCII 后的值；长度与原值相同，可直接按字节比
}

// Registry 线程安全的登记表。
//
// 读路径（Scan）无锁竞争：Set 整体替换 entries / buckets，读者在 RLock 下取到
// 切片头后即可脱锁使用，旧快照不被原地改写。
type Registry struct {
	mu       sync.RWMutex
	byType   map[string][]string
	entries  []entry
	buckets  map[byte][]int
	gen      uint64
	onChange []func()
}

// New 构造空登记表。
func New() *Registry {
	return &Registry{byType: map[string][]string{}, buckets: map[byte][]int{}}
}

// Set 整体替换登记内容并通知订阅者（线程安全）。
//
// 入参应已通过 Validate；本方法仍会做规范化（去空白 / 同类型内按折叠值去重），
// 不会因为重复项出问题。
func (r *Registry) Set(values map[string][]string) {
	byType := normalize(values)
	entries := make([]entry, 0, len(byType))
	buckets := map[byte][]int{}
	// 类型名排序，保证 entries 顺序确定（便于测试与排查）。
	typeNames := make([]string, 0, len(byType))
	for tp := range byType {
		typeNames = append(typeNames, tp)
	}
	sort.Strings(typeNames)
	for _, tp := range typeNames {
		for _, v := range byType[tp] {
			idx := len(entries)
			f := foldASCII(v)
			entries = append(entries, entry{typeName: tp, folded: f})
			buckets[f[0]] = append(buckets[f[0]], idx)
		}
	}

	r.mu.Lock()
	r.byType = byType
	r.entries = entries
	r.buckets = buckets
	r.gen++
	cbs := make([]func(), len(r.onChange))
	copy(cbs, r.onChange)
	r.mu.Unlock()

	// 回调在锁外执行：订阅者会去刷检测缓存（另有自己的锁），
	// 放在锁内既容易锁序倒挂，也让 Set 的临界区无谓变长。
	for _, fn := range cbs {
		fn()
	}
}

// Get 返回登记内容的深拷贝（面板 / 导出用）。
func (r *Registry) Get() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]string, len(r.byType))
	for tp, vs := range r.byType {
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[tp] = cp
	}
	return out
}

// Len 登记值总条数。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, vs := range r.byType {
		n += len(vs)
	}
	return n
}

// Generation 内容版本号，每次 Set 自增（0 表示从未设置过）。
func (r *Registry) Generation() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gen
}

// OnChange 注册变更回调（线程安全）。
//
// 用途：登记表一变，已缓存的检测结果就过期了——「刚补登一个值再重发同一句话」
// 恰恰是本功能的主用法，缓存不失效就等于没生效。回调里刷掉检测缓存。
func (r *Registry) OnChange(fn func()) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.onChange = append(r.onChange, fn)
	r.mu.Unlock()
}

// Scan 扫描文本中所有登记值，返回按 start 升序、互不重叠的实体。
//
// 同一起点取最长匹配（登记了「张三」和「张三丰」时正文里的「张三丰」算一条）。
// 重叠时保留先出现的（起点更小），与替换阶段的 sanitizeEntities 规则一致。
func (r *Registry) Scan(text string) []types.Entity {
	if text == "" {
		return nil
	}
	r.mu.RLock()
	entries, buckets := r.entries, r.buckets
	r.mu.RUnlock()
	if len(entries) == 0 {
		return nil
	}

	folded := foldASCII(text)
	var hits []types.Entity
	for i := 0; i < len(folded); i++ {
		c := folded[i]
		if c >= 0x80 && c < 0xC0 {
			// UTF-8 续字节：合法字符串里不可能是任何值的起点，顺带跳过中文正文
			// 里占多数的字节。
			continue
		}
		best := -1
		bestLen := 0
		for _, idx := range buckets[c] {
			e := entries[idx]
			if len(e.folded) > len(folded)-i {
				continue
			}
			if folded[i:i+len(e.folded)] != e.folded {
				continue
			}
			if len(e.folded) > bestLen {
				best, bestLen = idx, len(e.folded)
			}
		}
		if best < 0 {
			continue
		}
		// Value 取原文切片而非登记值：ASCII 大小写可能不同，下游（替换 / 面板）
		// 要的是正文里的那段。
		hits = append(hits, types.Entity{
			Type:  entries[best].typeName,
			Value: text[i : i+bestLen],
			Start: i,
			End:   i + bestLen,
			Score: 1,
		})
		i += bestLen - 1
	}
	// hits 已按起点升序、每点至多一条，贪心去重叠即可（保留先出现的那条）。
	out := hits[:0]
	for _, e := range hits {
		if len(out) > 0 && e.Start < out[len(out)-1].End {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Merge 把登记命中并入内层检测结果，用与替换阶段完全相同的重叠规则
// （start 升序、同起点长优先、贪心取不重叠）。
//
// 登记命中排在内层结果之前：同区间两边都报时，稳定排序 + 贪心会让登记的
// 类型胜出——用户显式声明的类型比模型猜的更可信。
func Merge(hits, inner []types.Entity) []types.Entity {
	if len(hits) == 0 {
		return inner // 无命中时原样返回，不干扰既有检测行为
	}
	all := make([]types.Entity, 0, len(hits)+len(inner))
	all = append(all, hits...)
	all = append(all, inner...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].End > all[j].End
	})
	res := make([]types.Entity, 0, len(all))
	for _, e := range all {
		if len(res) > 0 && e.Start < res[len(res)-1].End {
			continue
		}
		res = append(res, e)
	}
	return res
}

// foldASCII 把 A-Z 折成小写，其余字节原样保留。
//
// 精度取舍：只折 ASCII 字母，长度严格不变，因此偏移可直接映射回原文；
// CJK 逐字节精确比对，不会被误折。够覆盖 email / url / 大小写混写的英文名，
// 又不引入 Unicode 折叠那套（长度可变、会破坏偏移）的复杂度。
func foldASCII(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// normalize 去空白、丢弃空值、同类型内按折叠值去重（保序）。
func normalize(values map[string][]string) map[string][]string {
	out := make(map[string][]string, len(values))
	for tp, vs := range values {
		seen := make(map[string]bool, len(vs))
		uniq := make([]string, 0, len(vs))
		for _, v := range vs {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			f := foldASCII(v)
			if seen[f] {
				continue
			}
			seen[f] = true
			uniq = append(uniq, v)
		}
		if len(uniq) > 0 {
			out[tp] = uniq
		}
	}
	return out
}

// Validate 校验登记表内容。
//
// 错误信息只含类型与下标，不含登记值本身：启动期校验失败会被写进日志 /
// journald，不能在排查信息里把用户 PII 再泄露一遍。面板侧的同类检查在前端做，
// 那里是用户自己的浏览器，可以显示值。
func Validate(values map[string][]string) error {
	typeNames := make([]string, 0, len(values))
	for tp := range values {
		typeNames = append(typeNames, tp)
	}
	sort.Strings(typeNames) // 排序 → 报错顺序确定，便于测试与复现

	seen := make(map[string]string, len(values)) // 折叠值 → 已占用类型
	for _, tp := range typeNames {
		if !types.IsKnownType(tp) {
			return fmt.Errorf("registry: unknown entity type %q", tp)
		}
		for i, raw := range values[tp] {
			v := strings.TrimSpace(raw)
			switch {
			case v == "":
				return fmt.Errorf("registry: %s[%d]: empty value", tp, i)
			case v != raw:
				// 前后空白几乎总是从表格 / 聊天窗口粘贴带进来的，静默 trim 会让
				// 用户以为登记了「  张三  」也能命中，不如直接报错。
				return fmt.Errorf("registry: %s[%d]: value has leading/trailing whitespace", tp, i)
			case utf8.RuneCountInString(v) < 2:
				// 单字符值（如一个「我」）会把正文里每一次出现都当成 PII，
				// 静默毁掉整段文本，必须拦住。
				return fmt.Errorf("registry: %s[%d]: value too short (at least 2 characters)", tp, i)
			}
			f := foldASCII(v)
			if prev, dup := seen[f]; dup {
				if prev == tp {
					continue // 同类型内重复：无害，规范化阶段去掉
				}
				// 跨类型同一个值 → 该按哪个类型脱敏是歧义的，让用户自己定。
				return fmt.Errorf("registry: %s[%d]: value already registered as %s", tp, i, prev)
			}
			seen[f] = tp
		}
	}
	return nil
}

// fileHeader 写回文件时附带的说明。登记表是明文 PII，落盘位置与是否入库都得说清楚。
const fileHeader = `# LLMate Gate 登记表
#
# 这里登记的值一律视为对应类型的 PII：命中即脱敏，且绕过阈值过滤以保召回。
# 格式：entity_type: [值, ...]
#
# ⚠️ 本文件是明文 PII：不要提交进版本库、不要放共享目录、不要随配置一起分发。
#    由调试面板「规则」页维护，也可手工编辑后重启加载。
`

// LoadFile 读取登记表 YAML。
func LoadFile(path string) (map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string][]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("registry: parse %s: %w", path, redactYAMLError(err))
	}
	return m, nil
}

// SaveFile 原子写回登记表 YAML（文件 0600、目录 0700）。
//
// 原子：先写同目录临时文件再 rename，避免面板保存到一半崩了留下半截文件，
// 那样下次启动加载失败、整张登记表全丢。
func SaveFile(path string, values map[string][]string) error {
	out, err := yaml.Marshal(normalize(values))
	if err != nil {
		return fmt.Errorf("registry: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("registry: create dir: %w", err)
	}
	f, err := os.CreateTemp(dir, ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("registry: create temp: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // rename 成功后 os.Remove 会报 not exist，忽略
	if _, err := f.Write(append([]byte(fileHeader), out...)); err != nil {
		_ = f.Close()
		return fmt.Errorf("registry: write: %w", err)
	}
	// os.CreateTemp 建的文件是 0600，rename 覆盖后目标也是 0600。
	if err := f.Close(); err != nil {
		return fmt.Errorf("registry: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("registry: rename: %w", err)
	}
	return nil
}

// backtickRe 匹配 yaml.v3 错误消息里回显的值片段。
var backtickRe = regexp.MustCompile("`[^`]*`")

// redactYAMLError 抹掉 yaml.v3 错误里回显的值。
//
// yaml.v3 在类型不匹配时会把值截断后写进消息，形如
// “line 3: cannot unmarshal !!str `张三` into []string”——这条错误会进启动日志。
// 换成占位符后行号与错误类型都留着，排查够用，值不落地。
func redactYAMLError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(backtickRe.ReplaceAllString(err.Error(), "`…`"))
}
