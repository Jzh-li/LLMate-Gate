#!/usr/bin/env python3
"""LLMate-Gate 多轮端到端延迟基准客户端（收口 V1_READINESS R3 口径）。

测量三类指标，全部用标准库实现（无第三方依赖，CI / 本地同款）：
  1. gateway 非流式多轮往返延迟（对话上下文逐轮增长，模拟 Agent 记忆膨胀）
  2. 直达 mock 上游的基线往返延迟（不经网关）
  3. gateway 流式（SSE）首字延迟 TTFT 与总耗时

输出 p50/p95/p99/max，并计算「网关附加延迟」= (gateway - baseline) 的逐轮差值分布，
以此隔离出 PII 检测+替换+还原+额外一跳的真实开销。

默认非门禁（只量不拦）；加 --fail-overhead-ms N 可变为门禁（p99 超阈值即非零退出）。
"""
import sys
import json
import time
import argparse
import urllib.request
import urllib.error
import urllib.parse
import http.client

SYSTEM = "你是一个助手，请帮我处理一些个人信息。"

# 轮换的中英双语 PII 样本，覆盖多类型，逐轮追加以模拟 Agent 上下文增长。
PII_SAMPLES = [
    "我的手机号是 13800138000，请记一下。",
    "我的身份证号是 110101199003078515。",
    "我的邮箱是 zhangsan@example.com。",
    "工资卡号 6222021234567890123 用于发放。",
    "我叫李志强，负责对接。",
    "美国社保号 123-45-6789 也填一下。",
    "信用卡 4111111111111111 绑定支付。",
    "车牌 京A12345 是公司车。",
    "My phone is +1-415-555-0132 and SSN 987-65-4320.",
    "Reach me at jane.doe@corp.io, card 5555444433332222.",
]


def percentile(sorted_vals, p):
    if not sorted_vals:
        return 0.0
    if len(sorted_vals) == 1:
        return float(sorted_vals[0])
    k = max(1, int(round(p / 100.0 * len(sorted_vals))))
    k = min(k, len(sorted_vals))
    return float(sorted_vals[k - 1])


def build_messages(turn):
    msgs = [{"role": "system", "content": SYSTEM}]
    for i in range(turn + 1):
        msgs.append({"role": "user", "content": PII_SAMPLES[i % len(PII_SAMPLES)]})
    return msgs


def post_json(url, token, payload, timeout=30):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + token)
    resp = urllib.request.urlopen(req, timeout=timeout)
    return resp.read().decode()


def post_stream(url, token, payload, timeout=60):
    parsed = urllib.parse.urlparse(url)
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    conn = http.client.HTTPConnection(parsed.hostname, port, timeout=timeout)
    data = json.dumps(payload).encode()
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + token}
    t0 = time.perf_counter()
    conn.request("POST", parsed.path, body=data, headers=headers)
    resp = conn.getresponse()
    ttft = None
    while True:
        line = resp.fp.readline()
        if not line:
            break
        line = line.decode(errors="replace").strip()
        if line.startswith("data:"):
            chunk = line[5:].strip()
            if chunk == "[DONE]":
                break
            if ttft is None:
                ttft = time.perf_counter() - t0
    t1 = time.perf_counter()
    conn.close()
    return ttft, (t1 - t0)


def report(name, arr):
    s = sorted(arr)
    return (name, percentile(s, 50), percentile(s, 95), percentile(s, 99),
            (s[-1] if s else 0.0), len(s))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--gw-url", default="http://127.0.0.1:8405")
    ap.add_argument("--mock-url", default="http://127.0.0.1:8999")
    ap.add_argument("--token", default="test-token-12345")
    ap.add_argument("--turns", type=int, default=40)
    ap.add_argument("--stream-turns", type=int, default=20)
    ap.add_argument("--mock-delay-ms", type=int, default=0,
                    help="上报用：mock 上游模拟的生成延迟，仅用于报告标注")
    ap.add_argument("--fail-overhead-ms", type=int, default=0,
                    help=">0 时变为门禁：网关附加延迟 p99 超此值则退出码 1")
    args = ap.parse_args()

    chat = args.gw_url.rstrip("/") + "/v1/chat/completions"
    mock_chat = args.mock_url.rstrip("/") + "/v1/chat/completions"

    gw_lat, base_lat, stream_ttft, stream_total = [], [], [], []

    # warmup：确保网关已就绪
    try:
        post_json(chat, args.token, {"model": "mock-1", "messages": build_messages(0)})
    except Exception as e:  # noqa: BLE001
        sys.stderr.write("WARMUP FAILED (网关未就绪？): %s\n" % e)
        sys.exit(2)

    for t in range(args.turns):
        msgs = build_messages(t)
        payload = {"model": "mock-1", "messages": msgs, "stream": False}
        tg0 = time.perf_counter()
        post_json(chat, args.token, payload)
        tg1 = time.perf_counter()
        gw_lat.append((tg1 - tg0) * 1000.0)

        tb0 = time.perf_counter()
        post_json(mock_chat, args.token, payload)
        tb1 = time.perf_counter()
        base_lat.append((tb1 - tb0) * 1000.0)

    for t in range(args.stream_turns):
        msgs = build_messages(t % max(1, args.turns))
        payload = {"model": "mock-1", "messages": msgs, "stream": True}
        try:
            ttft, total = post_stream(chat, args.token, payload)
            if ttft is not None:
                stream_ttft.append(ttft * 1000.0)
            stream_total.append(total * 1000.0)
        except Exception as e:  # noqa: BLE001
            sys.stderr.write("STREAM turn %d error: %s\n" % (t, e))

    deltas = [g - b for g, b in zip(gw_lat, base_lat)]
    d_sorted = sorted(deltas)

    print("")
    print("=== LLMate-Gate 多轮端到端延迟基准 ===")
    print("mock 上游模拟生成延迟: %d ms" % args.mock_delay_ms)
    print("非流式轮数=%d  流式轮数=%d  (对话上下文逐轮增长)" % (args.turns, args.stream_turns))
    print("")
    print("%-28s %9s %9s %9s %9s %5s" % ("series", "p50", "p95", "p99", "max", "n"))
    for r in (report("gateway 非流式 (ms)", gw_lat),
             report("baseline 直达 (ms)", base_lat),
             report("stream TTFT (ms)", stream_ttft),
             report("stream 总耗时 (ms)", stream_total)):
        print("%-28s %9.2f %9.2f %9.2f %9.2f %5d" % r)
    print("")
    print("网关附加延迟（gateway - baseline，逐轮差值），单位 ms:")
    print("  p50=%.2f  p95=%.2f  p99=%.2f  max=%.2f" % (
        percentile(d_sorted, 50), percentile(d_sorted, 95),
        percentile(d_sorted, 99), (d_sorted[-1] if d_sorted else 0.0)))
    print("")
    print("解读：上述差值即 PII 检测+替换+还原+额外一跳的真实开销；")
    print("regex 引擎在 localhost 下应稳定远低于 1-2 ms。")
    if args.fail_overhead_ms > 0:
        p99 = percentile(d_sorted, 99)
        if p99 > args.fail_overhead_ms:
            print("FAIL: 网关附加延迟 p99 %.2f ms > 阈值 %d ms" % (p99, args.fail_overhead_ms))
            sys.exit(1)
        print("PASS: 网关附加延迟 p99 %.2f ms <= 阈值 %d ms" % (p99, args.fail_overhead_ms))
    sys.exit(0)


if __name__ == "__main__":
    main()
