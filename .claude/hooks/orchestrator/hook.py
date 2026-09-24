"""Single-object stdin/stdout entry point for orchestrator hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
import sys
from typing import Any

import codex
import json_transport


def dispatch(event: Mapping[str, Any]) -> dict[str, Any]:
    hook_name = event.get("hook_event_name")
    if hook_name == "Stop":
        return codex.stop(event)
    if event.get("tool_name") not in codex.CODEX_TOOLS:
        return {}
    handler = {
        "PreToolUse": codex.pre_tool_use,
        "PostToolUse": codex.post_tool_use,
        "PostToolUseFailure": codex.post_tool_use_failure,
    }.get(hook_name)
    return handler(event) if handler is not None else {}


def main() -> int:
    response: dict[str, Any] = {}
    value: Any = None
    try:
        stdin_buffer = getattr(sys.stdin, "buffer", None)
        if stdin_buffer is None:
            value = json.load(sys.stdin)
        else:
            value = json.loads(stdin_buffer.read().decode("utf-8", errors="strict"))
        if isinstance(value, Mapping):
            response = dispatch(value)
    except Exception as error:
        if (
            isinstance(value, Mapping)
            and value.get("hook_event_name") == "PreToolUse"
            and value.get("tool_name") in codex.CODEX_TOOLS
        ):
            try:
                response = codex.internal_pre_error_response(value, error)
            except Exception:
                response = {}
        elif (
            isinstance(value, Mapping)
            and value.get("hook_event_name") == "PostToolUse"
            and value.get("tool_name") in codex.CODEX_TOOLS
        ):
            try:
                response = codex.internal_post_error_response(value, error)
            except Exception:
                response = {}
        try:
            sys.stderr.write(
                f"orchestrator hook internal error: {type(error).__name__}: {error}\n"
            )
        except Exception:
            pass
    rendered = json_transport.dumps(response, separators=(",", ":")) + "\n"
    stdout_buffer = getattr(sys.stdout, "buffer", None)
    if stdout_buffer is None:
        sys.stdout.write(rendered)
        sys.stdout.flush()
    else:
        stdout_buffer.write(rendered.encode("utf-8"))
        stdout_buffer.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
