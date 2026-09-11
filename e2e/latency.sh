#!/usr/bin/env bash
# e2e/latency.sh —— 多轮端到端延迟基准（收口 V1_READINESS R3 口径不符）。
#
# 与 e2e.sh 共享同一套 mock 上游与网关配置，但目的是「量」而非「断言」：
#   - 起 mock-llm（带模拟生成延迟 -delay）与 llmate-gate（regex 引擎）
#   - 跑 latency_client.py：多轮非流式往返 + 直达基线 + 流式 TTFT
#   - 输出 p50/p95/p99，并给出「网关附加延迟」分布
#
# 默认只量不拦（基准用途）；如需门禁，设 FAIL_OVERHEAD_MS（毫秒）。
#
# 前置：llmate-gate[.exe] / mock-llm[.exe] 已构建（默认 $REPO/build，
#       Windows 下可用 LMGATE_BUILD 指向 C:/.../Temp/lmgate），
#       或脚本会尝试用 go build 现场构建。
#       也可复用 e2e.sh 的构建产物（同目录）。

set -uo pipefail

# 所有请求均为 loopback，统一绕过代理，避免本地/沙箱假 502。
export NO_PROXY='*'
export no_proxy='*'

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;;
  *) EXE="" ;;
esac

BUILD_DIR="${LMGATE_BUILD:-${REPO_ROOT}/build}"
LLMATE_BIN="${BUILD_DIR}/llmate-gate${EXE}"
MOCK_LLM_BIN="${BUILD_DIR}/mock-llm${EXE}"

# 网关配置复用 e2e-llm.yaml（其内部 upstream 指向 :8999），通过 --listen 覆盖端口。
if [ -f "${REPO_ROOT}/gateway/configs/e2e-llm.yaml" ]; then
  E2E_LLM_CFG="${REPO_ROOT}/gateway/configs/e2e-llm.yaml"
elif [ -f "${BUILD_DIR}/gateway/configs/e2e-llm.yaml" ]; then
  E2E_LLM_CFG="${BUILD_DIR}/gateway/configs/e2e-llm.yaml"
else
  E2E_LLM_CFG=""
fi

native_path() {
  local p="$1"
  if [ -n "$EXE" ] && command -v cygpath >/dev/null 2>&1; then
    cygpath -m "$p" 2>/dev/null || printf '%s' "$p"
  else
    printf '%s' "$p"
  fi
}
E2E_LLM_CFG="$(native_path "$E2E_LLM_CFG")"

# 端口与令牌（令牌须与 e2e-llm.yaml 的 ${GATEWAY_AUTH_TOKEN} 一致）。
LLM_PORT=8999
GW_PORT=8405
export GATEWAY_AUTH_TOKEN="${GATEWAY_AUTH_TOKEN:-test-token-12345}"
TOKEN="$GATEWAY_AUTH_TOKEN"
GW="http://127.0.0.1:${GW_PORT}"

# mock 上游模拟生成延迟（首字延迟）；默认 30ms，复现接近真实的「思考+生成」耗时。
MOCK_DELAY_MS="${LAT_MOCK_DELAY_MS:-30}"

# 轮数（可用环境变量覆盖）。
TURNS="${LAT_TURNS:-40}"
STREAM_TURNS="${LAT_STREAM_TURNS:-20}"
FAIL_OVERHEAD_MS="${FAIL_OVERHEAD_MS:-0}"

PYTHON="${PYTHON:-$(command -v python3 2>/dev/null || command -v python 2>/dev/null || echo python)}"

wait_listen() {
  local port="$1" timeout="${2:-30}" need_token="${3:-true}"
  for i in $(seq 1 $((timeout * 5))); do
    if [ "$need_token" = "true" ]; then
      if curl --max-time 30 -fsS -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
        return 0
      fi
    else
      if curl --max-time 30 -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 0.2
  done
  return 1
}

cleanup_all() {
  set +e
  if [ -n "$EXE" ]; then
    PIDS=$(tasklist 2>&1 | grep -iE 'llmate-gate|mock-llm' | awk '{print $2}')
    for PID in $PIDS; do taskkill /PID "$PID" /F >/dev/null 2>&1; done
  else
    pkill -f 'mock-llm'     >/dev/null 2>&1
    pkill -f 'llmate-gate'  >/dev/null 2>&1
  fi
  return 0
}
trap cleanup_all EXIT

echo "[latency] cleaning up stale processes ..."
cleanup_all
sleep 1

# 构建（若缺失则尝试现场构建；Windows 9P 下可能失败，给出清晰报错）。
if [ ! -x "$LLMATE_BIN" ] || [ ! -x "$MOCK_LLM_BIN" ]; then
  echo "[latency] 二进制缺失，尝试现场构建 ..."
  ( cd "${REPO_ROOT}/gateway" && CGO_ENABLED=0 go build -o "${LLMATE_BIN}" ./cmd/llmate-gate && CGO_ENABLED=0 go build -o "${MOCK_LLM_BIN}" ./cmd/mock-llm ) \
    || { echo "FAIL: 构建失败，请先运行 scripts/dev.sh build" >&2; exit 1; }
fi
if [ ! -f "$E2E_LLM_CFG" ]; then
  echo "FAIL: 找不到 e2e-llm.yaml 配置（dev.sh sync 是否已执行？）" >&2
  exit 1
fi

echo
echo "=== LLMate-Gate 多轮端到端延迟基准 ==="
echo "[latency] mock-llm :$LLM_PORT -delay ${MOCK_DELAY_MS}ms"
"$MOCK_LLM_BIN" --listen ":$LLM_PORT" --record -delay "${MOCK_DELAY_MS}ms" > /tmp/lat-mock.log 2>&1 &
LLM_PID=$!
echo "[latency] llmate-gate :$GW_PORT (e2e-llm.yaml)"
"$LLMATE_BIN" --listen ":$GW_PORT" --config "$E2E_LLM_CFG" > /tmp/lat-gw.log 2>&1 &
GW_PID=$!

if ! wait_listen "$LLM_PORT" 5 false; then
  echo "FAIL: mock-llm 未就绪"; cat /tmp/lat-mock.log; exit 1
fi
if ! wait_listen "$GW_PORT" 30; then
  echo "FAIL: gateway 未就绪"; cat /tmp/lat-gw.log; exit 1
fi
sleep 0.3

echo "[latency] 运行 latency_client.py ..."
"$PYTHON" "${REPO_ROOT}/e2e/latency_client.py" \
  --gw-url "$GW" \
  --mock-url "http://127.0.0.1:${LLM_PORT}" \
  --token "$TOKEN" \
  --turns "$TURNS" \
  --stream-turns "$STREAM_TURNS" \
  --mock-delay-ms "$MOCK_DELAY_MS" \
  --fail-overhead-ms "$FAIL_OVERHEAD_MS"
RC=$?

echo
echo "[latency] mock-llm 日志尾（如异常排查用）:"
tail -n 5 /tmp/lat-mock.log 2>/dev/null
echo "[latency] gateway 日志尾:"
tail -n 5 /tmp/lat-gw.log 2>/dev/null

exit $RC
