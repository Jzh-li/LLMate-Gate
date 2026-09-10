# cn-pii-bench（LLMate Gate 自评估语料 · 契约 §9 · 探路者探路）

LLMate Gate 的中文 PII 评测语料与评估器。**双层口径**：L1 检测器级（span 四元组
匹配）+ L2 泄漏级（结构化载体对等性 + 往返）。

设计借鉴 [privaite-bench](https://github.com/mandiant/privaite-bench)
（`bench_precision_recall.py` 实体级 P/R/F1 + FP/FN 逐条 dump；
`bench_structured.py` 4 种结构化载体对等性 + 往返），但**实现重写**：
- 评分对象是**检测器 span**（`/_api/detect`）和**端到端 payload**（`/v1/privacy/redact`），
  不是字符串包含；
- 只覆盖中文 PII，没有 multi-language / spaCy / ONNX。

## 目录

```
bench/
├─ README.md             本文件
├─ runner.py             L1 检测器级评估（强四元组匹配 + by-subset + FP/FN dump + JSON 报告 + selftest）
├─ carriers.py           L2 泄漏级评估（4 载体对等性 + 往返 + 守门模式 + selftest）
├─ generate.py           240 条合成语料生成器（身份证校验位 / 手机号段 / Luhn / 复姓）
├─ validate.py           fixture 字段一致性自检
├─ fixtures/
│   ├─ cases.jsonl       240 条合成样本（8 子集 × 30）
│   └─ cases.schema.json
└─ reports/              评估产物（md + json 时间戳报告）
```

## 语料

8 子集 × 30 = 240 条，全部 `provenance=synthetic`（由 `generate.py` 合成）：

| 子集 | 内容 | 关键风险点 |
|---|---|---|
| person_name | 单姓 + 复姓 + 少数民族姓名 | 复姓长度对齐、上下文无关消歧 |
| phone | 三大运营商号段 | 段号合法 + 11 位 |
| id_card | 18 位 + ISO 7064 校验位 | 校验位合法 |
| bank_card | BIN + Luhn | Luhn 通过 |
| address | 直辖市 / 省市区逆向 | 行政区划对齐 |
| email | 域名白名单 | 邮箱格式 |
| tool_call | JSON 字符串里嵌 PII | 字符串内的 PII 检测 |
| mixed | 中英混合长文本 | 多类同现 |
| adversarial | 错位 / 间隔符 / 英文夹杂 | 干扰符 |

**注意**：在自作者合成语料上自评，F1 高只说明「检测器与生成器对同一套仿真规则
达成一致」，**不构成真实场景结论**（详见 `Specs/03-测试规约.md` §4.3.4）。

## 评估

### L1 检测器级（runner.py）

```bash
# 端到端（需先启网关）
python3 bench/runner.py --endpoint http://127.0.0.1:8401/_api/detect --gate

# 自检（不启网关）
python3 bench/runner.py --selftest
```

产物：`bench/reports/phase0_<engine>_<ts>.md` + `.json`
（包含 per_type / per_subset / fp_items / fn_items）。

### L2 泄漏级（carriers.py）

```bash
python3 bench/carriers.py --base-url http://127.0.0.1:8401 --gate
python3 bench/carriers.py --selftest --limit 40
```

4 载体：`flat` / `multimodal` / `tool_call` / `tool_call_nested`。
**regressions vs flat = 0、round-trip 100%** 是 v1 守门。

## 任务清单

- [x] 0.1 骨架（loader + fixtures 占位）
- [x] 0.2 注入 240 合成样本（8 子集 × 30）
- [x] 0.3 跑分脚本（runner.py / carriers.py / selftest）
- [x] 0.4 第三方标注硬约束（SPEC §4.3.4，待 v1 截止前补 human-annotated + third-party 集）
- [ ] 0.5 接入 PII Engineer 模型做基准（暂缓，等真实对抗语料召回率 < 85%）
