#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""bench/runner.py —— cn-pii-bench 0.4 评估器（契约 §9）。

对比 fixtures/cases.jsonl 的人工标注 ground truth 与网关 /_api/detect 的
实际输出，按精确匹配（type + value + [start, end)）计算各实体类型的
precision / recall / F1，并统计请求延迟 p50 / p95 / p99，输出 Markdown
报告到 bench/reports/<timestamp>.md。

设计原则（与 §9.2 对齐）：
- 匹配：同一 (type, value, start, end) 四元组才算 TP；其它算 FP 或 FN。
  这种"严格区间匹配"是私域评估的事实标准（与 privaite-bench 一致）。
- 每次评估只对单一候选引擎；切换引擎通过 --endpoint 指向不同的网关或
  sidecar 即可（regex / mock-detector / 未来的 PII Engineer）。
- 顺序：按 fixtures 中 case 顺序串行调用，延迟统计包含网络与引擎。

用法：
    python3 bench/runner.py \
        --endpoint http://127.0.0.1:8401/_api/detect \
        --engine regex \
        --cases bench/fixtures/cases.jsonl \
        --out bench/reports
"""
from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable

ROOT = Path(__file__).resolve().parent
DEFAULT_CASES = ROOT / "fixtures" / "cases.jsonl"
DEFAULT_OUT = ROOT / "reports"

# 候选类型集合（与 pkg/types 常量对齐）
ALL_TYPES = (
    "zh_person_name",
    "zh_phone",
    "zh_id_card",
    "zh_bank_card",
    "zh_address",
    "email",
    "ip_address",
    "date",
)


@dataclass
class Expect:
    type: str
    value: str
    start: int
    end: int


@dataclass
class Detect:
    type: str
    value: str
    start: int
    end: int


@dataclass
class CaseResult:
    case_id: str
    subset: str
    n_expected: int
    n_detected: int
    tp: int = 0
    fp: int = 0
    fn: int = 0
    latency_ms: int = 0
    error: str = ""


@dataclass
class PerType:
    tp: int = 0
    fp: int = 0
    fn: int = 0

    @property
    def precision(self) -> float:
        d = self.tp + self.fp
        return self.tp / d if d else 0.0

    @property
    def recall(self) -> float:
        d = self.tp + self.fn
        return self.tp / d if d else 0.0

    @property
    def f1(self) -> float:
        p, r = self.precision, self.recall
        return 2 * p * r / (p + r) if (p + r) else 0.0


def load_cases(path: Path) -> Iterable[tuple[str, str, list[Expect]]]:
    with path.open("r", encoding="utf-8") as f:
        for lineno, line in enumerate(f, 1):
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError as e:
                raise SystemExit(f"FAIL: invalid json at line {lineno}: {e}")
            cid = str(obj.get("id", f"line-{lineno}"))
            subset = str(obj.get("subset", "unknown"))
            expect = [
                Expect(
                    type=e["type"],
                    value=e["value"],
                    start=int(e["start"]),
                    end=int(e["end"]),
                )
                for e in obj.get("expect", [])
            ]
            yield cid, subset, expect, obj.get("text", "")


def call_detect(endpoint: str, text: str, timeout: float = 10.0) -> tuple[list[Detect], int, str]:
    """POST /_api/detect，返回 (entities, latency_ms, error)。"""
    payload = json.dumps({"text": text}).encode("utf-8")
    req = urllib.request.Request(
        endpoint,
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            elapsed_ms = int((time.perf_counter() - t0) * 1000)
            body = json.loads(raw.decode("utf-8"))
            err = body.get("error", "") or ""
            ents = [
                Detect(
                    type=e["type"],
                    value=e["value"],
                    start=int(e["start"]),
                    end=int(e["end"]),
                )
                for e in (body.get("entities") or [])
            ]
            return ents, elapsed_ms, err
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError) as e:
        elapsed_ms = int((time.perf_counter() - t0) * 1000)
        return [], elapsed_ms, f"{type(e).__name__}: {e}"


def strict_match(expected: list[Expect], detected: list[Detect]) -> tuple[int, int, int]:
    """返回 (tp, fp, fn)。严格四元组 (type, value, start, end) 匹配。"""
    expected_set = {(e.type, e.value, e.start, e.end) for e in expected}
    detected_set = {(d.type, d.value, d.start, d.end) for d in detected}
    tp = len(expected_set & detected_set)
    fp = len(detected_set - expected_set)
    fn = len(expected_set - detected_set)
    return tp, fp, fn


def eval_cases(endpoint: str, cases_path: Path) -> tuple[list[CaseResult], dict[str, PerType]]:
    per_type: dict[str, PerType] = defaultdict(PerType)
    results: list[CaseResult] = []
    latencies: list[int] = []

    for cid, subset, expect, text in load_cases(cases_path):
        detected, lat_ms, err = call_detect(endpoint, text)
        tp, fp, fn = strict_match(expect, detected)
        cr = CaseResult(
            case_id=cid,
            subset=subset,
            n_expected=len(expect),
            n_detected=len(detected),
            tp=tp, fp=fp, fn=fn,
            latency_ms=lat_ms,
            error=err,
        )
        results.append(cr)
        latencies.append(lat_ms)
        # 按 expect/detected 的 type 分别累计（即便类型不匹配也算 FP/FN）
        for e in expect:
            per_type[e.type].fn += 1
            per_type[e.type].tp += 0
        for d in detected:
            per_type[d.type].fp += 1

        # 重新对齐 tp/fp/fn 到 type 维度：上面简化累计可能把 fp/fn 错配到
        # 不同类型（strict_match 已记录全集），这里用 expected/detected 实际配对再校正：
        expected_set = {(e.type, e.value, e.start, e.end) for e in expect}
        detected_set = {(d.type, d.value, d.start, d.end) for d in detected}
        for tp_pair in expected_set & detected_set:
            per_type[tp_pair[0]].tp += 1
            per_type[tp_pair[0]].fp -= 1
            per_type[tp_pair[0]].fn -= 1
        for fp_pair in detected_set - expected_set:
            pass  # 已在 per_type[fp_pair[0]].fp += 1
        for fn_pair in expected_set - detected_set:
            pass  # 已在 per_type[fn_pair[0]].fn += 1

    return results, dict(per_type), latencies


def pct(xs: list[int], q: float) -> int:
    if not xs:
        return 0
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round(q * (len(xs) - 1)))))
    return xs[k]


def render_report(
    engine: str,
    endpoint: str,
    results: list[CaseResult],
    per_type: dict[str, PerType],
    latencies: list[int],
    cases_path: Path,
) -> str:
    n = len(results)
    n_err = sum(1 for r in results if r.error)
    tp_total = sum(r.tp for r in results)
    fp_total = sum(r.fp for r in results)
    fn_total = sum(r.fn for r in results)
    p_total = tp_total / (tp_total + fp_total) if (tp_total + fp_total) else 0.0
    r_total = tp_total / (tp_total + fn_total) if (tp_total + fn_total) else 0.0
    f1_total = 2 * p_total * r_total / (p_total + r_total) if (p_total + r_total) else 0.0

    lines = [
        f"# cn-pii-bench 评估报告 · 引擎 `{engine}`",
        "",
        f"- 端点：`{endpoint}`",
        f"- 语料：`{cases_path}`（{n} 条）",
        f"- 评估时间：{time.strftime('%Y-%m-%d %H:%M:%S')}",
        f"- 匹配口径：**严格四元组** (type, value, start, end)",
        f"- 错误请求：{n_err} / {n}",
        "",
        "## 总体指标",
        "",
        f"| precision | recall | F1 |",
        f"|---|---|---|",
        f"| {p_total:.4f} | {r_total:.4f} | {f1_total:.4f} |",
        "",
        "## 延迟（ms）",
        "",
        f"| p50 | p95 | p99 | max | mean |",
        f"|---|---|---|---|---|",
        f"| {pct(latencies, 0.5)} | {pct(latencies, 0.95)} | {pct(latencies, 0.99)} | {max(latencies or [0])} | {int(statistics.mean(latencies)) if latencies else 0} |",
        "",
        "## 分实体类型",
        "",
        "| 类型 | TP | FP | FN | precision | recall | F1 |",
        "|---|---|---|---|---|---|---|",
    ]

    # 固定类型顺序：先 ALL_TYPES，再补未列出
    seen = set()
    for t in ALL_TYPES:
        if t in per_type:
            seen.add(t)
            s = per_type[t]
            lines.append(
                f"| {t} | {s.tp} | {s.fp} | {s.fn} | "
                f"{s.precision:.4f} | {s.recall:.4f} | {s.f1:.4f} |"
            )
    for t, s in per_type.items():
        if t not in seen:
            lines.append(
                f"| {t} | {s.tp} | {s.fp} | {s.fn} | "
                f"{s.precision:.4f} | {s.recall:.4f} | {s.f1:.4f} |"
            )

    # 错误明细（如有）
    if n_err:
        lines += ["", "## 错误明细", ""]
        for r in results:
            if r.error:
                lines.append(f"- `{r.case_id}` ({r.subset}): {r.error}")

    # 漏报 TopN（按 type 汇总 fn）
    lines += [
        "",
        "## 漏报（FN）按类型汇总",
        "",
        "| 类型 | FN |",
        "|---|---|",
    ]
    fn_by_type = sorted(per_type.items(), key=lambda kv: kv[1].fn, reverse=True)
    for t, s in fn_by_type:
        if s.fn:
            lines.append(f"| {t} | {s.fn} |")

    lines += ["", "---", ""]
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    p.add_argument("--endpoint", required=True, help="网关 /_api/detect 完整 URL")
    p.add_argument("--engine", default="regex", help="引擎名（写入报告）")
    p.add_argument("--cases", default=str(DEFAULT_CASES), help="cases.jsonl 路径")
    p.add_argument("--out", default=str(DEFAULT_OUT), help="报告输出目录")
    args = p.parse_args(argv)

    cases_path = Path(args.cases)
    if not cases_path.exists():
        print(f"FAIL: cases not found: {cases_path}", file=sys.stderr)
        return 1

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[bench] evaluating {cases_path} via {args.endpoint}", file=sys.stderr)
    results, per_type, latencies = eval_cases(args.endpoint, cases_path)

    ts = time.strftime("%Y%m%d-%H%M%S")
    report_path = out_dir / f"phase0_{args.engine}_{ts}.md"
    report = render_report(args.engine, args.endpoint, results, per_type, latencies, cases_path)
    report_path.write_text(report, encoding="utf-8")

    # 控制台简短摘要
    tp_total = sum(r.tp for r in results)
    fp_total = sum(r.fp for r in results)
    fn_total = sum(r.fn for r in results)
    p_total = tp_total / (tp_total + fp_total) if (tp_total + fp_total) else 0.0
    r_total = tp_total / (tp_total + fn_total) if (tp_total + fn_total) else 0.0
    f1_total = 2 * p_total * r_total / (p_total + r_total) if (p_total + r_total) else 0.0
    print(
        f"[bench] {args.engine}: precision={p_total:.4f} recall={r_total:.4f} F1={f1_total:.4f} "
        f"(TP={tp_total} FP={fp_total} FN={fn_total}, p50={pct(latencies, 0.5)}ms "
        f"p95={pct(latencies, 0.95)}ms p99={pct(latencies, 0.99)}ms)",
        file=sys.stderr,
    )
    print(f"[bench] report -> {report_path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())