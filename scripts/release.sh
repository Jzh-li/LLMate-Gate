#!/usr/bin/env bash
# scripts/release.sh —— LLMate Gate 跨平台 release 打包（最小分发）
#
# 目标平台（5 个组合；darwin 需在 macOS 主机或 osxcross 下编译，本脚本不强制要求）：
#   linux/amd64         (主力服务器 / WSL)
#   linux/arm64         (Apple Silicon on Asahi / ARM server)
#   darwin/amd64        (Intel Mac)
#   darwin/arm64        (Apple Silicon)
#   windows/amd64       (主力桌面)
#
# 产物布局：
#   dist/
#   ├── llmate-gate-linux-amd64
#   ├── llmate-gate-linux-arm64
#   ├── llmate-gate-darwin-amd64
#   ├── llmate-gate-darwin-arm64
#   ├── llmate-gate-windows-amd64.exe
#   └── SHA256SUMS
#
# 用法：
#   ./scripts/release.sh                # 全部平台，版本=git describe
#   ./scripts/release.sh v0.1.0         # 指定 tag
#   ./scripts/release.sh v0.1.0 linux    # 仅 Linux
#   LMGDIR=/path/to/built/binary ./scripts/release.sh v0.1.0
#
# 依赖：本机装 go ≥ 1.22；无需 Docker / CGO 工具链。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
FILTER="${2:-all}"
# 默认 dist 目录：仓内 .build/dist（.gitignore 已忽略）；可通过 LMGBUILD 覆盖
OUT_DIR="${LMGBUILD:-$REPO_ROOT/.build/dist}"
LMGDIR="${LMGDIR:-$REPO_ROOT}"

# CGO 必须禁用，否则 _third_party 库会引入 glibc 依赖，跨发行版会挂
export CGO_ENABLED=0
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

log()  { printf '[release] %s\n' "$*"; }
fail() { printf '[release][FAIL] %s\n' "$*" >&2; exit 1; }

# 检测仓是否在 WSL 9P 路径下
is_9p_workspace() {
  case "$REPO_ROOT" in
    *9p*|*"\\wsl"*|*"/wsl.localhost"*) return 0 ;;
    *) return 1 ;;
  esac
}

# 当 LMGDIR 是 9P 路径下的源码目录时，临时借用 dev.sh 的 scratch 逻辑同步编译。
# 非 9P 下 LMGDIR 通常已经是本机 fs，直接原地构建即可，无需额外同步。
prepare_build() {
  if [[ "$REPO_ROOT" == "$LMGDIR" ]] && is_9p_workspace; then
    log "检测到 WSL 9P 路径，使用 dev.sh 同步到 scratch 目录编译"
    bash "$REPO_ROOT/scripts/dev.sh" sync
    LMGDIR="$LMGATE_BUILD"
    if [[ -z "$LMGDIR" || ! -d "$LMGDIR/gateway" ]]; then
      # dev.sh 默认 scratch（mktemp）未知具体路径；保守回退
      LMGDIR="$LMGDIR/gateway"
    fi
  fi
}

# 单平台编译。os/arch/exe 由调用方决定
build_one() {
  local goos="$1" goarch="$2" ext="${3:-}"
  local out_name="llmate-gate-${goos}-${goarch}${ext}"
  log "==> ${goos}/${goarch}"
  # 必须 cd 到模块目录（NTFS scratch 已知怪事：跨目录 -o 不落盘）
  ( cd "$LMGDIR" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
      -o "$out_name" ./cmd/llmate-gate )
  mv -f "$LMGDIR/$out_name" "$OUT_DIR/$out_name"
  printf '  %-50s %d bytes\n' "$OUT_DIR/$out_name" "$(stat -c %s "$OUT_DIR/$out_name" 2>/dev/null || stat -f %z "$OUT_DIR/$out_name")"
}

# 当前机器可编译的平台
current_can_build() {
  local goos="$1" goarch="$2"
  local cur_os cur_arch
  cur_os="$(uname -s | tr 'A-Z' 'a-z')"
  cur_arch="$(uname -m)"
  case "$cur_arch" in x86_64) cur_arch=amd64;; aarch64) cur_arch=arm64;; esac
  case "$cur_os" in mingw*|msys*) cur_os=windows;; darwin) cur_os=darwin;; linux) cur_os=linux;; esac
  [[ "$goos" == "$cur_os" && "$goarch" == "$cur_arch" ]]
}

run() {
  prepare_build
  mkdir -p "$OUT_DIR"
  ( cd "$(dirname "$OUT_DIR")" && rm -f "$OUT_DIR"/llmate-gate-* "$OUT_DIR/SHA256SUMS" )

  local -a targets=(
    "linux amd64"
    "linux arm64"
    "darwin amd64"
    "darwin arm64"
    "windows amd64 .exe"
  )

  for t in "${targets[@]}"; do
    # shellcheck disable=SC2206
    local parts=($t)
    local goos="${parts[0]}" goarch="${parts[1]}" ext="${parts[2]:-}"
    case "$FILTER" in
      all) ;;
      linux)   [[ "$goos" == linux   ]] || continue ;;
      darwin)  [[ "$goos" == darwin  ]] || continue ;;
      windows) [[ "$goos" == windows ]] || continue ;;
      *) fail "unknown filter: $FILTER (all|linux|darwin|windows)" ;;
    esac
    if ! current_can_build "$goos" "$goarch"; then
      log "  skip ${goos}/${goarch}（当前机器 $(uname -s)/$(uname -m) 不能原生编译）"
      continue
    fi
    build_one "$goos" "$goarch" "$ext"
  done

  log "==> SHA256SUMS"
  ( cd "$OUT_DIR" && sha256sum llmate-gate-* > SHA256SUMS )
  cat "$OUT_DIR/SHA256SUMS"

  log "==> 完成。产物："
  ls -la "$OUT_DIR"
}

run
