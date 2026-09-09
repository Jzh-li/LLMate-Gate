#!/usr/bin/env python3
"""LLMate Gate —— Claude Code hooks 核心逻辑（stdlib only，零外部依赖）。

把 Claude Code 的 PreToolUse / PostToolUse 事件接到 LLMate Gate 的常驻隐私
API（POST /v1/privacy/redact）做递归脱敏与 PII 告警。

Claude Code hook 协议的硬约束（决定本脚本的设计）：
  - PreToolUse  可以 allow / deny / ask，且可通过 `updatedInput` **整体替换**
                  tool_input（不能只改某个字段，必须回显未改动的字段）。
  - PostToolUse 只能「观察」：可用 systemMessage 把告警注入上下文，或 exit 2
                  阻断——但**无法改写已经产生的 tool_result**。

因此本脚本的角色是「隐私闸门 + 出参告警」：
  - PreToolUse：递归扫描 tool_input。若检出 PII：
        · 默认（LMGATE_HOOK_MODE=block）DENY，reason 附脱敏预览，用户可手动放行；
        · 若设 LMGATE_HOOK_MODE=redact，则 ALLOW 并回写脱敏后的 updatedInput
          （工具以占位符参数运行，真实值保存在网关 vault）。
      未检出 PII 则直接 ALLOW。
  - PostToolUse：扫描 tool_output，若检出 PII 则通过 systemMessage 告警
      （工具已执行，只能提示，无法改写其返回值）。

真正的「入参脱敏 + 出参还原」透明替换属于 OpenAI 兼容代理路径
（base_url -> localhost:8400），由 VS Code 扩展（任务 2.1）负责。
"""
import json
import os
import sys
import urllib.request
import urllib.error

GATEWAY = os.environ.get("LMGATE_GATEWAY", "http://127.0.0.1:8400").rstrip("/")
AUTH = os.environ.get("LMGATE_AUTH_TOKEN", "")
MODE = os.environ.get("LMGATE_HOOK_MODE", "block").lower()  # block | redact
FAIL_CLOSED = os.environ.get("LMGATE_HOOK_FAIL_CLOSED", "0") == "1"

# 这些工具的 file_path / old_string / new_string 若被脱敏会直接破坏工具执行，
# 故在 redact 模式下整体保留、不参与脱敏。
FILE_TOOLS = {"Write", "Edit", "MultiEdit", "NotebookEdit"}
PRESERVE_KEYS = ("file_path", "old_string", "new_string")

PREVIEW_MAX = 2000


def _log(msg):
    # 调试信息走 stderr，绝不污染 stdout（stdout 必须是合规 JSON）。
    if os.environ.get("LMGATE_HOOK_DEBUG"):
        sys.stderr.write("[lmgate_hook] " + msg + "\n")


def call_redact(payload):
    """对网关 /v1/privacy/redact 发请求。返回 (resp_dict, err_str)。"""
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(
        GATEWAY + "/v1/privacy/redact",
        data=body,
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + AUTH,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return json.loads(r.read().decode("utf-8")), None
    except urllib.error.HTTPError as e:
        return None, "HTTP %d from gateway" % e.code
    except Exception as e:  # noqa: BLE001 - 钩子必须容忍任何网络异常
        return None, str(e) or type(e).__name__


def emit_pre(decision, reason=None, updated_input=None, system_message=None):
    out = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": decision,
        }
    }
    if reason is not None:
        out["hookSpecificOutput"]["permissionDecisionReason"] = reason
    if updated_input is not None:
        out["hookSpecificOutput"]["updatedInput"] = updated_input
    if system_message is not None:
        out["systemMessage"] = system_message
    sys.stdout.write(json.dumps(out, ensure_ascii=False))
    sys.exit(0)


def pre_tool(evt):
    tool = evt.get("tool_name", "")
    inp = evt.get("tool_input")
    if not isinstance(inp, (dict, list, str)):
        # 无结构化入参可扫（如某些工具的纯标量），直接放行。
        emit_pre("allow")
        return

    # redact 模式下，写/改文件类工具的关键字段先抽出来、脱敏后再塞回。
    preserve = {}
    if tool in FILE_TOOLS and isinstance(inp, dict):
        for k in PRESERVE_KEYS:
            if k in inp:
                preserve[k] = inp[k]

    payload = {"json": inp, "strategy": "placeholder"}
    resp, err = call_redact(payload)

    if err is not None:
        if FAIL_CLOSED:
            emit_pre(
                "deny",
                reason=("⛔ LLMate Gate 不可达（%s），按 fail-closed 拦截工具调用。"
                        "如需放行请设置 LMGATE_HOOK_FAIL_CLOSED=0。" % err),
            )
        # 默认 fail-open：网关挂了不能让整个工作流卡死，仅告警。
        emit_pre(
            "allow",
            system_message=("⚠️ LLMate Gate 不可达（%s），工具参数未经脱敏扫描。"
                            "请确认网关已在 %s 运行。" % (err, GATEWAY)),
        )
        return

    if not resp.get("changed"):
        emit_pre("allow")
        return

    redacted = resp.get("json", inp)
    if isinstance(redacted, dict):
        for k, v in preserve.items():
            redacted[k] = v

    if MODE == "redact":
        emit_pre(
            "allow",
            updated_input=redacted,
            system_message="🔒 LLMate Gate 已对工具参数递归脱敏（占位符由网关侧还原）",
        )
        return

    # 默认 block：拦截 + 脱敏预览，用户在 Claude Code 里可手动放行。
    preview = json.dumps(redacted, ensure_ascii=False)
    if len(preview) > PREVIEW_MAX:
        preview = preview[:PREVIEW_MAX] + " …(已截断)"
    emit_pre(
        "deny",
        reason=("⛔ LLMate Gate 检测到工具参数含敏感信息（手机号 / 邮箱 / 身份证等），已拦截。\n"
                "脱敏预览：\n" + preview + "\n"
                "（如需本次放行，请在 Claude Code 中确认；或设 LMGATE_HOOK_MODE=redact 自动脱敏后执行）"),
    )


def post_tool(evt):
    out = evt.get("tool_output")
    if out is None:
        sys.exit(0)
    if isinstance(out, str):
        payload = {"text": out}
    else:
        payload = {"json": out}

    resp, err = call_redact(payload)
    if err is not None or not resp.get("changed"):
        sys.exit(0)  # 干净退出，不污染 transcript

    msg = ("⚠️ LLMate Gate：工具返回内容中包含疑似敏感信息"
           "（手机号 / 邮箱 / 身份证 / 银行卡等）。请确认其是否应被外发或落盘。")
    sys.stdout.write(json.dumps({"systemMessage": msg}, ensure_ascii=False))
    sys.exit(0)


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in ("pre", "post"):
        sys.stderr.write("usage: lmgate_hook.py {pre|post}\n")
        sys.exit(2)
    raw = sys.stdin.read()
    try:
        evt = json.loads(raw) if raw.strip() else {}
    except json.JSONDecodeError as e:
        _log("stdin 非 JSON: %s" % e)
        evt = {}

    if sys.argv[1] == "pre":
        pre_tool(evt)
    else:
        post_tool(evt)


if __name__ == "__main__":
    main()
