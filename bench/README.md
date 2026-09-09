# cn-pii-bench （LLMate Gate 自评估语料 · 契约 §9 · 探路者探路）

LLMate Gate 计划：跑全套 PII 检测（regex + PII Engineer）评估时，给出
**中文场景 PII 语料**：姓名 + 手机 + 身份证 + 邮箱 + 银行卡 + IP + 地址 + 组织机构。
v0.1 阶段至少给 100+ 条合成样本，覆盖：

* 短文本（含 1-3 类 PII）
* 长文档（多段落，多类 PII 混合，含模板干扰）
* 对抗样例（错位、间隔符、英文夹杂）
* 复姓 + 少数民族姓名 + 历史名人

## 目录结构

```
bench/
├─ README.md
├─ loader_test.go        # fixture 加载器单测
├─ fixtures/
│   ├─ cases.jsonl       # 合成样本，每行 {id,text,expect:[{type,value,start,end}]}
│   ├─ README.md         # 字段约定
│   └─ cases_test.go     # fixture 自检（无重叠、无空文本、start+len=end）
└─ run.sh                # 后续：eval 跑分，对比检测器输出与 cases.jsonl
```

## 评测指标（v0.1）

* **实体级 P/R/F1**（按类型 + 总体）
* **位置敏感 P/R/F1**（start/end 须与 expect 相符）

## 任务清单

* [x] 0.1 骨架（loader_test.go + fixtures/cases.jsonl 占位）
* [ ] 0.2 注入 100+ 合成样本
* [ ] 0.3 跑分脚本：内置 regex 引擎 vs cases.jsonl
* [ ] 0.4 接入 PII Engineer 模型做基准（v0 暂未集成，先空跑）
