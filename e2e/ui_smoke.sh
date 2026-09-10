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
WATCHDOG_PID=""

# 带时间戳的日志：CI 上卡住时能定位到具体阶段耗时。
log() { echo "[ui_smoke $(date +%H:%M:%S)] $*"; }

# 所有 HTTP 请求统一走 c()，强制超时。
# 背景：CI 上曾出现 job 撞 15 分钟超时被 cancel（"The operation was canceled"），
# 原因是脚本里的 curl 全部没有超时，一旦对端"连上但不返回"就无限挂起。
# 加了超时后，任何挂起都会在有限时间内转成失败，且能看到完整现场。
CURL_MAX_TIME="${UI_SMOKE_CURL_MAX_TIME:-10}"
c() { curl --connect-timeout 3 --max-time "$CURL_MAX_TIME" "$@"; }

# 进程是否"真的活着"。
# 关键：Linux 上 kill -0 对**僵尸进程**同样返回成功。网关启动失败（如端口被占用
# 导致 bind 失败）后进程立即退出，若 bash 尚未 reap，PID 仍存在，kill -0 就会把
# 死进程误判为存活 —— 进而误判"新实例已就绪"，后续断言全部打空。
proc_alive() {
  local pid="${1:-}"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  # Linux：僵尸（Z）视为已死。/proc 不存在（macOS/Windows）时降级为 kill -0。
  if [ -r "/proc/$pid/stat" ]; then
    local st
    st=$(sed -n 's/.*) \([A-Za-z]\).*/\1/p' "/proc/$pid/stat" 2>/dev/null)
    [ "$st" = "Z" ] && return 1
  fi
  return 0
}

# 现场快照：异常退出/超时前把能拿到的信息全打出来，避免 CI 被 cancel 后无据可查。
dump_diag() {
  echo "=== [ui_smoke] diagnostic dump ==="
  echo "--- gateway pid=${PID:-none} alive=$(proc_alive "$PID" && echo yes || echo no) ---"
  echo "--- gateway log ($LOG) ---"
  tail -60 "$LOG" 2>/dev/null || echo "(no log)"
  echo "--- listeners on $ADDR ---"
  (ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep -F "${ADDR#*:}" || echo "(no ss/netstat or no match)"
  echo "--- related processes ---"
  ps -eo pid,stat,etime,cmd 2>/dev/null | grep -E 'llmate-gate' | grep -v grep || echo "(none)"
  echo "=== end diagnostic dump ==="
}

cleanup() {
  [ -n "$WATCHDOG_PID" ] && kill "$WATCHDOG_PID" 2>/dev/null
  if [ -n "$PID" ] && proc_alive "$PID"; then
    kill "$PID" 2>/dev/null
  fi
  rm -f "$LOG"
}
trap cleanup EXIT

# 自保护 watchdog：默认 300s 未跑完就打印现场并自杀。
# 目的：CI job 被 cancel 时日志会丢一半，且看不到任何失败原因；
# 自己超时退出则能留下完整现场，并把 job 的失败原因变成明确的非零退出。
start_watchdog() {
  local limit="${UI_SMOKE_TIMEOUT:-300}"
  (
    sleep "$limit"
    echo "[ui_smoke] TIMEOUT: exceeded ${limit}s, aborting with diagnostics"
    dump_diag
    kill -9 "$$" 2>/dev/null
  ) &
  WATCHDOG_PID=$!
}

# 启动（或重启）网关。
# 关键：必须先确认旧实例已退出、端口已释放，再启动新实例。否则新实例 bind 失败、
# 旧实例仍在服务，后续断言会打在旧进程上造成假失败（CI 上曾因此让 D5 四项全 FAIL）。
start_gateway() {
  local extra="${1:-}"
  local label="${2:-}"
  local i
  if [ -n "$PID" ]; then
    log "stopping old gateway (pid=$PID) ..."
    kill "$PID" 2>/dev/null || true
    for i in $(seq 1 50); do
      proc_alive "$PID" || break
      sleep 0.2
    done
    if proc_alive "$PID"; then
      echo "[ui_smoke] old gateway did not exit gracefully, sending SIGKILL"
      kill -9 "$PID" 2>/dev/null || true
      for i in $(seq 1 50); do
        proc_alive "$PID" || break
        sleep 0.2
      done
    fi
    if proc_alive "$PID"; then
      echo "[ui_smoke] WARN: old gateway still alive after SIGKILL (pid=$PID)"
    fi
    PID=""
  fi
  # 等旧实例真正停止（healthz 不再响应；curl 带超时，连不上即视为已停）
  log "waiting for port release ..."
  for i in $(seq 1 50); do
    c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1 || break
    sleep 0.2
  done
  log "launching gateway ${label} ..."
  "$BIN" --listen "$ADDR" ${extra:+"$extra"} ${CFG[@]+"${CFG[@]}"} >"$LOG" 2>&1 &
  PID=$!
  # 等新实例就绪
  for i in $(seq 1 50); do
    c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1 && break
    sleep 0.2
  done
  # 新实例必须活着；否则说明启动失败，后续断言无意义
  if ! proc_alive "$PID"; then
    echo "FAIL: gateway 启动失败（端口未释放或参数错误）"
    dump_diag
    exit 1
  fi
  log "gateway ready (pid=$PID)"
}

start_watchdog

log "starting $BIN at $ADDR ..."
start_gateway "" "(debug)"
if ! c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" >/dev/null 2>&1; then
  echo "FAIL: gateway not ready"
  dump_diag
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
HTML=$(c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/_debug")
check "D1 /_debug returns HTML" 'echo "$HTML" | grep -q "<html"'

# D2: 发请求后 1s 内 _api/traffic 含检测到的实体
# 注：内置 regex 引擎只识别正则可匹配的实体（手机/邮箱/身份证等），
# 中文姓名依赖 PII Engineer 模型。本测试用手机号（zh_phone）。
# 不使用 -f，因为上游不可达时返回 502，但请求已被网关处理并发布到 traffic。
c -sS -X POST "$PROBE/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"我的手机 13800138000"}]}' \
  >/dev/null 2>&1 || true
sleep 2
TRAFFIC=$(c -sS -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic" 2>&1 || echo "")
[ -n "${UI_DEBUG:-}" ] && echo "[debug] TRAFFIC content=${TRAFFIC}"
check "D2 _api/traffic has zh_phone" 'echo "$TRAFFIC" | grep -q "zh_phone"'
check "D2 _api/traffic has <<zh_phone_1>>" 'echo "$TRAFFIC" | grep -qE "<<zh_phone_[0-9]+>>"'

# D3: Playground 检测（不出站）
DET=$(c -fsS -X POST "$PROBE/_api/detect" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000"}')
check "D3 _api/detect returns zh_phone" 'echo "$DET" | grep -q "zh_phone"'

# D4: Playground 替换（占位符格式与契约 §5.2 一致：<<type_index>>）
REPL=$(c -fsS -X POST "$PROBE/_api/replace" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"text":"我的手机是 13800138000","strategy":"placeholder"}')
check "D4 _api/replace has <<zh_phone_1>>" 'echo "$REPL" | grep -qE "<<zh_phone_[0-9]+>>"'
check "D4 _api/replace preserves 13800138000 in mapping" 'echo "$REPL" | grep -q "13800138000"'

# D5: --no-debug 重启后所有 debug 端点 404
log "restarting with --no-debug ..."
start_gateway "--no-debug" "(--no-debug)"
log "probing D5 endpoints ..."
check "D5 /_debug -> 404" '[ "$(c -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_debug")" = "404" ]'
check "D5 /ws/events -> 404" '[ "$(c -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/ws/events")" = "404" ]'
check "D5 /_api/traffic -> 404" '[ "$(c -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic")" = "404" ]'
check "D5 /_api/detect -> 404" '[ "$(c -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $TOKEN" "$PROBE/_api/detect")" = "404" ]'

# D6: 面板崩溃不影响代理 —— 默认 debug 模式下并发发请求 + 拉取流量。
log "restarting for D6 ..."
start_gateway "" "(debug)"
(prod_curl() {
  for i in $(seq 1 10); do
    c -s -o /dev/null -X POST "$PROBE/v1/chat/completions" \
      -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"messages":[{"role":"user","content":"李四"}]}' || true
    sleep 0.2
  done
}) &
PROD=$!
for i in $(seq 1 5); do
  c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/_api/traffic" >/dev/null 2>&1 || true
  sleep 0.2
done
wait $PROD
check "D6 gateway still healthy after traffic + panel polls" 'c -fsS -H "Authorization: Bearer $TOKEN" "$PROBE/healthz" | grep -q "ok"'

echo
echo "=== Summary: PASS=$pass FAIL=$fail ==="
[ "$fail" -eq 0 ]
