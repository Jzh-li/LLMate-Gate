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
			b = NewNamedRules(sp.Name, opts.Whitelist)
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
		// 名字必须与 Spec 一致，这里不等就直接拒绝装配。
		//
		// 阈值表是按**后端盖在证据上的名字**（Evidence.Engine）登记的，不是按配置里
		// 的名字。两者一旦不等，Mapper 查不到专属条目、静默退回内置缺省——用户改的
		// 阈值毫无效果，而且没有任何报错。这类「配置空转」必须在装配期炸掉：它是静默的，
		// 靠使用者自己发现不了。
		if got := b.Name(); got != sp.Name {
			return nil, fmt.Errorf("judge: backends[%d] is named %q but its spec says %q; "+
				"a backend's own name keys its threshold table, so the two must match", i, got, sp.Name)
		}
		backends = append(backends, b)
	}

	e, err := NewEvaluator(backends, opts)
	if err != nil {
		return nil, err
	}
	// per-backend 阈值 + confidence 语义：两者都随后端走，不能共用一套。
	// 键用 backends[i].Name()（而非 sp.Name）——它与上面的一致性检查配合，
	// 把「键 = 后端自报名」这条不变量摆在调用点上。
	for i, sp := range specs {
		th := types.DefaultThresholds()
		if sp.Thresholds != nil {
			th = *sp.Thresholds
		}
		e.SetMapper(backends[i].Name(), th, backends[i].Capabilities().GivesConfidence)
	}
	return e, nil
}
