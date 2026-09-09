// Command bench-runner 用真实 RegexEngine 跑 cn-pii-bench 语料，输出基线指标。
//
// 这不是「四方对比」（那部分按用户要求暂不做），而是对 Phase 0 立项前提的
// 自测基线：在已知 ground truth 上量化内置正则引擎的精确率/召回率，
// 为 DECISION.md 提供数据支撑。
//
// 用法：
//
//	go run ./cmd/bench-runner ../../bench/fixtures/cases.jsonl
//	go run ./cmd/bench-runner --dump ../../bench/fixtures/cases.jsonl
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"gateway/internal/detector"
	"gateway/pkg/types"
)

type entity struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type caseIn struct {
	ID     string   `json:"id"`
	Subset string   `json:"subset"`
	Text   string   `json:"text"`
	Expect []entity `json:"expect"`
}

func main() {
	dump := flag.Bool("dump", false, "输出每条 case 的原始检测实体（调试用）")
	flag.Parse()
	path := "../../bench/fixtures/cases.jsonl"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	eng := detector.NewRegexEngine()
	ctx := context.Background()

	var cases []caseIn
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var c caseIn
		if err := json.Unmarshal(line, &c); err != nil {
			fmt.Fprintln(os.Stderr, "parse:", err)
			os.Exit(1)
		}
		cases = append(cases, c)
	}

	// 类型级聚合
	gtCount := map[string]int{}
	detCount := map[string]int{}
	tpCount := map[string]int{}

	// 子集级召回
	subsetFN := map[string]int{}
	subsetTP := map[string]int{}

	var totalTP, totalFP, totalFN int

	for _, c := range cases {
		resp, err := eng.Detect(ctx, &types.DetectRequest{Text: c.Text})
		if err != nil {
			fmt.Fprintln(os.Stderr, "detect:", err)
			os.Exit(1)
		}
		dets := resp.Entities

		if *dump {
			b, _ := json.MarshalIndent(dets, "", "  ")
			fmt.Printf("=== %s (%s)\n%s\nexpect: %d det: %d\n", c.ID, c.Subset, c.Text, len(c.Expect), len(dets))
			_ = b
		}

		for _, g := range c.Expect {
			gtCount[g.Type]++
			subsetFN[c.Subset]++
		}
		for _, d := range dets {
			detCount[d.Type]++
		}

		// 贪心匹配：ground truth 与 detection 按 (type + 区间重叠) 配对
		used := make([]bool, len(dets))
		for _, g := range c.Expect {
			matched := -1
			for i, d := range dets {
				if used[i] {
					continue
				}
				if d.Type != g.Type {
					continue
				}
				if d.Start < g.End && g.Start < d.End { // 区间重叠
					matched = i
					break
				}
			}
			if matched >= 0 {
				used[matched] = true
				tpCount[g.Type]++
				totalTP++
				subsetTP[c.Subset]++
			} else {
				totalFN++
			}
		}
		for i := range dets {
			if !used[i] {
				totalFP++
			}
		}
	}

	out := map[string]interface{}{
		"corpus":       path,
		"cases":        len(cases),
		"precision":    safeDiv(totalTP, totalTP+totalFP),
		"recall":       safeDiv(totalTP, totalTP+totalFN),
		"f1":           safeDiv(2*totalTP, 2*totalTP+totalFP+totalFN),
		"tp":           totalTP,
		"fp":           totalFP,
		"fn":           totalFN,
		"by_type":      aggregate(gtCount, detCount, tpCount),
		"by_subset":    subsetAggregate(subsetTP, subsetFN),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))

	// 召回率低于 0.9 视为立项风险点，非零退出便于 CI 守门
	if safeDiv(totalTP, totalTP+totalFN) < 0.9 {
		os.Exit(2)
	}
}

func aggregate(gt, det, tp map[string]int) []map[string]interface{} {
	keys := map[string]bool{}
	for k := range gt {
		keys[k] = true
	}
	for k := range det {
		keys[k] = true
	}
	var rows []map[string]interface{}
	for k := range keys {
		g := gt[k]
		d := det[k]
		t := tp[k]
		rows = append(rows, map[string]interface{}{
			"type":     k,
			"gt":       g,
			"detected": d,
			"tp":       t,
			"recall":   safeDiv(t, g),
			"prec":     safeDiv(t, d),
		})
	}
	return rows
}

func subsetAggregate(tp, fn map[string]int) []map[string]interface{} {
	keys := map[string]bool{}
	for k := range tp {
		keys[k] = true
	}
	for k := range fn {
		keys[k] = true
	}
	var rows []map[string]interface{}
	for k := range keys {
		// fn[k] 已计入该子集全部 ground truth 实体（匹配与否都 +1），故 gt = fn[k]。
		g := fn[k]
		rows = append(rows, map[string]interface{}{
			"subset": k,
			"gt":     g,
			"tp":     tp[k],
			"recall": safeDiv(tp[k], g),
		})
	}
	return rows
}

func safeDiv(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}
