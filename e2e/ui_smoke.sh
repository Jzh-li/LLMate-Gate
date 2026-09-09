#!/usr/bin/env bash
# e2e/ui_smoke.sh —— 调试面板冒烟测试（UI设计 §5.1 D1-D6）。
#
# 前置：
#   - GO 程序已编译（scripts/dev.sh build）
#   - 占位 :8400
#
# 用法：bash e2e/ui_smoke.sh [addr]，默认 :8400
#
# 跨平台：Linux(GitHub CI) / macOS / Windows(git-bash) 均可运行。

set -uo pipefail

# 环境里的 HTTP(S)_PROXY 会把 127.0.0.1 请求也走代理，导致本地/沙箱出现假 502。
# 所有请求均为 loopback，统一绕过代理。
export NO_PROXY='*'
export no_proxy='*'

ADDR="${1:-:8400}"
PROBE="http://127.0.0.1${ADDR}"
TOKEN="ui-smoke-$$"
export GATEWAY_AUTH_TOKEN="$TOKEN"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;;
  *) EXE="" ;;
esac
BUILD_DIR="${LMGATE_BUILD:-${ROOT}/build}"
BIN="${BUILD_DIR}/llmate-gate${EXE}"
[ -x "$BIN" ] || BIN="${BUILD_DIR}/llmate-gate"
if [ ! -x "$BIN" ]; then
  echo "FAIL: binary not found at $BIN, run scripts/dev.sh build first" >&2
  exit 1
fi

# Windows 二进制无法解析 git-bash 的 POSIX 路径（/c/...），需转成 C:/...
native_path() {
  local p="$1"
  if [ -n "$EXE" ] && command -v cygpath >/dev/null 2>&1; then
    cygpath -m "$p" 2>/dev/null || printf '%s' "$p"
  else
    printf '%s' "$p"
  fi
}

# 用 ui-smoke 专用配置（上游指向本地未监听端口）→ 不出网、结果确定。
CFG=()
if [ -f "${ROOT}/gateway/configs/ui-smoke.yaml" ]; then
  CFG=(--config "$(native_path "${ROOT}/gateway/configs/ui-smoke.yaml")")
fi

LOG=$(mktemp)
PID=""
cleanup() { [ -n "$PID" ] && kill "$PID" 2>/dev/null; rm -f "$LOG"; }
trap cleanup EXIT

# 启动（或重启）网关。
# 关键：必须先确认旧实例已退出、端口已释放，再启动新实例。否则新实例 bind 失败、
# 旧实例仍在服务，后续断言会打在旧进程上造成假失败（CI 上曾因此让 D5 四项全 FAIL）。
start_gateway() {
  local extra="${1:-}"
  local i
  if [ -n "$PID" ]; then
    # 优雅退出：先 SIGTERM，5s 内未退出再 SIGKILL，避免旧进程不释放端口时
    # 无限等待导致 CI 挂死（"The operation was canceled"）。
    kill "$PID" 2>/dev/null || true
    for i in $(seq 1 25); do
      kill -0 "$PID" 2>/dev/null || break
      sleep 0.2
    done
    if kill -0 "$PID" 2>/dev/null; then
      echo "[ui_smoke] old gateway did not exit gracefully, sending SIGKILL"
      kill -9 "$PID" 2>/dev/null || true
      for i in $(seq 1 25); do
        kill -0 "$PID" 2>/dev/null || break
        sleep 0.2
      done
    fi
    PID=""
  fi
  # 等旧实例真正停止（healthz 不再响应）
  for i in $(seq 1 50); do
    curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1 || break
    sleep 0.2
  done
  "$BIN" --listen "$ADDR" ${extra:+"$extra"} ${CFG[@]+"${CFG[@]}"} >"$LOG" 2>&1 &
  PID=$!
  # 等新实例就绪
  for i in $(seq 1 50); do
    curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  # 新实例必须活着；否则说明启动失败，后续断言无意义
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "FAIL: gateway 启动失败（端口未释放或参数错误）"
    cat "$LOG"
    exit 1
  fi
}

echo "[ui_smoke] starting $BIN at $ADDR ..."
start_gateway ""
if ! curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1; then
  echo "FAIL: gateway not ready"
  cat "$LOG"
  exit 1
fi

pass=0; fail=0
check() {
  local name="$1"; local cond="$2"
  if eval "$cond"; then
    echo "PASS $name"; pass=$((pass+1))
  else
    echo "FAIL $name"; fail=$((fail+1))
  fi
}

# D1: /_debug 返回面板 HTML
HTML=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/_debug")
check "D1 /_debug returns HTML" 'echo "$HTML" | grep -q "<html"'

# D2: 发请求后 1s 内 _api/traffic 含检测到的实体
# 注：内置 regex 引擎只识别正则可匹配的实体（手机/邮箱/身份证等），
# 中文姓名依赖 PII Engineer 模型。本测试用手机号（zh_phone）。
# 不使用 -f，因为上游不可达时返回 502，但请求已被网关处理并发布到 traffic。
curl -sS -X POST "$PROBE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"我的手机 13800138000"}]}' \
  >/dev/null 2>&1 || true
sleep 2
TRAFFIC=$(curl -sS -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic" 2>&1 || echo "")
[ -n "${UI_DEBUG:-}" ] && echo "[debug] TRAFFIC content=${TRAFFIC}"
check "D2 _api/traffic has zh_phone" 'echo "$TRAFFIC" | grep -q "zh_phone"'
check "D2 _api/traffic has <<zh_phone_1>>" 'echo "$TRAFFIC" | grep -qE "<<zh_phone_[0-9]+>>"'

# D3: Playground 检测（不出站）
DET=$(curl -fsS -X POST "$PROBE/_api/detect" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000"}')
check "D3 _api/detect returns zh_phone" 'echo "$DET" | grep -q "zh_phone"'

# D4: Playground 替换（占位符格式与契约 §5.2 一致：<<type_index>>）
REPL=$(curl -fsS -X POST "$PROBE/_api/replace" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000","strategy":"placeholder"}')
check "D4 _api/replace has <<zh_phone_1>>" 'echo "$REPL" | grep -qE "<<zh_phone_[0-9]+>>"'
check "D4 _api/replace preserves 13800138000 in mapping" 'echo "$REPL" | grep -q "13800138000"'

# D5: --no-debug 重启后所有 debug 端点 404
echo "[ui_smoke] restarting with --no-debug ..."
start_gateway "--no-debug"
check "D5 /_debug -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_debug")" = "404" ]'
check "D5 /ws/events -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/ws/events")" = "404" ]'
check "D5 /_api/traffic -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic")" = "404" ]'
check "D5 /_api/detect -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_api/detect")" = "404" ]'

# D6: 面板崩溃不影响代理 —— 默认 debug 模式下并发发请求 + 拉取流量。
echo "[ui_smoke] restarting for D6 ..."
start_gateway ""
(prod_curl() {
  for i in $(seq 1 10); do
    curl -s -o /dev/null -X POST "$PROBE/v1/chat/completions" \
      -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"messages":[{"role":"user","content":"李四"}]}' || true
    sleep 0.2
  done
}) &
PROD=$!
for i in $(seq 1 5); do
  curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic" >/dev/null 2>&1 || true
  sleep 0.2
done
wait $PROD
check "D6 gateway still healthy after traffic + panel polls" 'curl -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" | grep -q "ok"'

echo
echo "=== Summary: PASS=$pass FAIL=$fail ==="
[ "$fail" -eq 0 ]
