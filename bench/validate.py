#!/usr/bin/env python3
"""bench/validate.py —— 校验 bench/fixtures/cases.jsonl 的结构与一致性。

可作 E2E 自检 / CI 步骤的轻量 gate（不依赖 Go）。同时打印每个 case 的实体数。
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
CASES = ROOT / "fixtures" / "cases.jsonl"
SCHEMA = ROOT / "fixtures" / "cases.schema.json"


def main() -> int:
    if not CASES.exists():
        print(f"FAIL: missing {CASES}")
        return 1

    n_total = 0
    n_pos = 0
    n_neg = 0
    type_counter: dict[str, int] = {}
    errors: list[str] = []

    with CASES.open("r", encoding="utf-8") as f:
        for lineno, line in enumerate(f, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                c = json.loads(line)
            except json.JSONDecodeError as e:
                errors.append(f"line {lineno}: invalid JSON: {e}")
                continue

            n_total += 1
            text: str = c.get("text", "")
            cid: str = c.get("id", f"line{lineno}")
            expect = c.get("expect", [])
            if not expect:
                n_neg += 1
                continue

            n_pos += 1
            # detector（pkg/types.Entity）的 start/end 为 UTF-8 字节偏移，
            # 因此这里按字节切片比较，不能用 Python 字符串的字符索引。
            tb = text.encode("utf-8")
            for j, e in enumerate(expect):
                t = e.get("type", "")
                v = e.get("value", "")
                s = e.get("start", -1)
                ed = e.get("end", -1)
                type_counter[t] = type_counter.get(t, 0) + 1
                if s < 0:
                    errors.append(f"{cid}[{j}]: start 必须 ≥ 0")
                if ed > len(tb):
                    errors.append(f"{cid}[{j}]: end {ed} > len(text bytes) {len(tb)}")
                if ed <= s:
                    errors.append(f"{cid}[{j}]: end 必须 > start")
                if 0 <= s < ed <= len(tb):
                    seg = tb[s:ed].decode("utf-8", "replace")
                    if seg != v:
                        errors.append(
                            f"{cid}[{j}]: value={v!r} != text[{s}:{ed}]={seg!r}"
                        )

    print("=== bench/validate.py 报告 ===")
    print(f"total cases: {n_total}  positive: {n_pos}  negative: {n_neg}")
    print(f"实体按类型统计:")
    for t, n in sorted(type_counter.items()):
        print(f"  {t}: {n}")

    if errors:
        print()
        print(f"FAIL ({len(errors)} 错误):")
        for e in errors[:20]:
            print(f"  - {e}")
        if len(errors) > 20:
            print(f"  ... 还有 {len(errors)-20} 项未显示")
        return 1
    print()
    print("PASS: 全部样本结构合法")
    return 0


if __name__ == "__main__":
    sys.exit(main())
