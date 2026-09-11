#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
bench/generate_en.py —— 英文 PII 语料生成器（2026-09-11 双语基线）。

产出 bench/fixtures/cases_en.jsonl：6 个子集 × 30 篇 = 180 条英文 PII 语料，
每条带 ground truth（type / value / UTF-8 字节偏移 start,end）。

设计原则（与 generate.py 中文版完全一致，保证两种语言下行为可对比）：
- 生成值与 detector 实际使用的校验算法一致：
  * US SSN → pkg/global.ValidUSSSN（SSA 规则：area 001-899 除 666，
    group 01-99，serial 0001-9999）
  * 信用卡 → pkg/global.ValidCreditCard（Luhn）+ IsInternationalCard（IIN 前缀）
  * URL → pkg/global.ValidURL（http/https + host）
- 弱格式实体（英文车牌）遵循「强上下文」约束（rePlateEN 要求
  license/vehicle/registration plate 引导词），只保证精确率。
- 偏移按 UTF-8 字节计算（中文字牌在英文文本中也用字节偏移）。
- 确定性：固定 seed，可复现。

子集（与中文版 8 子集对齐 + 英文特有）：
  us_ssn       — 美国 SSN（强格式）
  credit_card  — 国际信用卡 Visa/MC/Amex/Discover/JCB（强格式 + IIN）
  url          — URL（强格式）
  plate_en     — 英文车牌（弱格式，上下文引导）
  plate_zh     — 中文车牌嵌在英文上下文（双语交叉）
  mixed        — 中英混合多实体

用法：
    python3 bench/generate_en.py                 # 写到 bench/fixtures/cases_en.jsonl
    python3 bench/generate_en.py --out x.jsonl   # 指定输出
    python3 bench/generate_en.py --self-check     # 仅自检
"""

import argparse
import json
import os
import random
import sys

CN = os.path.dirname(os.path.abspath(__file__))
DEFAULT_OUT = os.path.join(CN, "fixtures", "cases_en.jsonl")
SEED = 20260911
PER_SUBSET = 30

# ---- 与 pkg/global 保持一致的算法 ------------------------------------------------

def luhn_check_digit(partial: str) -> str:
    s = 0
    alt = True
    for c in reversed(partial):
        d = int(c)
        if alt:
            d *= 2
            if d > 9:
                d -= 9
        s += d
        alt = not alt
    return str((10 - s % 10) % 10)


def gen_us_ssn(rnd) -> str:
    """SSA 规则：area 001-899（除 666），group 01-99，serial 0001-9999。"""
    area = rnd.randint(1, 899)
    while area == 666:
        area = rnd.randint(1, 899)
    group = rnd.randint(1, 99)
    serial = rnd.randint(1, 9999)
    return "%03d-%02d-%04d" % (area, group, serial)


# 国际信用卡 IIN 前缀（与 pkg/global.IsInternationalCard 对齐）
# 元组 = (前缀, 随机位数)：总长 = len(前缀) + 随机位数 + 1 位 Luhn
CARD_BRANDS = [
    ("4", 14),        # Visa 16
    ("55", 13),       # MasterCard 16
    ("51", 13),       # MasterCard 16
    ("37", 12),       # Amex 15
    ("34", 12),       # Amex 15
    ("6011", 11),     # Discover 16
    ("65", 13),       # Discover 16
    ("3530", 11),     # JCB 16
]


def gen_credit_card(rnd) -> str:
    prefix, body_len = rnd.choice(CARD_BRANDS)
    body = prefix + "".join(rnd.choice("0123456789") for _ in range(body_len))
    return body + luhn_check_digit(body)


def gen_url(rnd) -> str:
    host = rnd.choice([
        "docs.example.com", "api.example.org", "shop.example.net",
        "status.example.io", "cdn.example.dev",
    ])
    path = rnd.choice([
        "", "/setup", "/api/v2/users", "/checkout", "/reports/q4",
        "/onboarding?step=2", "/files/report.pdf",
    ])
    scheme = rnd.choice(["https", "https", "https", "http"])  # https 为主
    return "%s://%s%s" % (scheme, host, path)


def gen_plate_en(rnd) -> str:
    """2-3 大写字母 + 可选连字符 + 3-4 数字 + 可选尾字母（rePlateEN 形态）。"""
    letters = "".join(rnd.choice("ABCDEFGHJKLMNPRSTUVWXYZ") for _ in range(rnd.choice([2, 3])))
    sep = rnd.choice(["", "-", "-"])
    digits = "%d" % rnd.randint(100, 9999)
    tail = rnd.choice(["", "", "X", "B"])
    return letters + sep + digits + tail


def gen_plate_zh(rnd) -> str:
    """中文车牌：省份简称 + 字母 + 5-6 位字母数字（rePlate 形态）。"""
    prov = rnd.choice("京沪粤浙苏川渝")
    letter = rnd.choice("ABCDEFGHJKLMNPRSTUVWXYZ")
    n = rnd.choice([5, 6])
    body = "".join(rnd.choice("0123456789ABCDEFGHJKLMNPRSTUVWXYZ") for _ in range(n))
    return prov + letter + body


def gen_email(rnd) -> str:
    local = rnd.choice(["john.doe", "jane_smith", "r.brown", "m.wilson", "alex.tan", "sam.lee"])
    dom = rnd.choice(["example.com", "mail.example.org", "corp.example.io"])
    return "%s@%s" % (local, dom)


def gen_ip(rnd) -> str:
    return ".".join(str(rnd.randint(1, 223)) for _ in range(4))


# ---- 偏移工具（与中文版一致） ------------------------------------------------

def byte_span(text: str, value: str):
    """返回 value 在 text 中的 UTF-8 字节偏移 (start,end)。"""
    ci = text.index(value)
    bstart = len(text[:ci].encode("utf-8"))
    bend = bstart + len(value.encode("utf-8"))
    return bstart, bend


# ---- 子集构造 ----------------------------------------------------------------

def build_us_ssn(rnd):
    v = gen_us_ssn(rnd)
    text = "The employee's SSN is %s for payroll processing." % v
    s, e = byte_span(text, v)
    return text, [{"type": "us_ssn", "value": v, "start": s, "end": e}]


def build_credit_card(rnd):
    v = gen_credit_card(rnd)
    text = "Please charge the order to card %s before shipping." % v
    s, e = byte_span(text, v)
    return text, [{"type": "credit_card", "value": v, "start": s, "end": e}]


def build_url(rnd):
    v = gen_url(rnd)
    # 一半句子 URL 居中（后跟空格），一半句尾（后跟句号——正则应排除句号）
    if rnd.random() < 0.5:
        text = "Visit %s for the full documentation." % v
    else:
        text = "You can download the archive from %s." % v
    s, e = byte_span(text, v)
    return text, [{"type": "url", "value": v, "start": s, "end": e}]


def build_plate_en(rnd):
    v = gen_plate_en(rnd)
    lead = rnd.choice([
        "The vehicle's license plate",
        "His license plate number",
        "The registration plate",
    ])
    text = "%s: %s was reported stolen." % (lead, v)
    s, e = byte_span(text, v)
    return text, [{"type": "plate", "value": v, "start": s, "end": e}]


def build_plate_zh(rnd):
    v = gen_plate_zh(rnd)
    text = "My Beijing-registered car has plate %s since 2023." % v
    s, e = byte_span(text, v)
    return text, [{"type": "plate", "value": v, "start": s, "end": e}]


def build_mixed(rnd):
    """中英混合多实体——验证「覆盖范围与中文规则一致」的核心场景。"""
    email = gen_email(rnd)
    ip = gen_ip(rnd)
    ssn = gen_us_ssn(rnd)
    card = gen_credit_card(rnd)
    plate = gen_plate_zh(rnd)
    url = gen_url(rnd)
    text = ("Contact %s from %s. The customer SSN %s pays with card %s, "
            "vehicle plate %s, see %s for the order." % (email, ip, ssn, card, plate, url))
    ents = []
    for v, t in [(email, "email"), (ip, "ip_address"), (ssn, "us_ssn"),
                 (card, "credit_card"), (plate, "plate"), (url, "url")]:
        s, e = byte_span(text, v)
        ents.append({"type": t, "value": v, "start": s, "end": e})
    return text, ents


BUILDERS = {
    "us_ssn": build_us_ssn,
    "credit_card": build_credit_card,
    "url": build_url,
    "plate_en": build_plate_en,
    "plate_zh": build_plate_zh,
    "mixed": build_mixed,
}


# ---- 主流程（与中文版同构） ------------------------------------------------------

def generate(rnd):
    cases = []
    for subset, builder in BUILDERS.items():
        for i in range(PER_SUBSET):
            text, ents = builder(rnd)
            cases.append({
                "id": "en-%s-%03d" % (subset, i + 1),
                "subset": subset,
                "text": text,
                "expect": ents,
            })
    return cases


def self_check(cases):
    errors = 0
    for c in cases:
        t = c["text"]
        tb = t.encode("utf-8")
        for g in c["expect"]:
            seg = tb[g["start"]:g["end"]].decode("utf-8", "replace")
            if seg != g["value"]:
                errors += 1
                print("  OFFSET MISMATCH %s: expect=%r got=%r" % (c["id"], g["value"], seg), file=sys.stderr)
            if g["start"] < 0 or g["end"] > len(tb) or g["start"] >= g["end"]:
                errors += 1
                print("  RANGE ERROR %s: %r" % (c["id"], g), file=sys.stderr)
    return errors


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=DEFAULT_OUT)
    ap.add_argument("--self-check", action="store_true", help="仅做内部一致性自检，不写文件")
    args = ap.parse_args()

    rnd = random.Random(SEED)
    cases = generate(rnd)
    errs = self_check(cases)
    if errs:
        print("SELF-CHECK FAILED: %d errors" % errs, file=sys.stderr)
        sys.exit(1)
    print("self-check OK: %d cases, %d entities" %
          (len(cases), sum(len(c["expect"]) for c in cases)))

    if args.self_check:
        return

    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as f:
        for c in cases:
            f.write(json.dumps(c, ensure_ascii=False) + "\n")
    print("wrote %s (%d cases)" % (args.out, len(cases)))


if __name__ == "__main__":
    main()
