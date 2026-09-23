package judge

import (
	"context"
	"errors"
	"time"

	"gateway/pkg/types"
)

// stubJudge 进程内 mock 后端（S3）。
//
// 它的存在意义是**让骨架的边界行为可以在没有任何模型的情况下被验证**：
// schema 外输出、解析失败、超时、Health 不过、unknown 传播、降级链——
// 这些是骨架自身的性质，不该等到接上真模型才能测。等 S4 接上模型后如果
// 表现异常，有这层测试就能立刻区分「是骨架错」还是「是模型差」。
type stubJudge struct {
	name      string
	cap       types.Capabilities
	ev        types.Evidence
	err       error
	healthErr error
	delay     time.Duration
	calls     int
	lastDesc  types.ActionDescriptor
}

func (s *stubJudge) Name() string                     { return s.name }
func (s *stubJudge) Capabilities() types.Capabilities { return s.cap }
func (s *stubJudge) Health(context.Context) error     { return s.healthErr }

func (s *stubJudge) Judge(ctx context.Context, d types.ActionDescriptor) (types.Evidence, error) {
	s.calls++
	s.lastDesc = d
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return types.Evidence{}, ctx.Err()
		}
	}
	if s.err != nil {
		return types.Evidence{}, s.err
	}
	ev := s.ev
	if ev.Engine == "" {
		ev.Engine = s.name
	}
	return ev, nil
}

// 常用构造。
func benignStub(name string) *stubJudge {
	return &stubJudge{name: name, ev: types.Evidence{Category: types.CatBenign}}
}

func catStub(name string, cat types.ActionCategory, sev, conf float64) *stubJudge {
	return &stubJudge{name: name, ev: types.Evidence{Category: cat, Severity: sev, Confidence: conf}}
}

func unknownStub(name string) *stubJudge {
	return &stubJudge{name: name, ev: types.Evidence{Category: types.CatUnknown}}
}

func failingStub(name string, err error) *stubJudge {
	return &stubJudge{name: name, err: err, healthErr: err}
}

var errBoom = errors.New("boom")

// sampleDesc 一条典型的「打包整仓」描述。
func sampleDesc() types.ActionDescriptor {
	return types.ActionDescriptor{
		Kind: types.KindToolCall, Phase: types.PhaseExecuted,
		Tool: "Bash", Command: "tar -czf repo.tar.gz .", Target: "tar",
	}
}
