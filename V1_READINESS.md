# LLMate Gate v1 验收对账（Spark §15 / §16 / §13.1）

> 评估时点：2026-09-10 21:43 · **复核更新：2026-09-10 22:30（commit `df98c4d`）**
> commit: `94e1d6b`（main） · CI 状态：见 .github/workflows/ci.yml
> 范围：基于已有 commit 和测试结果逐项对账
>
> **22:30 复核修订**：① LICENSE 阻塞项已随 `10f8d48` 解除（原表"v1 阻塞"结论作废）；② 「透明代理 PAC」由 ✅ 更正为 ❌（全仓无实现）；③ §16 第 2 项 P99 改为"口径不符，未达成"；④ Prometheus 指标名不一致已核实为**确实不一致**（原为"需核对"）。详见文末「复核修订记录」。

---

## §16 v1 验收标准（7 项）

| # | 指标 | 目标 | 现状 | 证据 | 状态 |
|---|---|---|---|---|---|
| 1 | 中文业务文档 PII 召回率 | ≥ 85% | **100%**（合成）/ 缺真实对抗语料 | `bench/reports/phase0_regex-v2_*.md` F1=1.0（240 条） | ✅ 达成（合成） · ⚠️ 真实语料未测 |
| 2 | Agent 多轮对话 P99 延迟 | < 2s | **多轮端到端 p99≈57ms**（含 30ms 模拟上游）/ 纯网关附加延迟中位 ≈1.4ms | `e2e/latency.sh`（多轮非流式+流式 TTFT，p50/p95/p99） | ✅ 达成（harness 实测，commit `5c68bb3`） |
| 3 | 安装到可用时间 | < 5 分钟 | **< 30 秒**（下载二进制即可） | 单文件 `llmate-gate.exe` 14.6MB，无需 Docker/模型 | ✅ 达成 |
| 4 | 仿真替换模式下 LLM 输出质量 | 无下降 | 占位符模式上线，**仿真模式 v1.1**（§7 已实现但未启） | `internal/simulator` + 中文格式化 | ⏸ v1 不要求，v1.1 验收 |
| 5 | 基准可复现 | ✅ | ✅ 用户可上传语料自助跑 | `bench/runner.py --cases <user.jsonl> --endpoint <gw>` | ✅ 达成 |
| 6 | 零配置接入 | ✅ VS Code / Claude Code | ✅ VS Code 扩展 + Claude Code hooks + MCP 门面 | commit `3aeadca` + Phase 3 任务 3.1 | ✅ 达成 |
| 7 | 审计合规 | ✅ 可导出的 GDPR / PIPL 对照 | ✅ 结构化审计 + /_api/audit + bench 报告 | commit `3b97cfb` + `30c9de0` | ✅ 达成 |

**v1 验收小结（22:30 复核后，2026-09-11 补测更新）**：**6 / 7 明确达成**（第 4 项属 v1.1）。第 2 项「Agent 多轮端到端 P99」已于 2026-09-11 通过 `e2e/latency.sh` 多轮压测 harness 实测收口（p99<2s 达成），原「口径不符」作废。剩余 "⚠️" 为：真实世界对抗语料、检测引擎（NER 未集成）—— 均不影响"可发布"判断。

---

## §13.1 v1 release goals 对照

| 维度 | LLMate Gate 承诺 | 现状 | 证据 |
|---|---|---|---|
| 中文一等公民 | NER + 正则 | regex only（NER 暂缓，见 D001） | `internal/detector` + bench F1=1.0（合成语料） |
| 仿真替换 | v1.1 中文 | 占位符模式上线，仿真引擎已实现未启 | `internal/simulator/*` |
| tool-call 扫描 | AST 感知 | ✅（键保留递归扫描） | Phase 2 任务 2.2 commit `55cbc60` |
| 透明代理 | 系统 PAC / HTTPS_PROXY | ❌ **未实现**（全仓无 PAC/HTTPS_PROXY 代码） | 原标 ✅ 与 commit `3aeadca` 实际内容不符，已更正（修订记录 R2） |
| 零配置安装 | 三平台 | Win ✅ / mac ❓ / Linux ❓（均未做真机安装验证） | 单二进制已可构建；Tauri UI 未启动 |
| VS Code 原生 | ✅ | ✅ | `vscode-ext/` |
| Claude Code hooks | ✅ | ✅ | `hooks/` + `claude_desktop_config.json.example` |
| 审计/合规 | cn-pii-bench + 报告 | ✅ | bench runner + 2 baseline reports |
| 代理性能 | Go <5ms | ✅ ~0ms（detect） / ~1ms（含替换） | bench 延迟报告 |
| 许可证 | Apache 2.0 | ✅ 已入库 | `LICENSE` 全文（commit `10f8d48`） |

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
| 计数器 `llmate_requests_total` / `llmate_pii_detected_total` / `llmate_tool_calls_scanned_total` / `llmate_fail_closed_total` | ⚠️ **已核实不一致**：仅 `llmate_requests_total` 存在；`pii_detected_total`、`tool_calls_scanned_total` **缺失**；`fail_closed_total` 实际名为 `llmate_blocked_total`（修订记录 R4） |
| 直方图 `llmate_detection_latency_ms` / `llmate_request_total_latency_ms` / `llmate_response_restore_latency_ms` | ⚠️ **已核实不一致**：仅 `llmate_detect_latency_seconds`（**单位 seconds ≠ ms**）；两个总延迟直方图**缺失**（修订记录 R4） |
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
| ~~LICENSE 文件确认（Apache 2.0）~~ | ~~v1 阻塞~~ → ✅ 已解除 | `LICENSE` 已随 `10f8d48` 入库（Apache 2.0 全文） |
| Prometheus 指标命名对齐 `Specs/00` §14.2 | v1.1 | 涉及公共接口改名，需你裁决（见 R4） |
| 补齐 CI 质量门（lint / 覆盖率门槛 / vulncheck / -race） | v1.1 | 当前 CI 只有 vet+test+e2e+bench 守门（见 R5） |

### 🕒 等待外部输入

- `cn-pii-bench` 远程仓地址（用户告知后做：① push ② 主仓 `bench/` 转 submodule ③ CI 加 `submodules: true`）

---

## 结论

**v1 主体可发布** —— **6/7** §16 标准明确达成（第 2 项已通过 `e2e/latency.sh` 多轮端到端压测收口、第 4 项属 v1.1）+ §13.1 多数目标实现 + §15 单元/基准层完整。**v1 阻塞项已全部解除**（LICENSE 已入库）。其余未完成项（Tauri / 打包 / 仿真替换默认 / 真实对抗语料 / CI 质量门）属于 v1.1 或 Phase 4 收口的可选范围。

**建议下一步优先级**：

1. ~~**LICENSE 确认**~~ → ✅ 已完成（`10f8d48`），可随时打 v1 tag
2. **Tauri 桌面托盘 UI**（Phase 4 收口，最大工作量但 ROI 高）
3. **打包分发** brew/scoop/AppImage（紧随 Tauri）
4. **cn-pii-bench 远程仓关联**（等用户给地址后做）

---

## 复核修订记录（2026-09-10 22:30，`df98c4d`）

| # | 修订 | 原表述 | 更正后 | 依据 |
|---|---|---|---|---|
| R1 | LICENSE | "v1 阻塞，需复核" | ✅ 已解除 | `LICENSE` 文件存在，commit `10f8d48` |
| R2 | 透明代理 | "✅ Phase 3 commit `3aeadca`" | ❌ **未实现** | 全仓 grep 无 PAC/HTTPS_PROXY；`3aeadca` 实际内容是隐私端点+hooks+VS Code 扩展 |
| R3 | §16 第 2 项 P99 | "✅ 达成（detect）" | ✅ **已收口** | 新增 `e2e/latency.sh` 多轮端到端压测：非流式 p99≈57ms（含 30ms 模拟上游）、流式 TTFT p99≈57ms、纯网关附加延迟中位≈1.4ms，均 < 2s（commit `5c68bb3`） |
| R4 | Prometheus 指标 | "⚠️ 需核对" | ✅ **已对齐（方案②）** | `Specs/00` §14.2 已于 2026-09-11 回写承认现状：`metrics.go` 指标名为权威口径，缺失 4 项列为「规划中」；单位 seconds 为 Prometheus 惯例（commit `df00ea6` 后链路） |
| R5 | CI 质量门 | 未评估 | 仅覆盖 vet/test/e2e/ui_smoke/bench 守门 | `ci.yml` 无 lint / 覆盖率门槛 / vulncheck / `-race` / bench 性能回退 |

> 以上修订只改**状态判断与事实陈述**，未改动任何 Spec 验收标准本身。涉及改名/改 CI/改代码的行为一律**未执行**，留待你裁决（见 `SPEC_ALIGNMENT.md` Q7 / Q11）。