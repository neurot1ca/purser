"""Subprocess runner shared by hook contract tests."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys


PACKAGE_ROOT = Path(__file__).resolve().parents[1]
HOOK_ENTRYPOINT = PACKAGE_ROOT / "context_io_hook.py"
ENFORCEMENT_ENTRYPOINT = PACKAGE_ROOT / "hook.py"


def run_contract_hook(event, payload, state, *, entrypoint="context"):
    """Invoke the Python entrypoint unless an event-specific runner replaces it."""
    enforcement_event = entrypoint == "enforcement" and event in {
        "PreToolUse", "PostToolUse", "PostToolUseFailure",
    }
    event_command = os.environ.get(f"PURSER_HOOK_CMD_{event.upper()}")
    command = event_command if event_command is not None else os.environ.get("PURSER_HOOK_CMD")
    if command is not None:
        argv = shlex.split(command, posix=os.name != "nt")
        if not argv:
            raise ValueError("PURSER_HOOK_CMD override must not be empty")
    else:
        target = ENFORCEMENT_ENTRYPOINT if entrypoint == "enforcement" else HOOK_ENTRYPOINT
        argv = [sys.executable, str(target)]
    if enforcement_event:
        argv.append("enforce")
    env = os.environ.copy()
    env["PURSER_STATE_DIR"] = str(state)
    return subprocess.run(
        argv,
        input=payload if isinstance(payload, bytes) else json.dumps(payload).encode("utf-8"),
        capture_output=True,
        cwd=(payload.get("cwd", Path(__file__).parents[4])
             if isinstance(payload, dict) else Path(__file__).parents[4]),
        env=env,
        timeout=15,
        check=False,
    )
