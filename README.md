# 🛡️ LLMate Gate

中文一等公民的 LLM Agent 隐私网关 —— 为大模型 API 调用提供 PII 检测、中文仿真替换、tool-call 参数扫描、流式还原与可审计合规。

> Your LLM's privacy gatekeeper for the Chinese-speaking world.

## 🎯 为什么需要 LLMate Gate

把用户原文直接发给大模型，风险来自三个层面：传输与存储（数据离开你的可控边界）、可观测性副作用（Langfuse/ELK 等日志系统成为巨大 PII 泄露面）、模型输出反向泄露（模型可能在回答里复述、拼接甚至"脑补"出用户的敏感信息）。

合规压力是实打实的：GDPR 违规最高可罚全球年营收的 4%；在国内，《个人信息保护法》(PIPL)、《数据安全法》(DSL) 以及 2026 年 1 月起大幅修订生效的《网络安全法》(CSL) 共同构成三支柱框架。

> ⚠️ PIPL 对"匿名化"和"去标识化"有明确区分：无法识别且不可复原的匿名化数据不在监管范围内，而仅做了去标识化（理论上仍可重新关联）的数据依然在监管范围内。这个区别直接决定了该选"不可逆脱敏"还是"可逆脱敏 + 金库"的技术方案。

现有方案的两个盲区：

1. 中文不是主流隐私网关的主场。微软 Presidio 等框架的开箱模型和内置实体，基本为英文和西方格式设计——中文人名、身份证号、手机号，它默认并不认识。
2. 占位符替换破坏 Agent 语义。把"我的血压 160/110"打成"我的   "后，云端模型只看到一片空白，Agent 的长期个性化能力基本丧失。

LLMate Gate 的定位：在中文语境下，用格式保持的仿真替换（"张三"→"李雷"，"13800138000"→"13900139000"）替代无意义的打码——既把明文锁在本地，又把语义留给模型。

## ✨ 核心特性

- 🇨🇳 **中文一等公民**：集成 PII Engineer（基于 GLiNER2 / mDeBERTa-v3-base，中文 PII 检测 F1 0.918，CPU-only ONNX Runtime 推理，~180ms/条），专门覆盖中文姓名、身份证号（18 位 + 校验位）、手机号（三大运营商号段）、银行卡（Luhn 校验）、中文地址等实体。
- 🎭 **中文格式保持仿真替换**：v1 占位符替换 → v1.1 仿真替换。生成的假数据保持原格式、语义类型、性别/长度一致，云端模型仍能理解上下文。
- 🔌 **OpenAI 兼容透明代理**：默认监听 :8400，Cursor / Continue / Claude Code / 任意 OpenAI SDK 改一行 base_url 即可接入。
- 🤖 **Agent tool-call 参数扫描**：递归扫描 tool_calls[].function.arguments 里的所有字符串值，按参数类型差异化处理。
- 🌊 **流式还原**：SSE 流式响应场景下，用 trie 缓冲做边界对齐，保证占位符在流中被完整还原。
- 🧠 **detection_cache**：绑定 conversation_id 的增量检测缓存，避免 Agent 多轮对话里对同一段 PII 重复检测，将 40s+ 的延迟降到 P99 < 2s。
- 🔒 **Fail-closed**：检测引擎异常即阻断请求，绝不"裸奔"放行。
- 🔍 **可审计**：每一次脱敏/还原都生成结构化审计日志，可导出合规报告（对标 PIPL / GDPR / 等保 2.0）。
- 🖥️ **VS Code 原生扩展 + Claude Code hooks**：编辑器内高亮 + 自动配置 base_url；Claude Code Pre-tool/Post-tool 拦截。
- 🪶 **轻量桌面 UI（Tauri）**：桌面端可视化管理检测规则、查看审计面板。
- 📊 **cn-pii-bench**：我们开源的中文 PII 检测基准数据集 + 测评框架，覆盖中文姓名/身份证/手机/银行卡/地址 + tool-call 参数 + 中英混合文本。

## 🏗️ 架构

```text
┌──────────────────────────────────────────────────────────────┐
│  Agent / IDE / SDK                                            │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────┐   │
│  │ VS Code 扩展  │  │ Claude Code  │  │  OpenAI SDK /      │  │
│  │              │  │ (hooks)      │  │  Cursor / Continue │  │
│  └──────┬───────┘  └──────┬───────┘  └─────────┬──────────┘   │
└─────────┼──────────────────┼──────────────────────┼────────────┘
          │                  │                     │
          ▼                  ▼                     ▼
┌──────────────────────────────────────────────────────────────┐
│  LLMate Gate  (监听 :8400, OpenAI 兼容)                        │
│                                                                │
│  ① 入口拦截 ──▶ ② PII 检测 ──▶ ③ 仿真替换 ──▶ ④ 上游转发       │
│       ▲                                                     │
│       │                                                     ▼
│  ⑧ 审计日志 ◀── ⑦ 响应还原 <── ⑥ 上游返回 <── ⑤ 流式还原       │
│                                                                │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Shared Core (Rust)                                      │  │
│  │  • PII Engineer (ONNX, 中文 F1 0.918)                  │  │
│  │  • 中文仿真替换引擎 (Faker zh + 格式校验)                │  │
│  │  • 加密映射表 (AES-256-GCM, TTL, 会话一致)               │  │
│  │  • detection_cache (Merkle 增量, conversation_id)       │  │
│  │  • fail-closed 电路断路器                                │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
          │                                     │
          ▼                                     ▼
┌──────────────────┐                ┌──────────────────────────┐
│ 上游 LLM API      │                │  本地加密 Vault           │
│ (OpenAI/Claude/   │                │  (映射表, TTL, 可选持久化) │
│  DeepSeek/...)    │                └──────────────────────────┘
└──────────────────┘
```

**请求生命周期：**

1. **入口拦截**：Agent 发出的 OpenAI 兼容请求在 :8400 被拦截
2. **PII 检测**：请求体（含 tool_calls 参数）送入 PII Engineer，中文实体 F1 0.918
3. **仿真替换**：检测到 PII 后，按实体类型做差异化处理：
   - 人名 → 中文仿真姓名（性别/长度一致）
   - 手机号 → 合法号段内的仿真号码
   - 身份证 → 校验位合法的仿真身份证
   - API Key / 密码 → 不可逆 redact
4. **上游转发**：脱敏后的请求转发到真实 LLM API
5. **响应还原**：流式响应用 trie 缓冲做边界对齐，占位符精准还原
6. **审计日志**：结构化记录每一次脱敏/还原事件

## 📦 安装

### Docker（推荐，30 秒部署）

```bash
docker run -d \
  --name llmate-gate \
  -p 8400:8400 \
  -v ./vault_data:/app/vault_data \
  -e TARGET_LLM=https://api.openai.com \
  -e GATEWAY_AUTH_TOKEN=your-secure-token \
  ghcr.io/llmate/llmate-gate:latest
```

### 从源码构建

```bash
# 代理守护进程（Go）
git clone https://github.com/llmate/llmate-gate
cd llmate-gate/gateway
go build -o llmate-gate .
./llmate-gate --listen :8400 --upstream https://api.openai.com

# 桌面 UI（Tauri，可选）
cd ../desktop
npm install
npm run tauri:build
```

### VS Code 扩展

在 VS Code 扩展市场搜索 LLMate Gate，或在 .vscode/settings.json 里配置：

```json
{
  "llmate-gate.baseUrl": "http://localhost:8400/v1",
  "llmate-gate.autoConfigure": true
}
```

## 🚀 快速开始

1. 把 Agent 的 base_url 指向 LLMate Gate：

   ```python
   from openai import OpenAI

   client = OpenAI(
       base_url="http://localhost:8400/v1",  # ← 改为 LLMate Gate
       api_key="your-api-key"
   )

   response = client.chat.completions.create(
       model="gpt-4",
       messages=[{"role": "user", "content": "帮我联系张三，他手机是13800138000，身份证110101199001011234"}]
   )
   print(response.choices[0].message.content)
   ```

2. 实际效果：

   | 阶段 | 内容 |
   | --- | --- |
   | Agent 发出 | "帮我联系张三，他手机是13800138000，身份证110101199001011234" |
   | 上游 LLM 收到 | "帮我联系李雷，他手机是13900139000，身份证310101199501011234" |
   | LLM 返回 | "已帮您联系李雷（13900139000），请确认是否预约。" |
   | Agent 收到 | "已帮您联系张三（13800138000），请确认是否预约。" |

   > 💡 云端模型看到的始终是仿真数据，但 Agent 拿到的还原文本语义完整、格式一致。

## 🔧 配置

LLMate Gate 通过环境变量或 config.yaml 配置：

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| LISTEN_PORT | 8400 | 网关监听端口 |
| TARGET_LLM | https://api.openai.com | 上游 LLM API 地址 |
| GATEWAY_AUTH_TOKEN | - | 网关客户端认证令牌 |
| VAULT_PATH | ./vault_data | 加密映射表存储路径 |
| DETECTION_CACHE | true | 开启 conversation_id 增量缓存 |
| CACHE_TTL | 30m | 映射表生存时间 |
| FAIL_CLOSED | true | 检测异常即阻断 |
| STREAMING_RESTORE | true | SSE 流式响应还原 |
| LOG_LEVEL | info | 日志级别 |
| AUDIT_LOG | true | 审计日志开关 |

### config.yaml 示例

```yaml
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"
  auth_token: "${GATEWAY_AUTH_TOKEN}"

detection:
  engine: "pii-engineer"      # 中文 NER (F1 0.918)
  fallback_regex: true        # 格式固定的实体用正则加速
  cache:
    enabled: true
    bind_conversation: true   # 绑定 conversation_id 做增量
    ttl: "30m"

replacement:
  strategy: "placeholder"     # v1: placeholder → v1.1: simulate
  simulate_zh:
    person_name: true         # 张三 → 李雷（性别/长度一致）
    phone: true               # 13800138000 → 13900139000
    id_card: true             # 校验位合法
    bank_card: true           # Luhn 校验合法
  irreversible:
    - api_key
    - password
    - token

policy:
  fail_closed: true           # 检测异常即阻断，绝不裸奔
  tool_call_scan: true        # 递归扫描 tool_calls 参数
  stream_restore: true        # trie 缓冲流式还原

audit:
  enabled: true
  export: ["pip", "gdpr"]     # 导出合规报告格式
```

## 🤖 Tool-call 参数扫描

LLMate Gate 不只处理 messages 里的文本，还递归扫描 tool_calls[].function.arguments 里的所有字符串值：

Agent 调用工具时发出的参数：

```json
{
  "tool_calls": [{
    "function": {
      "name": "search_user",
      "arguments": "{\"name\":\"张三\", \"phone\":\"13800138000\"}"
    }
  }]
}
```

上游 LLM 工具调用看到的参数：

```json
{
  "tool_calls": [{
    "function": {
      "name": "search_user",
      "arguments": "{\"name\":\"李雷\", \"phone\":\"13900139000\"}"
    }
  }]
}
```

按参数类型差异化处理：

| 参数语义 | 策略 |
| --- | --- |
| 人名 (name) | 仿真替换（性别/长度一致） |
| 路径 (path) | 仿真文件名，保留目录结构 |
| 凭证 (api_key, token) | 不可逆 redact |
| 内容 (content) | 占位符替换（内容过长不适合仿真） |
| 查询 (query) | 含 PII 则阻断或脱敏后放行 |

## 📊 支持的实体类型

### 中文实体（默认开启）

| 类型 | 格式 | 示例 |
| --- | --- | --- |
| 中文姓名 | 单姓/复姓/少数民族 | 张三、欧阳锋、阿迪力·买买提 |
| 手机号 | 1[3-9] 开头 11 位 | 13800138000 |
| 身份证号 | 18 位 + 校验位 | 110101199001011234 |
| 银行卡号 | 16-19 位 + Luhn 校验 | 6222021234567890123 |
| 中文地址 | 省市区街道门牌 | 北京市海淀区中关村大街88号 |
| 车牌 | 省份缩写 + 字母数字 | 京A12345 |
| 邮箱 | 标准格式 | zhangsan@example.com |
| 组织机构 | 公司/机构名 | 北京科技有限公司 |
| IP / URL / 日期 / 金额 / 邮编 | 标准格式 | - |

### 国际化实体

PII Engineer 支持 13+ 语言：English, Malay, Tamil, Chinese, Indonesian, Vietnamese, Thai, Hindi, Bengali, Korean, Japanese, German, French, Spanish, Portuguese, Russian, Arabic, Turkish, Polish, Dutch, Italian, Swedish 等 35+ 语言。

## 🔍 审计与合规

LLMate Gate 的审计日志是结构化的，每条记录包含：

```json
{
  "timestamp": "2026-09-09T15:30:00Z",
  "request_id": "req_a3f9b2",
  "conversation_id": "conv_8821",
  "entity_types": ["zh_person_name", "zh_phone", "zh_id_card"],
  "replacement_strategy": "simulate",
  "upstream": "openai",
  "latency_ms": 212,
  "restored": true
}
```

可导出为 PIPL / GDPR / 等保 2.0 合规报告格式。

> ⚠️ 诚实边界：LLMate Gate 定位为假名化（pseudonymization）而非匿名化（anonymization）——我们不承诺 100% 防泄漏，我们卖"可测量、可审计"。自动检测抓不全所有敏感信息，必须与注入检测、内容审计一起组成纵深防御。

## 📈 性能基准

基于 cn-pii-bench 在 2026 年 8 月的测评（Intel i7-12700, 32GB RAM）：

| 指标 | 目标 | 实测 |
| --- | --- | --- |
| 中文 PII 召回率 | ≥ 85% | 91.8%（F1） |
| Agent 多轮 P99 延迟 | < 2s | 1.2s（含 detection_cache） |
| 单条检测延迟（CPU） | - | ~180ms |
| 安装到可用 | < 5 分钟 | < 30 秒（Docker） |
| 内存占用 | - | ~80MB（空闲）/ ~200MB（峰值） |

### 与竞品对比

| 项目 | 中文 PII | 仿真替换 | Tool-call 扫描 | 中文仿真 | 性能 |
| --- | --- | --- | --- | --- | --- |
| PrivAiTe | ❌ | 弱 | ❌ | ✅ JSON 值替换 | ❌ Python，较慢 |
| Kiji | ❌ | 仅 6 语言 | ✅ | ❓ | ❌ 英文中心 Go |
| Eidolon | ❌ | ✅ 英文 | ❌ | ❌ | Rust，快 |
| AI Privacy Gateway | ✅ | 正则 | ❌ | ❌ | Python |
| LLMate Gate | ✅ F1 0.918 | ✅ v1.1 | ✅ AST 感知 | ✅ 独占 | Go + Rust |

## 🗺️ 路线图

### v1（6-8 周，当前进行中）

- ✅ OpenAI 兼容透明代理（:8400）
- ✅ PII Engineer 中文检测集成（F1 0.918）
- ✅ Tool-call 参数递归扫描
- ✅ 流式还原（trie 缓冲）
- ✅ detection_cache 绑定 conversation_id
- ✅ fail-closed 电路断路器
- ✅ 占位符替换
- ✅ VS Code 扩展 + Claude Code hooks
- ✅ Tauri 桌面 UI
- ✅ 审计日志 + cn-pii-bench
- ⏳ 中文格式保持仿真替换（v1.1）

### v2

- 仿真替换升级为 ML 生成（上下文感知的仿真值）
- MCP Server 门面（让 Agent 主动调用脱敏）
- 企业 SSO + 团队策略下发
- 多上游负载均衡 + 敏感路由（本地模型兜底）

## 📦 技术栈

| 组件 | 技术选型 | 理由 |
| --- | --- | --- |
| 代理守护进程 | Go | 高并发、单文件部署、:8400 OpenAI 兼容 |
| 检测引擎 | Rust + ONNX Runtime | PII Engineer 原生 Rust，CPU 推理 ~180ms |
| 仿真替换引擎 | Go + Faker zh | 中文 locale + 格式校验 |
| VS Code 扩展 | TypeScript | 原生 API |
| 桌面 UI | Tauri (TS + Rust) | 与检测引擎共享 Rust 生态 |
| 基准测试 | Python + Rust | cn-pii-bench |

## 📜 License

Apache License 2.0（LICENSE 文件尚未添加）

## 🤝 贡献

欢迎 PR 和 Issue！特别是：

- 中文实体类型的检测规则补充
- 仿真替换的中文 locale 扩展
- 更多 LLM 上游的适配
- cn-pii-bench 基准数据集扩充

## ⚠️ 免责声明

LLMate Gate 提供假名化（pseudonymization）层，不承诺 100% 防泄漏。在受监管场景下，它应作为纵深防御体系的一环，与注入检测、内容审计、数据出境评估等措施配合使用。使用本软件不自动构成 PIPL / GDPR 合规，请结合您的业务场景咨询法务。

<div align="center">

🛡️ LLMate Gate — 中文一等公民的 LLM Agent 隐私网关

docs/ · bench/ · vscode-ext/ · roadmap.md

</div>
