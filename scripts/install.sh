#!/usr/bin/env bash
# scripts/install.sh —— LLMate Gate Linux/macOS 一键安装（最小分发）
#
# 行为：
#   1. 拷 llmate-gate 到 ~/.local/bin/
#   2. 生成最小可用配置到 ~/.local/share/llmate-gate/config.yaml
#   3. 创建 systemd --user unit（Linux）/ launchd plist（macOS）开机自启
#   4. 立即启动并做健康检查
#
# 用法：
#   ./scripts/install.sh                                  # 自动识别平台
#   ./scripts/install.sh --binary ./llmate-gate-linux-amd64
#   ./scripts/install.sh --port 8400 --no-autostart
#   ./scripts/install.sh --dry-run                         # 只打印动作，不实际改动系统
#   ./scripts/install.sh --uninstall                       # 不需要 --binary
#
# 配置键名以 Specs/05-实现规格-AS_BUILT.md §3.1 为准。网关解析配置为
# 严格模式：出现未知键会直接启动失败（这是刻意的，避免拼错的键被静默忽略）。
set -euo pipefail

PRODUCT="llmate-gate"
INSTALL_BIN="${HOME}/.local/bin/${PRODUCT}"
DATA_DIR="${XDG_DATA_HOME:-${HOME}/.local/share}/${PRODUCT}"
CONFIG="${DATA_DIR}/config.yaml"
SERVICE_NAME="llmate-gate.service"
LAUNCHD_LABEL="com.${PRODUCT}.daemon"
PORT="8400"
NO_AUTOSTART=0
DO_UNINSTALL=0
DRY_RUN=0
BIN_SRC=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary)  BIN_SRC="$2"; shift 2 ;;
    --port)    PORT="$2"; shift 2 ;;
    --no-autostart) NO_AUTOSTART=1; shift ;;
    --uninstall) DO_UNINSTALL=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help)
      sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '[install] %s\n' "$*"; }
fail() { printf '[install][FAIL] %s\n' "$*" >&2; exit 1; }

# DRY_RUN 模式：把所有"破坏性操作"只打印不执行
run() {
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

# run_write <target>：从 stdin 读内容写入 target。
# 写文件同样是破坏性操作——旧版用裸 `cat >` 生成配置，导致 --dry-run 也真的
# 落盘了一份 config.yaml；这份多出来的文件又会让下次真装的 --port 失效。
run_write() {
  local target="$1"
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  [dry-run] write %s (%s 行)\n' "$target" "$(wc -l | tr -d ' ')"
  else
    cat > "$target"
  fi
}

OS="$(uname -s | tr 'A-Z' 'a-z')"
case "$OS" in
  linux) ;;
  darwin) ;;
  *)
    fail "unsupported OS: $OS (uname -s → $OS)。本脚本仅支持 Linux + macOS。"
    ;;
esac

# --- 卸载 ---
# 必须早于 binary 定位：卸载不需要 binary 源文件，旧版把这段放在 `! -f "$BIN_SRC"`
# 检查之后，导致不带 --binary 时卸载直接失败。
if [[ $DO_UNINSTALL -eq 1 ]]; then
  log "卸载 $PRODUCT"
  case "$OS" in
    linux)
      # 服务未安装（容器无 dbus、或从未 enable）都属正常情况，
      # 不能让 set -e 在第一步就中断掉整个卸载。
      run systemctl --user disable --now "$SERVICE_NAME" || true
      run rm -f "${HOME}/.config/systemd/user/${SERVICE_NAME}"
      run systemctl --user daemon-reload || true
      ;;
    darwin)
      run launchctl bootout "gui/$(id -u)/${LAUNCHD_LABEL}" || true
      run rm -f "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
      ;;
  esac
  run pkill -f "$INSTALL_BIN" || true
  # 二进制必须一并删除：只摘服务不删文件的话，~/.local/bin 里仍留着可执行文件。
  run rm -f "$INSTALL_BIN"
  log "  已清理服务、进程与 $INSTALL_BIN"
  log "  数据目录 $DATA_DIR 保留（含配置与日志；如需清空请手动 rm -r）"
  exit 0
fi

# --- 定位 binary（仅安装路径需要）---
guess_binary() {
  local repo_root arch
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  # 架构必须按**本机实际**推导：写死 amd64 会在 arm64 机器上找一个不存在的文件，
  # 而 arm64 上恰好存在 amd64 产物时（交叉编译后）又会装上跑不起来的二进制。
  # 命名规范与 scripts/release.sh / CI release.yml 一致：llmate-gate-<goos>-<goarch>[.exe]
  case "$(uname -m)" in
    x86_64|amd64)   arch=amd64 ;;
    aarch64|arm64)  arch=arm64 ;;
    armv7l|armv6l)  arch=arm   ;;
    i?86)           arch=386   ;;
    *)              arch="$(uname -m)" ;;
  esac
  case "$OS" in
    linux)   echo "${repo_root}/dist/llmate-gate-linux-${arch}" ;;
    darwin)  echo "${repo_root}/dist/llmate-gate-darwin-${arch}" ;;
    windows) echo "${repo_root}/dist/llmate-gate-windows-${arch}.exe" ;;
  esac
}

# 找不到 binary 时，把 dist/ 里现有产物列出来，而不是只说「找不到」。
list_available_binaries() {
  local repo_root
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  if [[ -d "${repo_root}/dist" ]]; then
    local found
    found="$(find "${repo_root}/dist" -maxdepth 1 -name 'llmate-gate-*' -type f 2>/dev/null | sort)"
    if [[ -n "$found" ]]; then
      echo "  dist/ 里现有的产物：" >&2
      echo "$found" | sed 's/^/    /' >&2
      return 0
    fi
  fi
  echo "  dist/ 目录为空或不存在。" >&2
}

if [[ -z "$BIN_SRC" ]]; then BIN_SRC="$(guess_binary)"; fi
if [[ ! -f "$BIN_SRC" ]]; then
  printf '[install][FAIL] 找不到 binary：%s。请先 ./scripts/release.sh 编译，或用 --binary 指定。\n' \
    "$BIN_SRC" >&2
  list_available_binaries
  exit 1
fi

# --- 安装 ---
run mkdir -p "${HOME}/.local/bin" "$DATA_DIR"

# 拷 binary
run install -m 0755 "$BIN_SRC" "$INSTALL_BIN"
log "已安装 → $INSTALL_BIN"

# 既有配置可能用了别的端口：健康检查按 --port 走，两者不一致会让安装看起来
# 失败（其实是查错了端口）。这里明确告知，并以 --port 为准启动。
if [[ -f "$CONFIG" ]]; then
  cfg_port="$(sed -n 's/^[[:space:]]*listen:[[:space:]]*"\?:\{0,1\}\([0-9]\{2,\}\)"\?.*/\1/p' "$CONFIG" | head -1)"
  if [[ -n "$cfg_port" && "$cfg_port" != "$PORT" ]]; then
    log "注意：既有配置 $CONFIG 使用端口 :${cfg_port}，本次 --port 为 :${PORT}。"
    log "      将以 :${PORT} 启动（--listen 优先于配置文件），并按其做健康检查。"
  fi
fi

# 默认 config
if [[ ! -f "$CONFIG" ]]; then
  run_write "$CONFIG" <<YAML
# LLMate Gate 配置（最小可用）
# 键名以 Specs/05-实现规格-AS_BUILT.md §3.1 为准。
# 网关以严格模式解析：未知键会导致启动失败，不会静默忽略。
gateway:
  listen: ":${PORT}"
  upstream: "https://api.openai.com"     # 上游 base URL
  upstream_api_key: ""                   # 必填：上游 LLM 的 API key
  debug: true
detection:
  engine: "regex"
replacement:
  strategy: "placeholder"
YAML
  log "已生成默认配置 → $CONFIG"
  log "  ⚠ 请填写 gateway.upstream_api_key（上游 API key），否则转发会被上游拒绝"
fi

# 平台特定的"开机自启"
if [[ $NO_AUTOSTART -eq 0 ]]; then
  case "$OS" in
    linux)
      run mkdir -p "${HOME}/.config/systemd/user"
      run_write "${HOME}/.config/systemd/user/${SERVICE_NAME}" <<UNIT
[Unit]
Description=LLMate Gate (PII 脱敏本机网关)
After=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_BIN} --config ${CONFIG} --listen :${PORT}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
UNIT
      run systemctl --user daemon-reload || true
      run systemctl --user enable --now "$SERVICE_NAME" || true
      log "已注册 systemd --user unit（loginctl enable-linger \$USER 后才真正开机自启）"
      ;;
    darwin)
      run mkdir -p "${HOME}/Library/LaunchAgents"
      run_write "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LAUNCHD_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${INSTALL_BIN}</string>
    <string>--config</string>
    <string>${CONFIG}</string>
    <string>--listen</string>
    <string>:${PORT}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>${DATA_DIR}/stdout.log</string>
  <key>StandardErrorPath</key><string>${DATA_DIR}/stderr.log</string>
</dict>
</plist>
PLIST
      run launchctl bootstrap "gui/$(id -u)" "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist" || true
      log "已注册 launchd agent"
      ;;
  esac
fi

# 立即启动（如果没装服务）
# --listen 显式传入：否则端口由配置文件决定，与脚本健康检查用的 $PORT 可能不一致。
if [[ $NO_AUTOSTART -eq 1 ]]; then
  if [[ $DRY_RUN -eq 1 ]]; then
    printf '  [dry-run] nohup %s --config %s --listen :%s &\n' "$INSTALL_BIN" "$CONFIG" "$PORT"
  else
    nohup "$INSTALL_BIN" --config "$CONFIG" --listen ":${PORT}" >"$DATA_DIR/stdout.log" 2>"$DATA_DIR/stderr.log" &
    sleep 1
    log "已后台启动（PID=$!，日志 $DATA_DIR/）"
  fi
fi

# 健康检查：重试若干次，避免把"还在启动"误判为失败
if [[ $DRY_RUN -eq 1 ]]; then
  log "（dry-run：跳过健康检查）"
else
  health_ok=0
  for _ in 1 2 3 4 5 6; do
    if curl -fsS -m 2 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      health_ok=1; break
    fi
    sleep 1
  done

  if [[ $health_ok -eq 1 ]]; then
    log "✓ 健康检查通过"
  else
    log "WARN 健康检查未通过（:${PORT} 等待 6s 无响应）。最近日志："
    if [[ -f "$DATA_DIR/stderr.log" ]]; then
      tail -n 5 "$DATA_DIR/stderr.log" | sed 's/^/    /'
    fi
    log "  文件已安装，但服务未就绪。常见原因：端口被占用、配置有误、上游 key 未填。"
    exit 1
  fi
fi

log "安装完成。"
log "  调试面板：  http://127.0.0.1:${PORT}/_debug"
log "  卸载：    $0 --uninstall"
