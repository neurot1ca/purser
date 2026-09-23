"""Standalone, deadline-bounded execution of project-declared checks."""

from __future__ import annotations

import os
from pathlib import Path
import shutil
import signal
import subprocess
import time
from collections.abc import Iterable

from project_profile import Check, is_protected


def select_checks(checks: Iterable[Check], changed_paths: Iterable[str]) -> tuple[Check, ...]:
    """Select checks using the profile's protected-path glob semantics."""
    paths = tuple(changed_paths)
    return tuple(check for check in checks if check.when is None or any(
        is_protected(path, check.when) for path in paths
    ))


def _status(repo_root: Path) -> dict[str, str] | None:
    try:
        completed = subprocess.run(
            ["git", "status", "--porcelain=v1", "-z", "--untracked-files=all"],
            cwd=repo_root, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            timeout=10, check=False,
        )
        if completed.returncode != 0:
            return None
    except (OSError, subprocess.TimeoutExpired):
        return None
    records = completed.stdout.decode("utf-8", errors="replace").split("\0")
    statuses: dict[str, str] = {}
    index = 0
    while index < len(records):
        record = records[index]
        if len(record) >= 4 and record[2] == " ":
            statuses[record[3:]] = record[:2]
            if "R" in record[:2] or "C" in record[:2]:
                index += 1  # The next NUL field is the original path.
        index += 1
    return statuses


def _stop_tree(process: subprocess.Popen[bytes]) -> None:
    if os.name == "nt":
        descendants = _windows_descendants(process.pid)
        try:
            subprocess.run(
                ["taskkill", "/T", "/F", "/PID", str(process.pid)],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                timeout=1, check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            pass
        _windows_terminate(descendants)
    else:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if process.poll() is None:
        process.kill()
    try:
        process.communicate(timeout=0.2)
    except subprocess.TimeoutExpired:
        pass


def _windows_descendants(parent_pid: int) -> list[int]:
    """Snapshot descendants before taskkill can orphan them (Windows only)."""
    import ctypes
    from ctypes import wintypes

    class ProcessEntry(ctypes.Structure):
        _fields_ = [
            ("dwSize", wintypes.DWORD), ("cntUsage", wintypes.DWORD),
            ("th32ProcessID", wintypes.DWORD), ("th32DefaultHeapID", ctypes.c_void_p),
            ("th32ModuleID", wintypes.DWORD), ("cntThreads", wintypes.DWORD),
            ("th32ParentProcessID", wintypes.DWORD), ("pcPriClassBase", wintypes.LONG),
            ("dwFlags", wintypes.DWORD), ("szExeFile", wintypes.WCHAR * 260),
        ]

    kernel = ctypes.windll.kernel32
    kernel.CreateToolhelp32Snapshot.restype = ctypes.c_void_p
    snapshot = kernel.CreateToolhelp32Snapshot(2, 0)
    if snapshot == ctypes.c_void_p(-1).value:
        return []
    parents: dict[int, int] = {}
    entry = ProcessEntry()
    entry.dwSize = ctypes.sizeof(entry)
    try:
        while kernel.Process32FirstW(snapshot, ctypes.byref(entry)):
            parents[entry.th32ProcessID] = entry.th32ParentProcessID
            while kernel.Process32NextW(snapshot, ctypes.byref(entry)):
                parents[entry.th32ProcessID] = entry.th32ParentProcessID
            break
    finally:
        kernel.CloseHandle(ctypes.c_void_p(snapshot))
    descendants: list[int] = []
    frontier = [parent_pid]
    while frontier:
        children = [pid for pid, owner in parents.items() if owner in frontier and pid not in descendants]
        descendants.extend(children)
        frontier = children
    return descendants


def _windows_terminate(pids: list[int]) -> None:
    import ctypes

    kernel = ctypes.windll.kernel32
    kernel.OpenProcess.restype = ctypes.c_void_p
    for pid in reversed(pids):
        handle = kernel.OpenProcess(1, False, pid)
        if handle:
            kernel.TerminateProcess(ctypes.c_void_p(handle), 1)
            kernel.CloseHandle(ctypes.c_void_p(handle))


def _run_one(repo_root: Path, check: Check, remaining: float) -> dict[str, object]:
    started = time.monotonic()
    executable = shutil.which(check.argv[0])
    if executable is None:
        return {"name": check.name, "status": "UNKNOWN", "exit_code": None,
                "duration_ms": 0, "tail": f"unresolvable executable: {check.argv[0]}"}
    command = [executable, *check.argv[1:]]
    creation = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
    try:
        process = subprocess.Popen(
            command, cwd=repo_root, env={**os.environ, "CI": "1"},
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, **creation,
        )
        try:
            output, _ = process.communicate(timeout=min(check.timeout_seconds, remaining))
            code = process.returncode
            status = "PASS" if code == 0 else "FAIL"
            tail = "" if code == 0 else output.decode("utf-8", errors="replace")[-1500:]
        except subprocess.TimeoutExpired as exc:
            _stop_tree(process)
            code = None
            status = "UNKNOWN"
            tail = ((exc.output or b"").decode("utf-8", errors="replace") + "\ntimed out")[-1500:]
    except OSError as exc:
        code = None
        status = "UNKNOWN"
        tail = f"launch error: {exc}"[-1500:]
    return {"name": check.name, "status": status, "exit_code": code,
            "duration_ms": int((time.monotonic() - started) * 1000), "tail": tail}


def run_checks(repo_root: Path, checks: Iterable[Check], *, deadline_seconds: float) -> dict[str, object]:
    """Run selected checks in order and report bounded results and git side effects."""
    if not isinstance(repo_root, (str, os.PathLike)) or not isinstance(deadline_seconds, (int, float)):
        raise TypeError("repo_root must be a path and deadline_seconds must be numeric")
    selected = tuple(checks)
    if not all(isinstance(check, Check) for check in selected):
        raise TypeError("checks must contain Check objects")
    if not selected:
        return {"status": "NOT_RUN", "results": [], "side_effect_paths": []}
    root = Path(repo_root)
    before = _status(root)
    deadline = time.monotonic() + max(0, deadline_seconds)
    results: list[dict[str, object]] = []
    for check in selected:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            results.append({"name": check.name, "status": "UNKNOWN", "exit_code": None,
                            "duration_ms": 0, "tail": "not run: deadline"})
        else:
            results.append(_run_one(root, check, remaining))
    after = _status(root)
    side_effect_paths = None if before is None or after is None else sorted(
        path for path in before.keys() | after.keys() if before.get(path) != after.get(path)
    )[:50]
    statuses = {result["status"] for result in results}
    status = "FAIL" if "FAIL" in statuses else "UNKNOWN" if "UNKNOWN" in statuses else "PASS"
    return {"status": status, "results": results, "side_effect_paths": side_effect_paths}


__all__ = ["select_checks", "run_checks"]
