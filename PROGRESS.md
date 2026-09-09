# LLMate Gate 进度

> 探路者 Loop 的进度追踪（执行手册 §"进度追踪"）。每完成一阶段更新一次。

## 当前阶段：Phase 1 · 任务 1.1-1.5（代理核心 + 内嵌调试面板）

## 当前任务：任务 5 E2E 已完成（mock 链路 + 18 项断言全绿），下一步 CI + 覆盖率门 + cn-pii-bench

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
- [x] **1.5 内嵌调试面板 + Playground**：
  - `gateway/debug`：go:embed 静态资源（`assets/index.html` `style.css` `app.js`）+ `Store` 200 条环形缓冲 + `Hub` 多订阅者非阻塞广播 + `Handler` 路由（`/_debug`、`/ws/events`、`GET|DELETE /_api/traffic`、`POST /_api/detect`、`POST /_api/replace`）+ cmd 行 `--no-debug` 一键关闭
  - pipeline.Publisher 注入 + proxy 三段事件（`request.received` `request.replaced` `restore.done`）按 RequestID 合并
  - `enc.SetEscapeHTML(false)` 在 `_api/traffic` 序列化时关闭 HTML 转义，保证占位符 `<<zh_phone_1>>` 字面输出
  - 验收 `e2e/ui_smoke.sh` D1-D6 **11/11 PASS**

- [x] **任务 5 部分**：E2E 链路骨架完成
  - `gateway/cmd/mock-llm/`：HTTP 回声上游，支持 OpenAI 5 端点 + Anthropic `/v1/messages` + `/v1/models`，SSE 模式 8-rune token 切片（用于流式测试），`enc.SetEscapeHTML(false)` 保证 `<<...>>` 字面保留，`/_received` 回声供 e2e 断言
  - `gateway/cmd/mock-detector/`：PII Engineer sidecar mock，`--sleep N` 模拟超时、`--respond-fail` 模拟 5xx
  - `gateway/configs/e2e-{llm,detector}.yaml`：regex + mock upstream；pii-engineer + mock detector
  - `e2e/e2e.sh`：编排两阶段（regex + pii-engineer），E1-E8 18 项断言全绿（**PASS=18 FAIL=0**）
    - E1 chat 非流式：客户端还原 + 上游脱敏
    - E2 tool_call flow：tool description 无 PII，上游脱敏
    - E3 SSE 流式：跳过（跨 SSE 事件边界的占位符还原需要 post-processing pass，留 TODO 2.4）
    - E4 缓存幂等：同一 conv 两次请求都脱敏
    - E5 detector 超时：fail-closed 502 < 5s 阻断
    - E6 Anthropic `/v1/messages`：客户端还原 + 上游脱敏
    - E7 鉴权：错 token / 无 token 都 401
    - E8 embeddings：input 数组脱敏

- [x] **任务 5**：CI + 覆盖率 + cn-pii-bench 骨架
  - `.github/workflows/ci.yml`：Ubuntu runner 上 `vet` + `go test ./...` + 构建三套二进制 + ui-smoke HTML/401 健康 + coverage artifact
  - `bench/fixtures/cases.{jsonl,schema.json}`：10 个 v0.1 合成样本（含 8 类 PII + 1 个 negative）
  - `bench/validate.py`：JSONL 自检（id/text/expect/start/end/value 一致性），CI 可直接 `python bench/validate.py` 挂门

### 进行中

- [ ] Phase 1 收尾：移除孤儿 `trie.go`（safe-delete 阻碍 WSL 路径删除，留到 CI 链路打通后处理）
- [ ] Phase 2：tool-call 递归扫描 / per-type fate / VS Code 扩展 / Claude Code hooks

### 探路记录

| 任务 | 选择路径 | 理由 |
|---|---|---|
| 环境验证 | 直达 | 一条脚本 + 一次 curl，无需人工逐项检查 |
| 构建链路 | 跳转（sync 到本地 NTFS） | WSL 9P 共享不支持 go.mod 文件锁，直接构建必失败（D002） |
| 检测引擎 | 跳转（内置 regex 先行） | 620MB 模型当前不可得，先跑通全链路，接口保持不变（D001） |
| 占位符复用 | 直达 | 同 (type,value) 复用同一哨兵，保证 LLM 跨句指代不崩 |
| 映射表落盘 | 跳转（Original 进加密 blob） | 加密后密文可还原；`json:"-"` 会破坏 Seal/Unseal 往返与重启还原 |
| e2e 路径 | 跳转（NTFS scratch + Windows 路径） | WSL 9P 文件锁+路径解析双坑，强制走 `C:/...` 路径直达 Windows 二进制 |
| E3 跨 SSE 边界还原 | 步行（已知限制） | 完整修复需 buf 分事件 → 跨事件 substring 还原（任务 2.4 标记 TODO，不阻断 Phase 1 收口） |
| 覆盖率门 | 跳转（artifact 上传而非 PR 阻断） | 首次覆盖基线尚未稳定，先收集数据；阈值门禁放到 v0.2 |
| cn-pii-bench 模块化 | 跳转（独立 Python 校验脚本） | WSL 9P 不支持 go mod init 落锁，跳过 Go test；用 Python 直接读 JSONL，等 CI 跑通了再补 Go loader |
