# LLMate Gate 实现规格（AS-BUILT）

> **文档版本**：v1.0（2026-09-15）
> **层级**：L2-AsBuilt（实现现状规格）
> **取证基线**：`origin/main @ c64f246`，工作树干净
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
├─ cmd/                      5 个入口
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
│  ├─ pipeline/  编排器：串起 检测→替换→存储→还原
│  ├─ proxy/     HTTP 反向代理：多协议上游路由、tool_call 扫描、流式还原
│  ├─ server/    HTTP 服务层：路由、鉴权、healthz、metrics
│  ├─ audit/     结构化审计（JSON Lines + 内存环 200 条）
│  ├─ metrics/   16 个 Prometheus 指标
│  └─ errors/    错误码
├─ pkg/
│  ├─ types/     跨包共享类型 + 15 类实体权威表
│  ├─ cn/        中文实体校验（身份证校验位、手机号段、Luhn）
│  └─ global/    国际实体校验（URL / US SSN / 信用卡）
├─ debug/        内嵌面板：Hub(WS) + Store(环) + Handler(API) + assets
└─ configs/      示例与冒烟配置
```

依赖方向单向：`cmd → server/proxy → pipeline → {detector, replacer, vault, cache, audit, metrics}`；`registry` 与 `circuit` 以装饰器方式包在 `detector` 外层（`registry.Wrap` → `circuit.NewGuardedClient`），不侵入检测器内部。

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

来源：`internal/server/server.go:61-74`、`debug/handler.go:101-110`。

| 方法 | 路径 | 处理 | 备注 |
|---|---|---|---|
| POST | `/v1/chat/completions` | 代理 + 脱敏/还原 | OpenAI 协议 |
| POST | `/v1/completions` | 同上 | |
| POST | `/v1/embeddings` | 同上 | 请求侧脱敏 |
| POST | `/v1/responses` | 同上 | OpenAI Responses API |
| POST | `/v1/messages` | 同上 | **Anthropic 协议**，自动选 anthropic 上游 |
| ANY | `/v1/models` | 纯透传 | 无需脱敏 |
| GET | `/healthz` | 健康检查 | 检测器不可用时返回 503 `degraded` |
| GET | `/metrics` | Prometheus | |
| POST | `/v1/privacy/redact` | 递归脱敏 | **常驻隐私 API，不受 `--no-debug` 门控** |
| POST | `/v1/privacy/restore` | 按 request_id 还原 | 同上 |
| GET | `/_debug` `/_debug/*` | 调试面板 | 仅回环 |
| GET | `/ws/events` | 面板实时事件 | 仅回环 |
| POST | `/_api/traffic` | 流量查询 | 仅回环 |
| POST | `/_api/detect` | 单段检测 | 仅回环 |
| POST | `/_api/replace` | 试替换 | 仅回环 |
| GET/POST | `/_api/rules` | 策略热切换 | 仅回环 |
| GET/POST | `/_api/dictionary` | 仿真词典读写（热加载） | 仅回环 |
| GET/POST | `/_api/registry` | 登记表读写（热加载） | 仅回环 |
| GET | `/_api/audit` | 审计查询 | 仅回环 |

`/_api/*` 与 `/_debug` 全部经 `loopbackOnly` 包装（`debug/handler.go`）：即使网关监听了 `0.0.0.0`，面板也不对外。早期存在的 `debug_bind` 配置项已删除（`04982a8`），因为「可配置外部绑定」与「隐私网关」的定位冲突。

### 2.2 鉴权

`server.go:137-165`。`gateway.auth_token` 为空则完全不鉴权；非空时接受两种凭证：

- `Authorization: Bearer <token>`
- `X-Api-Key: <token>`

不匹配返回 401 + `{"error":{"code":"unauthorized","message":...}}`。

> 注意：鉴权中间件**覆盖全部路由**，包括常驻隐私 API。这是刻意的——该 API 能读回明文 PII。

### 2.3 隐私 API 契约

`/v1/privacy/redact` 支持三种形态（`proxy/privacy.go:99-235`）：

| 入参 | 行为 |
|---|---|
| `{"text": "..."}` | 单段文本脱敏 |
| `{"json": {...}}` | 递归遍历 JSON，只对字符串**值**脱敏，键永不脱敏 |
| `{"gate_only": true}` | **只扫描不改写**：返回 `has_pii` / `entities` / `blocked`，不建 request_id |

`gate_only` 是给 Claude Code hooks / MCP 这类「先看 PII 再决定拦放」的边界用的，`block_types` 可指定「只对哪些类型判定 blocked」，留空表示任意 PII 命中即 blocked。

返回 `request_id` 后，`/v1/privacy/restore` 可多次还原同一 `request_id`（映射表按 id 存放，不是一次性）。

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
  request_timeout: "30s"
  debug: true
  log_level: "info"

detection:
  engine: "regex"              # regex | pii-engineer
  fallback_regex: true         # 格式固定实体走正则加速，不进模型
  sidecar:
    command: "cargo run --release"
    endpoint: "http://127.0.0.1:8000"
    healthz: "/healthz"
    start_timeout: "60s"
    restart_limit: 3
    auto_start: false
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
  fail_closed: true
  tool_call_scan: true
  stream_restore: true

vault:
  path: "./vault_data"
  encryption: "aes-256-gcm"
  key_derivation: "scrypt"
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

`fallback_regex: true` 的含义是「格式固定实体走正则加速通道，不进模型」——即模型只负责弱格式实体。

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
| `reDate` | 日期 | 支持 `-/.年` 分隔 |
| `rePlate` / `rePlateEN` / `rePlateCA` | 车牌 | 中文严格；英文/加州模式**必须带引导词**（弱格式实体策略） |
| `reURL` | URL | 结尾不允许句尾标点 |
| `reUSSSN` | 美国 SSN | 形态 + `global.ValidUSSSN` 二次过滤 |
| `reAPIKey` / `reJWT` / `reSecretKV` | 密钥类 | 不可逆处理 |
| `reNameCtx` / `reNameTile` | 中文人名 | **弱格式实体：只做高精度低召回** —— 必须有强上下文引导词（我叫/姓名/联系人…）或称谓后缀（先生/女士…） |
| `reAddress` | 中文地址 | 必须命中行政区划链：省/自治区 **或直辖市** → 市/区/县 → 路/街/巷 → 门牌 |

RE2 不支持 lookaround，数字边界用手工邻字符检查替代（`hasDigitNeighbor` / `hasIDNeighbor`）。

> **正则引擎的能力边界（重要）**：人名与地址在 regex 引擎下**只保证精确率，不保证召回率**。源码注释直接写明：「完整召回依赖 `detection.engine=pii-engineer` 的 NER 模型」。这是 §12 已知缺陷里 `zh_person_name` / `zh_address` 真对抗 F1=0 的根因，不是 bug，是**已声明的设计边界**。

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

### 5.3 Vault

`vault.MemVault`：AES-256-GCM 加密 + scrypt 派生 + TTL 清扫（每分钟）+ `persist` 控制是否落盘。`Delete` 会先 **zeroize** 密钥材料。密钥随机生成不落盘（进程重启即失效，`persist=false` 时映射表也不留）。

### 5.4 流式还原（SSE）

`replacer.StreamRestorer`：trie 结构 + `isPrefixOfAny` 前缀判断。核心难点是**占位符可能被切开跨越 SSE 事件边界 / 跨越 TCP 分片**——还原器必须识别「当前缓冲是某个哨兵的前缀」并继续等待，而不是立即输出。

`SSERestorer` 在其上再包一层：按 SSE 事件解析，只对 `data:` 行的 JSON 字符串值做还原，保留非 data 行、保留数字类型不动（`TestSSERestorer_PreservesNumbers` / `_NonTextKeysUntouched`）。

残留不完整占位符计入 `llmate_stream_orphan_placeholders_total`。

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

`policy.tool_call_scan` 控制。递归遍历请求体，对 `tool_calls` 的 `arguments` 逐个值做检测替换；支持嵌套结构；**JSON 字符串里内嵌的 PII 也会被处理**（`transform` / `anonymizeJSONString`，`proxy.go:306-401`）。

有一个已修复的坑值得留档（`07481b0`）：`tool_call.arguments` 脱敏后必须**仍是 JSON 字符串**，不能变成对象——否则上游 API 直接报 400。

Anthropic 的 `tool_use.input` 结构不同（在 `content` 列表里嵌 dict，且 `type` 字段不是 PII 字段名），有单独的白名单处理（`proxy.go:80-104`）。

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

### 7.2 Prometheus 指标（16 个）

`llmate_requests_total{endpoint,outcome}`、`llmate_detect_latency_seconds{engine}`、`llmate_replace_total{fate}`、`llmate_restore_total{endpoint}`、`llmate_blocked_total{reason}`（即 fail-closed 阻断）、`llmate_upstream_errors_total{endpoint,status}`、`llmate_stream_orphan_placeholders_total{endpoint}`、`llmate_detect_cache_hits_total{conversation}`、`llmate_detect_cache_misses_total{conversation}`、`llmate_detect_incremental_segments_total{action}`、`llmate_vault_size`、`llmate_active_streams`、`llmate_pii_detected_total{entity_type,fate}`、`llmate_tool_calls_scanned_total`、`llmate_request_total_latency_seconds{endpoint}`、`llmate_response_restore_latency_seconds{endpoint}`。

延迟类指标**单位统一为秒**（Prometheus 惯例）。指标名属公共接口，改名会破坏已对接的 Grafana/告警，故冻结。

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
| Linux/macOS 安装 | `scripts/install.sh` | 装 `~/.local/bin`，注册 systemd --user / launchd |
| Windows 安装 | `scripts/install.ps1` | 装 `%LOCALAPPDATA%\Programs`，注册计划任务 + 桌面快捷方式 |
| 包管理 | `scoop-bucket/llmate-gate.json` | Windows amd64 + arm64 双入口 |
| 发布 | `.github/workflows/release.yml` | tag `v*.*.*` 触发 → 6 平台交叉编译 → 建 Release + 上传产物；支持 `workflow_dispatch` 手动重发 |

---

## 9. 质量门禁与测试

### 9.1 CI（9 个 job）

`verify`（vet + `go test -race`）/ `build` / `e2e`（E1-E8）/ `ui-smoke`（D1-D6）/ `coverage`（总门槛 + 逐包门槛）/ `bench`（fixture 校验）/ `bench-baseline`（真实引擎召回）/ `lint`（golangci-lint，8 个 linter，0 issues）/ `perf`（8 个 Go benchmark，min-of-6，容差 1.25×）/ `vuln`（govulncheck）。

Go 版本：`GO_VERSION: '1.25.13'`（CI 权威口径；`gateway/go.mod` 声明 `go 1.24`）。

### 9.2 端到端

- `e2e/e2e.sh`：387 行，断言 E1-E8（契约级）
- `e2e/ui_smoke.sh`：256 行，断言 D1-D6（面板）
- `e2e/latency.sh` + `latency_client.py`：多轮 P99 基准（纯标准库）

---

## 10. 实测数据（本机实跑，2026-09-15）

本机（原生 Linux / arm64，Go 1.25.13）编译并运行网关后，用 `cn-pii-bench/runner.py` 实跑三个语料：

| 语料 | 条数 | P | R | F1 | TP | FP | FN | p50/p95/p99 |
|---|---|---|---|---|---|---|---|---|
| 真对抗 `cases_adversarial.jsonl` | 28 | **0.9565** | **0.6111** | **0.7458** | 22 | 1 | 14 | 0/1/8 ms |
| 合成中文 `cases.jsonl` | 240 | 1.0000 | 1.0000 | 1.0000 | 360 | 0 | 0 | 0/1/2 ms |
| 英文基线 `cases_en.jsonl` | 180 | 1.0000 | 1.0000 | 1.0000 | 330 | 0 | 0 | 0/1/4 ms |

**真对抗 F1=0.7458 与仓库内报告 `reports/adversarial_20260912-195233.md` 完全一致**，说明该数字可复现、可信。

### 10.1 真对抗 by-subset（诚实口径）

| subset | n | P | R | F1 | 结论 |
|---|---|---|---|---|---|
| email | 4 | 1.0 | 1.0 | 1.0 | 含 `+` 标签邮箱通过 |
| ip_address | 2 | 1.0 | 1.0 | 1.0 | 带端口通过 |
| phone | 9 | 1.0 | 1.0 | 1.0 | emoji 相邻、无分隔符均通过 |
| mixed | 2 | 1.0 | 0.75 | 0.857 | 人名漏检拖累 |
| tool_call | 2 | 1.0 | 0.5 | 0.667 | 人名漏检拖累 |
| **person_name** | 4 | — | 0 | **0** | 无上下文引导词时完全不召回 |
| **address** | 4 | — | 0 | **0** | 缺省级前缀 / 园区式地址不匹配 |
| **id_card_masked** | 1 | 0 | 0 | **0** | 掩码形态（`*`）不支持，且误报 1 条 |

> **合成语料 F1=1.0 不是对外宣称值。** 在自作者合成语料上自评，F1 高只说明「检测器与生成器对同一套仿真规则达成一致」，不构成真实场景结论（`Specs/03` §4.3.4 已明确）。诚实数字是 0.7458。

---

## 11. 能力矩阵

| 能力 | 状态 | 证据 |
|---|---|---|
| OpenAI 兼容代理（5 端点） | ✅ 已实现 | `server.go:62-66` |
| Anthropic 兼容（`/v1/messages`） | ✅ 已实现 | `proxy.go:562-573` |
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
| 递归 tool_call 参数扫描 | ✅ 已实现 | `proxy.go:306-401` |
| SSE 跨事件边界还原 | ✅ 已实现 | `replacer/sse.go` |
| Merkle 增量检测 | ✅ 已实现 | `cache/merkle.go` |
| 熔断 + fail-closed | ✅ 已实现 | `circuit/` |
| 结构化审计 + 环 + 查询 | ✅ 已实现 | `audit/` |
| 16 个 Prometheus 指标 | ✅ 已实现 | `metrics/` |
| 调试面板（4 Tab + 6 API） | ✅ 已实现 | `debug/` |
| MCP 3 工具 | ✅ 已实现 | `cmd/mcp-server` |
| Claude Code hooks | ✅ 已实现 | `hooks/` |
| VS Code 扩展 | ✅ 已实现 | `vscode-ext/` |
| Linux 安装脚本 | ⚠️ 真机跑通，但含 4 个缺陷 | 见 §12 |
| Windows 安装脚本 | ✅ 已真机冒烟（`0cea49b`） | |
| NER 检测（pii-engineer） | ⚠️ 客户端已实现，sidecar 为 mock | `piiengineer.go` 就绪，无真实服务 |
| Homebrew tap | ❌ 未建 | |
| issue/PR 模板 | ❌ 未建 | |
| Dockerfile / 服务端托管 | ❌ 明确不做 | 用户拍板（2026-09-10） |
| Tauri 桌面 UI / systray | ❌ 已放弃 | CGO 与纯 Go 交叉编译冲突 |

---

## 12. 已知缺陷（本次真机冒烟新发现）

上一版交接包把「Linux/macOS `install.sh` 真机冒烟」列为唯一待办。本次在原生 Linux 上执行后，发现 4 个此前从未暴露的缺陷。详见 `Specs/06-代码审阅-优化点清单.md`。

摘要：

| # | 缺陷 | 严重度 |
|---|---|---|
| 1 | `install.sh` / `install.ps1` 生成的 config.yaml **schema 错误**（顶层 `upstream`/`detector`/`replacer` 三个键都不存在） | 🔴 高 |
| 2 | `config.Load` 未启用严格模式，**未知键被静默忽略**，导致 #1 无法被用户察觉 | 🔴 高 |
| 3 | `--dry-run` 仍会真实写入 config.yaml（该写操作未走 `run()` 包装） | 🟡 中 |
| 4 | `--uninstall` 不删 binary/配置；且 `set -e` 下 `systemctl` 一失败就中断 | 🟡 中 |
| 5 | `--port` 与已存在 config.yaml 不一致时，启动端口 ≠ 健康检查端口 → 假失败 | 🟡 中 |

其余已知能力边界（**非缺陷，是已声明的设计边界**）：

- regex 引擎对人名/地址只保证精确率；完整召回需 NER sidecar
- 掩码身份证（`110101********8531`）不支持
- `+86-186-1234-5678` 这类带国际前缀/分隔符的手机号不支持

---

## 13. 与既有文档的差异对照

| 项 | `HANDOFF.md`（旧） | 代码实际 |
|---|---|---|
| HEAD | `17f4609` | `c64f246` |
| Go 版本 | 1.24.5 | CI `1.25.13`，go.mod `1.24` |
| dev loop 路径 | WSL 9P (`//wsl.localhost/...`) + Windows NTFS scratch | 原生 Linux 直接仓内构建；WSL 描述已不适用 |
| 功能覆盖 | 停在 09-12（无 registry / 词典 / 身份卡 / 多上游 / bypass） | 这些均已实现并接线 |
| 任务队列 | 4 项待办 + 已闭环表 | 仅 1 项真机冒烟，且已执行（发现 4 缺陷） |
| 实测数据 | 合成 F1=1.0 为主 | 真对抗 0.7458 才是诚实口径 |

---

## 14. 维护约定

1. **每个任务收口时必须更新本文档**的对应章节——本文档是「现状」的唯一入口。
2. 新增实体类型 → 更新 §4.2 + §4.3。
3. 新增配置项 → 更新 §3.1 + §3.2。
4. 新增 HTTP 路由 → 更新 §2.1。
5. 新增指标 → 更新 §7.2。
6. 每次跑完真对抗语料 → 更新 §10。
7. **本文档不记录排期与待办**（那是 `HANDOFF.md` §5 的职责），只记录「现在是什么样」。
