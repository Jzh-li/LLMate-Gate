# LLMate-Gate · 会话交接包

> **给下一个会话**：本文件是**交接入口**，不是规格。请按 `§0 状态速览 → §3 dev loop → §5 任务队列` 读；需要了解「系统现在到底是什么样」时读 **`Specs/05-实现规格-AS_BUILT.md`**（以代码为准的唯一现状来源）。
>
> **本文件已于 2026-09-15 按代码全量重建**：此前版本停留在 09-12，未覆盖 09-13~09-15 的功能演进，且 dev loop 章节写的是 WSL/Windows 路径，在原生 Linux 上完全不可用。

---

## 0. 当前状态速览（2026-09-15 20:10 重建）

| 项 | 状态 |
|---|---|
| **HEAD** | `c64f246`，工作树干净，与 origin/main 同步 |
| **仓可见性** | ✅ public（anonymous 可 clone） |
| **Release** | ✅ `v0.1.0` 已发布：7 assets（6 平台 + SHA256SUMS） |
| **CI** | ✅ 9 job 全绿（verify(-race) / build / e2e / ui-smoke / coverage / bench / bench-baseline / lint / perf / vuln） |
| **Go 版本** | CI `1.25.13`；`gateway/go.mod` 声明 `go 1.24` |
| **合成语料 F1** | 1.0000（中文 240 + 英文 180）—— **过拟合基线，不是对外宣称值** |
| **真对抗 F1** | **0.7458**（28 条；P=0.9565 / R=0.6111）—— 诚实数字 |
| **本机实测复现** | ✅ 三个语料全部复现记录值（详见 `Specs/05` §10） |
| **已知缺陷** | 5 项（安装脚本 4 项 + 可观测性 1 项），见 `Specs/06` |

> ⚠️ **诚实口径提示**：README 与对外描述若引用 F1，请用 **0.7458**（真对抗）。合成语料 F1=1.0 只证明「检测器与生成器对同一套仿真规则达成一致」，不构成真实场景结论（`Specs/03` §4.3.4）。

---

## 1. 这是什么

LLMate-Gate 是 LLM 网关中间件，反向代理 OpenAI / Anthropic 兼容接口，在请求/响应中自动**检测 + 替换 + 还原**中文 PII。

不是 SDK、不是聊天前端，而是能嵌入现有 LLM 工具链的隐私中间层：VS Code 扩展、Claude Code hooks、MCP 客户端都通过它走。

当前形态：**v1 功能面基本收口**，真对抗口径下暴露的能力边界（人名/地址召回）是 v1.1 的决策入口。

---

## 2. 文档索引（先读哪份）

| 想知道 | 读 |
|---|---|
| **系统现在到底是什么样**（以代码为准） | `Specs/05-实现规格-AS_BUILT.md` |
| 有哪些已知缺陷与优化点 | `Specs/06-代码审阅-优化点清单.md` |
| 为什么这样设计（规划期） | `Specs/00-整体技术方案.md` |
| 接口/字段/错误码契约 | `Specs/02-接口与数据契约规范.md` |
| 测试策略 | `Specs/03-测试规约.md` |
| 面板 UI 约定 | `Specs/04-UI设计.md` |
| 用户可见的用法 | `README.md` |
| 进度总账 | `PROGRESS.md` |
| v1 验收对账 | `V1_READINESS.md` |

---

## 3. 核心 dev loop

### 3.1 环境基座（2026-09-15 实测可用）

本仓**不再假设 WSL + Windows 双环境**。原生 Linux 上可直接在仓内构建。

```bash
# Go 工具链（本机安装在 /home/jzhli/.gotoolchain/go，版本 1.25.13）
export GOROOT=/home/jzhli/.gotoolchain/go
export PATH=$GOROOT/bin:$PATH
export GOPATH=/home/jzhli/.gopath
export GOCACHE=/home/jzhli/.gocache
export GOFLAGS=-mod=mod
export GOPROXY=https://goproxy.cn,direct
export TMPDIR=/home/jzhli/.gotmp          # ← 关键，见下
unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY   # ← 关键，见下

cd gateway
go build -o ../.build/llmate-gate ./cmd/llmate-gate
go test -race ./...
```

### 3.2 四个环境坑（下个会话直接绕开）

| 坑 | 现象 | 绕法 |
|---|---|---|
| **`$HOME` 被指向 `/root`** | `failed to initialize build cache at /root/.cache/go-build: permission denied` | 命令里显式 `export HOME=/home/jzhli`，或设 `GOCACHE` |
| **`/tmp` 是 10MB tmpfs** | `compile: writing output: ... no space left on device`（Go 默认把编译工作目录放 `$TMPDIR`） | `export TMPDIR=/home/jzhli/.gotmp`（`/home` 有 340G） |
| **注入的 `http_proxy`** | `git fetch` 返回 502；Go 拉模块异常 | 已全局修好：`git config --global http.https://github.com/.proxy ""`。命令内仍需 `unset http_proxy ...`（Go/curl 会读环境变量） |
| **代理拦截 127.0.0.1** | `curl` 访问本机端口被劫持，报 `upstream connect failed` | `curl --noproxy '*'`；Python 用显式无代理 opener（`runner.py` 已内置）；**注意**：本机 `127.0.0.1` 直连是通的 |

### 3.3 跑冒烟（本机实测流程）

```bash
cd gateway
# 起网关（ui-smoke 配置：上游指向未监听端口，保证不出网）
.exec ../../.gotoolchain/bin/llmate-gate --config configs/ui-smoke.yaml --listen :8401 &
curl -sS --noproxy '*' http://127.0.0.1:8401/healthz
curl -sS --noproxy '*' -X POST http://127.0.0.1:8401/_api/detect \
  -H 'Content-Type: application/json' -d '{"text":"联系电话13800138000"}'
# → {"entities":[{"type":"zh_phone",...,"confidence":0.95}],"latency_ms":0}
```

> **注意**：用 `run_in_background` 常驻方式启动服务。若在普通 Bash 调用里 `nohup ... &`，进程会随该次调用结束被回收，导致后续命令全部 `Connection refused`（本次排查踩过）。

### 3.4 跑 bench

```bash
cd cn-pii-bench
PY=/home/jzhli/.workbuddy/binaries/python/versions/3.13.12/bin/python3   # 需 3.10+（用了 `X | None` 语法）
$PY runner.py --endpoint http://127.0.0.1:8401/_api/detect \
    --cases fixtures/cases_adversarial.jsonl --engine regex --out ./reports
```

### 3.5 提交与推送

```bash
export HOME=/home/jzhli
git -C /home/jzhli/LLMate-Gate fetch origin          # 推送前必做，见 §9
git -C /home/jzhli/LLMate-Gate commit -m "..."       # 项目级身份已是 jzh-li <jzh-li@outlook.com>
git -C /home/jzhli/LLMate-Gate push origin main
```

**提交身份硬约束**：author/committer 必须是 `jzh-li <jzh-li@outlook.com>`。**绝不**出现 `workbuddy@local` / `noreply@workbuddy.ai` / commit message 末尾的 `Co-Authored-By` / 任何工具署名或 `.workbuddy` 字样（改用「本地数据目录」）。

---

## 4. 仓库布局

| 路径 | 用途 |
|---|---|
| `gateway/` | Go 模块（主子模块） |
| `gateway/cmd/` | 5 个入口：llmate-gate / mock-llm / mock-detector / mcp-server / bench-runner |
| `gateway/internal/` | 16 个包（详见 `Specs/05` §1.2） |
| `gateway/pkg/` | types（实体权威表）/ cn / global |
| `gateway/debug/` | 内嵌面板（Hub + Store + Handler + assets） |
| `gateway/configs/` | 配置示例 + 冒烟模板 |
| `bench/` | cn-pii-bench 子模块（**本机尚未拉取内容**，见 §5） |
| `Specs/` | 00~04 规划文档 + **05 实现规格** + **06 优化点** |
| `hooks/` `vscode-ext/` `scoop-bucket/` `scripts/` | 集成与分发 |
| `.workbuddy/` | 会话日志（**gitignored**，不随 git 走） |

---

## 5. 任务队列（2026-09-15 重建）

### ✅ 本轮已完成：Linux 真机冒烟（此前长期挂账的唯一待办）

在本机（原生 Linux/arm64）完成了完整循环：编译 → 启动 → bench → 安装 → 卸载。结果：

- 三个语料实测复现记录值（真对抗 0.7458 / 中文合成 1.0 / 英文 1.0）
- **发现 5 个从未暴露的缺陷**（此前"从未在真 Linux 上跑过"的直接后果）

### 🔴 待办 1：修复 5 个缺陷（建议按批次）

| 批次 | 内容 | 依据 |
|---|---|---|
| 1 | 安装脚本配置 schema 错误 + 配置解析严格化 | `Specs/06` P0-1 / P0-2 |
| 2 | `install.sh` 卸载不删 binary / dry-run 写盘 / `--port` 假失败 | `Specs/06` P1-3 / P1-4 / P1-5 |
| 3 | 流式孤儿指标未接线 | `Specs/06` P1-6 |
| 4 | 请求体重复读 + 类型表手工同步 + 上游配置歧义 | `Specs/06` P2-7 / P2-8 / P2-9 |

**批次 1 必须同批做**：单独启用严格解析会立刻让现存错误模板启动失败。

### 🟡 待办 2：cn-pii-bench 丰富化

现状：真对抗仅 28 条，**其中含 7 对重复，唯一文本约 15 条**；4 条 `expect: []` 把已知漏检固化成了期望（召回率被系统性抬高）。

改进方向（详见 `Specs/06` B-10 / B-11）：

1. 去重或按唯一文本聚合；
2. 区分 `expect_miss`（已知缺口，单独统计）与 `expect: []`（确实不该有 PII）；
3. 每个 subset 扩到 15-20 条唯一文本；
4. 报告头部同时打印「声明条数 / 唯一文本数」。

另：`bench/` 子模块在本机是**空目录**，需 `git submodule update --init` 后才有内容。

### ⚪ v1.1 决策入口：真对抗盲区 → 是否上 NER

| 盲区 | 真对抗 F1 | 候选解法 |
|---|---|---|
| `zh_person_name` | 0.0 | NER sidecar，或词表 + 上下文启发式 |
| `zh_address` | 0.0 | 行政区划词典 + 后缀模式 |
| `id_card_masked` | 0.0 | 允许 `*` 通配的身份证正则 |

- **触发条件**：用户决定"要打真实场景"时启；否则 v1 以 regex 引擎能力范围为声明即可
- 成本参考：PII Engineer 公开规格 F1=0.918（英文维度），集成工作量 = 多日
- **注意**：这三项都是 `regex.go:14-19` 明确声明的设计边界，不是 bug

### 🟢 可选

- Homebrew tap 上架（`scoop-bucket` 已有 Windows 侧）
- issue / PR 模板（`.github/ISSUE_TEMPLATE/`、`PULL_REQUEST_TEMPLATE.md`）

### ⚪ 明确不做

| 项 | 原因 |
|---|---|
| Tauri 桌面 UI / systray | CGO 与纯 Go 交叉编译冲突，改走系统原生 |
| Dockerfile / 服务端托管 / 域名 / K8s / 自动更新 / 遥测 | 用户拍板（2026-09-10）：核心功能优先，部署后置。产品形态是本机常驻网关 |

---

## 6. v1 状态（Spark §16 七项对账）

| # | 指标 | 目标 | 现状 |
|---|---|---|---|
| 1 | 中文召回率 | ≥ 85% | 合成 100% / **真对抗 61.11%** |
| 2 | P99 延迟 | < 2s | **8ms**（真对抗语料实测） |
| 3 | 安装到可用 | < 5min | <30s（单文件 ~14MB） |
| 4 | 仿真替换 LLM 质量 | 无下降 | 已实现，默认未启用 |
| 5 | 基准可复现 | ✅ | `runner.py --cases <任意 jsonl>` |
| 6 | 零配置接入 | ✅ | VS Code + hooks + MCP |
| 7 | 审计合规 | ✅ | 结构化审计 + bench 报告 |

**关键澄清**：第 1 项在**合成口径**下达标，在**真对抗口径**下不达标。对外表述必须区分二者。

---

## 7. 关键约束（任何会话先确认）

| 项 | 规则 |
|---|---|
| **Git 身份** | `jzh-li <jzh-li@outlook.com>`；无工具署名、无 `Co-Authored-By` |
| **推送前** | 必须 `git fetch origin` 看 `HEAD..origin/main`——可能有另一个会话的提交（曾实测差 16 个 commit 被拒） |
| **隐私边界** | 登记表文件、`vault_data/`、`audit.log`、`.workbuddy/` **一律不进版本库**；任何分支都公开可读 |
| **`.workbuddy/` 位置** | 与「打开哪个文件夹」绑定，不一定是仓库根（本项目恰好在仓库根） |
| **`log_pii`** | 默认 false；除非用户明确要求，不要开启 |
| **安全承诺** | `fail_closed` 默认 true——检测异常即阻断，绝不「检测失败就放行」。改动此行为需用户拍板 |

---

## 8. 链接清单

| 想看 | 文件 |
|---|---|
| **实现现状（权威）** | `Specs/05-实现规格-AS_BUILT.md` |
| **缺陷与优化点** | `Specs/06-代码审阅-优化点清单.md` |
| 主技术方案 | `Specs/00-整体技术方案.md` |
| 接口契约 | `Specs/02-接口与数据契约规范.md` |
| 审计 schema | `gateway/internal/audit/audit.go` |
| 指标定义 | `gateway/internal/metrics/metrics.go` |
| 面板代码 | `gateway/debug/assets/{index.html,app.js}` |
| 真对抗报告 | `cn-pii-bench/reports/adversarial_20260912-195233.md` |
| 独立仓 | `https://github.com/Jzh-li/cn-pii-bench` |

---

## 9. 上下文同步协议（跨会话 / 跨机器 / 跨账号）

### 9.1 三层记忆的可达性

| 层 | 载体 | 跨机器 |
|---|---|---|
| 云端 | 服务端 profile + 会话检索 | ❌ |
| 用户级本地 | `~/.workbuddy/MEMORY.md` | ❌ |
| 项目级本地 | `.workbuddy/memory/*`、`TODO_QUEUE.md` | ❌（gitignored） |
| **仓内 tracked 文件** | `HANDOFF.md` / `Specs/05` 等 | ✅ **唯一通道** |

**结论：`.workbuddy/` 被 `.gitignore` 第 36 行整目录忽略。要让下一个会话看到，只能写进 tracked 文件。**

### 9.2 纪律（真正的瓶颈不在机制，在纪律）

上次的失败模式：功能推进了 12 个 commit，但交接文档没跟上，导致新会话读到的状态是错的。

**因此：每个任务收口时必须更新**：

1. `Specs/05-实现规格-AS_BUILT.md` 的对应章节（§3 配置 / §4 检测 / §7 可观测 / §10 实测数据）
2. 本文件 §0 状态速览（HEAD / 数据 / 缺陷数）
3. 本文件 §5 任务队列（增删条目）

### 9.3 把本地数据目录搬到另一台机器

`.workbuddy/` 不随 git 走。搬迁步骤：

```bash
# 旧机器：打包（位置必须在工作区根，不一定是仓库根）
cd ~ && tar czf wb-sync.tgz -C ~/.workbuddy skills memory MEMORY.md SOUL.md IDENTITY.md USER.md 2>/dev/null

# 新机器：解包到 ~/.workbuddy/
tar xzf wb-sync.tgz -C ~/.workbuddy/
```

⚠️ **禁止把 `.workbuddy/` 提交进 public 仓**——任何分支都公开可读。

### 9.4 会话被打断而未推送

```bash
git stash && git push origin <branch>    # 或先 rebase 到 origin/main 再推
```

---

## 10. 历史收口记录（2026-09-11 ~ 09-12）

| 任务 | commit | 说明 |
|---|---|---|
| bench 转 cn-pii-bench 子模块 | `4ccd538` `7fb48a6` `970fb4a` | `bench/` 改为 submodule；CI bench job 加 `submodules: true` |
| `--version` 命令行开关 | `6b922ac` | 供 scoop `post_install` 校验 |
| 多轮端到端 P99 基准 harness | `5c68bb3` `df00ea6` | `e2e/latency.sh` + `latency_client.py` |
| 指标对齐（改 spec 承认现状） | `fa1ac05` | `Specs/00` §14.2 回写为实现侧真实指标名 |
| A2 补齐 4 个缺失 Prometheus 指标 | `a7fe259` | 新增 pii_detected / tool_calls_scanned / request_latency / restore_latency |
| A1 CI 质量门（子集） | `b1a2d27` | `-race` + govulncheck + 覆盖率 35% |
| install.ps1 BOM 修复 + 真机验证 | `0cea49b` | 无 BOM 含中文 ps1 在 PS 5.1 下按 ANSI 误读导致语法错误；真机安装/卸载循环通过 |
| golangci-lint 清零 + lint job | `c35dbe8` | 8 个 linter，本地 0 issues |
| Go benchmark + perf 守门 | `2629802` | 8 个基准 + `bench-guard.sh`（min-of-6，容差 1.25×） |
| CI 三道新门禁 | `9c59657` | lint / perf / 逐包覆盖率门槛 |

---

## 11. 历史收口记录（2026-09-12 下午 ~ 09-15）

| 任务 | commit | 说明 |
|---|---|---|
| 治理三件套 | `c794ae0` | SECURITY / CONTRIBUTING / CODE_OF_CONDUCT |
| bench 指向真对抗语料 | `82975ab` | submodule bump `c787470` → `ba68298` |
| Go 版本漂移修复链 | `e655c78` `d40a7e5` `6acb124` `fef6f73` | 升 `1.25.13`，修 20 个 stdlib 漏洞 |
| scripts 去硬编码用户路径 | `8c6a237` | 默认仓内 `.build/` |
| install.sh 加 `--dry-run` | `a32cec1` | 与 README Quickstart 同批 |
| **多协议上游路由** | `836936d` | OpenAI / Anthropic 配置驱动 |
| **bypass 透传策略** | `b87bfba` | 不脱敏，整条原样过 |
| **仿真词典** | `95fbb67` | 用户指定「真值 → 仿真值」 |
| **规则页重做** | `fc34009` | 策略开关 + 词典编辑器 |
| **登记表** | `addd75b` `d47d924` `d12c810` `2c4842e` | 精确匹配补召回 + 缓存联动 + 面板维护 |
| **身份卡** | `8ce1966` `83dee5f` `cc0d745` | 跨重启稳定的仿真身份 |
| tool_call JSON 字符串修复 | `07481b0` | 脱敏后仍保持 JSON 字符串形态 |
| 面板强制回环 | `04982a8` | 删除从未生效的 `debug_bind` |
| 类型表收敛 | `0a4e511` | `AllTypes()` / `IsKnownType()` 单一出口 |
| 双语检测基线 | `c70542c` | 中英 PII 实体对齐 + 命名空间分离 |
| Responses API 协议路径 | `68f00b8` | T12a + Q-conflicts 裁决 |
| 真对抗语料 + 评估器 | `82975ab` | 28 条，F1=0.7458 |
| 跨机器搬运方法 | `c64f246` | §9 补搬迁步骤 |

> **注意**：上表中 `836936d` 起的**加粗**条目是 09-13~09-15 新增的功能，此前版本的 HANDOFF 完全未记录——这正是本文件重建的原因。
