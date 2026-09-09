#!/usr/bin/env bash
# LLMate Gate 开发辅助脚本
#
# 背景（环境验证结论）：本仓库工作区位于 WSL 的 9P 网络共享
# (\\wsl.localhost\Debian\...)，Go 工具链无法对该路径上的 go.mod 加文件锁
# （报错：RLock ...\go.mod: Incorrect function），因此 `go build/test` 必须
# 在**本地 NTFS 目录**执行。本脚本负责把源码同步到本地 scratch 目录再编译。
#
# 用法：
#   ./scripts/dev.sh envcheck    # 环境验证
#   ./scripts/dev.sh sync        # 同步源码到构建目录
#   ./scripts/dev.sh build       # 编译二进制
#   ./scripts/dev.sh test        # 跑测试（含覆盖率）
#   ./scripts/dev.sh vet         # 静态检查
#   ./scripts/dev.sh run [args]  # 运行网关
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# 本地 NTFS 构建目录（可通过 LMGATE_BUILD 覆盖）
BUILD_DIR="${LMGATE_BUILD:-/c/Users/jzh-l/AppData/Local/Temp/lmgate}"
SRC_DIR="${BUILD_DIR}/gateway"

# Go 模块代理：proxy.golang.org 在本机不可达，使用 goproxy.cn
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export CGO_ENABLED=0

log()  { printf '[dev] %s\n' "$*"; }
fail() { printf '[dev][FAIL] %s\n' "$*" >&2; exit 1; }

envcheck() {
  log "=== LLMate Gate 环境验证 ==="
  local ok=1

  printf '%-24s %s\n' "repo" "$REPO_ROOT"
  printf '%-24s %s\n' "build_dir" "$BUILD_DIR"

  if command -v go >/dev/null 2>&1; then
    printf '%-24s %s\n' "go" "$(go version)"
  else
    printf '%-24s %s\n' "go" "MISSING"; ok=0
  fi

  if command -v git >/dev/null 2>&1; then
    printf '%-24s %s\n' "git" "$(git --version)"
    printf '%-24s %s\n' "remote" "$(git -C "$REPO_ROOT" -c safe.directory='*' remote get-url origin 2>/dev/null || echo 'N/A')"
  else
    printf '%-24s %s\n' "git" "MISSING"; ok=0
  fi

  # 模块代理连通性
  local code
  code="$(curl -s -o /dev/null -m 15 -w '%{http_code}' https://goproxy.cn/github.com/stretchr/testify/@v/list || echo 000)"
  printf '%-24s %s\n' "goproxy.cn" "$code"
  [ "$code" = "200" ] || { log "WARN goproxy.cn 不可达，go mod tidy 可能失败"; ok=0; }

  # go.mod 文件锁能力（WSL UNC 路径会失败）
  mkdir -p "$BUILD_DIR"
  if go -C "$BUILD_DIR" version >/dev/null 2>&1 || true; then :; fi
  if (cd "$BUILD_DIR" && go env GOMODCACHE >/dev/null 2>&1); then
    printf '%-24s %s\n' "local_fs_lock" "OK"
  else
    printf '%-24s %s\n' "local_fs_lock" "FAIL"; ok=0
  fi
  printf '%-24s %s\n' "workspace_lock" "SKIP (9P share 不支持，改用 $BUILD_DIR 构建)"

  [ "$ok" = "1" ] && log "=== 环境验证通过 ===" || { log "=== 环境验证存在告警 ==="; return 0; }
}

sync() {
  log "sync $REPO_ROOT/gateway -> $SRC_DIR"
  mkdir -p "$BUILD_DIR"
  # 保留 SRC_DIR 的 go.sum（9P UNC 不支持 go mod tidy，源码库不带 go.sum）。
  # 否则每次 sync 都会让 build 因 go.sum 缺失失败。
  local GO_SUM_BACKUP=""
  if [ -f "$SRC_DIR/go.sum" ]; then
    GO_SUM_BACKUP=$(mktemp)
    cp "$SRC_DIR/go.sum" "$GO_SUM_BACKUP"
  fi
  rm -rf "$SRC_DIR"
  cp -r "$REPO_ROOT/gateway" "$SRC_DIR"
  if [ -n "$GO_SUM_BACKUP" ]; then
    cp "$GO_SUM_BACKUP" "$SRC_DIR/go.sum"
    rm -f "$GO_SUM_BACKUP"
  fi
  log "sync done"
}

need_sync() { [ -d "$SRC_DIR" ] || sync; }

build() {
  need_sync
  # 注意：Go 在 WSL 下使用 -o /mnt/c/... 路径会失败（NTFS 写权限问题），
  # 改用当前目录下输出，再用 cp 移动到 BUILD_DIR（Windows 路径）。
  (cd "$SRC_DIR" && go build -o ./llmate-gate.exe ./cmd/llmate-gate) \
    && cp "$SRC_DIR/llmate-gate.exe" "$BUILD_DIR/llmate-gate.exe" \
    && log "build ok -> $BUILD_DIR/llmate-gate.exe"
}

vet() {
  need_sync
  (cd "$SRC_DIR" && go vet ./...)
}

test() {
  need_sync
  local args=("$@")
  [ ${#args[@]} -eq 0 ] && args=(-race -coverprofile="$BUILD_DIR/cover.out" ./...)
  (cd "$SRC_DIR" && go test "${args[@]}")
}

run() {
  need_sync
  (cd "$SRC_DIR" && go run ./cmd/llmate-gate "$@")
}

cmd="${1:-envcheck}"
shift || true
case "$cmd" in
  envcheck) envcheck "$@" ;;
  sync)     sync "$@" ;;
  build)    build "$@" ;;
  vet)      vet "$@" ;;
  test)     test "$@" ;;
  run)      run "$@" ;;
  *)        fail "unknown command: $cmd" ;;
esac
