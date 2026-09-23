package judge

import (
	"fmt"

	"gateway/pkg/types"
)

// NewFromSpecs 按 Spec 列表装配判断层：降级链 + per-backend 阈值。
//
// 这是判断层的**装配点**：配置校验（internal/config）负责「YAML 里的键合法吗」，
// 本函数负责「按这些键构造出什么对象」。两者之间隔着 Spec 这层投影，于是
// 本包不依赖 config，配置测试与后端测试可以各自独立。
//
// 顺序即降级顺序：调用方应按「语义能力降序」排列（用户模型在前、rules 在最后）。
func NewFromSpecs(specs []Spec, opts EvaluatorOptions) (*Evaluator, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("judge: no backends configured")
	}
	backends := make([]Judge, 0, len(specs))
	for i, sp := range specs {
		if sp.Name == "" {
			return nil, fmt.Errorf("judge: backends[%d].name is required", i)
		}
		var b Judge
		switch sp.Kind {
		case KindRules:
			b = NewRules(opts.Whitelist)
		case KindOpenAI:
			b = NewOpenAICompat(OpenAIOptions{
				Name: sp.Name, BaseURL: sp.BaseURL, Model: sp.Model,
				SchemaMode: sp.SchemaMode, Timeout: sp.Timeout,
				MaxInputBytes: sp.MaxInputBytes,
			})
		case KindHTTP:
			b = NewHTTPEndpoint(HTTPOptions{
				Name: sp.Name, URL: sp.BaseURL, Timeout: sp.Timeout,
				MaxInputBytes: sp.MaxInputBytes,
			})
		default:
			return nil, fmt.Errorf("judge: unknown backend kind %q (want rules|openai|http)", sp.Kind)
		}
		backends = append(backends, b)
	}

	e, err := NewEvaluator(backends, opts)
	if err != nil {
		return nil, err
	}
	// per-backend 阈值 + confidence 语义：两者都随后端走，不能共用一套。
	for i, sp := range specs {
		th := types.DefaultThresholds()
		if sp.Thresholds != nil {
			th = *sp.Thresholds
		}
		e.SetMapper(sp.Name, th, backends[i].Capabilities().GivesConfidence)
	}
	return e, nil
}
