#!/usr/bin/env bash
# e2e/ui_smoke.sh —— 调试面板冒烟测试（UI设计 §5.1 D1-D6）。
#
# 前置：
#   - GO 程序已编译（scripts/dev.sh build）
#   - 占位 :8400
#
# 用法：bash e2e/ui_smoke.sh [addr]，默认 :8400

set -uo pipefail

ADDR="${1:-:8400}"
PROBE="http://127.0.0.1${ADDR}"
TOKEN="ui-smoke-$$"
export GATEWAY_AUTH_TOKEN="$TOKEN"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="${LMGATE_BUILD:-/c/Users/jzh-l/AppData/Local/Temp/lmgate}"
BIN="${BUILD_DIR}/llmate-gate.exe"
[ -x "$BIN" ] || BIN="${BUILD_DIR}/llmate-gate"
if [ ! -x "$BIN" ]; then
  echo "FAIL: binary not found at $BIN, run scripts/dev.sh build first" >&2
  exit 1
fi

LOG=$(mktemp)
cleanup() { kill "$PID" 2>/dev/null || true; rm -f "$LOG"; }
trap cleanup EXIT

echo "[ui_smoke] starting $BIN at $ADDR ..."
"$BIN" --listen "$ADDR" >"$LOG" 2>&1 &
PID=$!

# 等待 healthz（最多 10s）
for i in $(seq 1 20); do
  if curl -fsS "$PROBE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
if ! curl -fsS "$PROBE/healthz" >/dev/null 2>&1; then
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
HTML=$(curl -fsS "$PROBE/_debug")
check "D1 /_debug returns HTML" 'echo "$HTML" | grep -q "<html"'

# D2: 发请求后 1s 内 _api/traffic 含检测到的实体
# 注：内置 regex 引擎只识别正则可匹配的实体（手机/邮箱/身份证等），
# 中文姓名依赖 PII Engineer 模型。本测试用手机号（zh_phone）。
# 不使用 -f，因为上游不通时返回 502，但请求已被网关处理并发布到 traffic。
curl -sS -X POST "$PROBE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"我的手机 13800138000"}]}' \
  >/dev/null 2>&1 || true
sleep 2
TRAFFIC=$(curl -sS "$PROBE/_api/traffic" 2>&1 || echo "")
echo "[debug] TRAFFIC content=${TRAFFIC}"
check "D2 _api/traffic has zh_phone" 'echo "$TRAFFIC" | grep -q "zh_phone"'
check "D2 _api/traffic has <<zh_phone_1>>" 'echo "$TRAFFIC" | grep -qE "<<zh_phone_[0-9]+>>"'

# D3: Playground 检测（不出站；本机断网也通过）
DET=$(curl -fsS -X POST "$PROBE/_api/detect" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000"}')
check "D3 _api/detect returns zh_phone" 'echo "$DET" | grep -q "zh_phone"'

# D4: Playground 替换（占位符格式与契约 §5.2 一致：<<type_index>>）
REPL=$(curl -fsS -X POST "$PROBE/_api/replace" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000","strategy":"placeholder"}')
check "D4 _api/replace has <<zh_phone_1>>" 'echo "$REPL" | grep -qE "<<zh_phone_[0-9]+>>"'
check "D4 _api/replace preserves 13800138000 in mapping" 'echo "$REPL" | grep -q "13800138000"'

# D5: --no-debug 重启后所有 debug 端点 404
echo "[ui_smoke] restarting with --no-debug ..."
kill "$PID" 2>/dev/null || true
sleep 0.5
"$BIN" --listen "$ADDR" --no-debug >"$LOG" 2>&1 &
PID=$!
for i in $(seq 1 20); do
  if curl -fsS "$PROBE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
check "D5 /_debug -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" "$PROBE/_debug")" = "404" ]'
check "D5 /ws/events -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" "$PROBE/ws/events")" = "404" ]'
check "D5 /_api/traffic -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" "$PROBE/_api/traffic")" = "404" ]'
check "D5 /_api/detect -> 404" '[ "$(curl -s -o /dev/null -w "%{http_code}" "$PROBE/_api/detect")" = "404" ]'

# D6: 面板崩溃不影响代理 —— 默认 debug 模式下并发发请求 + 拉取流量。
echo "[ui_smoke] restarting for D6 ..."
kill "$PID" 2>/dev/null || true
sleep 0.5
"$BIN" --listen "$ADDR" >"$LOG" 2>&1 &
PID=$!
for i in $(seq 1 20); do
  if curl -fsS "$PROBE/healthz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
# 并发：每 200ms 发一个请求 + 持续 GET /_api/traffic / WebSocket
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
  curl -fsS "$PROBE/_api/traffic" >/dev/null 2>&1 || true
  sleep 0.2
done
wait $PROD
check "D6 gateway still healthy after traffic + panel polls" 'curl -fsS "$PROBE/healthz" | grep -q "ok"'

echo
echo "=== Summary: PASS=$pass FAIL=$fail ==="
[ "$fail" -eq 0 ]