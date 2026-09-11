package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"
)

// DetectTimeout 单次检测超时（契约 §0.4）。
const DetectTimeout = 500 * time.Millisecond

// PIIEngineerClient 通过本机 HTTP 调用 PII Engineer sidecar（契约 §2.1）。
type PIIEngineerClient struct {
	endpoint string
	healthz  string
	client   *http.Client
	opts     *options
}

// NewPIIEngineerClient 构造 sidecar 客户端。
func NewPIIEngineerClient(endpoint, healthz string, timeout time.Duration, opts ...Option) *PIIEngineerClient {
	if timeout <= 0 {
		timeout = DetectTimeout
	}
	return &PIIEngineerClient{
		endpoint: endpoint,
		healthz:  healthz,
		client:   &http.Client{Timeout: timeout + 100*time.Millisecond},
		opts:     newOptions(opts...),
	}
}

// Name 引擎名称（契约 §2.2）。
func (c *PIIEngineerClient) Name() string { return "pii-engineer" }

// Detect 调用 POST /api/detect（契约 §2.1）。
func (c *PIIEngineerClient) Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, DetectTimeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "marshal detect request", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/detect", bytes.NewReader(body))
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "build detect request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "read detect response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, gatewayerrors.Errorf(gatewayerrors.CodeDetectorUnavailable, "detector status %d", resp.StatusCode)
	}

	var out types.DetectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "decode detect response", err)
	}
	out.Entities = filterAndSort(out.Entities, c.opts)
	return &out, nil
}

// DetectBatch 调用 POST /api/detect/batch（契约 §2.1）。
func (c *PIIEngineerClient) DetectBatch(ctx context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, DetectTimeout*2)
	defer cancel()

	payload := struct {
		Items []*types.DetectRequest `json:"items"`
	}{Items: reqs}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "marshal batch request", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/detect/batch", bytes.NewReader(body))
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeInvalidRequest, "build batch request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, mapTransportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "read batch response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, gatewayerrors.Errorf(gatewayerrors.CodeDetectorUnavailable, "detector batch status %d", resp.StatusCode)
	}
	var out []*types.DetectResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "decode batch response", err)
	}
	for i := range out {
		if out[i] == nil {
			out[i] = &types.DetectResponse{}
			continue
		}
		out[i].Entities = filterAndSort(out[i].Entities, c.opts)
	}
	return out, nil
}

// Health 调用 GET /healthz（契约 §2.1）。
func (c *PIIEngineerClient) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+c.healthz, nil)
	if err != nil {
		return gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "build health request", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "detector health check failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return gatewayerrors.Errorf(gatewayerrors.CodeDetectorUnavailable, "detector health status %d", resp.StatusCode)
	}
	return nil
}

// mapTransportError 把传输层错误映射为统一错误码：超时 → detector_timeout，其余 → detector_unavailable。
func mapTransportError(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return gatewayerrors.Wrap(gatewayerrors.CodeDetectorTimeout,
			fmt.Sprintf("PII detection exceeded %s", DetectTimeout), err)
	}
	return gatewayerrors.Wrap(gatewayerrors.CodeDetectorUnavailable, "detector unreachable", err)
}
