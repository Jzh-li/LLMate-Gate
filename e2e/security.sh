#!/usr/bin/env bash
# e2e/security.sh —— 自身暴露面的端到端行为验收（Specs/07）。
#
# 与 e2e.sh 的分工：e2e.sh 验的是「功能链路还通不通」，本脚本验的是「边界还锁不锁得住」。
# 两者的断言方式不同——单测打的是替身，这里全部打真实进程上的真实 HTTP，因为被加固的
# 正是中间件分发与令牌解析这些「只有组装起来才成立」的东西。
#
# 覆盖：
#   S1 探针匿名 / 数据面 / 控制面：三面各自的凭据要求
#   S2 令牌来源：自动生成、落盘 0600、重启复用、只打印一次
#   S3 显式免鉴权：放行且必须打 SECURITY WARNING
#   S4 vault.persist：配置即启动失败
#   S5 还原来源隔离：控制面 API 还原不出数据面的 request_id
#   S6 调试面板：数据端点归控制面，静态壳匿名，令牌可换 Cookie
#
# 前置：llmate-gate 已构建（默认 $REPO_ROOT/build，可用 LMGATE_BUILD 覆盖）。
# 所有断言通过 → 0 退出；任一失败 → 1 退出。

set -uo pipefail

# loopback 请求不要走环境里的 HTTP(S)_PROXY（否则出现假 502）
export NO_PROXY='*'
export no_proxy='*'

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*) EXE=".exe" ;;
  *) EXE="" ;;
esac

BUILD_DIR="${LMGATE_BUILD:-${REPO_ROOT}/build}"
GW_BIN="${BUILD_DIR}/llmate-gate${EXE}"

PORT="${SEC_PORT:-8403}"
BASE="http://127.0.0.1:${PORT}"
CTRL="ctrl-token-e2e-0123456789"

WORK="$(mktemp -d 2>/dev/null || echo "${TMPDIR:-/tmp}/llmate-sec-$$")"
mkdir -p "$WORK"
cleanup() { [ -n "${GW:-}" ] && kill "$GW" 2>/dev/null; wait "$GW" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

if [ ! -x "$GW_BIN" ]; then
  echo "缺少二进制：$GW_BIN"; echo "先执行：go build -o build/llmate-gate ./cmd/llmate-gate"; exit 1
fi

PASS=0; FAIL=0
ck() { # ck <描述> <期望> <实际>
  if [ "$2" = "$3" ]; then echo "  PASS  $1 ($3)"; PASS=$((PASS+1));
  else echo "  FAIL  $1 (期望 $2，实际 $3)"; FAIL=$((FAIL+1)); fi
}
ok() { # ok <描述> <0=通过>
  if [ "$2" = "0" ]; then echo "  PASS  $1"; PASS=$((PASS+1));
  else echo "  FAIL  $1"; FAIL=$((FAIL+1)); fi
}
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 4 "$@"; }
# not401 <curl args...>：断言请求通过了鉴权层（结果是不是 401 不重要）
not401() { [ "$(code "$@")" = 401 ] && echo 401 || echo not401; }

start_gw() { # start_gw <config>
  : > "$WORK/out.log"
  "$GW_BIN" --config "$1" >> "$WORK/out.log" 2>&1 &
  GW=$!
  for _ in $(seq 1 40); do
    curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && return 0
    sleep 0.25
  done
  return 1
}
stop_gw() { kill "$GW" 2>/dev/null; wait "$GW" 2>/dev/null; GW=""; }

write_cfg() { # write_cfg <path> <extra lines>
  cat > "$1" <<EOF
gateway:
  listen: ":$PORT"
  upstream: "http://127.0.0.1:18999"
  auth_token_file: "$WORK/auth_token"
  control_auth_token: "$CTRL"
  request_timeout: "2s"
  log_level: "info"
$2
detection:
  engine: "regex"
policy:
  fail_closed: true
vault:
  request_ttl: "30m"
audit:
  enabled: true
  path: "$WORK/audit.log"
  max_size_mb: 100
  max_backups: 3
EOF
}

# ============ S1 / S2：三面凭据 + 令牌生命周期 ============
echo "=== S1/S2 鉴权三面与令牌生命周期 ==="
write_cfg "$WORK/config.yaml" '  debug: false'
start_gw "$WORK/config.yaml" || { echo "网关未就绪"; cat "$WORK/out.log"; exit 1; }

ck "healthz 匿名 200"        200 "$(code "$BASE/healthz")"
ck "数据面无凭据 401"          401 "$(code "$BASE/v1/models")"
ck "metrics 无凭据 401"        401 "$(code "$BASE/metrics")"
ck "控制面无凭据 401"          401 "$(code -X POST -H 'Content-Type: application/json' -d '{}' "$BASE/v1/privacy/restore")"

if [ -f "$WORK/auth_token" ]; then ok "未配 auth_token 时自动生成并落盘" 0; else ok "未配 auth_token 时自动生成并落盘" 1; fi
ck "令牌文件权限 0600" 600 "$(stat -c '%a' "$WORK/auth_token" 2>/dev/null || stat -f '%Lp' "$WORK/auth_token" 2>/dev/null)"
TOK1="$(cat "$WORK/auth_token" 2>/dev/null)"
ck "令牌长度 64（32 字节随机）" 64 "${#TOK1}"

ck "数据面带令牌放行"          not401 "$(not401 -H "Authorization: Bearer $TOK1" "$BASE/v1/models")"
ck "控制面拒绝数据面令牌"      401    "$(code -X POST -H "Authorization: Bearer $TOK1" -H 'Content-Type: application/json' -d '{}' "$BASE/v1/privacy/restore")"
ck "数据面拒绝控制面令牌"      401    "$(code -H "Authorization: Bearer $CTRL" "$BASE/v1/models")"
ck "控制面接受控制面令牌"      not401 "$(not401 -X POST -H "Authorization: Bearer $CTRL" -H 'Content-Type: application/json' -d '{}' "$BASE/v1/privacy/restore")"

# --- S5：还原来源隔离（数据面 vs 控制面）---
# 数据面：带一个已知的 X-Request-ID 发一次 LLM 请求。上游不可达（502）不影响结论——
# 映射表在转发之前就已经写入，这正是被审计的那一步。
echo "--- S5 还原来源隔离 ---"
DATAPLANE_ID="victim-request-id-0001"
curl -sS -o /dev/null --max-time 4 -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $TOK1" -H "Content-Type: application/json" \
  -H "X-Request-ID: $DATAPLANE_ID" \
  -d '{"messages":[{"role":"user","content":"我的手机 13800138000"}]}' 2>/dev/null || true

ATTACK_BODY="$(curl -sS --max-time 4 -X POST "$BASE/v1/privacy/restore" \
  -H "Authorization: Bearer $CTRL" -H "Content-Type: application/json" \
  -d "{\"request_id\":\"$DATAPLANE_ID\",\"text\":\"<<zh_phone_1>>\"}" 2>/dev/null)"
ATTACK_CODE="$(code -X POST "$BASE/v1/privacy/restore" -H "Authorization: Bearer $CTRL" \
  -H "Content-Type: application/json" -d "{\"request_id\":\"$DATAPLANE_ID\",\"text\":\"<<zh_phone_1>>\"}")"
ck "按数据面 request_id 还原被拒" 404 "$ATTACK_CODE"
if printf '%s' "$ATTACK_BODY" | grep -q '13800138000'; then
  ok "数据面原文未泄漏" 1
else
  ok "数据面原文未泄漏" 0
fi

# 控制面自己的表仍要能还原——否则就是「安全了但功能没了」
REDACT_BODY="$(curl -sS --max-time 4 -X POST "$BASE/v1/privacy/redact" \
  -H "Authorization: Bearer $CTRL" -H "Content-Type: application/json" \
  -d '{"text":"我的手机 13800138000"}' 2>/dev/null)"
CTRL_ID="$(printf '%s' "$REDACT_BODY" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')"
if [ -n "$CTRL_ID" ]; then ok "控制面 redact 返回 request_id" 0; else ok "控制面 redact 返回 request_id" 1; fi
REDACTED="$(printf '%s' "$REDACT_BODY" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p')"
RESTORE_BODY="$(curl -sS --max-time 4 -X POST "$BASE/v1/privacy/restore" \
  -H "Authorization: Bearer $CTRL" -H "Content-Type: application/json" \
  -d "{\"request_id\":\"$CTRL_ID\",\"text\":\"$REDACTED\"}" 2>/dev/null)"
if printf '%s' "$RESTORE_BODY" | grep -q '13800138000'; then
  ok "控制面自建表可正常还原（功能未被误伤）" 0
else
  ok "控制面自建表可正常还原（功能未被误伤）" 1
fi

echo "--- 启动日志的安全告警 ---"
grep -E "NEW ACCESS TOKEN|access token|SECURITY WARNING|control plane" "$WORK/out.log" | sed 's/^/    /'

stop_gw

# --- S2b：重启复用令牌，且不再打印令牌值 ---
echo "--- S2b 重启 ---"
start_gw "$WORK/config.yaml" || { echo "网关未就绪"; cat "$WORK/out.log"; exit 1; }
TOK2="$(cat "$WORK/auth_token" 2>/dev/null)"
ck "重启后令牌不变" "$TOK1" "$TOK2"
if grep -q "NEW ACCESS TOKEN" "$WORK/out.log"; then
  ok "重启后不再打印令牌值" 1
else
  ok "重启后不再打印令牌值" 0
fi
stop_gw

# ============ S3：显式免鉴权 ============
echo "=== S3 显式免鉴权 ==="
write_cfg "$WORK/config-noauth.yaml" '  allow_unauthenticated: true
  debug: true'
start_gw "$WORK/config-noauth.yaml" || { echo "网关未就绪"; cat "$WORK/out.log"; exit 1; }
ck "免鉴权时数据面放行" not401 "$(not401 "$BASE/v1/models")"
if grep -q "SECURITY WARNING" "$WORK/out.log"; then
  ok "免鉴权必须打印 SECURITY WARNING" 0
  grep "SECURITY WARNING" "$WORK/out.log" | head -2 | sed 's/^/    /'
else
  ok "免鉴权必须打印 SECURITY WARNING" 1
fi
stop_gw

# ============ S6：调试面板 ============
echo "=== S6 调试面板归控制面 ==="
write_cfg "$WORK/config-debug.yaml" '  debug: true'
start_gw "$WORK/config-debug.yaml" || { echo "网关未就绪"; cat "$WORK/out.log"; exit 1; }

ck "面板静态壳匿名可达"        200    "$(code "$BASE/_debug")"
ck "面板静态资源匿名可达"      200    "$(code "$BASE/_debug/app.js")"
ck "面板数据端点无凭据 401"    401    "$(code "$BASE/_api/traffic")"
ck "面板数据端点拒绝数据面令牌" 401    "$(code -H "Authorization: Bearer $(cat "$WORK/auth_token")" "$BASE/_api/traffic")"
ck "面板数据端点接受控制面令牌" not401 "$(not401 -H "Authorization: Bearer $CTRL" "$BASE/_api/traffic")"

# 浏览器路径：令牌换 Cookie。校验 Set-Cookie 与 Location，再带着 Cookie 打数据端点。
BOOT_HDR="$(curl -sS -D - -o /dev/null --max-time 4 "$BASE/_debug?token=$CTRL" 2>/dev/null)"
BOOT_CODE="$(code "$BASE/_debug?token=$CTRL")"
ck "令牌换 Cookie 返回 302" 302 "$BOOT_CODE"
if printf '%s' "$BOOT_HDR" | grep -qi 'HttpOnly'; then ok "Cookie 为 HttpOnly" 0; else ok "Cookie 为 HttpOnly" 1; fi
if printf '%s' "$BOOT_HDR" | grep -qi 'SameSite=Strict'; then ok "Cookie 为 SameSite=Strict" 0; else ok "Cookie 为 SameSite=Strict" 1; fi
if printf '%s' "$BOOT_HDR" | grep -qi '^location: /_debug'; then ok "重定向剥离查询串（令牌不留地址栏）" 0; else ok "重定向剥离查询串（令牌不留地址栏）" 1; fi
ck "错误令牌不换取 Cookie" 401 "$(code "$BASE/_debug?token=wrong-token")"

COOKIE="lmgate_panel=$CTRL"
ck "Cookie 可用于面板数据端点" not401 "$(not401 -H "Cookie: $COOKIE" "$BASE/_api/traffic")"
ck "面板 Cookie 不适用于控制面 API" 401 "$(code -X POST -H "Cookie: $COOKIE" -H 'Content-Type: application/json' -d '{}' "$BASE/v1/privacy/restore")"

stop_gw

echo "=== S4 废弃开关 vault.persist ==="
# 单独一份最小配置：persist 的校验发生在 vault 段，混在一起会把「哪一项导致失败」变模糊。
cat > "$WORK/config-persist.yaml" <<EOF
gateway:
  listen: ":$PORT"
  upstream: "http://127.0.0.1:18999"
  allow_unauthenticated: true
vault:
  persist: true
EOF
if "$GW_BIN" --config "$WORK/config-persist.yaml" > "$WORK/persist.log" 2>&1; then
  ok "persist=true 时启动失败" 1
else
  ok "persist=true 时启动失败" 0
  grep -o "vault.persist is not supported[^\"]*" "$WORK/persist.log" | head -1 | sed 's/^/    /'
fi

echo
echo "================ 结果：PASS=$PASS FAIL=$FAIL ================"
[ "$FAIL" -eq 0 ] || exit 1
