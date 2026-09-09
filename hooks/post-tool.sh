#!/usr/bin/env bash
# Claude Code PostToolUse hook —— LLMate Gate 出参 PII 告警。
# 读取 stdin 的 hook 事件 JSON，交给 lmgate_hook.py 处理（stdlib python3，零依赖）。
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$DIR/lmgate_hook.py" post
