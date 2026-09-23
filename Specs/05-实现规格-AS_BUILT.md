# LLMate Gate 实现规格（AS-BUILT）

> **文档版本**：v1.10（2026-09-23）
> **层级**：L2-AsBuilt（实现现状规格）
> **取证基线**：`origin/main @ ca63991`（v1.0 取证于 `c64f246`，其后历版为 `0a21dfa` → `ca63991`；
> v1.1 增补第 6 批缺陷修复；
> v1.2 增补 §9.1 的 `bench-gate` 守门与子模块锚点约定；
> v1.3 增补 §5.2 的热加载并发约定与 §9.1 的 CI 失败自述；
> v1.4 增补 §9.1 的 job 级联依赖说明；
> v1.5 增补 §9.1 的门禁解耦 —— `needs` 只表达产物依赖；
> v1.6 新增 §15 判断层（Judge）实现，修正 §6.2 `tool_call_scan` 的错误陈述并新增 §6.2.1，
> §7.2 指标 16 → 19；
> v1.7 §15.5 补「阈值表的键 = 后端自报名」不变量（`Specs/06` #29）；§12.2 记本机
> golangci-lint 已就位（此前 lint 类问题只能靠 CI 反馈）；
> v1.8 §12.2 记格式门禁已补（`Specs/06` #30：`formatters` 未声明 ⇒ 格式一致性零门禁）；
> v1.9 §12.1 记 8 个「零读者 / 仅校验」配置键的处置（`Specs/06` #31）+ **修正 §4.1 对
> `fallback_regex` 的错误陈述**（原文描述了并不存在的行为）；
> v1.10 §12.1 记「文档/help 命令级承诺」的修正（`Specs/06` #32，本轮只做文档侧）；
> §12.2 增三条待定 —— 启动期错误不带 cause、usage 承诺的 `-V` 未注册、
> 示例配置测试只硬编码单文件）
> **取证方法**：全量 `git log`（92 commit）+ 逐包读源码 + 本机实际编译运行验证。
> **核心规则**：**本文档以代码为唯一事实来源。** 任何与 `HANDOFF.md` / `Specs/00` 冲突之处，以本文档为准；本文档与代码冲突时，以代码为准并回来更新本文档。
> **不回答的问题**：为什么这样设计（见 `Specs/00`）、原始排期（见 `Specs/01`）。

---

## 0. 为什么需要本文档

既有文档分两类，各自有明确缺口：

| 文档 | 性质 | 缺口 |
|---|---|---|
| `Specs/00~04` | 规划期写的设计/契约 | 描述的是「打算做成什么样」，Phase 划分与部分接口已被实现演进覆盖 |
| `HANDOFF.md` | 会话交接包 | 更新靠人工纪律；上一版停在 09-12，未覆盖 09-13~09-15 的 12 个功能 commit |

结果：**代码已经具备的能力，文档里没有**；**文档里写着的路径，在新机器上不可用**。

本文档用「读代码 + 跑代码」的方式补齐这一层：只记录**代码里实际存在、且经运行验证**的行为。

---

## 1. 系统全貌

### 1.1 进程构成

单仓五进制，全部 Go，无 CGO 依赖（保证纯静态交叉编译）：

| 二进制 | 入口 | 职责 |
|---|---|---|
| `llmate-gate` | `cmd/llmate-gate` | 网关主进程：反向代理 + 脱敏 + 还原 + 调试面板 |
| `mock-llm` | `cmd/mock-llm` | 本地假上游，用于端到端冒烟（`-delay` 可模拟慢上游） |
| `mock-detector` | `cmd/mock-detector` | 假 PII 检测 sidecar，占位（尚未接真实 PII Engineer） |
| `mcp-server` | `cmd/mcp-server` | MCP 薄门面，暴露 3 个工具给 MCP 客户端 |
| `bench-runner` | `cmd/bench-runner` | 跑 `bench/` 语料并输出报告 |

代码规模：非测试 39 文件 / 9113 行；测试 25 文件 / 4308 行（测试:实现 ≈ 0.47）。

### 1.2 包结构

```
gateway/
├─ cmd/                      6 个入口（含 judge-bench）
├─ internal/
│  ├─ config/    配置加载、${ENV} 展开、身份卡展开、启动期校验
│  ├─ registry/  登记表（用户自报 PII 值 → 精确匹配补召回）
│  ├─ detector/  regex 引擎 / pii-engineer sidecar 客户端
│  ├─ cache/     LRU 检测缓存 + Merkle 增量缓存
│  ├─ circuit/   熔断器（GuardedClient 包装检测器）
│  ├─ replacer/  替换与还原：占位符 / 仿真 / 掩码 / 抹除 + SSE 还原
│  ├─ simulator/ 格式保持仿真值生成（8 类实体）+ 身份卡
│  ├─ policy/    per-type 命运（reversible/mask/redact）决策
│  ├─ vault/     映射表加密存储（AES-256-GCM + scrypt，TTL 清扫）
│  ├─ judge/     行为判断层骨架：rules/openai/http 后端、降级链、Mapper、探针、评测（§15）
│  ├─ pipeline/  编排器：串起 检测→替换→存储→还原
│  ├─ proxy/     HTTP 反向代理：多协议上游路由、tool_call 扫描、流式还原、判断层旁挂
│  ├─ server/    HTTP 服务层：路由、鉴权、healthz、metrics
│  ├─ audit/     结构化审计（JSON Lines + 内存环 200 条）
│  ├─ metrics/   19 个 Prometheus 指标
│  └─ errors/    错误码
├─ pkg/
│  ├─ types/     跨包共享类型 + 15 类实体权威表 + 判断层契约（judge.go）
│  ├─ cn/        中文实体校验（身份证校验位、手机号段、Luhn）
│  └─ global/    国际实体校验（URL / US SSN / 信用卡）
├─ debug/        内嵌面板：Hub(WS) + Store(环) + Handler(API) + assets
└─ configs/      示例与冒烟配置
```

依赖方向单向：`cmd → server/proxy → pipeline → {detector, replacer, vault, cache, audit, metrics}`；`registry` 与 `circuit` 以装饰器方式包在 `detector` 外层（`registry.Wrap` → `circuit.NewGuardedClient`），不侵入检测器内部。判断层是**旁挂**：`proxy → judge → pkg/types`，`judge` 不依赖 `config`（配置投影在 `cmd/llmate-gate` 里做）。

### 1.3 一条请求的完整生命周期

```
客户端
  │ POST /v1/chat/completions
  ▼
server.handleLLM            → detectStream() 读出 body 判定 stream
  ▼
proxy.Handle                → 生成 request_id / conversation_id
  │                         → publish(request.received)  ← 面板立即可见
  ▼
proxy.anonymizeBody
  ├─ strategy==bypass ? 原样返回（不建映射表）
  ├─ 非 JSON ? 整段当文本脱敏
  └─ JSON → extractMessageContents → Merkle 增量
              │  命中段：零检测，直接复用实体
              │  未命中：pipeline.DetectText
              ▼
           replacer.Session.Replace   ← 一次请求共享一个 Session
              │  每个字符串值 → 检测 → 按 fate 替换
              │  收集 MappingEntry
              ▼
  publish(replaced)  ← 脱敏后内容立即可见
  ▼
  pipeline.Store(request_id → 映射表)   ← 加密后进 Vault
  ▼
proxy.forward → 选上游（openai/anthropic）→ 改写鉴权头 → 发请求
  ▼
  响应（普通 or SSE）
  ├─ fullResponse：整体还原
  └─ streamResponse：SSERestorer（trie 缓冲，跨事件边界还原）
  ▼
recordAudit（20 字段）→ /metrics 计数 → publish(restore.done)
  ▼
客户端
```

关键点：**一次请求只用一个 `replacer.Session`**（`proxy.go:188`）。这保证占位符编号在跨字段、跨消息范围内全局唯一，避免同类型不同值跨段碰撞。

---

## 2. 对外 HTTP 接口

### 2.1 路由全表

来源：`internal/server/server.go`（`Handler` / `registerRoutes`）、`debug/handler.go:100-111`（`Mount`）。

| 方法 | 路径 | 处理 | 面 | 备注 |
|---|---|---|---|---|
| POST | `/v1/chat/completions` | 代理 + 脱敏/还原 | 数据面 | OpenAI 协议 |
| POST | `/v1/completions` | 同上 | 数据面 | |
| POST | `/v1/embeddings` | 同上 | 数据面 | 请求侧脱敏 |
| POST | `/v1/responses` | 同上 | 数据面 | OpenAI Responses API |
| POST | `/v1/messages` | 同上 | 数据面 | **Anthropic 协议**，自动选 anthropic 上游 |
| ANY | `/v1/models` | 纯透传 | 数据面 | 无需脱敏 |
| GET | `/healthz` | 健康检查 | 探针（匿名） | 检测器不可用时返回 503 `degraded` |
| GET | `/metrics` | Prometheus | 数据面 | |
| POST | `/v1/privacy/redact` | 递归脱敏 | 控制面 | **常驻隐私 API，不受 `--no-debug` 门控** |
| POST | `/v1/privacy/restore` | 按 request_id 还原 | 控制面 | 同上；**只能还原控制面自建的表**（见下） |
| GET | `/_debug`、`/_debug/*` | 调试面板静态壳 | **匿名 + 仅回环** | 壳内不含请求数据 |
| WS | `/ws/events` | 面板实时事件 | 控制面 | |
| GET/DELETE | `/_api/traffic` | 流量查询 / 清空 | 控制面 | |
| POST | `/_api/detect` | 单段检测 | 控制面 | |
| POST | `/_api/replace` | 试替换 | 控制面 | |
| GET/PUT | `/_api/rules` | 策略热切换 | 控制面 | |
| GET/PUT | `/_api/dictionary` | 仿真词典读写（热加载） | 控制面 | |
| GET/PUT | `/_api/registry` | 登记表读写（热加载） | 控制面 | 返回明文 PII |
| GET | `/_api/audit` | 审计查询 | 控制面 | |

`/_api/*`、`/_debug`、`/ws/events` 全部经 `loopbackOnly` 包装（`debug/handler.go`）：即使网关监听了 `0.0.0.0`，面板也不对外。早期存在的 `debug_bind` 配置项已删除（`04982a8`），因为「可配置外部绑定」与「隐私网关」的定位冲突。

**面板的路由在 `debug=false` 时根本不注册**（`Server.AdoptDebugRoutes` 未被调用 → root 上没有这几条规则）→ 落到数据面 mux 的 404。刻意造成 404 而不是 401：否则「面板关掉了吗」会变成一个需要猜的问题。

`/_debug` 走 `Server.panelBootstrap`：带 `?token=<有效令牌>` 时下发 `HttpOnly; SameSite=Strict` Cookie 并 302 回不带查询串的同一路径；不带该参数时原样交给下游渲染面板壳。`/_api/*` 与 `/ws/events` 走 `Server.panelAuthMiddleware`，在控制面令牌之外额外接受该 Cookie（`/v1/privacy/*` **不**接受 Cookie，保持最小权限）。

### 2.2 鉴权

`server.go`。路由分四层，各自独立（`Server.Handler`）：

| 面 | 路径 | 令牌 |
|---|---|---|
| 探针 | `/healthz` | **不鉴权**（匿名可访问） |
| 数据面 | `/v1/*`（LLM 转发）、`/metrics` | `gateway.auth_token` |
| 控制面 | `/v1/privacy/redact`、`/v1/privacy/restore` | `gateway.control_auth_token`，留空回退数据面令牌 |
| 控制面（面板） | `/_api/*`、`/ws/events` | 同控制面令牌，另接受面板 Cookie |
| 控制面（面板壳） | `/_debug`、`/_debug/*` | **不鉴权** + 仅回环 |

令牌解析在 `config.ResolveAuthToken`：`auth_token` 非空 → 用它；否则若 `allow_unauthenticated: true` → 不鉴权；否则读 `auth_token_file`（不存在则生成 32 字节随机令牌、`0600` 落盘、启动日志打印一次）。**空令牌只可能来自显式的 `allow_unauthenticated`。**

**为什么面板数据端点归控制面**：它们能读到请求明文与登记表（用户自报的真实 PII 值），与 `/v1/privacy/*` 属于同一类能力，而数据面令牌的语义只是「可以转发」。改造前这些路径挂在数据面 mux 上。

**为什么面板壳匿名**：壳（HTML/JS/CSS）不含任何请求数据；更重要的是它是「输入令牌」这个动作的载体——壳本身要凭据，第一次认证就无从完成。静态壳的暴露面只有面板本身的界面结构。

接受两种凭证形式：`Authorization: Bearer <token>`、`X-Api-Key: <token>`，用 `crypto/subtle.ConstantTimeCompare` 比较。不匹配返回 401 + `{"error":{"code":"unauthorized","message":"missing or invalid credentials for <scope>"}}`。

> 升级须知：`auth_token` 为空**不再**等于不鉴权；`/metrics` 现在需要数据面令牌。

### 2.3 隐私 API 契约

`/v1/privacy/redact` 支持三种形态（`proxy/privacy.go`）：

| 入参 | 行为 |
|---|---|
| `{"text": "..."}` | 单段文本脱敏 |
| `{"json": {...}}` | 递归遍历 JSON，只对字符串**值**脱敏，键永不脱敏 |
| `{"gate_only": true}` | **只扫描不改写**：返回 `has_pii` / `entities` / `blocked`，不建 request_id |

`gate_only` 是给 Claude Code hooks / MCP 这类「先看 PII 再决定拦放」的边界用的，`block_types` 可指定「只对哪些类型判定 blocked」，留空表示任意 PII 命中即 blocked。

`entities[].value`（命中原文）**默认不回显**，需要判定拦放时 `type` 已足够；要拿到原文需显式传 `include_values: true`。理由是默认回显会让网关同时成为一个「提交文本 → 取出其中 PII」的提取接口。

返回 `request_id` 后，`/v1/privacy/restore` 可多次还原同一 `request_id`（映射表按 id 存放，不是一次性）。

**还原的来源约束**：`restore` 只接受由 `redact` 建立的映射表。`Processor.Store`（数据面，`/v1/*` 转发）写 `origin="data"`，`Processor.StoreControl`（本 API）写 `origin="control"`；`Processor.AssertRestorableViaAPI` 在还原分支之前校验，`origin != "control"`（含空值）一律返回 `ErrNotFound` → **HTTP 404**。

动机：`request_id` 在数据面来自客户端可指定的 `X-Request-ID`（`proxy.requestID` 只约束其**形状** `[A-Za-z0-9_-]{8,64}`，不约束来源）。不校验来源时，「知道一个 request_id」就等于「能读出那次请求被脱敏掉的原文」——一条绕过 vault 加密与 TTL 语义、也不经过内容审计的读取路径。

拒绝统一为 404（与「不存在 / 已过期」不可区分）而不是 403：`403` 会确认该 ID 真实存在，等于提供一个存在性探针。

---

## 3. 配置契约

来源：`internal/config/config.go`。用 `yaml.v3` 解析，支持 `${ENV}` 与 `${ENV:-default}` 展开（先展开字符串、后送 YAML 解析，避免 `$` 与 YAML 语法互扰）。

### 3.1 完整 schema

```yaml
gateway:
  listen: ":8400"              # 必填，必须以 ':' 开头
  upstream: "https://..."      # 必填，字符串
  auth_token: "${GATEWAY_AUTH_TOKEN}"
  upstream_api_key: "${OPENAI_API_KEY}"
  upstreams:                   # 可选，多协议上游
    - protocol: "anthropic"    # openai | anthropic
      base_url: "https://api.deepseek.com/anthropic"
      api_key: "${DEEPSEEK_API_KEY}"   # 留空 = 透传客户端鉴权头
      api_version: "2023-06-01"
      path_prefix: ""          # 替换入口路径的 /v1 段（GLM=/v4，DashScope=/compatible-mode/v1）
  request_timeout: "30s"       # ⚠️ 尚未生效（无读者）
  debug: true
  log_level: "info"            # ⚠️ 尚未生效（无读者，也没有级别过滤机制）

detection:
  engine: "regex"              # regex | pii-engineer
  fallback_regex: true         # ⚠️ 尚未生效（无读者）：不会让任何实体绕过模型
  sidecar:
    command: "cargo run --release"
    endpoint: "http://127.0.0.1:8000"
    healthz: "/healthz"
    start_timeout: "60s"       # ⚠️ 尚未生效（无读者）
    restart_limit: 3           # ⚠️ 尚未生效（无读者）
    auto_start: false          # ⚠️ 尚未生效（全仓无 os/exec，从不拉起子进程）
  thresholds: { zh_phone: 0.8, ... }
  cache:
    enabled: true
    bind_conversation: true    # 缓存键绑定 conversation_id
    ttl: "30m"
    max_entries: 10000
  registry:
    enabled: false
    path: "./registry.yaml"    # 明文 PII，权限 0600

replacement:
  strategy: "placeholder"      # placeholder | simulate | bypass
  simulate_zh:
    person_name: true
    phone: true
    id_card: true
    bank_card: true
    dictionary: {}             # entity_type → (真实值 → 仿真值)
    identity_card: {}          # entity_type → 真实值（每种类型一个）
  irreversible: ["api_key", "password", "token"]
  per_type_fate: {}            # entity_type → reversible | mask | redact

policy:
  fail_closed: true          # 三个字段里只有这个是真实开关（main.go 读它）
  tool_call_scan: true       # 历史遗留：只能为 true/省略，写 false 启动期报错
  stream_restore: true       # 历史遗留：同上

vault:
  path: "./vault_data"
  encryption: "aes-256-gcm"    # 不可配：只接受这个值或省略，写别的启动期报错
  key_derivation: "scrypt"     # 不可配：同上
  request_ttl: "30m"
  persist: false

audit:
  enabled: true
  path: "./audit.log"
  export: ["pip", "gdpr"]
  log_pii: false
```

### 3.2 启动期校验（`config.Validate`，失败即退出码 2）

**设计原则：校验失败一律退出，绝不进入降级服务状态。**

| 规则 | 理由 |
|---|---|
| **YAML 严格解析（`yaml.KnownFields(true)`）** | 未知键直接报错而非静默忽略。这是「配置写错但服务照常起来」这类事故的唯一根治手段：`install.sh` 早期版本生成的 config.yaml 顶层写了 `upstream`/`detector`/`replacer` 三个**不存在的键**，服务仍能启动并**全量透传**（脱敏静默失效）——严格模式让这类错误在启动期就崩掉 |
| `gateway.upstream` 非空、`listen` 以 `:` 开头 | 基础可用性 |
| `upstreams[].protocol ∈ {openai, anthropic}`、`base_url` 非空 | 防止静默走错上游 |
| `strategy ∈ {placeholder, simulate, bypass}` | 拼写错误直接拦 |
| `engine ∈ {regex, pii-engineer}` | 同上 |
| `cache.ttl >= 1m` | 过短缓存无意义 |
| `registry.enabled` 时 `path` 必填 | 面板加的值不落盘，重启即静默消失 |
| `vault.request_ttl >= 5m` | |
| 阈值落在 [0,1] | |
| `per_type_fate` 值合法 | |
| **仿真词典「仿真值全局唯一」** | 见下方说明 |
| 身份卡展开后再做上述唯一性校验 | 手写词典与身份卡撞车同样在启动期拦下 |

> **为什么仿真值要全局唯一**：响应侧还原表是 `map[哨兵串]原值` 一张**平表**，不分类型分桶（`replacer.EntryPairs`）。两个真实值即使类型不同，只要共用一个仿真值，在还原表里就是同键互相覆盖——其中一个永久还原不回来，还会被错认成另一个。这是静默数据损坏，必须在入口拦下（`config.go:382-414`）。

#### 「默认上游 + 同协议覆盖」的合并语义（`EffectiveUpstreams()`）

`gateway.upstream` 是**默认上游**；`upstreams[]` 里同协议的条目**覆盖**该协议的默认上游，未覆盖的协议回落到默认。合并逻辑只有一份实现——`config.EffectiveUpstreams()`，启动期建路由（`cmd/llmate-gate/main.go`）与配置打印（`config.String()`）**共用它**，避免「打印出来的配置」和「实际生效的路由」两套算法漂移。

`EffectiveUpstream.String()` 对 api_key **只输出 `key=set` / `key=none(passthrough)`**，从不回显明文——日志与面板都可能被外传。

### 3.3 身份卡（`identity_card`）的语义

身份卡是 `dictionary` 的**语法糖**，不是第二条运行时路径（`config.go:257-286` + `simulator/identitycard.go`）：

- 用户按类型声明「我自己的真实值」，网关用**固定密钥** `llmate-gate/identity-card/v1` 生成格式保持、**跨重启稳定**的仿真值；
- 加载期展开进 `dictionary`，之后完全走同一条路径，无特殊分支；
- **手写词典优先**：同一 `(类型, 真实值)` 两边都写时，以手写为准；
- 与 session key 刻意的区别：session key 是进程随机的（重启即失效），身份卡要的是「内置身份」——把仿真名提前告知下游后，无论何时重启都还是那个名。

---

## 4. 检测层

### 4.1 引擎选择

`main.go:83-91`：`regex` 走内置正则引擎；`pii-engineer` 走 sidecar HTTP 客户端（500ms 超时）。

> ⚠️ **修正（2026-09-23，v1.9）**：本文档此前在此处写
> 「`fallback_regex: true` 的含义是『格式固定实体走正则加速通道，不进模型』」——
> **这句话描述的行为不存在**。`detection.fallback_regex` 除 `Default()` 赋 `true` 外
> **没有任何读取方**，配了它也不会让任何实体绕过模型；`engine: pii-engineer` 时全部实体
> 仍进模型。这一条不是「某个开关不灵」，它承诺的是**数据是否送模型**
> （隐私 + 性能），因此必须显式更正。同族共 8 个键，见 §12.1 与 `Specs/06` #31。

### 4.2 实体类型权威表（15 类）

`pkg/types/detect.go:38-73`。命名约定：

| 前缀 | 含义 | 类型 |
|---|---|---|
| `zh_*` | 仅中文语料有效 | `zh_person_name` / `zh_phone` / `zh_id_card` / `zh_bank_card` / `zh_address` |
| 无前缀（中文无关） | 国际通用 | `email` / `ip_address` / `date` / `api_key` / `password` / `token` |
| 无前缀（英文专属） | 沿用 Presidio/privaite 惯例 | `plate` / `url` / `us_ssn` / `credit_card` |

`AllTypes()` / `IsKnownType()` 是单一权威出口，面板下拉、登记表校验、配置校验统一用它（`0a4e511`）。

不可逆类型：`api_key` / `password` / `token`（`IsIrreversible`）。

### 4.3 正则规则清单

`detector/regex.go:41-88`：

| 规则 | 目标 | 关键设计 |
|---|---|---|
| `rePhone` | `1[3-9]\d{9}` | 段号合法 |
| `reIDCard18` / `reIDCard15` | 身份证 | 日期段 + ISO 7064 校验位（`pkg/cn`） |
| `reCreditCard` | 13-19 位数字 | **先 Luhn，再按 IIN 前缀分发** `zh_bank_card` / `credit_card`（`cardDispatch`） |
| `reEmail` | 邮箱 | |
| `reIPv4` | IPv4 | 逐段 0-255 校验 |
| `reDate` | 日期 | 支持 `-/.年` 分隔；**日分支必须长优先**（`(?:3[01]|[12][0-9]|0?[1-9])`）—— 后面跟的是可选的 `日?`，短优先会因 leftmost-first 提前接受而截断跨度（`2024-09-17` → `2024-09-1`，见 `Specs/06` B-24） |
| `rePlate` / `rePlateEN` / `rePlateCA` | 车牌 | 中文严格；英文/加州模式**必须带引导词**（弱格式实体策略） |
| `reURL` | URL | 结尾不允许句尾标点 |
| `reUSSSN` | 美国 SSN | 形态 + `global.ValidUSSSN` 二次过滤 |
| `reAPIKey` / `reJWT` / `reSecretKV` | 密钥类 | 不可逆处理 |
| `reNameCtx` / `reNameTile` | 中文人名 | **弱格式实体：只做高精度低召回** —— 必须有强上下文引导词（我叫/姓名/联系人…）或称谓后缀（先生/女士…）；捕获后经 `trimNameParticle` 修边（见下） |
| `reAddress` | 中文地址 | 必须命中行政区划链：省/自治区 **或直辖市** → 市/区/县 → 路/街/巷 → 门牌 |

RE2 不支持 lookaround，数字边界用手工邻字符检查替代（`hasDigitNeighbor` / `hasIDNeighbor`）。

#### 4.3.1 去重键必须是**区间**，不能是**值**（安全约束）

`scanPersonName` / `scanPlateEN` 内部对同一条规则的多次命中做去重，去重键是 `span{start,end}`——**绝不能改成按 PII 值去重**。

原因：一段文本里同一个 PII 值可以合法地出现多次（「我叫李杰琪，联系人李杰琪。」）。按值去重会**只上报第一次**，后续出现的明文就**原样发给上游**，且在审计/指标里完全无痕。这是真实可复现的 PII 泄漏，早期版本正是这么写的（见 §12 与 `Specs/06` P0-12）。

区间重叠的**跨规则**消解不在这里做，统一交给下游 `replacer.sanitizeEntities`（按区间排序，重叠时保留更长者）。

#### 4.3.2 人名修边 `trimNameParticle`

`([一-龥]{2,4})` 是贪婪捕获，会把紧跟人名的结构助词/动词一起吞进来（`员工张三的身份证号是…` → `张三的身`，污染精确率）。捕获后按下表修边：

| 捕获长度 | 判定 | 结果 |
|---|---|---|
| 4 | 前两字在 `commonSurnames`（含 21 个复姓：欧阳/司马/上官/诸葛/尉迟/令狐/呼延…） | 判为复姓四字名，**原样保留** |
| 4 | 第 3 字是硬性助词（不在 `commonSurnames`） | 截到前 2 字 |
| 4 | 其它 | 截到前 3 字 |
| 3 | 第 3 字是硬性助词 | 截到前 2 字 |
| 3 | 其它 | 原样保留 |

**`nameParticles` 只收硬性助词（的/了/是/在/把/被/与/及/为/以/之…），刻意排除「和/有/会/能/要」**——这些字可以合法地是人名的末字（李永和、张有、陈会）。把它们放进助词集会把人名截断（`李永和` → `李永`），比多吞一个字更糟。残留：两字人名后紧跟和/有/会/能/要 时仍会多吞一字，归入 NER 才能根治的已知边界（§12）。

> **能力边界**：人名与地址在 regex 引擎下**只保证精确率，不保证召回率**。源码注释直接写明：「完整召回依赖 `detection.engine=pii-engineer` 的 NER 模型」。修边只治「多吞字」（精确率），治不了「没上下文引导词就完全不召回」（召回率）。

> **这条边界在实测里的样子**：真对抗语料 §10.1 中 `person_name` / `address` 两个 subset 的 F1 仍是 0（引导词缺失、无省级前缀的就完全不召回），而 `phone` / `email` / `ip_address` / `id_card_masked` 全部 1.0。不是 bug，是**已声明的设计边界**——regex 引擎不承诺弱格式实体的召回。

### 4.4 登记表（`registry`）

定位：检测层的**一次精确匹配前置**，不是第二条运行时路径。

- 用户把「真实存在、但模型/正则必然漏检」的值（中文姓名、自定义地址、内部工号式邮箱）登记进来；
- 命中即产出实体，`Score: 1`，**绕过阈值过滤**（保召回）；
- 与内层检测结果 `Merge` 时，登记命中排在前（用户显式声明的类型比模型猜的更可信），用与替换阶段完全相同的重叠规则（start 升序、同起点长优先、贪心不重叠）。

实现要点：

| 点 | 做法 | 理由 |
|---|---|---|
| 匹配加速 | 按首字节分桶 `buckets[byte][]int` | 跳过 UTF-8 续字节（`0x80~0xBF` 不可能是起点），中文正文里占多数的字节被直接跳掉 |
| 大小写 | 只折叠 ASCII 字母，**长度不变** | 偏移可 1:1 映射回原文；CJK 逐字节精确 |
| 同起点 | 取最长匹配 | 登记了「张三」和「张三丰」时，「张三丰」算一条 |
| 校验 | ≥2 字符、无首尾空白、跨类型不可同值 | 单字符值（如「我」）会把整段文本毁掉 |
| 错误信息 | **只报类型 + 下标，绝不回显值** | 启动期校验失败要写日志，不能在排查信息里二次泄露 PII |
| 落盘 | 原子写（临时文件 + rename），文件 0600 / 目录 0700 | 保存到一半崩了不会留半截文件导致下次启动全丢 |
| 缓存联动 | `OnChange` → `dc.Flush()` + `merkle.Clear()` | 「刚补登一个值再重发同一句话」是本功能主用法，缓存不失效等于没生效 |

YAML 解析错误也会经过 `redactYAMLError` 抹掉回显值（yaml.v3 会把值截断写进消息）。

### 4.5 三层缓存

| 层 | 实现 | 键 | 失效 |
|---|---|---|---|
| 段级 LRU | `cache.LRU` | `conversation_id + sha256(text)`（`bind_conversation` 控制） | TTL 30m / LRU 淘汰 / `Flush()` |
| 会话级 Merkle | `cache.MerkleCache` | 按 conversation 维护段哈希序列 | 前缀分歧重算 / 截断重置 / `Clear()` |
| 引擎侧 | 无 | — | — |

Merkle 的用途是**多轮对话只扫新增 turn**：请求体里 `messages` 数组按段哈希做前缀复用，命中段零检测直接复用实体（`proxy.go:190-211`），并输出 `llmate_detect_incremental_segments_total{action=detected|reused}` 观测收益。

### 4.6 熔断与 fail-closed

`circuit.New(5, 10s, onStateChange)`：连续 5 次失败 → OPEN；冷却 10s → 半开探测。以 `GuardedClient` 装饰检测器，对 pipeline 透明。

`policy.fail_closed: true` 时，检测异常 → 请求被阻断（`proxy.go:138-150`），记 `llmate_blocked_total{reason}`，绝不「检测失败就原样放行」。这是隐私网关的核心安全承诺。

---

## 5. 替换层

### 5.1 策略 × 命运

两个正交维度：

**策略（`replacement.strategy`，全局）**

| 策略 | 行为 |
|---|---|
| `placeholder` | 替换为 `<<zh_person_name_1>>` 式占位符，响应侧还原 |
| `simulate` | 替换为格式保持的仿真值（张三 → 李雷），响应侧还原 |
| `bypass` | **整条请求原样透传，完全不脱敏**；不建映射表（`proxy.go:173-175`） |

`bypass` 用于「网关只做记录/前置」的拓扑，代价是零隐私保护，只适合本机调试。

**命运（fate，逐类型）**

| fate | 行为 |
|---|---|
| `reversible` | 可还原（占位符/仿真值） |
| `mask` | 保留部分字符（如 `110101********8531`） |
| `redact` | 完全抹除，不可还原 |

决策链：`policy.FateFor(entityType, strategy)` —— `per_type_fate` 配置 > `irreversible` 列表并入 > 策略默认。不可逆类型（api_key/password/token）强制 `redact`。

> 注意：`simulate_zh` 的四个开关（person_name/phone/id_card/bank_card）**不影响别名的生成，只控制是否启用仿真通道**；仿真器本身支持 8 类（见 5.2）。

### 5.2 仿真器（`simulator`）

`Generator.Fake(entityType, value, sessionKey)`，基于 HMAC 派生 + 种子随机，保证**同值恒映射同假值**（同一 session key 内）。

支持 8 类：`zh_person_name` / `zh_phone` / `zh_id_card` / `zh_bank_card` / `zh_address` / `email` / `ip_address` / `date`。

格式保持要求举例：

| 类型 | 约束 |
|---|---|
| 身份证 | 18 位 + **校验位必须正确**（`TestIDCard_Checksum`）；且不生成真实存在的身份证号 |
| 手机号 | 保持合法号段 |
| 银行卡 | **Luhn 必须通过** |
| 姓名 | 保持长度与复姓结构（欧阳 → 复姓仍复姓）；少数民族姓名结构保持 |
| 邮箱 | **保留域名**，只改 local part |
| IP | 保持地址类别（内网仍内网） |
| 日期 | 保持原格式 |

词典查表优先：`dictLookup(entityType, value)` 命中即用用户指定值，未命中回落 HMAC 派生。

**并发约定（新增，v1.3）**：`Generator` 的开关与词典是本类型里**唯一会被热加载改写**的状态
（`SetDictionary` 由面板 PUT 触发），因此定下三条硬约束：

1. `cfg` 字段**不导出**。导出字段挡不住调用方绕过 `mu` 直读；不导出让**编译器**成为护栏——
   比加测试更可靠，因为测试只能覆盖想到的路径。
2. 所有访问只走 `SetSimulateConfig` / `SetDictionary` / `Dictionary` / `dictLookup` / `enabled`。
3. `enabled(on func(SimulateZHConfig) bool)` **在 `RLock` 内**求值。**不要**在锁外写 `on(g.cfg)`：
   按值传参会**整体拷贝** `SimulateZHConfig`（含 `Dictionary` 的 map 头），与 `SetDictionary`
   的写入构成真实数据竞态（B-19，CI `-race` 上暴露）。`on == nil` 表示该类型恒开。

同类约定适用于 `replacer.impl`：`Strategy()`（**每个请求**都走的热路径，`proxy.strategy()`）
与 `NewSession()` 都在锁内快照 `r.cfg`。注意 `RLock` **不可重入**——
写者排在两次 `RLock` 之间会死锁，故 `NewSession` 先取快照、再在锁外归一化策略，
而不是持锁调 `r.Strategy()`。

### 5.3 Vault

`vault.MemVault`：AES-256-GCM 加密 + scrypt 派生 + TTL 清扫（每分钟）+ `persist` 控制是否落盘。`Delete` 会先 **zeroize** 密钥材料。密钥随机生成不落盘（进程重启即失效，`persist=false` 时映射表也不留）。

### 5.4 流式还原（SSE）

`replacer.StreamRestorer`：trie 结构 + `isPrefixOfAny` 前缀判断。核心难点是**占位符可能被切开跨越 SSE 事件边界 / 跨越 TCP 分片**——还原器必须识别「当前缓冲是某个哨兵的前缀」并继续等待，而不是立即输出。

`SSERestorer` 在其上再包一层：按 SSE 事件解析，只对 `data:` 行的 JSON 字符串值做还原，保留非 data 行、保留数字类型不动（`TestSSERestorer_PreservesNumbers` / `_NonTextKeysUntouched`）。

残留不完整占位符（流结束时仍在缓冲里、拼不成完整哨兵的字节）**每个还原实例自己记一份**（`StreamRestorer.orphans`，`Close()` 时 +1），代理层用 `restorer.Orphans()` 的**本请求增量**打点。

> **为什么不能直接采样全局值**：`StreamOrphanTotal` 是进程级累计，Prometheus 的 `.Add()` 语义是「累加增量」。若直接用全局累计值当增量上报（早期实现即如此），单请求计数会变成「进程启动至今」的雪球，且并发请求互相污染——该指标实际上退化成不可用的死指标。全局累计只适合直接 `promhttp` 暴露，不适合参与请求级打点。

---

## 6. 代理层

### 6.1 多协议上游路由

配置驱动，不写死厂商（`proxy.go:562-602`）：

- 入口同时暴露 OpenAI 兼容（`/v1/chat/completions` 等）与 Anthropic 兼容（`/v1/messages`）两类端点；
- `targetFor(endpoint)`：`messages` → anthropic 上游，其余 → openai 上游；
- `upstreamFor(endpoint, path)`：`path_prefix` 非空时替换掉路径里的 `/v1` 段（智谱 GLM 用 `/v4`，DashScope 用 `/compatible-mode/v1`）；
- `setUpstreamAuth`：
  - `api_key` 非空 → 删掉客户端的 `Authorization`/`X-Api-Key`，改注入自己的（Anthropic 用 `x-api-key`，OpenAI 用 `Bearer`）；
  - `api_key` 为空 → **透传模式**，保留客户端原样鉴权头。用于「gate 前置在 ccswitch 之类的下一跳之前」的拓扑：gate 只做脱敏、不持有凭据。

### 6.2 tool_call 扫描

**始终开启，没有开关**。递归遍历请求体，对 OpenAI `tool_calls[].function.arguments` 与 Anthropic `tool_use.input` 逐个值做检测替换；支持嵌套结构；**JSON 字符串里内嵌的 PII 也会被处理**（`transform` `proxy.go:353` / `anonymizeJSONString` `proxy.go:424`）。

> 配置里的 `policy.tool_call_scan` 是**历史遗留键，代码从未读取它**——过去写 `false` 什么都不会发生，是一句无声的谎话。现在合法取值收窄成 `true`（或缺省），写 `false` 在启动期报错。不给它真开关的理由：tool_calls 的 arguments 承载命令、路径、模型自造字面量，是整份请求里 PII 密度最高、最容易被外发的位置。详见 `config.PolicyConfig` 注释。

> 同一段文本出现在**同一请求的多个字段**时，每个字段都必须独立脱敏——不能因为「这个值我已经替换过了」就跳过。L2 泄漏级对等性测试（§10.3 的 `tool_call_nested` 载体）正是用这个场景做探针的。

有一个已修复的坑值得留档（`07481b0`）：`tool_call.arguments` 脱敏后必须**仍是 JSON 字符串**，不能变成对象——否则上游 API 直接报 400。

Anthropic 的 `tool_use.input` 结构不同（在 `content` 列表里嵌 dict，且 `type` 字段不是 PII 字段名），靠 `piiBlockTypes` 容器型 block 表强制进入 PII 上下文（`proxy.go:108-132`）。

### 6.2.1 判断层观测（P2 旁挂）

代理在遍历请求体的**同一个递归**里顺带喂给行为判断层一个结构化描述子（`observeToolCall` `proxy.go:472`；`transform` / `anonymizeJSONString` 的 `observe` 回调参数）：

- 取 `block["name"]` 作工具名，`arguments`（OpenAI）或 `input`（Anthropic，**必须 `type` 命中 `tool_use`/`server_tool_use`/`mcp_tool_use`**）作参数；
- 从 `commandFields`（`command`/`cmd`/`script`/`code`/`shell`/`url`/`file_path`/`path`/`pattern`）里取第一个命中的字符串作 `Command`，`Target` 取首词；
- 调 `judgment.Evaluate`，只打日志与指标。

**关键约束**：`observeToolCall` 无返回值——不修改 block、不写映射表、不影响字节。影子模式下开关判断层的上游字节与客户端响应必须逐字节相同，由 `TestProxy_JudgmentShadow_DoesNotChangeTraffic` 钉住。它看到的是**原始命令**（脱敏前的值），因为判断对象是「模型想做什么」，不是「脱敏后剩什么」。

### 6.3 请求改写要点

- **JSON 键永不脱敏**，只对字符串值递归；
- 重新序列化时 `SetEscapeHTML(false)`——否则 `<<`/`>>` 会被编码成 `\u003c`/`\u003e`，占位符在请求/响应中失效（`proxy.go:244-248`）；
- 非 JSON body 整段当文本脱敏；
- 事件发布是**早发布**的：`request.received` 在脱敏前就发，`replaced` 在脱敏后立刻发，不等上游响应。面板因此能看到「请求已收到但还没回」的中间态。Hub 按 `request_id` 合并事件。

---

## 7. 可观测性

### 7.1 审计事件（`audit.Event`，20 字段）

`log_pii: false`（默认）时不记原文。JSON Lines 一行一条，同时进内存环（最近 200 条）供 `/_api/audit` 查询。

```
timestamp / schema_version / request_id / conversation_id / client_id
upstream / model
detected_entities[{type, score, value?}] / replaced_count
strategy / restored / streaming
latency_ms / detector_latency_ms
outcome / error_code
sample_text
```

`Export: ["pip", "gdpr"]` 声明合规导出目标。

### 7.2 Prometheus 指标（19 个）

`llmate_requests_total{endpoint,outcome}`、`llmate_detect_latency_seconds{engine}`、`llmate_replace_total{fate}`、`llmate_restore_total{endpoint}`、`llmate_blocked_total{reason}`（即 fail-closed 阻断）、`llmate_upstream_errors_total{endpoint,status}`、`llmate_stream_orphan_placeholders_total{endpoint}`、`llmate_detect_cache_hits_total{conversation}`、`llmate_detect_cache_misses_total{conversation}`、`llmate_detect_incremental_segments_total{action}`、`llmate_vault_size`、`llmate_active_streams`、`llmate_pii_detected_total{entity_type,fate}`、`llmate_tool_calls_scanned_total`、`llmate_request_total_latency_seconds{endpoint}`、`llmate_response_restore_latency_seconds{endpoint}`、**`llmate_verdict_total{action,category,engine}`**、**`llmate_judge_latency_seconds{engine}`**、**`llmate_judge_unavailable_total{engine,reason}`**（后三个为判断层，见 §15.6）。

延迟类指标**单位统一为秒**（Prometheus 惯例）。指标名属公共接口，改名会破坏已对接的 Grafana/告警，故冻结。

打点口径的两条硬约定：

| 约定 | 说明 |
|---|---|
| **请求级指标必须用「本请求增量」，不得用全局累计值** | `llmate_stream_orphan_placeholders_total` 是典型：源码层维护 process-global 累计 + 每实例计数两套，代理层只取实例值（`restorer.Orphans()`）打点。回归测试 `TestProxy_StreamOrphanMetric` / `TestProxy_BufferedOrphanMetric` 断言指标值**恰好为 1.0**（而非 ≥1），用于钉死这一点 |
| 面板/配置打印不回显密钥 | `config.String()` 对 api_key 只输出 `key=set` / `key=none(passthrough)` |

### 7.3 调试面板

`/_debug`，4 个 Tab：流量 / Playground / 规则 / 审计。

规则页支持**热加载**：策略切换（`repl.SetStrategy`）、仿真词典编辑、登记表编辑——保存即落盘 + 立即生效，无需重启。登记表保存会触发两层检测缓存失效。

---

## 8. 集成面

| 集成点 | 载体 | 说明 |
|---|---|---|
| MCP | `cmd/mcp-server` | 3 个工具：`anonymize`（脱敏+返回 request_id）、`deanonymize`（按 id 还原）、`scan_tool_params`（预检式扫描：是否含 PII + 脱敏样例） |
| Claude Code hooks | `hooks/lmgate_hook.py` + `pre-tool.sh` / `post-tool.sh` | pre-tool 调 `/v1/privacy/redact` 做递归脱敏与 PII 告警 |
| VS Code 扩展 | `vscode-ext/` | 命令 `llmateGate.enable` / `disable` / `openDashboard`；可自动拉起网关进程；未找到 binary 时给出明确提示 |
| Linux/macOS 安装 | `scripts/install.sh` | 装 `~/.local/bin`，注册 systemd --user / launchd。支持 `--binary` / `--port` / `--no-autostart` / `--dry-run` / `--uninstall`；binary 路径按 `uname -m` 推导架构（**不写死**） |
| Windows 安装 | `scripts/install.ps1` | 装 `%LOCALAPPDATA%\Programs`，注册计划任务 + 桌面快捷方式 |
| 包管理 | `scoop-bucket/llmate-gate.json` | Windows amd64 + arm64 双入口 |
| 发布 | `.github/workflows/release.yml` | tag `v*.*.*` 触发 → 6 平台交叉编译 → 建 Release + 上传产物；支持 `workflow_dispatch` 手动重发 |

---

## 9. 质量门禁与测试

### 9.1 CI（10 个 job）

| job | 内容 |
|---|---|
| `verify` | vet + `go test -race`；失败时把失败用例写回 check run 注解 |
| `build` | 交叉编译冒烟 |
| `e2e` | E1-E8（契约级） |
| `ui-smoke` | D1-D6（面板） |
| `coverage` | 总门槛 + 逐包门槛 |
| `bench` | 三语料结构校验（`validate.py --all`，重复**硬错**）+ 评估器自检 |
| **`bench-gate`** | **起真实网关 → L2 泄漏级守门 + 真对抗阈值守门**（端到端） |
| `bench-baseline` | 进程内真实引擎召回（`cmd/bench-runner`） |
| `lint` | golangci-lint，8 个 linter，0 issues |
| `perf` | 8 个 Go benchmark，min-of-6，容差 1.25× |
| `vuln` | govulncheck |

**`bench-gate` 补的是「跑不起来的守门」**：此前 L2 载体守门与真对抗语料**从未在 CI 里执行过**，
只有进程内引擎的召回基线。而历史上 L2 报出的 30 条 regression 恰恰属于
「同一段文本出现在同一请求的多个字段」这类只有端到端才测得到的缺陷。
新增 job 的配置与阈值见 `gateway/configs/bench-gate.yaml` 与 `.github/workflows/ci.yml`。

**子模块**：`bench/` 指向 `cn-pii-bench`，`.gitmodules` 声明 `branch = main`。
CI 用 `submodules: true` 按**父仓记录的 SHA** 检出（不受 `branch` 影响），
所以每次改动 bench 侧脚本或语料后，**必须同步更新父仓的锚点**，否则 CI 测的还是旧数据。

**`verify` 的失败必须自述（v1.3）**：Actions 的 job 日志 REST 接口要求 admin 权限
（公开仓库同样返回 `403 Must have admin rights to Repository`），没有该权限的人
只能看到一句「Process completed with exit code 1.」。因此 `go test` 的输出 tee 到文件，
失败时由 `scripts/ci-test-annotate.py` 把 `--- FAIL` / `FAIL <pkg>` / `DATA RACE`
写成 `::error::` 注解——**注解不依赖日志权限**。该步骤不改变构建结论
（判定失败仍由 `go test` 那一步负责，`set -o pipefail` 保证退出码不被 `tee` 吞掉）。
这条改动是 B-19 排查代价的直接产物：本机 `-race` 不可用（见 §12.2），
竞态缺陷只能靠 CI 反馈，那么「CI 能不能说清楚为什么红」就是能力问题而非体验问题。

**job 依赖只表达「需要上游产出」（v1.5）**：全仓唯一的 `needs` 是 `e2e → build`
—— e2e 跑的是**构建出来的二进制**，属真实产物依赖。
其余 8 个 job（`build` / `coverage` / `bench` / `bench-gate` / `bench-baseline` /
`lint` / `perf` / `vuln`）互不依赖，各自 checkout + setup-go 后报结论。

> 原先它们全部 `needs: verify`，于是 `verify` 一红就全被 skipped，**一次 CI 只产出一个信号**。
> 实际代价：`lint` 报的 S1016 从 `8bb0dbb` 起就存在，却因 `verify` 连续三轮红而被跳过整整三轮。
> 改为并行后墙钟时间不增反减（公开仓库 Action 分钟数不计量）。
> 详见 `Specs/06` B-19 的「连带发现 1」。

Go 版本：`GO_VERSION: '1.25.13'`（CI 权威口径；`gateway/go.mod` 声明 `go 1.24`）。

### 9.2 端到端

- `e2e/e2e.sh`：387 行，断言 E1-E8（契约级）
- `e2e/ui_smoke.sh`：256 行，断言 D1-D6（面板）
- `e2e/latency.sh` + `latency_client.py`：多轮 P99 基准（纯标准库）

### 9.3 本地回归三件套（跨仓，改动检测层后必跑）

CI 只跑仓内 fixture 校验；**真实指标回归在姊妹仓 `cn-pii-bench`**，因为需要「起网关 + 打真实流量」：

```bash
# 1) 单元/契约
cd gateway && go test ./... -count=1 && go vet ./...

# 2) 语料自检（重复即硬错，防止「换数字灌水」把指标做虚）
cd ../cn-pii-bench && python3 validate.py --all

# 3) 三口径实测（需网关在 :8413 运行）
python3 runner.py --endpoint http://127.0.0.1:8413 --cases fixtures/cases.jsonl
python3 runner.py --endpoint http://127.0.0.1:8413 --cases fixtures/cases_en.jsonl
python3 bench_runner_adversarial.py --endpoint http://127.0.0.1:8413
python3 carriers.py --base-url http://127.0.0.1:8413      # 不带 --limit，跑全量
```

> **不要加 `--limit`。** 子集采样会整类地掩盖缺陷：`--limit 60` 恰好只取到
> `person_name` + `phone`，银行卡一条不进样本（`Specs/06` B-16）。

守门模式（供 CI，退出码 2 即未通过）：

```bash
python3 carriers.py --base-url http://127.0.0.1:8413 --gate
python3 bench_runner_adversarial.py --endpoint http://127.0.0.1:8413/v1/privacy/redact \
  --report --gate-on span --min-precision 0.99 --min-recall 0.53
```

> ⚠️ **2026-09-21 起阈值再次变动**（本节其余文字未动，勿照旧值执行）：
> `--min-recall` 由 `0.46` 改为 **`0.53`**，`--gate-on span` 不变。
> 原因：检测器侧落地**形态容忍**（分隔符 / 全角归一化），门禁锚定的 28 条语料
> span R 由 0.4792 升到 **0.5417** —— **阈值只是跟随基线，防回归语义不变**。
> 窗宽推算与实测见 `HANDOFF.md` §14。
>
> ⚠️ **2026-09-17 起的上一次变动**（保留以说明阈值演进）：
> `--min-recall` 由 `0.60` 改为 **`0.46`**，并显式加 `--gate-on span`（此前靠脚本默认值）。
> 原因是门禁锚定的 28 条基线语料在 09-16 修订过（GT 37 → 48），基线由 span R=0.6216 变为 0.4792。
> 推算与实测见 `HANDOFF.md` §13 末。

判读顺序：**先看 L2 载体 regression 是否为 0**（泄漏级，最敏感，能抓到 span 级指标看不见的泄漏），再看真对抗 P/F1，最后才看合成语料（自作者语料 F1=1.0 只说明「检测器与生成器口径一致」，不构成结论）。

> 本容器无法跑 `-race`（ThreadSanitizer 不可用，见 §12.2），以 `go test` + `go vet` 替代；CI 覆盖 `-race`。

---

## 10. 实测数据（本机实跑，2026-09-15 20:40）

> ⚠️ **勘误（2026-09-21）：本节的真对抗数字属「语料修订前」口径，不要当作当前基线引用。**
>
> 两次叠加的口径变化：
> 1. **09-16 语料修订** —— 修掉「模板内联真 PII 未标注 + 地址标注粒度自相矛盾」
>    （详见 `cn-pii-bench/README.md`「语料修订」节），GT 由 37 增至 48；
> 2. **09-21 检测器形态容忍** —— 分隔符 / 全角归一化落地，语料**逐字节未变**。
>
> 真对抗 28 条的**当前**数字（`engine=regex`，09-21 实跑）：
>
> | 口径 | P | R | F1 |
> |---|---|---|---|
> | **span（主口径）** | 1.0000 | **0.5417** | **0.7027** |
> | strict（下界） | 0.9231 | 0.5000 | **0.6486** |
> | 悲观（弱点计 FN） | 1.0000 | 0.5306 | **0.6933** |
>
> 报告原文 `bench/reports/cases_adversarial_20260921-110625.md`。
> （09-16 的对应值为 span R 0.4792 / strict 0.5915 / 悲观 0.6389。）
>
> 另有 318 条「表面形式 × 载体」矩阵语料（`cases_adversarial_ext.jsonl`）：
> span **0.9664**（P 1.0000 / R 0.9349）/ strict 0.9602 —— 修复前为 span 0.5946。
> 其中 **290 条形态矩阵现已 100% 检出**；与 28 条语料剩下的 22 个 FN 完全重合，
> 且全部是弱格式实体（人名 / 地址缩写），属 NER 范畴而非形态问题。
> 它**仍不参与门禁判定**（2026-09-17 留下的「等形态容忍落地后再决定」条件已满足，
> 但升级属质量标准决策，本轮未做）；自 09-17 起它以「形态诊断（非门禁）」步骤在
> CI 的 `bench-gate` job 里**每轮运行**（带 `--report`、不带阈值，报告随 artifact 上传）。
>
> **门禁对齐（2026-09-21）**：`bench` 子模块 gitlink 由 `b377e16` 前移到 `b6779a7`
> （仅新增本轮报告与 README 同步，**评估器脚本零改动**），`--min-recall` 0.46 → **0.53**。
> 合成中文 / 英文两个语料与 p50/p95/p99 延迟**本轮未复跑**，沿用本节记录
> （延迟另行在本机按 `gate_only=true` 实测，见 `README.md` 真对抗节）。
>
> **门禁对齐（2026-09-17）**：`bench` 子模块 gitlink 已由 `c8db66d` 前移到 dev HEAD `b377e16`，
> 即 CI 现在用的是**修订后**的语料（此前是「修复后的评估器 + 修订前的语料」，尺子与库存不一致）。
>
> **本节的下述表格作为 09-15 那一次实测的历史记录保留**，不作为当前对外口径。

本机（原生 Linux / arm64，Go 1.25.13）以**当前 HEAD 源码**用 `go build` 重新编译、
在独立端口运行网关后，用 `cn-pii-bench/runner.py` / `bench_runner_adversarial.py` / `carriers.py` 实跑：

| 语料 | 条数 | P | R | F1 | TP | FP | FN | p50/p95/p99 |
|---|---|---|---|---|---|---|---|---|
| 真对抗 `cases_adversarial.jsonl`（严格口径） | 28 | **1.0000** | **0.6216** | **0.7667** | 23 | 0 | 14 | 0/1/10 ms |
| 真对抗（悲观口径，4 条 `expect_miss` 计入 FN） | 28 | 1.0000 | 0.5610 | **0.7188** | 23 | 0 | 18 | — |
| 合成中文 `cases.jsonl` | 240 | 1.0000 | 1.0000 | 1.0000 | 360 | 0 | 0 | 0/1/4 ms |
| 英文基线 `cases_en.jsonl` | 180 | 1.0000 | 1.0000 | 1.0000 | 330 | 0 | 0 | 0/1/2 ms |

**真对抗精确率已从 0.9565 提升到 1.0000**：此前唯一的 1 条误报来自人名贪婪捕获（`张三的身`），由 `trimNameParticle` 修边（§4.3.2 / `Specs/06` P1-13）消除。召回率不变（0.6111 → 0.6216 的微升来自语料去重后重新配平），说明该缺陷是**纯精确率问题**，修的没有副作用。

**语料质量**：三个语料均为 **0 重复组**（28/240/180 条全为唯一正文）。真对抗语料的 21 个独立句法形态（数字归一后）说明它不是「一句话换几个数字」灌水出来的。

### 10.1 真对抗 by-subset（诚实口径）

| subset | n | TP | FP | FN | P | R | F1 | 结论 |
|---|---|---|---|---|---|---|---|---|
| email | 4 | 4 | 0 | 0 | 1.0 | 1.0 | 1.0 | 含 `+` 标签邮箱通过 |
| ip_address | 2 | 2 | 0 | 0 | 1.0 | 1.0 | 1.0 | 带端口通过 |
| phone | 9 | 6 | 0 | 0 | 1.0 | 1.0 | 1.0 | emoji 相邻、无分隔符通过；3 条 `+86-` 形态在 `expect_miss` 中豁免 |
| **id_card_masked** | 1 | 1 | 0 | 0 | 1.0 | 1.0 | **1.0** | 掩码本身仍是弱点（`expect_miss`），但同句的真名 `张三` 已正确检出——此前该 case 的 `expect: []` 反而把真 PII 判成误报 |
| mixed | 2 | 6 | 0 | 2 | 1.0 | 0.75 | 0.8571 | 人名漏检拖累 |
| tool_call | 2 | 4 | 0 | 4 | 1.0 | 0.5 | 0.6667 | 人名漏检拖累 |
| **person_name** | 4 | 0 | 0 | 4 | — | 0 | **0** | 无上下文引导词时完全不召回（设计边界） |
| **address** | 4 | 0 | 0 | 4 | — | 0 | **0** | 缺省级前缀 / 园区式地址不匹配（设计边界） |

### 10.2 已知弱点台账（`expect_miss`）

语料用 `expect_miss` 显式登记「已知弱点」，与 `expect: []`（确实无 PII）区分开——评估器对被登记的漏报**豁免 FP 计分**，同时保留台账口径，避免「把已知弱点洗成精度」。

当前 4 条，**已恢复 0 / 仍漏报 4**：

| case | 类型 | 值 | 原因 |
|---|---|---|---|
| `phone_fmt_adv-004/005/006` | `zh_phone` | `+86-…-…-…` | `known_weakness_unnormalized_phone_format` |
| `idcard_mask_adv-028` | `zh_id_card` | `110101********8531` | `real_world_masked_id_card` |

严格口径与悲观口径**两个数都报**：0.7667 与 0.7188。只报前者会显得比实际强。
（**语料修订 + 形态容忍后为 span 0.7027 / strict 0.6486 / 悲观 0.6933** —— 见 §10 顶部勘误。）

### 10.3 L2 泄漏级载体对等性（`carriers.py`，**全量 240 条**）

口径是**泄漏级**（脱敏后载荷中 PII 原串是否消失），不是检测器 span 级：

| 载体 | 已脱敏 | 泄漏 | 相对 flat 的 regression | 往返通过 |
|---|---|---|---|---|
| `flat` | 360 | 0 | — | 240/240 |
| `multimodal` | 360 | 0 | OK | 240/240 |
| `tool_call` | 360 | 0 | OK | 240/240 |
| `tool_call_nested` | 360 | 0 | OK | 240/240 |

✅ **PARITY OK** —— 3 种结构化载体相对 flat 基线 **0 regression**、往返 **240/240**。v1 门禁达标。

另有 **36 条按设计不可逆**（`zh_bank_card` 30 条 + 含银行卡的 `adversarial` 6 条）单列入台账：银行卡在 `strategy=placeholder` 下的命运是 mask（`replacer.go:292-295`，保留后 4 位），本就不可还原，故豁免往返断言。

> **此前的 30 条 regression 不是载体适配问题，而是检测层的值去重缺陷**（§4.3.1 / `Specs/06` P0-12 / B-15）：`tool_call_nested` 的样本把同一段文本同时放进 `user.profile.bio` 和 `tags[0]`，第二处因按值去重而未脱敏，于是被 L2 判为泄漏。修掉去重键后 regression 直接归零——这也说明 L2 泄漏级口径**能捕捉到 span 级指标看不见的缺陷**。

> **掩盖缺陷的采样陷阱**：早期只跑 `--limit 60`（`cases.jsonl` 前 60 条 = `person_name` 30 + `phone` 30），一条银行卡都没进样本，于是「往返 60/60」看起来全绿，实际上整类缺陷不可见。**守门必须跑全量。** 详见 `Specs/06` B-16。

> **合成语料 F1=1.0 不是对外宣称值。** 在自作者合成语料上自评，F1 高只说明「检测器与生成器对同一套仿真规则达成一致」，不构成真实场景结论（`Specs/03` §4.3.4 已明确）。诚实数字是**真对抗 span 0.7027 / strict 0.6486 / 悲观 0.6933**（语料修订 + 形态容忍后口径，见 §10 顶部勘误）。

---

## 11. 能力矩阵

| 能力 | 状态 | 证据 |
|---|---|---|
| OpenAI 兼容代理（5 端点） | ✅ 已实现 | `server.go:61-65` |
| Anthropic 兼容（`/v1/messages`） | ✅ 已实现 | `proxy.go:573-589`（`isAnthropicEndpoint` + `targetFor`） |
| 配置驱动多上游 + 路径前缀 | ✅ 已实现 | `config.go:48-56` |
| 中文 PII 检测（10 类） | ✅ 已实现 | `detector/regex.go` |
| 国际 PII 检测（5 类） | ✅ 已实现 | `pkg/global` |
| 占位符替换 + 还原 | ✅ 已实现 | `replacer/` |
| 格式保持仿真替换（8 类） | ✅ 已实现 | `simulator/` |
| 仿真词典（用户指定假值） | ✅ 已实现 | `95fbb67` |
| 身份卡（跨重启稳定身份） | ✅ 已实现 | `8ce1966` `83dee5f` |
| 登记表（补召回） | ✅ 已实现 | `addd75b` `d12c810` |
| bypass 透传策略 | ✅ 已实现 | `b87bfba` |
| per-type 命运（mask/redact） | ✅ 已实现 | `policy/` |
| 递归 tool_call 参数扫描 | ✅ 已实现 | `proxy.go:317-411`（`transform` + `anonymizeJSONString`） |
| SSE 跨事件边界还原 | ✅ 已实现 | `replacer/sse.go` |
| Merkle 增量检测 | ✅ 已实现 | `cache/merkle.go` |
| 熔断 + fail-closed | ✅ 已实现 | `circuit/` |
| 结构化审计 + 环 + 查询 | ✅ 已实现 | `audit/` |
| 16 个 Prometheus 指标 | ✅ 已实现 | `metrics/` |
| 调试面板（4 Tab + 6 API） | ✅ 已实现 | `debug/` |
| MCP 3 工具 | ✅ 已实现 | `cmd/mcp-server` |
| Claude Code hooks | ✅ 已实现 | `hooks/` |
| VS Code 扩展 | ✅ 已实现 | `vscode-ext/` |
| Linux 安装脚本 | ✅ 真机跑通，5 个缺陷已全修 | `Specs/06` P1-1~P1-5；`--dry-run` 不再落盘、`--uninstall` 清理完整、端口一致性已校验 |
| Windows 安装脚本 | ✅ 已真机冒烟（`0cea49b`） | |
| **L2 泄漏级载体对等性** | ✅ **PARITY OK（regressions=0，往返 240/240）** | `carriers.py` **全量 240 条** × 4 载体，见 §10.3 |
| **语料自检（重复/漂移）** | ✅ `validate.py --all`，重复即硬错 | 三语料 0 重复组 |
| **按设计不可逆显式入账** | ✅ 可逆性实测探测 + 台账 | 36 条银行卡（mask），见 §10.3 |
| NER 检测（pii-engineer） | ⚠️ 客户端已实现，sidecar 为 mock | `piiengineer.go` 就绪，无真实服务 |
| Homebrew tap | ❌ 未建 | |
| issue/PR 模板 | ❌ 未建 | |
| Dockerfile / 服务端托管 | ❌ 明确不做 | 用户拍板（2026-09-10） |
| Tauri 桌面 UI / systray | ❌ 已放弃 | CGO 与纯 Go 交叉编译冲突 |

---

## 12. 缺陷台账与能力边界

本节的**唯一职责**是记录「现在还剩什么没修」。已修项只留一行结论，完整修复记录在 `Specs/06-代码审阅-优化点清单.md`。

### 12.1 已修缺陷（保留结论，便于追溯）

| # | 缺陷 | 严重度 | 状态 |
|---|---|---|---|
| 1 | `install.sh` / `install.ps1` 生成的 config.yaml **schema 错误**（顶层 `upstream`/`detector`/`replacer` 三个键都不存在） | 🔴 高 | ✅ 已修 |
| 2 | `config.Load` 未启用严格模式，**未知键被静默忽略**，导致 #1 无法被用户察觉 | 🔴 高 | ✅ 已修（`KnownFields(true)`，见 §3.2） |
| 3 | `--dry-run` 仍会真实写入 config.yaml（该写操作未走 `run()` 包装） | 🟡 中 | ✅ 已修 |
| 4 | `--uninstall` 不删 binary/配置；且 `set -e` 下 `systemctl` 一失败就中断 | 🟡 中 | ✅ 已修 |
| 5 | `--port` 与已存在 config.yaml 不一致时，启动端口 ≠ 健康检查端口 → 假失败 | 🟡 中 | ✅ 已修 |
| **P1-18** | `install.sh` 的 `guess_binary` 把架构写死（linux→amd64 / darwin→arm64）：arm64 Linux 上必然找不到产物；若 `dist/` 里恰有交叉编译的 amd64 产物则会装上**跑不起来**的二进制 | 🟡 中 | ✅ 已修（按 `uname -m` 推导 goarch + 失败时列出可用产物） |
| **P0-12** | **`scanPersonName` / `scanPlateEN` 按「值」去重 → 同一段文本里重复出现的 PII 只脱敏第一次，其余明文直发上游，且审计/指标无痕**（真实 PII 泄漏） | 🔴 **高** | ✅ 已修（去重键改为 `span{start,end}`，见 §4.3.1） |
| **P1-13** | 人名贪婪捕获把结构助词吞入（`员工张三的身份证号是…` → `张三的身`），造成真对抗 1 条误报 | 🟡 中 | ✅ 已修（`trimNameParticle`，见 §4.3.2） |
| **P1-6** | `llmate_stream_orphan_placeholders_total` 用进程级累计值当请求增量上报 → 雪球式虚高、并发污染，指标实为死指标 | 🟡 中 | ✅ 已修（每实例计数 + 本请求增量打点，见 §5.4 / §7.2） |
| **B-15** | L2 载体对等性 30 条 regression | 🟡 中 | ✅ 已修（根因即 P0-12；现 regressions=0，见 §10.3） |
| P2-7 | `Handle` 重复 `io.ReadAll` 同一 body（拆出 `HandleWithBody`） | 🟢 低 | ✅ 已修 |
| P2-8 | 「可仿真类型」清单在 `config`/`simulator` 两处各写一份，会漂移 | 🟢 低 | ✅ 已修（`simulator.simulatable` 为唯一权威表） |
| P2-9 | 上游合并语义在启动建路由与配置打印两处各写一份 | 🟢 低 | ✅ 已修（`config.EffectiveUpstreams()` 单一实现） |
| **B-19** | **表驱动重构把「读一个 bool」变成「按值拷贝整个 `SimulateZHConfig`」（含 `Dictionary` 的 map 头）→ 与 `SetDictionary` 的写入构成数据竞态，CI `verify` 的 `-race` 红灯**；连带 `replacer.Strategy()` / `NewSession()` 也在锁外读 `r.cfg` | 🔴 高 | ✅ 已修（`cfg` 不导出 + `enabled()` 锁内求值 + `Strategy()`/`NewSession()` 锁内快照，见 §5.2） |
| **#26** | `policy.tool_call_scan` / `stream_restore` 是空转的假开关 | 🟡 中 | ✅ 已修（收窄成只能为真，写 false 启动期报错；见 `Specs/06` #26） |
| **#31** | **8 个「零读者 / 仅校验」配置键** —— `gateway.request_timeout`、`gateway.log_level`、`detection.fallback_regex`、`sidecar.{start_timeout,restart_limit,auto_start}`、`vault.key_derivation` 零读取方；`vault.encryption` 仅被校验、不被使用 | 🟡 中 | ✅ 已修（2026-09-23）：字段与示例配置逐条标注「尚未生效」；两个不可配的 vault 键**收窄成唯一合法值**（写别的启动期报错）+ 单测。**未改行为、未删除任何键**——实现与否留给后续决定，见 `Specs/06` #31 |
| **#32** | **文档/help 给出的命令与实现不符** —— README「从源码构建」两条命令都跑不通（`--upstream` 这个 flag 不存在、`configs/config.yaml` 这个文件不存在），同名标题还出现两节 | 🟡 中 | ⏸ 文档侧已修（2026-09-23，`66bf806`）：补 `cp config.example.yaml config.yaml` 一步、删掉错位的重复节、加「上游只能写在配置文件里」说明。**代码侧 3 条见 §12.2** |

### 12.2 未修 / 明确不做

| # | 项 | 类型 | 说明 |
|---|---|---|---|
| P2-14 | SSE 帧外的不完整哨兵字节不计入 orphan | ⏸ **已接受** | `SSERestorer` 只解析 `data:` 行，被切在帧边界之外的裸字节不进入 trie 缓冲，因此不计数。这是**刻意选择**：帧外的字节本就不该做还原（不是 JSON 值），计入反而产生噪声告警。保留观察，不修 |
| — | `go test -race` 在本容器不可用 | 环境限制 | `FATAL: ThreadSanitizer: unsupported VMA range (Found 39 - Supported 48)`，非代码问题。本机以 `go test ./...` + `go vet ./...` 替代；CI 的 `verify` job 覆盖 `-race`。**代价是竞态缺陷只能靠 CI 反馈**，故 CI 的失败必须自述（见 §9.1 与 Specs/06 B-19） |
| — | `pii-engineer` sidecar 为 mock | 能力缺口 | 客户端已就绪，无真实 NER 服务 |
| — | ~~本机没有 golangci-lint~~ | ✅ 已解决 | 此前 lint 类问题**只能靠 CI 反馈**（`unused` 一次、`QF1001` 一次）。现已在本机装上 CI 同版本（v2.6.1，`/home/jzhli/.gotmp/golangci-lint-2.6.1-linux-arm64/`），**推之前先跑**：`HOME=/home/jzhli XDG_CACHE_HOME=/home/jzhli/.cache TMPDIR=/home/jzhli/.gotmp <bin> run --timeout 5m --config .golangci.yml`。三个坑：解包要 `tar --no-same-owner`（release tarball 的 uid/gid 本机不存在），运行时必须给 `HOME`/`XDG_CACHE_HOME`（否则去 `mkdir /root/.cache` 被拒），**且必须给 `TMPDIR`** —— `/tmp` 是 10MB tmpfs，漏了会报 `write /tmp/go-build…/importcfg: no space left on device`（看着像磁盘满，实际 `/home` 还有 332G）；且 `go` 要走绝对路径（PATH 里那个是 1.15.9）。详见 `HANDOFF.md` §16 收口段 |
| — | ~~格式一致性零门禁~~ | ✅ 已解决（`Specs/06` #30） | `.golangci.yml` 缺 `formatters` 段 —— v2 里 gofmt/goimports **不在 `linters` 下**，不显式声明就完全不跑。已加 `formatters: enable: [gofmt]` 并全仓 `gofmt -w`。**注意 `issues.uniq-by-line`（默认 true）按行去重**：一行既不合格式又被别的 linter 命中时只显示后者，属可用性差异而非盲区 |
| — | 启动期错误不带 cause（`Cause()` 全仓零调用） | ⏸ **待定**（`Specs/06` #32-E） | `errors.Error()` 有意不带 cause（「不对外暴露内部细节」），但日志侧也没有补 `Cause()` ⇒ 启动报 `config error: invalid_config: read config file`，看不出是路径写错、权限不足还是路径是个目录。修法：新增 `errors.LogString`（Code+Message 后附 cause），8 处 `log.Fatalf` 改用它，**不动 `Error()` 与 HTTP 响应**。2026-09-23 拍板本轮不做 |
| — | usage 承诺的 `-V` 别名未注册 | ⏸ **待定**（`Specs/06` #32-A） | `cmd/llmate-gate/main.go:54` 的 help 文本写着 `(also -V)`，但只注册了 `version`；实测 `-V` 报 `flag provided but not defined: -V`（exit 2）—— 同一条 usage 输出里自相矛盾。修法：注册一行（Unix 惯例），或反向删掉那段文本 |
| — | `TestExampleConfig_Loads` 只硬编码单个文件 | ⏸ **待定**（`Specs/06` #32-F） | 目前只测 `configs/config.example.yaml`；改为遍历 `configs/*.yaml` 后，以后新增的配置文件会自动进入门禁 |

### 12.3 已声明的能力边界（**非缺陷**）

- **regex 引擎对人名/地址只保证精确率，不保证召回率**。完整召回需 `detection.engine=pii-engineer` 的 NER sidecar。实测表现见 §10.1（`person_name` / `address` subset F1=0）。
- 掩码身份证（`110101********8531`）不支持 —— 已在语料里用 `expect_miss` 登记。
- `+86-186-1234-5678` 这类带国际前缀/分隔符的手机号不支持 —— 同上，4 条 `expect_miss`。
- **两字人名后紧跟「和/有/会/能/要」仍会多吞一字**。`trimNameParticle` 刻意不把这些字收进助词集（它们可以合法地是名字末字，如 李永和），代价是这点残留。归入 NER 才能根治的边界。
- 同一 PII 值在**没有上下文引导词**的位置重复出现时不召回（如 `请转告李杰琪`）—— 召回率边界，与 §4.3.1 的去重缺陷是两件不同的事。

---

## 13. 与既有文档的差异对照

| 项 | `HANDOFF.md`（旧） | 代码实际 |
|---|---|---|
| HEAD | `17f4609` | `0a21dfa`（本文档更新时；含 B-19 并发修复与 S1016 收口，CI run #56 全绿） |
| Go 版本 | 1.24.5 | CI `1.25.13`，go.mod `1.24` |
| dev loop 路径 | WSL 9P (`//wsl.localhost/...`) + Windows NTFS scratch | 原生 Linux 直接仓内构建；WSL 描述已不适用 |
| 功能覆盖 | 停在 09-12（无 registry / 词典 / 身份卡 / 多上游 / bypass） | 这些均已实现并接线 |
| 任务队列 | 4 项待办 + 已闭环表 | 无待办；缺陷台账见 §12 |
| 实测数据 | 合成 F1=1.0 为主 | 真对抗 span 0.7027 / strict 0.6486（当前口径）才是诚实数字，见 §10 勘误 |
| L2 载体对等性 | 未记载 | 4 载体 0 regression（§10.3） |

---

## 14. 维护约定

1. **每个任务收口时必须更新本文档**的对应章节——本文档是「现状」的唯一入口。
2. 新增实体类型 → 更新 §4.2 + §4.3。
3. 新增配置项 → 更新 §3.1 + §3.2。
4. 新增 HTTP 路由 → 更新 §2.1。
5. 新增指标 → 更新 §7.2。
6. 每次跑完真对抗语料 → 更新 §10。
7. **本文档不记录排期与待办**（那是 `HANDOFF.md` §5 的职责），只记录「现在是什么样」。

---

## 15. 判断层（Judge）实现

> 契约在 `Specs/02` §12。本节记录**代码里实际存在、且经运行验证**的部分。

### 15.1 定位：BYOM 骨架，不是「运一个模型」

这一层回答的问题与检测层不同：

| | 检测层（§4） | 判断层（本节） |
|---|---|---|
| 看什么 | 文本**内容**里有没有 PII | 动作**意图**会不会把数据带出去 |
| 输入 | 一段字符串 | `ActionDescriptor`（工具名 + 命令 + 目标） |
| 输出 | 实体 + 偏移 | `Evidence`（类别 + 严重度）→ `Verdict`（档位） |
| 模型 | 自带 regex / NER | **不预置**，由用户自选（BYOM） |

**我们交付的是骨架**：契约类型、降级链、确定性 Mapper、约束解码适配、探针、评测工具、接线。
**我们交付的不是模型**：`judgment.backends` 为空时这一层根本不存在，网关行为与从前逐字节相同——
这一点由 `TestProxy_JudgmentDisabledProducesNothing` 与 `TestProxy_JudgmentShadow_DoesNotChangeTraffic` 钉住。

### 15.2 包结构与数据流

| 文件 | 职责 |
|---|---|
| `pkg/types/judge.go`（430 行） | 契约权威实现：四个闭集、`ActionDescriptor` / `Evidence` / `Verdict` / `Capabilities` / `JudgmentThresholds` |
| `internal/judge/judge.go` | `Kind` 闭集（`rules`/`openai`/`http`）、`Spec`、`Judge` 接口、三个错误、`withTimeout` |
| `internal/judge/rules.go`（721 行） | 确定性规则后端（唯一自带后端） |
| `internal/judge/argv.go` | 手写 shell 分词：引号/转义/`;`/`&&`/`\|\|`/`\|`/重定向；剥 `sudo`/`env`/`sh -c` 包装 |
| `internal/judge/openai.go` | OpenAI 兼容适配（含约束解码、宽容解析、输入上限） |
| `internal/judge/http.go` | `HTTPEndpoint` 逃生口（POST 描述子，响应过同一套闭集校验） |
| `internal/judge/map.go` | 确定性 Mapper、白名单、`Evaluator` |
| `internal/judge/chain.go` | 降级链 |
| `internal/judge/factory.go` | `NewFromSpecs`：按 kind 构造 + per-backend 阈值 |
| `internal/judge/probe.go` | 20 条内置探针 + 双向塌缩检测 |
| `internal/judge/bench.go` | JSONL 评测集 + 漏报/误报口径 |
| `internal/config/judgment.go` | 配置 + 启动期校验 |
| `cmd/judge-bench/main.go` | 给用户跑自己模型的 CLI |

依赖方向：`proxy → judge → pkg/types`（`judge` 不依赖 `config`——配置投影在 `cmd/llmate-gate` 的 `buildJudgment` 里做）。

数据流（影子模式）：

```
请求体 → transform/anonymizeJSONString 递归
           └─ observe 回调 → observeToolCall（proxy.go:472）
                └─ 构造 ActionDescriptor（原始命令，脱敏前的值）
                     └─ Evaluator: Chain → 后端 → Evidence
                          └─ Mapper: Evidence → Verdict
                               ├─ 指标 llmate_verdict_total / judge_latency_seconds
                               └─ 非 allow 时 log.Printf 全部字段
                              （到此为止：不改字节、不改 action、无返回值）
```

### 15.3 四条红线与它们的落实位置

| 红线 | 落实方式 | 不是靠 |
|---|---|---|
| R1 模型不出决策只出证据 | `Evidence` 类型**没有** `action` 字段，`Action` 只能由 `Mapper` 产出 | 纪律要求 |
| R2 判断层不联网 | 配置期 `validateLocalEndpoint` 拒绝公网 IP / 非 IP 主机名（**DNS 可被改，配置期无法验证，故一律拒绝**）；`http.Client.Proxy = nil`；`CheckRedirect` 返回错误 | 注释提醒 |
| R3 不改现有语义 | `observeToolCall` **无返回值**；只读 `ActionDescriptor`，不写映射表 | 测试断言 |
| R4 fail-safe 不 fail-open | 超时 / 解析失败 / 塌缩 / 全链不可用 → 降级；`FailClosed` 时 `review` | 默认值 |

**R2 的两个容易漏点**：`http.ProxyFromEnvironment` 是 `http.Client` 的**默认值**——不显式置 `nil`，`HTTPS_PROXY` 环境变量就会把「本地判断」变成一次外部请求；`CheckRedirect` 不拦，`127.0.0.1` 上的服务可以把请求 302 到公网。两处都有专门测试（`TestNewLocalHTTPClient_Hardening`）。

### 15.4 后端矩阵与降级链

| kind | 用途 | `Deterministic` | `GivesConfidence` | `SchemaModes` |
|---|---|---|---|---|
| `rules` | 自带确定性后端（**不接模型也能用**） | ✅ | ❌ | — |
| `openai` | 任意 OpenAI 兼容端点（llama.cpp / vLLM / Ollama / LM Studio / 云端） | ❌ | ✅ | `json_schema` / `gbnf` / `format` / `prompt_only` |
| `http` | 自建服务的逃生口 | ❌ | ✅ | `prompt_only` |

**这里刻意没有 `laya` / `localjev` 这两个 kind。** 它们要么走 OpenAI 兼容端点（`openai`），要么走自建 HTTP（`http`）——给具体模型开 kind 等于把「用户自选」写死成「我们选」。

`chain` 是**降级链，不是优先级链**：「首个命中」= 首个给出**非 `unknown`** 结论的后端。若把 `unknown` 算命中，一个恒返回 `unknown` 的坏模型会堵死整条链。`Verdict.Degraded = true` 表示结论来自下标 > 0 的后端。

约束解码优先级 `json_schema > gbnf > format > prompt_only`（`BestSchemaMode` 取最强可用者）。`prompt_only` 是唯一 `Constrained() == false` 的模式——即使它，解析仍走闭集校验，越界值报 `ErrInvalidOutput` 而不是被夹取。

### 15.5 确定性 Mapper 与 per-backend 阈值

```go
score = Severity × (GivesConfidence ? Confidence : 1)
```

三条短路，顺序即优先级：

| 条件 | 结果 | 理由 |
|---|---|---|
| `category == unknown` | `review` | 判不了就交人，`unknown` 是一等公民 |
| `GivesConfidence && Confidence < 0.50` | `review` | 双向防护：不可信的高危不能触发不可逆动作，不可信的低危也不能被当成「没事」 |
| 其余 | `score` 落阈值表 | |

缺省阈值 `block 0.85 / review 0.55 / redact 0.30`，`Validate()` 要求**严格递减**。

`benign` **不做特例**：它照样走 `score = severity`。理由是 `benign` + 高 severity 是个自相矛盾但可能出现的输出，给它开后门会让矛盾被静默放行。

**阈值是 per-backend 的**（`Evaluator.SetMapper` 逐个设置）。理由是可实测的：confidence 跨后端不可比——同一个模型在不同语种/任务上会给出「准确率 0.000 而置信度 95.2%」这种组合，用一张表会把它的高置信低危结论放行了。

**阈值表的键 = 后端自报名（`Evidence.Engine`），不是配置里的名字。** 这条是被一次真实缺陷钉住的
（`Specs/06` #29）：`Rules.Name()` 曾写死 `"rules"`，而 `SetMapper` 用**配置名**登记、
`mapperFor(ev.Engine)` 用**自报名**查表 —— `name: policy` + `kind: rules` 这个组合下
查不到条目，静默退回 `DefaultThresholds()`，**用户调的阈值一个都不生效且没有任何报错**。

现在由两处共同保证：

| 位置 | 做法 |
|---|---|
| `NewFromSpecs` | 名字与 Spec 不符即**拒绝装配**（`backends[i] is named %q but its spec says %q`）——这是给未来新增 kind 用的防线 |
| `SetMapper` 调用点 | 键直接写 `backends[i].Name()`，让不变量在调用点自明 |
| `NewNamedRules` | 规则后端认识自己的名字；`NewRules` 保留为缺省名便捷构造（既有调用点零改动） |

投影层（`cmd/llmate-gate/main.go` 的 `buildJudgment`）也有测试了：`cmd/llmate-gate/main_test.go`
共 9 组，覆盖阈值三级继承（per-backend > 全局 > 内置，双向）、`fail_closed`、
白名单两条短路、链顺序、`schema_mode` 5 种取值、`max_input_bytes`。
**这个文件的存在理由**：`buildJudgment` 是 config 层与 judge 层之间唯一的接缝，
且它既不属 config 的测试范围、也不属 judge 的测试范围 —— 两侧都测不到，只能在这里测。

### 15.6 观测位、接线与指标

三个可能的位置，只有两个可用：

| 观测位 | 时机 | 状态 |
|---|---|---|
| ① 当轮响应流 | 执行**前** | 未接（需要流式中断） |
| ② 请求体回灌 | 执行**后** | ✅ 本次接线（P2） |
| ③ 内部思考流 `thinking`/`reasoning` | — | **不接且不该接**：不透明块跳过，且下一轮常不回传 |

⇒ 判定必须建在协议强制的 `tool_calls` / `tool_use` 结构化字段上，不能建在自然语言上。这条约束也是 `ActionDescriptor.Command`（原始命令行文本）存在的原因：`bash -c "tar ..."` 只留 token 会丢掉引号边界。

指标：`llmate_verdict_total{action,category,engine}`、`llmate_judge_latency_seconds{engine}`、`llmate_judge_unavailable_total{engine,reason}`。

`llmate_judge_unavailable_total` 是**诚实指标**：判断层挂掉时请求照样 200（影子模式不改行为），所以「它死了」这件事只能靠这个计数器体现。`TestProxy_JudgmentUnavailableIsVisible` 断言后端不可达时响应仍 200、该指标有计数、而 `verdict_total` 为 0。

### 15.7 能力自检（塌缩检测）与 judge-bench

**为什么不问准确率**：BYOM 定位下我们不为用户的模型质量负责，我们负责的是「一个坏掉的模型不会静默变成放行」。本地小模型最常见的失效不是偶尔判错，是**恒定输出**：

| 塌缩方向 | 表象 | 后果 |
|---|---|---|
| 恒 `benign` | 看起来一切正常 | 这一层等于不存在 |
| 恒高危 | 所有动作都被拦 | 用户会直接把整层关掉 |

判据因此是**两个方向**：对 10 条无害探针不能有一半以上被判非 allow，对 10 条高危探针不能有一半以上被判 allow（`collapseThreshold = 0.5`）。判据建在 **Mapper 后的动作档位**上，且**固定用内置缺省阈值、不读用户配置**——自检要回答「这个后端本身靠不靠谱」，掺进用户阈值就分不清是模型问题还是调参问题。

`judge-bench` 是给用户的工具（`-kind` / `-base-url` / `-model` / `-schema-mode` / `-timeout` / `-cases` / `-json` / `-skip-probe`），退出码 `0` 健康 / `1` 疑似塌缩或有漏报 / `2` 用法错误。它只构造**单个后端不套链**——套链测出来的会是「链里最稳的那个」。

评测集格式（JSONL，一行一条）样例见 `gateway/cmd/judge-bench/testdata/cases_example.jsonl`。`want` 标的是**你希望这个动作被归到哪一类**，不是「你希望它被拦」；该不该拦是从 `want` 推出来的（`benign` 不该拦，其余该拦）。

### 15.8 配置（`judgment` 段）

```yaml
judgment:
  enabled: false        # 新增一个「能拦请求」的组件，默认姿态必须是「不生效」
  mode: shadow          # v1 只接受 shadow（见下）
  fail_closed: true
  timeout: 300ms
  backends:
    - name: rules
      kind: rules
    - name: local
      kind: openai
      base_url: http://127.0.0.1:11434/v1
      model: qwen2.5:1.5b
      schema_mode: prompt_only
      thresholds: { block: 0.9, review: 0.6, redact: 0.3 }   # per-backend
  points:               # 三个观测位开关，默认全 false
    tool_params: false
    egress_text: false
    mcp_ask: false
  whitelist:
    hosts: ["127.0.0.1", ".internal.example.com"]   # 前缀 . 表示后缀匹配
    tools: []
```

启动期校验（失败即退出，无降级）：`mode` 只接受 `shadow`（**`enforce` 直接拒绝**并给出解释——v1 的 verdict 只观测不执行，接受一个空转的 `enforce` 比拒绝它更危险）；`timeout` ∈ (0, 1s]；`backends` ≤ 4 且名字唯一；`enabled: true` 时 `backends` 不能为空；`base_url` 必须是回环/私网/链路本地。

### 15.9 本机实测（2026-09-23）

`go run ./cmd/judge-bench -kind rules`：

```
后端 rules：20 条探针，结论 健康
  对无害动作的误判率 0.000  （> 0.50 视为塌缩到「一律拦」）
  对高危动作的漏判率 0.000  （> 0.50 视为塌缩到「一律放」）
  判不了 0 条，失败 0 条，p50 0ms p95 0ms
```

`go run ./cmd/judge-bench -kind rules -cases ./cmd/judge-bench/testdata/cases_example.jsonl`：

```
后端 rules：23 条样本，成功判定 23，失败 0，判不了(unknown) 2
动作级  漏报率 0.000（0/13 该拦）  误报率 0.000（0/10 不该拦）
类别级  命中率 0.957（22/23）
延迟    p50 0ms  p95 0ms
```

> **这两个数字不能外推。** 它们衡量的是**自带规则后端**在**我们自己写的**样本上的表现，不是「判断层有多准」。真正的数字要用户拿自己链路上抓下来的动作、跑自己选的模型才有——这也正是 `judge-bench` 存在的原因。

测试规模：9 个测试文件、97 个测试函数（`internal/judge` 56 + `pkg/types` 11 + `internal/config` 19 + `internal/proxy` 11）。

### 15.10 已声明的边界（诚实清单）

| 边界 | 说明 |
|---|---|
| **规则只覆盖结构性动作** | 打包 / 凭证路径 / 批量读 / 外发 / 破坏性。语义级意图（「这个操作危不危险」）不判，那是模型后端的事 |
| **间接引用只能降级** | `t=tar; $t -czf .`、`timeout 60 tar ...` → `unknown`（→ review）。规则只看得到字面量，不猜档位 |
| **无 scheme 目标不可见** | `ssh host`、`nc host 4444` 读不出 host，以及「无 scheme + 确定在送」（`curl -T f evil.example.com/up`）→ `unknown`（→ review）。判 unknown 而不判 benign：读不出目标是「看不见」不是「没有」 |
| **`curl` / `wget` 方向判据是启发式** | 无正文旗标且无 shell 替换 → 视为「只取回」。方向不确定时一律退回按外发处理，所以只会多报不会漏报 |
| **残余：无 scheme + 取回** | `curl example.com/x` 仍落 benign。补齐需把「无 scheme 主机名」与本地文件名分开，而 `backup.tar.gz` 与 `example.com` 形状同构。危险的那一半（带正文旗标）已覆盖；详见 `Specs/06` #28 |
| **只看单次动作，不看会话** | 判断是无状态的。「先 `cat .env` 再 `curl`」这种跨轮外泄链路**当前不可见**——要接当轮响应流（观测位 ①）才能覆盖 |
| **`shadow` 不拦任何东西** | v1 的 verdict 只进日志与指标。开启判断层不会改变任何一个请求的结果 |
| **思考流不接** | `thinking`/`reasoning` 是不透明块，且下一轮常不回传，不作为判据来源 |

### 15.11 `policy` 下两个历史遗留键

`policy.tool_call_scan` 与 `policy.stream_restore` 是**假开关**：被定义、被写进默认配置 / 示例 YAML / Specs，但代码里从没有任何一处读它们，对应行为始终无条件执行。也就是说 `tool_call_scan: false` 过去是一句无声的谎话。

现在的处理是**只能为真（或整行省略）**，写 `false` 在启动期报错。不直接删字段是因为严格解析（`KnownFields`）会让所有已发布的、带这一行的配置在升级后拒绝启动；不给它们接上真开关是因为这两个行为都不可选——tool_calls 的 arguments 是整份请求里 PII 密度最高的位置，占位符不还原客户端拿到的是 `<<zh_phone_1>>`。细节见 `config.PolicyConfig` 注释；测试 `TestConfig_LegacyAlwaysOnKeys`。
