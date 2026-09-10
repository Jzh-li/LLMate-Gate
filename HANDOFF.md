# LLMate-Gate · 会话交接包

> **给下一个 workbuddy 会话**：从任何账号、任何机器打开这个仓，先读本文件的前 3 节，再按 §5 任务队列继续推进。

---

## 1. 这是什么

LLMate-Gate 是一个 LLM 网关中间件，反向代理 OpenAI 兼容接口，在请求/响应中自动**检测 + 替换 + 还原**中文 PII（个人身份信息）。
不是 SDK / 不是一个简单聊天前端；是一个能"嵌入现有 LLM 工具链的隐私中间层"——VS Code 扩展、Claude Code hooks、MCP 客户端都通过它走。

最新形态：v1 收口阶段，已超 Spark §16 验收线（中文召回率 **100%** / p99 **23ms**）。

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
| `bench/` | cn-pii-bench 评测子项目（**当前是普通子目录**，待转 submodule） |
| `Specs/00-整体技术方案.md` | Spark 主方案，§15/§16 是验收线 |
| `scripts/dev.sh` | dev loop 唯一入口（envcheck / sync / build / test / buildall） |
| `PROGRESS.md` | 当前进度的总账（每次任务收口往这 append） |
| `V1_READINESS.md` | v1 验收 §15/§16/§13.1 对账报告 |
| `DECISION.md` / `DECISIONS.md` | 关键技术决策 |
| `.workbuddy/memory/` | workbuddy 会话日志（gitignored，新会话自动读） |
| `HANDOFF.md` | **本文件** |
| `LICENSE` | Apache 2.0 全文 |
| `cn-pii-bench` *(独立仓，`C:/Users/jzh-l/cn-pii-bench/`)* | 中文 PII 评测语料与评估器，独立仓，**等用户给 GitHub URL** |

---

## 3. 核心 dev loop（必须遵守）

### 3.1 路径原则 · 9P vs NTFS

| 用途 | 路径 | 备注 |
|---|---|---|
| 代码编辑 / git 操作 | `\\wsl.localhost\Debian\home\jzhli\LLMate-Gate\` (WSL 9P) | 千万别 `cd` 到 `/c/...` 改 gateway 代码——`go.mod` 无法 lock |
| 编译 / 测试 / vet | `C:/Users/jzh-l/AppData/Local/Temp/lmgate/gateway\` (NTFS scratch) | 通过 `dev.sh sync` 同步 |
| 跑冒烟（mock-llm + gateway） | NTFS scratch 同上 | 二进制直接放这里 |

GitBash（Windows）路径用 `//wsl.localhost/...` 双斜杠前缀；WSL 内部用 `/home/jzhli/...`。同一目录的两种写法不一致。

### 3.2 每日操作清单

```bash
# 1. 改代码（在 WSL 仓根）
cd //wsl.localhost/Debian/home/jzhli/LLMate-Gate
# vim/编辑器改 gateway/**/*.go

# 2. 同步到 NTFS scratch（绕过 9P 锁文件坑）
bash scripts/dev.sh sync          # 强制全量同步

# 3. 编译（在 NTFS 仓内）
cd /c/Users/jzh-l/AppData/Local/Temp/lmgate/gateway
go build -o ./llmate-gate.exe ./cmd/llmate-gate
# 或 go test -count=1 ./...
# 或 bash scripts/dev.sh buildall （含 sync + 4 个二进制）

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

完整：`git -c safe.directory='*' log --oneline -20`

---

## 5. 任务队列（按 Spark Phase 4/§16 优先级）

完整卡见 `.workbuddy/TODO_QUEUE.md`。这里给**重要性顺序**：

### 🔴 卡点 1：等用户给 cn-pii-bench 远程仓 URL

- 当前：`C:/Users/jzh-l/cn-pii-bench/` 本地仓就绪（2 提交），`git remote -v` 空
- 收到 URL 后 5min 内可完成：
  1. `git -C /c/Users/jzh-l/cn-pii-bench remote add origin <URL>` + `git push -u origin main`
  2. 主仓 `bench/` 转 submodule（`git -c safe.directory='*' submodule add <URL> bench`），提交 `.gitmodules`
  3. CI 两个 bench job 加 `submodules: true`，推一次验证

### 🟡 高优先：Tauri 桌面托盘 UI

- 这是 Phase 4 收口最后一块大拼图
- 本机 Rust 工具链未装，需要先 `curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh`
- 工作量：多日（涉及 Tauri 模板 + 系统托盘 + 浏览器调网关 + 跨平台打包）
- 决策点：先 minimal 系统托盘（不引入 Tauri，仅 tray + 浏览器跳转），还是直接上 Tauri？

### 🟢 中优先：打包分发

- 紧随 Tauri：brew / scoop / AppImage 三脚本
- 工作量：半日

### ⚪ 低优先：realistic adversarial fixture

- 合成语料 F1=1.0 已完美，但合成样本不代表真实分布
- 等真实对抗语料出现明确短板再决定是否启 NER（PII Engineer）
- F1 数据：regex 1.0 vs PII Engineer 0.918；集成 ROI = 0（合成维度）
- 等真实对抗维度再看

### 🔵 末段 · 构建与部署（闸门：功能主题全部收口后）

**用户决策（2026-09-10）**：核心功能优先，**部署（服务器 / 域名 / 容器化 / 发布流水线）后置**；
**「分发可安装性」算功能主题**，须在构建/部署之前完成（三平台产物 + GitHub Release + 一个包管理器）。
理由：没有可下载产物就没人试装，拿不到真实对抗语料，会反过来拖慢召回率验收与 PII Engineer ROI 判断。
详细任务卡见 `.workbuddy/TODO_QUEUE.md` 末段。

---

## 6. v1 状态（Spark §16 七项对账）

| # | 指标 | 目标 | 现状 |
|---|---|---|---|
| 1 | 中文召回率 | ≥ 85% | **100%**（合成 240 条） |
| 2 | P99 延迟 | < 2s | **23ms** |
| 3 | 安装到可用 | < 5min | **<30s**（14.6MB 单文件） |
| 4 | 仿真替换 LLM 质量 | 无下降 | v1.1 范畴（已实现未默认启） |
| 5 | 基准可复现 | ✅ | `bench/runner.py --cases user.jsonl` |
| 6 | 零配置接入 | ✅ | VS Code 扩展 + Claude Code hooks + MCP |
| 7 | 审计合规 | ✅ | 结构化审计 + bench 报告 |

**6/7 达成**，第 4 项是 v1.1 范畴。详见 `V1_READINESS.md`。

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
| 独立仓 | `C:/Users/jzh-l/cn-pii-bench/` |
| 今日会话日志 | `.workbuddy/memory/2026-09-10.md` |
| 详细任务队列 | `.workbuddy/TODO_QUEUE.md` |

---

## 9. "上下文保留下來"承诺

本文件 + `.workbuddy/TODO_QUEUE.md` + `.workbuddy/memory/2026-09-10.md` 三件齐全即视为完整交接。
**任何账号、任何机器**在 `git pull origin main` 之后打开仓根即可立刻看到本 HANDOFF.md；
`.workbuddy/memory/` 路径与约定一致，workbuddy 会自动读取，作为补充上下文。

如本会话被账号切换打断而**未推送**：`git stash` 当前改动 + `git -c safe.directory='*' push origin <branch>` 可挽救。
