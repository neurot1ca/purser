"""Small stdio MCP bridge for ``codex exec`` (Python 3.11, stdlib only).

For test harnesses, PURSER_CODEX_BIN may be a JSON argv prefix such as
``["C:/Python/python.exe", "C:/path/fake_codex.py"]``. A plain value is a
single executable path. This override is never parsed through a shell.

Async completion uses a loopback TCP listener. A separate waiter can connect
with the job's stored signal token and block on recv(); the bridge writes the
terminal record before sending a byte. TCP sockets work on Windows and POSIX
with the standard library; a dead bridge closes the connection visibly.
"""

from __future__ import annotations

import argparse
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
        "type": "object", "properties": {"jobId": {"type": "string"}}, "required": ["jobId"]}},
    {"name": "codex_cancel", "description": "Cancel a durable Codex job.", "inputSchema": {
        "type": "object", "properties": {"jobId": {"type": "string"}, "reason": {"type": "string"}}, "required": ["jobId"]}},
]

TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS"}
RETENTION_SECONDS = 7 * 24 * 60 * 60


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
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)


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


def _stop_process(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    try:
        if os.name == "nt":
            subprocess.run(["taskkill", "/T", "/F", "/PID", str(process.pid)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        else:
            os.killpg(process.pid, signal.SIGKILL)
    except (OSError, ProcessLookupError) as exc:
        print(f"Codex cancellation failed: {exc}", file=sys.stderr, flush=True)


class Server:
    def __init__(self, codex_bin: str | None = None) -> None:
        self.codex_bin = codex_bin
        self.state_dir = _state_dir()
        self.write_lock = threading.Lock()
        self.jobs_lock = threading.Lock()
        self.jobs: dict[Any, subprocess.Popen[bytes] | None] = {}
        self.cancelled: set[Any] = set()
        self.job_dir = purser_state_root.default_state_root() / "codex_exec_mcp" / "jobs"
        self.job_processes: dict[str, subprocess.Popen[bytes]] = {}
        self.job_events: dict[str, threading.Event] = {}
        self.listener = None
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
            value = json.loads(self._job_path(job_id).read_text(encoding="utf-8"))
            return value if value.get("jobId") == job_id else None
        except (OSError, ValueError, AttributeError):
            return None

    def _write_job(self, job: dict[str, Any]) -> None:
        _atomic_json(self._job_path(job["jobId"]), job)

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
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout <= 0:
            return _job_result({"state": "FAILED", "errorCode": "INVALID_ARGUMENT", "message": "timeoutSeconds must be positive"})
        mode = args.get("mode", "new")
        if self.listener is None:
            return _job_result({"state": "FAILED", "errorCode": "SIGNAL_UNAVAILABLE", "message": "Async completion signal unavailable"})
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
        job_id = hashlib.sha256(call.encode("utf-8")).hexdigest()
        with self.jobs_lock:
            existing = self._read_job(job_id)
            if existing is not None:
                return _job_result({key: existing[key] for key in ("jobId", "state", "acceptedAt", "deadlineAt")})
            if mode == "reply" and (not isinstance(args.get("threadId"), str) or
                                    _load_record(self.state_dir, args["threadId"]) is None):
                return _job_result({"state": "FAILED", "errorCode": "UNKNOWN_THREAD", "message": "Thread is unknown to this server"})
            now = time.time()
            job = {"jobId": job_id, "call": call, "state": "QUEUED", "acceptedAt": now,
                   "deadlineAt": now + timeout, "owner": os.getpid(),
                   "signalPort": self.listener.getsockname()[1], "signalToken": uuid.uuid4().hex}
            self.job_dir.mkdir(parents=True, exist_ok=True)
            if os.name != "nt":
                os.chmod(self.job_dir, 0o700)
            reservation = self.job_dir / (job_id + ".lock")
            try:
                reservation.mkdir()
            except FileExistsError:
                existing = self._read_job(job_id)
                return _job_result({"jobId": job_id, "state": existing["state"] if existing else "UNKNOWN_OR_IN_PROGRESS"})
            try:
                self._write_job(job)
            finally:
                reservation.rmdir()
            self.job_events[job_id] = threading.Event()
            threading.Thread(target=self._job_worker, args=(job_id, name, args), daemon=True).start()
        return _job_result({"jobId": job_id, "state": "RUNNING", "acceptedAt": now, "deadlineAt": now + timeout})

    def _job_worker(self, job_id: str, name: str, args: dict[str, Any]) -> None:
        try:
            with self.jobs_lock:
                job = self._read_job(job_id)
                if job is None or job["state"] != "QUEUED":
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
        if state not in TERMINAL and job["jobId"] not in self.job_events:
            state = "UNKNOWN_OR_IN_PROGRESS"
            job.update(state=state, finishedAt=time.time(), errorCode="PROCESS_UNCONFIRMED", message="Process state cannot be confirmed")
            self._write_job(job)
        value = {"jobId": job["jobId"], "state": "RUNNING" if state == "QUEUED" else state}
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
        with self.jobs_lock:
            self.cancelled.add(job_id)
            process = self.job_processes.get(job_id)
        if process is not None:
            _stop_process(process)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
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
            config_args = record["config_args"]
        else:
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
                creation = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
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
                try:
                    if async_job:
                        remaining = max(0, job["deadlineAt"] - time.time())
                        stdout_bytes, stderr_bytes = process.communicate(input=prompt.encode("utf-8"), timeout=remaining)
                    else:
                        stdout_bytes, stderr_bytes = process.communicate(input=prompt.encode("utf-8"))
                except subprocess.TimeoutExpired:
                    _stop_process(process)
                    try:
                        process.communicate(timeout=5)
                    except subprocess.TimeoutExpired:
                        self._finish_job(request_id, "UNKNOWN_OR_IN_PROGRESS", "PROCESS_UNCONFIRMED")
                        return _tool_result(None, error="process termination unconfirmed")
                    self._finish_job(request_id, "TIMED_OUT", "DEADLINE_EXCEEDED")
                    return _tool_result(None, error="deadline exceeded")
                stdout = stdout_bytes.decode("utf-8", errors="replace")
                stderr = stderr_bytes.decode("utf-8", errors="replace")
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
                events = []
                for line in stdout.splitlines():
                    try:
                        event = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if isinstance(event, dict):
                        events.append(event)
                started = next((event.get("thread_id") for event in events if event.get("type") == "thread.started" and isinstance(event.get("thread_id"), str) and event.get("thread_id")), None)
                failures = [_event_message(event) for event in events if event.get("type") in ("error", "turn.failed")]
                if process.returncode != 0 or not started or failures:
                    detail = "\n".join(failures) if failures else stderr[-4000:] or f"exit code {process.returncode}"
                    return _tool_result(started, error=detail)
                last_message = output_path.read_text(encoding="utf-8") if output_path.is_file() else ""
                if not last_message:
                    last_message = next((item.get("text") for event in reversed(events) if event.get("type") == "item.completed" for item in [event.get("item")] if isinstance(item, dict) and item.get("type") == "agent_message" and isinstance(item.get("text"), str)), "")
                if name == "codex":
                    _save_record(self.state_dir, {"thread_id": started, "cwd": cwd, "model": model, "config_args": config_args})
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


def main() -> None:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex-bin", help="Codex binary path (or JSON argv prefix for test harnesses)")
    args = parser.parse_args()
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
