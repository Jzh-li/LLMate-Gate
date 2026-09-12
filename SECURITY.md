# Security Policy

LLMate Gate 处理 LLM 请求/响应中的个人可识别信息（PII）。
我们把"凭据/对话内容泄漏"视为 P0 事件——响应窗口以**天**计，不是周。

---

## Supported Versions

仅以下版本接收安全修复：

| Version | Supported |
| ------- | --------- |
| `v0.1.x` | ✅ Active |
| `< v0.1.0` | ❌ End-of-life |

Tag 命名遵循 SemVer。补丁版本号递增即视为安全修复。

---

## Reporting a Vulnerability

### 请**不要**在 GitHub Issue / Discussion 公开提交漏洞

任何带 PoC 的攻击路径一旦公开，对所有用户立刻放大风险面。

### 私密报告渠道（任选其一）

| 渠道 | 地址 |
|---|---|
| Email（推荐） | `jzh-li@outlook.com` |
| GitHub Private Vulnerability Reporting | https://github.com/Jzh-li/LLMate-Gate/security/advisories/new |
| OpenSSF Scorecard 投递 | 走 GitHub Security Advisories 流程 |

### 报告应包含

1. **触发条件**：哪个 endpoint / 请求体字段 / 配置组合
2. **影响范围**：可被读取的数据 / 可被替换的字段 / 拒绝服务的链路
3. **复现步骤**：最小 PoC（curl / 代码片段）
4. **已尝试缓解**：你这边是否已有 workaround
5. **发现者署名偏好**：公开致谢 / 匿名

---

## Response SLA

| 阶段 | 目标时间 |
|---|---|
| 确认接收 | 48 小时内 |
| 影响评估 + 修复方案 | 7 天内 |
| 补丁发布 | 14 天内（critical）/ 30 天内（high）/ 90 天内（medium 以下） |

- **Critical**（数据明文外泄 / 远程代码执行）：48 小时内出修复版本
- **High**（绕过脱敏逻辑）：7 天
- **Medium/Low**：纳入下个常规 release

修复版本发布前会通知报告者确认披露时机。

---

## Scope

### In scope

- LLMate Gate 主进程 (`cmd/llmate-gate`) 的 PII 检测 / 替换 / 还原链路
- Vault 加密 (`internal/vault` — AES-256-GCM + scrypt)
- Audit 日志完整性
- `mcp-server` stdio 接口
- Debug 面板（`/_debug`）的访问控制

### Out of scope

- 上游 LLM 服务本身（OpenAI / Anthropic / 等）的安全
- 用户自己部署时配置的 TLS / 网络暴露面
- 第三方包的安全问题（请直接上报上游）
- 已被 `govulncheck` 公开公告且未达 Go 当前 patch 版本的 stdlib 漏洞

---

## Disclosure Policy

我们遵循 **Coordinated Disclosure**：

1. 收到报告 → 内部确认 + 影响评估
2. 修复开发 → 报告者验证（可选）
3. 修复版本发布 + 同步 CVE / GHSA（如必要）
4. 公开致谢 + 详细分析（报告者署名偏好公开时）

发现者不会因善意研究被追责。我们保留对**恶意利用**追究的权利。

---

## Acknowledgments

本项目安全扫描由以下工具持续执行（CI 全绿才合并）：

- `govulncheck` — Go 官方漏洞数据库
- `golangci-lint` (govet / staticcheck) — 静态分析
- 单元测试 + race detector — 并发安全
- 端到端冒烟 (`e2e/e2e.sh`) — 行为契约

---

## Contact

- Security email: `jzh-li@outlook.com`
- Project issues（非漏洞）: https://github.com/Jzh-li/LLMate-Gate/issues

Last updated: 2026-09-12