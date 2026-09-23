// Package judge 实现行为判断层（契约 §12）。
//
// 定位（BYOM 骨架）：本包提供「把判断接到编排里」的全部确定性结构——契约、
// 后端接口、降级链、确定性 Mapper、自检探针、评测工具；**不预置任何模型**。
// 模型由使用方自选，通过 `kind: openai` / `kind: http` 接入。
//
// 本包对「判断得准不准」不做承诺，只承诺四件事：
//
//	同输入必同 action（Action 由 Mapper 按阈值算，不由后端给）
//	可审计（Evidence.Reasons + Engine 进日志）
//	可解释（category / severity / confidence 三项分离）
//	fail-safe（超时、解析失败、能力塌缩一律降级或交人，绝不默认放行）
package judge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gateway/pkg/types"
)

// Kind 后端类型（契约 §12 后端矩阵）。
//
// laya / localjev 这类具体模型**不需要专门的 kind**：它们要么暴露 OpenAI 兼容
// 接口（用 openai），要么给自定义 JSON（用 http）。这正是「一个适配器吃下
// 绝大多数本地模型」的落点——新增一个模型不该新增一行 Go 代码。
type Kind string

const (
	// KindRules 进程内确定性规则后端：零依赖、可进 CI 回归、默认兜底。
	KindRules Kind = "rules"
	// KindOpenAI 任意 OpenAI 兼容服务（Ollama / llama.cpp server / vLLM /
	// LM Studio / LocalAI / Xinference ...）。
	KindOpenAI Kind = "openai"
	// KindHTTP 自定义 JSON 逃生口（自建服务）。请求/响应模板由 Spec 描述。
	KindHTTP Kind = "http"
)

// AllKinds 全部后端类型（闭集）。
func AllKinds() []Kind { return []Kind{KindRules, KindOpenAI, KindHTTP} }

// IsKnownKind 后端类型是否在闭集内。
func IsKnownKind(k Kind) bool {
	for _, x := range AllKinds() {
		if x == k {
			return true
		}
	}
	return false
}

// Spec 一个后端的构造参数。
//
// 刻意与 config.JudgmentBackendConfig 解耦：config 负责「YAML 里的键合法吗」，
// Spec 负责「构造这个后端需要什么」。投影发生在装配层（cmd/llmate-gate），
// 于是本包不依赖 config，测试可以只用 Spec 构造任意后端。
type Spec struct {
	// Name 后端名（进 Evidence.Engine 与指标标签，必须唯一且非空）。
	//
	// ⚠️ 这个名字会成为该后端**阈值表的键**：Mapper 是按 Evidence.Engine 查表的，
	// 所以构造出的后端必须原样自报此名（NewFromSpecs 会校验，不等即拒绝装配）。
	Name string
	// Kind 后端类型。
	Kind Kind
	// BaseURL 服务地址（kind=openai/http 必填）。
	BaseURL string
	// Model 模型名（kind=openai 用）。
	Model string
	// SchemaMode 期望的约束解码模式；实际生效模式由后端按 Capabilities 取最强可用者。
	SchemaMode types.SchemaMode
	// Timeout 该后端单次判定超时（0 → 用调用方 ctx 的期限）。
	Timeout time.Duration
	// MaxInputBytes 输入上限（0 → 用后端 Capabilities 声明的值）。
	MaxInputBytes int
	// Thresholds 该后端专属阈值表（nil → 用 types.DefaultThresholds()）。
	//
	// per-backend 是硬要求，不是可选项：confidence 跨后端不可比。
	Thresholds *types.JudgmentThresholds
}

// Judge 一个判断后端。
//
// 与 detector.Client 同范式：单一职责、可替换、能力自声明。
// 实现方必须保证：
//   - 只读 ActionDescriptor，不做任何网络/文件副作用（除访问自己的 base_url）；
//   - 判不了时返回 Category == unknown，**不是** benign；
//   - 自身异常返回 error，由 Chain 降级。
//
// 返回值为值类型而非指针：Evidence 很小，且 nil 指针的漏判代价高于一次拷贝。
type Judge interface {
	// Name 后端名。
	Name() string
	// Capabilities 能力声明（决定降级路径与 confidence 是否参与加权）。
	Capabilities() types.Capabilities
	// Judge 对一条描述给出证据。
	Judge(ctx context.Context, d types.ActionDescriptor) (types.Evidence, error)
	// Health 可用性探测。
	//
	// 约定：返回 nil 表示可用；返回错误表示**本后端当前不可信**，Chain 会跳过它。
	// 规则后端恒返回 nil。远程后端在实现里不应做昂贵探测——
	// 昂贵的能力自检走 Probe（probe.go），不在热路径上。
	Health(ctx context.Context) error
}

// 后端级错误。
var (
	// ErrUnavailable 后端不可用（连接失败 / Health 不过 / 超时）。
	ErrUnavailable = errors.New("judge: backend unavailable")
	// ErrInvalidOutput 后端输出无法解析成闭集内的 Evidence。
	ErrInvalidOutput = errors.New("judge: invalid backend output")
	// ErrInputTooLarge 输入超出后端声明的上限。
	ErrInputTooLarge = errors.New("judge: input too large")
)

// unavailable 包装「后端不可用」并保留原因。
func unavailable(engine string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, engine, err)
}

// invalidOutput 包装「输出不可用」并保留原因。
func invalidOutput(engine string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrInvalidOutput, engine, err)
}

// withTimeout 给单次判定套上后端自己的超时。
//
// 注意这里**不做 fail-open**：超时返回 ctx 错误，由 Chain 决定降级给谁。
// 判断层的姿态由 config.judgment.fail_closed 决定，不在后端里决定。
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
