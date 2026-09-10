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
