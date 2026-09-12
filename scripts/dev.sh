#!/usr/bin/env bash
# LLMate Gate 开发辅助脚本
#
# 背景（环境验证结论）：当仓位于 WSL 的 9P 网络共享 (\\wsl.localhost\...)
# 时，Go 工具链无法对路径上的 go.mod 加文件锁（报错：RLock ...\go.mod:
# Incorrect function），因此 go build/test 必须在一个**本机文件系统**的
# scratch 目录里执行。本脚本负责把源码 rsync 到 scratch 目录再编译。
#
# 默认 scratch 目录：
#   - 仓不在 9P（普通 Linux/macOS）   → 仓内 .build/   （.gitignore 已忽略）
#   - 仓在 9P（WSL 远程挂载）         → mktemp -d 一次性目录，脚本退出后保留
#   用户可通过 env 覆盖：LMGATE_BUILD=/path  强制 scratch 目录
#
# 用法：
#   ./scripts/dev.sh envcheck    # 环境验证
#   ./scripts/dev.sh sync        # 同步源码到 scratch 目录
#   ./scripts/dev.sh build       # 编译二进制
#   ./scripts/dev.sh test        # 跑测试（含覆盖率）
#   ./scripts/dev.sh vet         # 静态检查
#   ./scripts/dev.sh run [args]  # 运行网关
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 判断仓是否在 WSL 9P 路径下（路径含 //wsl.localhost/、\\wsl.localhost\、/9p/）。
is_9p_workspace() {
  case "$REPO_ROOT" in
    *9p*|*"\\wsl"*|*"/wsl.localhost"*) return 0 ;;
    *) return 1 ;;
  esac
}

# 默认 scratch 目录：非 9P 用仓内 .build/；9P 退回一次性 mktemp 目录。
default_build_dir() {
  if is_9p_workspace; then
    mktemp -d -t lmgate-build.XXXXXX
  else
    printf '%s' "$REPO_ROOT/.build"
  fi
}

BUILD_DIR="${LMGATE_BUILD:-$(default_build_dir)}"
SRC_DIR="${BUILD_DIR}/gateway"

# Go 模块代理：proxy.golang.org 在国内常不可达，使用 goproxy.cn
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

  # go.mod 文件锁能力（9P 路径会失败，所以 scratch 已自动落到本机 fs）
  mkdir -p "$BUILD_DIR"
  if (cd "$BUILD_DIR" && go env GOMODCACHE >/dev/null 2>&1); then
    printf '%-24s %s\n' "local_fs_lock" "OK ($BUILD_DIR)"
  else
    printf '%-24s %s\n' "local_fs_lock" "FAIL"; ok=0
  fi
  if is_9p_workspace; then
    printf '%-24s %s\n' "workspace_path" "9P（scratch 已自动避开）"
  else
    printf '%-24s %s\n' "workspace_path" "本机 fs（直接仓内构建即可）"
  fi

  [ "$ok" = "1" ] && log "=== 环境验证通过 ===" || { log "=== 环境验证存在告警 ==="; return 0; }
}

sync() {
  log "sync $REPO_ROOT/gateway -> $SRC_DIR"
  mkdir -p "$BUILD_DIR"
  # go.sum 已入库（CI 干净 checkout 必需），随源码一起同步。
  [ -f "$REPO_ROOT/gateway/go.sum" ] || log "WARN gateway/go.sum 缺失，构建可能失败"
  rm -rf "$SRC_DIR"
  cp -r "$REPO_ROOT/gateway" "$SRC_DIR"
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

# 构建三套二进制（e2e 需要 mock-llm / mock-detector）。
# 注意：-o 千万别写 /c/Users/... 这类 POSIX 路径——Windows 版 Go 会解析成
# C:////c////Users////...，二进制静默落到别处，测试跑的还是旧文件。一律用相对路径 + cp。
buildall() {
  need_sync
  (cd "$SRC_DIR" && \
    go build -o ./llmate-gate.exe ./cmd/llmate-gate && \
    go build -o ./mock-llm.exe ./cmd/mock-llm && \
    go build -o ./mock-detector.exe ./cmd/mock-detector && \
    go build -o ./mcp-server.exe ./cmd/mcp-server) || return 1
  cp "$SRC_DIR/llmate-gate.exe" "$SRC_DIR/mock-llm.exe" "$SRC_DIR/mock-detector.exe" "$SRC_DIR/mcp-server.exe" "$BUILD_DIR/" && \
    log "buildall ok -> $BUILD_DIR"
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
  buildall) buildall "$@" ;;
  vet)      vet "$@" ;;
  test)     test "$@" ;;
  run)      run "$@" ;;
  *)        fail "unknown command: $cmd" ;;
esac
