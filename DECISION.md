# DECISION.md — Phase 0 立项决策

> 对应《执行手册》Phase 0：「确认中文仿真替换是独占代差」。
> 结论：**GO（继续）**。本文档是本阶段的决策门凭证，数据来自真实引擎在已知语料上的量化测量。

---

## 1. 决策问题

LLMate Gate 的核心卖点是 **中文仿真替换（format-preserving, reversible Chinese PII replacement）**：
脱敏时保留原文的语义结构与格式（手机号仍是 11 位、身份证仍是 18 位、姓名仍是 2–4 字中文），
还原时按占位符精确可逆。这区别于「整段打码 / 整段删除」的粗暴方案。

Phase 0 要回答：**这套方案是否构成一个可防御的代差？** 即——在真实中文语料上，
内置检测引擎能否「高精度、可接受的召回」地找出需要替换的实体，从而让仿真替换真正可用。

---

## 2. 方法（与 SPEC 0.1 对齐）

- **语料**：`bench/fixtures/cases.jsonl`，**240 条**，8 个子集 × 30 篇：
  `person_name / phone / id_card / bank_card / address / tool_call / mixed / adversarial`。
- **Ground truth**：每条带人工标注的 `type / value / UTF-8 字节偏移`，由 `bench/generate.py`
  确定性生成（seed `20260910`），生成值复用与 `pkg/cn` 一致的校验算法（手机号段、身份证校验位、Luhn），
  保证可被引擎合法检出；偏移经 `bench/validate.py` 自检（按字节切片，非字符切片）。
- **被测引擎**：内置 `RegexEngine`（`internal/detector`，即 `detection.engine=regex`）。
  通过 `cmd/bench-runner` 直接调用，不依赖网关配置，结果可复现。
- **指标**：精确率 P = TP/(TP+FP)、召回率 R = TP/(TP+FN)、F1。匹配规则为「同类型 + 区间重叠」。

> **四方对比（PrivAiTe / AI Privacy Gateway / Eidolon）按用户要求暂不做**，见 §6。

> ⚠️ **2026-09-10 数据刷新注记（本文档 §3/§4 的数字已被后续提交作废）**
> `30c9de0`（地址正则支持直辖市）之后，同一语料重跑结果为 **Precision 1.000 / Recall 1.000 / F1 1.000**
> （240/240，延迟 p99 23ms），报告见 `bench/reports/phase0_regex-v2_20260910-214153.md`。
> 本文档 §3 保留的是 **v1（修复前）** 原始测量，作为立项当时的凭证，**不再代表当前基线**。
>
> **两套评估器口径不同，数字不可直接比较**（`SPEC_ALIGNMENT.md` C5 / Q5）：
> - 本 §2 声明的口径 = Go `cmd/bench-runner`，"**同类型 + 区间重叠**"；
> - `PROGRESS.md` 与 `bench/reports/*-v2` 的口径 = Python `bench/runner.py` 经 `/_api/detect`，"**严格四元组 (type,value,start,end)**"。
> CI 的 `bench-baseline` job 跑的是前者（召回 <0.9 守门），对外汇报用的是后者。
> **当前处置**：未删除任一评估器、未改 CI；仅在此标注差异，等你裁决以谁为准。

---

## 3. 结果

**整体（240 cases, 360 实体）**：

| 指标 | 值 |
|---|---|
| 精确率 Precision | **1.000** |
| 召回率 Recall | **0.944** |
| F1 | **0.971** |
| TP / FP / FN | 340 / 0 / 20 |

**按实体类型**：

| 类型 | GT | 检出 | TP | 召回 | 精确 |
|---|---|---|---|---|---|
| zh_phone（手机） | 103 | 103 | 103 | **1.000** | 1.000 |
| zh_id_card（身份证） | 67 | 67 | 67 | **1.000** | 1.000 |
| zh_bank_card（银行卡） | 35 | 35 | 35 | **1.000** | 1.000 |
| email（邮箱） | 35 | 35 | 35 | **1.000** | 1.000 |
| zh_person_name（人名） | 60 | 60 | 60 | **1.000** | 1.000 |
| zh_address（地址） | 60 | 40 | 40 | **0.667** | 1.000 |

**按子集召回**：`phone / id_card / bank_card / person_name / mixed / adversarial` 均为 **1.000**；
`address 0.700`（21/30）、`tool_call 0.817`（49/60，缺失项即 tool-call JSON 内的地址实体）。

---

## 4. 解读

1. **精确率 1.000、零误报** —— 这是仿真替换成立的前提。若检测误报，会把无辜文本替换掉、
   还原时破坏原文。本引擎对格式固定实体（手机/身份证/银行卡/邮箱/人名）做到零误报，
   因此「仿真替换」在结构化实体上是**安全可逆**的，这正是相对「打码/删除」方案的代差。

2. **结构化实体召回 100%** —— 强格式实体（带校验位/号段/域名）在正则引擎下既准又全，
   覆盖了绝大多数真实 PII 泄漏面（手机、身份证、银行卡、邮箱）。

3. **地址召回 0.667 是已知边界，非缺陷** —— 地址正则要求
   `省/自治区 + 市/区/县/旗 + 路/街/…` 的行政区划链；而直辖市「北京市」「上海市」无「省」字，
   约 10/30 漏检。这正是《技术方案》早已声明的事实：**弱格式实体（中文人名、地址）在正则引擎下
   只保证「高精度、低召回」，完整召回依赖 `detection.engine=pii-engineer` 的 NER 模型**。
   语料如实暴露此边界，而非掩盖。

4. **对抗子集 100% 召回** —— 说明复杂上下文（长句包裹、中文紧贴、邮箱夹在回执文本中、
   银行卡与区号数字用横杠分隔）下引擎未被干扰；digitBoundary / idBoundary 边界保护有效。

---

## 5. 决策

**GO。** 在已知 ground truth 上，内置引擎以零误报 + 0.944 整体召回验证了「中文仿真替换」的可行性，
代差成立。剩余的地址召回缺口被 SPEC 设计明确归因为 NER sidecar 职责，属 Phase 2「Agent 场景优化」范畴，
不构成立项阻塞。

**不做 PIVOT 的理由**：不存在需要切换技术路线的信号——结构化实体已 100% 覆盖，弱格式实体的
召回路径（pii-engineer NER）已在架构内预留，无需另起炉灶。

---

## 6. 已知限制与后续动作

| 项 | 状态 | 归属 |
|---|---|---|
| 地址等弱格式实体召回（≈0.67） | 已知，设计内 | Phase 2：接入 pii-engineer NER sidecar |
| per-type fate 配置化（可逆/不可逆） | 部分实现 | Phase 2 阶段 2 |
| tool-call 参数逐值脱敏 | 部分实现 | Phase 2 阶段 2 |
| **四方对比基准（PrivAiTe / AI Privacy Gateway / Eidolon）** | **未做** | 用户要求暂缓；需 Docker 起竞品，成本高 |
| 中文仿真细化（复姓长度、邮箱域名、身份证校验位对齐） | 已实现 | Phase 1 |

**回归守门**：`ci.yml` 的 `bench-baseline` job 在每次 push 用 `bench-runner` 重跑本语料，
整体召回 < 0.9 即失败，防止检测能力静默退化。

---

## 7. 复现

```bash
# 生成语料（确定性，可复现）
python3 bench/generate.py
# 校验语料结构/偏移一致性
python3 bench/validate.py
# 用真实引擎跑基线指标
cd gateway && go run ./cmd/bench-runner ../../bench/fixtures/cases.jsonl
```

---

## 8. 差异化清单（2026-09-11）

> 本节是接管的索引。任何会话接手时先读本节，回答"我们凭什么 / 给谁看 / 现在做到哪一步"。
> **本节只描述"已验证的事实差异"和"已识别的空白"，不列形容词式差异点（如"我们做得好"）。
> 三层结构对应三类决策受众，每条差异都标注：竞品对应状态、当前落地状态、依据文件。

### 8.0 三层结构与受众

| 层 | 名称 | 受众 | 答什么问题 | 竞品追平成本 |
|---|---|---|---|---|
| 第 1 类 | **独占链**（竞品 100% 做不到） | 投资人 / 自己 | "这产品凭什么不可替代" | 3-6 个月重写链 |
| 第 2 类 | **架构对齐**（竞品做到了但没做对） | 技术买家（架构师） | "为什么不是你直接 fork PrivAiTe 加中文" | 数周改造 |
| 第 3 类 | **产品形态**（竞品没做的工程形态） | 最终用户（开发者） | "装这个要多久、坑多不多" | 1-2 周 |

### 8.1 第 1 类 · 独占链（竞品 100% 做不到）

| 差异点 | 竞品对应状态 | 当前落地 | 依据 / 下一步 |
|---|---|---|---|
| **中文 PII 形态学仿真**（校验位合法的身份证 / 号段合法的手机号 / 长度对齐的复姓） | Faker 加 zh locale 是单点；竞品要重写"仿真+同值映射+校验位算法"整链 | ✅ `internal/simulator` | `Specs/00` §5（仿真替换规则） |
| **中文 planted PII 真机链路基准**（cn-pii-bench L3 真机 wire 测量） | 没有任何竞品做中文真机抓 wire；PrivAiTe 只做了 EN/FR/DE/IT | ❌ **空白** | `TODO_QUEUE.md` 末段·C 组；触发条件：v1 验收通过后启动 |
| **结构化审计 + 中文合规导出**（PIPL/GDPR/等保 2.0/CSL/DSL 五段对比） | PrivAiTe 无 audit API；其他竞品只有 best-effort | ✅ `internal/audit` 五段对比 + 5 个 exporter | `Specs/02` §9；`gateway/internal/audit` |

### 8.2 第 2 类 · 架构对齐（竞品做到了但没做对）

> PrivAiTe 的核心是「协议级 allowlist/denylist + 递归 scrub + 真机链路测量」三件套。
> 我们当前只做了第 2 件的子集；要做完才能和 PrivAiTe 在架构上对齐（中文差异化是单点加挂）。

| 差异点 | 竞品对应状态 | 当前落地 | 下一步动作 |
|---|---|---|---|
| **tool_call arguments 递归扫描** | LiteLLM/LLM Guard **100% 漏 tool_call**（PrivAiTe COMPARISON.md 实测） | ✅ `proxy.transform(parsed, true, anon)` | 已对齐 |
| **N 轮 round-trip 不泄漏**（agent 第 N+1 轮把还原真值重发出去） | 大多数竞品未考虑 | ✅ `internal/cache/merkle.go` | 已对齐 |
| **Block allowlist**（不碰 thinking/base64/reasoning/mcp_list_tools 等不透明 part） | LiteLLM 把整 message 扔 Presidio，不区分 block | ❌ **缺** | **A 动作**：借 PrivAiTe `_BINARY_PART_TYPES` / `_THINKING_TYPES` 思路在 `privacy.go` 加过滤 |
| **`gate_only` API**（只判不改，给 MCP-server / hook 边界用） | 几乎所有竞品只有 redact | ❌ **缺** | **B 动作**：在 `/v1/privacy/redact` 加 `gate_only=true` 参数 |
| **Multimodal content parts 扫描**（image_url / text parts） | PrivAiTe 已实现 | ❌ **缺** | **B 组**：补 `bench/carriers.py` 的 multimodal 载体 |

### 8.3 第 3 类 · 产品形态（竞品没做的工程形态）

| 差异点 | 竞品对应状态 | 当前落地 | 依据 |
|---|---|---|---|
| **单 Go 二进制 + 零依赖**（10MB / <1s 启动 / ~30MB 内存） | Python 竞品需 pip install + 模型下载；Rust 竞品（cloakpipe/Eidolon）也是 Rust 工具链 | ✅ | `scripts/release.sh` 已交付 |
| **三平台交叉编译 + Scoop / Homebrew**（OS-native 包管理器） | PrivAiTe 是 wheel；cloakpipe 是 cargo install；**无 OS-native** | ✅ | `scoop-bucket/llmate-gate.json`；`release.yml` |
| **VS Code 扩展 + Claude Code hooks + MCP stdio 三件套** | 每个竞品只做一层（PrivAiTe 只有 hook） | ✅ | `vscode-ext/` + `hooks/` + `cmd/mcp-server/` |
| **加密 vault 内存常驻 + 失败关闭 + 检测/替换/还原全可审计** | 多数竞品 vault 落盘 / 不 fail-closed | ✅ | `gateway/internal/vault`（AES-256-GCM） |

### 8.4 已识别空白（**未做 = 真护城河机会**）

> 这些是当前**没有**但**应该做**的差异点。每条对应 TODO_QUEUE 一个任务卡。

| 空白 | 影响 | 建议触发时机 |
|---|---|---|
| **真机链路 wire 测量**（L3 benchmark） | 没它，中文差异化没有客观锚点 → 投资人/技术买家都无法被说服 | v1 验收通过 → 启动 4 周专项 |
| **block allowlist**（防止 base64 / thinking / encrypted_content 误改） | 当前没保护，可能误判 / 改坏 | v0.2 立即（半天工作量，零风险） |
| **`gate_only` API** | 当前 MCP/hook 只能 redact 不能只判 | v0.2 立即（半天工作量） |
| **Multimodal 载体对等性** | `bench/carriers.py` 缺 multimodal 载体 | v0.2 立即（半天工作量） |

### 8.5 怎么用本清单

- **写 README §"与竞品对比"时** → 抄 §8.1 + §8.3 的"当前落地 ✅"行
- **客户问"我已经有 Presidio/PrivAiTe，为什么还要你"** → 答 §8.1 + §8.2 的独占链
- **判断 v1.1/v2 优先级时** → 看 §8.4"已识别空白"，按"对护城河贡献 × 工作量"排序
- **避免"形容词差异点"陷阱** → 任何想新增的差异点必须填齐：竞品对应状态 + 当前落地 + 依据文件。任一空缺就退回
- **多账号/多机器接管时** → 本节是项目根 `DECISION.md` 的一部分（git tracked），永远随代码走

### 8.6 不做的清单（显式排除，避免范围蔓延）

| 不做 | 理由 |
|---|---|
| **「我们做得好」式形容词差异** | 无可验证、无可决策；写出来就是文档垃圾 |
| **「比竞品快 X%」式性能差异**（除非有 bench 数据） | 无数据 = 无差异；性能是必要条件不是差异化 |
| **「支持更多语言」**（法语/日语/韩语等第三语种） | 与"中文一等公民"定位冲突，会稀释护城河。**注（2026-09-11 拍板）**：中英双语是国际化基线，不算"更多语言"——英文实体（`url`/`us_ssn`/`credit_card`/`plate`）沿用国际类型名，不做 `en_*` 命名，见 `pkg/global/` |
| **「我们也支持模型 fine-tune」** | `Specs/00 §3.1` 已显式排除：集成而非训练 |
| **「我们是开源的」** | PrivAiTe / Presidio / LLM Guard 都是开源，不是差异 |
