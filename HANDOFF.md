# LLMate-Gate · 会话交接包

> **给下一个会话**：本文件是**交接入口**，不是规格。请按 `§0 状态速览 → §3 dev loop → §5 任务队列` 读；需要了解「系统现在到底是什么样」时读 **`Specs/05-实现规格-AS_BUILT.md`**（以代码为准的唯一现状来源）。
>
> **本文件已于 2026-09-15 按代码全量重建，2026-09-17 增量同步**：09-16 的两轮加固（自身暴露面 S1-S6 / 越权读取收口 / 调试面板归控制面）与 cn-pii-bench 语料扩充（28 → 318 条）已并入本文。

---

## 0. 当前状态速览（2026-09-17 09:15 重建）

| 项 | 状态 |
|---|---|
| **HEAD** | `51a9040`，工作树干净，**与 origin/main 同步**（2026-09-17 共推送 8 个提交） |
| **语料副本** | `bench/` 子模块本机为空目录；语料工作副本 = 独立克隆 `/home/jzhli/cn-pii-bench`，`dev` @ `b9533c1`（见 §3.2 第 6 坑） |
| **仓可见性** | ✅ public（anonymous 可 clone） |
| **Release** | ✅ `v0.1.0` 已发布：7 assets（6 平台 + SHA256SUMS） |
| **CI** | ✅ **11 job 全绿 @ `51a9040`**（verify / build / e2e / coverage / bench / bench-gate / bench-baseline / lint / **vscode-ext** / perf / vuln） |
| **Go 版本** | CI `1.25.13`；`gateway/go.mod` 声明 `go 1.24` |
| **合成语料 F1** | 1.0000（中文 240 + 英文 180）—— **过拟合基线，不是对外宣称值** |
| **真对抗 F1** | span **0.6479**（主口径）/ strict **0.5915**（下界）/ 悲观 0.6389（28 条 / 48 GT） |
| **真对抗矩阵 F1** | span **0.5946** / strict 0.5863（318 条 / 338 GT；形态诊断用，**暂不进门禁**） |
| **自身暴露面** | ✅ 三/四面凭据分级 + 越权读取收口（映射表来源隔离 + 统一 404）+ 调试面板归控制面；`e2e/security.sh` S1-S6 共 32 断言已进 CI |
| **本机实测复现** | ✅ 09-15 三语料全跑复现当时记录值；09-16 语料修订后**真对抗已复跑**（span 0.6479），合成 / 英文未复跑 —— 见 `Specs/05` §10 勘误 |
| **已知缺陷** | **0 项待修**（`Specs/06` 摘要表全 ✅ 闭环；P2-14 复核判定为可接受） |

> ⚠️ **诚实口径提示**：README 与对外描述若引用 F1，请用**真对抗的 span `0.6479` / strict `0.5915`**。
> 合成语料 F1=1.0 只证明「检测器与生成器对同一套仿真规则达成一致」，不构成真实场景结论（`Specs/03` §4.3.4）。
>
> ⚠️ **与 09-15 记录的数字不可直接比较。** 本表旧值 `0.7458` 与 `Specs/05` §10 的 `0.7667 / 0.7188`
> 都是**语料修订前**的数字。09-16 的语料修订修掉了「模板内联真 PII 未标注 + 地址标注粒度自相矛盾」，
> GT 从 37 增至 48 —— 数字下降是**度量变准了，不是引擎变差了**。详见 `cn-pii-bench/README.md`「语料修订」节。

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

**仍在挂账的一项**：`1.6 检测器形态容忍` —— 缺口已由矩阵语料定位（见
`cn-pii-bench/README.md`「检测质量线：形态覆盖缺口」节），改动落在核心检测逻辑且会**扩大脱敏范围**，
**待用户拍板后再动**。

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

> ⚠️ **遗留（待拍板）**：语料修订（GT 37 → 48）在 `249b030` 及之后，**不在**本次前移范围内。
> 若把 gitlink 直接前移到 dev HEAD，召回会变为 span 0.4792，低于现有阈值 0.60 ——
> 需要「基线变更 + 阈值同步」一起决策。另：cn-pii-bench 的 `main` 落后 dev 三个提交，
> 是否继续维护待定。
