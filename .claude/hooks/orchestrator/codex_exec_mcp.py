"""Small stdio MCP bridge for ``codex exec`` (Python 3.11, stdlib only).

For test harnesses, PURSER_CODEX_BIN may be a JSON argv prefix such as
``["C:/Python/python.exe", "C:/path/fake_codex.py"]``. A plain value is a
single executable path. This override is never parsed through a shell.

Each async job has a detached host with a token-checked loopback listener. The
host writes terminal output and state before notifying connected waiters.
"""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import ctypes
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any
import uuid

ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS = 24 * 60 * 60
STALL_COLD_START_FLOOR_SECONDS = 15 * 60
STALL_COLD_START_CEILING_SECONDS = 30 * 60

if os.name == "nt":
    import msvcrt
else:
    import fcntl

if os.name == "nt":
    from ctypes import wintypes

import state_root as purser_state_root
import coder_config
import model_listing
import project_root


def _record_claude_decision(job: dict[str, Any], decision: str, kind: str, source_job_id: str,
                            reviewer1_valid: Any = None, reviewer1_false: Any = None) -> None:
    task_id = job.get("taskId")
    if not isinstance(task_id, str) or not task_id:
        return
    phase = job.get("jobPhase", job.get("phase"))
    task_type = phase if phase in {"implementation", "operation", "plan"} else "unknown"
    try:
        import telemetry
        repo = job.get("repoRoot")
        key = project_root.project_key(Path(repo).resolve()) if isinstance(repo, str) else None
        payload = {"decision": decision, "task_type": task_type,
                   "source": {"kind": kind, "job_id": source_job_id}}
        for field, value in (("reviewer1_valid_count", reviewer1_valid),
                             ("reviewer1_false_count", reviewer1_false)):
            if isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                payload[field] = value
        telemetry.emit("CLAUDE_DECISION_RECORDED", task_id=task_id, project_key=key, **payload)
    except BaseException:
        pass


def _record_scope_conflict_outcome(job: dict[str, Any], action: str) -> None:
    job_id = job.get("jobId")
    if not isinstance(job_id, str):
        return
    try:
        import ledger
        import telemetry
        root = purser_state_root.default_state_root()
        task_id = job.get("taskId")
        repo = job.get("repoRoot")
        key = project_root.project_key(Path(repo).resolve()) if isinstance(repo, str) else None
        queue_wait_ms = None
        try:
            index = json.loads((root / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
            if (isinstance(index, dict) and index.get("jobId") == job_id
                    and index.get("project_key") == key and isinstance(index.get("call_key"), str)):
                queue_wait_ms = ledger.ScopeLeases(root / "projects" / key).queue_wait_ms(index["call_key"])
        except (OSError, ValueError, TypeError, KeyError):
            pass
        telemetry.emit("SCOPE_CONFLICT_OUTCOME", task_id=task_id, project_key=key,
                       state_root=root, action=action, job_id=job_id,
                       queue_wait_ms=queue_wait_ms)
    except BaseException:
        pass


def _unapplied_thread_job(job_dir: Path, thread_id: str) -> dict[str, Any] | None:
    latest = None
    try:
        for path in sorted(job_dir.glob("*.json")):
            try:
                job = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                continue
            if (isinstance(job, dict) and job.get("threadId") == thread_id
                    and job.get("state") == "SUCCEEDED" and job.get("applyState") != "applied"
                    and job.get("worktreePath")):
                timestamp = job.get("finishedAt")
                if timestamp is None:
                    timestamp = job.get("acceptedAt", 0)
                latest_timestamp = latest.get("finishedAt") if latest is not None else None
                if latest_timestamp is None and latest is not None:
                    latest_timestamp = latest.get("acceptedAt", 0)
                if latest is None or timestamp > latest_timestamp:
                    latest = job
    except OSError:
        pass
    return latest


def _same_worktree_content(worktree: Path, repo_root: Path, path: str) -> bool:
    worktree_file = worktree / path
    repo_file = repo_root / path
    if worktree_file.is_symlink() or repo_file.is_symlink():
        return (worktree_file.is_symlink() and repo_file.is_symlink()
                and os.readlink(worktree_file) == os.readlink(repo_file))
    if not worktree_file.exists() or not repo_file.exists():
        return not worktree_file.exists() and not repo_file.exists()
    return (worktree_file.is_file() and repo_file.is_file()
            and worktree_file.read_bytes() == repo_file.read_bytes())


TOOL_SCHEMAS = [
    {
        "name": "codex",
        "description": "Run a new Codex thread.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "prompt": {"type": "string"},
                "label": {"type": "string", "description": "Short human task description in the user's language (up to 80 characters)."},
                "model": {"type": "string"},
                "cwd": {"type": "string"},
                "config": {"type": "object"},
                "developer-instructions": {"type": "string"},
                "base-instructions": {"type": "string"},
                "compact-prompt": {"type": "string"},
                "sandbox": {"type": "string", "enum": ["read-only", "workspace-write", "danger-full-access"]},
                "approval-policy": {"type": "string", "enum": ["on-request", "never"]},
            },
            "required": ["prompt"],
        },
    },
    {
        "name": "codex-reply",
        "description": "Continue a thread created by this server.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "threadId": {"type": "string"},
                "conversationId": {"type": "string", "description": "Deprecated alias for threadId."},
                "prompt": {"type": "string"},
                "label": {"type": "string", "description": "Short human task description in the user's language (up to 80 characters); may be omitted to reuse the thread label."},
            },
            "required": ["prompt"],
        },
    },
]

TOOL_SCHEMAS += [
    {"name": "codex_start", "description": "Start a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {**TOOL_SCHEMAS[0]["inputSchema"]["properties"],
            "call": {"type": "string"}, "mode": {"type": "string", "enum": ["new", "reply"]},
            "worktree": {"type": "boolean"},
            "threadId": {"type": "string"}, "timeoutSeconds": {"type": "number"},
            "taskId": {"type": "string"}, "route": {"type": "string"},
            "etaMinutes": {"type": "integer"}, "jobModel": {"type": "string"},
            "jobEffort": {"type": "string"}, "jobLabel": {"type": "string"},
            "jobPhase": {"type": "string"}, "readinessCriteria": {"type": "object"},
            "readinessGrounded": {"type": "boolean"}, "readinessAgent": {"type": "string"},
            "readinessModel": {"type": "string"}},
        "required": ["prompt"]}},
    {"name": "codex_result", "description": "Read a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"},
            "adopt": {"type": "boolean", "description": "After user consent, finalize a job from an earlier session in this project."}},
        "required": ["jobId"]}},
    {"name": "codex_cancel", "description": "Cancel a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"}, "reason": {"type": "string"}}, "required": ["jobId"]}},
    {"name": "codex_apply", "description": "Apply a job worktree patch.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"},
            "resolved": {"type": "boolean", "description": "Record a hand-resolved conflict after verifying the repository."},
            "reviewer1Valid": {"type": "integer", "minimum": 0},
            "reviewer1False": {"type": "integer", "minimum": 0}},
        "required": ["jobId"]}},
]

TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS"}
CONTINUATION_PROMPT = ("The previous execution was interrupted. Inspect the working tree and the task "
                       "in this thread, avoid repeating completed steps, finish the remaining work, "
                       "run the required focused validation, and report the final result.")
RETENTION_SECONDS = 7 * 24 * 60 * 60
# Allow codex exec to exit normally after its terminal stdout event before stopping a hung tree.
CODEX_TERMINAL_GRACE_SECONDS = 20
RESPONSIVENESS_ATTEMPTS = (0, 30, 60, 120, 240)
WINDOWS_CREATE_NEW_PROCESS_GROUP = 0x00000200
WINDOWS_CREATE_NO_WINDOW = 0x08000000
WINDOWS_DETACHED_PROCESS = 0x00000008


def _process_creation_options(*, windows: bool | None = None) -> dict[str, Any]:
    if windows is None:
        windows = os.name == "nt"
    if windows:
        return {"creationflags": WINDOWS_CREATE_NEW_PROCESS_GROUP | WINDOWS_CREATE_NO_WINDOW}
    return {"start_new_session": True}


def _launch_host(job_dir: Path, job_id: str, generation: int = 1,
                 ownership_token: str = "") -> None:
    command = [sys.executable, str(Path(__file__).resolve()), "--host-job", job_id,
               "--host-job-dir", str(job_dir), "--generation", str(generation),
               "--ownership-token", ownership_token]
    if os.name != "nt":
        subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL, creationflags=0,
                         start_new_session=True, close_fds=True)
        return
    # A detached process group alone still has the bridge as its recorded
    # parent, and taskkill /T can follow that ancestry. Give the host the
    # desktop shell as its recorded parent via STARTUPINFOEX. Fail closed if
    # there is no accessible shell or job breakaway is denied.
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    user = ctypes.WinDLL("user32", use_last_error=True)
    user.GetShellWindow.restype = wintypes.HWND
    user.GetWindowThreadProcessId.argtypes = [wintypes.HWND, ctypes.POINTER(wintypes.DWORD)]
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    kernel.InitializeProcThreadAttributeList.argtypes = [ctypes.c_void_p, wintypes.DWORD,
                                                          wintypes.DWORD, ctypes.POINTER(ctypes.c_size_t)]
    kernel.UpdateProcThreadAttribute.argtypes = [ctypes.c_void_p, wintypes.DWORD, ctypes.c_size_t,
                                                  ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p,
                                                  ctypes.c_void_p]
    kernel.DeleteProcThreadAttributeList.argtypes = [ctypes.c_void_p]
    shell_window = user.GetShellWindow()
    shell_pid = wintypes.DWORD()
    user.GetWindowThreadProcessId(wintypes.HWND(shell_window), ctypes.byref(shell_pid))
    if not shell_pid.value and os.environ.get("PURSER_CODEX_TEST_PARENT_PID"):
        # Headless test harnesses have no Explorer shell. The harness process
        # is outside the bridge tree and exercises the same parent attribute.
        shell_pid.value = int(os.environ["PURSER_CODEX_TEST_PARENT_PID"])
    if not shell_pid.value:
        raise OSError("desktop shell parent unavailable")
    kernel.OpenProcess.restype = wintypes.HANDLE
    parent = kernel.OpenProcess(0x0080, False, shell_pid.value)  # PROCESS_CREATE_PROCESS
    if not parent:
        raise ctypes.WinError(ctypes.get_last_error())

    class STARTUPINFO(ctypes.Structure):
        _fields_ = [("cb", wintypes.DWORD), ("lpReserved", wintypes.LPWSTR),
                    ("lpDesktop", wintypes.LPWSTR), ("lpTitle", wintypes.LPWSTR),
                    ("dwX", wintypes.DWORD), ("dwY", wintypes.DWORD),
                    ("dwXSize", wintypes.DWORD), ("dwYSize", wintypes.DWORD),
                    ("dwXCountChars", wintypes.DWORD), ("dwYCountChars", wintypes.DWORD),
                    ("dwFillAttribute", wintypes.DWORD), ("dwFlags", wintypes.DWORD),
                    ("wShowWindow", wintypes.WORD), ("cbReserved2", wintypes.WORD),
                    ("lpReserved2", ctypes.c_void_p), ("hStdInput", wintypes.HANDLE),
                    ("hStdOutput", wintypes.HANDLE), ("hStdError", wintypes.HANDLE)]

    class STARTUPINFOEX(ctypes.Structure):
        _fields_ = [("StartupInfo", STARTUPINFO), ("lpAttributeList", ctypes.c_void_p)]

    class PROCESS_INFORMATION(ctypes.Structure):
        _fields_ = [("hProcess", wintypes.HANDLE), ("hThread", wintypes.HANDLE),
                    ("dwProcessId", wintypes.DWORD), ("dwThreadId", wintypes.DWORD)]

    size = ctypes.c_size_t()
    kernel.InitializeProcThreadAttributeList(None, 1, 0, ctypes.byref(size))
    attributes = ctypes.create_string_buffer(size.value)
    try:
        if not kernel.InitializeProcThreadAttributeList(attributes, 1, 0, ctypes.byref(size)):
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            parent_value = wintypes.HANDLE(parent)
            if not kernel.UpdateProcThreadAttribute(attributes, 0, 0x00020000,
                                                    ctypes.byref(parent_value), ctypes.sizeof(parent_value), None, None):
                raise ctypes.WinError(ctypes.get_last_error())
            startup = STARTUPINFOEX()
            startup.StartupInfo.cb = ctypes.sizeof(startup)
            startup.lpAttributeList = ctypes.cast(attributes, ctypes.c_void_p)
            info = PROCESS_INFORMATION()
            line = ctypes.create_unicode_buffer(subprocess.list2cmdline(command))
            flags = 0x00080000 | 0x01000000 | WINDOWS_CREATE_NEW_PROCESS_GROUP | WINDOWS_CREATE_NO_WINDOW
            kernel.CreateProcessW.argtypes = [wintypes.LPCWSTR, wintypes.LPWSTR, ctypes.c_void_p,
                                               ctypes.c_void_p, wintypes.BOOL, wintypes.DWORD,
                                               ctypes.c_void_p, wintypes.LPCWSTR, ctypes.c_void_p,
                                               ctypes.POINTER(PROCESS_INFORMATION)]
            if not kernel.CreateProcessW(sys.executable, line, None, None, False, flags,
                                         None, str(Path.cwd()), ctypes.byref(startup), ctypes.byref(info)):
                raise ctypes.WinError(ctypes.get_last_error())
            kernel.CloseHandle(info.hThread)
            kernel.CloseHandle(info.hProcess)
        finally:
            kernel.DeleteProcThreadAttributeList(attributes)
    finally:
        kernel.CloseHandle(parent)


def _contain_host_children() -> Any:
    if os.name != "nt":
        return None
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.CreateJobObjectW.argtypes = [ctypes.c_void_p, wintypes.LPCWSTR]
    kernel.SetInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p, wintypes.DWORD]
    kernel.AssignProcessToJobObject.argtypes = [wintypes.HANDLE, wintypes.HANDLE]
    kernel.GetCurrentProcess.restype = wintypes.HANDLE
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]

    class BASIC_LIMIT(ctypes.Structure):
        _fields_ = [("PerProcessUserTimeLimit", ctypes.c_int64), ("PerJobUserTimeLimit", ctypes.c_int64),
                    ("LimitFlags", wintypes.DWORD), ("MinimumWorkingSetSize", ctypes.c_size_t),
                    ("MaximumWorkingSetSize", ctypes.c_size_t), ("ActiveProcessLimit", wintypes.DWORD),
                    ("Affinity", ctypes.c_size_t), ("PriorityClass", wintypes.DWORD),
                    ("SchedulingClass", wintypes.DWORD)]

    class EXTENDED_LIMIT(ctypes.Structure):
        _fields_ = [("BasicLimitInformation", BASIC_LIMIT), ("IoInformation", ctypes.c_uint64 * 6),
                    ("ProcessMemoryLimit", ctypes.c_size_t), ("JobMemoryLimit", ctypes.c_size_t),
                    ("PeakProcessMemoryUsed", ctypes.c_size_t), ("PeakJobMemoryUsed", ctypes.c_size_t)]

    kernel.CreateJobObjectW.restype = wintypes.HANDLE
    handle = kernel.CreateJobObjectW(None, None)
    if not handle:
        raise ctypes.WinError(ctypes.get_last_error())
    try:
        limits = EXTENDED_LIMIT()
        limits.BasicLimitInformation.LimitFlags = 0x00002000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not kernel.SetInformationJobObject(handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            raise ctypes.WinError(ctypes.get_last_error())
        if not kernel.AssignProcessToJobObject(handle, kernel.GetCurrentProcess()):
            raise ctypes.WinError(ctypes.get_last_error())
    except BaseException:
        kernel.CloseHandle(handle)
        raise
    return handle  # Keep open until host exits; Codex descendants inherit the job.


def _job_result(value: dict[str, Any]) -> dict[str, Any]:
    return {"content": [{"type": "text", "text": json.dumps(value, ensure_ascii=False)}],
            "structuredContent": value, "isError": "errorCode" in value}


def _atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if os.name != "nt":
        os.chmod(path.parent, 0o700)
    temp = path.with_name("." + uuid.uuid4().hex + ".tmp")
    try:
        with open(temp, "x", encoding="utf-8") as stream:
            if os.name != "nt":
                os.chmod(temp, 0o600)
            json.dump(value, stream, ensure_ascii=False)
            stream.flush()
            os.fsync(stream.fileno())
        purser_state_root.retry_permission_error(lambda: os.replace(temp, path))
    finally:
        temp.unlink(missing_ok=True)


def _measure_silent_gap(activity: dict[str, Any], now: float) -> None:
    previous = activity.get("lastActivityAt")
    if isinstance(previous, (int, float)) and not isinstance(previous, bool):
        previous_max = activity.get("longestSilentGapSeconds", 0.0)
        if not isinstance(previous_max, (int, float)) or isinstance(previous_max, bool):
            previous_max = 0.0
        activity["longestSilentGapSeconds"] = max(
            previous_max, max(0.0, now - previous))


def _record_activity(activity: dict[str, Any], now: float) -> None:
    _measure_silent_gap(activity, now)
    activity["lastActivityAt"] = now


@contextmanager
def _ownership_lock(job_dir: Path, job_id: str):
    """Serialize generation claims across bridge and host processes."""
    job_dir.mkdir(parents=True, exist_ok=True)
    path = job_dir / (job_id + ".owner-lock")
    with open(path, "a+b") as stream:
        if os.name != "nt":
            os.chmod(path, 0o600)
            fcntl.flock(stream, fcntl.LOCK_EX)
        else:
            while True:
                try:
                    stream.seek(0)
                    msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
                    break
                except OSError:
                    time.sleep(0.02)
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == "nt":
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream, fcntl.LOCK_UN)


def _windows_process_identity(pid: int) -> tuple[str, int | None]:
    """Return process state and Windows creation FILETIME ticks."""
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.OpenProcess.restype = wintypes.HANDLE
    kernel.GetProcessTimes.argtypes = [wintypes.HANDLE, ctypes.c_void_p, ctypes.c_void_p,
                                       ctypes.c_void_p, ctypes.c_void_p]
    kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
    kernel.WaitForSingleObject.restype = wintypes.DWORD
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    handle = kernel.OpenProcess(0x00100000 | 0x00000400, False, pid)
    if not handle:
        return ("DEAD" if ctypes.get_last_error() == 87 else "UNCONFIRMED", None)
    try:
        created = ctypes.c_uint64()
        exited = ctypes.c_uint64()
        kernel_time = ctypes.c_uint64()
        user_time = ctypes.c_uint64()
        if not kernel.GetProcessTimes(handle, ctypes.byref(created), ctypes.byref(exited),
                                      ctypes.byref(kernel_time), ctypes.byref(user_time)):
            return "UNCONFIRMED", None
        wait = kernel.WaitForSingleObject(handle, 0)
        if wait == 0:
            return "DEAD", created.value
        if wait != 0x102:
            return "UNCONFIRMED", created.value
        return "ALIVE", created.value
    finally:
        kernel.CloseHandle(handle)


def _posix_process_identity(pid: int) -> tuple[str, int | None]:
    """Return Linux process state and /proc start ticks, or fail closed."""
    try:
        stat = Path(f"/proc/{pid}/stat").read_text(encoding="ascii")
        fields = stat[stat.rfind(")") + 2:].split()
        state = fields[0]
        created = int(fields[19])  # field 22: process start time
    except FileNotFoundError:
        return "DEAD", None
    except (OSError, ValueError, IndexError):
        return "UNCONFIRMED", None
    return ("DEAD" if state in {"Z", "X"} else "ALIVE"), created


def _posix_process_group_stopped(pid: int) -> bool:
    """Confirm a detached POSIX child session has no live processes."""
    try:
        entries = list(Path("/proc").iterdir())
    except OSError:
        return False
    for entry in entries:
        if not entry.name.isdigit():
            continue
        try:
            stat = (entry / "stat").read_text(encoding="ascii")
            fields = stat[stat.rfind(")") + 2:].split()
            if int(fields[2]) == pid and fields[0] not in {"Z", "X"}:  # pgrp, state
                return False
        except FileNotFoundError:
            continue
        except (OSError, ValueError, IndexError):
            return False
    return True


def _toml(value: Any) -> str:
    if isinstance(value, str):
        return '"' + value.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n").replace("\r", "\\r").replace("\t", "\\t") + '"'
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float) and math.isfinite(value):
        return repr(value)
    if isinstance(value, list):
        return "[" + ", ".join(_toml(item) for item in value) + "]"
    raise ValueError(f"unsupported config value: {type(value).__name__}")


def _flatten_config(config: dict[str, Any], prefix: str = "") -> list[str]:
    result = []
    for key, value in config.items():
        if not isinstance(key, str) or not key or any(part == "" for part in key.split(".")):
            raise ValueError("config keys must be nonempty strings")
        name = f"{prefix}.{key}" if prefix else key
        if isinstance(value, dict):
            result.extend(_flatten_config(value, name))
        else:
            result.append(f"{name}={_toml(value)}")
    return result


def _state_dir() -> Path:
    override = os.environ.get("PURSER_CODEX_SHIM_STATE")
    if override:
        return Path(override).expanduser().resolve()
    return purser_state_root.default_state_root() / "codex_exec_mcp" / "threads"


def _record_path(state_dir: Path, thread_id: str) -> Path:
    return state_dir / (hashlib.sha256(thread_id.encode("utf-8")).hexdigest() + ".json")


def _save_record(state_dir: Path, record: dict[str, Any]) -> None:
    state_dir.mkdir(parents=True, exist_ok=True)
    path = _record_path(state_dir, record["thread_id"])
    temp_name = str(state_dir / (".thread-" + uuid.uuid4().hex + ".json"))
    try:
        with open(temp_name, "x", encoding="utf-8") as stream:
            json.dump(record, stream, ensure_ascii=False)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp_name, path)
    finally:
        if os.path.exists(temp_name):
            os.unlink(temp_name)


def _load_record(state_dir: Path, thread_id: str) -> dict[str, Any] | None:
    try:
        record = json.loads(_record_path(state_dir, thread_id).read_text(encoding="utf-8"))
        if (record.get("thread_id") == thread_id and isinstance(record.get("cwd"), str)
                and isinstance(record.get("config_args"), list)
                and all(isinstance(arg, str) for arg in record["config_args"])):
            return record
    except (OSError, ValueError, AttributeError):
        pass
    return None


def _binary_prefix(argument: str | None) -> list[str]:
    raw = argument or os.environ.get("PURSER_CODEX_BIN")
    if raw:
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            parsed = raw
        if isinstance(parsed, list):
            if not parsed or not all(isinstance(item, str) and item for item in parsed):
                raise ValueError("PURSER_CODEX_BIN JSON list must contain nonempty strings")
            prefix = parsed
        elif isinstance(parsed, str) and parsed:
            prefix = [parsed]
        else:
            raise ValueError("invalid Codex binary override")
    else:
        resolved = shutil.which("codex")
        if not resolved:
            raise ValueError("Codex executable was not found")
        prefix = [resolved]
    executable = Path(prefix[0])
    # npm also installs an extensionless POSIX launcher on Windows. Prefer its
    # .cmd sibling, then bypass cmd.exe by invoking the JS entry directly.
    if os.name == "nt" and not executable.suffix and executable.with_suffix(".cmd").is_file():
        executable = executable.with_suffix(".cmd")
    if executable.suffix.lower() in (".cmd", ".bat"):
        entry = executable.parent / "node_modules" / "@openai" / "codex" / "bin" / "codex.js"
        node = shutil.which("node")
        if not entry.is_file():
            raise ValueError(f"Codex JS entry is missing beside launcher: {entry}")
        if not node:
            raise ValueError("node executable is required for the Codex JS entry")
        return [node, str(entry), *prefix[1:]]
    return prefix


def _tool_result(thread_id: str | None, *, content: str | None = None, error: str | None = None,
                 usage: dict[str, Any] | None = None) -> dict[str, Any]:
    value = {"threadId": thread_id, "error": error} if error is not None else {"threadId": thread_id, "content": content}
    if error is None and usage:
        value["usage"] = usage
    result: dict[str, Any] = {"content": [{"type": "text", "text": json.dumps(value, ensure_ascii=False)}], "isError": error is not None}
    if error is None:
        result["structuredContent"] = value
    return result


def _codex_turn_usage(events: list[dict[str, Any]]) -> dict[str, int | float]:
    fields = {"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens"}
    for event in reversed(events):
        usage = event.get("usage")
        if event.get("type") != "turn.completed" or not isinstance(usage, dict):
            continue
        return {
            key: value for key, value in usage.items()
            if key in fields and isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(value) and value >= 0
        }
    return {}


def _event_message(event: dict[str, Any]) -> str:
    parts = []
    for value in (event.get("message"), event.get("error")):
        if isinstance(value, str):
            parts.append(value)
        elif isinstance(value, dict):
            for key in ("message", "code"):
                if isinstance(value.get(key), str):
                    parts.append(value[key])
    return ": ".join(parts) if parts else json.dumps(event, ensure_ascii=False)


def _agent_command(agent: str, prefix: list[str], name: str, cwd: str,
                   model: str | None, config_args: list[str], sandbox: str | None,
                   thread_id: str | None, output_path: Path, prompt: str) -> list[str]:
    if agent == "agy":
        if not isinstance(model, str) or not coder_config.AGY_MODEL.fullmatch(model):
            raise ValueError("Antigravity model must be a Gemini model from agy models")
        if sandbox is not None or len(config_args) != 1 or not config_args[0].startswith("model_reasoning_effort="):
            raise ValueError("unsupported Antigravity sandbox or config")
        effort = json.loads(config_args[0].split("=", 1)[1])
        if not isinstance(effort, str) or effort not in coder_config.AGY_EFFORTS:
            raise ValueError("Antigravity effort must be low, medium, high or max")
        command = prefix + ["--print", prompt, "--output-format", "json", "--model", model,
                            "--mode", "accept-edits", "--dangerously-skip-permissions", "--effort", effort]
        if name == "codex-reply":
            command += ["--conversation", thread_id]
        else:
            command += ["--new-project", "--add-dir", cwd]
        return command
    if agent == "opencode":
        if not model or "/" not in model or model.startswith("/") or model.endswith("/"):
            raise ValueError("OpenCode model must be provider/model")
        if sandbox is not None:
            raise ValueError("OpenCode sandbox has no verified equivalent")
        variants = [arg.split("=", 1)[1] for arg in config_args
                    if arg.startswith("model_reasoning_effort=")]
        if len(variants) > 1 or len(variants) != len(config_args):
            raise ValueError("unsupported OpenCode config")
        command = prefix + ["run", "--format", "json", "--model", model]
        if variants:
            variant = json.loads(variants[0])
            if not isinstance(variant, str) or not variant:
                raise ValueError("OpenCode variant must be a nonempty string")
            command += ["--variant", variant]
        if name == "codex-reply":
            command += ["--session", thread_id]
        return command + ["--", prompt]
    command = prefix + ["exec"]
    if name == "codex-reply":
        command.append("resume")
    command += ["--json", "--skip-git-repo-check"]
    if model:
        command += ["-m", model]
    for arg in config_args:
        command += ["-c", arg]
    if name == "codex":
        if sandbox is not None:
            command += ["-s", sandbox]
        command += ["-C", cwd]
    command += ["-o", str(output_path)]
    if name == "codex-reply":
        command.append(thread_id)
    return command + ["-"]


def _agent_event(agent: str, event: dict[str, Any]) -> tuple[str | None, bool, str | None, str | None]:
    """Return session identity, terminal marker, failure, and final text."""
    kind = event.get("type")
    if agent == "agy":
        session = event.get("conversation_id")
        session = session if isinstance(session, str) and session else None
        status = event.get("status")
        success = isinstance(status, str) and status.casefold() == "success"
        response = event.get("response")
        failure = (None if success and isinstance(response, str) else
                   str(event.get("error") or status or "Antigravity completion missing"))
        return session, success, failure, response if isinstance(response, str) else None
    if agent == "opencode":
        session = event.get("sessionID")
        session = session if isinstance(session, str) and session else None
        part = event.get("part")
        failure = _event_message(event) if kind == "error" else None
        if kind == "tool_use" and isinstance(part, dict) and part.get("state", {}).get("status") == "error":
            failure = str(part["state"].get("error") or "OpenCode tool failed")
        final = part.get("text") if kind == "text" and isinstance(part, dict) else None
        stopped = kind == "step_finish" and isinstance(part, dict) and part.get("reason") == "stop"
        return session, stopped or bool(failure), failure, final if isinstance(final, str) else None
    session = event.get("thread_id") if kind == "thread.started" else None
    session = session if isinstance(session, str) and session else None
    failure = _event_message(event) if kind in ("error", "turn.failed") else None
    item = event.get("item")
    final = item.get("text") if kind == "item.completed" and isinstance(item, dict) and item.get("type") == "agent_message" else None
    return session, kind in ("turn.completed", "turn.failed", "error"), failure, final if isinstance(final, str) else None


def _retry_after_seconds(value: Any) -> float | None:
    if isinstance(value, dict):
        for key, item in value.items():
            if key.casefold().replace("_", "-") == "retry-after":
                try:
                    seconds = float(item)
                    if math.isfinite(seconds) and seconds >= 0:
                        return seconds
                except (TypeError, ValueError):
                    pass
            found = _retry_after_seconds(item)
            if found is not None:
                return found
    elif isinstance(value, list):
        for item in value:
            found = _retry_after_seconds(item)
            if found is not None:
                return found
    return None


def _launch_failure_evidence(agent: str, events: list[dict[str, Any]], *, stderr: str = "",
                             stdout_had_output: bool = False, output_had_data: bool = False,
                             process_started: bool = True, process_returncode: int | None = None) -> dict[str, Any]:
    """Classify failed launches; only empty, pre-work evidence is retryable."""
    if not process_started:
        return {"type": "AMBIGUOUS", "retryAfterSeconds": None,
                "noWorkProof": False, "proof": "PROCESS_NOT_STARTED"}
    body = stderr + "\n" + "\n".join(json.dumps(event, ensure_ascii=False) for event in events)
    retry_after = _retry_after_seconds(events)
    if retry_after is None:
        match = re.search(r"\bretry[- ]after\s*[:=]?\s*(\d+(?:\.\d+)?)\s*(?:seconds?|s)?\b", body, re.I)
        if match:
            retry_after = float(match.group(1))
    if agent == "codex":
        work_started = any(str(event.get("type", "")).startswith("item.") for event in events)
    elif agent == "opencode":
        work_started = _opencode_has_work(events)
    else:
        work_started = any(event.get("type") != "error" for event in events)

    # Assumed provider message variants: these text/status spellings are not
    # guaranteed by either JSON event protocol; keep the match narrow.
    if not work_started and re.search(r"\b(?:HTTP\s*)?402\b|enforced_spend_limit_reached|insufficient (?:account )?balance|quota[- ]exceeded|billing.{0,40}(?:quota|credit)|(?:quota|credit).{0,40}billing", body, re.I):
        return {"type": "CREDIT_EXHAUSTED", "retryAfterSeconds": None, "noWorkProof": False}
    if not work_started and re.search(r"\b429\b|rate[- ]limit(?:ed)?", body, re.I):
        return {"type": "RATE_LIMITED", "retryAfterSeconds": retry_after, "noWorkProof": False}

    if agent == "opencode" and not work_started:
        provider_error = any(
            event.get("type") == "error"
            and isinstance(event.get("error"), dict)
            and event["error"].get("name") in {"ProviderError", "ModelNotFoundError"}
            for event in events
        )
        if provider_error:
            return {"type": "PROVIDER_ERROR", "retryAfterSeconds": None, "noWorkProof": True}
        if process_returncode not in (None, 0):
            return {"type": "NONZERO_EXIT", "retryAfterSeconds": None, "noWorkProof": True}

    if not events and not stdout_had_output and not output_had_data and not stderr.strip():
        return {"type": "NO_RESPONSE", "retryAfterSeconds": None,
                "noWorkProof": True, "proof": "NO_EVENTS_OR_OUTPUT"}
    # Verified locally: the bridge parses Codex `turn.failed` / `error` and
    # OpenCode `error` envelopes. Their provider-specific billing text is assumed.
    return {"type": "AMBIGUOUS", "retryAfterSeconds": None, "noWorkProof": False}


def _run_with_responsiveness_retries(run_attempt, *, clock=time.monotonic,
                                     sleep=time.sleep) -> dict[str, Any]:
    """Run at the fixed offsets, repeating only a proven no-response launch."""
    started = clock()
    for index, offset in enumerate(RESPONSIVENESS_ATTEMPTS, 1):
        delay = max(0.0, started + offset - clock())
        if delay:
            sleep(delay)
        result = run_attempt()
        evidence = result.get("typedEvidence") if isinstance(result, dict) else None
        if not (isinstance(evidence, dict) and evidence.get("type") == "NO_RESPONSE"
                and evidence.get("noWorkProof") is True):
            if isinstance(result, dict):
                result["responsivenessAttempts"] = index
            return result
    result["responsivenessAttempts"] = len(RESPONSIVENESS_ATTEMPTS)
    return result


def probe_api_primary(agent: str, model: str, *, codex_bin: str | None = None,
                      timeout: float = 60.0) -> dict[str, Any]:
    """Probe the configured CLI in an empty temporary cwd, outside any worktree."""
    process_started = False
    temp = None
    try:
        if agent not in ("codex", "opencode"):
            raise ValueError("API probe supports Codex and OpenCode CLI agents")
        server = Server(codex_bin=codex_bin, agent=agent)
        temp = tempfile.mkdtemp(prefix="purser-api-probe-")
        cwd = Path(temp)
        output_path = cwd / "last-message.txt"
        sandbox = "read-only" if agent == "codex" else None
        command = _agent_command(agent, server._agent_prefix(agent), "codex", str(cwd),
                                 model, [], sandbox, None, output_path, "Reply with OK.")
        creation_options = _process_creation_options()
        process = subprocess.Popen(command, cwd=cwd,
                                   stdin=subprocess.PIPE if agent == "codex" else subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   creationflags=creation_options.pop("creationflags", 0),
                                   **creation_options)
        process_started = True
        timed_out = False
        try:
            stdout, stderr = process.communicate(
                input=b"Reply with OK." if agent == "codex" else None, timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            _stop_process(process)
            try:
                stdout, stderr = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                stdout, stderr = b"", b""
        events = []
        for line in stdout.splitlines():
            try:
                event = json.loads(line)
            except (ValueError, UnicodeError):
                continue
            if isinstance(event, dict):
                events.append(event)
        parsed = [_agent_event(agent, event) for event in events]
        failures = [failure for _, _, failure, _ in parsed if failure]
        if timed_out:
            return {"ok": False, "failure": {"type": "NO_RESPONSE", "retryAfterSeconds": None,
                                                "noWorkProof": False}}
        completed = any(event.get("type") == "step_finish" and isinstance(event.get("part"), dict)
                        and event["part"].get("reason") == "stop" for event in events) if agent == "opencode" else any(
                            event.get("type") == "turn.completed" for event in events)
        if process.returncode == 0 and completed and not failures:
            return {"ok": True}
        evidence = _launch_failure_evidence(
            agent, events, stderr=stderr.decode("utf-8", errors="replace"),
            stdout_had_output=bool(stdout.strip()),
            output_had_data=output_path.is_file() and output_path.stat().st_size > 0)
        return {"ok": False, "failure": evidence}
    except (OSError, ValueError, TypeError) as exc:
        evidence = _launch_failure_evidence(agent, [], stderr=str(exc), process_started=process_started)
        return {"ok": False, "failure": evidence}
    finally:
        if temp is not None:
            shutil.rmtree(temp, ignore_errors=True)


def _opencode_missing_model(event: dict[str, Any], model: str) -> bool:
    """Accept only a structured model-not-found error naming the requested model."""
    error = event.get("error")
    if event.get("type") != "error" or not isinstance(error, dict):
        return False
    data = error.get("data")
    name = error.get("name")
    if not isinstance(data, dict) or name not in {"ModelNotFoundError", "ProviderError"}:
        return False
    message = data.get("message")
    if not isinstance(message, str) or not re.search(r"\bmodel\b.{0,40}\bnot found\b", message, re.I):
        return False
    return data.get("modelID") == model or model in message


def _opencode_has_work(events: list[dict[str, Any]]) -> bool:
    for event in events:
        kind = event.get("type")
        part = event.get("part")
        tool = part.get("tool") if isinstance(part, dict) else None
        if (kind == "tool_use" or kind == "text"
                or (isinstance(kind, str) and "edit" in kind.casefold())
                or tool in {"edit", "write", "patch"}):
            return True
    return False


def _stop_process(process: subprocess.Popen[bytes]) -> bool:
    if process.poll() is not None:
        return False
    try:
        if os.name == "nt":
            stopped = subprocess.run(["taskkill", "/T", "/F", "/PID", str(process.pid)],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
                                     creationflags=WINDOWS_CREATE_NO_WINDOW)
            if stopped.returncode == 0:
                return True  # taskkill confirmed the complete Windows process tree.
            process.kill()
            return False  # Verify the host job object after terminating the root.
        else:
            os.killpg(process.pid, signal.SIGKILL)
            return False  # Verify the process group after sending the kill signal.
    except (OSError, ProcessLookupError) as exc:
        print(f"Codex cancellation failed: {exc}", file=sys.stderr, flush=True)
        return False


def _child_tree_stopped(process: subprocess.Popen[bytes], job_handle: Any) -> bool:
    if os.name == "nt":
        if not job_handle or process.poll() is None:
            return False
        kernel = ctypes.WinDLL("kernel32", use_last_error=True)

        class ACCOUNTING(ctypes.Structure):
            _fields_ = [("TotalUserTime", ctypes.c_int64), ("TotalKernelTime", ctypes.c_int64),
                        ("ThisPeriodTotalUserTime", ctypes.c_int64),
                        ("ThisPeriodTotalKernelTime", ctypes.c_int64),
                        ("TotalPageFaults", wintypes.DWORD), ("TotalProcesses", wintypes.DWORD),
                        ("ActiveProcesses", wintypes.DWORD), ("TotalTerminatedProcesses", wintypes.DWORD)]

        kernel.QueryInformationJobObject.argtypes = [wintypes.HANDLE, ctypes.c_int, ctypes.c_void_p,
                                                      wintypes.DWORD, ctypes.c_void_p]
        info = ACCOUNTING()
        until = time.monotonic() + 1.0
        while time.monotonic() < until:
            if not kernel.QueryInformationJobObject(job_handle, 1, ctypes.byref(info),
                                                    ctypes.sizeof(info), None):
                return False
            if info.ActiveProcesses <= 1:  # Only the detached host remains.
                return True
            time.sleep(0.02)
        return False
    try:
        if _posix_process_group_stopped(process.pid):
            return True
        os.killpg(process.pid, 0)
        return False
    except ProcessLookupError:
        return True
    except OSError:
        return False


class Server:
    def __init__(self, codex_bin: str | None = None, *, host: bool = False,
                 job_dir: Path | None = None, agent: str | None = None) -> None:
        self.codex_bin = codex_bin
        self._fixed_agent = agent is not None
        self.agent = agent or os.environ.get("PURSER_AGENT") or self._configured_agent()
        self.state_dir = _state_dir()
        self.write_lock = threading.Lock()
        self.jobs_lock = threading.Lock()
        self.jobs: dict[Any, subprocess.Popen[bytes] | None] = {}
        self.cancelled: set[Any] = set()
        self.job_dir = job_dir or purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs"
        self.job_processes: dict[str, subprocess.Popen[bytes]] = {}
        self.job_events: dict[str, threading.Event] = {}
        self.child_job_handle: Any = None
        self.claim_generation: int | None = None
        self.claim_token: str | None = None
        self.listener = None
        if not host:
            return
        try:
            self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            self.listener.bind(("127.0.0.1", 0))
            self.listener.listen()
            threading.Thread(target=self._signal_loop, daemon=True).start()
        except OSError as exc:
            print(f"Codex async signal unavailable: {exc}", file=sys.stderr, flush=True)
            if self.listener is not None:
                self.listener.close()
            self.listener = None

    @staticmethod
    def _configured_agent() -> str:
        config = coder_config.load(Path(__file__).with_name("config.json"))
        selected = config.get("coder_agent", "codex")
        if selected not in ("codex", "opencode", "agy"):
            raise ValueError("coder_agent must be codex, opencode or agy")
        return selected

    def _agent_prefix(self, agent: str | None = None) -> list[str]:
        agent = agent or self.agent
        if agent == "codex":
            return _binary_prefix(self.codex_bin)
        if agent not in ("opencode", "agy"):
            raise ValueError("unknown agent")
        if self.codex_bin:
            value = json.loads(self.codex_bin) if self.codex_bin.startswith("[") else self.codex_bin
            if isinstance(value, str) and value:
                return [value]
            if isinstance(value, list) and value and all(isinstance(item, str) and item for item in value):
                return value
            raise ValueError("invalid agent binary override")
        binary = coder_config.opencode_binary() if agent == "opencode" else coder_config.agy_binary()
        if not binary:
            raise ValueError(f"{agent} executable was not found")
        return [binary]

    def _signal_loop(self) -> None:
        while True:
            try:
                connection, _ = self.listener.accept()
                threading.Thread(target=self._signal_client, args=(connection,), daemon=True).start()
            except OSError:
                return

    def _signal_client(self, connection: socket.socket) -> None:
        with connection:
            try:
                connection.settimeout(5)
                identity = connection.recv(256).decode("ascii").strip().split(":")
                if len(identity) not in (2, 3):
                    return
                job = self._read_job(identity[0])
                if job is None or job.get("signalToken") != identity[1]:
                    return
                if len(identity) == 3:
                    if identity[2] == "CANCEL":
                        cancelled = self._cancel_job({"jobId": identity[0]})
                        connection.sendall(b"1" if cancelled.get("structuredContent", {}).get("state") == "CANCELLED" else b"0")
                    elif identity[2] == "PING":
                        connection.sendall(b"1")
                    return
                event = self.job_events.get(identity[0])
                if event is None:
                    return
                connection.settimeout(None)
                event.wait()
                connection.sendall(b"1")
            except (OSError, UnicodeError):
                return

    def _job_path(self, job_id: str) -> Path:
        return self.job_dir / (job_id + ".json")

    def _read_job(self, job_id: Any) -> dict[str, Any] | None:
        if not isinstance(job_id, str) or not job_id or any(ch not in "0123456789abcdef" for ch in job_id):
            return None
        try:
            path = self._job_path(job_id)
            text = purser_state_root.retry_permission_error(lambda: path.read_text(encoding="utf-8"))
            value = json.loads(text)
            return value if value.get("jobId") == job_id else None
        except (OSError, ValueError, AttributeError):
            return None

    def _write_job(self, job: dict[str, Any]) -> None:
        _atomic_json(self._job_path(job["jobId"]), job)

    def _check_readiness_before_success(self, job_id: str, result: dict[str, Any]) -> None:
        started_ns = time.perf_counter_ns()
        verdict: dict[str, Any] = {"verdict": "UNKNOWN", "criterion": None, "evidence": []}
        job = None
        agent = "unknown"
        try:
            job = self._read_job(job_id)
            readiness = job.get("readinessCheck") if job else None
            if not isinstance(readiness, dict) or readiness.get("grounded") is not True:
                return
            agent = readiness.get("agent")
            if not isinstance(agent, str):
                agent = "unknown"
            if agent == "off":
                verdict["verdict"] = "UNAVAILABLE"
            elif (isinstance(job.get("worktreePath"), str)
                  and isinstance(job.get("snapshot"), str)
                  and isinstance(readiness.get("objective"), str)
                  and isinstance(readiness.get("acceptance_criteria"), str)):
                import ledger
                import policy
                import readiness_check
                import reviewer

                checker_model = readiness.get("model", "")
                implementer_family = policy.vendor_family(policy.model_family(job.get("model")))
                checker_family = policy.vendor_family(policy.model_family(checker_model))
                if (implementer_family and checker_family
                        and implementer_family == checker_family):
                    verdict["verdict"] = "UNAVAILABLE"
                else:
                    worktree = Path(job["worktreePath"])
                    delta = ledger.compute_job_delta(worktree, job["snapshot"])
                    delta = ledger.filter_job_delta(worktree, delta)
                    if delta.get("status") == ledger.OK:
                        _paths, attributed_diff = reviewer._delegated_diff(worktree, delta)
                        criteria = {
                            "grounded": True,
                            "objective": readiness["objective"],
                            "acceptance_criteria": readiness["acceptance_criteria"],
                        }
                        packet, encoded = readiness_check.build_packet(
                            criteria, attributed_diff,
                            result.get("content", "") if isinstance(result.get("content"), str) else "",
                        )
                        loaded_config = coder_config.load(Path(__file__).with_name("config.json"))
                        verdict = readiness_check.run_checker(
                            packet, encoded, agent=agent, model=checker_model,
                            repo_root=job.get("repoRoot", worktree),
                            executable=loaded_config.get("reviewer1_executable"),
                        )
        except Exception:
            pass
        try:
            if job is not None:
                import telemetry
                import project_root

                repo = job.get("repoRoot")
                key = project_root.project_key(Path(repo).resolve()) if isinstance(repo, str) else None
                telemetry.emit(
                    "READINESS_CHECK_OUTCOME", task_id=job.get("taskId"), project_key=key,
                    state_root=purser_state_root.default_state_root(), verdict=verdict["verdict"],
                    agent=agent, duration_ms=max(0, (time.perf_counter_ns() - started_ns) // 1_000_000),
                )
        except BaseException:
            pass
        try:
            with self.jobs_lock:
                job = self._read_job(job_id)
                readiness = job.get("readinessCheck") if job else None
                if job is not None and job.get("state") == "RUNNING" and isinstance(readiness, dict):
                    readiness["result"] = verdict
                    readiness["checkedAt"] = time.time()
                    self._write_job(job)
        except Exception:
            pass

    def _checkpoint_liveness(self, job_id: str, generation: int, ownership_token: str,
                             activity: dict[str, Any], *, now: float,
                             process_alive: bool, force: bool = False) -> bool:
        silent_for = max(0.0, now - activity.get("lastActivityAt", now))
        threshold = activity.get("stallThresholdSeconds")
        should_stall = (isinstance(threshold, (int, float)) and not isinstance(threshold, bool)
                        and math.isfinite(threshold) and silent_for >= threshold)
        was_stalled = isinstance(activity.get("stallDetectedAt"), (int, float))
        transition = should_stall != was_stalled
        if not transition and not force and now - activity.get("checkpointAt", 0.0) < 60:
            return False
        with _ownership_lock(self.job_dir, job_id):
            job = self._read_job(job_id)
            if (job is None or job.get("state") != "RUNNING"
                    or job.get("generation") != generation
                    or job.get("ownershipToken") != ownership_token):
                return False
            activity["processAlive"] = process_alive
            activity["processAliveAt"] = now
            activity["checkpointAt"] = now
            if should_stall and not was_stalled:
                activity["stallDetectedAt"] = now
                job["stallDetectedAt"] = now
                job["errorCode"] = "STALLED"
            elif not should_stall and was_stalled:
                activity.pop("stallDetectedAt", None)
                job.pop("stallDetectedAt", None)
                if job.get("errorCode") == "STALLED":
                    job.pop("errorCode", None)
            job["activeAgent"] = activity["agent"]
            job["liveness"] = {key: value for key, value in activity.items()
                               if key != "checkpointAt"}
            self._write_job(job)
            return True

    def _stall_threshold(self, job: dict[str, Any]) -> int:
        route, agent = job.get("route"), job.get("activeAgent")
        live = job.get("liveness", {})
        if not isinstance(live, dict):
            live = {}
        if not isinstance(agent, str):
            agent = live.get("agent")
        samples = []
        try:
            if isinstance(route, str) and isinstance(agent, str):
                sample_paths = self.job_dir.glob("*.json")
            else:
                sample_paths = ()
            for path in sample_paths:
                try:
                    record = json.loads(path.read_text(encoding="utf-8"))
                except (OSError, ValueError, TypeError):
                    continue
                if not isinstance(record, dict):
                    continue
                live = record.get("liveness", {})
                if not isinstance(live, dict):
                    live = {}
                gap = live.get("longestSilentGapSeconds")
                record_agent = record.get("activeAgent", live.get("agent"))
                if (record.get("state") == "SUCCEEDED" and record.get("route") == route
                        and record_agent == agent and isinstance(gap, (int, float))
                        and not isinstance(gap, bool) and math.isfinite(gap) and gap >= 0):
                    samples.append(float(gap))
        except OSError:
            pass
        if len(samples) >= 5:
            ordered = sorted(samples)
            p90 = ordered[math.ceil(0.9 * len(ordered)) - 1]
            return max(STALL_COLD_START_FLOOR_SECONDS, math.ceil(p90 * 1.5))
        if agent == "agy":
            return STALL_COLD_START_CEILING_SECONDS
        eta = job.get("etaMinutes")
        eta_seconds = (float(eta) * 60 if isinstance(eta, (int, float))
                       and not isinstance(eta, bool) and math.isfinite(eta) and eta > 0 else
                       STALL_COLD_START_FLOOR_SECONDS)
        return max(STALL_COLD_START_FLOOR_SECONDS,
                   min(STALL_COLD_START_CEILING_SECONDS, math.ceil(eta_seconds * 2)))

    def _host_ping(self, job: dict[str, Any]) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", job["signalPort"]), timeout=0.5) as connection:
                connection.settimeout(0.5)
                connection.sendall(f'{job["jobId"]}:{job["signalToken"]}:PING'.encode("ascii"))
                return connection.recv(1) == b"1"
        except (OSError, KeyError, TypeError, ValueError):
            return False

    def _classify_host(self, job: dict[str, Any]) -> str:
        pid = job.get("hostPid")
        created = job.get("hostCreatedAt")
        if (not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0
                or not isinstance(created, int) or isinstance(created, bool)):
            return "UNCONFIRMED"
        if os.name == "nt":
            if not job.get("hostContained"):
                return "UNCONFIRMED"
            state, actual_created = _windows_process_identity(pid)
        elif sys.platform.startswith("linux"):
            state, actual_created = _posix_process_identity(pid)
        else:
            return "UNCONFIRMED"
        if actual_created is not None and actual_created != created:
            return "DEAD"
        return state

    def _host_alive(self, job: dict[str, Any]) -> bool:
        return self._classify_host(job) == "ALIVE"

    def recover_job(self, job_id: str, *, now: float | None = None) -> str:
        """Claim one replacement generation after durable host and child-tree proof."""
        with _ownership_lock(self.job_dir, job_id):
            job = self._read_job(job_id)
            if job is None or job.get("state") in TERMINAL:
                return "UNCHANGED"
            root = self.job_dir.parent.parent
            try:
                index = json.loads((root / "async_jobs" / (job_id + ".json")).read_text(encoding="utf-8"))
            except (OSError, ValueError, TypeError):
                index = None
            if isinstance(index, dict) and index.get("stage") == "CONSUMED":
                return "UNCHANGED"
            current = time.time() if now is None else now
            # An unclaimed QUEUED generation can still start. No process identity
            # means there is no proof that it is safe to replace it.
            if job.get("leaseQueued") and job["state"] == "QUEUED" and not job.get("hostPid"):
                if current >= job["deadlineAt"]:
                    self._finish_job(job_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                    return "TIMED_OUT"
                job.update(generation=job["generation"] + 1,
                           ownershipToken=uuid.uuid4().hex)
                self._write_job(job)
                try:
                    _launch_host(self.job_dir, job_id, job["generation"], job["ownershipToken"])
                except OSError:
                    return "UNCONFIRMED"
                return "RECOVERED"
            if (not job.get("hostPid")
                    or (os.name == "nt" and not job.get("hostContained"))):
                return "UNCONFIRMED"
            classification = self._classify_host(job)
            if classification != "DEAD":
                return classification

            def ambiguous(code: str, message: str) -> str:
                job.update(state="UNKNOWN_OR_IN_PROGRESS", finishedAt=current,
                           errorCode=code, message=message)
                self._write_job(job)
                return "AMBIGUOUS"

            if job.get("leaseQueued") and job["state"] == "QUEUED":
                if current >= job["deadlineAt"]:
                    self._finish_job(job_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                    return "TIMED_OUT"
                job.update(generation=job["generation"] + 1,
                           ownershipToken=uuid.uuid4().hex)
                for key in ("hostPid", "hostCreatedAt", "hostContained", "signalPort"):
                    job.pop(key, None)
                self._write_job(job)
                try:
                    _launch_host(self.job_dir, job_id, job["generation"], job["ownershipToken"])
                except OSError:
                    return ambiguous("RECOVERY_HOST_LAUNCH_FAILED", "Queued host launch failed")
                return "RECOVERED"

            if not isinstance(job.get("hostCreatedAt"), int) or not isinstance(job.get("generation"), int):
                return ambiguous("RECOVERY_PROOF_MISSING", "Host or child-tree termination proof is missing")
            if os.name != "nt" and job.get("state") == "RUNNING":
                child_pid = job.get("pid")
                if (not isinstance(child_pid, int) or isinstance(child_pid, bool) or child_pid <= 0
                        or not _posix_process_group_stopped(child_pid)):
                    return "UNCONFIRMED"
            attempts = job.get("resumedAfterInterruption", 0)
            if not isinstance(attempts, int) or attempts < 0:
                return ambiguous("RECOVERY_PROOF_MISSING", "Recovery attempt history is invalid")
            if attempts >= 2:
                return ambiguous("RECOVERY_ATTEMPTS_EXHAUSTED", "Automatic recovery attempt limit reached")
            launch_path = self.job_dir / (job_id + ".launch")
            try:
                launch = json.loads(launch_path.read_text(encoding="utf-8"))
                original_name = launch.get("originalName", launch["name"])
                original_args = launch.get("originalArgs", launch["args"])
                if original_name not in ("codex", "codex-reply") or not isinstance(original_args, dict):
                    raise ValueError("invalid launch arguments")
                thread_id = job.get("threadId")
                if thread_id is not None and (not isinstance(thread_id, str) or not thread_id):
                    raise ValueError("invalid thread identity")
                if thread_id:
                    config = job.get("threadConfig")
                    if not isinstance(config, dict) or not isinstance(config.get("cwd"), str) or not isinstance(config.get("config_args"), list):
                        return ambiguous("RECOVERY_THREAD_CONFIG_MISSING", "Thread resume configuration is missing")
                    name = "codex-reply"
                    args = {"threadId": thread_id, "prompt": CONTINUATION_PROMPT}
                    _save_record(self.state_dir, {"thread_id": thread_id, **config})
                else:
                    if original_name != "codex":
                        return ambiguous("RECOVERY_THREAD_MISSING", "Original reply thread identity is missing")
                    name = "codex"
                    args = {**original_args, "prompt": original_args["prompt"] + "\n\n" + CONTINUATION_PROMPT}
                if not isinstance(original_args.get("prompt"), str):
                    raise ValueError("original prompt missing")
            except (OSError, ValueError, KeyError, TypeError) as exc:
                return ambiguous("RECOVERY_LAUNCH_MISSING", f"Recovery launch data unavailable: {exc}")
            timeout = original_args.get("timeoutSeconds", ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS)
            if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or timeout <= 0:
                return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery timeout is invalid")
            deadline = time.time() + timeout
            if isinstance(index, dict):
                try:
                    if index.get("jobId") != job_id or index.get("call") != job.get("call"):
                        raise ValueError("async index does not match job")
                    pending = json.loads((root / "pending" / (index["call_key"] + ".json")).read_text(encoding="utf-8"))
                    if pending.get("job_id") != job_id or pending.get("async_call") != job.get("call"):
                        raise ValueError("pending call does not match job")
                    import ledger
                    lease = ledger.ScopeLeases(root / "projects" / index["project_key"])
                    extension = lease.extend_retention(pending.get("lease_owner", index["call_key"]),
                                                       deadline + RETENTION_SECONDS)
                    if extension["status"] != ledger.OK:
                        raise ValueError(extension["code"])
                except (OSError, ValueError, KeyError, TypeError) as exc:
                    return ambiguous("RECOVERY_OWNERSHIP_MISSING", f"Async ownership unavailable: {exc}")
            launch.update(name=name, args=args, originalName=original_name, originalArgs=original_args)
            _atomic_json(launch_path, launch)
            job.update(state="QUEUED", generation=job["generation"] + 1,
                       ownershipToken=uuid.uuid4().hex, resumedAfterInterruption=attempts + 1,
                       deadlineAt=deadline)
            for key in ("hostPid", "hostCreatedAt", "hostContained", "signalPort", "startedAt", "finishedAt", "exitCode", "errorCode", "message"):
                job.pop(key, None)
            if isinstance(job.get("stallDetectedAt"), (int, float)):
                job["errorCode"] = "STALLED"
            self._write_job(job)
            try:
                _launch_host(self.job_dir, job_id, job["generation"], job["ownershipToken"])
            except OSError as exc:
                return ambiguous("RECOVERY_HOST_LAUNCH_FAILED", f"Replacement host launch failed: {exc}")
            return "RECOVERED"

    def _await_host(self, job_id: str) -> dict[str, Any]:
        until = time.monotonic() + 10
        while time.monotonic() < until:
            job = self._read_job(job_id)
            if job is not None and job.get("leaseQueued") and job.get("state") == "QUEUED" and self._host_ping(job):
                return _job_result({key: job[key] for key in ("jobId", "state", "acceptedAt", "deadlineAt")})
            if job is not None and job.get("state") == "RUNNING" and (self._host_alive(job) or self._host_ping(job)):
                return _job_result({key: job[key] for key in ("jobId", "state", "acceptedAt", "deadlineAt")})
            if job is not None and job.get("state") in TERMINAL - {"UNKNOWN_OR_IN_PROGRESS"}:
                return _job_result({**{key: job[key] for key in ("jobId", "acceptedAt", "deadlineAt")},
                                    "state": "RUNNING"})
            if job is not None and job.get("state") == "UNKNOWN_OR_IN_PROGRESS":
                return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                                    "errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership or child state unconfirmed"})
            time.sleep(0.02)
        return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                            "errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership was not acknowledged"})

    def _finish_job(self, job_id: str, state: str, code: str | None = None,
                    result: dict[str, Any] | None = None, message: str | None = None) -> None:
        with self.jobs_lock:
            job = self._read_job(job_id)
            if job is None or job["state"] in TERMINAL:
                return
            if job_id in self.cancelled and state not in ("CANCELLED", "UNKNOWN_OR_IN_PROGRESS"):
                return
            job.update(state=state, finishedAt=time.time())
            if state in TERMINAL:
                job.pop("stallDetectedAt", None)
                if job.get("errorCode") == "STALLED":
                    job.pop("errorCode", None)
            process = self.job_processes.get(job_id)
            if process is not None and process.poll() is not None:
                job["exitCode"] = process.returncode
            if code:
                message = message or {
                    "CODEX_FAILED": "Codex failed", "DEADLINE_EXCEEDED": "Codex deadline exceeded",
                    "CANCELLED": "Codex cancelled", "PROCESS_UNCONFIRMED": "Process termination unconfirmed",
                    "RECOVERY_RESUME_FAILED": "Interrupted Codex thread could not be resumed",
                    "ADMISSION_BASELINE_UNAVAILABLE": "Fresh admission baseline could not be bound; FULL review required",
                    "WORKTREE_SNAPSHOT_FAILED": "Worktree snapshot could not be created",
                    "WORKTREE_MISSING": "Job worktree is unavailable",
                    "WORKTREE_SETUP_FAILED": "Worktree setup failed",
                }[code]
                if code == "WORKTREE_SETUP_FAILED":
                    setup_error = job.get("setupError", {})
                    reason = ("timed out" if setup_error.get("timedOut") else
                              f"exited with code {setup_error.get('exitCode')}")
                    output = setup_error.get("output", "")
                    message = f"Worktree setup {reason}" + (f": {output}" if output else "")
                job.update(errorCode=code, message=message)
            if result is not None:
                output = self.job_dir / (job_id + ".output")
                data = result["content"].encode("utf-8")
                if len(data) > 1_000_000:
                    job.update(state="FAILED", errorCode="CODEX_FAILED", message="Codex output exceeded limit")
                else:
                    temporary = output.with_name("." + uuid.uuid4().hex + ".tmp")
                    with open(temporary, "xb") as stream:
                        if os.name != "nt":
                            os.chmod(temporary, 0o600)
                        stream.write(data)
                        stream.flush()
                        os.fsync(stream.fileno())
                    os.replace(temporary, output)
                    job.update(threadId=result["threadId"], outputFile=output.name)
                    usage = result.get("usage")
                    if isinstance(usage, dict):
                        job["usage"] = _codex_turn_usage([{"type": "turn.completed", "usage": usage}])
                    if "attemptedModels" in result:
                        job.update(attemptedModels=result["attemptedModels"],
                                   servedAgent=result.get("servedAgent"),
                                   servedModel=result.get("servedModel"))
            if job["state"] in TERMINAL:
                job.setdefault("boardDisplayCount", 0)
            self._write_job(job)
            if job.get("leaseQueued") and "startedAt" not in job and state in {"CANCELLED", "TIMED_OUT"}:
                try:
                    import ledger
                    root = self.job_dir.parent.parent
                    index = json.loads((root / "async_jobs" / (job_id + ".json")).read_text(encoding="utf-8"))
                    lease = ledger.ScopeLeases(root / "projects" / index["project_key"])
                    lease.release(index["call_key"])
                except (OSError, ValueError, KeyError, TypeError):
                    pass  # Finalization retries lease release if the index is unavailable.
            if state != "UNKNOWN_OR_IN_PROGRESS":
                (self.job_dir / (job_id + ".launch")).unlink(missing_ok=True)
            self.jobs.pop(job_id, None)
            self.job_processes.pop(job_id, None)
            event = self.job_events.get(job_id)
            if event:
                event.set()

    def _start_job(self, args: dict[str, Any]) -> dict[str, Any]:
        job_agent = args.get("agent", self.agent)
        if job_agent not in ("codex", "opencode", "agy"):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "Invalid agent"})
        if "agent" not in args and not self._fixed_agent and not os.environ.get("PURSER_AGENT"):
            try:
                job_agent = self._configured_agent()
            except (OSError, ValueError, json.JSONDecodeError) as exc:
                return _job_result({"state": "FAILED", "errorCode": "INVALID_CONFIG", "message": str(exc)})
        call = args.get("call")
        prompt = args.get("prompt")
        timeout = args.get("timeoutSeconds", ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS)
        if not isinstance(call, str) or not call or not isinstance(prompt, str):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "call and prompt are required"})
        try:
            uuid.UUID(call)
        except (ValueError, TypeError):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "call must be a UUID"})
        job_id = hashlib.sha256(call.encode("utf-8")).hexdigest()
        if self._read_job(job_id) is not None:
            return self._await_host(job_id)
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout <= 0:
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "timeoutSeconds must be positive"})
        mode = args.get("mode", "new")
        if mode not in ("new", "reply"):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "invalid mode"})
        name = "codex-reply" if mode == "reply" else "codex"
        try:
            self._agent_prefix(job_agent)
            if mode == "new":
                if not isinstance(args.get("cwd", os.getcwd()), str) or not args.get("cwd", os.getcwd()):
                    raise ValueError("invalid cwd")
                if args.get("model") is not None and not isinstance(args["model"], str):
                    raise ValueError("invalid model")
                if not isinstance(args.get("config", {}), dict):
                    raise ValueError("invalid config")
                _flatten_config(args.get("config", {}))
                if args.get("sandbox") not in (None, "read-only", "workspace-write", "danger-full-access"):
                    raise ValueError("invalid sandbox")
                if "worktree" in args and not isinstance(args["worktree"], bool):
                    raise ValueError("invalid worktree")
                if args.get("approval-policy") not in (None, "on-request", "never"):
                    raise ValueError("invalid approval-policy")
                for field in ("developer-instructions", "base-instructions", "compact-prompt"):
                    if field in args and not isinstance(args[field], str):
                        raise ValueError("invalid " + field)
                if job_agent in ("opencode", "agy"):
                    if args.get("approval-policy") is not None or any(
                            field in args for field in ("developer-instructions", "base-instructions", "compact-prompt")):
                        raise ValueError("unsupported OpenCode option")
                    _agent_command(job_agent, [], name, args.get("cwd", os.getcwd()),
                                   args.get("model"), _flatten_config(args.get("config", {})),
                                   args.get("sandbox"), None, Path("unused"), prompt)
        except (OSError, ValueError, TypeError):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "Invalid Codex launch arguments"})
        send_back_from = None
        with self.jobs_lock:
            existing = self._read_job(job_id)
            if existing is not None:
                pass
            else:
                reply_record = None
                if mode == "reply":
                    if not isinstance(args.get("threadId"), str):
                        return _job_result({"state": "FAILED", "errorCode": "UNKNOWN_THREAD", "message": "Thread is unknown to this server"})
                    reply_record = _load_record(self.state_dir, args["threadId"])
                    if reply_record is None:
                        return _job_result({"state": "FAILED", "errorCode": "UNKNOWN_THREAD", "message": "Thread is unknown to this server"})
                    send_back_from = _unapplied_thread_job(self.job_dir, args["threadId"])
                    if send_back_from is not None and isinstance(send_back_from.get("snapshot"), str):
                        # Replay preparation advances the worker baseline, while the
                        # thread record may still describe the original worktree tree.
                        reply_record["snapshot"] = send_back_from["snapshot"]
                        reply_record["baselineSnapshot"] = send_back_from["snapshot"]
                    if send_back_from is not None and reply_record.get("worktreePath"):
                        import ledger
                        repo_root = Path(reply_record["repoRoot"]).resolve()
                        worktree = Path(reply_record["worktreePath"]).resolve()
                        snapshot = reply_record.get("baselineSnapshot", reply_record.get("snapshot"))
                        if isinstance(snapshot, str) and worktree.is_dir():
                            try:
                                patch = ledger.job_patch(worktree, snapshot)
                                raw_paths = ledger._run_git(
                                    repo_root, ["apply", "--numstat", "-z", "-"], 30.0,
                                    "listing thread changes", input_data=patch.encode("utf-8", "surrogateescape")) if patch else b""
                                paths = [ledger._decode_path(row.split(b"\t", 2)[2])
                                         for row in raw_paths.split(b"\0") if row]
                                if all(_same_worktree_content(worktree, repo_root, path) for path in paths):
                                    send_back_from = None
                            except (OSError, ledger._InspectionError):
                                pass
                    if send_back_from is None and reply_record.get("worktreePath"):
                        import ledger
                        repo_root = Path(reply_record["repoRoot"]).resolve()
                        worktree = Path(reply_record["worktreePath"]).resolve()
                        lock_key = hashlib.sha256(os.fspath(repo_root).encode("utf-8")).hexdigest()
                        lock_path = self.job_dir.parent / "apply" / (lock_key + ".lock")
                        try:
                            with ledger._lease_lock(lock_path):
                                head = ledger._decode_path(ledger._run_git(
                                    repo_root, ["rev-parse", "--verify", "HEAD"], 30.0,
                                    "reading reply repository HEAD")).strip()
                                snapshot = ledger._decode_path(ledger._run_git(
                                    repo_root, ["rev-parse", f"{head}^{{tree}}"], 30.0,
                                    "reading reply repository tree")).strip()
                                ledger._run_git(worktree, ["clean", "-fd"], 30.0,
                                                "clearing applied reply worktree changes")
                                ledger._run_git(worktree, ["reset", "--hard", head], 30.0,
                                                "resetting applied reply worktree HEAD")
                        except (OSError, ledger._InspectionError):
                            return _job_result({"state": "FAILED", "errorCode": "WORKTREE_REBASE_FAILED",
                                                "message": "Applied thread worktree could not be rebased to the current checkout"})
                        reply_record.update(snapshot=snapshot, baselineSnapshot=snapshot, head=head)
                now = time.time()
                job = {"jobId": job_id, "call": call, "state": "QUEUED", "acceptedAt": now,
                       "deadlineAt": now + timeout, "signalToken": uuid.uuid4().hex,
                       "generation": 1, "ownershipToken": uuid.uuid4().hex}
                if reply_record is not None:
                    job.update({key: reply_record[key] for key in
                                ("worktreePath", "snapshot", "head", "repoRoot",
                                 "baselineSnapshot") if key in reply_record})
                    if job.get("worktreePath"):
                        job["baselineSnapshot"] = job.get("baselineSnapshot", job["snapshot"])
                        job["snapshot"] = job["baselineSnapshot"]
                if args.get("leaseQueued") is True:
                    job["leaseQueued"] = True
                for field in ("taskId", "route", "etaMinutes"):
                    if field in args:
                        job[field] = args[field]
                for stored, supplied in (("model", "jobModel"), ("effort", "jobEffort"),
                                         ("label", "jobLabel"), ("phase", "jobPhase")):
                    value = args.get(supplied, args.get(stored))
                    if value is not None:
                        job[stored] = value
                if args.get("readinessGrounded") is True and isinstance(args.get("readinessCriteria"), dict):
                    criteria = args["readinessCriteria"]
                    if (set(criteria) == {"objective", "acceptance_criteria"}
                            and all(isinstance(criteria[key], str) for key in criteria)
                            and sum(len(value.encode("utf-8")) for value in criteria.values()) <= 20_000
                            and args.get("readinessAgent") in ("agy", "off")
                            and isinstance(args.get("readinessModel"), str)):
                        job["readinessCheck"] = {
                            "grounded": True, **criteria,
                            "agent": args["readinessAgent"], "model": args["readinessModel"],
                        }
                self.job_dir.mkdir(parents=True, exist_ok=True)
                if os.name != "nt":
                    os.chmod(self.job_dir, 0o700)
                reservation = self.job_dir / (job_id + ".lock")
                try:
                    reservation.mkdir()
                except FileExistsError:
                    existing = self._read_job(job_id)
                else:
                    # Claim persists even if the launching bridge dies. A later
                    # bridge can observe but never start this call a second time.
                    _atomic_json(self.job_dir / (job_id + ".launch"),
                                 {"name": name, "args": args, "codexBin": self.codex_bin,
                                 "agent": job_agent})
                    self._write_job(job)
                    if mode == "new" and isinstance(job.get("taskId"), str) and job["taskId"]:
                        for path in sorted(self.job_dir.glob("*.json")):
                            earlier_id = path.stem
                            if earlier_id == job_id:
                                continue
                            with _ownership_lock(self.job_dir, earlier_id):
                                earlier = self._read_job(earlier_id)
                                if (earlier is None or earlier.get("taskId") != job["taskId"]
                                        or earlier.get("decisionRecorded")
                                        or earlier.get("state") != "SUCCEEDED"
                                        or not earlier.get("worktreePath")
                                        or earlier.get("applyState") == "applied"):
                                    continue
                                _record_claude_decision(earlier, "rejected", "redispatch", job_id)
                                earlier["decisionRecorded"] = True
                                self._write_job(earlier)
                    try:
                        _launch_host(self.job_dir, job_id, job["generation"], job["ownershipToken"])
                    except OSError as exc:
                        print(f"Codex host launch failed: {exc}", file=sys.stderr, flush=True)
                        return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                                            "errorCode": "PROCESS_UNCONFIRMED", "message": "Host launch unconfirmed"})
                    if send_back_from is not None:
                        with _ownership_lock(self.job_dir, send_back_from["jobId"]):
                            current = self._read_job(send_back_from["jobId"])
                            if current is not None:
                                _record_claude_decision(current, "send_back", "reply", job_id)
                                if isinstance(current.get("taskId"), str) and current["taskId"]:
                                    current["decisionRecorded"] = True
                                self._write_job(current)
        return self._await_host(job_id)

    def _job_worker(self, job_id: str, name: str, args: dict[str, Any]) -> None:
        try:
            if self._read_job(job_id).get("leaseQueued") is True:
                import codex
                root = self.job_dir.parent.parent
                while True:
                    job = self._read_job(job_id)
                    if job is None or job["state"] != "QUEUED":
                        return
                    if time.time() >= job["deadlineAt"]:
                        self._finish_job(job_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                        return
                    try:
                        admitted = codex.bind_queued_baseline(root, job_id)
                    except FileNotFoundError:
                        admitted = False  # PostToolUse has not written the index yet.
                    except (OSError, ValueError, TypeError, KeyError, codex.WiringError):
                        self._finish_job(job_id, "UNKNOWN_OR_IN_PROGRESS", "ADMISSION_BASELINE_UNAVAILABLE")
                        return
                    if admitted:
                        break
                    time.sleep(1.0)
            if time.time() >= self._read_job(job_id)["deadlineAt"]:
                self._finish_job(job_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                return
            with self.jobs_lock:
                job = self._read_job(job_id)
                if (job is None or job["state"] != "QUEUED"
                        or (self.claim_generation is not None and
                            (job.get("generation") != self.claim_generation or
                             job.get("ownershipToken") != self.claim_token))):
                    return
                job.update(state="RUNNING", startedAt=time.time())
                self._write_job(job)
            result = _run_with_responsiveness_retries(
                lambda: self._run_tool(name, args, job_id, async_job=True))
            evidence = result.get("typedEvidence") if isinstance(result, dict) else None
            with self.jobs_lock:
                current = self._read_job(job_id)
                if current is not None and current["state"] not in TERMINAL:
                    if isinstance(evidence, dict):
                        current["launchFailureEvidence"] = evidence
                    if isinstance(result, dict) and isinstance(result.get("responsivenessAttempts"), int):
                        current["responsivenessAttempts"] = result["responsivenessAttempts"]
                    self._write_job(current)
            value = result.get("structuredContent")
            if result.get("isError") or not isinstance(value, dict):
                with self.jobs_lock:
                    current = self._read_job(job_id)
                    if (current is not None and current["state"] not in TERMINAL
                            and isinstance(result.get("attemptedModels"), list)):
                        current.update(attemptedModels=result["attemptedModels"],
                                       servedAgent=result.get("servedAgent"),
                                       servedModel=result.get("servedModel"))
                        self._write_job(current)
                if (current is not None and "pid" not in current
                        and time.time() >= current["deadlineAt"]):
                    self._finish_job(job_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                elif current and current.get("resumedAfterInterruption") and name == "codex-reply":
                    self._finish_job(job_id, "UNKNOWN_OR_IN_PROGRESS", "RECOVERY_RESUME_FAILED")
                else:
                    detail = None
                    if result.get("isError") and result.get("content"):
                        try:
                            error_value = json.loads(result["content"][0]["text"]).get("error")
                            if isinstance(error_value, str) and error_value:
                                detail = f"Codex failed: {error_value.splitlines()[0]}"[:200]
                        except (IndexError, KeyError, TypeError, ValueError):
                            pass
                    self._finish_job(job_id, "FAILED", result.get("errorCode", "CODEX_FAILED"),
                                     message=detail)
            else:
                self._check_readiness_before_success(job_id, value)
                self._finish_job(job_id, "SUCCEEDED", result=value)
        except Exception as exc:
            print(f"Codex job error: {exc}", file=sys.stderr, flush=True)
            self._finish_job(job_id, "FAILED", "CODEX_FAILED")

    def _result_job(self, args: dict[str, Any]) -> dict[str, Any]:
        job = self._read_job(args.get("jobId"))
        if job is None:
            return _job_result({"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
        state = job["state"]
        if state == "EXPIRED":
            return _job_result({"jobId": job["jobId"], "state": "FAILED", "errorCode": "JOB_EXPIRED", "message": "Job record expired"})
        if state in TERMINAL and time.time() - job.get("finishedAt", job["acceptedAt"]) > RETENTION_SECONDS:
            if state != "EXPIRED":
                (self.job_dir / job.get("outputFile", "unused")).unlink(missing_ok=True)
                job.update(state="EXPIRED", errorCode="JOB_EXPIRED", message="Job record expired")
                self._write_job(job)
            return _job_result({"jobId": job["jobId"], "state": "FAILED", "errorCode": "JOB_EXPIRED", "message": "Job record expired"})
        if state not in TERMINAL:
            host_state = self.recover_job(job["jobId"])
            job = self._read_job(job["jobId"]) or job
            state = job["state"]
            if host_state == "UNCONFIRMED":
                return _job_result({"jobId": job["jobId"], "state": state,
                                    "message": "Host identity unconfirmed"})
        if state == "SUCCEEDED" and not job.get("worktreePath") and not job.get("decisionRecorded"):
            with _ownership_lock(self.job_dir, job["jobId"]):
                current = self._read_job(job["jobId"])
                if (current is not None and current.get("state") == "SUCCEEDED"
                        and not current.get("worktreePath") and not current.get("decisionRecorded")
                        and isinstance(current.get("taskId"), str) and current["taskId"]):
                    _record_claude_decision(current, "accepted", "result", current["jobId"])
                    current["decisionRecorded"] = True
                    self._write_job(current)
                    job = current
        value = {"jobId": job["jobId"], "state": state}
        if "attemptedModels" in job:
            value.update(attemptedModels=job["attemptedModels"],
                         servedAgent=job.get("servedAgent"), servedModel=job.get("servedModel"))
        if state == "SUCCEEDED":
            try:
                value.update(threadId=job["threadId"], content=(self.job_dir / job["outputFile"]).read_text(encoding="utf-8"))
                if isinstance(job.get("usage"), dict) and job["usage"]:
                    value["usage"] = job["usage"]
            except (OSError, KeyError):
                value.update(state="FAILED", errorCode="OUTPUT_UNAVAILABLE", message="Job output is unavailable")
        elif state in TERMINAL:
            value.update(errorCode=job.get("errorCode", "PROCESS_UNCONFIRMED"), message=job.get("message", "Process state cannot be confirmed"))
        if "worktreePath" in job:
            value["worktreePath"] = job["worktreePath"]
        return _job_result(value)

    def _apply_job(self, args: dict[str, Any]) -> dict[str, Any]:
        if "resolved" in args and not isinstance(args["resolved"], bool):
            return _job_result({"applied": False, "reason": "resolved must be a boolean."})
        job = self._read_job(args.get("jobId"))
        if job is None:
            return _job_result({"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
        if job.get("discarded") is True:
            return _job_result({"jobId": job["jobId"], "errorCode": "DISCARDED",
                                "message": "This job was discarded and cannot be applied."})
        if not job.get("worktreePath"):
            return _job_result({"state": "FAILED", "errorCode": "NOT_WORKTREE_JOB", "message": "Job has no worktree"})
        hand_resolvable_replay_failure = (
            args.get("resolved") is True
            and job.get("state") == "FAILED"
            and job.get("applyConflictReason") == "worker_replay"
        )
        if job.get("state") != "SUCCEEDED" and not hand_resolvable_replay_failure:
            return _job_result({"state": "FAILED", "errorCode": "JOB_NOT_READY", "message": "Job has not succeeded"})
        import ledger
        repo_root = Path(job["repoRoot"]).resolve()
        lock_key = hashlib.sha256(os.fspath(repo_root).encode("utf-8")).hexdigest()
        lock_path = purser_state_root.default_state_root() / "codex_exec_mcp" / "apply" / (lock_key + ".lock")
        with ledger._lease_lock(lock_path):
            job = self._read_job(job["jobId"])
            if job.get("applyState") == "applied" and isinstance(job.get("applyResult"), dict):
                _record_claude_decision(job, "accepted", "apply", job["jobId"],
                                        args.get("reviewer1Valid"), args.get("reviewer1False"))
                self._release_applied_job_lease(job)
                return _job_result(job["applyResult"])
            if args.get("resolved") is True:
                if (job.get("applyState") != "conflict"
                        and job.get("applyConflictReason") != "worker_replay"):
                    return _job_result({"applied": False,
                                        "reason": "Job is not in conflict state."})
                patch = ledger.job_patch(job["worktreePath"], job["snapshot"])
                raw_paths = ledger._run_git(repo_root, ["apply", "--numstat", "-z", "-"], 30.0,
                                            "listing job patch paths",
                                            input_data=patch.encode("utf-8", "surrogateescape")) if patch else b""
                paths = [ledger._decode_path(row.split(b"\t", 2)[2])
                         for row in raw_paths.split(b"\0") if row]
                if not paths:
                    return _job_result({"applied": False, "reason": "Job has no changed paths."})
                unmerged = ledger._run_git(repo_root, ["diff", "--name-only", "--diff-filter=U", "-z"],
                                           30.0, "checking repository merge conflicts")
                if unmerged:
                    return _job_result({"applied": False, "reason": "Repository has unmerged paths."})
                for path in paths:
                    relative = Path(path)
                    target = repo_root / relative
                    if (relative.is_absolute() or ".." in relative.parts
                            or not target.resolve().is_relative_to(repo_root)):
                        return _job_result({"applied": False, "reason": "Job contains an unsafe path."})
                    if target.is_file() and not target.is_symlink():
                        if any(line.startswith((b"<<<<<<<", b">>>>>>>"))
                               for line in target.read_bytes().splitlines()):
                            return _job_result({"applied": False,
                                                "reason": f"Conflict markers remain in {path}."})
                value = {"applied": True, "resolvedByHand": True, "paths": paths}
                job.update(applyState="applied", applyResult=value, appliedAt=time.time())
                if isinstance(job.get("taskId"), str) and job["taskId"]:
                    job["decisionRecorded"] = True
                self._write_job(job)
                _record_scope_conflict_outcome(job, "resolved_by_hand")
                _record_claude_decision(job, "accepted", "apply", job["jobId"],
                                        args.get("reviewer1Valid"), args.get("reviewer1False"))
                self._release_applied_job_lease(job)
                return _job_result(value)
            if "applyResult" in job:
                return _job_result(job["applyResult"])
            if not Path(job["worktreePath"]).is_dir():
                return _job_result({"state": "FAILED", "errorCode": "WORKTREE_MISSING", "message": "Job worktree is unavailable"})
            patch = ledger.job_patch(job["worktreePath"], job["snapshot"])
            paths = []
            if patch:
                raw_paths = ledger._run_git(repo_root, ["apply", "--numstat", "-z", "-"], 30.0,
                                            "listing job patch paths", input_data=patch.encode("utf-8", "surrogateescape"))
                paths = [ledger._decode_path(row.split(b"\t", 2)[2]) for row in raw_paths.split(b"\0") if row]
            if paths and all(_same_worktree_content(Path(job["worktreePath"]), repo_root, path)
                             for path in paths):
                value = {"applied": True, "paths": paths}
                job.update(applyState="applied", applyResult=value, appliedAt=time.time())
                if isinstance(job.get("taskId"), str) and job["taskId"]:
                    job["decisionRecorded"] = True
                self._write_job(job)
                _record_scope_conflict_outcome(job, "applied")
                _record_claude_decision(job, "accepted", "apply", job["jobId"],
                                        args.get("reviewer1Valid"), args.get("reviewer1False"))
                self._release_applied_job_lease(job)
                return _job_result(value)
            reviewed = job.get("reviewedSnapshot")
            if isinstance(reviewed, str):
                drift = ledger.snapshot_drift_paths(repo_root, reviewed, paths)
                if drift:
                    revision = int(job.get("snapshotRevision", 0)) + 1
                    job.update(snapshotRevision=revision, reviewRefreshPending=True)
                    job.pop("reviewedSnapshot", None)
                    self._write_job(job)
                    try:
                        root = purser_state_root.default_state_root()
                        index_path = root / "async_jobs" / f"{job['jobId']}.json"
                        index = json.loads(index_path.read_text(encoding="utf-8"))
                        index["stage"] = "REVIEW_REFRESH"
                        temporary = index_path.with_suffix(".tmp")
                        temporary.write_text(json.dumps(index, separators=(",", ":")), encoding="utf-8")
                        os.replace(temporary, index_path)
                    except (OSError, ValueError, TypeError):
                        return _job_result({"applied": False, "errorCode": "REVIEW_REFRESH_UNAVAILABLE",
                                            "message": "Job was re-prepared but review refresh could not be scheduled."})
                    return _job_result({"applied": False, "errorCode": "REVIEW_STALE_REPREPARE_REQUIRED",
                                        "message": "Checkout changed in the reviewed job paths. Call codex_result to re-prepare and review the diff before applying.",
                                        "paths": drift})
            outcome = ledger.apply_job_patch(repo_root, patch)
            if not outcome["applied"]:
                try:
                    project_key = project_root.project_key(repo_root)
                    ledger.ScopeLeases(
                        purser_state_root.default_state_root() / "projects" / project_key
                    ).record_conflicts(outcome["conflicts"])
                except (OSError, ValueError, TypeError) as exc:
                    print(f"Conflict learning unavailable: {exc}", file=sys.stderr, flush=True)
            value = ({"applied": True, "paths": paths} if outcome["applied"] else
                     {"applied": False, "conflicts": outcome["conflicts"],
                      "applicable": outcome["applicable"], "worktreePath": job["worktreePath"]})
            job.update(applyState="applied" if outcome["applied"] else "conflict", applyResult=value)
            if outcome["applied"]:
                job["appliedAt"] = time.time()
                if isinstance(job.get("taskId"), str) and job["taskId"]:
                    job["decisionRecorded"] = True
            self._write_job(job)
            if outcome["applied"]:
                _record_scope_conflict_outcome(job, "applied")
                _record_claude_decision(job, "accepted", "apply", job["jobId"],
                                        args.get("reviewer1Valid"), args.get("reviewer1False"))
                self._release_applied_job_lease(job)
            return _job_result(value)

    @staticmethod
    def _release_applied_job_lease(job: dict[str, Any]) -> bool:
        try:
            root = purser_state_root.default_state_root()
            target = Path(job["worktreePath"]).resolve()
            for job_path in (root / "codex_exec_mcp" / "jobs").glob("*.json"):
                try:
                    bound = json.loads(job_path.read_text(encoding="utf-8"))
                    if (not isinstance(bound, dict) or not isinstance(bound.get("jobId"), str)
                            or Path(bound.get("worktreePath", "")).resolve() != target):
                        continue
                    if Server._release_job_scope_lease(root, bound["jobId"]):
                        index = json.loads((root / "async_jobs" / f"{bound['jobId']}.json").read_text(encoding="utf-8"))
                        (root / "pending" / f"{index['call_key']}.json").unlink(missing_ok=True)
                except (OSError, ValueError, TypeError, KeyError):
                    continue
            return True
        except (OSError, ValueError, TypeError, KeyError) as exc:
            print(f"Applied job scope lease release unavailable: {exc}", file=sys.stderr, flush=True)
            return False

    @staticmethod
    def _release_job_scope_lease(root: Path, job_id: str) -> bool:
        import ledger
        try:
            index = json.loads((root / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
            result = ledger.ScopeLeases(root / "projects" / index["project_key"]).release(index["call_key"])
            return result["status"] == ledger.OK or result["code"] == "LEASE_OWNER_MISSING"
        except (OSError, ValueError, TypeError, KeyError):
            return False

    def _cancel_job(self, args: dict[str, Any]) -> dict[str, Any]:
        job = self._read_job(args.get("jobId"))
        if job is None:
            return _job_result({"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
        job_id = job["jobId"]
        if job.get("discarded") is True:
            return _job_result({"jobId": job_id, "state": job["state"], "discarded": True})
        if job["state"] in TERMINAL and job.get("worktreePath") and job.get("applyState") != "applied":
            with _ownership_lock(self.job_dir, job_id):
                current = self._read_job(job_id)
                if (current is not None and current["state"] in TERMINAL and current.get("worktreePath")
                        and current.get("applyState") != "applied"):
                    current["discarded"] = True
                    current.setdefault("discardedAt", time.time())
                    _record_claude_decision(current, "rejected", "discard", job_id)
                    _record_scope_conflict_outcome(current, "discarded")
                    if isinstance(current.get("taskId"), str) and current["taskId"]:
                        current["decisionRecorded"] = True
                    self._write_job(current)
            self._release_job_scope_lease(purser_state_root.default_state_root(), job_id)
            return _job_result({"jobId": job_id, "state": job["state"], "discarded": True})
        if job.get("leaseQueued") and job["state"] == "QUEUED":
            with _ownership_lock(self.job_dir, job_id):
                current = self._read_job(job_id)
                if current is not None and current["state"] == "QUEUED":
                    self._finish_job(job_id, "CANCELLED", "CANCELLED")
                    _record_scope_conflict_outcome(current, "discarded")
                    return self._result_job({"jobId": job_id})
        if self.listener is None:
            if job["state"] in TERMINAL:
                return self._result_job({"jobId": job_id})
            try:
                with socket.create_connection(("127.0.0.1", job["signalPort"]), timeout=5) as connection:
                    connection.settimeout(7)
                    connection.sendall(f'{job_id}:{job["signalToken"]}:CANCEL'.encode("ascii"))
                    confirmed = connection.recv(1) == b"1"
            except (OSError, KeyError, TypeError, ValueError):
                confirmed = False
            if not confirmed:
                return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                                    "errorCode": "PROCESS_UNCONFIRMED", "message": "Process termination unconfirmed"})
            result = self._result_job({"jobId": job_id})
            if result.get("structuredContent", {}).get("state") == "CANCELLED":
                return result
            return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                                "errorCode": "PROCESS_UNCONFIRMED", "message": "Process termination unconfirmed"})
        with self.jobs_lock:
            self.cancelled.add(job_id)
            process = self.job_processes.get(job_id)
        if process is not None:
            tree_kill_confirmed = _stop_process(process)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._finish_job(job_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                return self._result_job({"jobId": job_id})
            if not tree_kill_confirmed and not _child_tree_stopped(process, self.child_job_handle):
                self._finish_job(job_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                return self._result_job({"jobId": job_id})
        self._finish_job(job_id, "CANCELLED", "CANCELLED")
        return self._result_job({"jobId": job_id})

    def send(self, message: dict[str, Any]) -> None:
        with self.write_lock:
            sys.stdout.write(json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n")
            sys.stdout.flush()

    def _run_tool(self, name: str, arguments: dict[str, Any], request_id: Any,
                  async_job: bool = False) -> dict[str, Any]:
        if arguments.get("mechanicalFallback") is not True:
            return self._run_tool_once(name, arguments, request_id, async_job)
        if not async_job:
            model, agent = arguments.get("model"), arguments.get("agent", self.agent)
            result = self._run_tool_once(name, arguments, request_id, async_job)
            value = result.get("structuredContent")
            if isinstance(value, dict):
                value.update(attemptedModels=[model] if isinstance(model, str) else [],
                             servedAgent=agent, servedModel=model)
            return result
        fallback_agent = arguments.get("fallbackAgent")
        fallback_model = arguments.get("fallbackModel")
        fallback_effort = arguments.get("fallbackEffort")
        if (name != "codex" or arguments.get("agent") != "opencode"
                or not isinstance(arguments.get("model"), str)
                or fallback_agent not in ("codex", "opencode", "agy")
                or not isinstance(fallback_model, str) or not isinstance(fallback_effort, str)):
            return self._run_tool_once(name, arguments, request_id, async_job)
        attempted = []
        current = dict(arguments)
        while True:
            model = current.get("model")
            agent = current.get("agent", "opencode")
            attempted.append(model)
            result = self._run_tool_once(name, current, request_id, async_job)
            if not result.get("isError"):
                value = result.get("structuredContent")
                if isinstance(value, dict):
                    value.update(attemptedModels=attempted, servedAgent=agent, servedModel=model)
                return result
            if agent != "opencode" or current.get("mechanicalFallback") is not True:
                result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                return result

            missing_model = result.get("retryableModelMissing") is True
            evidence = result.get("typedEvidence")
            safe_failure = (missing_model
                            or isinstance(evidence, dict) and evidence.get("noWorkProof") is True)
            with self.jobs_lock:
                if request_id in self.cancelled:
                    return _tool_result(None, error="cancelled") | {
                        "attemptedModels": attempted, "servedAgent": None, "servedModel": None}
                current_job = self._read_job(request_id)
                if current_job is None or time.time() >= current_job["deadlineAt"]:
                    result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                    return result
            if safe_failure:
                try:
                    import ledger
                    delta = ledger.compute_job_delta(current_job["worktreePath"], current_job["snapshot"])
                    safe_failure = delta.get("status") == ledger.OK and delta.get("changed_paths") == []
                except Exception:
                    safe_failure = False
            if not safe_failure:
                result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                return result

            if missing_model:
                if not model_listing.mark_candidate_failed(model):
                    result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                    return result
                candidates = [item for item in coder_config.opencode_candidates() if item not in attempted]
                if candidates:
                    current = dict(arguments, agent="opencode", model=candidates[0])
                    continue
                if fallback_agent != "codex":
                    config = dict(arguments.get("config", {}))
                    if fallback_agent == "agy" and fallback_effort != "none":
                        config["model_reasoning_effort"] = fallback_effort
                    current = dict(arguments, agent=fallback_agent, model=fallback_model, config=config)
                    current.pop("mechanicalFallback", None)
                    continue
            elif fallback_agent != "codex":
                result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                return result

            with self.jobs_lock:
                if request_id in self.cancelled:
                    return _tool_result(None, error="cancelled") | {
                        "attemptedModels": attempted, "servedAgent": None, "servedModel": None}
                current_job = self._read_job(request_id)
                if current_job is None or time.time() >= current_job["deadlineAt"]:
                    result.update(attemptedModels=attempted, servedAgent=None, servedModel=None)
                    return result
            config = dict(arguments.get("config", {}))
            if fallback_effort != "none":
                config["model_reasoning_effort"] = fallback_effort
            current = dict(arguments, agent="codex", model=fallback_model, config=config)
            current.pop("mechanicalFallback", None)
            try:
                import mechanical_learning
                mechanical_learning.record_sample(model, False)
            except BaseException:
                pass

    def _run_tool_once(self, name: str, arguments: dict[str, Any], request_id: Any,
                       async_job: bool = False) -> dict[str, Any]:
        launch_attempted = False
        process_started = False
        if name not in ("codex", "codex-reply"):
            return _tool_result(None, error=f"unknown tool: {name}")
        agent = arguments.get("agent", self.agent)
        if agent not in ("codex", "opencode", "agy"):
            return _tool_result(None, error="agent must be codex, opencode or agy")
        prompt = arguments.get("prompt")
        if not isinstance(prompt, str):
            return _tool_result(None, error="prompt must be a string")
        if name == "codex-reply":
            thread_id = arguments.get("threadId", arguments.get("conversationId"))
            if not isinstance(thread_id, str) or not thread_id:
                return _tool_result(None, error="threadId must be a nonempty string")
            record = _load_record(self.state_dir, thread_id)
            if record is None:
                return _tool_result(thread_id, error="thread is unknown to this server; a fresh codex call is required")
            if record.get("agent", "codex") != agent:
                return _tool_result(thread_id, error="thread belongs to a different agent")
            if record.get("worktreePath"):
                if not Path(record["worktreePath"]).is_dir():
                    result = _tool_result(thread_id, error="worktree is unavailable")
                    result["errorCode"] = "WORKTREE_MISSING"
                    return result
                cwd = record["worktreePath"]
            else:
                cwd = record["cwd"]
            model = record.get("model")
            saved_sandbox = record.get("sandbox")
            # The resumed thread retains its original output-history budget.
            config_args = [arg for arg in record["config_args"]
                           if not arg.startswith("tool_output_token_limit=")]
            if saved_sandbox == "read-only":
                config_args = [arg for arg in config_args if not arg.startswith("sandbox_mode=")]
                config_args.append('sandbox_mode="read-only"')
        else:
            saved_sandbox = None
            cwd_value = arguments.get("cwd", os.getcwd())
            if not isinstance(cwd_value, str) or not cwd_value:
                return _tool_result(None, error="cwd must be a nonempty string")
            cwd = str(Path(cwd_value).resolve())
            model = arguments.get("model")
            if model is not None and not isinstance(model, str):
                return _tool_result(None, error="model must be a string")
            config = arguments.get("config", {})
            if not isinstance(config, dict):
                return _tool_result(None, error="config must be an object")
            try:
                config_args = _flatten_config(config)
                for key, target in (("developer-instructions", "developer_instructions"), ("base-instructions", "base_instructions"), ("compact-prompt", "compact_prompt")):
                    if key in arguments:
                        if not isinstance(arguments[key], str):
                            raise ValueError(f"{key} must be a string")
                        config_args.append(f"{target}={_toml(arguments[key])}")
                if "approval-policy" in arguments:
                    if arguments["approval-policy"] not in ("on-request", "never"):
                        raise ValueError("invalid approval-policy")
                    config_args.append(f"approval_policy={_toml(arguments['approval-policy'])}")
            except ValueError as exc:
                return _tool_result(None, error=str(exc))
        try:
            if name == "codex":
                sandbox = arguments.get("sandbox")
                if sandbox is not None:
                    if sandbox not in ("read-only", "workspace-write", "danger-full-access"):
                        raise ValueError("invalid sandbox")
                if agent in ("opencode", "agy") and (arguments.get("approval-policy") is not None or any(
                        field in arguments for field in ("developer-instructions", "base-instructions", "compact-prompt"))):
                    raise ValueError("unsupported OpenCode option")
            else:
                sandbox = saved_sandbox
            self.state_dir.mkdir(parents=True, exist_ok=True)
            temp_dir = self.state_dir / ("codex-output-" + uuid.uuid4().hex)
            temp_dir.mkdir()
            try:
                output_path = temp_dir / "last-message.txt"
                command = _agent_command(agent, self._agent_prefix(agent), name, cwd, model,
                                         config_args, sandbox, thread_id if name == "codex-reply" else None,
                                         output_path, prompt)
                creation = _process_creation_options()
                with self.jobs_lock:
                    if request_id in self.cancelled:
                        return _tool_result(None, error="cancelled")
                    if async_job and time.time() >= self._read_job(request_id)["deadlineAt"]:
                        return _tool_result(None, error="deadline exceeded")
                    launch_attempted = True
                    process = subprocess.Popen(command, cwd=cwd, stdin=subprocess.PIPE if agent == "codex" else subprocess.DEVNULL,
                                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                               creationflags=creation.get("creationflags", 0),
                                               start_new_session=creation.get("start_new_session", False))
                    process_started = True
                    self.jobs[request_id] = process
                    if async_job:
                        self.job_processes[request_id] = process
                        job = self._read_job(request_id)
                        started_at = time.time()
                        previous = job.get("liveness", {})
                        if not isinstance(previous, dict):
                            previous = {}
                        activity = {
                            "route": job.get("route"), "agent": agent,
                            "lastActivityAt": previous.get("lastActivityAt", started_at),
                            "stdoutBytes": previous.get("stdoutBytes", 0),
                            "processAlive": True, "processAliveAt": started_at,
                            "longestSilentGapSeconds": previous.get("longestSilentGapSeconds", 0.0),
                            "stallThresholdSeconds": self._stall_threshold(
                                {**job, "activeAgent": agent}),
                            **({"stallDetectedAt": job["stallDetectedAt"]}
                               if isinstance(job.get("stallDetectedAt"), (int, float)) else {}),
                            "checkpointAt": started_at,
                        }
                        generation = job.get("generation", 1)
                        ownership_token = job.get("ownershipToken", "")
                        with _ownership_lock(self.job_dir, request_id):
                            current = self._read_job(request_id)
                            if (current is not None and current.get("state") == "RUNNING"
                                    and current.get("generation") == generation
                                    and current.get("ownershipToken") == ownership_token):
                                current["activeAgent"] = agent
                                current["liveness"] = {key: value for key, value in activity.items()
                                                       if key != "checkpointAt"}
                                current["pid"] = process.pid
                                self._write_job(current)
                    activity_lock = threading.Lock()

                    def checkpoint_activity(process_alive: bool, *, force: bool = False) -> bool:
                        if not async_job:
                            return False
                        with activity_lock:
                            return self._checkpoint_liveness(
                                request_id, generation, ownership_token, activity,
                                now=time.time(), process_alive=process_alive, force=force)

                events: list[dict[str, Any]] = []
                stdout_had_output: list[bool] = []
                stderr_chunks: list[bytes] = []
                terminal_event = threading.Event()
                exit_forced_after_terminal = False

                def read_stdout_line(line: bytes, recorded_session: list[bool]) -> None:
                    if line.strip():
                        stdout_had_output.append(True)
                    try:
                        event = json.loads(line)
                    except (ValueError, UnicodeError):
                        return
                    if not isinstance(event, dict):
                        return
                    events.append(event)
                    if async_job:
                        with activity_lock:
                            _record_activity(activity, time.time())
                        checkpoint_activity(process.poll() is None)
                    started_id, terminal, _, _ = _agent_event(agent, event)
                    if terminal:
                        terminal_event.set()
                    if async_job and started_id and not recorded_session[0]:
                        recorded_session[0] = True
                        thread_config = {"cwd": cwd, "model": model,
                                         "config_args": config_args,
                                         "sandbox": sandbox, "agent": agent}
                        current = self._read_job(request_id) if async_job else None
                        if current is not None:
                            thread_config.update({key: current[key] for key in
                                                  ("worktreePath", "snapshot", "baselineSnapshot",
                                                   "head", "repoRoot")
                                                  if key in current})
                        with self.jobs_lock:
                            current = self._read_job(request_id)
                            if current is not None and current["state"] not in TERMINAL:
                                current.update(threadId=started_id, threadConfig=thread_config)
                                self._write_job(current)
                        if name == "codex":
                            _save_record(self.state_dir, {"thread_id": started_id, **thread_config})

                def read_stdout() -> None:
                    recorded_session = False
                    recorded = [recorded_session]
                    pending = b""
                    while True:
                        chunk = process.stdout.read1(65536)
                        if not chunk:
                            break
                        if async_job:
                            with activity_lock:
                                activity["stdoutBytes"] += len(chunk)
                                _record_activity(activity, time.time())
                            checkpoint_activity(process.poll() is None)
                        pending += chunk
                        while b"\n" in pending:
                            line, pending = pending.split(b"\n", 1)
                            read_stdout_line(line, recorded)
                    if pending:
                        read_stdout_line(pending, recorded)

                stdout_reader = threading.Thread(target=read_stdout, daemon=True)
                stderr_reader = threading.Thread(target=lambda: stderr_chunks.append(process.stderr.read()), daemon=True)
                stdout_reader.start()
                stderr_reader.start()
                try:
                    if agent == "codex":
                        try:
                            process.stdin.write(prompt.encode("utf-8"))
                        except BrokenPipeError:
                            pass  # The child exited before consuming stdin.
                        finally:
                            try:
                                process.stdin.close()
                            except BrokenPipeError:
                                pass
                    if async_job:
                        while process.poll() is None and not terminal_event.is_set():
                            remaining = job["deadlineAt"] - time.time()
                            if remaining <= 0:
                                raise subprocess.TimeoutExpired(command, remaining)
                            checkpoint_activity(process.poll() is None)
                            terminal_event.wait(timeout=min(remaining, 0.1))
                        if process.poll() is None:
                            remaining = max(0, job["deadlineAt"] - time.time())
                            try:
                                process.wait(timeout=min(CODEX_TERMINAL_GRACE_SECONDS, remaining))
                            except subprocess.TimeoutExpired:
                                if time.time() >= job["deadlineAt"]:
                                    raise
                                tree_kill_confirmed = _stop_process(process)
                                try:
                                    process.wait(timeout=5)
                                except subprocess.TimeoutExpired:
                                    self._finish_job(request_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                                    return _tool_result(None, error="process termination unconfirmed")
                                if not tree_kill_confirmed and not _child_tree_stopped(process, self.child_job_handle):
                                    self._finish_job(request_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                                    return _tool_result(None, error="child tree termination unconfirmed")
                                exit_forced_after_terminal = True
                    else:
                        process.wait()
                except subprocess.TimeoutExpired:
                    _stop_process(process)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        self._finish_job(request_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                        return _tool_result(None, error="process termination unconfirmed")
                    stdout_reader.join()
                    stderr_reader.join()
                    if async_job:
                        with activity_lock:
                            _measure_silent_gap(activity, time.time())
                    checkpoint_activity(process.poll() is None, force=True)
                    self._finish_job(request_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                    return _tool_result(None, error="deadline exceeded")
                stdout_reader.join()
                stderr_reader.join()
                if async_job:
                    with activity_lock:
                        _measure_silent_gap(activity, time.time())
                    checkpoint_activity(process.poll() is None, force=True)
                stderr = b"".join(stderr_chunks).decode("utf-8", errors="replace")
                with self.jobs_lock:
                    self.jobs[request_id] = None
                    if async_job:
                        job = self._read_job(request_id)
                        if job is not None and job["state"] not in TERMINAL:
                            job["exitCode"] = process.returncode
                            self._write_job(job)
                        self.job_processes.pop(request_id, None)
                        if request_id in self.cancelled:
                            return _tool_result(None, error="cancelled")
                parsed = [_agent_event(agent, event) for event in events]
                started = next((session for session, _, _, _ in parsed if session), None)
                failures = [failure for _, _, failure, _ in parsed if failure]
                if agent == "agy" and name == "codex-reply" and started != thread_id:
                    failures.append("Antigravity did not resume the requested conversation; start a fresh call")
                completed = any(event.get("type") == "step_finish" and isinstance(event.get("part"), dict)
                                and event["part"].get("reason") == "stop" for event in events) if agent == "opencode" else any(
                                    isinstance(event.get("status"), str) and event["status"].casefold() == "success"
                                    for event in events) if agent == "agy" else any(
                                    event.get("type") == "turn.completed" for event in events)
                forced_complete = exit_forced_after_terminal and completed and not failures
                if (process.returncode != 0 and not forced_complete) or not started or failures or ((async_job or agent in ("opencode", "agy")) and not completed):
                    detail = ("\n".join(failures) if failures else
                              "turn completion event missing" if async_job and not completed else
                              stderr[-4000:] or f"exit code {process.returncode}")
                    failed_result = _tool_result(started, error=detail)
                    failed_result["typedEvidence"] = _launch_failure_evidence(
                        agent, events, stderr=stderr,
                        stdout_had_output=bool(stdout_had_output),
                        output_had_data=output_path.is_file() and output_path.stat().st_size > 0,
                        process_returncode=process.returncode)
                    if (agent == "opencode" and async_job and not _opencode_has_work(events)
                            and any(_opencode_missing_model(event, model) for event in events)):
                        failed_result["retryableModelMissing"] = True
                    return failed_result
                last_message = output_path.read_text(encoding="utf-8") if agent == "codex" and output_path.is_file() else ""
                if not last_message:
                    last_message = next((final for _, _, _, final in reversed(parsed) if final), "")
                if name == "codex":
                    thread_record = {"thread_id": started, "cwd": cwd, "model": model,
                                     "config_args": config_args, "sandbox": sandbox, "agent": agent}
                    current = self._read_job(request_id) if async_job else None
                    if current is not None:
                        thread_record.update({key: current[key] for key in
                                              ("worktreePath", "snapshot", "baselineSnapshot",
                                               "head", "repoRoot")
                                              if key in current})
                    _save_record(self.state_dir, thread_record)
                if async_job and forced_complete:
                    with self.jobs_lock:
                        job = self._read_job(request_id)
                        if job is not None and job["state"] not in TERMINAL:
                            job["exitForcedAfterComplete"] = True
                            self._write_job(job)
                return _tool_result(
                    started, content=last_message,
                    usage=_codex_turn_usage(events) if agent == "codex" else None,
                )
            finally:
                shutil.rmtree(temp_dir, ignore_errors=True)
        except (OSError, ValueError, TypeError) as exc:
            result = _tool_result(None, error=str(exc))
            if launch_attempted:
                result["typedEvidence"] = _launch_failure_evidence(
                    agent if "agent" in locals() else self.agent, [],
                    process_started=process_started, stderr="" if not process_started else str(exc))
            return result

    def _worker(self, request_id: Any, params: dict[str, Any]) -> None:
        try:
            name = params.get("name")
            arguments = params.get("arguments", {})
            if not isinstance(arguments, dict):
                result = _tool_result(None, error="arguments must be an object")
            elif name == "codex_start":
                result = self._start_job(arguments)
            elif name == "codex_result":
                result = self._result_job(arguments)
            elif name == "codex_cancel":
                result = self._cancel_job(arguments)
            elif name == "codex_apply":
                result = self._apply_job(arguments)
            else:
                result = self._run_tool(name, arguments, request_id)
            with self.jobs_lock:
                cancelled = request_id in self.cancelled
                self.jobs.pop(request_id, None)
                self.cancelled.discard(request_id)
            if not cancelled:
                self.send({"jsonrpc": "2.0", "id": request_id, "result": result})
        except Exception as exc:  # Keep the protocol loop alive on worker failure.
            print(f"Codex worker error: {exc}", file=sys.stderr, flush=True)
            with self.jobs_lock:
                cancelled = request_id in self.cancelled
                self.jobs.pop(request_id, None)
                self.cancelled.discard(request_id)
            if not cancelled:
                self.send({"jsonrpc": "2.0", "id": request_id, "result": _tool_result(None, error=str(exc))})

    def handle(self, message: dict[str, Any]) -> None:
        method = message.get("method")
        request_id = message.get("id")
        if method == "notifications/cancelled":
            params = message.get("params", {})
            cancelled_id = params.get("requestId") if isinstance(params, dict) else None
            if cancelled_id is not None:
                with self.jobs_lock:
                    self.cancelled.add(cancelled_id)
                    process = self.jobs.get(cancelled_id)
                if process is not None:
                    _stop_process(process)
            return
        if request_id is None:
            return
        if method == "initialize":
            params = message.get("params", {})
            version = params.get("protocolVersion") if isinstance(params, dict) else None
            result = {"protocolVersion": version, "serverInfo": {"name": "codex-exec-mcp", "version": "1.0.0"}, "capabilities": {"tools": {}}}
        elif method == "ping":
            result = {}
        elif method == "tools/list":
            result = {"tools": TOOL_SCHEMAS}
        elif method == "tools/call":
            params = message.get("params", {})
            if not isinstance(params, dict):
                self.send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32602, "message": "Invalid params"}})
                return
            with self.jobs_lock:
                self.jobs[request_id] = None
            threading.Thread(target=self._worker, args=(request_id, params), daemon=True).start()
            return
        else:
            self.send({"jsonrpc": "2.0", "id": request_id, "error": {"code": -32601, "message": "Method not found"}})
            return
        self.send({"jsonrpc": "2.0", "id": request_id, "result": result})


def _host_main(job_id: str, job_dir: Path | None, generation: int = 1,
               ownership_token: str = "") -> None:
    if job_dir is None or len(job_id) != 64 or any(ch not in "0123456789abcdef" for ch in job_id):
        return
    server = Server(host=True, job_dir=job_dir)
    launch_path = job_dir / (job_id + ".launch")
    try:
        if server.listener is None:
            raise OSError("host signal listener unavailable")
        with _ownership_lock(job_dir, job_id):
            job = server._read_job(job_id)
            if (job is None or job.get("state") != "QUEUED"
                    or job.get("generation") != generation
                    or job.get("ownershipToken") != ownership_token):
                return  # A later generation owns the job.
            containment = _contain_host_children()
            server.child_job_handle = containment
            if os.name == "nt":
                if not containment:
                    raise OSError("host Job Object containment unconfirmed")
                process_state, created = _windows_process_identity(os.getpid())
                if process_state != "ALIVE" or created is None:
                    raise OSError("host process identity unavailable")
            elif sys.platform.startswith("linux"):
                process_state, created = _posix_process_identity(os.getpid())
                if process_state != "ALIVE" or created is None:
                    raise OSError("host process identity unavailable")
            else:
                created = None
            launch = json.loads(launch_path.read_text(encoding="utf-8"))
            job.update(hostPid=os.getpid(), hostCreatedAt=created,
                       hostContained=bool(containment), signalPort=server.listener.getsockname()[1])
            server._write_job(job)
        if (launch["name"] in ("codex", "codex-reply")
                and (job.get("worktreePath") or launch["args"].get("worktree") is True)):
            import ledger
            if job.get("worktreePath"):
                worktree_path = Path(job["worktreePath"])
                if not worktree_path.is_dir():
                    server._finish_job(job_id, "FAILED", "WORKTREE_MISSING")
                    return
                if job.get("replySnapshotPending") is True:
                    if job.get("resumedAfterInterruption"):
                        server._finish_job(job_id, "FAILED", "WORKTREE_SNAPSHOT_INTERRUPTED")
                        return
                    # Older queued reply records may still carry this marker.
                    # Keep their thread baseline instead of snapshotting the
                    # staged state left by the previous turn.
                    job["snapshot"] = job.get("baselineSnapshot", job["snapshot"])
                    job.pop("replySnapshotPending", None)
                    server._write_job(job)
                repo_root = Path(job.get("repoRoot", worktree_path)).resolve()
                requested_cwd = Path(launch["args"].get("cwd", repo_root)).resolve()
                try:
                    relative_cwd = requested_cwd.relative_to(repo_root)
                except ValueError:
                    relative_cwd = Path()
                launch["args"] = {**launch["args"], "cwd": os.fspath(worktree_path / relative_cwd)}
            else:
                requested_cwd = Path(launch["args"].get("cwd", os.getcwd())).resolve()
                repo_root = requested_cwd
                try:
                    repo_root = Path(os.fsdecode(ledger._run_git(
                        repo_root, ["rev-parse", "--show-toplevel"], 30.0, "resolving job repository root")).strip()).resolve()
                    relative_cwd = requested_cwd.relative_to(repo_root)
                    worktree_parent = purser_state_root.default_state_root() / "wt"
                    snapshot = ledger.create_job_worktree(
                        repo_root, job_id, worktree_parent)
                except ledger.WorktreeSetupError as exc:
                    output = exc.output.encode("utf-8", "replace")[-2048:].decode("utf-8", "replace")
                    job = server._read_job(job_id)
                    if job is not None:
                        job.update(worktreePath=os.fspath(worktree_parent / job_id[:12]),
                                   repoRoot=os.fspath(repo_root),
                                   setupError={"exitCode": exc.exit_code,
                                               "timedOut": exc.timed_out, "output": output})
                        server._write_job(job)
                    server._finish_job(job_id, "FAILED", "WORKTREE_SETUP_FAILED")
                    return
                except Exception:
                    server._finish_job(job_id, "FAILED", "WORKTREE_SNAPSHOT_FAILED")
                    return
                job = server._read_job(job_id)
                job.update(worktreePath=snapshot["path"], snapshot=snapshot["snapshot"],
                           baselineSnapshot=snapshot["snapshot"],
                           head=snapshot["head"], repoRoot=os.fspath(repo_root))
                server._write_job(job)
                launch["args"] = {**launch["args"], "cwd": os.fspath(Path(snapshot["path"]) / relative_cwd)}
                worktree_path = Path(snapshot["path"])
            mapped_cwd = Path(launch["args"]["cwd"])
            if not mapped_cwd.is_dir():
                job = server._read_job(job_id)
                job["cwdFallback"] = os.fspath(mapped_cwd.relative_to(worktree_path))
                server._write_job(job)
                launch["args"] = {**launch["args"], "cwd": os.fspath(worktree_path)}
            if launch.get("agent", "codex") == "codex":
                args = dict(launch["args"])
                config = dict(args.get("config", {}))
                config = {key: value for key, value in config.items()
                          if key != "sandbox_workspace_write"
                          and not key.startswith("sandbox_workspace_write.")}
                config["sandbox_workspace_write"] = {
                    "writable_roots": [os.fspath(worktree_path.resolve())]}
                args["config"] = config
                launch["args"] = args
        server.codex_bin = launch["codexBin"]
        server.agent = launch.get("agent", "codex")
        server.claim_generation = generation
        server.claim_token = ownership_token
        server.job_events[job_id] = threading.Event()
        server._job_worker(job_id, launch["name"], launch["args"])
        # Let connected signal clients receive the terminal notification.
        time.sleep(0.2)
        # The host exit closes its job handle after Codex and its descendants
        # have already exited. A forced host death closes it in the kernel.
        _ = containment
    except BaseException as exc:
        try:
            (job_dir / (job_id + ".host-error")).write_text(str(exc)[:500], encoding="utf-8")
        except OSError:
            pass


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex-bin", help="Codex binary path (or JSON argv prefix for test harnesses)")
    parser.add_argument("--host-job", help=argparse.SUPPRESS)
    parser.add_argument("--host-job-dir", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--generation", type=int, default=1, help=argparse.SUPPRESS)
    parser.add_argument("--ownership-token", default="", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.host_job:
        _host_main(args.host_job, args.host_job_dir, args.generation, args.ownership_token)
        return
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
    server = Server(args.codex_bin)
    # The recovery scan can take longer than the MCP connect timeout, so it
    # must not delay the initialize reply.
    def _recovery_scan() -> None:
        try:
            import async_session_maintenance
            async_session_maintenance.cleanup_jobs(server.job_dir.parent.parent, check_ceiling=True)
        except Exception as exc:
            print(f"Codex recovery scan unavailable: {exc}", file=sys.stderr, flush=True)
    threading.Thread(target=_recovery_scan, name="purser-recovery-scan", daemon=True).start()
    for line in sys.stdin:
        try:
            message = json.loads(line)
            if isinstance(message, dict):
                server.handle(message)
        except (ValueError, TypeError) as exc:
            print(f"Invalid MCP input: {exc}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
