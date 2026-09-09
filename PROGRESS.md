# LLMate Gate 进度

> 探路者 Loop 的进度追踪（执行手册 §"进度追踪"）。每完成一阶段更新一次。

## 当前阶段：Phase 1 · 任务 1.1-1.5（代理核心 + 内嵌调试面板）

## 当前任务：1.2 检测引擎客户端 + 共享核心

## 状态：执行中

### 已完成

- [x] **环境验证**（`scripts/dev.sh envcheck`）：Go 1.24.5 / git / goproxy.cn / 远程 `git@github.com:Jzh-li/LLMate-Gate.git`
- [x] **0.0 项目骨架**：`gateway` Go 模块、目录布局按契约 §1.1
- [x] **1.1a `pkg/types`**：Entity / DetectRequest / DetectResponse / MappingTable / MappingEntry / Fate
- [x] **1.1b `internal/errors`**：统一错误码 + HTTP 状态映射
- [x] **1.1c `internal/config`**：YAML + `${ENV}` 展开 + 启动期校验（含单测 7 例）

### 进行中

- [ ] 1.2 检测引擎客户端（regex 引擎 + pii-engineer sidecar + 阈值 + 批量 + 熔断）
- [ ] 1.3 占位符替换 + trie 流式还原 + 加密映射表
- [ ] 1.4 detection_cache（conversation 绑定）+ fail-closed
- [ ] 1.5 内嵌调试面板 + Playground

### 下一步

- Phase 1 验收：`curl localhost:8400/v1/chat/completions` 端到端脱敏 + 流式还原
- Phase 2：tool-call 递归扫描 / per-type fate / VS Code 扩展 / Claude Code hooks
- Phase 4：审计合规导出 + cn-pii-bench

### 探路记录

| 任务 | 选择路径 | 理由 |
|---|---|---|
| 环境验证 | 直达 | 一条脚本 + 一次 curl，无需人工逐项检查 |
| 构建链路 | 跳转（sync 到本地 NTFS） | WSL 9P 共享不支持 go.mod 文件锁，直接构建必失败（D002） |
| 检测引擎 | 跳转（内置 regex 先行） | 620MB 模型当前不可得，先跑通全链路，接口保持不变（D001） |
