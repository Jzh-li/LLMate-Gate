# LLMate Gate 进度

> 探路者 Loop 的进度追踪（执行手册 §"进度追踪"）。每完成一阶段更新一次。

## 当前阶段：Phase 1 · 任务 1.1-1.5（代理核心 + 内嵌调试面板）

## 当前任务：1.6 服务层已完成；下一步 1.5 内嵌调试面板

## 状态：执行中

### 已完成

- [x] **环境验证**（`scripts/dev.sh envcheck`）：Go 1.24.5 / git / goproxy.cn / 远程 `git@github.com:Jzh-li/LLMate-Gate.git`
- [x] **0.0 项目骨架**：`gateway` Go 模块、目录布局按契约 §1.1
- [x] **1.1a `pkg/types`**：Entity / DetectRequest / DetectResponse / MappingTable / MappingEntry / Fate
- [x] **1.1b `internal/errors`**：统一错误码 + HTTP 状态映射
- [x] **1.1c `internal/config`**：YAML + `${ENV}` 展开 + 启动期校验（含单测 7 例）
- [x] **1.2 检测器**：内置中文 regex 引擎（`pkg/cn` 校验位/格式校验）+ PII Engineer sidecar HTTP 客户端（契约 §2）+ 阈值 + 批量 + 熔断器（`internal/circuit`）；单测覆盖
- [x] **1.3 替换与还原**：`internal/replacer` 占位符协议（redact/mask/reversible+simulate）、统一多哨兵流式还原（兼容占位符 `<<...>>` 与仿真值，跨块前缀缓冲、orphan 计量、fail-safe）、同值复用同一哨兵；`internal/simulator` 中文格式保持仿真（身份证校验位、手机号、邮箱域名、IP 类别、复姓长度对齐、少数民族「·」结构）；单测覆盖
- [x] **1.3b 加密映射表**：`internal/vault` AES-256-GCM（`crypto/aes`）+ scrypt 派生密钥，Seal/Unseal 往返、篡改即失败、TTL 过期、Zeroize 清零（契约 §7）；`pkg/types` Original 经加密落盘
- [x] **1.4 detection_cache**：conversation 绑定缓存 + fail-closed（契约 §8.2）；单测覆盖
- [x] **1.6 服务层**：
  - `internal/pipeline.Processor`：检测→替换→vault→还原编排（fail-closed、检测异常映射 §0.3 错误码）
  - `internal/proxy.Proxy`：OpenAI 兼容反向代理（chat/completions、completions、embeddings、responses、messages、models），请求脱敏（含 `arguments` 嵌套 JSON 递归扫描）+ 响应还原（流式 SSE 跨块），`enc.SetEscapeHTML(false)` 防占位符 `<`/`>` 被 JSON 转义
  - `internal/server.Server`：路由 + Bearer 鉴权中间件 + `/healthz` + `/metrics` + panic recover
  - `internal/audit`：JSON Lines 事件日志（5 段对比 + PII 受控），Exporter 支持 `pip`/`gdpr`/`dsl`/`csl`/`等保2.0`
  - `internal/metrics`：Prometheus 收集器（请求/检测延迟/替换/还原/阻断/上游错误/流式 orphan/缓存命中/映射表大小/活跃流）
  - `cmd/llmate-gate`：入口（配置加载→会话密钥派生→vault/detector/circuit/guarded client/replacer/cache/audit/metrics/pipeline/proxy/server 装配 + 信号优雅退出）
  - 集成测试：`TestProxy_ChatCompletions_Restore`（非流式还原）、`TestProxy_ChatCompletions_StreamRestore`（SSE 流式 + 客户端 content 拼接还原）、`TestProxy_Embeddings_Anonymize`（`input` 脱敏）、`TestProxy_FailClosed_Blocks`（`detector_unavailable` → 500 阻断）

### 进行中

- [ ] 1.5 内嵌调试面板 + Playground（go:embed + WebSocket + 环形缓冲）
- [ ] 端到端验收（`curl localhost:8400/v1/chat/completions`） + Phase 1 收尾

### 下一步

- Phase 1 验收：`e2e/e2e.sh` + `ui_smoke.sh`（任务 5）
- Phase 2：tool-call 递归扫描 / per-type fate / VS Code 扩展 / Claude Code hooks
- Phase 4：审计合规导出 + cn-pii-bench

### 探路记录

| 任务 | 选择路径 | 理由 |
|---|---|---|
| 环境验证 | 直达 | 一条脚本 + 一次 curl，无需人工逐项检查 |
| 构建链路 | 跳转（sync 到本地 NTFS） | WSL 9P 共享不支持 go.mod 文件锁，直接构建必失败（D002） |
| 检测引擎 | 跳转（内置 regex 先行） | 620MB 模型当前不可得，先跑通全链路，接口保持不变（D001） |
| 占位符复用 | 直达 | 同 (type,value) 复用同一哨兵，保证 LLM 跨句指代不崩 |
| 映射表落盘 | 跳转（Original 进加密 blob） | 加密后密文可还原；`json:"-"` 会破坏 Seal/Unseal 往返与重启还原 |
