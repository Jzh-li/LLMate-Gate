# Contributing to LLMate Gate

感谢你愿意贡献。在动手前请通读本指南，能帮我们双方节省大量时间。

---

## 项目速览

LLMate Gate 是 LLM API 隐私网关：检测请求体 PII → 替换 → 发给上游 LLM → 还原响应。
**Go 1.25+ 单二进制**，5 个子命令 (`llmate-gate` / `mock-llm` / `mock-detector` / `mcp-server` / `bench-runner`)。

- 主仓: https://github.com/Jzh-li/LLMate-Gate
- Issue tracker: GitHub Issues
- 安全问题: 见 [SECURITY.md](./SECURITY.md)（**不要**走 issue 提漏洞）

---

## 开发环境

### 必备

- Go **1.25.x**（CI 锁 1.25.13；本机可略高）
- Git
- `bash`（用于 `scripts/dev.sh` / `scripts/release.sh`）
- `curl`（健康检查 + 模块代理探测）

### 一次性准备

```bash
git clone https://github.com/Jzh-li/LLMate-Gate.git
cd LLMate-Gate
bash scripts/dev.sh envcheck    # 验证 go/git/模块代理
bash scripts/dev.sh buildall    # 编译 4 个二进制
```

`scripts/dev.sh` 自动处理 scratch 目录：
- 普通 Linux/macOS → 仓内 `.build/`（已 gitignore）
- WSL 9P 路径 → `mktemp -d` 一次性 scratch
- 用户可通过 `LMGATE_BUILD=/path` 强制覆盖

### 跑测试

```bash
bash scripts/dev.sh test                 # 含 -race + 覆盖率
bash scripts/dev.sh test ./internal/vault   # 单包测试
bash scripts/e2e/e2e.sh                 # 端到端契约（E1-E8）
bash scripts/bench-guard.sh             # 性能回退守门
```

---

## 提 PR 的标准流程

1. **开 issue 先**（除非是 typo / docs）：描述动机 + 方案概览，避免大改动走错方向
2. Fork → 新分支命名 `<scope>/<verb>-<thing>` 例：`detector/add-phone-segment`, `docs/fix-typo`
3. 提交前必须本地通过：
   - `bash scripts/dev.sh test`（含 race + 覆盖率）
   - `golangci-lint run ./...`（CI 锁 8 linter 集）
   - `bash scripts/e2e/e2e.sh`
   - 若改 performance 相关：`bash scripts/bench-guard.sh`
4. PR 描述模板（自动填充）必须填：
   - **Why**: 改这个的动机 / 关联 issue
   - **What**: 改了什么
   - **Risk**: 可能的影响 + 你已做的回滚验证
5. 等 CI 9 job 全绿 + 1 个 reviewer 通过

---

## Commit 规范

我们用 **Conventional Commits** 轻量版：

```
<type>(<scope>): <subject>

<body>

<footer>
```

| type | 用途 |
|---|---|
| `feat` | 新功能 |
| `fix` | 修 bug |
| `refactor` | 内部结构调整，不改行为 |
| `perf` | 性能优化 |
| `test` | 测试增补 |
| `docs` | 文档 |
| `chore` | 构建 / 工具 / 依赖 |
| `ci` | CI 配置 |

**scope** 用包名或模块名（detector / proxy / vault / ci / docs ...）。

**subject** 用祈使句、不超过 60 字、英文句号结尾。**不要**首字母大写（除专有名词）。

示例：

```
feat(detector): add 95xxx phone segment regex

新增中国电信 95xxx 号段。原有 13/14/15/17/18/19 之外扩展。

Closes #123
```

### 提交身份硬规则

- `user.name` / `user.email` 必须与你的 git 历史一致，**不要**伪造他人身份
- 单次提交末尾**不要**附 `Co-Authored-By: ...` trailer
- commit message 正文**不要**提及 AI 助手（workbuddy / claude / gpt / ... 等）

---

## Code Review 标准

我们 review 时关注：

1. **正确性**：边界情况、并发安全、错误传播
2. **测试**：新功能必须有单测；bug 修复必须先复现再写回归测试
3. **可观测性**：关键路径有 Prometheus 指标 / 日志 / 审计
4. **可回滚**：单个 PR 不应该让 main 不可编译
5. **不引入新依赖**：除非必要，优先 stdlib + 现存依赖

我们**不**强求：
- 100% 行覆盖率（看包级别，详见 `.github/workflows/ci.yml`）
- 每个 PR 都有 issue（typo / docs 直接提）

---

## 范围边界

### 我们欢迎

- ✅ 性能优化（任何 ns/op 改进，配 benchmark 数据）
- ✅ 中文 PII 检测覆盖（更细分号段、新格式）
- ✅ 新端点兼容（OpenAI / Anthropic / 其他）
- ✅ 可观测性增强（metrics / tracing）
- ✅ 文档改进、错别字、示例

### 我们会慎重

- ⚠️ 引入新第三方依赖：先在 issue 讨论 ROI
- ⚠️ 重大架构调整：先出 RFC（建议放 `docs/rfcs/`）
- ⚠️ 加密方案更换：必须有密码学专家 review

### 我们**不会**接受

- ❌ 把任何 LLM 调用换成第三方商业 API（保持本机零外呼是核心）
- ❌ 关闭 vault 默认加密
- ❌ 删除 fail-closed 默认行为
- ❌ 在 `cmd/` 下添加新的二进制而无明确用例

---

## 调试技巧

### 启用调试面板

```bash
LLMATE_DEBUG=1 llmate-gate --config configs/dev.yaml
# 访问 http://localhost:8400/_debug
```

### 跑特定端到端用例

```bash
bash scripts/e2e/e2e.sh           # E1-E8 全部
bash scripts/e2e/latency.sh       # 多轮延迟基线
bash scripts/e2e/ui_smoke.sh      # 调试面板 HTML 渲染
```

### 本地 submodule (cn-pii-bench)

```bash
git submodule update --init --recursive
bash scripts/bench-runner.sh      # 跑语料基线
```

---

## License

贡献的代码遵循 [LICENSE](./LICENSE) 文件。
提 PR 即默认你同意按本项目许可证发布你的贡献。

---

## Contact

- Issue: https://github.com/Jzh-li/LLMate-Gate/issues
- Security: 见 [SECURITY.md](./SECURITY.md)
- 一般问题：issue 里开 discussion 标签

Last updated: 2026-09-12