"""Single-object stdin/stdout entry point for orchestrator hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
import sys
from typing import Any

import codex


def dispatch(event: Mapping[str, Any]) -> dict[str, Any]:
    if event.get("tool_name") not in codex.CODEX_TOOLS:
        return {}
    handler = {
        "PreToolUse": codex.pre_tool_use,
        "PostToolUse": codex.post_tool_use,
        "PostToolUseFailure": codex.post_tool_use_failure,
    }.get(event.get("hook_event_name"))
    return handler(event) if handler is not None else {}


def main() -> int:
    response: dict[str, Any] = {}
    value: Any = None
    try:
        value = json.load(sys.stdin)
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
    json.dump(response, sys.stdout, ensure_ascii=False, separators=(",", ":"))
    sys.stdout.write("\n")
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
