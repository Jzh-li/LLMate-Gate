# LLMate Gate · VS Code 扩展

将 Continue / Cursor 等 OpenAI 兼容扩展的 `base_url` 自动指向本地 LLMate Gate 隐私网关
（`localhost:8400`），并管理网关守护进程的启动 / 停止。状态栏实时显示网关状态。

## 功能

- **一键启用**：激活后弹窗提示，确认后把 Continue 的 OpenAI 兼容模型 `apiBase` 指向
  `http://localhost:<port>/v1`，从此所有 OpenAI 兼容请求经网关脱敏 / 还原。
- **守护进程管理**：自动（或手动）拉起 `llmate-gate` 二进制，停止时清理进程。
- **状态栏**：显示「运行中 / 已停止」，运行中可一键打开调试面板
  （`http://localhost:<port>/_debug`）。
- **健康检查**：周期性轮询 `/healthz`，断线自动更新状态。

## 安装与构建

```bash
cd vscode-ext
npm install
npm run compile          # 产出 out/extension.js
# 调试：F5 在 Extension Development Host 中运行
```

打包发布（可选）：`npx @vscode/vsce package`（需先 `npm install -g @vscode/vsce`）。

## 配置项（Settings）

| 设置 | 默认 | 说明 |
|---|---|---|
| `llmateGate.gatewayBinaryPath` | `""` | 网关二进制路径；留空按 PATH 查找 `llmate-gate` |
| `llmateGate.gatewayPort` | `8400` | 网关监听端口 |
| `llmateGate.autoStart` | `true` | 启动 VS Code 时自动拉起网关 |
| `llmateGate.configPath` | `""` | 网关 YAML 配置路径；留空用内置默认 |
| `llmateGate.authToken` | `""` | 网关 `auth_token`；经环境变量传入（不出现在进程参数） |

## 命令

- `LLMate Gate: 启用（指向本地网关）` —— 启动网关并改写 Continue 配置
- `LLMate Gate: 停用` —— 停止网关，还原被改写的 `apiBase`
- `LLMate Gate: 打开调试面板` —— 浏览器打开 `/_debug`

## 与 Claude Code hooks 的关系

- **本扩展 = 透明代理主通道**：把 LLM 流量整体导向网关，请求脱敏 + 响应还原对用户透明。
- **Claude Code hooks（见 `../hooks`） = 边界兜底**：工具调用入参的 PII 闸门与出参告警，
  协议限制下无法改写工具返回值，故作为补充。

两者共用同一网关与占位符协议，映射表 / 审计一致。
