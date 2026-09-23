package judge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gateway/pkg/types"
)

// 本文件是给**用户**用的自测工具：拿一批带标注的动作打自己的模型，得到
// 漏报 / 误报口径。
//
// 它借鉴 cn-pii-bench 的方法论，但样本形态不同：那边标的是「文本里的 PII 实体
// 与偏移」，这边标的是「动作 → 期望类别」。两者不能直接互用——判断层要的是
// ActionDescriptor 级别的标注，评测集必须重建，这是 J6 的真实工作量所在。
//
// 为什么这件事不可省：判断层没有独立评测集，就复现不了「诚实口径」，
// 于是「模型接上去了、看起来还行」会成为唯一的结论——而那不是一个结论。

// Case 一条带标注的评测样本（JSONL 一行一条）。
type Case struct {
	Name string `json:"name"`
	// Descriptor 待判断的动作。
	Descriptor types.ActionDescriptor `json:"descriptor"`
	// Want 期望类别（闭集）。用它推导「该不该拦」：benign → 不该拦，其余 → 该拦。
	Want types.ActionCategory `json:"want"`
	// Note 标注理由（报告里带出来，便于复查标注本身对不对）。
	Note string `json:"note,omitempty"`
}

// Validate 标注是否合法（闭集外的期望值会让统计口径失真）。
func (c Case) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("judge: case name is required")
	}
	if !types.IsKnownCategory(c.Want) {
		return fmt.Errorf("judge: case %q has unknown want category %q", c.Name, c.Want)
	}
	return c.Descriptor.Validate()
}

// BenchReport 评测结果。
//
// 两个口径分开列，因为它们回答不同的问题：
//
//	动作级（MissRate / FalseAlarmRate）——「接上它之后，实际效果如何」
//	类别级（CategoryAccuracy）      ——「它对行为的理解有多准」
//
// 前者决定能不能上线，后者只是诊断参考。只看后者最容易自我感觉良好：
// 一个把所有东西都判成 archive 的模型，类别命中率可能不低，但动作级全是误报。
type BenchReport struct {
	Engine   string `json:"engine"`
	Cases    int    `json:"cases"`
	Scored   int    `json:"scored"`
	Errors   int    `json:"errors"`
	Unknowns int    `json:"unknowns"`

	// 动作级：漏报 = 该拦没拦；误报 = 不该拦却拦了。
	//
	// WantDanger / WantBenign 是两边的分母，显式记下来：报告里用 Scored 当分母
	// 会把两个方向的比率一起算错，而这种错很隐蔽（数字看着合理）。
	FalseNegatives int     `json:"false_negatives"`
	FalsePositives int     `json:"false_positives"`
	WantDanger     int     `json:"want_danger"`
	WantBenign     int     `json:"want_benign"`
	MissRate       float64 `json:"miss_rate"`
	FalseAlarmRate float64 `json:"false_alarm_rate"`

	// 类别级。
	CategoryHits     int            `json:"category_hits"`
	CategoryAccuracy float64        `json:"category_accuracy"`
	PerCategory      map[string]int `json:"per_category_hits"`

	Failures []string `json:"failures,omitempty"`
	P50Ms    int64    `json:"p50_ms"`
	P95Ms    int64    `json:"p95_ms"`
}

// dangerousCategory 该类别是否属于「该拦」的一侧。
//
// `unknown` 归到「该拦」是有意的：一个连类别都说不出的动作，在真实链路上会走
// review（交人）。把它算进「不该拦」会让漏报率被系统性低估。
func dangerousCategory(c types.ActionCategory) bool {
	return c != types.CatBenign
}

// Run 跑一批样本。阈值固定用内置缺省（与探针同理：评测的是后端本身）。
func Run(ctx context.Context, j Judge, cases []Case) BenchReport {
	mp := NewMapper(j.Name(), types.DefaultThresholds(), j.Capabilities().GivesConfidence)
	r := BenchReport{Engine: j.Name(), Cases: len(cases), PerCategory: map[string]int{}}

	var latencies []int64
	var wantDanger, wantBenign int
	for i, c := range cases {
		if err := c.Validate(); err != nil {
			r.Errors++
			r.Failures = append(r.Failures, fmt.Sprintf("样本 %d 标注非法：%v", i, err))
			continue
		}
		start := time.Now()
		ev, err := j.Judge(ctx, c.Descriptor)
		latencies = append(latencies, time.Since(start).Milliseconds())
		if err != nil {
			r.Errors++
			r.Failures = append(r.Failures, fmt.Sprintf("%s: 判定失败：%v", c.Name, err))
			continue
		}
		r.Scored++
		if ev.Category == types.CatUnknown {
			r.Unknowns++
		}
		if ev.Category == c.Want {
			r.CategoryHits++
			r.PerCategory[string(c.Want)]++
		}
		act := mp.Map(ev).Action
		if dangerousCategory(c.Want) {
			wantDanger++
			if act == types.ActionAllow {
				r.FalseNegatives++
				r.Failures = append(r.Failures, fmt.Sprintf("漏报 %s：期望 %s，得到 %s(%s sev=%.2f)",
					c.Name, c.Want, types.ActionAllow, ev.Category, ev.Severity))
			}
		} else {
			wantBenign++
			if act != types.ActionAllow {
				r.FalsePositives++
				r.Failures = append(r.Failures, fmt.Sprintf("误报 %s：期望 benign，得到 %s(%s sev=%.2f)",
					c.Name, act, ev.Category, ev.Severity))
			}
		}
	}

	r.WantDanger, r.WantBenign = wantDanger, wantBenign
	if wantDanger > 0 {
		r.MissRate = float64(r.FalseNegatives) / float64(wantDanger)
	}
	if wantBenign > 0 {
		r.FalseAlarmRate = float64(r.FalsePositives) / float64(wantBenign)
	}
	if r.Scored > 0 {
		r.CategoryAccuracy = float64(r.CategoryHits) / float64(r.Scored)
	}
	r.P50Ms, r.P95Ms = percentiles(latencies)
	return r
}

// LoadCases 从 JSONL 读取评测集。空行与以 # 开头的行被忽略（便于写注释与分组）。
func LoadCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []Case
	sc := bufio.NewScanner(f)
	// 单行可能是一条长命令 + 长标注，默认 64KB 不够。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(text), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, line, err)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no cases loaded", path)
	}
	return out, nil
}

// ProbeCasesFromBench 把评测集的前若干条转成探针，用于「先跑塌缩检测」。
//
// 用途：用户手上有一批真实样本时，可以先看模型有没有整体性失效，
// 再谈逐条准确率——顺序反了会把「模型坏了」误读成「准确率不够高」。
func ProbeCasesFromBench(cases []Case, limit int) []ProbeCase {
	if limit <= 0 || limit > len(cases) {
		limit = len(cases)
	}
	out := make([]ProbeCase, 0, limit)
	for i := 0; i < limit; i++ {
		kind := ProbeDangerous
		if cases[i].Want == types.CatBenign {
			kind = ProbeBenign
		}
		out = append(out, ProbeCase{
			Name: cases[i].Name, Kind: kind,
			Desc: cases[i].Descriptor, Note: cases[i].Note,
		})
	}
	return out
}

// Report 把报告渲染成可读文本（CLI 输出用）。
func (r BenchReport) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "后端 %s：%d 条样本，成功判定 %d，失败 %d，判不了(unknown) %d\n",
		r.Engine, r.Cases, r.Scored, r.Errors, r.Unknowns)
	fmt.Fprintf(&b, "动作级  漏报率 %.3f（%d/%d 该拦）  误报率 %.3f（%d/%d 不该拦）\n",
		r.MissRate, r.FalseNegatives, r.WantDanger,
		r.FalseAlarmRate, r.FalsePositives, r.WantBenign)
	fmt.Fprintf(&b, "类别级  命中率 %.3f（%d/%d）\n", r.CategoryAccuracy, r.CategoryHits, r.Scored)
	fmt.Fprintf(&b, "延迟    p50 %dms  p95 %dms\n", r.P50Ms, r.P95Ms)
	if len(r.PerCategory) > 0 {
		keys := make([]string, 0, len(r.PerCategory))
		for k := range r.PerCategory {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.PerCategory[k]))
		}
		fmt.Fprintf(&b, "命中分布  %s\n", strings.Join(parts, " "))
	}
	if len(r.Failures) > 0 {
		fmt.Fprintf(&b, "问题样本（最多 20 条）：\n")
		for i, f := range r.Failures {
			if i >= 20 {
				fmt.Fprintf(&b, "  … 另有 %d 条\n", len(r.Failures)-20)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", f)
		}
	}
	return b.String()
}

// Report 探针报告的可读形式。
func (v ProbeVerdict) Report() string {
	var b strings.Builder
	verdict := "健康"
	if !v.Healthy {
		verdict = "疑似塌缩"
	}
	fmt.Fprintf(&b, "后端 %s：%d 条探针，结论 %s\n", v.Engine, v.Probes, verdict)
	fmt.Fprintf(&b, "  对无害动作的误判率 %.3f  （> %.2f 视为塌缩到「一律拦」）\n", v.FalseAlarm, collapseThreshold)
	fmt.Fprintf(&b, "  对高危动作的漏判率 %.3f  （> %.2f 视为塌缩到「一律放」）\n", v.MissRate, collapseThreshold)
	fmt.Fprintf(&b, "  判不了 %d 条，失败 %d 条，p50 %dms p95 %dms\n", v.Unknowns, v.Errors, v.P50Ms, v.P95Ms)
	if len(v.Failures) > 0 {
		fmt.Fprintf(&b, "  问题探针：\n")
		for i, f := range v.Failures {
			if i >= 10 {
				fmt.Fprintf(&b, "    … 另有 %d 条\n", len(v.Failures)-10)
				break
			}
			fmt.Fprintf(&b, "    - %s\n", f)
		}
	}
	return b.String()
}
