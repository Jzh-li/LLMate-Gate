# LLMate Gate · Claude Code hooks

把 Claude Code 的工具调用边界接上 LLMate Gate 的脱敏能力：工具参数里的 PII 在
执行前被拦下 / 脱敏，工具返回里的 PII 被告警。

> 前置：网关必须已在运行（默认 `http://127.0.0.1:8400`，auth 与 `LMGATE_AUTH_TOKEN` 一致）。
> 启动网关属于 [VS Code 扩展](../../vscode-ext) 的职责，也可手动：
> `llmate-gate --listen :8400 --config configs/ui-smoke.yaml`（并设置 `GATEWAY_AUTH_TOKEN`）。

## 安装

1. 把下面两段写进 `~/.claude/settings.json`（用户级）或项目 `.claude/settings.json`：
   ```json
   {
     "PreToolUse": [
       { "matcher": "Bash|Write|Edit|MultiEdit|NotebookEdit|WebFetch|WebSearch",
         "hooks": [ { "type": "command",
           "command": "bash /absolute/path/to/LLMate-Gate/hooks/pre-tool.sh" } ] }
     ],
     "PostToolUse": [
       { "matcher": "Bash|Write|Edit|MultiEdit|NotebookEdit|WebFetch|WebSearch|Read",
         "hooks": [ { "type": "command",
           "command": "bash /absolute/path/to/LLMate-Gate/hooks/post-tool.sh" } ] }
     ]
   }
   ```
   （完整示例见 [`settings.json.example`](./settings.json.example)。）

2. 配置环境变量（建议放进 shell profile 或 Claude Code 的 env）：
   ```bash
   export LMGATE_GATEWAY="http://127.0.0.1:8400"   # 网关地址
   export LMGATE_AUTH_TOKEN="<与网关 auth_token 一致>" # 鉴权
   ```

3. 依赖：`python3`（stdlib 即可，**零外部依赖**）。

## 行为

### PreToolUse（`pre-tool.sh`）

递归扫描 `tool_input`：

| 场景 | 默认 `block` 模式 | `LMGATE_HOOK_MODE=redact` |
|---|---|---|
| 检出 PII | **deny**，reason 附脱敏预览，用户可在 Claude Code 手动放行 | **allow** + `updatedInput` 回写脱敏后的参数（占位符运行） |
| 无 PII | allow | allow |
| 网关不可达 | 默认 fail-open（allow + 告警）；设 `LMGATE_HOOK_FAIL_CLOSED=1` 则 deny | 同左 |

写/改文件类工具（Write/Edit/MultiEdit/NotebookEdit）的 `file_path` / `old_string` /
`new_string` 在 `redact` 模式下整体保留，不参与脱敏，避免破坏工具执行。

### PostToolUse（`post-tool.sh`）

扫描 `tool_output`：检出 PII 时通过 `systemMessage` 注入告警（工具已执行，只能提示，
**无法改写返回值**）。无 PII 则干净退出、不污染 transcript。

## 协议约束（为什么这么做）

Claude Code hook 只能：PreToolUse `allow/deny/ask` + **整体替换** `updatedInput`；
PostToolUse 只能观察（`systemMessage` / `exit 2` 阻断），**不能改写 tool_result**。
因此「递归脱敏工具参数」在 `redact` 模式下以 `updatedInput` 实现，「还原返回值」则受协议
所限只能告警。真正的「入参脱敏 + 出参还原」透明替换由 OpenAI 兼容代理路径
（`base_url -> localhost:8400`，见 VS Code 扩展）承担——那是主通道，hook 是边界兜底。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `LMGATE_GATEWAY` | `http://127.0.0.1:8400` | 网关地址 |
| `LMGATE_AUTH_TOKEN` | （空） | 与网关 `auth_token` 一致 |
| `LMGATE_HOOK_MODE` | `block` | `block` 拦截 / `redact` 自动脱敏后执行 |
| `LMGATE_HOOK_FAIL_CLOSED` | `0` | `1` 时网关不可达则 deny（fail-closed） |
| `LMGATE_HOOK_DEBUG` | （空） | 任意值开启 stderr 调试日志 |

## 端到端自测

不依赖真实 Claude Code，直喂 hook 事件即可验证：

```bash
export LMGATE_GATEWAY="http://127.0.0.1:8400" LMGATE_AUTH_TOKEN="<token>"
echo '{"tool_name":"Bash","tool_input":{"command":"echo 13800138000"}}' \
  | python3 hooks/lmgate_hook.py pre      # 默认 block -> deny + 脱敏预览
echo '{"tool_name":"Bash","tool_input":{"command":"echo 13800138000"}}' \
  | LMGATE_HOOK_MODE=redact python3 hooks/lmgate_hook.py pre   # allow + updatedInput
echo '{"tool_name":"Bash","tool_output":"contact a@b.com"}' \
  | python3 hooks/lmgate_hook.py post     # systemMessage 告警
```
