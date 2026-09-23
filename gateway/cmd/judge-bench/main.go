// Command judge-bench 是判断层的自测工具：拿内置探针与（可选）自备评测集，
// 量一个判断后端的**塌缩**与**漏报/误报**。
//
// 它存在的理由是 BYOM 定位：LLMate-Gate 不预置模型，模型质量由使用者自己的
// 选型负责。那么「这个模型能不能用」就必须有一个能自己跑的工具来回答，
// 而不是靠「接上去了、看起来还行」。
//
// 用法：
//
//	# 只跑内置探针（20 条，验证模型没有整体性失效）
//	judge-bench -kind openai -base-url http://127.0.0.1:11434/v1 -model qwen2.5:7b-instruct
//
//	# 加自备评测集（JSONL，字段见 internal/judge/bench.go 的 Case）
//	judge-bench -kind openai ... -cases ./cases.jsonl
//
//	# 只看规则后端（不接模型；用于确认骨架本身正常）
//	judge-bench -kind rules
//
// 退出码：0 = 健康；1 = 疑似塌缩或存在漏报；2 = 用法/初始化错误。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"gateway/internal/judge"
	"gateway/pkg/types"
)

func main() {
	var (
		kind       = flag.String("kind", "rules", "后端类型：rules | openai | http")
		baseURL    = flag.String("base-url", "", "服务地址（openai/http 必填；必须是本机或内网）")
		model      = flag.String("model", "", "模型名（kind=openai 必填）")
		schemaMode = flag.String("schema-mode", "prompt_only", "约束解码：json_schema | gbnf | format | prompt_only")
		timeout    = flag.Duration("timeout", 5*time.Second, "单次判定超时（离线自测可以放宽，热路径上是 300ms）")
		casesPath  = flag.String("cases", "", "评测集 JSONL 路径（可选）")
		asJSON     = flag.Bool("json", false, "以 JSON 输出结果")
		skipProbe  = flag.Bool("skip-probe", false, "跳过内置探针自检")
	)
	flag.Parse()

	backend, err := buildBackend(*kind, *baseURL, *model, types.SchemaMode(*schemaMode), *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败：%v\n", err)
		os.Exit(2)
	}

	ctx := context.Background()
	exit := 0

	var probe judge.ProbeVerdict
	if !*skipProbe {
		probe = judge.Probe(ctx, backend)
		if !*asJSON {
			fmt.Println("== 内置探针自检 ==")
			fmt.Print(probe.Report())
			fmt.Println()
		}
		if !probe.Healthy {
			exit = 1
		}
	}

	var report judge.BenchReport
	if *casesPath != "" {
		cases, lerr := judge.LoadCases(*casesPath)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "读取评测集失败：%v\n", lerr)
			os.Exit(2)
		}
		report = judge.Run(ctx, backend, cases)
		if !*asJSON {
			fmt.Println("== 评测集 ==")
			fmt.Print(report.Report())
			fmt.Println()
		}
		if report.FalseNegatives > 0 {
			exit = 1
		}
	}

	if *asJSON {
		out := map[string]any{}
		if !*skipProbe {
			out["probe"] = probe
		}
		if *casesPath != "" {
			out["bench"] = report
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "输出 JSON 失败：%v\n", err)
			os.Exit(2)
		}
	} else if exit != 0 {
		fmt.Println("结论：不健康（见上方问题项）。判断层接入前请先解决这些问题。")
		fmt.Println("提示：若问题集中在少数类别，可先用 judgment.whitelist 把这些动作放行，")
		fmt.Println("      或在独立评测集上定位是模型能力问题还是骨架配置问题。")
	} else {
		fmt.Println("结论：健康。")
	}
	os.Exit(exit)
}

// buildBackend 按参数构造一个后端。
//
// 刻意只构造**单个后端**、不套降级链：自测要针对具体后端，
// 套上链会测成「链里最稳的那个」，把想测的模型藏在 rules 兜底后面。
func buildBackend(kind, baseURL, model string, mode types.SchemaMode, timeout time.Duration) (judge.Judge, error) {
	switch judge.Kind(kind) {
	case judge.KindRules:
		return judge.NewRules(judge.Whitelist{}), nil
	case judge.KindOpenAI:
		if baseURL == "" || model == "" {
			return nil, fmt.Errorf("kind=openai 需要 -base-url 与 -model")
		}
		return judge.NewOpenAICompat(judge.OpenAIOptions{
			Name: "openai", BaseURL: baseURL, Model: model,
			SchemaMode: mode, Timeout: timeout,
		}), nil
	case judge.KindHTTP:
		if baseURL == "" {
			return nil, fmt.Errorf("kind=http 需要 -base-url")
		}
		return judge.NewHTTPEndpoint(judge.HTTPOptions{
			Name: "http", URL: baseURL, Timeout: timeout,
		}), nil
	default:
		return nil, fmt.Errorf("未知的 -kind %q（want rules|openai|http）", kind)
	}
}
