#!/usr/bin/env bash
# scripts/install.sh —— LLMate Gate Linux/macOS 一键安装（最小分发）
#
# 行为：
#   1. 拷 llmate-gate 到 ~/.local/bin/
#   2. 创建 systemd --user unit（Linux）/ launchd plist（macOS）开机自启
#   3. 立即启动
#
# 用法：
#   ./scripts/install.sh                                  # 自动识别平台
#   ./scripts/install.sh --binary ./llmate-gate-linux-amd64
#   ./scripts/install.sh --port 8400 --no-autostart
#   ./scripts/install.sh --uninstall
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
BIN_SRC=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary)  BIN_SRC="$2"; shift 2 ;;
    --port)    PORT="$2"; shift 2 ;;
    --no-autostart) NO_AUTOSTART=1; shift ;;
    --uninstall) DO_UNINSTALL=1; shift ;;
    -h|--help)
      sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '[install] %s\n' "$*"; }
fail() { printf '[install][FAIL] %s\n' "$*" >&2; exit 1; }

OS="$(uname -s | tr 'A-Z' 'a-z')"
case "$OS" in linux) ;; darwin) ;; *) fail "unsupported OS: $OS" ;; esac

# 默认 binary 路径
guess_binary() {
  local repo_root
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  case "$OS" in
    linux)  echo "${repo_root}/dist/llmate-gate-linux-amd64" ;;
    darwin) echo "${repo_root}/dist/llmate-gate-darwin-arm64" ;;
  esac
}

if [[ -z "$BIN_SRC" ]]; then BIN_SRC="$(guess_binary)"; fi
if [[ ! -f "$BIN_SRC" ]]; then
  fail "找不到 binary：$BIN_SRC。请先 ./scripts/release.sh 编译，或用 --binary 指定。"
fi

# --- 卸载 ---
if [[ $DO_UNINSTALL -eq 1 ]]; then
  log "卸载 $PRODUCT"
  case "$OS" in
    linux)
      systemctl --user disable --now "$SERVICE_NAME" 2>/dev/null || true
      rm -f "${HOME}/.config/systemd/user/${SERVICE_NAME}"
      systemctl --user daemon-reload
      ;;
    darwin)
      launchctl bootout "gui/$(id -u)/${LAUNCHD_LABEL}" 2>/dev/null || true
      rm -f "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
      ;;
  esac
  pkill -f "$INSTALL_BIN" 2>/dev/null || true
  log "  已清理服务与进程"
  log "  数据目录 $DATA_DIR 保留（如需清空请手动 rm -r）"
  exit 0
fi

# --- 安装 ---
mkdir -p "${HOME}/.local/bin" "$DATA_DIR"

# 拷 binary
install -m 0755 "$BIN_SRC" "$INSTALL_BIN"
log "已安装 → $INSTALL_BIN"

# 默认 config
if [[ ! -f "$CONFIG" ]]; then
  cat > "$CONFIG" <<YAML
# LLMate Gate 配置（最小可用）
gateway:
  listen: ":${PORT}"
  debug: true
upstream:
  base_url: "https://api.openai.com"
  api_key: ""            # 必填：上游 LLM API key
detector:
  engine: "regex"
replacer:
  strategy: "placeholder"
YAML
  log "已生成默认配置 → $CONFIG（请补 api_key）"
fi

# 平台特定的"开机自启"
if [[ $NO_AUTOSTART -eq 0 ]]; then
  case "$OS" in
    linux)
      mkdir -p "${HOME}/.config/systemd/user"
      cat > "${HOME}/.config/systemd/user/${SERVICE_NAME}" <<UNIT
[Unit]
Description=LLMate Gate (PII 脱敏本机网关)
After=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_BIN} --config ${CONFIG}
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
UNIT
      systemctl --user daemon-reload
      systemctl --user enable --now "$SERVICE_NAME"
      log "已注册 systemd --user unit（loginctl enable-linger \$USER 后才真正开机自启）"
      ;;
    darwin)
      mkdir -p "${HOME}/Library/LaunchAgents"
      cat > "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist" <<PLIST
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
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>${DATA_DIR}/stdout.log</string>
  <key>StandardErrorPath</key><string>${DATA_DIR}/stderr.log</string>
</dict>
</plist>
PLIST
      launchctl bootstrap "gui/$(id -u)" "${HOME}/Library/LaunchAgents/${LAUNCHD_LABEL}.plist"
      log "已注册 launchd agent"
      ;;
  esac
fi

# 立即启动（如果没装服务）
if [[ $NO_AUTOSTART -eq 1 ]]; then
  nohup "$INSTALL_BIN" --config "$CONFIG" >"$DATA_DIR/stdout.log" 2>"$DATA_DIR/stderr.log" &
  sleep 1
  log "已后台启动（PID=$!，日志 $DATA_DIR/）"
fi

# 健康检查
sleep 1
if curl -fsS -m 3 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
  log "✓ 健康检查通过"
else
  log "WARN 健康检查未通过（可能还在启动或端口被占用）"
fi

log "安装完成。"
log "  调试面板：  http://127.0.0.1:${PORT}/_debug"
log "  卸载：    $0 --uninstall"
