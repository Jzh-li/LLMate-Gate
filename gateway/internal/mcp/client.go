// Package mcp 是 LLMate Gate 的薄 MCP 门面客户端。
//
// 它不实现 MCP 协议本身（协议由 cmd/mcp-server 用 MCP SDK 承载），只负责把
// anonymize / deanonymize / scan_tool_params 三个语义调用转发到网关的常驻隐私端点：
//
//	POST /v1/privacy/redact   —— 递归脱敏任意 JSON / 文本，返回占位符 + request_id
//	POST /v1/privacy/restore  —— 按 request_id 把占位符还原为原文
//
// 直接复用网关进程内的同一 vault 与占位符协议，因此与 OpenAI 反代层「共享映射表」
// （执行手册 Phase 3 验收项）。本包为零外部依赖的纯 net/http 实现，可用 httptest 替身单测。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// DefaultGatewayURL 网关默认监听地址（与整体技术方案一致）。
	DefaultGatewayURL = "http://127.0.0.1:8400"

	envGatewayURL   = "LLMATE_GATEWAY_URL"
	envGatewayToken = "LLMATE_GATEWAY_TOKEN"
)

// AnonymizeReq anonymize 工具入参（与网关 /v1/privacy/redact 对齐）。
//
// 两种形态任选其一：json（任意嵌套 JSON）或 text（纯文本）。
type AnonymizeReq struct {
	JSON           json.RawMessage `json:"json,omitempty"`
	Text           string          `json:"text,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Strategy       string          `json:"strategy,omitempty"` // placeholder（默认）| simulate
}

// AnonymizeResp anonymize 工具出参。
type AnonymizeResp struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	RequestID string          `json:"request_id"`
	Changed   bool            `json:"changed"`
	Strategy  string          `json:"strategy"`
}

// DeanonymizeReq deanonymize 工具入参（与网关 /v1/privacy/restore 对齐）。
type DeanonymizeReq struct {
	JSON      json.RawMessage `json:"json,omitempty"`
	Text      string          `json:"text,omitempty"`
	RequestID string          `json:"request_id"`
}

// DeanonymizeResp deanonymize 工具出参。
type DeanonymizeResp struct {
	JSON json.RawMessage `json:"json,omitempty"`
	Text string          `json:"text,omitempty"`
}

// ScanReq scan_tool_params 工具入参（与 anonymize 同形，但语义是「预检」）。
type ScanReq struct {
	JSON           json.RawMessage `json:"json,omitempty"`
	Text           string          `json:"text,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Strategy       string          `json:"strategy,omitempty"`
}

// ScanResp scan_tool_params 工具出参：在不改变调用方流程的前提下，报告是否存在 PII，
// 并提供脱敏样例 + request_id（如需还原可复用）。
type ScanResp struct {
	HasPII         bool            `json:"has_pii"`
	RequestID      string          `json:"request_id,omitempty"`
	RedactedJSON   json.RawMessage `json:"redacted_json,omitempty"`
	RedactedText   string          `json:"redacted_text,omitempty"`
	Strategy       string          `json:"strategy,omitempty"`
	Recommendation string          `json:"recommendation,omitempty"` // block | proceed | redact
}

// Client 网关隐私端点客户端。
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient 显式构造。baseURL 不带结尾斜杠；token 为空时仍发起请求（由网关返回 401，
// 错误信息会透传给调用方）。
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultGatewayURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientFromEnv 从环境变量构造：LLMATE_GATEWAY_URL（默认 http://127.0.0.1:8400）、
// LLMATE_GATEWAY_TOKEN（回退到 GATEWAY_AUTH_TOKEN）。
func NewClientFromEnv() *Client {
	url := os.Getenv(envGatewayURL)
	if url == "" {
		url = DefaultGatewayURL
	}
	tok := os.Getenv(envGatewayToken)
	if tok == "" {
		tok = os.Getenv("GATEWAY_AUTH_TOKEN")
	}
	return NewClient(url, tok)
}

// GatewayURL 返回当前生效的网关地址（供启动期日志使用）。
func (c *Client) GatewayURL() string { return c.baseURL }

// TokenSet 返回是否已配置鉴权令牌（启动期告警用）。
func (c *Client) TokenSet() bool { return c.token != "" }

// Anonymize 递归脱敏，返回占位符与 request_id。
func (c *Client) Anonymize(ctx context.Context, req AnonymizeReq) (*AnonymizeResp, error) {
	resp := new(AnonymizeResp)
	if err := c.post(ctx, "/v1/privacy/redact", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Deanonymize 按 request_id 还原占位符为原文。
func (c *Client) Deanonymize(ctx context.Context, req DeanonymizeReq) (*DeanonymizeResp, error) {
	resp := new(DeanonymizeResp)
	if err := c.post(ctx, "/v1/privacy/restore", req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ScanToolParams 预检式脱敏：复用 redact 端点，把结果包装成「是否含 PII + 脱敏样例」报告。
// 与 anonymize 的区别仅在于返回形态（结构化扫描报告），底层同一映射表可后续还原。
func (c *Client) ScanToolParams(ctx context.Context, req ScanReq) (*ScanResp, error) {
	red, err := c.Anonymize(ctx, AnonymizeReq(req))
	if err != nil {
		return nil, err
	}
	out := &ScanResp{
		HasPII:         red.Changed,
		RequestID:      red.RequestID,
		RedactedJSON:   red.JSON,
		RedactedText:   red.Text,
		Strategy:       red.Strategy,
		Recommendation: "proceed",
	}
	if red.Changed {
		// 检出 PII：建议先以脱敏形态执行，必要时再按 request_id 还原。
		out.Recommendation = "redact"
	}
	return out, nil
}

// marshalNoEscape 与网关 writePrivacyJSON 保持一致：禁用 HTML 转义，保证占位符 <<type_index>>
// 以字面量发出（默认 json.Marshal 会把 < 转成 \u003c，破坏占位符且让网关端字面串匹配失效）。
func marshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// post 通用 JSON 请求封装：Bearer 鉴权 + 限流读取 + 非 2xx 返回带响应体的错误。
func (c *Client) post(ctx context.Context, path string, body, out interface{}) error {
	buf, err := marshalNoEscape(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call gateway %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway %s: status %d: %s", path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode gateway response: %w (body=%s)", err, string(raw))
	}
	return nil
}
