package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gateway/internal/detector"
	"gateway/pkg/types"

	"github.com/stretchr/testify/require"
)

// newReg 构造已装载的登记表。
func newReg(t *testing.T, values map[string][]string) *Registry {
	t.Helper()
	require.NoError(t, Validate(values))
	r := New()
	r.Set(values)
	return r
}

// ---------- 匹配 ----------

// TestScan_ASCIICaseInsensitive 邮箱 / URL 的常见写法差异必须命中，且 Value 取原文。
func TestScan_ASCIICaseInsensitive(t *testing.T) {
	r := newReg(t, map[string][]string{types.EntityEmail: {"ZhangSan@Example.COM"}})
	text := "联系 Zhangsan@example.com 或 ZHANGSAN@EXAMPLE.COM 都行"
	hits := r.Scan(text)
	require.Len(t, hits, 2)
	for _, h := range hits {
		require.Equal(t, types.EntityEmail, h.Type)
		require.Equal(t, text[h.Start:h.End], h.Value, "Value 必须是正文里的那一段")
		require.Equal(t, 1.0, h.Score, "显式登记即最高置信度")
	}
	require.Equal(t, "Zhangsan@example.com", hits[0].Value)
	require.Equal(t, "ZHANGSAN@EXAMPLE.COM", hits[1].Value)
}

// TestScan_CJKExact 中文不做任何折叠，逐字节精确；长度不变才能保证偏移可映射回原文。
func TestScan_CJKExact(t *testing.T) {
	r := newReg(t, map[string][]string{types.EntityPersonName: {"张三丰"}})

	hits := r.Scan("今天见到张三丰了")
	require.Len(t, hits, 1)
	require.Equal(t, "张三丰", hits[0].Value)
	// 前四字各 3 字节 → start 必须是字节偏移 12
	require.Equal(t, 12, hits[0].Start)
	require.Equal(t, 12+9, hits[0].End)

	require.Empty(t, r.Scan("张三 丰"), "中间隔了字符不算命中")
	require.Empty(t, r.Scan("张"))
	require.Empty(t, r.Scan("李四"))
}

// TestScan_LongestMatchAtSameStart 同一起点取最长：登记了「张三」和「张三丰」时正文
// 里的「张三丰」只产出一条，类型取更长的那个。
func TestScan_LongestMatchAtSameStart(t *testing.T) {
	r := newReg(t, map[string][]string{
		types.EntityPersonName: {"张三", "张三丰"},
		types.EntityAddress:    {"张三丰路"},
	})
	hits := r.Scan("张三丰")
	require.Len(t, hits, 1)
	require.Equal(t, "张三丰", hits[0].Value)
	require.Equal(t, types.EntityPersonName, hits[0].Type)

	// 正文里同时出现更长的登记值时，长的那条胜出
	hits = r.Scan("走张三丰路")
	require.Len(t, hits, 1)
	require.Equal(t, "张三丰路", hits[0].Value)
	require.Equal(t, types.EntityAddress, hits[0].Type)
}

// TestScan_OverlapKeepsEarlierStart 重叠时保留先出现的，与替换阶段 sanitizeEntities 同规则。
func TestScan_OverlapKeepsEarlierStart(t *testing.T) {
	r := newReg(t, map[string][]string{
		types.EntityPersonName: {"ABCD"},
		types.EntityEmail:      {"CDEF"},
	})
	hits := r.Scan("ABCDEF")
	require.Len(t, hits, 1)
	require.Equal(t, "ABCD", hits[0].Value)
}

// TestScan_NoHitAtBoundary 长度不足 / 只差一个字符都不能误报。
func TestScan_NoHitAtBoundary(t *testing.T) {
	r := newReg(t, map[string][]string{types.EntityPersonName: {"abcdef"}})
	require.Empty(t, r.Scan("abcde"))
	require.Empty(t, r.Scan("bcdef"))
	require.Empty(t, r.Scan(""))
	require.Empty(t, New().Scan("abcdef"), "空登记表什么都不匹配")

	hits := r.Scan("abcdef")
	require.Len(t, hits, 1)
	require.Equal(t, "abcdef", hits[0].Value)
}

// TestScan_MultiByteTextOffsets 中文正文里命中 ASCII 值时偏移仍然正确。
func TestScan_MultiByteTextOffsets(t *testing.T) {
	r := newReg(t, map[string][]string{types.EntityEmail: {"a@b.cn"}})
	text := "邮箱：张三 a@b.cn 请查收"
	hits := r.Scan(text)
	require.Len(t, hits, 1)
	require.Equal(t, text[hits[0].Start:hits[0].End], "a@b.cn")
}

// TestScan_ValueRegisteredWithSpaces 值内部允许空格（英文姓名），只拒绝首尾空白。
func TestScan_ValueRegisteredWithSpaces(t *testing.T) {
	r := newReg(t, map[string][]string{types.EntityPersonName: {"Zhang San"}})
	hits := r.Scan("ask zhang san please")
	require.Len(t, hits, 1)
	require.Equal(t, "zhang san", hits[0].Value)
}

// ---------- 合并 ----------

// TestMerge_RegistryTypeWinsOnSameSpan 同区间两边都报时，登记的显式类型胜出。
func TestMerge_RegistryTypeWinsOnSameSpan(t *testing.T) {
	hits := []types.Entity{{Type: types.EntityPersonName, Value: "王小明", Start: 0, End: 9, Score: 1}}
	inner := []types.Entity{{Type: types.EntityAddress, Value: "王小明", Start: 0, End: 9, Score: 0.7}}
	got := Merge(hits, inner)
	require.Len(t, got, 1)
	require.Equal(t, types.EntityPersonName, got[0].Type)
}

// TestMerge_NoHitsReturnsInner 无命中必须原样返回，保持既有检测行为不变。
func TestMerge_NoHitsReturnsInner(t *testing.T) {
	inner := []types.Entity{{Type: types.EntityPhone, Start: 0, End: 11}}
	got := Merge(nil, inner)
	require.Len(t, got, 1)
	require.Equal(t, inner[0], got[0])
	require.Equal(t, types.EntityPhone, got[0].Type)
}

// TestMerge_LongerInnerWins 模型检出了包含登记值的更长实体时，不应被切碎。
func TestMerge_LongerInnerWins(t *testing.T) {
	inner := []types.Entity{{Type: types.EntityAddress, Value: "北京市朝阳区建国路88号", Start: 0, End: 30, Score: 0.9}}
	hits := []types.Entity{{Type: types.EntityAddress, Value: "建国路88号", Start: 15, End: 30, Score: 1}}
	got := Merge(hits, inner)
	require.Len(t, got, 1)
	require.Equal(t, "北京市朝阳区建国路88号", got[0].Value)
}

// TestMerge_DisjointBothKept 互不重叠的命中与内层结果都保留，并按 start 升序。
func TestMerge_DisjointBothKept(t *testing.T) {
	hits := []types.Entity{{Type: types.EntityPersonName, Value: "王小明", Start: 20, End: 29, Score: 1}}
	inner := []types.Entity{{Type: types.EntityPhone, Value: "13800138000", Start: 0, End: 11, Score: 0.9}}
	got := Merge(hits, inner)
	require.Len(t, got, 2)
	require.Equal(t, 0, got[0].Start)
	require.Equal(t, 20, got[1].Start)
}

// ---------- 校验 ----------

// TestValidate 登记表校验规则。
func TestValidate(t *testing.T) {
	t.Run("合法", func(t *testing.T) {
		require.NoError(t, Validate(map[string][]string{
			types.EntityPersonName: {"王小明", "李四"},
			types.EntityEmail:      {"me@corp.cn"},
		}))
	})
	t.Run("空表", func(t *testing.T) {
		require.NoError(t, Validate(nil))
		require.NoError(t, Validate(map[string][]string{}))
		require.NoError(t, Validate(map[string][]string{types.EntityPersonName: {}}))
	})
	t.Run("未知类型", func(t *testing.T) {
		err := Validate(map[string][]string{"zh_person": {"王小明"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown entity type")
	})
	t.Run("单字符值被拒", func(t *testing.T) {
		// 登记一个「我」会把正文里每一次出现都当 PII，静默毁掉整段文本
		require.Error(t, Validate(map[string][]string{types.EntityPersonName: {"我"}}))
		require.Error(t, Validate(map[string][]string{types.EntityPersonName: {"a"}}))
		require.NoError(t, Validate(map[string][]string{types.EntityPersonName: {"ab"}}))
	})
	t.Run("首尾空白被拒", func(t *testing.T) {
		err := Validate(map[string][]string{types.EntityPersonName: {" 王小明 "}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "whitespace")
	})
	t.Run("空值被拒", func(t *testing.T) {
		require.Error(t, Validate(map[string][]string{types.EntityPersonName: {"   "}}))
	})
	t.Run("同类型内重复放行", func(t *testing.T) {
		require.NoError(t, Validate(map[string][]string{types.EntityPersonName: {"王小明", "王小明"}}))
	})
	t.Run("跨类型重复被拒", func(t *testing.T) {
		err := Validate(map[string][]string{
			types.EntityPersonName: {"王小明"},
			types.EntityAddress:    {"王小明"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "already registered as")
	})
	t.Run("跨类型重复_大小写不敏感", func(t *testing.T) {
		err := Validate(map[string][]string{
			types.EntityEmail:     {"Me@Corp.cn"},
			types.EntityIPAddress: {"me@corp.CN"},
		})
		require.Error(t, err)
	})
	t.Run("错误信息不回显登记值", func(t *testing.T) {
		// 启动期校验失败会写日志 / journald，排查信息里不能再把 PII 泄露一遍
		err := Validate(map[string][]string{
			types.EntityPersonName: {"王小明"},
			types.EntityAddress:    {"王小明"},
		})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "王小明")

		err = Validate(map[string][]string{types.EntityPersonName: {"我"}})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "我")
	})
}

// TestSet_Normalizes 装载时去空白、同类型内按折叠值去重。
func TestSet_Normalizes(t *testing.T) {
	r := New()
	r.Set(map[string][]string{
		types.EntityEmail: {"Me@Corp.cn", "me@corp.cn", "  me@corp.cn  ", ""},
	})
	require.Equal(t, 1, r.Len(), "ASCII 大小写不同的同一个邮箱只留一条")
	require.Equal(t, []string{"Me@Corp.cn"}, r.Get()[types.EntityEmail], "保留首次出现的大小写")
	require.Empty(t, r.Get()[types.EntityAddress])

	// 去重后仍然只产出一条命中
	require.Len(t, r.Scan("write to me@corp.cn"), 1)
}

// ---------- 热加载 / 并发 ----------

// TestSet_GenerationAndOnChange 每次 Set 递增版本号并通知订阅者。
func TestSet_GenerationAndOnChange(t *testing.T) {
	r := New()
	require.Equal(t, uint64(0), r.Generation())

	calls := 0
	r.OnChange(func() { calls++ })

	r.Set(map[string][]string{types.EntityPersonName: {"王小明"}})
	require.Equal(t, uint64(1), r.Generation())
	require.Equal(t, 1, calls)

	r.Set(map[string][]string{types.EntityPersonName: {"王小明", "李四"}})
	require.Equal(t, uint64(2), r.Generation())
	require.Equal(t, 2, calls)
	require.Equal(t, 2, r.Len())
}

// TestSet_Concurrent 并发读写不得 panic / 不得读到半更新的表（-race 下跑）。
func TestSet_Concurrent(t *testing.T) {
	r := New()
	r.Set(map[string][]string{types.EntityPersonName: {"王小明"}})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if i%2 == 0 {
					r.Set(map[string][]string{
						types.EntityPersonName: {"王小明", "李四"},
						types.EntityEmail:      {"me@corp.cn"},
					})
				} else {
					_ = r.Scan("王小明 写了 me@corp.cn 和 李四")
					_ = r.Get()
					_ = r.Len()
				}
			}
		}(i)
	}
	wg.Wait()

	// 每一条命中都必须是完整、自洽的
	for _, h := range r.Scan("王小明 写了 me@corp.cn") {
		require.NotEmpty(t, h.Type)
		require.NotEmpty(t, h.Value)
	}
}

// ---------- 文件 ----------

// TestSaveLoad_RoundTrip 写回再读回内容一致，且文件权限收紧到 0600。
func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registry.yaml")
	want := map[string][]string{
		types.EntityPersonName: {"王小明", "李四"},
		types.EntityEmail:      {"me@corp.cn"},
	}
	require.NoError(t, SaveFile(path, want))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "登记表是明文 PII，权限必须收紧")

	got, err := LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestSaveFile_WriteExportMerge 面板里改完再存，先前的内容不会丢。
func TestSaveFile_ExportMerge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, SaveFile(path, map[string][]string{
		types.EntityPersonName: {"王小明"},
	}))
	require.NoError(t, SaveFile(path, map[string][]string{
		types.EntityPersonName: {"王小明", "李四"},
		types.EntityAddress:    {"北京市朝阳区建国路88号"},
	}))
	got, err := LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, []string{"王小明", "李四"}, got[types.EntityPersonName], "列表顺序按写入顺序保留")
	require.Len(t, got[types.EntityAddress], 1)
}

// TestLoadFile_Missing 文件不存在时错误能被 os.IsNotExist 识别（首次运行走空表）。
func TestLoadFile_Missing(t *testing.T) {
	_, err := LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	require.Error(t, err)
	require.True(t, os.IsNotExist(err))
}

// TestLoadFile_BadYAMLDoesNotLeakValue yaml.v3 的类型错误会把值截断后带进消息，
// 这条错误会进启动日志，必须抹掉。
func TestLoadFile_BadYAMLDoesNotLeakValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte("zh_person_name: 王小明\n"), 0o600))
	_, err := LoadFile(path)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "王小明", "yaml 错误里的值必须被抹掉")
	require.Contains(t, err.Error(), "line", "行号要留着，不然没法排查")
}

// TestSaveFile_Empty 清空登记表也要能落盘。
func TestSaveFile_Empty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	require.NoError(t, SaveFile(path, map[string][]string{}))
	got, err := LoadFile(path)
	require.NoError(t, err)
	require.Empty(t, got)
}

// ---------- Wrap ----------

type stubDetector struct {
	detector.Client // 未实现的方法由内嵌接口兜底
	mu              sync.Mutex
	resp            *types.DetectResponse
	err             error
	calls           int
	batchCalls      int
}

func (s *stubDetector) Detect(context.Context, *types.DetectRequest) (*types.DetectResponse, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.resp, s.err
}

func (s *stubDetector) DetectBatch(_ context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error) {
	s.mu.Lock()
	s.batchCalls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*types.DetectResponse, len(reqs))
	for i := range reqs {
		out[i] = s.resp
	}
	return out, nil
}

func (s *stubDetector) Name() string { return "stub" }

// TestWrap_Detect 登记命中与内层结果合并。
func TestWrap_Detect(t *testing.T) {
	// "王小明 13800138000"：姓名占 [0,9)，空格 1 字节，手机号占 [10,21)
	inner := &stubDetector{resp: &types.DetectResponse{
		Entities:  []types.Entity{{Type: types.EntityPhone, Value: "13800138000", Start: 10, End: 21, Score: 0.9}},
		LatencyMs: 7,
	}}
	c := Wrap(inner, newReg(t, map[string][]string{types.EntityPersonName: {"王小明"}}))
	require.NotNil(t, c)

	resp, err := c.Detect(context.Background(), &types.DetectRequest{Text: "王小明 13800138000"})
	require.NoError(t, err)
	require.Len(t, resp.Entities, 2)
	require.Equal(t, types.EntityPersonName, resp.Entities[0].Type, "登记命中按 start 升序排在前面")
	require.Equal(t, types.EntityPhone, resp.Entities[1].Type)
	require.Equal(t, int64(7), resp.LatencyMs)
	require.Equal(t, "stub", c.Name())
}

// TestWrap_NoHitKeepsInner 没有登记命中时原样返回内层结果对象。
func TestWrap_NoHitKeepsInner(t *testing.T) {
	inner := &stubDetector{resp: &types.DetectResponse{Entities: []types.Entity{{Type: types.EntityPhone, Start: 0, End: 11, Score: 1}}}}
	c := Wrap(inner, newReg(t, map[string][]string{types.EntityPersonName: {"王小明"}}))
	resp, err := c.Detect(context.Background(), &types.DetectRequest{Text: "13800138000"})
	require.NoError(t, err)
	require.Same(t, inner.resp, resp, "无命中不该重建对象")
}

// TestWrap_ErrorPropagates 内层报错必须原样上抛，交给 fail-closed 决策。
func TestWrap_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("engine down")
	inner := &stubDetector{err: sentinel}
	c := Wrap(inner, newReg(t, map[string][]string{types.EntityPersonName: {"王小明"}}))
	_, err := c.Detect(context.Background(), &types.DetectRequest{Text: "王小明"})
	require.ErrorIs(t, err, sentinel)
	_, err = c.DetectBatch(context.Background(), []*types.DetectRequest{{Text: "王小明"}})
	require.ErrorIs(t, err, sentinel)
}

// TestWrap_NilRegistryOrInner 未启用登记表时必须是零改动：连包装都不包。
func TestWrap_NilRegistryOrInner(t *testing.T) {
	inner := &stubDetector{}
	require.Equal(t, detector.Client(inner), Wrap(inner, nil), "reg 为 nil 应原样返回内层")
	require.Nil(t, Wrap(nil, New()))
}

// TestWrap_Batch DetectBatch 逐条补召回。
func TestWrap_Batch(t *testing.T) {
	inner := &stubDetector{resp: &types.DetectResponse{
		Entities: []types.Entity{{Type: types.EntityPhone, Value: "13800138000", Start: 10, End: 21, Score: 0.9}},
	}}
	c := Wrap(inner, newReg(t, map[string][]string{types.EntityPersonName: {"王小明"}}))
	resps, err := c.DetectBatch(context.Background(), []*types.DetectRequest{
		{Text: "王小明 13800138000"},
		{Text: "13800138000"},
	})
	require.NoError(t, err)
	require.Len(t, resps, 2)
	require.Len(t, resps[0].Entities, 2)
	require.Len(t, resps[1].Entities, 1)
	require.Equal(t, 1, inner.batchCalls)
}

// TestWrap_RegisteredValueBypassesTypeFilter 登记命中不受内层检测器的类型白名单
// （detector.WithEntityFilter）影响——这是召回保证，也是把 Wrap 放在内层过滤之后的理由。
func TestWrap_RegisteredValueBypassesTypeFilter(t *testing.T) {
	// 内层只允许 zh_phone，中文姓名被白名单刷掉
	eng := detector.NewRegexEngine(detector.WithEntityFilter([]string{types.EntityPhone}))
	c := Wrap(eng, newReg(t, map[string][]string{types.EntityPersonName: {"王小明"}}))

	raw, err := eng.Detect(context.Background(), &types.DetectRequest{Text: "我叫王小明，电话13800138000"})
	require.NoError(t, err)
	for _, e := range raw.Entities {
		require.Equal(t, types.EntityPhone, e.Type, "前提：内层白名单确实刷掉了姓名")
	}

	resp, err := c.Detect(context.Background(), &types.DetectRequest{Text: "我叫王小明，电话13800138000"})
	require.NoError(t, err)
	var gotName bool
	for _, e := range resp.Entities {
		if e.Type == types.EntityPersonName {
			gotName = true
		}
	}
	require.True(t, gotName, "登记表必须补上被白名单刷掉的召回")
}

// TestScan_LargeRegistry 登记量大时行为不变。
func TestScan_LargeRegistry(t *testing.T) {
	vals := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		vals = append(vals, fmt.Sprintf("name_%03d_%s", i, strings.Repeat("x", i%7+2)))
	}
	r := newReg(t, map[string][]string{types.EntityPersonName: vals})
	require.Empty(t, r.Scan(""))
	require.Empty(t, r.Scan("name_xxx"), "长度不足不命中")

	hits := r.Scan("prefix " + vals[7] + " suffix")
	require.Len(t, hits, 1)
	require.Equal(t, vals[7], hits[0].Value)
}
