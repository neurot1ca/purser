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
    command = os.environ.get(f"PURSER_HOOK_CMD_{event.upper()}") or os.environ.get("PURSER_HOOK_CMD")
    if command:
        argv = shlex.split(command, posix=os.name != "nt")
    else:
        target = ENFORCEMENT_ENTRYPOINT if entrypoint == "enforcement" else HOOK_ENTRYPOINT
        argv = [sys.executable, str(target)]
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
