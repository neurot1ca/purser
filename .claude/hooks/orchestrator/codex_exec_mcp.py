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
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
from typing import Any
import uuid

if os.name == "nt":
    import msvcrt
else:
    import fcntl

if os.name == "nt":
    from ctypes import wintypes

import state_root as purser_state_root


TOOL_SCHEMAS = [
    {
        "name": "codex",
        "description": "Run a new Codex thread.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "prompt": {"type": "string"},
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
            },
            "required": ["prompt"],
        },
    },
]

TOOL_SCHEMAS += [
    {"name": "codex_start", "description": "Start a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {**TOOL_SCHEMAS[0]["inputSchema"]["properties"],
            "call": {"type": "string"}, "mode": {"type": "string", "enum": ["new", "reply"]},
            "threadId": {"type": "string"}, "timeoutSeconds": {"type": "number"}},
        "required": ["prompt"]}},
    {"name": "codex_result", "description": "Read a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"},
            "adopt": {"type": "boolean", "description": "After user consent, finalize a job from an earlier session in this project."}},
        "required": ["jobId"]}},
    {"name": "codex_cancel", "description": "Cancel a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"}, "reason": {"type": "string"}}, "required": ["jobId"]}},
]

TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS"}
RETENTION_SECONDS = 7 * 24 * 60 * 60
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
                         stderr=subprocess.DEVNULL, start_new_session=True, close_fds=True)
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


def _tool_result(thread_id: str | None, *, content: str | None = None, error: str | None = None) -> dict[str, Any]:
    value = {"threadId": thread_id, "error": error} if error is not None else {"threadId": thread_id, "content": content}
    result: dict[str, Any] = {"content": [{"type": "text", "text": json.dumps(value, ensure_ascii=False)}], "isError": error is not None}
    if error is None:
        result["structuredContent"] = value
    return result


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
        os.killpg(process.pid, 0)
        return False
    except ProcessLookupError:
        return True
    except OSError:
        return False


class Server:
    def __init__(self, codex_bin: str | None = None, *, host: bool = False,
                 job_dir: Path | None = None) -> None:
        self.codex_bin = codex_bin
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

    def _host_ping(self, job: dict[str, Any]) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", job["signalPort"]), timeout=0.5) as connection:
                connection.settimeout(0.5)
                connection.sendall(f'{job["jobId"]}:{job["signalToken"]}:PING'.encode("ascii"))
                return connection.recv(1) == b"1"
        except (OSError, KeyError, TypeError, ValueError):
            return False

    def _classify_host(self, job: dict[str, Any]) -> str:
        if os.name != "nt":
            return "UNCONFIRMED"
        pid = job.get("hostPid")
        created = job.get("hostCreatedAt")
        if (not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0
                or not isinstance(created, int) or isinstance(created, bool)
                or not job.get("hostContained")):
            return "UNCONFIRMED"
        state, actual_created = _windows_process_identity(pid)
        if actual_created is not None and actual_created != created:
            return "DEAD"
        return state

    def _host_alive(self, job: dict[str, Any]) -> bool:
        return self._classify_host(job) == "ALIVE"

    def _await_host(self, job_id: str) -> dict[str, Any]:
        until = time.monotonic() + 10
        while time.monotonic() < until:
            job = self._read_job(job_id)
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
                    result: dict[str, Any] | None = None) -> None:
        with self.jobs_lock:
            job = self._read_job(job_id)
            if job is None or job["state"] in TERMINAL:
                return
            if job_id in self.cancelled and state not in ("CANCELLED", "UNKNOWN_OR_IN_PROGRESS"):
                return
            job.update(state=state, finishedAt=time.time())
            process = self.job_processes.get(job_id)
            if process is not None and process.poll() is not None:
                job["exitCode"] = process.returncode
            if code:
                job.update(errorCode=code, message={
                    "CODEX_FAILED": "Codex failed", "DEADLINE_EXCEEDED": "Codex deadline exceeded",
                    "CANCELLED": "Codex cancelled", "PROCESS_UNCONFIRMED": "Process termination unconfirmed",
                }[code])
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
            self._write_job(job)
            if state != "UNKNOWN_OR_IN_PROGRESS":
                (self.job_dir / (job_id + ".launch")).unlink(missing_ok=True)
            self.jobs.pop(job_id, None)
            self.job_processes.pop(job_id, None)
            event = self.job_events.get(job_id)
            if event:
                event.set()

    def _start_job(self, args: dict[str, Any]) -> dict[str, Any]:
        call = args.get("call")
        prompt = args.get("prompt")
        timeout = args.get("timeoutSeconds", 2700)
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
            _binary_prefix(self.codex_bin)
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
                if args.get("approval-policy") not in (None, "on-request", "never"):
                    raise ValueError("invalid approval-policy")
                for field in ("developer-instructions", "base-instructions", "compact-prompt"):
                    if field in args and not isinstance(args[field], str):
                        raise ValueError("invalid " + field)
        except (OSError, ValueError, TypeError):
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "Invalid Codex launch arguments"})
        with self.jobs_lock:
            existing = self._read_job(job_id)
            if existing is not None:
                pass
            else:
                if mode == "reply" and (not isinstance(args.get("threadId"), str) or
                                        _load_record(self.state_dir, args["threadId"]) is None):
                    return _job_result({"state": "FAILED", "errorCode": "UNKNOWN_THREAD", "message": "Thread is unknown to this server"})
                now = time.time()
                job = {"jobId": job_id, "call": call, "state": "QUEUED", "acceptedAt": now,
                       "deadlineAt": now + timeout, "signalToken": uuid.uuid4().hex,
                       "generation": 1, "ownershipToken": uuid.uuid4().hex}
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
                                 {"name": name, "args": args, "codexBin": self.codex_bin})
                    self._write_job(job)
                    try:
                        _launch_host(self.job_dir, job_id, job["generation"], job["ownershipToken"])
                    except OSError as exc:
                        print(f"Codex host launch failed: {exc}", file=sys.stderr, flush=True)
                        return _job_result({"jobId": job_id, "state": "UNKNOWN_OR_IN_PROGRESS",
                                            "errorCode": "PROCESS_UNCONFIRMED", "message": "Host launch unconfirmed"})
        return self._await_host(job_id)

    def _job_worker(self, job_id: str, name: str, args: dict[str, Any]) -> None:
        try:
            with self.jobs_lock:
                job = self._read_job(job_id)
                if (job is None or job["state"] != "QUEUED"
                        or (self.claim_generation is not None and
                            (job.get("generation") != self.claim_generation or
                             job.get("ownershipToken") != self.claim_token))):
                    return
                job.update(state="RUNNING", startedAt=time.time())
                self._write_job(job)
            result = self._run_tool(name, args, job_id, async_job=True)
            value = result.get("structuredContent")
            if result.get("isError") or not isinstance(value, dict):
                self._finish_job(job_id, "FAILED", "CODEX_FAILED")
            else:
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
            host_state = self._classify_host(job)
            if host_state == "DEAD":
                job.update(state="UNKNOWN_OR_IN_PROGRESS", finishedAt=time.time(),
                           errorCode="PROCESS_UNCONFIRMED",
                           message="Host exited; completion evidence requires reconciliation")
                self._write_job(job)
                state = job["state"]
            elif host_state == "UNCONFIRMED":
                return _job_result({"jobId": job["jobId"], "state": state,
                                    "message": "Host identity unconfirmed"})
        value = {"jobId": job["jobId"], "state": state}
        if state == "SUCCEEDED":
            try:
                value.update(threadId=job["threadId"], content=(self.job_dir / job["outputFile"]).read_text(encoding="utf-8"))
            except (OSError, KeyError):
                value.update(state="FAILED", errorCode="OUTPUT_UNAVAILABLE", message="Job output is unavailable")
        elif state in TERMINAL:
            value.update(errorCode=job.get("errorCode", "PROCESS_UNCONFIRMED"), message=job.get("message", "Process state cannot be confirmed"))
        return _job_result(value)

    def _cancel_job(self, args: dict[str, Any]) -> dict[str, Any]:
        job = self._read_job(args.get("jobId"))
        if job is None:
            return _job_result({"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
        job_id = job["jobId"]
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
        if name not in ("codex", "codex-reply"):
            return _tool_result(None, error=f"unknown tool: {name}")
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
            command = _binary_prefix(self.codex_bin) + ["exec"]
            if name == "codex-reply":
                command.append("resume")
            command += ["--json", "--skip-git-repo-check"]
            if model:
                command += ["-m", model]
            for arg in config_args:
                command += ["-c", arg]
            if name == "codex":
                sandbox = arguments.get("sandbox")
                if sandbox is not None:
                    if sandbox not in ("read-only", "workspace-write", "danger-full-access"):
                        raise ValueError("invalid sandbox")
                    command += ["-s", sandbox]
                if name == "codex":
                    command += ["-C", cwd]
            self.state_dir.mkdir(parents=True, exist_ok=True)
            temp_dir = self.state_dir / ("codex-output-" + uuid.uuid4().hex)
            temp_dir.mkdir()
            try:
                output_path = temp_dir / "last-message.txt"
                command += ["-o", str(output_path)]
                if name == "codex-reply":
                    command.append(thread_id)
                command.append("-")
                creation = _process_creation_options()
                with self.jobs_lock:
                    if request_id in self.cancelled:
                        return _tool_result(None, error="cancelled")
                    process = subprocess.Popen(command, cwd=cwd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **creation)
                    self.jobs[request_id] = process
                    if async_job:
                        self.job_processes[request_id] = process
                        job = self._read_job(request_id)
                        job["pid"] = process.pid
                        self._write_job(job)
                events: list[dict[str, Any]] = []
                stderr_chunks: list[bytes] = []

                def read_stdout() -> None:
                    for line in process.stdout:
                        try:
                            event = json.loads(line)
                        except (ValueError, UnicodeError):
                            continue
                        if not isinstance(event, dict):
                            continue
                        events.append(event)
                        started_id = event.get("thread_id")
                        if (async_job and event.get("type") == "thread.started"
                                and isinstance(started_id, str) and started_id):
                            thread_config = {"cwd": cwd, "model": model,
                                             "config_args": config_args, "sandbox": sandbox}
                            with self.jobs_lock:
                                current = self._read_job(request_id)
                                if current is not None and current["state"] not in TERMINAL:
                                    current.update(threadId=started_id, threadConfig=thread_config)
                                    self._write_job(current)
                            if name == "codex":
                                _save_record(self.state_dir, {"thread_id": started_id, **thread_config})

                stdout_reader = threading.Thread(target=read_stdout, daemon=True)
                stderr_reader = threading.Thread(target=lambda: stderr_chunks.append(process.stderr.read()), daemon=True)
                stdout_reader.start()
                stderr_reader.start()
                try:
                    try:
                        process.stdin.write(prompt.encode("utf-8"))
                    except BrokenPipeError:
                        pass  # The child exited before consuming stdin.
                    finally:
                        try:
                            process.stdin.close()
                        except BrokenPipeError:
                            pass
                    remaining = max(0, job["deadlineAt"] - time.time()) if async_job else None
                    process.wait(timeout=remaining)
                except subprocess.TimeoutExpired:
                    _stop_process(process)
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        self._finish_job(request_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                        return _tool_result(None, error="process termination unconfirmed")
                    stdout_reader.join()
                    stderr_reader.join()
                    self._finish_job(request_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                    return _tool_result(None, error="deadline exceeded")
                stdout_reader.join()
                stderr_reader.join()
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
                started = next((event.get("thread_id") for event in events if event.get("type") == "thread.started" and isinstance(event.get("thread_id"), str) and event.get("thread_id")), None)
                failures = [_event_message(event) for event in events if event.get("type") in ("error", "turn.failed")]
                if process.returncode != 0 or not started or failures:
                    detail = "\n".join(failures) if failures else stderr[-4000:] or f"exit code {process.returncode}"
                    return _tool_result(started, error=detail)
                last_message = output_path.read_text(encoding="utf-8") if output_path.is_file() else ""
                if not last_message:
                    last_message = next((item.get("text") for event in reversed(events) if event.get("type") == "item.completed" for item in [event.get("item")] if isinstance(item, dict) and item.get("type") == "agent_message" and isinstance(item.get("text"), str)), "")
                if name == "codex":
                    _save_record(self.state_dir, {"thread_id": started, "cwd": cwd, "model": model, "config_args": config_args, "sandbox": sandbox})
                return _tool_result(started, content=last_message)
            finally:
                shutil.rmtree(temp_dir, ignore_errors=True)
        except (OSError, ValueError, TypeError) as exc:
            return _tool_result(None, error=str(exc))

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
            else:
                created = None
            launch = json.loads(launch_path.read_text(encoding="utf-8"))
            job.update(hostPid=os.getpid(), hostCreatedAt=created,
                       hostContained=bool(containment), signalPort=server.listener.getsockname()[1])
            server._write_job(job)
        server.codex_bin = launch["codexBin"]
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
    for line in sys.stdin:
        try:
            message = json.loads(line)
            if isinstance(message, dict):
                server.handle(message)
        except (ValueError, TypeError) as exc:
            print(f"Invalid MCP input: {exc}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
