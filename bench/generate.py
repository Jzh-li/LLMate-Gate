#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
cn-pii-bench 语料生成器（SPEC 任务 0.1）。

产出 bench/fixtures/cases.jsonl：8 个子集 × 30 篇 = 240 条中文 PII 语料，
每条带人工标注的 ground truth（type / value / UTF-8 字节偏移 start,end）。

设计原则（保证评测不失真）：
- 生成值与 detector 实际使用的校验算法一致：
  * 手机号段 → pkg/cn.PhonePrefixes + ValidPhone
  * 身份证校验位 → pkg/cn.IDCardChecksum（GB 11643）
  * 银行卡 Luhn → pkg/cn.LuhnCheckDigit
- 人名/地址遵循 regex 引擎的「强上下文 / 行政区划关键词」约束，保证可检出。
- 偏移按 UTF-8 字节计算，与 detector（pkg/types.Entity.Start/End 为字节偏移）对齐。
- 确定性：固定 seed，可复现。

用法：
    python3 bench/generate.py                 # 默认写到 bench/fixtures/cases.jsonl
    python3 bench/generate.py --out x.jsonl   # 指定输出
    python3 bench/generate.py --self-check     # 仅做内部一致性自检后退出（不写文件）
"""

import argparse
import json
import os
import random
import sys

CN = os.path.dirname(os.path.abspath(__file__))
DEFAULT_OUT = os.path.join(CN, "fixtures", "cases.jsonl")
SEED = 20260910
PER_SUBSET = 30

# ---- 与 pkg/cn 保持一致的算法 ------------------------------------------------

PHONE_PREFIXES = [
    "130", "131", "132", "133", "134", "135", "136", "137", "138", "139",
    "145", "146", "147", "148", "149",
    "150", "151", "152", "153", "155", "156", "157", "158", "159",
    "166", "167",
    "170", "171", "172", "173", "174", "175", "176", "177", "178", "179",
    "180", "181", "182", "183", "184", "185", "186", "187", "188", "189",
    "190", "191", "192", "193", "195", "196", "197", "198", "199",
]


def id_card_checksum(body17: str) -> str:
    weights = [7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2]
    check_map = "10X98765432"
    s = sum(int(c) * weights[i] for i, c in enumerate(body17))
    return check_map[s % 11]


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


# ---- 值生成器 ----------------------------------------------------------------

def gen_phone(rnd) -> str:
    return rnd.choice(PHONE_PREFIXES) + "".join(rnd.choice("0123456789") for _ in range(8))


def gen_id_card(rnd) -> str:
    # 公开省级代码 + 市级 + 区级（不对应真实自然人）
    region = rnd.choice(["110101", "310115", "440305", "510107", "320106", "330106"])
    birth = "%04d%02d%02d" % (rnd.choice([1985, 1990, 1995, 2000, 2001]), rnd.randint(1, 12), rnd.randint(1, 28))
    seq = "%03d" % rnd.randint(0, 999)
    body17 = region + birth + seq
    return body17 + id_card_checksum(body17)


def gen_bank_card(rnd) -> str:
    # 【2026-09-11】62 前缀（银联标准）+ 18 位（借记卡常见长度）。
    # 仍消耗 15 次 rnd.choice，保证下游子集（address/tool_call/mixed/adversarial）
    # 的随机序列与旧版完全一致，只有 bank_card 子集的值本身变化。
    # 动机：cardDispatch 按 IIN 前缀分发，随机首位的卡号会命中国际 IIN
    # （4=Visa 等，约 27% 概率）被判为 credit_card，打穿中文 bank_card F1。
    # 真实中国银行卡即 62 开头，语料本就该如此。
    partial = "62" + "".join(rnd.choice("0123456789") for _ in range(15))  # 17 位
    return partial + luhn_check_digit(partial)  # 18 位，62 前缀，Luhn 合法


def gen_email(rnd) -> str:
    local = rnd.choice(["zhangwei", "li.na", "wang_fang", "zhaomin", "chenjie", "liu.yang"])
    dom = rnd.choice(["example.com", "mail.cn", "corp.io", "service.net"])
    return "%s@%s" % (local, dom)


def gen_ip(rnd) -> str:
    return ".".join(str(rnd.randint(1, 223)) for _ in range(4))


def gen_date(rnd) -> str:
    return "%04d-%02d-%02d" % (rnd.choice([2023, 2024, 2025]), rnd.randint(1, 12), rnd.randint(1, 28))


def gen_name(rnd) -> str:
    # 复姓优先随机，单姓补充；首字均在常见姓氏表内（detector 强约束）
    compound = rnd.random() < 0.15
    surnames = ["张", "李", "王", "赵", "欧阳", "司马", "上官", "诸葛", "陈", "刘", "杨", "黄", "周", "吴"]
    given = rnd.choice(["伟", "娜", "芳", "敏", "杰", "洋", "静", "磊", "强", "丽", "宇", "悦", "晨", "航"])
    sur = rnd.choice(surnames)
    if compound and len(sur) == 2:
        return sur + given
    if len(sur) == 2:  # 复姓但本轮回退单字给字
        return sur + given
    return sur + given + (rnd.choice(["", "轩", "琪"]) if rnd.random() < 0.4 else "")


def gen_address(rnd) -> str:
    prov = rnd.choice(["广东省", "北京市", "上海市", "浙江省", "江苏省", "四川省"])
    city = rnd.choice(["深圳市", "海淀区", "浦东新区", "杭州市", "南京市", "成都市"])
    road = rnd.choice(["科技园路", "中关村大街", "南京西路", "文三路", "天府大道", "软件大道"])
    return "%s%s%s%s号" % (prov, city, road, rnd.randint(1, 299))


# ---- 偏移工具 ----------------------------------------------------------------

def byte_span(text: str, value: str):
    """返回 value 在 text 中的 UTF-8 字节偏移 (start,end)。value 必须作为连续子串出现。"""
    ci = text.index(value)
    bstart = len(text[:ci].encode("utf-8"))
    bend = bstart + len(value.encode("utf-8"))
    return bstart, bend


# ---- 子集构造 ----------------------------------------------------------------

def build_person_name(rnd):
    name = gen_name(rnd)
    text = "我叫%s，是本次项目的对接人，后续邮件往来请找我。" % name
    s, e = byte_span(text, name)
    return text, [{"type": "zh_person_name", "value": name, "start": s, "end": e}]


def build_phone(rnd):
    v = gen_phone(rnd)
    text = "客户的联系电话是 %s，请在工作时间回访，勿发短信。" % v
    s, e = byte_span(text, v)
    return text, [{"type": "zh_phone", "value": v, "start": s, "end": e}]


def build_id_card(rnd):
    v = gen_id_card(rnd)
    text = "身份证号：%s（仅用于本次实名认证，不作他用）。" % v
    s, e = byte_span(text, v)
    return text, [{"type": "zh_id_card", "value": v, "start": s, "end": e}]


def build_bank_card(rnd):
    v = gen_bank_card(rnd)
    text = "收款账户 %s 归属招商银行，请核对后再转账。" % v
    s, e = byte_span(text, v)
    return text, [{"type": "zh_bank_card", "value": v, "start": s, "end": e}]


def build_address(rnd):
    v = gen_address(rnd)
    text = "收件地址：%s，收件人请保持电话畅通。" % v
    s, e = byte_span(text, v)
    return text, [{"type": "zh_address", "value": v, "start": s, "end": e}]


def build_tool_call(rnd):
    # 模拟 LLM tool-call 的 arguments JSON 内嵌 PII
    phone = gen_phone(rnd)
    addr = gen_address(rnd)
    args = '{"action":"create_order","contact_phone":"%s","ship_to":"%s"}' % (phone, addr)
    text = '{"name":"create_order","arguments":%s}' % json.dumps(args, ensure_ascii=False)
    p_s, p_e = byte_span(text, phone)
    ents = [{"type": "zh_phone", "value": phone, "start": p_s, "end": p_e}]
    a_s, a_e = byte_span(text, addr)
    ents.append({"type": "zh_address", "value": addr, "start": a_s, "end": a_e})
    return text, ents


def build_mixed(rnd):
    name = gen_name(rnd)
    phone = gen_phone(rnd)
    email = gen_email(rnd)
    idc = gen_id_card(rnd)
    text = ("用户 %s（手机 %s，邮箱 %s）提交认证，证件 %s，请审核。" %
            (name, phone, email, idc))
    n_s, n_e = byte_span(text, name)
    p_s, p_e = byte_span(text, phone)
    em_s, em_e = byte_span(text, email)
    id_s, id_e = byte_span(text, idc)
    ents = [
        {"type": "zh_person_name", "value": name, "start": n_s, "end": n_e},
        {"type": "zh_phone", "value": phone, "start": p_s, "end": p_e},
        {"type": "email", "value": email, "start": em_s, "end": em_e},
        {"type": "zh_id_card", "value": idc, "start": id_s, "end": id_e},
    ]
    return text, ents


def build_adversarial(rnd):
    """对抗子集：上下文刻意制造干扰，仍保留连续干净的 PII 子串（偏移可精确标注）。
    目的：诚实暴露 regex 引擎在复杂上下文下的召回边界，而非伪造 100% 召回。"""
    kind = rnd.randint(0, 3)
    if kind == 0:  # 紧贴其他文字、长句中
        phone = gen_phone(rnd)
        text = "备注栏请填写紧急联系人电话%s以及关系说明后提交审核" % phone
        s, e = byte_span(text, phone)
        return text, [{"type": "zh_phone", "value": phone, "start": s, "end": e}]
    if kind == 1:  # 身份证被中文包裹
        idc = gen_id_card(rnd)
        text = "持证人的法定证件编号依次为%s现予登记备案" % idc
        s, e = byte_span(text, idc)
        return text, [{"type": "zh_id_card", "value": idc, "start": s, "end": e}]
    if kind == 2:  # 邮箱夹在 URL 风格文本中
        email = gen_email(rnd)
        text = "回执将发送至账户绑定邮箱%s如未收到请检查垃圾箱" % email
        s, e = byte_span(text, email)
        return text, [{"type": "email", "value": email, "start": s, "end": e}]
    # kind == 3：银行卡与区号数字相邻但用横杠分隔（digitBoundary 不应误伤）
    bank = gen_bank_card(rnd)
    text = "对公账号-%s-开户行代码-102" % bank
    s, e = byte_span(text, bank)
    return text, [{"type": "zh_bank_card", "value": bank, "start": s, "end": e}]


BUILDERS = {
    "person_name": build_person_name,
    "phone": build_phone,
    "id_card": build_id_card,
    "bank_card": build_bank_card,
    "address": build_address,
    "tool_call": build_tool_call,
    "mixed": build_mixed,
    "adversarial": build_adversarial,
}


# ---- 主流程 ------------------------------------------------------------------

def generate(rnd):
    cases = []
    for subset, builder in BUILDERS.items():
        for i in range(PER_SUBSET):
            text, ents = builder(rnd)
            cases.append({
                "id": "%s-%03d" % (subset, i + 1),
                "subset": subset,
                "text": text,
                "expect": ents,
            })
    return cases


def self_check(cases):
    """校验每条 ground truth 的 value 与字节偏移自洽。"""
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
    with open(args.out, "w", encoding="utf-8", newline="\n") as f:
        for c in cases:
            f.write(json.dumps(c, ensure_ascii=False) + "\n")
    print("wrote %s" % args.out)


if __name__ == "__main__":
    main()
