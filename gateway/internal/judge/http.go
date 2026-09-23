package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gateway/pkg/types"
)

// HTTPEndpoint 自定义 HTTP 逃生口（契约 §12 后端矩阵 kind=http）。
//
// 存在理由：BYOM 总不能穷举所有本地服务形态。凡是「POST 一段 JSON、返回一段
// JSON」的自建服务都能接进来，不必为它写一个 Go 适配器。
//
// 协议（最简形态，故意不含认证——判断后端被限定在本机/内网，不需要凭证）：
//
//	请求  POST {base_url}
//	      Content-Type: application/json
//	      body: ActionDescriptor 的 JSON（pkg/types/judge.go §12.2）
//	响应  200 + {"category":"...","severity":0.9,"confidence":0.8,"reasons":[...]}
//
// 响应同样过闭集校验：越界的 category 与超范围的数值都算无效输出（→ 降级）。
// 逃生口放宽的是**协议**，不是**契约**。
type HTTPEndpoint struct {
	name          string
	url           string
	timeout       time.Duration
	maxInputBytes int
	client        *http.Client
}

// HTTPOptions 构造参数。
type HTTPOptions struct {
	Name          string
	URL           string
	Timeout       time.Duration
	MaxInputBytes int
	Client        *http.Client
}

// NewHTTPEndpoint 构造自定义 HTTP 后端。
func NewHTTPEndpoint(o HTTPOptions) *HTTPEndpoint {
	maxIn := o.MaxInputBytes
	if maxIn <= 0 {
		maxIn = defaultMaxInputBytes
	}
	client := o.Client
	if client == nil {
		client = newLocalHTTPClient(o.Timeout)
	}
	return &HTTPEndpoint{
		name: o.Name, url: strings.TrimSpace(o.URL),
		timeout: o.Timeout, maxInputBytes: maxIn, client: client,
	}
}

func (h *HTTPEndpoint) Name() string { return h.name }

// Capabilities 自定义服务的能力无法推断，按最保守声明。
//
// SchemaModes 留空 = 仅 prompt_only：不假定对方做了约束解码，于是解析失败
// 会走 fail-safe。GivesConfidence 声明为 true —— 若对方返回 confidence，
// 置信地板会保护「没把握的结论不落档」；若对方不返回，Confidence 为 0
// 会触发地板 → review。这是保守一侧，宁可多交人。
func (h *HTTPEndpoint) Capabilities() types.Capabilities {
	return types.Capabilities{
		GivesConfidence: true,
		MaxInputBytes:   h.maxInputBytes,
		Deterministic:   false,
	}
}

// Health 只做 TCP 连通性检查（与 openai 后端同理由：不在探测里跑推理）。
func (h *HTTPEndpoint) Health(ctx context.Context) error {
	addr, err := parseEndpoint(h.url)
	if err != nil {
		return unavailable(h.name, err)
	}
	ctx, cancel := withTimeout(ctx, h.timeout)
	defer cancel()
	conn, derr := dialTCP(ctx, addr)
	if derr != nil {
		return unavailable(h.name, derr)
	}
	_ = conn.Close()
	return nil
}

// Judge 把描述原样 POST 给用户的服务。
func (h *HTTPEndpoint) Judge(ctx context.Context, d types.ActionDescriptor) (types.Evidence, error) {
	if err := d.Validate(); err != nil {
		return types.Evidence{}, err
	}
	if n := d.InputBytes(); n > h.maxInputBytes {
		return types.Evidence{}, fmt.Errorf("%w: %s: %d > %d", ErrInputTooLarge, h.name, n, h.maxInputBytes)
	}

	start := time.Now()
	ctx, cancel := withTimeout(ctx, h.timeout)
	defer cancel()

	body, err := json.Marshal(d)
	if err != nil {
		return types.Evidence{}, invalidOutput(h.name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return types.Evidence{}, unavailable(h.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return types.Evidence{}, unavailable(h.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return types.Evidence{}, unavailable(h.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return types.Evidence{}, unavailable(h.name,
			fmt.Errorf("http %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 200)))
	}

	obj, ok := extractJSONObject(string(raw))
	if !ok {
		return types.Evidence{}, invalidOutput(h.name,
			fmt.Errorf("response is not a json object: %s", truncate(strings.TrimSpace(string(raw)), 200)))
	}
	var out modelOutput
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return types.Evidence{}, invalidOutput(h.name, fmt.Errorf("decode response: %v", err))
	}
	ev, err := out.toEvidence(h.name)
	if err != nil {
		return types.Evidence{}, invalidOutput(h.name, err)
	}
	ev.LatencyMs = time.Since(start).Milliseconds()
	return ev, nil
}
