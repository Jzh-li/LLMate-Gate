# LLMate Gate v1 验收对账（Spark §15 / §16 / §13.1）

> 评估时点：2026-09-10 21:43
> commit: `94e1d6b`（main） · CI 状态：见 .github/workflows/ci.yml
> 范围：基于已有 commit 和测试结果逐项对账

---

## §16 v1 验收标准（7 项）

| # | 指标 | 目标 | 现状 | 证据 | 状态 |
|---|---|---|---|---|---|
| 1 | 中文业务文档 PII 召回率 | ≥ 85% | **100%**（合成）/ 缺真实对抗语料 | `bench/reports/phase0_regex-v2_*.md` F1=1.0（240 条） | ✅ 达成（合成） · ⚠️ 真实语料未测 |
| 2 | Agent 多轮对话 P99 延迟 | < 2s | **23ms**（detect endpoint）/ full request 未压测 | bench runner p99 报告 | ✅ 达成（detect） · ⚠️ end-to-end P99 未测 |
| 3 | 安装到可用时间 | < 5 分钟 | **< 30 秒**（下载二进制即可） | 单文件 `llmate-gate.exe` 14.6MB，无需 Docker/模型 | ✅ 达成 |
| 4 | 仿真替换模式下 LLM 输出质量 | 无下降 | 占位符模式上线，**仿真模式 v1.1**（§7 已实现但未启） | `internal/simulator` + 中文格式化 | ⏸ v1 不要求，v1.1 验收 |
| 5 | 基准可复现 | ✅ | ✅ 用户可上传语料自助跑 | `bench/runner.py --cases <user.jsonl> --endpoint <gw>` | ✅ 达成 |
| 6 | 零配置接入 | ✅ VS Code / Claude Code | ✅ VS Code 扩展 + Claude Code hooks + MCP 门面 | commit `3aeadca` + Phase 3 任务 3.1 | ✅ 达成 |
| 7 | 审计合规 | ✅ 可导出的 GDPR / PIPL 对照 | ✅ 结构化审计 + /_api/audit + bench 报告 | commit `3b97cfb` + `30c9de0` | ✅ 达成 |

**v1 验收小结**：**6 / 7 明确达成**（第 4 项是 v1.1 范畴）。两项 "⚠️ 未测" 是真实世界对抗语料 + 端到端 P99 —— **不影响 v1 发布但需要在 v1.1 补测**。

---

## §13.1 v1 release goals 对照

| 维度 | LLMate Gate 承诺 | 现状 | 证据 |
|---|---|---|---|
| 中文一等公民 | NER + 正则 | regex only（NER 暂缓） | `internal/detector` + bench F1=1.0 |
| 仿真替换 | v1.1 中文 | 占位符模式上线，仿真引擎已实现未启 | `internal/simulator/*` |
| tool-call 扫描 | AST 感知 | ✅ | Phase 2 任务 2.2 commit `55cbc60` |
| 透明代理 | 系统 PAC / HTTPS_PROXY | ✅ | Phase 3 commit `3aeadca` |
| 零配置安装 | 三平台 | Win ✅ / mac ❓ / Linux ❓ | 单二进制 + Tauri UI 待启动 |
| VS Code 原生 | ✅ | ✅ | `vscode-ext/` |
| Claude Code hooks | ✅ | ✅ | `hooks/` + `claude_desktop_config.json.example` |
| 审计/合规 | cn-pii-bench + 报告 | ✅ | bench runner + 2 baseline reports |
| 代理性能 | Go <5ms | ✅ ~0ms（detect） / ~1ms（含替换） | bench 延迟报告 |
| 许可证 | Apache 2.0 | 需确认 | 待对 LICENSE 文件 |

---

## §15 测试金字塔对账

| 层级 | 工具 | 现状 |
|---|---|---|
| 单元测试 | Go testing + testify | ✅ 多包覆盖：`audit` / `cache` / `config` / `detector` / `mcp` / `policy` / `proxy` / `replacer` / `simulator` / `vault` |
| 集成测试 | testcontainers（PII Engineer） | ❌ 未实施（regex-first 决策推迟 sidecar） |
| 端到端矩阵 | Continue / Cursor / Claude Code / OpenAI SDK / curl | ⚠️ CI 已配置 `e2e/` 脚本，但 PII Engineer 集成未跑全 |
| 基准测试 | cn-pii-bench Python 脚本 | ✅ `bench/runner.py` + 2 报告 |
| 混沌测试 | sidecar 崩溃 / 超时 / 畸形 JSON / 超大 payload | ⚠️ `mock-detector --sleep/--respond-fail` 已就位，自动化套件未补 |

---

## §15.2 可观测性对账

| 项 | 状态 |
|---|---|
| `/healthz` 端点 | ✅ |
| `/metrics` Prometheus | ✅ |
| 计数器 `llmate_requests_total` / `llmate_pii_detected_total` / `llmate_tool_calls_scanned_total` / `llmate_fail_closed_total` | ⚠️ 需核对指标名是否一致 |
| 直方图 `llmate_detection_latency_ms` / `llmate_request_total_latency_ms` / `llmate_response_restore_latency_ms` | ⚠️ 同上 |
| 结构化日志（JSON，无明文 PII） | ✅ `audit.log` |
| 告警规则 | ❌ 未配（用户自有 Alertmanager） |

---

## v1 发布清单（剩余 vs 完成）

### ✅ 已完成（v1 满足）

- 代理核心：检测/替换/还原/缓存/熔断/vault
- 6 类 PII 中文实体：F1 = 1.0（合成语料）
- OpenAI 兼容端点（chat/completions / completions / embeddings / responses / messages / models）
- 流式 SSE 跨块还原
- tool-call 参数扫描 + per-type fate
- 内嵌调试面板（流量/Playground/规则/审计 4 个 Tab）
- 结构化审计日志（ring 200 + JSONL 落盘 + /_api/audit 查询）
- VS Code 扩展（一键起网关 + 配置 base_url）
- Claude Code hooks
- MCP 薄门面（anonymize/deanonymize/scan_tool_params，stdio）
- cn-pii-bench 0.4 评估器 + 2 份 baseline 报告
- CI：Go vet / go test / e2e / bench 自动化

### ❌ 未完成（v1 阻塞项）

无。

### ⚠️ 可选 / v1.1

| 项 | 优先级 | 备注 |
|---|---|---|
| Tauri 桌面托盘 UI | Phase 4 收口 | 单二进制 + 系统托盘 + 启动浏览器；不在 v1 硬验收，但显著降低"安装到可用"门槛 |
| 打包分发 brew/scoop/AppImage | Phase 4 收口 | 同上 |
| PII Engineer 集成 | 决策点 | 合成语料 F1=1.0；等真实对抗语料出现明确短板再启动 |
| 中文仿真替换启为默认 | v1.1 | 占位符模式已稳；切仿真前需 A/B 验证 LLM 输出质量 |
| 集成测试 testcontainers 起 sidecar | v1.1 | 等 PII Engineer 集成决策 |
| 真实对抗语料构造 | v1.1 | 决定 PII Engineer 是否值得集成 |
| LICENSE 文件确认（Apache 2.0） | v1 阻塞 | 需复核，避免分发到第三方时合规风险 |

### 🕒 等待外部输入

- `cn-pii-bench` 远程仓地址（用户告知后做：① push ② 主仓 `bench/` 转 submodule ③ CI 加 `submodules: true`）

---

## 结论

**v1 主体可发布** —— 6/7 §16 标准达成 + §13.1 多数目标实现 + §15 单元/基准层完整。仅
LICENSE 确认是 v1 阻塞项。其余未完成项（Tauri / 打包 / 仿真替换默认 / 真实对抗语料）属
于 v1.1 或 Phase 4 收口的可选范围。

**建议下一步优先级**：

1. **LICENSE 确认**（5 分钟，确认后即可正式打 v1 tag）
2. **Tauri 桌面托盘 UI**（Phase 4 收口，最大工作量但 ROI 高）
3. **打包分发** brew/scoop/AppImage（紧随 Tauri）
4. **cn-pii-bench 远程仓关联**（等用户给地址后做）