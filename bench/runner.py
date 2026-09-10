#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""bench/runner.py —— cn-pii-bench 0.4 评估器（契约 §9）。

对比 fixtures/cases.jsonl 的人工标注 ground truth 与网关 /_api/detect 的
实际输出，按精确匹配（type + value + [start, end)）计算各实体类型的
precision / recall / F1，并统计请求延迟 p50 / p95 / p99，输出 Markdown
报告到 bench/reports/<timestamp>.md。

⚠️ 冲突标注（2026-09-10，待裁决，见 SPEC_ALIGNMENT.md C5 / Q5）：
本评估器与 Go 版 cmd/bench-runner 并存，两者口径不同、数字不可直接比较：
  - 本文件：经网关 /_api/detect，严格四元组 (type, value, start, end)，当前基线 F1=1.0 / p99=23ms
  - cmd/bench-runner：直连 RegexEngine，"同类型 + 区间重叠"，DECISION.md §3 记录的是它修复前的 F1=0.971
CI 的 bench-baseline job 跑的是 Go 版（召回 <0.9 守门），对外汇报用的是本文件。
方案① 统一到本文件（口径更严、与 privaite-bench 一致，需改 CI）：数字唯一，但失去直连引擎的快速守门。
方案② 两者并存但显式标注（当前处置）：零风险，代价是两套数字需人工分辨。
当前未删除任一评估器、未改 CI。

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
    # 逐条明细（借鉴 privaite-bench 的 fp_items / fn_items：汇总看不出为什么错）
    fp_items: list = field(default_factory=list)
    fn_items: list = field(default_factory=list)


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
        # 逐条明细：汇总数字无法回答"到底哪一个错了"
        for fp_pair in sorted(detected_set - expected_set):
            cr.fp_items.append({"type": fp_pair[0], "value": fp_pair[1],
                                "start": fp_pair[2], "end": fp_pair[3]})
        for fn_pair in sorted(expected_set - detected_set):
            cr.fn_items.append({"type": fn_pair[0], "value": fn_pair[1],
                                "start": fn_pair[2],
                                "end": fn_pair[3]})

    return results, dict(per_type), latencies


def pct(xs: list[int], q: float) -> int:
    if not xs:
        return 0
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round(q * (len(xs) - 1)))))
    return xs[k]


# --------------------------------------------------------------------------
# 自检：进程内假端点，验证评估器本身没坏
# --------------------------------------------------------------------------

def selftest() -> int:
    """双工况自检：完美实现 → F1=1.0；故意坏实现 → F1<1.0 且被守住。

    跑 40 条子集（假端点只认这 40 条；全跑 240 条会得到海量 FN，掩盖真信号）。
    """
    import http.server
    import tempfile
    import threading

    cases = []
    for cid, subset, expect, text in load_cases(DEFAULT_CASES):
        cases.append({"id": cid, "subset": subset, "text": text,
                      "expect": [{"type": e.type, "value": e.value,
                                  "start": e.start, "end": e.end}
                                 for e in expect]})
        if len(cases) >= 40:
            break
    tmp = tempfile.NamedTemporaryFile(
        mode="w", suffix=".jsonl", delete=False, encoding="utf-8")
    for c in cases:
        tmp.write(json.dumps(c, ensure_ascii=False) + "\n")
    tmp.close()
    sub_path = Path(tmp.name)

    def run(perfect: bool) -> tuple:
        text_to_ents: dict[str, list[dict]] = {}
        for c in cases:
            if perfect:
                text_to_ents[c["text"]] = c["expect"]
            else:
                half = c["expect"][:len(c["expect"]) // 2]
                text_to_ents[c["text"]] = list(half) + [
                    {"type": "phone", "value": "00000000000",
                     "start": 0, "end": 11}]

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):  # noqa: N802
                n = int(self.headers.get("Content-Length", 0))
                req = json.loads(self.rfile.read(n) or b"{}")
                ents = text_to_ents.get(req.get("text", ""), [])
                raw = json.dumps({"entities": ents}).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            ep = f"http://127.0.0.1:{srv.server_address[1]}/_api/detect"
            results, _per_type, _lat = eval_cases(ep, sub_path)
            tp = sum(r.tp for r in results)
            fp = sum(r.fp for r in results)
            fn = sum(r.fn for r in results)
            p = tp / (tp + fp) if (tp + fp) else 0.0
            r = tp / (tp + fn) if (tp + fn) else 0.0
            f1 = 2 * p * r / (p + r) if (p + r) else 0.0
            return len(results), tp, fp, fn, p, r, f1
        finally:
            srv.shutdown()

    n, tp, fp, fn, p, r, f1 = run(perfect=True)
    print(f"[selftest/perfect]  n={n} TP={tp} FP={fp} FN={fn} "
          f"P={p:.4f} R={r:.4f} F1={f1:.4f}")
    if f1 < 0.99:
        print("  FAIL  完美实现本应 F1≈1.0 —— 评估器有 bug")
        return 1

    n, tp, fp, fn, p, r, f1 = run(perfect=False)
    print(f"[selftest/broken ]  n={n} TP={tp} FP={fp} FN={fn} "
          f"P={p:.4f} R={r:.4f} F1={f1:.4f}")
    if f1 >= 0.99:
        print("  FAIL  坏实现没被抓出来 —— 评估器是瞎的")
        return 1

    print("SELFTEST PASSED")
    return 0


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

    # 分子集（subset 之前采了但从未进报告——整体 F1 会把某个子集的塌方抹平）
    by_subset: dict[str, list[int]] = {}
    for r in results:
        acc = by_subset.setdefault(r.subset, [0, 0, 0, 0])  # tp, fp, fn, n
        acc[0] += r.tp
        acc[1] += r.fp
        acc[2] += r.fn
        acc[3] += 1
    if by_subset:
        lines += ["", "## 分子集", "",
                  "| 子集 | 条数 | TP | FP | FN | precision | recall | F1 |",
                  "|---|---|---|---|---|---|---|---|"]
        for sub, (tp, fp, fn, cnt) in sorted(by_subset.items()):
            pp = tp / (tp + fp) if (tp + fp) else 0.0
            rr = tp / (tp + fn) if (tp + fn) else 0.0
            ff = 2 * pp * rr / (pp + rr) if (pp + rr) else 0.0
            lines.append(f"| {sub} | {cnt} | {tp} | {fp} | {fn} | "
                         f"{pp:.4f} | {rr:.4f} | {ff:.4f} |")

    # 错误明细（如有）
    if n_err:
        lines += ["", "## 错误明细", ""]
        for r in results:
            if r.error:
                lines.append(f"- `{r.case_id}` ({r.subset}): {r.error}")

    # FP / FN 逐条明细（借鉴 privaite-bench：只给汇总数字等于不给线索）
    fp_all = [(r.case_id, r.subset, i) for r in results for i in r.fp_items]
    fn_all = [(r.case_id, r.subset, i) for r in results for i in r.fn_items]
    lines += ["", "## 误报（FP）逐条", ""]
    if not fp_all:
        lines.append("无。")
    else:
        lines += ["| case | 子集 | 类型 | 值 | 区间 |", "|---|---|---|---|---|"]
        for cid, sub, i in fp_all[:50]:
            lines.append(f"| `{cid}` | {sub} | {i['type']} | `{i['value']}` | "
                         f"[{i['start']},{i['end']}) |")
        if len(fp_all) > 50:
            lines.append(f"（共 {len(fp_all)} 条，此处省略 {len(fp_all) - 50} 条，全量见 JSON 报告）")

    lines += ["", "## 漏报（FN）逐条", ""]
    if not fn_all:
        lines.append("无。")
    else:
        lines += ["| case | 子集 | 类型 | 值 | 区间 |", "|---|---|---|---|---|"]
        for cid, sub, i in fn_all[:50]:
            lines.append(f"| `{cid}` | {sub} | {i['type']} | `{i['value']}` | "
                         f"[{i['start']},{i['end']}) |")
        if len(fn_all) > 50:
            lines.append(f"（共 {len(fn_all)} 条，此处省略 {len(fn_all) - 50} 条，全量见 JSON 报告）")

    # 诚实声明：语料是自作者合成，F1 不构成真实场景结论
    lines += [
        "",
        "## 口径声明（必读）",
        "",
        "> 本语料由 `generate.py` 合成，**不是真实流量，也没有第三方独立标注**。",
        "> 在自作者语料上自评，F1 高只说明「检测器与生成器对同一套仿真规则达成一致」，",
        "> **不等于真实场景召回率**。参照 privaite-bench 的做法（用 AI4Privacy / Gretel /",
        "> Nemotron 三方标签交叉校验），在拿到**人工或第三方标注子集**之前，",
        "> 本报告数字不得用于对外宣称。",
        "",
        "---",
        "",
    ]
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    p.add_argument("--endpoint", default="", help="网关 /_api/detect 完整 URL（selftest 时不需要）")
    p.add_argument("--engine", default="regex", help="引擎名（写入报告）")
    p.add_argument("--cases", default=str(DEFAULT_CASES), help="cases.jsonl 路径")
    p.add_argument("--out", default=str(DEFAULT_OUT), help="报告输出目录")
    p.add_argument("--gate", action="store_true", help="守门模式：F1<0.9 退出码 2（供 CI 用）")
    p.add_argument("--selftest", action="store_true",
                   help="不启网关，自检评估器：假端点 F1 必须 = 1.0，伪造漏报必须 < 1.0")
    args = p.parse_args(argv)

    if args.selftest:
        return selftest()

    if not args.endpoint:
        p.error("--endpoint is required (unless --selftest)")

    cases_path = Path(args.cases)
    if not cases_path.exists():
        print(f"FAIL: cases not found: {cases_path}", file=sys.stderr)
        return 1

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"[bench] evaluating {cases_path} via {args.endpoint}", file=sys.stderr)
    results, per_type, latencies = eval_cases(args.endpoint, cases_path)

    ts = time.strftime("%Y%m%d-%H%M%S")
    md_path = out_dir / f"phase0_{args.engine}_{ts}.md"
    js_path = out_dir / f"phase0_{args.engine}_{ts}.json"
    report = render_report(args.engine, args.endpoint, results, per_type, latencies, cases_path)
    md_path.write_text(report, encoding="utf-8")

    # 机读 JSON 报告（供 CI / 第三方消费；借鉴 privaite-bench 的 results/*.json）
    by_subset: dict[str, dict] = {}
    for r in results:
        s = by_subset.setdefault(r.subset, {"tp": 0, "fp": 0, "fn": 0, "n": 0})
        s["tp"] += r.tp; s["fp"] += r.fp; s["fn"] += r.fn; s["n"] += 1
    js = {
        "engine": args.engine,
        "endpoint": args.endpoint,
        "cases": str(cases_path),
        "n": len(results),
        "timestamp": ts,
        "metrics": {
            "precision": sum(r.tp for r in results) /
                max(1, sum(r.tp + r.fp for r in results)),
            "recall": sum(r.tp for r in results) /
                max(1, sum(r.tp + r.fn for r in results)),
        },
        "latency_ms": {
            "p50": pct(latencies, 0.5), "p95": pct(latencies, 0.95),
            "p99": pct(latencies, 0.99), "max": max(latencies or [0]),
            "mean": int(statistics.mean(latencies)) if latencies else 0,
        },
        "per_type": {t: {"tp": s.tp, "fp": s.fp, "fn": s.fn,
                          "precision": s.precision, "recall": s.recall, "f1": s.f1}
                     for t, s in per_type.items()},
        "per_subset": by_subset,
        "fp_items": [{"case": r.case_id, "subset": r.subset, **i}
                     for r in results for i in r.fp_items],
        "fn_items": [{"case": r.case_id, "subset": r.subset, **i}
                     for r in results for i in r.fn_items],
    }
    f1 = js["metrics"]["precision"] + js["metrics"]["recall"]
    js["metrics"]["f1"] = (2 * js["metrics"]["precision"] * js["metrics"]["recall"] / f1
                           if f1 else 0.0)
    js_path.write_text(json.dumps(js, ensure_ascii=False, indent=2), encoding="utf-8")

    # 控制台简短摘要
    tp_total = sum(r.tp for r in results)
    fp_total = sum(r.fp for r in results)
    fn_total = sum(r.fn for r in results)
    p_total = js["metrics"]["precision"]
    r_total = js["metrics"]["recall"]
    f1_total = js["metrics"]["f1"]
    print(
        f"[bench] {args.engine}: precision={p_total:.4f} recall={r_total:.4f} F1={f1_total:.4f} "
        f"(TP={tp_total} FP={fp_total} FN={fn_total}, p50={pct(latencies, 0.5)}ms "
        f"p95={pct(latencies, 0.95)}ms p99={pct(latencies, 0.99)}ms)",
        file=sys.stderr,
    )
    print(f"[bench] md  -> {md_path}", file=sys.stderr)
    print(f"[bench] json-> {js_path}", file=sys.stderr)

    if args.gate and f1_total < 0.9:
        print(f"[bench] GATE FAILED: F1={f1_total:.4f} < 0.9", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())