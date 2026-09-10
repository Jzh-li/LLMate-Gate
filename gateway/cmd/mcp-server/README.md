# LLMate Gate MCP 门面（Phase 3 · 任务 3.1，v1.1）

薄 MCP Server：以 **stdio** 传输承载 MCP 协议（Claude Desktop / 任意 MCP 客户端可直接 spawn），
把三个隐私工具暴露给 Agent。工具实现全部委托给 `gateway/internal/mcp` 客户端，后者经 HTTP 调网关
常驻隐私端点 `/v1/privacy/redact|restore`——因此本门面与 OpenAI 反代层**共用同一映射表与占位符协议**
（执行手册 Phase 3 验收项「与代理层共享映射表」）。

## 工具

| 工具 | 入参 | 出参 | 说明 |
|---|---|---|---|
| `anonymize` | `json`(对象/数组) 或 `text`，可选 `conversation_id`、`strategy` | 脱敏后内容 + `request_id` + `changed` + `strategy` | 递归脱敏任意嵌套 JSON（键保留、仅字符串值脱敏）或纯文本 |
| `deanonymize` | `json`/`text` + 必填 `request_id` | 还原后的原文 | 按 `request_id` 把占位符还原（同一映射表可多次还原） |
| `scan_tool_params` | 同 `anonymize` | `{ has_pii, request_id, redacted_*, recommendation }` | 预检式 PII 扫描：报告是否含 PII + 脱敏样例 + `request_id`，适合高权限工具调用前的隐私闸门 |

## 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `LLMATE_GATEWAY_URL` | `http://127.0.0.1:8400` | 网关地址 |
| `LLMATE_GATEWAY_TOKEN` | 回退到 `GATEWAY_AUTH_TOKEN` | 网关 Bearer 令牌（**必填**，否则调用网关收到 401） |

> 网关必须先在 `:8400` 运行且配置好 `GATEWAY_AUTH_TOKEN`（见 `gateway/cmd/llmate-gate`）。
> 占位符协议统一为字面 `<<type_index>>`（如 `<<zh_phone_1>>`、`<<email_1>>`），开发期可直接肉眼核对。

## 构建

```bash
# 经 scripts/dev.sh 同步到本地 NTFS 构建目录后编译（WSL 9P 不支持 go.mod 文件锁）
bash scripts/dev.sh sync
cd "$LMGATE_BUILD/gateway" && go build -o ./mcp-server.exe ./cmd/mcp-server
# 或一次性构建全部二进制：bash scripts/dev.sh buildall
```

## Claude Desktop 接入

把 `claude_desktop_config.json.example` 的内容并入你的
`%APPDATA%\Claude\claude_desktop_config.json`（Windows）/ `~/Library/Application Support/Claude/claude_desktop_config.json`（macOS），
把 `command` 指向编译出的 `mcp-server` 可执行文件，并填好 `LLMATE_GATEWAY_TOKEN`：

```json
{
  "mcpServers": {
    "llmate-gate": {
      "command": "C:/path/to/mcp-server.exe",
      "env": {
        "LLMATE_GATEWAY_URL": "http://127.0.0.1:8400",
        "LLMATE_GATEWAY_TOKEN": "<你的网关令牌>"
      }
    }
  }
}
```

重启 Claude Desktop 后，`anonymize` / `deanonymize` / `scan_tool_params` 即可在对话中调用。

## 设计取舍

- **为什么走 HTTP 调网关而非内嵌核心**：内嵌会让 MCP 进程持有独立 vault，与反代层映射表分离；
  经 HTTP 调常驻端点则天然复用同一进程内的 vault，满足「共享映射表」验收，且零核心代码复制。
- **为什么用 mark3labs/mcp-go（v0.37.0）而非自实现协议**：官方 SDK 一行引入，专注业务；
  该版本兼容 Go 1.24（v1.0.0 要求 Go ≥ 1.25.5，会顶高整个模块版本，故 pin 旧版）。
- **占位符转义**：所有输出（工具结果文本、客户端请求体）均 `SetEscapeHTML(false)`，保证 `<<...>>` 字面传输。
