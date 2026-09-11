#!/usr/bin/env bash
# scripts/bench-guard.sh —— Go benchmark 性能回退守门
#
# 用法: bench-guard.sh <baseline.txt> <current.txt> [tolerance]
#   tolerance: 允许的劣化比例，默认 1.25（+25%，容纳共享 runner 噪声）
#
# 逻辑: 两个文件都是 `go test -bench` 原始输出；同名基准取最小 ns/op
#       （min 对 CPU 基准最稳，过滤调度抖动）；current_min > baseline_min * tolerance 即失败。
# 退出码: 0 = 通过/无基线可比; 1 = 检出回退; 2 = 用法错误
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <baseline.txt> <current.txt> [tolerance]" >&2
  exit 2
fi
BASELINE="$1"
CURRENT="$2"
TOL="${3:-1.25}"

if [[ ! -f "$BASELINE" ]]; then
  echo "[bench-guard] baseline 不存在：$BASELINE（首次运行应先生成）" >&2
  exit 2
fi

# 解析 go test -bench 输出 → "name min_ns" 两列；同名多 count 取最小。
parse_min() {
  awk '
    /^Benchmark[A-Za-z0-9_]+-[0-9]+\s/ {
      name = $1; sub(/-[0-9]+$/, "", name)
      for (i = 2; i <= NF; i++) {
        if ($(i+1) == "ns/op") { v = $i + 0; if (!(name in min) || v < min[name]) min[name] = v }
      }
    }
    END { for (n in min) printf "%s %.1f\n", n, min[n] }
  ' "$1" | sort
}

baseline_min=$(parse_min "$BASELINE")
current_min=$(parse_min "$CURRENT")

fail=0
while read -r name bns; do
  cns=$(awk -v n="$name" '$1 == n {print $2}' <<<"$current_min")
  if [[ -z "$cns" ]]; then
    echo "::warning::基准 $name 在当前运行中缺失（可能被删除或改名）"
    continue
  fi
  verdict=$(awk -v b="$bns" -v c="$cns" -v t="$TOL" 'BEGIN { printf "%s", (c > b*t) ? "REGRESSION" : "ok" }')
  printf '%-42s baseline=%12.1fns current=%12.1fns ratio=%.3f %s\n' \
    "$name" "$bns" "$cns" "$(awk -v c="$cns" -v b="$bns" 'BEGIN {print c/b}')" "$verdict"
  if [[ "$verdict" == "REGRESSION" ]]; then
    echo "::error::基准 $name 回退超阈值（current/baseline > ${TOL}）"
    fail=1
  fi
done <<<"$baseline_min"

if [[ $fail -eq 1 ]]; then
  echo "[bench-guard] 检出性能回退" >&2
  exit 1
fi
echo "[bench-guard] 通过（无回退超阈值）"
