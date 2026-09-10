# DECISIONS.md — LLMate Gate 决策记录

> 探路者模式的产物：每个偏离 Spec 或 Spec 未覆盖的决定都记录在此，含理由与影响。

---

## D001 · 检测引擎默认改为内置正则引擎（`regex`），PII Engineer 降级为可选

**日期**：2026-09-09
**背景**：Spec §10.1 要求集成 PII Engineer（Rust/ONNX，~620MB 模型、~800MB 常驻内存、`cargo build`）。
**探路**：
1. 直达：`cargo build` PII Engineer 并下载 ONNX 模型 → 需要 Rust 工具链 + 620MB 模型下载，当前环境不具备；
2. 跳转：先用内置中文正则引擎（覆盖手机/身份证/银行卡/邮箱/车牌/IP 等**强格式实体**）跑通端到端链路，PII Engineer 作为可替换实现；
3. 步行：阻塞等待模型就绪再开发 → 全链路无法验证，违反"先验证再投入"。

**决策**：🥈 跳转。`detection.engine` 支持 `regex`（默认，零依赖）与 `pii-engineer`（HTTP sidecar），
两者实现同一个 `detector.Client` 接口。待模型可用时改一行配置即可切换，代理层零改动。

**影响**：
- 中文召回率目标（≥85%）在 `regex` 引擎下**只对强格式实体成立**，人名/地址等弱格式实体需要 NER；
- `cn-pii-bench` 在 regex 引擎下的召回率数字不代表 v1 验收基线，切到 pii-engineer 后需重跑。

---

## D002 · 构建必须在本地 NTFS 目录执行（`scripts/dev.sh`）

**日期**：2026-09-09
**背景**：工作区位于 WSL 9P 网络共享 `\\wsl.localhost\Debian\home\jzhli\LLMate-Gate`。
**现象**：`go build`/`go mod tidy` 直接报 `go: RLock \\wsl.localhost\...\go.mod: Incorrect function.` —— Go 需要对 `go.mod` 加文件锁，9P 共享不支持。
**决策**：源码仍以 WSL 工作区为唯一真理源（git 仓库在此），`scripts/dev.sh sync` 把 `gateway/` 复制到本地 NTFS scratch 目录后再 `go build/test`。
**影响**：CI（GitHub Actions，Linux）不受影响；本地开发必须走 `./scripts/dev.sh <cmd>`。

---

## D003 · Go 模块代理使用 goproxy.cn

**日期**：2026-09-09
**现象**：`proxy.golang.org` 走 CONNECT 隧道返回 502，不可达；`goproxy.cn` 返回 200。
**决策**：`scripts/dev.sh` 内固定 `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=sum.golang.google.cn`。
**影响**：仅影响本地构建，不改仓库内容。CI 用默认代理。

---

## D004 · 调试面板放在 `internal/debug/` 而非 `debug/`

**日期**：2026-09-09
**背景**：《接口与数据契约规范.md》§1.1 声明自己是包结构的权威，布局为 `gateway/internal/...`；
《UI设计.md》§1.2 画的是 `gateway/debug/`（与 `internal/` 平级）。
**决策**：以契约规范 §1.1 为准，置于 `gateway/internal/debug/`（含 `assets/`）。
**影响**：无功能差异，仅目录位置。

> ⚠️ **2026-09-10 更正（决策未被执行，现状相反）**
> 实际代码位于 **`gateway/debug/`**（与 `internal/` 平级），与 `UI设计.md` §1.2、`整体技术方案.md` 附录 D 一致；
> 契约 §1.1 的包结构中**根本没有列出 debug 包**，因此本条"以契约 §1.1 为准"的前提并不成立。
> **两方案利弊**：
> ① **改文档承认现状**（`gateway/debug/`）——零风险、与 UI 设计/技术方案一致、不动 `go:embed` 与构建脚本；缺点是与本决策记录原文相反。
> ② **搬回 `gateway/internal/debug/`**——符合本条决策；但需改 `go:embed` 路径、`scripts/dev.sh` 同步规则、所有 import，且会与 UI 设计 §1.2 / 技术方案附录 D 冲突（那两份也要一起改）。
> **当前处置**：未搬动代码（属破坏性重构），保留现状；**待你裁决**（`SPEC_ALIGNMENT.md` Q9）。
