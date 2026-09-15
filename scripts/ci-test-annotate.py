#!/usr/bin/env python3
"""把 go test 输出翻译成 GitHub 注解，让 CI 红灯自述失败原因。

背景：Actions 的 job 日志只对仓库有 admin 权限的人可见（REST 取日志返回
403 "Must have admin rights to Repository"），没有日志权限的人（外部协作者、
以及按 API 排查问题的自动化）只能看到一句
"Process completed with exit code 1." —— 等于一个说不出所以然的红灯。

注解（workflow command `::error::`）不依赖日志权限，会直接出现在
Checks 面板与 REST 的 check-runs/annotations 里，因此把失败用例名、
竞态报告摘要写回注解，能让红灯自己把原因讲清楚。

用法：ci-test-annotate.py <go-test-log>
退出码恒为 0 —— 本步骤只负责报告，判定失败由 go test 那一步负责。
"""

import re
import sys

MAX_ANNOTATIONS = 10
MAX_TAIL_LINES = 12


def emit(level: str, msg: str, title: str | None = None) -> None:
    """输出一条 GitHub 注解；换行必须转义成 %0A，否则会被截断成一行。"""
    msg = msg.replace("%", "%25").replace("\r", "").replace("\n", "%0A")
    head = f"::{level}"
    if title:
        head += f" title={title}"
    print(f"{head}::{msg}")


def main() -> int:
    if len(sys.argv) < 2:
        print("用法: ci-test-annotate.py <go-test-log>")
        return 0

    try:
        with open(sys.argv[1], encoding="utf-8", errors="replace") as fh:
            lines = fh.read().splitlines()
    except OSError as exc:
        # 失败可能发生在测试步骤之前（如 go mod verify / go vet），此时没有日志文件。
        emit("notice", f"未找到 go test 日志（{exc}）——失败点可能在测试步骤之前")
        return 0

    # 1) 竞态报告优先。它是唯一无法在本地复现的一类失败
    #    （容器里 ThreadSanitizer 起不来：unsupported VMA range），
    #    所以必须显式点名，否则容易被当成普通断言失败去找。
    if any("DATA RACE" in line for line in lines):
        emit("error", "go test -race 检测到 DATA RACE；堆栈见日志尾部摘要", title="race")

    # 2) 包级 FAIL 与用例级 --- FAIL 都要，前者定位包，后者定位用例。
    fails = [
        line.strip()
        for line in lines
        if line.startswith("--- FAIL") or re.match(r"^FAIL\s+\S+", line)
    ]
    for item in fails[:MAX_ANNOTATIONS]:
        emit("error", item, title="go test")
    if len(fails) > MAX_ANNOTATIONS:
        emit("error", f"另有 {len(fails) - MAX_ANNOTATIONS} 条失败未逐条列出")

    # 3) 附日志尾部：竞态报告、断言详情都在最后一段里。
    tail = [line for line in lines if line.strip()][-MAX_TAIL_LINES:]
    if tail:
        emit("notice", "日志尾部：\n" + "\n".join(tail))

    if not fails:
        emit("notice", "未从日志里解析到 FAIL 行，请查看原始日志")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
