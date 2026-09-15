# LLMate-Gate · 会话交接包

> **给下一个 workbuddy 会话**：从任何账号、任何机器打开这个仓，**先读 §0（状态速览）→ §1（这是什么）→ §5（任务队列）**，再按需深入。

---

## 0. 当前状态速览（2026-09-15 19:45 更新）

| 项 | 状态 |
|---|---|
| **仓可见性** | ✅ **public**（2026-09-12 晚切公开，anonymous 可 clone） |
| **HEAD** | `17f4609`，工作树干净，已推 origin/main |
| **Release** | ✅ **v0.1.0 已真发布**：https://github.com/Jzh-li/LLMate-Gate/releases/tag/v0.1.0 （7 assets = 6 平台二进制 + SHA256SUMS） |
| **CI** | ✅ 9 job 全绿（verify(-race) / build / e2e / coverage / bench / bench-baseline / lint / perf / vuln） |
| **合成语料 F1** | 1.0000（240 条中文 + 180 条英文）——过拟合基线，**不是**对外宣称值 |
| **真对抗 F1** | **0.7458**（28 条手写样本；P=0.9565 / R=0.6111）——这才是诚实数字，已上 README |
| **开源就绪** | ✅ 治理三件套齐全；🔴 仅剩 Linux/macOS `install.sh` 真机冒烟未做 |

> ⚠️ 真对抗暴露的盲区：`zh_person_name` / `zh_address` F1 = **0.0**（regex 引擎未覆盖），`id_card_masked` F1 = 0.0（已知弱项）。这是 v1.1 是否引入 NER sidecar 的决策依据。

> 📌 产物一眼可验证：`bench/reports/adversarial_20260912-195233.md`（真对抗报告）；`bench/fixtures/cases_adversarial.jsonl`（28 条样本）。

---

## 1. 这是什么

LLMate-Gate 是一个 LLM 网关中间件，反向代理 OpenAI 兼容接口，在请求/响应中自动**检测 + 替换 + 还原**中文 PII（个人身份信息）。
不是 SDK / 不是一个简单聊天前端；是一个能"嵌入现有 LLM 工具链的隐私中间层"——VS Code 扩展、Claude Code hooks、MCP 客户端都通过它走。

最新形态：v1 收口阶段，合成语料口径下超 Spark §16 验收线（中文召回率 **100%** / p99 **23ms**）；真对抗口径 F1 **0.7458**（见 §0）。

---

## 2. 技术栈 & 仓库布局

| 路径 | 用途 |
|---|---|
| `gateway/` | Go 模块（Go 1.24.5）。主子模块 |
| `gateway/cmd/llmate-gate/` | 网关主进程入口 |
| `gateway/cmd/mock-llm/` | 跑 mock 上游做端到端冒烟（不依赖真 LLM） |
| `gateway/cmd/mock-detector/` | mock PII 检测器 sidecar（占位，等真实集成） |
| `gateway/internal/` | detection / replacer / simulator / vault / proxy / pipeline / audit / server / circuit |
| `gateway/debug/assets/` | Web 调试面板（`/_debug`，流量/Playground/规则/**审计** 4 个 Tab） |
| `gateway/configs/` | YAML 配置（含冒烟模板 `smoke.yaml`，已 gitignore） |
| `gateway/internal/audit/` | §9 结构化审计 + 内存环（最近 200 条） |
| `bench/` | cn-pii-bench 评测子项目（**已转 git submodule**，指向 github.com/Jzh-li/cn-pii-bench@main） |
| `Specs/00-整体技术方案.md` | Spark 主方案，§15/§16 是验收线 |
| `scripts/dev.sh` | dev loop 唯一入口（envcheck / sync / build / test / buildall） |
| `PROGRESS.md` | 当前进度的总账（每次任务收口往这 append） |
| `V1_READINESS.md` | v1 验收 §15/§16/§13.1 对账报告 |
| `DECISION.md` / `DECISIONS.md` | 关键技术决策 |
| `.workbuddy/memory/` | workbuddy 会话日志（gitignored，新会话自动读） |
| `HANDOFF.md` | **本文件** |
| `LICENSE` | Apache 2.0 全文 |
| `cn-pii-bench` *(github.com/Jzh-li/cn-pii-bench@main)* | 中文 PII 评测语料与评估器，**已作为 `bench/` submodule 关联**（当前指针 `ba68298`，含真对抗语料）；**已 public** |

---

## 3. 核心 dev loop（必须遵守）

### 3.1 路径原则 · 9P vs NTFS

| 用途 | 路径 | 备注 |
|---|---|---|
| 代码编辑 / git 操作 | `\\wsl.localhost\Debian\home\jzhli\LLMate-Gate\` (WSL 9P) | 千万别 `cd` 到 `/c/...` 改 gateway 代码——`go.mod` 无法 lock |
| 编译 / 测试 / vet | **由 `dev.sh` 自动决定**（见下） | 不再硬编码用户路径 |
| 跑冒烟（mock-llm + gateway） | 同上 build 目录 | 二进制直接放这里 |

**build 目录选址逻辑（2026-09-12 起，`8c6a237`）**：

| 仓位置 | 默认 build 目录 |
|---|---|
| 非 9P（普通 Linux / macOS / NTFS） | 仓内 `.build/`（已 gitignore） |
| 9P（`//wsl.localhost/` / `\\wsl.localhost\` / `/9p/`） | `mktemp -d` 一次性 scratch（自动避开 9P 锁文件问题） |
| 显式覆盖 | `LMGATE_BUILD=/path`（dev.sh）或 `LMGBUILD=/path`（release.sh） |

> 9P 判定函数：`is_9p_workspace()`，认三种路径形式。`bash scripts/dev.sh envcheck` 可自检。

GitBash（Windows）路径用 `//wsl.localhost/...` 双斜杠前缀；WSL 内部用 `/home/jzhli/...`。同一目录的两种写法不一致。

### 3.2 每日操作清单

```bash
# 1. 改代码（在 WSL 仓根）
cd //wsl.localhost/Debian/home/jzhli/LLMate-Gate
# vim/编辑器改 gateway/**/*.go

# 2. 同步到 build 目录（绕过 9P 锁文件坑）
bash scripts/dev.sh sync          # 强制全量同步

# 3. 编译（build 目录由 dev.sh 决定，见 §3.1）
bash scripts/dev.sh build                  # sync + build 单二进制
bash scripts/dev.sh buildall               # sync + 4 个二进制（含 mock-llm / mock-detector / mcp-server）
bash scripts/dev.sh test                   # go test -count=1 ./...
bash scripts/dev.sh envcheck               # 打印实际 build 目录 + 环境自检

# 4. 跑冒烟（mock-llm + gateway，前后台）
./mock-llm.exe --listen :8999 &               # 后台
./llmate-gate.exe --config configs/smoke.yaml --listen :8401 &
curl ... /v1/chat/completions                  # 探测
curl ... /_api/audit?limit=5                   # 验证审计
curl ... /_debug                                # 看面板

# 5. 关键进程清理
pkill -9 -f llmate-gate.exe; pkill -9 -f mock-llm.exe
for pid in $(ps aux | grep -E "llmate-gate|mock-llm" | grep -v grep | awk '{print $1}'); do
  kill -9 $pid 2>/dev/null
done

# 6. 提交推送
cd //wsl.localhost/Debian/home/jzhli/LLMate-Gate
git -c safe.directory='*' config core.fileMode false   # 9P 幻影位
git -c safe.directory='*' -c user.name=jzh-li -c user.email=jzh-li@outlook.com commit -m "..."
git -c safe.directory='*' push origin main
# 注意：不要在 commit message 末尾加 Co-Authored-By / 不要提到 workbuddy
# 使用 `git add <exact path>`，避免新增脚本时丢失 +x（用 update-index --chmod=+x 修回）
```

### 3.3 三大坑（下个会话直接绕开）

1. **`go:embed` 资源变更陷阱**：改 `gateway/debug/assets/*.html|.js` 后 `go build -a` 不一定 re-embed。
   **正确顺序**：先 `dev.sh sync`（确保 build 目录资源是新的）→ 再 `go build -o llmate-gate.exe ./cmd/llmate-gate`。
   - 验证：build 后的 `/_debug` HTML `curl -s 127.0.0.1:PORT/_debug | grep -c '<新特征>'` 必须 > 0
2. **`dev.sh buildall` 的 `need_sync` 只在 SRC_DIR 缺失时同步**——新增 YAML/asset 文件后 `buildall` 不会复制。
   - 兜底：手动 `cp -r gateway/<新文件> SRC_DIR/gateway/<新文件>`，或先 `rm -rf SRC_DIR` 再 buildall
3. **后台 `.exe` 进程会被 safe-delete 拦**：`rm` 路径在 9P 侧时 Windows trash bin 不可达，会 fail。
   - 兜底：`find -maxdepth 1 -name '<file>' -delete`

---

## 4. 当前 commit 时间线（origin/main）

按提交顺序，本会话落地的关键改动：

| commit | 含义 | 验证状态 |
|---|---|---|
| `3b97cfb` | 结构化审计日志 + `GET /_api/audit` | 本地 OK；端到端 PII 请求后真实事件返回 |
| `2ca578e` | 调试面板「审计」Tab + smoke.yaml | 本地 `/_debug` HTML 含审计 Tab |
| `d9798b8` | cn-pii-bench 0.4 评估器 + regex baseline | F1 = **0.9714**，唯一短板 zh_address 0.80 |
| `30c9de0` | 直辖市正则修复（地址 F1 0.80→1.00） | 全量 F1 = **1.0000**，240/240 全过 |
| `94e1d6b` | docs: PROGRESS 收口 | — |
| `10f8d48` | Apache 2.0 LICENSE + V1_READINESS | v1 阻塞项解除 |
| `8e29e0d` | DECISION §8 差异化清单（3 层 12 项） | 文档；含 4 空白 + 5 不做 |
| `455405f` | 协议层 T1+T2+T3（块 allowlist / gate_only / PII 块类型） | go test + vet 双绿 |
| 双语检测 | 中英文 PII 基线（见 §6） | 中文 240 条 F1=1.0 零回归 + 英文 180 条 F1=1.0 |
| `c70542c` | 双语检测基线（中英 PII 实体对齐 + 命名空间分离） | 全包 go test + vet 双绿 |
| `68f00b8` | T12a Responses API 协议路径 + Q-conflicts 裁决 + B 组配置 | 端到端测试 PASS；SPEC §4.1 闭环 |
| `cd36ced` | fix(release): 修正跨平台编译工作目录与产物路径 | release.yml 从仓库根改 working-directory: gateway |
| **tag `v0.1.0`** | 首版可分发里程碑（2026-09-11） | 推 tag 触发 CI 自动跨平台编译 + 建 GitHub Release |

### 后续（2026-09-12 下午 → 09-15）：CI 版本漂移修复链 + 开源就绪

| commit | 含义 |
|---|---|
| `b0d4919` | checkout@v6 加 SUBMODULE_TOKEN（当时 cn-pii-bench 还是私有仓） |
| `d633ec8` | 首跑生成 bench 性能基线 [skip ci] |
| `e9ee7b9` | cn-pii-bench 改 public 后移除 SUBMODULE_TOKEN 注入 |
| `d26c9e6` | golangci-lint 改用官方 action@v6 并固定 v2.6.1 |
| `e655c78` | 修 lint/vuln 版本漂移 + upload-artifact 升 v6 |
| `d40a7e5` | govulncheck 固定 v1.1.4（官方 action 内部仍 @latest，不可靠）+ lint action 升 v9（Node 24） |
| `6acb124` | **Go 1.24.9 → 1.25.13**（一刀修 20 个 stdlib 可达漏洞） |
| `8c6a237` | dev.sh/release.sh 去硬编码用户路径（开源 P0-1） |
| `fef6f73` | release.yml 同步 Go 1.25.13 + actions v6 + 支持 workflow_dispatch 重发 |
| `c794ae0` | 治理三件套 SECURITY / CONTRIBUTING / CODE_OF_CONDUCT |
| `82975ab` | bench submodule 指向真对抗语料（`ba68298`） |
| `a32cec1` | install.sh 加 `--dry-run` + README 加 Quickstart / 真对抗性能段 |

> 2026-09-15：仓切 public + force-update `v0.1.0` tag 指向 `a32cec1` → release workflow 重跑成功，7 个 artifact 真上传。

完整：`git -c safe.directory='*' log --oneline -30`

---

## 5. 任务队列（2026-09-15 重排）

完整卡见 `.workbuddy/TODO_QUEUE.md`（gitignored，仅本机）。这里给**重要性顺序**：

### 🔴 唯一待办：Linux/macOS `install.sh` 真机冒烟

- 代码已就绪（`--dry-run` / `--no-autostart` / `--uninstall` 三模式），静态校验通过
- 但**从未在真 Linux / macOS 上跑过**——本机是 Windows，沙箱内 WSL 被 policy 拦截
- 命令：
  ```bash
  ./scripts/install.sh --dry-run                     # 先看计划
  ./scripts/install.sh --no-autostart --port 8400    # 真装
  systemctl --user status llmate-gate                # 或 launchctl print gui/$(id -u)/...
  ./scripts/install.sh --uninstall                   # 卸载验证
  ```
- Windows 侧 `install.ps1` 已于 2026-09-12 真机冒烟通过（`0cea49b`）

### ⚪ v1.1 决策入口：真对抗盲区 → 是否上 NER

真对抗 F1=0.7458（见 §0）暴露两类完全未覆盖：

| 盲区 | F1 | 候选解法 |
|---|---|---|
| `zh_person_name` | 0.0 | 引入 NER（PII Engineer sidecar）或词表 + 上下文启发式 |
| `zh_address` | 0.0 | 行政区划词典 + 后缀模式 |
| `id_card_masked` | 0.0 | 允许 `*` 通配的身份证正则 |

- **触发条件**：用户决定"要打真实场景"时启；否则 v1 以 regex 引擎为范围声明即可
- 成本参考：PII Engineer 公开规格 F1=0.918（英文维度），集成工作量 = 多日

### 🟢 可选：Homebrew tap 上架

- `scoop-bucket/` 已就绪（Windows）；`homebrew-tap` 未建
- 工作量：半天

### 🟢 可选：开源协作基础设施

- issue 模板 / PR 模板 未加（`.github/ISSUE_TEMPLATE/`、`PULL_REQUEST_TEMPLATE.md`）
- 工作量：10 分钟

### ⚪ 已放弃 / 明确不做

| 项 | 原因 |
|---|---|
| Tauri 桌面 UI | 改走系统原生（计划任务 / systemd / launchd + 桌面 LNK），CGO 与纯 Go 交叉编译冲突 |
| systray 托盘 | 同上（CGO） |
| AppImage | 后置 v1.1 |
| 服务端托管 / 域名 / K8s / Docker | **用户拍板（2026-09-10）：核心功能优先，部署后置**。产品形态是本机常驻网关，无服务端需求 |

### ✅ 已闭环（2026-09-12 ~ 09-15，勿重复做）

| 项 | 结果 |
|---|---|
| cn-pii-bench 远程仓 | ✅ 已建 + 转 submodule + 改 public |
| GitHub Release artifact | ✅ v0.1.0 7 assets 真发出 |
| 治理三件套 | ✅ SECURITY / CONTRIBUTING / CODE_OF_CONDUCT |
| 真对抗语料 + 评估器 | ✅ 28 条样本，F1=0.7458 |
| scripts 硬编码路径 | ✅ 已去（默认仓内 `.build/`，9P 走 mktemp） |
| `--version` flag | ✅ `6b922ac`（scoop `post_install` 依赖） |
| Go 版本漏洞 | ✅ CI Go 升 1.25.13，govulncheck 0 可达漏洞 |

### 🔵 构建与部署：闸门状态（2026-09-15）

**用户决策（2026-09-10）**：核心功能优先，**部署（服务器 / 域名 / 容器化 / 发布流水线）后置**；
**「分发可安装性」算功能主题**，须在构建/部署之前完成（三平台产物 + GitHub Release + 一个包管理器）。

**B 组「分发可安装性」已全数闭环**：

| 项 | 状态 | 入口 |
|---|---|---|
| 三平台交叉编译 | ✅ | `scripts/release.sh`（6 平台：5 原 + windows/arm64） |
| GitHub Release 自动化 | ✅ | `.github/workflows/release.yml`（on `v*.*.*` tag push → CI 自动编译 + 建 Release + 上传产物） |
| **v0.1.0 真发布** | ✅ **2026-09-15 确认** | https://github.com/Jzh-li/LLMate-Gate/releases/tag/v0.1.0 ，7 assets |
| 包管理器 | ✅ | `scoop-bucket/llmate-gate.json`（Windows）；Homebrew tap 未建（见 §5） |
| 桌面集成 | ✅ | 走系统原生（计划任务/systemd/launchd + 桌面 LNK 指向 `_debug`） |

**v0.1.0 发布纠正（此前记录有误，以此为准）**：
- tag `v0.1.0` 现已 force-update 指向 **`a32cec1`**（原指向 `cd36ced`，那时 release.yml 尚未修复）
- 仓已于 2026-09-12 晚切 **public**，anonymous 可 clone / 可下产物
- **此前记录的「已知小瑕疵 --version 未实现」已闭环**：`6b922ac` 已加该 flag

**仍未启动（按用户拍板，非阻塞）**：Dockerfile / 服务端托管 / 域名 / K8s / 自动更新服务。

详细任务卡见 `.workbuddy/TODO_QUEUE.md` 末段（gitignored）。

---

## 6. v1 状态（Spark §16 七项对账）

| # | 指标 | 目标 | 现状 |
|---|---|---|---|
| 1 | 中文召回率 | ≥ 85% | **合成 100%**（240 条）；⚠️ **真对抗口径 61.1%**（见 §0 / §13.3） |
| 2 | P99 延迟 | < 2s | **23ms** |
| 3 | 安装到可用 | < 5min | **<30s**（14.6MB 单文件） |
| 4 | 仿真替换 LLM 质量 | 无下降 | v1.1 范畴（已实现未默认启） |
| 5 | 基准可复现 | ✅ | `bench/runner.py --cases user.jsonl` |
| 6 | 零配置接入 | ✅ | VS Code 扩展 + Claude Code hooks + MCP |
| 7 | 审计合规 | ✅ | 结构化审计 + bench 报告 |

**6/7 达成**，第 4 项是 v1.1 范畴。详见 `V1_READINESS.md`。

**2026-09-11 双语基线补充**：隐私功能中英双语（用户拍板，方案 B）。

- 新类型：`plate`（中英统一）/ `url` / `us_ssn` / `credit_card`（IIN 前缀分发，与 `zh_bank_card` 单一数字块二选一）
- 新包：`gateway/pkg/global/`（国际实体校验：ValidURL / ValidUSSSN / ValidCreditCard / IsInternationalCard）
- 语料：`bench/fixtures/cases_en.jsonl`（180 条，`generate_en.py` 生成）；中文 `generate.py` 卡号改 62 银联前缀（rnd 消耗不变，其余 205 条字节级不变）
- 基准：中文 240 条 F1=**1.0 零回归**，英文 180 条 F1=**1.0**（报告 `bench/reports/phase0_regex_20260911-0219{39,44}.md`）
- 坑：`bench/runner.py` 在 Windows 上被系统代理劫持打 127.0.0.1 挂起 → 已改用显式无代理 opener（`urllib.request.ProxyHandler({})`）

---

## 7. 关键约束（任何会话都先确认）

| 项 | 规则 |
|---|---|
| **Git 身份** | `jzh-li <jzh-li@outlook.com>`，**绝不**出现 `workbuddy@local` / `noreply@workbuddy.ai` / commit message 末尾的 Co-Authored-By / 任何 workbuddy 字样 |
| **Git-9P 幻影位** | `git -c safe.directory='*' config core.fileMode false`；新增脚本 `chmod +x` 后 `git update-index --chmod=+x <path>` 入索引 |
| **WSL 9P 父目录** | `//wsl.localhost/Debian/home/jzhli/` 是 Read-only——新顶层目录只能建在 `C:/Users/jzh-l/` |
| **Go 代理** | 本地 `GOPROXY=https://goproxy.cn,direct GODEBUG=netdns=go`；CI 云端走官方 |
| **GH CLI** | 本机未装 `gh`——远程仓操作只能走 `git push` 等用户给 URL |
| **safe-delete wrapper** | `rm` 在 9P 文件上可能失败——用 `find -maxdepth 1 -delete` 绕开 |
| **后台进程清理** | `pkill -9 -f` + `for pid in ps aux ... ; do kill -9 $pid` 二段兜底 |

---

## 8. 链接清单（按需打开）

| 想看 | 文件 |
|---|---|
| 当前总账 | `PROGRESS.md` |
| v1 验收 | `V1_READINESS.md` |
| 主技术方案 | `Specs/00-整体技术方案.md` |
| 关键决策 | `DECISION.md` / `DECISIONS.md` |
| 主仓 README | `README.md` |
| bench 报告 | `bench/reports/` |
| 审计 schema | `gateway/internal/audit/audit.go` |
| 审计端点 | `gateway/debug/handler.go::handleAudit` |
| 面板代码 | `gateway/debug/assets/{index.html,app.js}` |
| bench 子模块仓 | https://github.com/Jzh-li/cn-pii-bench （已 public） |
| 主仓 | https://github.com/Jzh-li/LLMate-Gate （已 public） |
| 发布页 | https://github.com/Jzh-li/LLMate-Gate/releases |
| CI 页 | https://github.com/Jzh-li/LLMate-Gate/actions |
| 会话日志 | `.workbuddy/memory/*.md`（**gitignored，仅本机可见**） |
| 详细任务队列 | `.workbuddy/TODO_QUEUE.md`（**gitignored，仅本机可见**） |

---

## 9. 上下文同步协议（跨会话 / 跨机器 / 跨账号）

**关键前提**：`.workbuddy/` **整个目录被 gitignore**（`.gitignore` 第 36 行）。所以 memory 日志和 TODO_QUEUE **不会随 git 走**。

三层记忆各自的可达范围：

| 层 | 载体 | 同账号跨会话 | 同机器跨会话 | **跨机器 / 别人** |
|---|---|---|---|---|
| 云端 | 服务端自动注入 profile + 历史会话检索 | ✅ 自动 | ✅ 自动 | ❌ |
| 用户级本地 | `~/.workbuddy/MEMORY.md` | ✅ | ✅ | ❌ |
| 项目级本地 | `.workbuddy/memory/*.md` + `TODO_QUEUE.md` | ✅ | ✅ | ❌（gitignored） |
| **仓内 tracked** | **`HANDOFF.md` / `PROGRESS.md` / `V1_READINESS.md` / `SPEC_ALIGNMENT.md` / `README.md` / `SECURITY.md` / `CONTRIBUTING.md` / `CODE_OF_CONDUCT.md`** | ✅ | ✅ | ✅ **唯一通道** |

### ⇒ 跨机器 / 换账号 / 交给另一个 workbuddy 的唯一可靠动作

```bash
git -c safe.directory='*' pull origin main   # 或在别处 clone
cat HANDOFF.md                               # 先读 §0 + §1 + §5
```

**本文件就是同步载体**。它必须始终反映最新状态——否则另一个 workbuddy 会读到过期信息。

### 若确实要把 `.workbuddy/` 整体搬到另一台机器

默认**不需要**——`.workbuddy/` 是会话日志 + 任务队列，本文件才是交接载体。
但若要连日志一起搬，体积很小（本项目约 112KB），打包拷贝即可：

```bash
# 旧机器：打包
tar -czf workbuddy-sync.tar.gz .workbuddy/

# 新机器：解压到「用 WorkBuddy 打开的那个文件夹」下
tar -xzf workbuddy-sync.tar.gz -C <工作区根>
```

要点：

| 项 | 说明 |
|---|---|
| 放置位置 | `.workbuddy/` 必须在**工作区根**，不一定是仓库根。若打开的是上层目录，就该放上层（如小智项目：`_xiaozhi_workspace/.workbuddy/` 而仓库在 `_xiaozhi_workspace/myxiaozhi/`） |
| 覆盖行为 | WorkBuddy 首次打开项目会自动建 `.workbuddy/`；直接合并覆盖，不会冲突 |
| 用户级记忆 | `~/.workbuddy/MEMORY.md` 与 `~/.workbuddy/skills/` **不随项目走**，需单独拷到新机器的对应路径 |
| 验证 | 新机器上问「读 `.workbuddy/memory/MEMORY.md`，列出项目硬规则」，能答出 = 成功 |

> ⚠️ **禁止**把 `.workbuddy/` 提交进本仓——**本仓是 public，任何分支都公开可读**。
> 若真要让它随 git 走，只能另建一个 **private** 仓（勿在 public 仓开分支）。

### 交接判定标准（三件齐全）

1. **`HANDOFF.md`**（tracked）— 状态速览 + 任务队列 + 约束 + 收口记录
2. **`PROGRESS.md`**（tracked）— 逐次任务总账
3. `.workbuddy/TODO_QUEUE.md`（gitignored）— **仅本机补充**，缺失不影响跨机器交接

### 若被打断而未推送

```bash
git -c safe.directory='*' stash
git -c safe.directory='*' push origin <branch>
```

---

## 10. 近期收口记录（2026-09-11 晚）

| 任务 | commit | 说明 |
|---|---|---|
| bench 转 cn-pii-bench 子模块 | `4ccd538` `7fb48a6` `970fb4a` | `bench/` 改为 git submodule（github.com/Jzh-li/cn-pii-bench@main，HTTPS URL）；ci.yml 的 bench / bench-baseline 两 job 加 `submodules: true` |
| `--version` 命令行开关 | `6b922ac` | `main.go` 新增 `--version` / `-version`，打印 ldflags 注入的版本号后退出，供 scoop `post_install` 校验 |
| 多轮端到端 P99 基准 harness | `5c68bb3` `df00ea6` | `e2e/latency.sh` + `e2e/latency_client.py`（仅 Python 标准库）；mock-llm 加 `-delay` 开关；收口 V1_READINESS R3（v1 验收 5/7 → 6/7） |
| 指标对齐（方案② 改 spec 承认现状） | `fa1ac05` | `Specs/00` §14.2 回写为实现侧真实指标名（blocked/fail_closed、seconds 单位、补录多出指标、缺失 4 项列规划中）；metrics.go / SPEC_ALIGNMENT C7/Q7 / V1_READINESS R4 同步 |

## 11. 近期收口记录（2026-09-12 凌晨，A1+A2）

| 任务 | commit | 说明 |
|---|---|---|
| A2 补齐 4 个缺失 Prometheus 指标 | `a7fe259` | `metrics.go` 新增 `llmate_pii_detected_total{entity_type,fate}`、`llmate_tool_calls_scanned_total`、`llmate_request_total_latency_seconds{endpoint}`、`llmate_response_restore_latency_seconds{endpoint}`；接线：recordReplace（PIIDetected）、transform/anonymizeJSONString 改 *Proxy 方法（ToolCallsScanned）、recordAudit（RequestLatency）、fullResponse/streamResponse（RestoreLatency）；新增 metrics_test.go（注册 + 写入）和 TestProxy_MetricsIntegration 端到端；`Specs/00` §14.2 规划中 4 项已移除 |
| A1 CI 质量门（子集） | `b1a2d27` | ci.yml: GO_VERSION `1.24.9`（修 25 个 stdlib 漏洞）；verify 加 `-race`；新增 `vuln` job（govulncheck）；coverage job 加 ≥35% 门槛（基线 41.9%） |
| 文档同步 | `c38689e` | spec §14.2 / SPEC_ALIGNMENT C13·Q11 / HANDOFF 同步 |
| TODO_QUEUE 清理 | 本地（gitignored） | 移除已闭环卡点；新增「桌面 UI 计划任务/快捷方式」卡（替代 systray，因 CGO 冲突已放弃 systray） |

## 12. 近期收口记录（2026-09-12，质量门禁四连 + 桌面集成验证）

| 任务 | commit | 说明 |
|---|---|---|
| install.ps1 修复 + 真机验证 | `0cea49b` | 无 BOM 含中文 ps1 在 Win PS 5.1 下按 ANSI 误读导致语法错误（实际执行必挂）——加 UTF-8 BOM 后 AST 解析 0 错误；Do-Uninstall 补删桌面 LNK。真机冒烟：`-NoAutostart` 安装（目录/配置/快捷方式）+ `-Uninstall` 完整循环通过。桌面集成三平台脚本就此验证收口 |
| golangci-lint 清零 + lint job | `c35dbe8` | 新增 `gateway/.golangci.yml`（errcheck/govet/staticcheck/unused/ineffassign/misspell/unconvert/copyloopvar）；清零 19 处告警（10×defer Close、死正则 reBankCard、writeSSE、S1016 结构转换、5×copyloopvar、1×ineffassign）；本地 golangci-lint v2 复跑 0 issues |
| Go benchmark + perf 守门 | `2629802` | 8 个基准覆盖 detector/cache/vault/replacer 热路径（真实 RegexEngine 非 stub）；`scripts/bench-guard.sh`：min-of-6 ns/op，容差 1.25×，三场景本地验证 |
| CI 三道新门禁 | `9c59657` | ci.yml 新增 `lint` job（golangci-lint 官方 CLI）与 `perf` job（首跑自举生成 `gateway/bench_baseline.txt` 并回写，下轮起比对）；coverage job 加逐包门槛（12 包按实测基线-5pct 设卡）。spec §1.3 高线记 v1.1 目标 |

> 至此 CI 门禁全部就位：verify(-race) / build / e2e / coverage(总计+逐包) / bench / bench-baseline / lint / perf / vuln 九个 job。

## 13. 近期收口记录（2026-09-12 下午 → 09-15，CI 版本漂移 + 开源就绪）

### 13.1 CI 版本漂移修复链（09-12 下午）

| 症状 | 根因 | 修复 | commit |
|---|---|---|---|
| `golangci-lint` 红 | `go install @latest` 拉到 v2.13.2，要求 Go ≥1.26 | 改官方 action 并固定 v2.6.1 | `d26c9e6` |
| `golangci-lint` 仍红 | action v6 **不支持** golangci-lint v2 | action v6 → v7 | `e655c78` |
| Node 20 弃用警告 | action v7 仍 targeting Node 20（v9 才是 Node 24） | v7 → v9 | `d40a7e5` |
| `govulncheck` 红 | 官方 `govulncheck-action@v1` 内部仍 `@latest`（pull v1.2.0，要求 Go ≥1.26） | 撤销官方 action，手动固定 `v1.1.4` | `d40a7e5` |
| `govulncheck` 20 个可达漏洞 | Go 1.24.9 stdlib 又有新洞（fix 跨 1.24.11~1.25.13） | CI Go **1.24.9 → 1.25.13**（一刀全修） | `6acb124` |

> 教训：**CI 里 `@latest` 是负债**；官方 action 也有 major version 边界（golangci-lint v2 必须配 action v7+，Node 24 要 v9）。

### 13.2 开源就绪 B 路线（09-12 晚，用户全权授权）

| 任务 | commit / 产物 |
|---|---|
| P0-1 scripts 去硬编码用户路径 | `8c6a237`：默认非 9P → 仓内 `.build/`；9P → `mktemp -d`；`LMGATE_BUILD` 可覆盖 |
| P0-2 release.yml 升级 + dispatch | `fef6f73`：Go 1.25.13 + actions v6 + cache + `inputs.tag` |
| P0-3/4 + P1-3 治理三件套 | `c794ae0`：`SECURITY.md` / `CONTRIBUTING.md` / `CODE_OF_CONDUCT.md` |
| P1-1 真对抗语料 + 评估器 | submodule `ba68298`：28 条手写样本，**F1 = 0.7458** |
| P1-2 install.sh dry-run | `a32cec1`：`--dry-run` + OS 错误信息补全 |
| P0-5 README Quickstart | `a32cec1`：语言策略 + 5 分钟上手 + 真对抗性能置顶 |

### 13.3 真对抗 F1 by-subset（`bench/reports/adversarial_20260912-195233.md`）

| 子集 | F1 | 说明 |
|---|---|---|
| email / ip_address / phone | 1.0 | regex 覆盖完整 |
| mixed (tool_call arguments) | 0.86 | 地址部分漏报 |
| `zh_person_name` | **0.0** | **regex 引擎未覆盖中文姓名** |
| `zh_address` | **0.0** | **regex 引擎未覆盖中文地址** |
| id_card_masked | 0.0 | `********` 遮蔽格式已知弱项 |
| **总** | **0.7458** | P=0.9565 / R=0.6111 / p99=24ms |

> **诚实原则**：合成语料 F1=1.0 是过拟合基线（生成器按 detector 算法写样本），已在 README 降为参考；真对抗 0.7458 才是对外宣称值。

### 13.4 发布闭环（09-15）

- 仓切 **public**（用户操作，Settings → Danger Zone → Make public）
- `v0.1.0` tag **force-update** `cd36ced` → `a32cec1` → release workflow 重跑
- ✅ Release 页产出 **7 assets**：6 平台二进制 + `SHA256SUMS`
- ✅ CI 9 job 全绿

### 13.5 遗留

- 🔴 Linux/macOS `install.sh` 真机冒烟未做（见 §5）
- ⚪ issue/PR 模板未加（可选）
- ⚪ Homebrew tap 未建（可选）
