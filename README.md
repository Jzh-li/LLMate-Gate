# 🛡️ LLMate Gate

中文一等公民的 LLM Agent 隐私网关 —— 为大模型 API 调用提供 PII 检测、中文仿真替换、tool-call 参数扫描、流式还原与可审计合规。

> Your LLM's privacy gatekeeper for the Chinese-speaking world.

> 🌏 **语言策略：本项目以中文为主，关键文档均中文撰写**  
> PII 模型 + 仿真语料 + 评估器均针对中文场景训练/构造。  
> 英文使用者欢迎提 issue / PR 帮我们翻译 README 与关键文档；现阶段中文是事实工作语言。

## 🚀 5 分钟 Quickstart

### 装（已发布 Release）

```bash
# macOS (Apple Silicon)
curl -L -o /tmp/lg https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-darwin-arm64
chmod +x /tmp/lg && sudo mv /tmp/lg /usr/local/bin/llmate-gate

# Linux (x86_64)
curl -L -o /tmp/lg https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-linux-amd64
chmod +x /tmp/lg && sudo mv /tmp/lg /usr/local/bin/llmate-gate

# Windows (PowerShell)
Invoke-WebRequest -Uri 'https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-windows-amd64.exe' -OutFile 'llmate-gate.exe'
```

### 跑（30 秒可起）

```bash
# 1. 起 mock-llm + llmate-gate（仅验证产品形态，不需要真 API key）
llmate-gate --config ./gateway/configs/e2e-llm.yaml --listen :8400 &
mock-llm --listen :8999 &

# 2. 健康检查
curl http://127.0.0.1:8400/healthz

# 3. 试一发
curl -X POST http://127.0.0.1:8400/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-4",
    "messages": [{"role":"user","content":"我叫李雷，手机13800138000，请回拨"}]
  }'
# 响应里 PII 已被替换；调试面板 http://127.0.0.1:8400/_debug 看审计日志
```

### 接 Cursor / Continue / Claude Code

改一行 base_url 即可：

```python
# Python OpenAI SDK
openai.api_base = "http://127.0.0.1:8400/v1"
```

```ts
// Node.js / TypeScript
const openai = new OpenAI({
  baseURL: "http://127.0.0.1:8400/v1",
});
```

### 真接 LLM（要 API key）

```bash
cat > ~/.local/share/llmate-gate/config.yaml <<EOF
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"        # 必填
  upstream_api_key: "${OPENAI_API_KEY}"     # 转发时用它换掉客户端凭证
EOF
llmate-gate --config ~/.local/share/llmate-gate/config.yaml
```

接 Claude Code 这类走 Anthropic 协议的客户端，用 `upstreams[]` 把 `/v1/messages`
单独指到 Anthropic 兼容上游（详见「配置」一节）：

```yaml
gateway:
  listen: ":8400"
  upstream: "https://api.openai.com"                    # OpenAI 侧兜底
  upstreams:
    - protocol: "anthropic"
      base_url: "https://api.deepseek.com/anthropic"
      api_key: "${DEEPSEEK_API_KEY}"
      api_version: "2023-06-01"
```

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
- 📝 **登记表（自报真实 PII，补召回）**：中文姓名、自定义地址这类值没有格式约束，统计模型给不出召回保证。把你的真实值登记进来，出现即精准命中，且不受检测阈值限制——「刚发现自己的名字被原样发出去了」可以当场补登、立即生效，不用重启。值明文存在独立文件里（0600），可在调试面板直接维护。
- 🎭 **中文格式保持仿真替换**：v1 占位符替换 → v1.1 仿真替换。生成的假数据保持原格式、语义类型、性别/长度一致，云端模型仍能理解上下文。
- 📇 **仿真词典（自定义真值→假值）**：内置仿真是随机派生的，可读性一般；词典让你指定「张三 → 王晓明」这类固定映射，命中即用、未命中回落内置派生。可在调试面板里直接编辑，改完即时生效、不用重启。配套的**身份卡**（`identity_card`）让你只声明真实值，网关自动生成稳定仿真值。
- 🔌 **OpenAI / Anthropic 双协议透明代理**：同一个入口同时接 OpenAI 兼容端点与 Anthropic 兼容端点，按 `gateway.upstreams[]` 配置各走各的上游（地址、鉴权方式、路径前缀都能分别配），适配国内厂商的协议差异。
- 🚦 **bypass 档位**：一个「网关在位但不改动流量」的开关，用来确认链路与延迟开销、或在上游拒包时快速排除脱敏层。仅建议本机调试/前置拓扑使用。
- 🤖 **Agent tool-call 参数扫描**：递归扫描 tool_calls[].function.arguments 里的所有字符串值，按参数类型差异化处理。
- 🌊 **流式还原**：SSE 流式响应场景下，用 trie 缓冲做边界对齐，保证占位符在流中被完整还原。
- 🧠 **detection_cache**：绑定 conversation_id 的增量检测缓存，避免 Agent 多轮对话里对同一段 PII 重复检测，将 40s+ 的延迟降到 P99 < 2s。
- 🔒 **Fail-closed**：检测引擎异常即阻断请求，绝不"裸奔"放行。
- 🔍 **可审计**：每一次脱敏/还原都生成结构化审计日志，可导出合规报告（对标 PIPL / GDPR / 等保 2.0）。
- 🎛️ **内嵌调试面板**：`/_debug` 提供流量回放、Playground 试脱敏、策略热切换、词典编辑、审计查询，全部无需改配置或重启（见「调试面板」一节）。
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

### 选项 1：从 GitHub Release 下载（推荐 · 30 秒）

```bash
# macOS Apple Silicon
curl -L -o llmate-gate https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-darwin-arm64
chmod +x llmate-gate && ./llmate-gate --version

# Linux x86_64
curl -L -o llmate-gate https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-linux-amd64
chmod +x llmate-gate && ./llmate-gate --version

# Windows (PowerShell)
Invoke-WebRequest -Uri https://github.com/Jzh-li/LLMate-Gate/releases/latest/download/llmate-gate-windows-amd64.exe -OutFile llmate-gate.exe
.\llmate-gate.exe --version
```

### 选项 2：包管理器

**Windows · Scoop**（需先 `Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope CurrentUser`）：

```powershell
scoop bucket add llmate-gate https://github.com/Jzh-li/scoop-bucket
scoop install llmate-gate
llmate-gate --version
```

> v0.1 manifest 已就绪（`scoop-bucket/llmate-gate.json`），待独立仓 `Jzh-li/scoop-bucket` 创建后可启用 `scoop install`。
> macOS · Homebrew tap 计划在 v1.1 上架。

### 选项 3：从源码构建

```bash
git clone https://github.com/Jzh-li/LLMate-Gate
cd LLMate-Gate/gateway
go build -o llmate-gate ./cmd/llmate-gate
./llmate-gate --config configs/config.yaml   # 默认监听 :8400，调试面板默认开启
```

### 选项 4：自交叉编译（多平台 release）

```bash
./scripts/release.sh v0.1.0      # 在当前机器能原生编译的子集（Windows 出 windows.exe，类推）
./scripts/install.ps1            # Windows：拷 + 注册计划任务 + 桌面快捷方式
./scripts/install.sh             # Linux/macOS：拷 + systemd/launchd 开机自启
```

### Docker（规划中，尚未落地）

> ⚠️ **冲突标注（2026-09-10 核对）**：本仓库当前**不存在 `Dockerfile` 或 `deploy/` 目录**，以下命令为规划形态，现阶段**不可用**。
> 两方案利弊：① **补一个 Dockerfile**（约半小时，`CGO_ENABLED=0` 静态构建，多阶段镜像）——保留本节并真正可用，适合一键部署；② **移除本节**——避免文档误导，但丢失部署形态占位。当前未擅自新增 `Dockerfile`（属新增交付物，需你确认），故仅标注。

```bash
# 规划形态，暂不可用
docker run -d -p 8400:8400 ghcr.io/llmate/llmate-gate:latest
```

### 从源码构建

```bash
# 代理守护进程（Go）
git clone https://github.com/Jzh-li/LLMate-Gate
cd LLMate-Gate/gateway
go build -o llmate-gate ./cmd/llmate-gate
./llmate-gate --listen :8400 --upstream https://api.openai.com

# 桌面 UI（Tauri）
# ⚠️ 未启动：仓库无 desktop/ 目录，Tauri 属 Phase 4 待办（见 HANDOFF §5）
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
  # 可选：按协议分别声明上游。不写则只有上面那一条 openai 上游。
  upstreams:
    - protocol: "anthropic"                 # /v1/messages 走这里
      base_url: "https://api.deepseek.com/anthropic"
      api_key: "${DEEPSEEK_API_KEY}"        # Anthropic → x-api-key
      api_version: "2023-06-01"
    - protocol: "openai"                    # /v1/chat/completions 等走这里
      base_url: "https://api.deepseek.com"
      api_key: "${DEEPSEEK_API_KEY}"        # OpenAI → Authorization: Bearer
      # path_prefix: "/v4"                # 智谱 GLM；DashScope 是 /compatible-mode/v1

detection:
  engine: "pii-engineer"      # 中文 NER (F1 0.918)
  fallback_regex: true        # 格式固定的实体用正则加速
  cache:
    enabled: true
    bind_conversation: true   # 绑定 conversation_id 做增量
    ttl: "30m"
  # 登记表：自报真实 PII 值，出现即脱敏（绕过阈值，保召回）。
  # 值本身不写在这里，只在 path 指向的文件里——那是明文 PII，别提交进版本库。
  # 也可以在调试面板「规则」页维护（保存即落盘，无需重启）。
  registry:
    enabled: true
    path: "./registry.yaml"

replacement:
  # placeholder : <<zh_person_name_1>> 式占位符
  # simulate    : 格式保持的仿真值（张三 → 李雷）
  # bypass      : 整条原样透传、完全不脱敏（仅本机调试 / 前置拓扑）
  strategy: "simulate"
  simulate_zh:
    person_name: true         # 张三 → 李雷（性别/长度一致）
    phone: true               # 13800138000 → 13900139000
    id_card: true             # 校验位合法
    bank_card: true           # Luhn 校验合法
    # 自定义词典：命中即用你给的假值，未命中回落内置派生。
    # 仿真值必须全局唯一——还原表是一张平表，两个真值共用一个假值会互相覆盖。
    dictionary:
      zh_person_name:
        "张三": "王晓明"
        "李四": "陈静"
    # 身份卡：dictionary 的糖。按类型声明「我自己的真实值」，网关用固定密钥生成
    # 格式保持、跨重启稳定的仿真值，展开进 dictionary（手写词典优先）。每种类型一个。
    identity_card:
      zh_person_name: "张三"
      zh_phone: "13800138000"
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

## 🎛️ 调试面板

`debug: true` 时网关内嵌一个调试面板，默认只绑定回环地址：

```
http://127.0.0.1:8400/_debug
```

四个页签：

| 页签 | 能做什么 |
| --- | --- |
| **流量** | 实时（WebSocket）看到每一发请求：原文 / 脱敏后 / 检出的实体 / 映射表 / 上游响应 / 还原结果，以及耗时与 outcome。用来排查「为什么这条没被脱敏」最直接。 |
| **Playground** | 粘一段文本，选「仅检测」或「检测 + 替换」，立刻看到命中的实体与替换结果。调词典和阈值时不用真的发请求。 |
| **规则** | 当前生效的策略与不可逆类型；三个策略开关（placeholder / simulate / bypass）点一下即切换；登记表与仿真词典的增删改。全部即时生效，**不需要改配置或重启**。 |
| **审计** | 查询最近的审计事件（可设条数、可自动刷新），点某一行展开完整 JSON。 |

### 登记表怎么用

「规则」页上半部分是登记表：每行是「实体类型 / 值」。和仿真词典正好互补——登记表决定**能不能认出来**（检测），词典决定**换成什么假值**（替换）。

- **什么时候用**：检出的漏了。中文姓名、内部项目代号、你司特有的地址写法，这些没有格式约束，模型只能猜；登记一次，之后每次出现都命中。
- **命中即脱敏，且不受阈值限制**。阈值是给统计模型调噪声用的（分数不到就放过），而你显式声明过的值不该再被它刷掉——这是召回保证。代价是登记时要自律：别登记「我」「的」这种高频短串。
- **匹配规则**：ASCII 大小写不敏感（`Me@Corp.cn` 能命中 `me@corp.cn`），中文逐字节精确；同一个值只能属于一个类型；至少 2 个字符。
- **中文里是「子串命中」**：中文没有词边界，所以登记 `张三` 时正文里的 `张三丰` 也会命中前两个字。方向是**宁多勿漏**——多脱敏只是难看，漏掉就是泄露。同一个起点取最长：同时登记了 `张三` 和 `张三丰`，正文里的 `张三丰` 只算一条、按更长的那个类型处理。这也是「至少 2 个字符」这条限制存在的原因。
- **登记后立刻生效**：网关会同步清掉两层检测缓存（LRU + Merkle 增量），所以**同一句话重发一次就会重新检测**。不清缓存的话「刚补登的值对刚才那句话不生效」，这恰恰是这个功能最主要的用法。
- **落盘**：保存时直接写进 `detection.registry.path` 指向的文件（0600 权限，目录 0700），重启后自动加载，不需要手工粘 YAML。文件是**明文 PII**，`.gitignore` 已排除 `registry.yaml` / `registry.yml`，别改成别的名字后又提交进版本库。
- 没启用（`detection.registry.enabled: false`）时面板只读并提示怎么开；这是一个硬开关——关掉就是关掉，面板翻不了。

### 仿真词典怎么用

「规则」页下半部分是词典编辑器：每行是「实体类型 / 真实值 / 仿真值」。

- 命中词典的实体直接用你写的仿真值；没命中的回落内置词表 + 确定性派生，两条路并存，所以不必把全部真值都填进去。
- 只在 `strategy: simulate` 时生效（placeholder / bypass 下词典不参与）。
- 前端和后端都会拦「仿真值重复」。**仿真值必须全局唯一，跨类型也算**——还原表是 `map[哨兵串]原值` 一张平表、不分类型分桶，两个真实值共用一个仿真值就会在还原时互相覆盖，其中一个永久还原不回来。服务端在配置加载期和面板提交时用同一套规则校验。
- 词典只存在运行时内存里。要跨重启保留，点「导出」把生成的 YAML 片段粘进 config 的 `replacement.simulate_zh.dictionary`。

### 身份卡怎么用

身份卡是词典的糖：不想自己给每个真实值起仿真名、又想让「我的仿真身份」稳定可预期时用它。

- 配置里按类型声明自己的真实值，网关用固定密钥生成格式保持、跨重启稳定的仿真值，等价于手写 `dictionary` 的 (真实值 → 仿真值) 对。每种类型一个（多值场景仍走词典）。
- **稳定**：同一真实值跨重启、跨安装恒映射到同一仿真值——因为派生密钥是固定的，不像会话密钥那样进程随机。方便把仿真名提前告知下游。
- **与手写词典冲突时，手写词典优先**：同一 (类型, 真实值) 两边都写了，以你显式给出的仿真值为准，身份卡不覆盖。
- 展开发生在配置加载期，结果并入词典后走同一套全局唯一校验——身份卡的仿真值和手写仿真值撞车同样会被启动期拦下。它不引入第二条运行时路径，就是个 authoring 糖。
- 只在 `strategy: simulate` 时生效。

### 面板 API

面板背后是一组普通 HTTP 端点，可以脱离 UI 直接用：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/_api/traffic` | 流量环形缓冲快照 |
| `DELETE` | `/_api/traffic` | 清空流量记录 |
| `POST` | `/_api/detect` | 只做检测，body `{"text": "..."}` |
| `POST` | `/_api/replace` | 检测 + 替换，body `{"text": "...", "strategy": "simulate"}` |
| `GET` | `/_api/rules` | 当前策略与不可逆类型 |
| `PUT` | `/_api/rules` | 切换策略，body `{"strategy": "bypass"}` |
| `GET` | `/_api/dictionary` | 当前仿真词典 + 可仿真的实体类型列表 |
| `PUT` | `/_api/dictionary` | 整体替换仿真词典（不是增量合并） |
| `GET` | `/_api/registry` | 当前登记表 + 可登记的类型 + 落盘路径 + 是否启用 |
| `PUT` | `/_api/registry` | 整体替换登记表：校验 → 写盘 → 生效并刷检测缓存 |
| `GET` | `/_api/audit?limit=N` | 近期审计事件 |
| `WS` | `/ws/events` | 实时事件流 |

> `/_api/registry` 的 `PUT` 顺序是**校验 → 落盘 → 生效**，任一步失败就整体不动：写坏了文件会导致下次启动加载失败、整张表全丢；生效了却没落盘会让用户以为重启后还在。

> ⚠️ 面板能看到**明文 PII**（Playground 与流量详情都含原文）。它**只接受回环地址的访问**（按 TCP 对端地址强制校验，不读可伪造的 `X-Forwarded-For`），且不鉴权；因此即便 `listen` 绑到 `0.0.0.0`，面板也不会对外可达。对外部署请用 `--no-debug` 彻底关掉面板，改用 `/_api` 之外、带鉴权的 `/v1/privacy/*` 接口。

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

> **2026-09-11 更新**：车牌（中英统一类型 `plate`，含 `License plate: ABC-1234` 上下文识别）、URL（`url`）、US SSN（`us_ssn`）、国际信用卡（`credit_card`，IIN 前缀识别 Visa/MasterCard/Amex/Discover/JCB）已接线并进入检测输出。双语基准 F1 = 1.0（中文 240 条零回归 + 英文 180 条，报告见 `bench/reports/`）。
> **组织机构 / 金额 / 邮编仍未实现**。当前类型常量表见 `gateway/pkg/types/detect.go`。

### 国际化实体（英文基线，2026-09-11 起）

内置正则引擎 `detection.engine=regex` 除中文实体外，同时覆盖以下英文/国际类型：

| 类型 | 格式 | 示例 |
| --- | --- | --- |
| 英文车牌 | license/plate 上下文引导 | License plate: ABC-1234 |
| URL | http/https/ftp + scheme/host 校验 | https://example.com/path |
| US SSN | AAA-GG-SSSS + SSA 区域规则 | 078-05-1120 |
| 国际信用卡 | 13-19 位 + Luhn + IIN 前缀 | 4242424242424242 (Visa) |

校验函数在 `gateway/pkg/global/`（`ValidURL` / `ValidUSSSN` / `ValidCreditCard` / `IsInternationalCard`），与中文 `pkg/cn/` 解耦、不互相依赖。

PII Engineer 侧模型支持 13+ 语言：English, Malay, Tamil, Chinese, Indonesian, Vietnamese, Thai, Hindi, Bengali, Korean, Japanese, German, French, Spanish, Portuguese, Russian, Arabic, Turkish, Polish, Dutch, Italian, Swedish 等 35+ 语言。

> ⚠️ **未兑现**：上述多语言能力来自 PII Engineer（尚未集成，见 `DECISIONS.md D001` 与 `DECISION.md`）。当前默认引擎为内置正则 `detection.engine=regex`，覆盖中英双语上表所列类型。

## 🔍 审计与合规

LLMate Gate 的审计日志是结构化的，每条记录包含：

```json
{
  "timestamp": "2026-09-09T15:30:00Z",
  "schema_version": "1",
  "request_id": "req_a3f9b2",
  "conversation_id": "conv_8821",
  "upstream": "openai",
  "detected_entities": [{"type": "zh_person_name", "score": 0.97}],
  "replaced_count": 3,
  "strategy": "placeholder",
  "restored": true,
  "streaming": false,
  "latency_ms": 212,
  "detector_latency_ms": 3,
  "outcome": "success"
}
```

> 字段名以 `Specs/02-接口与数据契约规范.md` §9.1 与 `gateway/internal/audit/audit.go` 为准（旧示例里的 `entity_types` / `replacement_strategy` 已废弃）。

可导出为 PIPL / GDPR / 等保 2.0 合规报告格式。

> ⚠️ 诚实边界：LLMate Gate 定位为假名化（pseudonymization）而非匿名化（anonymization）——我们不承诺 100% 防泄漏，我们卖"可测量、可审计"。自动检测抓不全所有敏感信息，必须与注入检测、内容审计一起组成纵深防御。

## 📈 性能基准

### 真对抗语料（28 条「真对抗 F1」，推荐阅读）

> 实测时点 **2026-09-12**，引擎 `detection.engine=regex`，语料 `bench/fixtures/cases_adversarial.jsonl`（28 条手写真对抗样本）。
> 报告原文：`bench/reports/adversarial_20260912-195233.md`。

| 指标 | 实测 | 含义 |
| --- | --- | --- |
| **真对抗 F1** | **0.7458** | 真对抗语料 (type, value) 严格匹配 |
| 精确率 (P) | 0.9565 | 报出来的里面有 96% 是真 PII |
| 召回率 (R) | 0.6111 | 真 PII 里有 61% 被找到 |
| 检测 p99 延迟 | 24ms | `/_api/privacy/redact gate_only=true` |

**按子集（F1）**：

| 子集 | F1 | 说明 |
| --- | --- | --- |
| email / ip_address / phone | **1.0** | 格式规则严格，regex 引擎完全覆盖 |
| mixed（含 tool_call） | 0.86 | 多类型混排，地址部分漏报 |
| person_name / address | **0.0** | 中文姓名 + 中文地址当前 regex **未覆盖** |
| id_card_masked | 0.0 | 已知弱项（`********` 遮蔽格式） |

> 💡 **为什么这比合成语料数字更重要**：合成语料 F1=1.0 是过拟合基线（生成器按检测器算法写样本）；真对抗语料是手写真实场景样本，暴露当前 regex 引擎的盲区。
> v1.1+ 引入 PII Engineer（中文 NER F1 0.918）将主要补齐 person_name + address 两块。

### 合成语料（240 条参考基线）

> 实测时点 **2026-09-10**，引擎 `detection.engine=regex`（内置正则），语料 `bench/fixtures/cases.jsonl`（240 条合成样本，8 子集 × 30）。
> 报告原文：`bench/reports/phase0_regex-v2_20260910-214153.md`。

| 指标 | 目标 | 实测 | 口径 |
| --- | --- | --- | --- |
| 中文 PII 召回率 | ≥ 85% | **100%**（F1 = 1.0000，240/240） | 合成语料，严格四元组匹配 |
| 检测端点 P99 延迟 | < 2s | **23ms**（p50=1ms / p95=21ms / max=30ms） | `/_api/detect` 单条 |
| Agent 多轮端到端 P99 | < 2s | **未测** ⚠️ | 见下方说明 |
| 安装到可用 | < 5 分钟 | **< 30 秒** | 单二进制，无需 Docker / 模型 |
| 内存占用 | - | 未测 | 待补 |

> ⚠️ **两点诚实说明**
> 1. 合成语料不代表真实分布；**真对抗数字 0.7458 更值得引用**（见上节）。
> 2. §16 第 2 项原意是"Agent 多轮对话端到端 P99"，当前只有**检测端点**的 p99；**端到端多轮未压测**，故该项不宣称达成（详见 `V1_READINESS.md`）。
> 3. 上一版 README 中的 "91.8% / 1.2s / ~180ms / 80-200MB" 来自 **PII Engineer 的公开规格**（`Specs/00` 附录 A），**不是本项目实测值**，已更正。

### 与竞品对比

> 下表"竞品"列来自 `Specs/00-整体技术方案.md` §13.1 的调研结论；**LLMate Gate 一行已按实测更正**（原写 "F1 0.918" 是 PII Engineer 的规格值，不是本产品实测）。

| 项目 | 中文 PII | 仿真替换 | Tool-call 扫描 | 中文仿真 | 性能 |
| --- | --- | --- | --- | --- | --- |
| PrivAiTe | ❌ | 弱 | ❌ | ✅ JSON 值替换 | ❌ Python，较慢 |
| Kiji | ❌ | 仅 6 语言 | ❓ 待测 | ❓ | ❌ 英文中心 Go |
| Eidolon | ❌ | ✅ 英文 | ❌ | ❌ | Rust，快 |
| AI Privacy Gateway | ✅ | 正则 | ❌ | ❌ | Python |
| **LLMate Gate（实测）** | ✅ F1 **0.7458**（真对抗）/ 1.0（合成） | ⏳ v1.1 | ✅ 键保留递归扫描 | ✅ 独占 | Go，检测 p99 23ms |

## 🗺️ 路线图

### v1（6-8 周，当前进行中）

- ✅ OpenAI 兼容透明代理（:8400，含 `/v1/messages` Anthropic 形态）
- ⏸️ **PII Engineer 中文检测集成** —— 暂缓：`DECISIONS.md D001` 降级为可选，默认引擎为内置 `regex`；合成语料 F1 已达 1.0，NER 集成 ROI 待真实对抗语料验证
- ✅ Tool-call 参数递归扫描（键保留）
- ✅ 流式还原（SSE 帧感知 + 跨事件占位符拼接）
- ✅ detection_cache 绑定 conversation_id（Merkle 前缀链增量）
- ✅ fail-closed 电路断路器
- ✅ 占位符替换 + 中文仿真引擎（仿真模式 v1.1 启用）
- ✅ VS Code 扩展 + Claude Code hooks
- ✅ **MCP Server 门面**（anonymize / deanonymize / scan_tool_params，stdio）—— 原计划 v1.1，**已提前在本阶段交付**
- ❌ **Tauri 桌面 UI** —— 未启动（Phase 4，无 Rust 工具链）
- ✅ 审计日志（结构化 JSONL + `/_api/audit` + 面板审计 Tab）+ cn-pii-bench（240 条 + 评估器）
- ⏳ 中文格式保持仿真替换启用为默认（v1.1，需 A/B 验证不降 LLM 输出质量）

### v2

- 仿真替换升级为 ML 生成（上下文感知的仿真值）
- 企业 SSO + 团队策略下发
- 多上游负载均衡 + 敏感路由（本地模型兜底）
- 真实对抗语料 + PII Engineer ROI 重评

## 📦 技术栈

| 组件 | 技术选型 | 理由 |
| --- | --- | --- |
| 代理守护进程 | Go 1.24 | 高并发、单文件部署、:8400 OpenAI 兼容 |
| 检测引擎 | **内置中文正则（默认 `regex`）**，PII Engineer sidecar 可选 | D001：模型 620MB + Rust 工具链暂不引入；两者同 `detector.Client` 接口，改配置即可切换 |
| 仿真替换引擎 | Go（规则生成 + 校验位） | 格式保持、中文原生、零依赖；v1.1 启用 |
| VS Code 扩展 | TypeScript | 原生 API |
| 桌面 UI | Tauri (TS + Rust) | **未启动**，Phase 4 待办 |
| MCP 门面 | Go + `mark3labs/mcp-go` | stdio，已交付（v0.37.0 pin，兼容 Go 1.24） |
| 基准测试 | Python（`bench/runner.py`）+ Go（`cmd/bench-runner`） | cn-pii-bench；两套口径见 `DECISION.md` 注释 |

## 📜 License

Apache License 2.0 — see [LICENSE](./LICENSE) for the full text.

Copyright 2026 LLMate Gate Contributors

## 🤝 贡献

完整指南见 [CONTRIBUTING.md](./CONTRIBUTING.md)：

- 开发环境（Go 1.25+ / dev.sh 自动 scratch）
- PR 流程（先 issue → fork → branch → CI 全绿）
- Conventional Commits 规范
- Code Review 标准（正确性 / 测试 / 可观测 / 可回滚）
- 范围边界（欢迎 / 慎重 / 不接）

**特别欢迎**：
- 中文实体类型的检测规则补充
- 仿真替换的中文 locale 扩展
- 更多 LLM 上游的适配
- cn-pii-bench 基准数据集扩充
- 文档翻译与错别字

## 🛡️ 安全

漏洞**请勿**在 Issue 公开提交，走私密渠道：[SECURITY.md](./SECURITY.md)。

| 阶段 | 目标时间 |
|---|---|
| 确认接收 | 48 小时内 |
| Critical 漏洞修复 | 48 小时内发版 |
| High 漏洞修复 | 7 天 |

## 📜 行为准则

所有互动需遵循 [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md)（Contributor Covenant v2.1）。

## ⚠️ 免责声明

LLMate Gate 提供假名化（pseudonymization）层，不承诺 100% 防泄漏。在受监管场景下，它应作为纵深防御体系的一环，与注入检测、内容审计、数据出境评估等措施配合使用。使用本软件不自动构成 PIPL / GDPR 合规，请结合您的业务场景咨询法务。

<div align="center">

🛡️ LLMate Gate — 中文一等公民的 LLM Agent 隐私网关

docs/ · bench/ · vscode-ext/ · roadmap.md

</div>
