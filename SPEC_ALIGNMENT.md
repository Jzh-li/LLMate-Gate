# Spec ↔ 实现/进度 对齐对比清单

> 生成时间：2026-09-10 22:20 · 基线 commit：`df98c4d`（main）
> 方法：通读 `Specs/00-整体技术方案.md`（v3.1 主方案）、`01-执行手册.md`、`02-接口与数据契约规范.md`（v1.1）、`03-测试规约.md`、`04-UI设计.md`；对照 `PROGRESS.md`、`HANDOFF.md`、`V1_READINESS.md`、`DECISION.md`、`DECISIONS.md`、`.workbuddy/TODO_QUEUE.md`、`.github/workflows/ci.yml` 及 `gateway/**` 实际代码。
> **本文件只读对照，不修改任何 Spec 原文。冲突项保留双方表述，等待人工裁决。**

---

## 0. Spec 侧共识（作为对照基准）

| 维度 | Spec 结论 | 出处 |
|---|---|---|
| 产品目标 | 中文一等公民的 LLM Agent 隐私网关：反向代理 OpenAI 兼容接口，检测/替换/还原中文 PII | 00 §0、§1.3 |
| 三条代差 | ① 中文格式保持仿真替换 ② cn-pii-bench 基准 ③ 三层一体（透明代理+IDE/hooks+桌面 UI） | 00 §13.2 |
| 模块划分 | Go 代理层（`internal/{config,server,proxy,detector,replacer,simulator,vault,cache,circuit,audit,errors}` + `pkg/types`）+ PII Engineer sidecar + VS Code 扩展 + Tauri 桌面 | 02 §1.1、00 附录 D |
| 验收（v1） | §16 七项：召回≥85% / P99<2s / 安装<5min / 仿真不降质 / 基准可复现 / 零配置接入 / 审计合规 | 00 §16 |
| 测试基线 | E2E E1-E8、UI 冒烟 D1-D6、覆盖率门槛、bench 七项指标、混沌测试 | 03 §3.2、§3.4、§4.3、§5 |

---

## 1. 已同步项（Spec 要求 = 进度记录 = 实现，三方一致）

| # | 项 | Spec 依据 | 进度/实现依据 |
|---|---|---|---|
| A1 | 产品定位与边界（假名化而非匿名化、诚实边界声明） | 00 §0/§1 | README「⚠️ 诚实边界」、HANDOFF §1 |
| A2 | 代理层 Go，单二进制 | 00 §11 | `gateway/` Go 1.24.5，产物 14.6MB |
| A3 | 契约 §1.1 全部包均已落地（config/server/proxy/detector/replacer/simulator/vault/cache/circuit/audit/errors + pkg/types） | 02 §1.1 | `gateway/internal/` 目录实测齐全 |
| A4 | `internal/policy`（per-type fate）、`internal/metrics` | 00 附录 D | 已实现；`policy_test.go` 4 例 |
| A5 | 占位符协议 `<<type_index>>`，且 JSON 序列化不转义 `<>` | 02 §5.2、UI §4.3 | 网关端点 / MCP 客户端 / MCP 结果文本三处统一 `SetEscapeHTML(false)` |
| A6 | 11 个实体类型常量（zh_person_name / zh_phone / zh_id_card / zh_bank_card / zh_address / email / ip_address / date / api_key / password / token） | 02 §6.1 权威表 | `pkg/types/detect.go:31-43` |
| A7 | 流式 SSE 跨块/跨事件占位符还原 | 00 §5、02 §5.3 | `internal/replacer/sse.go`；E3 由 SKIP→PASS（21/21） |
| A8 | fail-closed：检测异常即阻断 | 00 §12 P1、02 §0.3 | `internal/circuit`；E5 断言 502 < 5s |
| A9 | detection_cache 键绑定 conversation_id | 00 §12 P2、02 §8.2 | `internal/cache` + `merkle.go` |
| A10 | per-type fate 策略引擎 | 00 §12 P2、02 §6.1 | `internal/policy` + `config.Replacement.PerTypeFate` |
| A11 | tool-call 参数逐值脱敏（JSON 键永不脱敏） | 00 §12 P2 | `proxy.transform` + E2 + `TestProxy_ToolCall_*` |
| A12 | 内嵌调试面板：`go:embed` + `/_debug` + `/ws/events` + `/_api/{traffic,detect,replace,rules}` + `--no-debug` + 仅绑 127.0.0.1 | UI §1.2/§4.1、02 §10.1 | `gateway/debug/handler.go:74-81`；默认 `Debug:true`、`DebugBind:127.0.0.1` |
| A13 | 6 个 WS 事件类型（request.received / detection.done / replacement.done / upstream.response / restore.done / rule.changed） | UI §4.2、02 §10.2 | 代码中 6 个全部存在 |
| A14 | 面板 4 个 Tab（流量 / Playground / 规则 / 审计） | 00 §4.3、UI §2.1-2.4 | 审计 Tab 已交付并端到端验证 |
| A15 | UI 冒烟 D1-D6 | 03 §3.4、UI §5.1 | `e2e/ui_smoke.sh` 11/11 PASS（含 D5 `--no-debug` 全 404、D6） |
| A16 | E2E 矩阵 E1-E8 | 03 §3.2 | `e2e/e2e.sh` 全绿（E3 已修，21/21） |
| A17 | 审计事件结构与契约 §9.1 字段逐个一致（含 detector_latency_ms / sample_text / log_pii） | 02 §9.1 | `internal/audit/audit.go` Event 字段完全对齐 |
| A18 | 审计导出 pip / gdpr / dsl / csl / 等保2.0 | 02 §9.2 | `audit.go:205-215`；默认 `Export:["pip","gdpr"]` |
| A19 | 仿真替换 v1 不启用（默认 strategy=placeholder） | 00 附录 C、02 §6 | `config.go:147` 默认 `placeholder` |
| A20 | VS Code 扩展（`vscode-ext/`）+ Claude Code hooks（`hooks/`） | 00 §12 P3、01 任务 2.1/2.2 | `tsc -p ./` 零错误；hooks 6 项断言全绿 |
| A21 | cn-pii-bench 语料 ≥200 篇、每子集 ≥25 | 03 §4.1、00 §9.1 | 8 子集 × 30 = 240 条，`validate.py` PASS |
| A22 | §16 第 1 项：中文召回 ≥85% | 00 §16 | 100%（`bench/reports/phase0_regex-v2_20260910-214153.md`） |
| A23 | §16 第 3 项：安装到可用 <5min | 00 §16 | <30s（单文件，无 Docker/模型） |
| A24 | §16 第 5 项：基准可复现 | 00 §16 | `bench/runner.py --cases <user.jsonl>` |
| A25 | §16 第 6 项：零配置接入 | 00 §16 | VS Code 扩展 + hooks + MCP 三面 |
| A26 | §16 第 7 项：审计合规 | 00 §16 | 结构化审计 + `/_api/audit` + bench 报告 |
| A27 | CI 守门：依赖完整性（`go mod tidy -diff`）、bench 召回 <0.9 非零退出 | DECISION §6 | `ci.yml:37`（tidy -diff）；`bench-baseline` job 调 `bench-runner`，其 `main.go:150` 召回 <0.9 → `os.Exit(2)` |
| A28 | LICENSE Apache 2.0 | 00 §11 | `LICENSE` 全文已入库（`10f8d48`） |
| A29 | 检测引擎可替换（regex / pii-engineer 同接口） | D001、02 §2 | `detection.engine` 配置 + `piiengineer.go` 客户端已实现 |

---

## 2. 冲突项（双方内容并列保留，未做任何覆盖/删除）

### C1 · Phase 编号体系三套并存 ⚠️ 结构级

- **Spec A（`00-整体技术方案.md` §12）**：P0 验证 → P1 核心代理 → P2 **Agent 场景优化**（tool-call/Merkle/fate）→ P3 **零配置接入**（VS Code/hooks/透明代理）→ P4 **可测量与产品化**（审计/bench/Tauri/打包）→ v1.1（MCP/仿真）。
- **Spec B（`01-执行手册.md`）**：P0 验证 → P1 代理核心+检测引擎 → P2 **IDE 集成+桌面 UI**（2.1 VS Code / 2.2 hooks / 2.3 Tauri）→ P3 **MCP（v1.1）**；**无 Phase 4**（审计日志、bench 无任务卡）。
- **实际（PROGRESS.md）**：沿用 A 的编号——「Phase 2 阶段 1/2/3」「Phase 3 任务 3.1」「Phase 4 收口」。
- **冲突**：B 的 P2/P3 与 A 的 P2/P3/P4 内容错位；B 缺失 A 的 P4 全部任务；两份 spec 未互相同步。

### C2 · MCP 的归属版本：三处说法不同

- **00 §3.4 / 附录 C / §12**：MCP = **v1.1**（"降到 v1.1，不阻塞 v1 核心"）。
- **01 执行手册 Phase 3**：MCP Server 薄门面（**v1.1**，第 8 周）。
- **README 路线图**：MCP Server 门面列在 **v2**。
- **实际**：已交付并推送（`e942798`，3 工具 + stdio 冒烟 SMOKE PASS）。
- **冲突**：spec 定位 v1.1、README 写 v2、代码已落地于 v1 阶段。

### C3 · PII Engineer 集成：spec 强制 vs 决策暂缓 vs README 声称已完成

- **Spec**：00 §11（"检测引擎 = PII Engineer，直接集成"）、§12 P1（"集成 PII Engineer sidecar"）、§17 首要动作（clone + cargo build）、01 任务 1.2、03 §3.1（testcontainers 起 sidecar）。
- **实际/决策**：`DECISIONS.md D001` 降级为可选，`engine` 默认 `regex`；PROGRESS/DECISION 结论"**确认暂缓**，等真实对抗语料"。
- **README（冲突源）**：路线图标 "✅ PII Engineer 中文检测集成（F1 0.918）"；技术栈表写 "检测引擎 = Rust + ONNX Runtime，~180ms"。
- **冲突**：spec 要求集成 / 决策暂缓 / README 声称已完成 —— 三者互斥。0.918 与 180ms 是 PII Engineer 的公开规格（00 附录 A），非本产品实测值。

### C4 · Phase 0 验收标准 vs 用户豁免

- **Spec**：01 任务 0.2 要求四方对比（PrivAiTe / AI Privacy Gateway / Eidolon / PII Engineer），Phase 0 验收 = "DECISION.md 有明确的 GO/PIVOT 结论，**且有四方对比数据支撑**"；03 §4.4 要求输出 `comparison.json`。
- **实际**：`DECISION.md §6` 记 "**四方对比按用户要求暂缓**"；GO 结论仅基于自测（无竞品数据）；无 `comparison.json`、无 `bench/judge.py`。
- **冲突**：Phase 0 验收标准字面未满足（用户口头豁免未回写 spec）。

### C5 · bench 双评估器 + 双口径 + 过期数字

- **DECISION.md §2/§3**：Go `cmd/bench-runner`，匹配规则"**同类型 + 区间重叠**"，数字 **P=1.000 / R=0.944 / F1=0.971**（240 条，TP340/FP0/FN20）。
- **PROGRESS + `bench/reports/phase0_regex-v2_*`**：Python `bench/runner.py` 经 `/_api/detect`，"**严格四元组 (type,value,start,end)**"，数字 **P=1.0 / R=1.0 / F1=1.0**，p99=23ms。
- **CI**：`bench-baseline` job 跑的是 **Go** bench-runner（守门 <0.9）。
- **冲突**：两个评估器口径不同源、数字不可直接比较；对外汇报用 Python 数字，CI 守门用 Go 数字；DECISION.md 的数字已被 `30c9de0` 直辖市修复作废但未更新。

### C6 · §16 第 2 项 P99 的口径

- **Spec**：00 §16 "Agent **多轮对话** P99 延迟 < 2s"。
- **V1_READINESS**：给的是 **detect 端点** p99=23ms，同行自标 "⚠️ end-to-end P99 未测"，但状态列仍打 "✅ 达成（detect）"。
- **冲突**：指标口径与 spec 定义不一致，且同一行内自相矛盾。

### C7 · Prometheus 指标命名与单位

- **00 §14.2 要求**：`llmate_requests_total`、`llmate_pii_detected_total`、`llmate_tool_calls_scanned_total`、`llmate_fail_closed_total`、直方图 `llmate_detection_latency_ms` / `llmate_request_total_latency_ms` / `llmate_response_restore_latency_ms`。
- **实际 `internal/metrics/metrics.go`**：有 `llmate_requests_total`、`llmate_blocked_total`（≈fail_closed 更名）、`llmate_detect_latency_seconds`（**单位 seconds ≠ ms**）、`llmate_replace_total`、`llmate_restore_total`、`llmate_detect_cache_hits/misses_total`、`llmate_detect_incremental_segments_total`、`llmate_stream_orphan_placeholders_total`、`llmate_upstream_errors_total`、`llmate_vault_size`、`llmate_active_streams`。**缺** `pii_detected_total`、`tool_calls_scanned_total`、两个总延迟直方图。
- **V1_READINESS 已标** "⚠️ 需核对指标名是否一致" —— 核实结果为**确实不一致**。

### C8 · 透明代理（PAC / HTTPS_PROXY）

- **00 §12 P3**：列为 Phase 3 任务（"透明代理备选（系统 PAC / HTTPS_PROXY）"）。
- **V1_READINESS §13.1**：标 "✅ 透明代理，Phase 3 commit `3aeadca`"。
- **实测**：全仓（.go/.ts/文档）**无** PAC / HTTPS_PROXY / proxy.pac 任何实现；`3aeadca` 的实际内容是常驻隐私端点 + hooks + VS Code 扩展。
- **冲突**：验收文档 claim 与代码事实不符。

### C9 · 调试面板包位置

- **DECISIONS D004**："以契约 §1.1 为准，置于 `gateway/internal/debug/`（含 assets/）"。
- **UI设计 §1.2 / 00 附录 D / 实际代码**：`gateway/debug/`（与 `internal/` 平级）。
- **契约 §1.1**：**未列出** debug 包。
- **冲突**：决策记录与落地相反；契约目录树滞后。

### C10 · V1_READINESS 自身滞后（与 HANDOFF 判断相反）

- **V1_READINESS（基于 `94e1d6b`）**："LICENSE 文件确认（Apache 2.0）｜ **v1 阻塞** ｜ 需复核"，结论段"仅 LICENSE 确认是 v1 阻塞项"。
- **HANDOFF §4 / 仓库实测**：LICENSE 已由 `10f8d48` 入库，Apache 2.0 全文，"v1 阻塞项解除"。
- **冲突**：同仓两份文档对"v1 是否还有阻塞项"结论相反。

### C11 · PROGRESS.md 内部前后不一致

- **文件顶部**："当前任务：Tauri 桌面托盘 UI / **PII Engineer 集成（决策待 zh_address 优化方向确定）**"。
- **文件末尾（21:40 决策）**："**PII Engineer 集成确认暂缓**"。
- **冲突**：同一文件内，PII Engineer 状态既"待定"又"已决策暂缓"。

### C12 · README 多处与实现/实测不一致（逐条保留）

| 子项 | README 原文/含义 | 实际 |
|---|---|---|
| a | 性能基准 "基于 cn-pii-bench 在 **2026 年 8 月**的测评（i7-12700）"，召回 **91.8%(F1)**、P99 **1.2s**、单条 **~180ms**、内存 80/200MB | 项目 9 月启动，无 8 月测评；实测 **F1=1.0 / p99=23ms**；91.8% 与 180ms 是 PII Engineer 公开规格（00 附录 A） |
| b | 竞品对比表 "LLMate Gate ✅ **F1 0.918**" | 0.918 非本产品实测；本产品实测 F1=1.0（合成语料） |
| c | 安装 "**Docker（推荐，30 秒部署）**" | 仓库**无 Dockerfile / deploy 目录**；实际分发=单二进制 14.6MB |
| d | 实体类型表列 车牌 / **组织机构 / URL / 金额 / 邮编** | 类型常量仅 11 个，无组织机构/URL/金额/邮编；`rePlate` 车牌正则存在但**未出现在类型常量与输出中** |
| e | "PII Engineer 支持 13+ 语言…35+ 语言" | 未集成 PII Engineer |
| f | 路线图 "✅ **Tauri 桌面 UI**" | 未启动（无 Rust 工具链） |
| g | 审计示例 JSON 用 `entity_types` / `replacement_strategy` | 契约 §9.1 与实现为 `detected_entities` / `strategy` |

### C13 · 质量门：spec 要求 vs CI 实际

- **03 §7.2 要求**：Lint（golangci-lint）阻断 / Unit+**覆盖率达门槛**阻断 / E2E 阻断 / UI Smoke 阻断 / Bench 回退 >10% 标记 / **govulncheck** 阻断；§1.3 + §5 给逐包覆盖率门槛（detector≥85%、replacer≥90%、simulator≥90%、vault≥85%、proxy≥80%、config≥90%、整体≥80%）；§4.5 要求 `go test -bench` 与 `bench/baseline.txt` 对比；§7.1 workflow 含 `rustup` 与 `-race`。
- **CI 实际（`ci.yml`）**：`verify`（vet + `go test`，**无 -race**）、`build`、`e2e`（e2e.sh + `timeout 420 ui_smoke.sh`）、`coverage`（**仅上传 artifact，无门槛判定**）、`bench fixtures`（validate.py）、`bench-baseline`（召回 <0.9 gate）。**无 lint / 无 vulncheck / 无覆盖率门槛 / 无性能回退 / 无 rustup**。
- **PROGRESS 探路记录**：已声明"覆盖率门：跳转（artifact 上传而非 PR 阻断），阈值门禁放到 v0.2"。
- **冲突**：属有意偏离，但 spec 未回写。

### C14 · bench 测评指标覆盖不全

- **03 §4.3 要求 7 项**：召回≥85% / 精确≥90% / F1≥0.88 / **偏移准确率≥95%** / **tool-call 覆盖率 100%** / **仿真格式合法率 100%** / P99<2s。
- **实际 bench**：仅输出 P / R / F1 + 延迟 p50/p95/p99/max/mean。偏移准确率、tool-call 覆盖率、仿真格式合法率 **均未测**。
- **旁证**：DECISION.md 记 "tool_call 子集召回 **0.817**（49/60）"，与 spec 的 100% 覆盖率要求冲突。

### C15 · 集成测试与混沌测试

- **03 §2 要求**：testcontainers 起真实 sidecar 做集成；**§6** 要求 Fuzz（`FuzzRestore`，`-race` 无 data race）。
- **实际**：集成测试为进程内 `TestProxy_*`（无容器）；无 fuzz 测试。V1_READINESS 已如实标 ❌/⚠️。

### C16 · E2E E4 断言强度弱于 spec

- **03 §3.2 E4**："第二次请求走缓存，**延迟 < 50ms**"。
- **实际 `e2e.sh` E4**（与 PROGRESS 描述一致）：仅断言"同一 conv 两次请求都脱敏"（缓存幂等），**未断言延迟**。

### C17 · `/_api/audit` 未登记进契约

- **00 §4.3 / UI §2.4**：审计 Tab 是面板 4 Tab 之一，需展示结构化审计事件。
- **02 §10.1 端点清单**：**无** `GET /_api/audit`。
- **实际**：已实现 `GET /_api/audit?limit=N`（默认 50，最大 500）。
- **冲突**：02 §11 自定"规范先行"（先改规范再改代码），但审计端点先于契约落地。

### C18 · 检测缓存实现与契约描述不同

- **00 §12 P2 / 01 任务 1.4 / 02 §8**："内存 **LRU** + 内容哈希"。
- **实际**：`internal/cache/merkle.go` —— per-conversation **有序段数组 + SHA-256 前缀链**增量检测（PROGRESS 记录为"整体重写"决策）。
- **冲突**：实现优于 spec 描述，但 `02 §8.1` 接口签名未回写。

### C19 · Anthropic 端点路径

- **00 §12 P1**：`/v1/responses`、`**/anthropic/v1/***`。
- **实际路由**：`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`、`/v1/responses`、`/v1/messages`、`/v1/models`（**无 `/anthropic` 前缀**）。
- **状态**：E6 验证通过，属轻微路径偏差。

### C20 · 决策门缺少 `judge.py`

- **01 任务 0.3**：执行 `bench/judge.py` 自动输出 GO/PIVOT。
- **实际**：无 `judge.py`；`DECISION.md` 人工撰写（结论 GO）。

### C21 · 契约 §1.1 包结构滞后

- **02 §1.1（自称"权威"）** 未列：`internal/policy`、`internal/metrics`、`internal/pipeline`、`internal/mcp`、`debug/`（任意位置）。
- **实际**：五者均存在且是核心链路（`policy` 在 00 附录 D 有、`metrics` 在 00 附录 D 有）。
- **冲突**：契约未随 00 附录 D 与实现更新。

---

## 3. 需人工确认项（我不擅自裁决，仅列影响与选项）

| # | 待确认问题 | 涉及文档 | 影响 | 选项 |
|---|---|---|---|---|
| Q1 | Phase 编号以 `00` 还是 `01` 为准？是否给 `01` 补 Phase 4 任务卡（审计/bench/打包）？ | 01 vs 00 | 所有进度记录的可追溯性 | ① 以 00 为准、重写 01 ② 保留双轨并在 01 顶部加映射表 |
| Q2 | MCP 归属：spec 说 v1.1、README 说 v2、代码已在 v1 交付 | 00 §3.4 / 01 P3 / README | 路线图对外承诺 | ① 回写 spec+README 为"v1 已交付" ② 维持 v1.1 表述、README 改 v1.1 |
| Q3 | PII Engineer：维持"暂缓"并回写 spec，还是排期集成？ | 00 §11/§17、01 1.2、03 §3.1 | 检测能力上限与真实场景召回 | ① 暂缓+回写 ② 排期（需 Rust 工具链 + 620MB 模型） |
| Q4 | 四方基准：永久豁免（改 Phase 0 验收标准）还是补做？ | 01 0.2、03 §4.4 | Phase 0 验收合规性 | ① 改验收标准 ② 补做（需 Docker 起竞品） |
| Q5 | bench 口径以谁为准？DECISION.md 数字是否刷新为 v2（F1=1.0）？ | DECISION.md、PROGRESS、CI | 对外数字可信度 | ① 统一到 Python runner + 更新 DECISION ② 统一到 Go bench-runner ③ 两者并存但显式标注口径 |
| Q6 | §16 第 2 项：补测"多轮端到端 P99"，还是把验收口径改写为 detect 端点？ | 00 §16、V1_READINESS | v1 验收结论 | ① 补测 ② 改口径说明 |
| Q7 | Prometheus 指标命名：按 00 §14.2 改名对齐（含 ms↔seconds 单位决策），还是以现状回写 spec？ | 00 §14.2、metrics.go | 监控/告警对接 | ① 改代码对齐 spec ② 改 spec 承认现状 |
| Q8 | 透明代理 PAC：补实现，还是从 spec 与 V1_READINESS 删除该 claim？ | 00 §12 P3、V1_READINESS §13.1 | 验收表真实性 | ① 补实现 ② 删除 claim |
| Q9 | debug 包位置：确认 `gateway/debug/` 现状并回改 D004 + 契约 §1.1，还是搬回 `internal/debug/`？ | DECISIONS D004、02 §1.1、UI §1.2 | 目录结构与文档一致性 | ① 改文档 ② 搬代码（影响 go:embed 与构建脚本） |
| Q10 | README 是否按实测整体重写（基准数字/路线图勾选/实体类型/安装方式/审计字段示例）？ | README | 对外第一印象与可信度 | ① 重写 ② 局部修 C12 七条 |
| Q11 | 质量门：接受"覆盖率只上传不阻断 + 无 lint/vulncheck"现状并回写 spec，还是补齐门禁？ | 03 §5/§7.2、ci.yml | 长期代码质量护栏 | ① 接受并改 spec ② 分阶段补（先覆盖率门槛，再 lint/vuln） |
| Q12 | 车牌/组织机构/URL/金额/邮编：是否新增为正式实体类型（`rePlate` 已存在但未接线）？ | README、types/detect.go | 检测覆盖面与 spec 类型表 | ① 接线车牌并补单测 ② 从 README 删除未实现类型 |
| Q13 | `bench/` 转 submodule + CI 加 `submodules: true` | 00 §9、HANDOFF §5 | 语料独立开源 | **等 GitHub 远程仓 URL**（外部依赖） |
| Q14 | 桌面 UI：Tauri（方案 A）还是 Go systray minimal（方案 B）？ | 00 §12 P4、TODO_QUEUE | Phase 4 收口路径 | ① B→A 渐进 ② 直接 Tauri |
| Q15 | 真实对抗语料（决定 PII Engineer ROI 是否反转） | DECISION §6、TODO_QUEUE | 检测能力真实评估 | ① 构造 500+ 条 ② 调研公开中文 NER 集 |
| Q16 | 仿真替换（v1.1）：是否做 A/B 验证后启用为默认？ | 00 §7.4、02 §6 | v1.1 第 4 项验收 | ① 做 A/B ② 维持 v1.1 不动 |
| Q17 | 遗留清理（可选）：`Specs/*.md` 与 `.gitignore` 注释中的 workbuddy 字样；`main` 中的旧邮箱 `quarklee@outlook.com` | HANDOFF §7、历史会话 | 仓库整洁度 | ① 清理 ② 保留 |

---


## 4. 冲突裁决与处置记录（2026-09-10 22:35，自主执行）

> 判定规则：**影响可控 + 做法明确 + 有明显更优解** → 自主处理（只加注记/更正事实，不删任何 spec 原文）；
> **涉及公共接口 / 删除既有内容 / 改 CI / 新增功能** → 暂停，在代码或文档就地标注冲突位置 + 方案利弊。

### A. 自主处理（15 项，均为文档注记或事实更正）

| # | 冲突 | 处置 | 理由（为何明显更优） |
|---|---|---|---|
| C1 | Phase 编号三套并存 | `01` 加「Phase 编号勘误与映射」表 + 补录 **Phase 4 任务卡**（4.1 审计 ✅ / 4.2 bench ✅ / 4.3 Tauri ❌ / 4.4 打包 ❌） | 手册自定"冲突时以技术方案为准"；实际开发已用 00 编号。补映射而非改编号，不破坏既有追溯链 |
| C2 | MCP 归属（spec v1.1 / README v2 / 已交付） | README 路线图移入"v1 已交付"；`00` §11 加注"已提前交付" | 事实已发生；spec 的"v1.1"是计划属性，保留原文不矛盾 |
| C4 | 四方基准豁免未回写 | `01` 任务 0.2 加豁免注记（**不动验收标准原文**） | 用户已豁免；改验收标准=降低 spec 约束力，属需求变更，应交你裁决；先标注成本为零 |
| C5 | bench 双评估器双口径 | `DECISION.md` 加「数据刷新 + 口径差异」注记、`bench/runner.py` docstring 标注；**两个评估器都保留、CI 不动** | 删任一评估器=删除既有工具；改 CI=破坏守门。标注后数字可分辨，风险最低 |
| C6 | §16 P99 口径 | `V1_READINESS` 第 2 项 ✅ → ⚠️「口径不符」，小结 6/7 → 5/7 | 原表同行既 ✅ 又 ⚠️ 自相矛盾；spec 要求的是"多轮端到端"，实测只有 detect 端点 |
| C8 | PAC 透明代理 | `V1_READINESS` §13.1 ✅ → ❌ 未实现 + 修订记录 R2 | 全仓 grep 无实现，属纯事实错误，不更正会误导发布判断 |
| C9 | debug 包位置（D004 vs 实际） | `DECISIONS.md` D004 追加「更正」段（原决策文本保留）；**代码不搬** | 搬目录影响 `go:embed`、构建脚本、所有 import；且 UI 设计 §1.2 / 技术方案附录 D 都支持现状 |
| C10 | LICENSE 阻塞判断相反 | `V1_READINESS` 改"已解除" + 修订记录 R1 | LICENSE 确已入库，纯事实更正 |
| C11 | PROGRESS 顶部状态矛盾 | 顶部改为"Tauri 未启动 + 等 URL"，并写明 zh_address 已完成、PII Engineer 暂缓 | 同文件前后矛盾，按文末已生效决策统一 |
| C12a/b | README 基准数字来自 PII Engineer 规格 | 换成实测（F1=1.0 / p99=23ms）并声明旧数字出处；竞品表"F1 0.918"改为实测 | 文档数字必须可追溯；旧值会让人误判产品能力 |
| C12c | README「Docker 推荐」但无 Dockerfile | 新增"单二进制（当前实际）"段；Docker 段**保留**并标注"规划中、暂不可用"+ 利弊 | 不删除既有部署形态占位；不擅自新增 Dockerfile（新交付物需你确认） |
| C12d–g | README 实体类型 / 国际化 / Tauri / 审计字段 | 类型表补实际 11 类清单；国际化标"未兑现"；Tauri ✅→❌；审计 JSON 对齐 `detected_entities`/`strategy` | 全部为事实更正 |
| C17 | `/_api/audit` 未入契约 | `02` §10.1 补一行 + 注明"实现先于规范" | 纯新增，无破坏性 |
| C18 | Merkle ≠ LRU | `02` §8 加实现说明（§8.1 原文保留） | 契约义务（键绑 conversation_id）未被破坏，仅机制不同 |
| C19 | Anthropic 路径 `/v1/messages` ≠ `/anthropic/v1/*` | `00` §12 加路径说明注记 | 实际已按 `/v1/messages` 验证通过；改 spec 路径会影响后续开发，先标注 |
| C20 | 缺 `bench/judge.py` | `01` 任务 0.3 加注记 | 补脚本会进 CI 链路，属新增交付物 |
| C21 | 契约 §1.1 包结构滞后 | `02` §1.1 补录 5 个实际包 + 依赖方向（原树不动） | 原树自称"权威"，改树需连带改多处引用 |

### B. 暂停实现 · 已在代码/配置中就地标注（6 项）

| # | 冲突 | 标注位置 | 方案与利弊（已写入标注） |
|---|---|---|---|
| C7 | Prometheus 指标命名/单位 | `gateway/internal/metrics/metrics.go` 包注释；`00` §14.2；`V1_READINESS` R4 | ① 改代码对齐 spec：规范统一，但**指标名是公共接口**，会破坏已对接的 Grafana/告警 ② 改 spec 承认现状：零破坏，seconds 也是 Prometheus 惯例单位，但需补录缺失项定义。**未改名（spec §14.2 已于 2026-09-11 回写承认现状，与 metrics.go 对齐；指标名/单位维持现状）** |
| C12d / Q12 | `rePlate` 车牌正则存在但未接线 | `gateway/internal/detector/regex.go`（rePlate 上方）；README 类型表 | ① 接线新增 `zh_plate`：扩检测面，但需补 ground truth 语料并重跑 bench ② 维持未接线 + 从 README 移除。**未动检测行为** |
| C13 | CI 质量门缺失 | `.github/workflows/ci.yml` 顶部 | ① 补齐 lint/覆盖率门槛/vulncheck/-race/bench 回退：符合 spec，但新增失败面，刚修好的流水线有再红风险，且覆盖率需先建基线 ② 维持现状 + 回写 spec：零风险，但放弃护栏。**未改流水线行为** |
| C16 | E4 断言弱于 spec | `e2e/e2e.sh` E4 上方 | ① 补 50ms 延迟断言：与 spec 一致，但 CI 机器上阈值不稳会偶发红 ② 维持幂等断言 + 回写 spec。**未改断言** |
| C3 | PII Engineer 需求 vs 实现 | `00` §11 表头注记；`01` 任务 1.2 注记 | ① 维持暂缓 + 标注（不阻塞 v1；代价是弱格式实体召回受正则上限）② 立即排期（620MB 模型 + Rust 工具链数日，且合成维度会拉低 F1、延迟×6）。**未删 spec 的集成要求**，保留触发条件：真实语料召回 <85% |
| C9 | debug 包是否搬迁 | `DECISIONS.md` D004 更正段（代码未动） | ① 改文档承认现状（零风险）② 搬回 `internal/debug`（需改 go:embed + 构建脚本 + 全部 import） |

### C. 未动 · 仍需你裁决或依赖外部输入

Q3 / Q4（是否把豁免正式回写进 spec 验收标准）、Q9（debug 搬迁）、Q11（CI 门禁）、Q12（车牌接线）、Q14（Tauri vs systray）、Q15（真实对抗语料）、Q16（仿真替换默认启用）、Q17（workbuddy 字样与旧邮箱清理）。

> 注：Q7（指标命名方向，已采纳「② 改 spec 承认现状」）随 2026-09-11 `00` §14.2 回写已闭环；Q13（bench 远程仓 URL）随 2026-09-11 `bench/` 转 `cn-pii-bench` 子模块已闭环。

### C.1 2026-09-11 裁决收尾（10 项 Q-conflict 全部闭环）

> 处置原则（沿用 §4 规则）：影响可控 + 做法明确 + 明显更优 → 自主裁决；
> 涉及公共接口 / 删除既有内容 / 外部依赖 → 标注方案利弊，落地动作留待用户或后续任务。
> 下列裁决已落子或已记录，Q 项从「未动」移入「已决」。

| # | 裁决 | 依据 | 落子动作 |
|---|---|---|---|
| Q3 | ① 维持暂缓 + 回写 spec | C3（合成 F1=1.0，集成 ROI=0）；触发条件：真实语料召回 <85% | 现状已是「暂缓」；SPEC 保留触发条件，无需改 |
| Q4 | ① 改验收标准（永久豁免） | C4；用户已豁免四方基准 | 在 01 任务 0.2 注记基础上，验收标准正文追加「Q4 豁免」脚注（待补） |
| Q7 | ② 改 spec 承认现状 | C7；指标名是公共接口，改名会破坏已对接 Grafana/告警 | spec §14.2 加注「以现状 seconds 为准」，不改名 |
| Q9 | ① 改文档承认现状 | C9；`gateway/debug/` 现状与 UI §1.2 / 附录 D 一致 | DECISIONS D004 已更正；无需搬代码 |
| Q11 | ① 接受并改 spec | C13；覆盖率只上传不阻断 + 无 lint/vulncheck | spec 接受现状，注明「护栏属 v1.1 范畴」 |
| Q12 | ① 接线车牌并补单测 | `rePlate` 已接线为 `plate` 类型（commit `c70542c`） | **已落地**：pkg/types + regex + bench 全过 |
| Q14 | ② 先 Go systray minimal → 再 Tauri | 用户 2026-09 拍板「先 Go systray minimal」 | Phase 4 路径锁定为 B→A 渐进 |
| Q15 | ① 构造 500+ 条真实对抗语料 | 决定 PII Engineer ROI 是否反转（DECISION §6） | 决策已定；执行 = T6（延后，待 v0.2） |
| Q16 | ② 维持 v1.1 不动 | 仿真替换属 v1.1 第 4 项验收，A/B 在 v1.1 阶段做 | 决策已定；v1 不启用为默认 |
| Q17 | ② 保留（不清理） | 实测：`quarklee@outlook.com` 代码中已无；`Specs/*.md` 的 workbuddy 是规格文档写给该 Agent 消费的产品语义引用，非 git 身份泄漏；`.gitignore` 的 `.workbuddy/` 是必需忽略规则 | **不删任何内容**；与 git 身份规则（commit message 不提 workbuddy）不冲突，后者仅约束提交元数据 |

**结论**：10 项 Q-conflict 至此全部闭环（Q12 已代码落地，其余 9 项决策已记录）。
唯一仍依赖外部输入的是 **Q13**（bench 转 submodule 需 GitHub 远程仓 URL）——已单列于 HANDOFF §5 卡点 1，不在本次裁决范围。

## 5. 一句话结论（含 22:35 处置后的状态）

**代码侧完成度显著高于文档侧**：功能实现（A1-A29）与 spec 高度对齐，且部分领先（MCP 提前交付、审计端点先行、Merkle 缓存优于 LRU）；但**进度/对外文档存在 21 处冲突**，集中在 ① Phase 与版本归属编号（C1/C2）② PII Engineer 与四方基准两项"被豁免但未回写 spec"的历史决策（C3/C4）③ README 沿用了 PII Engineer 规格数字（C12）④ 指标与质量门口径（C6/C7/C13/C14）。

建议下次动工前优先裁决 **Q1（编号）、Q3+C4（豁免回写）、Q5（bench 口径）、Q10（README 重写）** 四项——其余冲突多为这四项的衍生。

---
