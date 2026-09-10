// Command mcp-server 是 LLMate Gate 的薄 MCP 门面（执行手册 Phase 3 任务 3.1，v1.1）。
//
// 它以 stdio 传输承载 MCP 协议（Claude Desktop / 任意 MCP 客户端可直接 spawn），
// 暴露三个工具：
//
//	anonymize        —— 递归脱敏任意 JSON / 文本中的中文 PII，返回占位符 + request_id
//	deanonymize      —— 按 request_id 把占位符还原为原文（同一映射表可多次还原）
//	scan_tool_params —— 预检式 PII 扫描：报告是否含 PII + 脱敏样例 + request_id
//
// 工具实现全部委托给 internal/mcp 客户端，后者经 HTTP 调网关常驻隐私端点
// /v1/privacy/redact|restore。因此本门面与 OpenAI 反代层共用同一 vault 与占位符协议
// （验收项「与代理层共享映射表」）。
//
// 配置（环境变量）：
//
//	LLMATE_GATEWAY_URL   网关地址，默认 http://127.0.0.1:8400
//	LLMATE_GATEWAY_TOKEN 网关 Bearer 令牌（回退到 GATEWAY_AUTH_TOKEN）
//
// 日志写到 stderr（stdio 传输占用 stdout，绝不能污染）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	gwmcp "gateway/internal/mcp"
)

const (
	serverName    = "llmate-gate"
	serverVersion = "1.1.0"
)

func main() {
	cli := gwmcp.NewClientFromEnv()
	if !cli.TokenSet() {
		fmt.Fprintln(os.Stderr, "[mcp] WARN: 未设置 LLMATE_GATEWAY_TOKEN / GATEWAY_AUTH_TOKEN，调用网关将收到 401；请在环境中配置网关令牌")
	}
	fmt.Fprintf(os.Stderr, "[mcp] LLMate Gate MCP 门面启动：gateway=%s\n", cli.GatewayURL())

	s := server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithToolCapabilities(false),
		server.WithInstructions("LLMate Gate 隐私门面：对 tool 参数 / 文本做中文 PII 脱敏与还原，复用网关同一映射表。"),
	)
	registerTools(s, cli)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "[mcp] fatal: %v\n", err)
		os.Exit(1)
	}
}

// registerTools 注册三个隐私工具。
func registerTools(s *server.MCPServer, cli *gwmcp.Client) {
	anonymize := mcpsdk.NewTool("anonymize",
		mcpsdk.WithDescription("递归脱敏任意 JSON / 文本中的中文 PII（手机号、邮箱、身份证、银行卡、地址等）：键保留、仅字符串值脱敏。"+
			"返回脱敏后的内容 + request_id，后续可用 deanonymize 还原。"),
		mcpsdk.WithObject("json",
			mcpsdk.Description("任意嵌套 JSON 对象/数组，将被递归脱敏"),
		),
		mcpsdk.WithString("text",
			mcpsdk.Description("纯文本，将被整体脱敏（与 json 二选一）"),
		),
		mcpsdk.WithString("conversation_id",
			mcpsdk.Description("可选：会话 ID，用于增量缓存命中"),
		),
		mcpsdk.WithString("strategy",
			mcpsdk.Description("可选：placeholder（默认，可逆占位符）| simulate（中文格式保持仿真）"),
		),
	)
	s.AddTool(anonymize, func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		j, t, err := extractIO(req.GetArguments())
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		resp, err := cli.Anonymize(ctx, gwmcp.AnonymizeReq{
			JSON:           j,
			Text:           t,
			ConversationID: req.GetString("conversation_id", ""),
			Strategy:       req.GetString("strategy", ""),
		})
		if err != nil {
			return mcpsdk.NewToolResultError(fmt.Sprintf("anonymize failed: %v", err)), nil
		}
		return resultText(resp)
	})

	deanonymize := mcpsdk.NewTool("deanonymize",
		mcpsdk.WithDescription("按 request_id 把 anonymize 产生的占位符还原为原文（同一个映射表可多次还原）。"),
		mcpsdk.WithObject("json",
			mcpsdk.Description("含占位符的 JSON，将被还原"),
		),
		mcpsdk.WithString("text",
			mcpsdk.Description("含占位符的纯文本，将被还原（与 json 二选一）"),
		),
		mcpsdk.WithString("request_id",
			mcpsdk.Required(),
			mcpsdk.Description("anonymize 返回的 request_id"),
		),
	)
	s.AddTool(deanonymize, func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		j, t, err := extractIO(req.GetArguments())
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		rid := req.GetString("request_id", "")
		if rid == "" {
			return mcpsdk.NewToolResultError("request_id is required"), nil
		}
		resp, err := cli.Deanonymize(ctx, gwmcp.DeanonymizeReq{JSON: j, Text: t, RequestID: rid})
		if err != nil {
			return mcpsdk.NewToolResultError(fmt.Sprintf("deanonymize failed: %v", err)), nil
		}
		return resultText(resp)
	})

	scan := mcpsdk.NewTool("scan_tool_params",
		mcpsdk.WithDescription("预检式 PII 扫描：对 tool 参数做脱敏并报告是否含 PII、给出脱敏样例与 request_id。"+
			"适合在真正调用高权限工具前先做隐私闸门判断（不阻塞流程）。"),
		mcpsdk.WithObject("json",
			mcpsdk.Description("待扫描的 tool 参数 JSON"),
		),
		mcpsdk.WithString("text",
			mcpsdk.Description("待扫描的纯文本"),
		),
		mcpsdk.WithString("conversation_id",
			mcpsdk.Description("可选：会话 ID"),
		),
		mcpsdk.WithString("strategy",
			mcpsdk.Description("可选：placeholder | simulate"),
		),
	)
	s.AddTool(scan, func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		j, t, err := extractIO(req.GetArguments())
		if err != nil {
			return mcpsdk.NewToolResultError(err.Error()), nil
		}
		resp, err := cli.ScanToolParams(ctx, gwmcp.ScanReq{
			JSON:           j,
			Text:           t,
			ConversationID: req.GetString("conversation_id", ""),
			Strategy:       req.GetString("strategy", ""),
		})
		if err != nil {
			return mcpsdk.NewToolResultError(fmt.Sprintf("scan failed: %v", err)), nil
		}
		return resultText(resp)
	})
}

// extractIO 从工具参数中取出 json / text 二选一，json 原样序列化为 RawMessage。
func extractIO(args map[string]any) (json.RawMessage, string, error) {
	if v, ok := args["json"]; ok && v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", fmt.Errorf("marshal json arg: %w", err)
		}
		return b, "", nil
	}
	if v, ok := args["text"]; ok {
		if s, ok := v.(string); ok {
			return nil, s, nil
		}
		return nil, "", fmt.Errorf("text must be a string")
	}
	return nil, "", fmt.Errorf("json or text is required")
}

// resultText 把结构体缩进序列化为工具结果文本（JSON）。
// 必须禁用 HTML 转义：响应里含占位符 <<type_index>>，默认 json.Marshal 会把 < 转成 \u003c。
func resultText(v interface{}) (*mcpsdk.CallToolResult, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcpsdk.NewToolResultText(buf.String()), nil
}
