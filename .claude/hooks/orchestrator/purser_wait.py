"""Block for one Codex async job's authenticated completion signal."""

from __future__ import annotations

import json
import math
from pathlib import Path
import socket
import sys
import time
from typing import Any

import state_root as purser_state_root


TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS"}


def _job_record(job_id: str) -> dict[str, Any] | None:
    if not job_id or len(job_id) > 128 or not job_id.isascii() or not job_id.isalnum():
        return None
    path = purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return value if isinstance(value, dict) and value.get("jobId") == job_id else None


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    job_id = args[0] if args else "invalid"
    if len(args) != 1:
        print(f"purser-wait {job_id} INVALID_ARGUMENT")
        return 2

    job = _job_record(job_id)
    if job is None:
        print(f"purser-wait {job_id} UNKNOWN_JOB")
        return 2
    state = job.get("state")
    if state in TERMINAL:
        print(f"purser-wait {job_id} {state}")
        return 0

    port, token, deadline = job.get("signalPort"), job.get("signalToken"), job.get("deadlineAt")
    if (isinstance(port, bool) or not isinstance(port, int) or not 1 <= port <= 65535
            or not isinstance(token, str) or not token.isascii() or not token
            or isinstance(deadline, bool) or not isinstance(deadline, (int, float))
            or not math.isfinite(deadline)):
        print(f"purser-wait {job_id} INVALID_JOB")
        return 2

    remaining = max(0.0, float(deadline) - time.time() + 60.0)
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=min(5.0, max(0.1, remaining))) as connection:
            connection.sendall(f"{job_id}:{token}".encode("ascii"))
            connection.settimeout(remaining)
            signal = connection.recv(1)
    except socket.timeout:
        print(f"purser-wait {job_id} TIMEOUT")
        return 4
    except (OSError, UnicodeError):
        print(f"purser-wait {job_id} BRIDGE_GONE")
        return 3

    if signal == b"1":
        print(f"purser-wait {job_id} TERMINAL")
        return 0
    print(f"purser-wait {job_id} BRIDGE_GONE")
    return 3


if __name__ == "__main__":
    raise SystemExit(main())
