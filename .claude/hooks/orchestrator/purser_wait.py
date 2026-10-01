"""Block for one Codex async job's authenticated completion signal."""

from __future__ import annotations

import json
import math
from pathlib import Path
import socket
import sys
import time
from typing import Any

import codex_exec_mcp
import state_root as purser_state_root
import stall_followup


TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS"}


def _job_record(job_id: str) -> dict[str, Any] | None:
    if not job_id or len(job_id) > 128 or not job_id.isascii() or not job_id.isalnum():
        return None
    path = purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    try:
        text = purser_state_root.retry_permission_error(lambda: path.read_text(encoding="utf-8"))
        value = json.loads(text)
    except (OSError, ValueError):
        return None
    return value if isinstance(value, dict) and value.get("jobId") == job_id else None


def _record_terminal_observation(job_id: str, state: str) -> None:
    job_dir = purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs"
    with codex_exec_mcp._ownership_lock(job_dir, job_id):
        current = _job_record(job_id)
        if current is None or current.get("state") != state:
            return
        current["waiterObservedTerminalAt"] = time.time()
        current["waiterObservedTerminalState"] = state
        codex_exec_mcp._atomic_json(job_dir / f"{job_id}.json", current)


def _surface_stall(job_id: str, job: dict[str, Any]) -> bool:
    """Atomically claim and report the one waiter return for this silence episode."""
    stall_detected_at = job.get("stallDetectedAt")
    if (job.get("state") != "RUNNING" or job.get("errorCode") != "STALLED"
            or isinstance(stall_detected_at, bool)
            or not isinstance(stall_detected_at, (int, float))):
        return False
    job_dir = purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs"
    try:
        with codex_exec_mcp._ownership_lock(job_dir, job_id):
            current = _job_record(job_id)
            if (current is None or current.get("state") != "RUNNING"
                    or current.get("errorCode") != "STALLED"
                    or current.get("stallDetectedAt") != stall_detected_at
                    or current.get("waiterObservedStallAt") == stall_detected_at):
                return False
            current["waiterObservedStallAt"] = stall_detected_at
            codex_exec_mcp._atomic_json(job_dir / f"{job_id}.json", current)
    except BaseException:
        return False
    print(f"purser-wait {job_id} STALLED (worker still running)")
    liveness = current.get("liveness")
    last_activity = liveness.get("lastActivityAt") if isinstance(liveness, dict) else None
    print(f"{stall_followup.instruction(last_activity)} Do not show the jobId to the user.")
    return True


def _report_record_state(job_id: str, job: dict[str, Any] | None = None) -> int:
    """Report a durable terminal state or an expired/unreadable job."""
    job = _job_record(job_id) if job is None else job
    if job is None:
        print(f"purser-wait {job_id} UNKNOWN_JOB")
        return 3
    state = job.get("state")
    if not isinstance(state, str) or not state:
        print(f"purser-wait {job_id} INVALID_JOB")
        return 3
    if state in TERMINAL:
        try:
            _record_terminal_observation(job_id, state)
        except BaseException:
            pass
    print(f"purser-wait {job_id} {state}")
    return 0 if state == "SUCCEEDED" else 1 if state in TERMINAL else 3


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
    while True:
        job = _job_record(job_id)
        if job is None:
            return _report_record_state(job_id)
        state = job.get("state")
        if state in TERMINAL:
            return _report_record_state(job_id, job)
        if _surface_stall(job_id, job):
            return 4
        port, token, deadline = job.get("signalPort"), job.get("signalToken"), job.get("deadlineAt")
        if (isinstance(port, bool) or (port is not None and
                (not isinstance(port, int) or not 1 <= port <= 65535))
                or not isinstance(token, str) or not token.isascii() or not token
                or isinstance(deadline, bool) or not isinstance(deadline, (int, float))
                or not math.isfinite(deadline)):
            print(f"purser-wait {job_id} INVALID_JOB")
            return 2
        remaining = float(deadline) - time.time()
        if remaining <= 0:
            return _report_record_state(job_id)
        if port is not None:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=min(1.0, remaining)) as connection:
                    connection.sendall(f"{job_id}:{token}".encode("ascii"))
                    endpoint = (job.get("generation"), port, token, deadline)
                    while True:
                        connection.settimeout(min(1.0, max(0.01, float(deadline) - time.time())))
                        try:
                            signal = connection.recv(1)
                        except socket.timeout:
                            signal = None
                        current = _job_record(job_id)
                        if current is None or current.get("state") in TERMINAL:
                            return _report_record_state(job_id, current)
                        if _surface_stall(job_id, current):
                            return 4
                        if signal is not None or (current.get("generation"), current.get("signalPort"),
                                                   current.get("signalToken"), current.get("deadlineAt")) != endpoint:
                            break
                        if time.time() >= float(deadline):
                            break
            except (OSError, UnicodeError):
                pass
        time.sleep(min(0.1, max(0.0, float(deadline) - time.time())))


if __name__ == "__main__":
    raise SystemExit(main())
