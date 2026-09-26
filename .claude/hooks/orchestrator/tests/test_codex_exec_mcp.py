"""Focused protocol tests; the real Codex executable is never launched."""

import importlib.util
import ctypes
import json
import os
from pathlib import Path
import queue
import shutil
import socket
import subprocess
import sys
import threading
import time
import uuid

import pytest


ROOT = Path(__file__).parents[1]
SCRIPT = ROOT / "codex_exec_mcp.py"
SCRATCH = ROOT.parents[2] / ".tmp" / "codex-exec-mcp-tests"
sys.path.insert(0, str(ROOT))
import codex
import codex_exec_mcp


def read_job_record(path):
    text = codex_exec_mcp.purser_state_root.retry_permission_error(
        lambda: path.read_text(encoding="utf-8"))
    return json.loads(text)


FAKE = r'''
import json
import os
from pathlib import Path
import sys
import time

args = sys.argv[1:]
prompt = sys.stdin.buffer.read()
record = {"argv": args, "stdin_hex": prompt.hex(), "cwd": os.getcwd()}
with open(os.environ["FAKE_RECORD"], "a", encoding="utf-8") as stream:
    stream.write(json.dumps(record) + "\n")
    stream.flush()
if os.environ.get("FAKE_SLOW") == "1":
    time.sleep(float(os.environ.get("FAKE_SLEEP_SECONDS", "2")))
thread = os.environ.get("FAKE_THREAD", "thread-123")
print(json.dumps({"type": "thread.started", "thread_id": thread}), flush=True)
release = os.environ.get("FAKE_RELEASE_AFTER_START")
if release:
    deadline = time.monotonic() + 45
    while not Path(release).exists() and time.monotonic() < deadline:
        time.sleep(0.02)
    if not Path(release).exists():
        sys.exit(3)
print(json.dumps({"type": "turn.started"}), flush=True)
if os.environ.get("FAKE_FAIL") == "1":
    print(json.dumps({"type": "turn.failed", "error": {"message": "Quota exceeded: try again tomorrow"}}), flush=True)
    sys.exit(2)
message = "Reply from fake Codex"
print(json.dumps({"type": "item.completed", "item": {"type": "agent_message", "text": message}}), flush=True)
print(json.dumps({"type": "turn.completed", "usage": {}}), flush=True)
if os.environ.get("FAKE_NO_OUTPUT") != "1":
    Path(args[args.index("-o") + 1]).write_text(message, encoding="utf-8")
'''


@pytest.fixture
def area():
    SCRATCH.mkdir(parents=True, exist_ok=True)
    path = SCRATCH / ("case-" + uuid.uuid4().hex)
    path.mkdir(parents=True, exist_ok=True)
    try:
        yield path
    finally:
        _stop_area_hosts(path)
        for attempt in range(20):
            try:
                shutil.rmtree(path)
                break
            except OSError:
                if attempt == 19:
                    raise
                time.sleep(0.05)


def _stop_area_hosts(area):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    if not jobs.is_dir():
        return
    for job_path in jobs.glob("*.json"):
        stop_at = time.monotonic() + 0.5
        deadline = stop_at + 1.0
        pid = None
        while time.monotonic() < deadline:
            try:
                record = json.loads(job_path.read_text(encoding="utf-8"))
                pid = record.get("hostPid")
            except PermissionError:
                time.sleep(0.02)
                continue
            except (OSError, ValueError):
                break
            if not isinstance(pid, int) or pid <= 0 or (os.name == "nt" and windows_process_exited(pid)):
                break
            if os.name != "nt":
                try:
                    os.kill(pid, 0)
                except ProcessLookupError:
                    break
                except PermissionError:
                    pass
            if time.monotonic() >= stop_at:
                try:
                    os.kill(pid, 9)
                except (OSError, ProcessLookupError):
                    pass
            time.sleep(0.02)


def test_job_read_retries_transient_permission_error(area, monkeypatch):
    job_id = "a" * 64
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    path = jobs / f"{job_id}.json"
    path.write_text(json.dumps({"jobId": job_id, "state": "RUNNING"}), encoding="utf-8")
    server = codex_exec_mcp.Server(job_dir=jobs)
    original = Path.read_text
    attempts = 0

    def flaky_read(target, *args, **kwargs):
        nonlocal attempts
        if target == path and attempts < 2:
            attempts += 1
            raise PermissionError(13, "sharing violation", str(path))
        return original(target, *args, **kwargs)

    monkeypatch.setattr(Path, "read_text", flaky_read)
    assert server._read_job(job_id)["state"] == "RUNNING"
    assert attempts == 2


def test_atomic_job_write_retries_transient_replace_permission_error(area, monkeypatch):
    job_id = "b" * 64
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    path = jobs / f"{job_id}.json"
    original = codex_exec_mcp.os.replace
    attempts = 0

    def flaky_replace(source, target):
        nonlocal attempts
        if target == path and attempts < 2:
            attempts += 1
            raise PermissionError(13, "sharing violation", str(path))
        return original(source, target)

    monkeypatch.setattr(codex_exec_mcp.os, "replace", flaky_replace)
    codex_exec_mcp._atomic_json(path, {"jobId": job_id, "state": "RUNNING"})
    assert attempts == 2
    assert read_job_record(path)["state"] == "RUNNING"


class Client:
    def __init__(self, area, *, bin_override=None, extra_env=None):
        fake = area / "fake_codex.py"
        fake.write_text(FAKE, encoding="utf-8")
        self.record = area / "calls.jsonl"
        env = os.environ.copy()
        env.update({"PURSER_CODEX_SHIM_STATE": str(area / "state"), "PURSER_STATE_DIR": str(area / "root"),
                    "PURSER_CODEX_TEST_PARENT_PID": str(os.getpid()), "FAKE_RECORD": str(self.record)})
        env.update(extra_env or {})
        override = bin_override if bin_override is not None else json.dumps([sys.executable, str(fake)])
        self.process = subprocess.Popen(
            [sys.executable, "-u", str(SCRIPT), "--codex-bin", override],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, encoding="utf-8", env=env,
            **({"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}),
        )
        self.messages = queue.Queue()
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        for line in self.process.stdout:
            self.messages.put(json.loads(line))

    def send(self, request_id, method, params=None):
        value = {"jsonrpc": "2.0", "id": request_id, "method": method}
        if params is not None:
            value["params"] = params
        self.process.stdin.write(json.dumps(value, ensure_ascii=False) + "\n")
        self.process.stdin.flush()

    def receive(self, timeout=5):
        return self.messages.get(timeout=timeout)

    def calls(self):
        return [json.loads(line) for line in self.record.read_text(encoding="utf-8").splitlines()] if self.record.exists() else []

    def close(self):
        self.process.stdin.close()
        try:
            self.process.wait(timeout=4)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=4)


@pytest.fixture
def client(area):
    instance = Client(area)
    try:
        yield instance
    finally:
        instance.close()


def call(client, request_id, name, arguments):
    client.send(request_id, "tools/call", {"name": name, "arguments": arguments})
    response = client.receive()
    assert response["id"] == request_id
    return response["result"]


def test_initialize_and_list(client):
    client.send(1, "initialize", {"protocolVersion": "2025-06-18"})
    initialized = client.receive()
    assert initialized["result"] == {
        "protocolVersion": "2025-06-18",
        "serverInfo": {"name": "codex-exec-mcp", "version": "1.0.0"},
        "capabilities": {"tools": {}},
    }
    client.send(2, "tools/list")
    tools = client.receive()["result"]["tools"]
    assert [tool["name"] for tool in tools] == ["codex", "codex-reply", "codex_start", "codex_result", "codex_cancel"]
    start = next(tool for tool in tools if tool["name"] == "codex_start")
    assert "call" not in start["inputSchema"]["required"]
    missing = call(client, 3, "codex_start", {"prompt": "work"})
    assert missing["structuredContent"]["errorCode"] == "INVALID_ARGUMENT"


def test_windows_codex_child_uses_hidden_process_group_flags():
    creation = codex_exec_mcp._process_creation_options(windows=True)
    flags = creation["creationflags"]

    assert flags & codex_exec_mcp.WINDOWS_CREATE_NO_WINDOW
    assert flags & codex_exec_mcp.WINDOWS_CREATE_NEW_PROCESS_GROUP


def test_slow_claim_does_not_become_terminal(tmp_path, monkeypatch):
    job_id = "a" * 64
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    server._write_job({"jobId": job_id, "call": "accepted-call", "state": "QUEUED",
                       "acceptedAt": 1, "signalPort": 0, "signalToken": "missing"})
    launch = tmp_path / f"{job_id}.launch"
    launch.write_text("prompt and args", encoding="utf-8")
    monkeypatch.setattr(server, "_classify_host", lambda job: "UNCONFIRMED")
    first = server._result_job({"jobId": job_id})["structuredContent"]
    record = read_job_record(tmp_path / f"{job_id}.json")
    second = server._result_job({"jobId": job_id})["structuredContent"]
    assert first == second
    assert record["state"] == "QUEUED"
    assert first["state"] == "QUEUED"
    assert "unconfirmed" in first["message"].lower()
    assert launch.exists()


@pytest.mark.parametrize("ping", [False, True])
def test_unconfirmed_host_never_writes_terminal(tmp_path, monkeypatch, ping):
    job_id = "d" * 64
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    job = {"jobId": job_id, "state": "RUNNING", "acceptedAt": 1}
    server._write_job(job)
    monkeypatch.setattr(server, "_classify_host", lambda _job: "UNCONFIRMED")
    monkeypatch.setattr(server, "_host_ping", lambda _job: ping)
    monkeypatch.setattr(server, "_write_job", lambda _job: pytest.fail("unconfirmed host wrote a terminal record"))
    result = server._result_job({"jobId": job_id})["structuredContent"]
    assert result["state"] == "RUNNING"
    assert "unconfirmed" in result["message"].lower()
    assert read_job_record(tmp_path / f"{job_id}.json") == job


@pytest.mark.skipif(os.name != "nt", reason="Windows process identity")
def test_host_identity_detects_pid_reuse_and_unreachable_live_process(tmp_path, monkeypatch):
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    state, created = codex_exec_mcp._windows_process_identity(os.getpid())
    assert state == "ALIVE" and created
    job = {"hostPid": os.getpid(), "hostCreatedAt": created, "hostContained": True}
    monkeypatch.setattr(server, "_host_ping", lambda _job: False)
    assert server._classify_host(job) == "ALIVE"
    assert server._host_alive(job)
    job_id = "c" * 64
    server._write_job({**job, "jobId": job_id, "state": "RUNNING", "acceptedAt": time.time()})
    assert server._result_job({"jobId": job_id})["structuredContent"]["state"] == "RUNNING"
    job["hostCreatedAt"] = created + 1
    assert server._classify_host(job) == "DEAD"


def test_superseded_generation_exits_without_starting_codex(tmp_path, monkeypatch):
    job_id = "b" * 64
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    server._write_job({"jobId": job_id, "state": "QUEUED", "generation": 2,
                       "ownershipToken": "new-owner"})
    monkeypatch.setattr(codex_exec_mcp.Server, "_job_worker",
                        lambda *args: pytest.fail("superseded host started Codex"))
    codex_exec_mcp._host_main(job_id, tmp_path, 1, "old-owner")
    assert read_job_record(tmp_path / f"{job_id}.json")["generation"] == 2
    assert not (tmp_path / f"{job_id}.host-claim").exists()


def test_windows_host_launch_uses_hidden_console_flags(monkeypatch, tmp_path):
    class MockFunction:
        def __init__(self, callback):
            self.callback = callback

        def __call__(self, *args):
            return self.callback(*args)

    creation = {}
    kernel = type("Kernel", (), {})()
    user = type("User", (), {})()
    user.GetShellWindow = MockFunction(lambda: 1)

    def get_shell_pid(_window, pid_pointer):
        ctypes.cast(pid_pointer, ctypes.POINTER(codex_exec_mcp.wintypes.DWORD))[0] = 321
        return 1

    user.GetWindowThreadProcessId = MockFunction(get_shell_pid)
    kernel.OpenProcess = MockFunction(lambda *_args: 1234)
    kernel.CloseHandle = MockFunction(lambda *_args: 1)

    def initialize_attributes(_attributes, _count, _flags, size_pointer):
        ctypes.cast(size_pointer, ctypes.POINTER(ctypes.c_size_t))[0] = 64
        return 1

    kernel.InitializeProcThreadAttributeList = MockFunction(initialize_attributes)
    kernel.UpdateProcThreadAttribute = MockFunction(lambda *_args: 1)
    kernel.DeleteProcThreadAttributeList = MockFunction(lambda *_args: None)

    def create_process(*args):
        creation["flags"] = args[5]
        return 1

    kernel.CreateProcessW = MockFunction(create_process)

    def win_dll(name, **_kwargs):
        return kernel if name == "kernel32" else user

    monkeypatch.setattr(codex_exec_mcp.ctypes, "WinDLL", win_dll, raising=False)
    monkeypatch.setattr(codex_exec_mcp, "os", type("WindowsOS", (), {"name": "nt", "environ": {}})())
    codex_exec_mcp._launch_host(tmp_path, "job")

    flags = creation["flags"]
    assert flags & codex_exec_mcp.WINDOWS_CREATE_NO_WINDOW
    assert not flags & codex_exec_mcp.WINDOWS_DETACHED_PROCESS
    assert flags & 0x01000000  # CREATE_BREAKAWAY_FROM_JOB
    assert flags & codex_exec_mcp.WINDOWS_CREATE_NEW_PROCESS_GROUP


def test_new_call_preserves_stdin_and_hook_result(client, area):
    cwd = area / "working"
    cwd.mkdir()
    prompt = 'Hello "world" & %\nİstanbul Türkçe\n'
    instructions = 'Say "merhaba"\nÇalışma yönergesi'
    result = call(client, 1, "codex", {
        "prompt": prompt, "model": "gpt-test", "cwd": str(cwd),
        "config": {"model_reasoning_effort": "high"},
        "developer-instructions": instructions,
    })
    assert result["isError"] is False
    assert codex._extract_codex_result(result) == ("thread-123", "Reply from fake Codex")
    assert result["structuredContent"] == {"threadId": "thread-123", "content": "Reply from fake Codex"}
    invocation = client.calls()[0]
    args = invocation["argv"]
    assert args[:1] == ["exec"]
    assert args[args.index("-m") + 1] == "gpt-test"
    assert "model_reasoning_effort=\"high\"" in args
    actual_instructions = next(arg for arg in args if arg.startswith("developer_instructions="))
    expected_instructions = f"developer_instructions={codex_exec_mcp._toml(instructions)}"
    assert actual_instructions == expected_instructions, (ascii(actual_instructions), ascii(expected_instructions))
    assert args[args.index("-C") + 1] == str(cwd)
    assert "--json" in args and "--skip-git-repo-check" in args and "-o" in args
    assert args[-1] == "-"
    assert bytes.fromhex(invocation["stdin_hex"]) == prompt.encode("utf-8")


def test_reply_uses_recorded_cwd_and_overrides(client, area):
    cwd = area / "reply-cwd"
    cwd.mkdir()
    first = call(client, 1, "codex", {"prompt": "first", "cwd": str(cwd), "model": "gpt-test", "config": {"nested": {"value": 3, "flag": True}}})
    assert not first["isError"]
    second = call(client, 2, "codex-reply", {"threadId": "thread-123", "prompt": "second"})
    assert not second["isError"]
    invocation = client.calls()[1]
    assert invocation["cwd"] == str(cwd)
    assert invocation["argv"][:2] == ["exec", "resume"]
    assert invocation["argv"][-2:] == ["thread-123", "-"]
    assert invocation["argv"][invocation["argv"].index("-m") + 1] == "gpt-test"
    assert "nested.value=3" in invocation["argv"]
    assert "nested.flag=true" in invocation["argv"]
    assert "-C" not in invocation["argv"] and "-s" not in invocation["argv"]


def test_output_limit_passed_on_fresh_call_but_not_reply(client, area):
    call(client, 1, "codex", {"prompt": "first", "cwd": str(area),
                              "config": {"tool_output_token_limit": 8000}})
    call(client, 2, "codex-reply", {"threadId": "thread-123", "prompt": "again"})
    first, second = client.calls()
    assert "tool_output_token_limit=8000" in first["argv"]
    assert not any(arg.startswith("tool_output_token_limit=") for arg in second["argv"])
    call(client, 3, "codex", {"prompt": "without", "cwd": str(area)})
    assert not any(arg.startswith("tool_output_token_limit=") for arg in client.calls()[2]["argv"])


def test_read_only_sandbox_persists_on_reply(client, area):
    call(client, 1, "codex", {"prompt": "plan", "cwd": str(area), "sandbox": "read-only"})
    call(client, 2, "codex-reply", {"threadId": "thread-123", "prompt": "continue"})
    first, second = client.calls()
    assert first["argv"][first["argv"].index("-s") + 1] == "read-only"
    assert 'sandbox_mode="read-only"' in second["argv"]


def test_unknown_thread_does_not_spawn(client):
    result = call(client, 1, "codex-reply", {"threadId": "absent", "prompt": "hello"})
    assert result["isError"] is True
    assert "unknown to this server" in json.loads(result["content"][0]["text"])["error"]
    assert client.calls() == []


def test_failed_turn_passes_quota_message(area):
    client = Client(area, extra_env={"FAKE_FAIL": "1"})
    try:
        result = call(client, 1, "codex", {"prompt": "run"})
        assert result["isError"] is True
        assert json.loads(result["content"][0]["text"])["error"] == "Quota exceeded: try again tomorrow"
    finally:
        client.close()


def test_cmd_without_js_entry_fails_closed(area):
    client = Client(area, bin_override=str(area / "missing.cmd"))
    try:
        result = call(client, 1, "codex", {"prompt": "run"})
        assert result["isError"] is True
        assert "JS entry is missing" in json.loads(result["content"][0]["text"])["error"]
        assert client.calls() == []
    finally:
        client.close()


def test_ping_during_slow_call(area):
    client = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        client.send(1, "tools/call", {"name": "codex", "arguments": {"prompt": "slow"}})
        deadline = time.monotonic() + 3
        while not client.calls() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert client.calls()
        client.send(2, "ping")
        assert client.receive(timeout=1) == {"jsonrpc": "2.0", "id": 2, "result": {}}
        assert client.receive(timeout=5)["id"] == 1
    finally:
        client.close()


def test_unknown_thread_mutation_is_detected(area, monkeypatch):
    """Red check: a deliberately broken guessed-cwd loader violates the guard."""
    server = codex_exec_mcp.Server(json.dumps([sys.executable, str(area / "fake.py")]))
    monkeypatch.setattr(server, "state_dir", area / "state")
    good = server._run_tool("codex-reply", {"threadId": "absent", "prompt": "hello"}, 1)
    assert "unknown to this server" in json.loads(good["content"][0]["text"])["error"]

    def guessed_record(_state, thread_id):
        return {"thread_id": thread_id, "cwd": str(area), "model": None, "config_args": []}

    monkeypatch.setattr(codex_exec_mcp, "_load_record", guessed_record)
    with pytest.raises(AssertionError, match="unknown to this server"):
        broken = server._run_tool("codex-reply", {"threadId": "absent", "prompt": "hello"}, 2)
        assert "unknown to this server" in json.loads(broken["content"][0]["text"])["error"]


def job_call(client, name, arguments):
    return call(client, uuid.uuid4().hex, name, arguments)["structuredContent"]


def finish(client, job_id, wanted):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        result = job_call(client, "codex_result", {"jobId": job_id})
        if result["state"] == wanted:
            return result
        time.sleep(0.02)
    pytest.fail(f"expected {wanted}, got {result}")


def kill_bridge_tree(client):
    if os.name == "nt":
        killed = subprocess.run(["taskkill", "/T", "/F", "/PID", str(client.process.pid)],
                                capture_output=True, text=True, check=False)
        if killed.returncode != 0:
            # The restricted test runner denies taskkill; direct kill still
            # tests bridge loss. The parent check below proves host ancestry.
            assert "Access denied" in killed.stderr
            client.process.kill()
    else:
        os.killpg(client.process.pid, 9)
    client.process.wait(timeout=5)


def windows_parent_pid(pid):
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)

    class ENTRY(ctypes.Structure):
        _fields_ = [("dwSize", wintypes.DWORD), ("cntUsage", wintypes.DWORD),
                    ("th32ProcessID", wintypes.DWORD), ("th32DefaultHeapID", ctypes.c_void_p),
                    ("th32ModuleID", wintypes.DWORD), ("cntThreads", wintypes.DWORD),
                    ("th32ParentProcessID", wintypes.DWORD), ("pcPriClassBase", ctypes.c_long),
                    ("dwFlags", wintypes.DWORD), ("szExeFile", ctypes.c_wchar * 260)]

    kernel.CreateToolhelp32Snapshot.restype = wintypes.HANDLE
    kernel.Process32FirstW.argtypes = [wintypes.HANDLE, ctypes.POINTER(ENTRY)]
    kernel.Process32NextW.argtypes = [wintypes.HANDLE, ctypes.POINTER(ENTRY)]
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    snapshot = kernel.CreateToolhelp32Snapshot(2, 0)
    assert snapshot != wintypes.HANDLE(-1).value
    try:
        entry = ENTRY()
        entry.dwSize = ctypes.sizeof(entry)
        found = kernel.Process32FirstW(snapshot, ctypes.byref(entry))
        while found:
            if entry.th32ProcessID == pid:
                return entry.th32ParentProcessID
            found = kernel.Process32NextW(snapshot, ctypes.byref(entry))
    finally:
        kernel.CloseHandle(snapshot)
    pytest.fail(f"process {pid} not found")


def windows_process_exited(pid):
    from ctypes import wintypes
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.OpenProcess.restype = wintypes.HANDLE
    kernel.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
    kernel.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
    kernel.CloseHandle.argtypes = [wintypes.HANDLE]
    handle = kernel.OpenProcess(0x00100000, False, pid)  # SYNCHRONIZE
    if not handle:
        return True
    try:
        return kernel.WaitForSingleObject(handle, 0) == 0
    finally:
        kernel.CloseHandle(handle)


def test_async_start_result_idempotency_and_sync(area):
    client = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        key = str(uuid.uuid4())
        started = job_call(client, "codex_start", {"call": key, "prompt": "slow",
                                                  "taskId": "bridge-task", "route": "subtle",
                                                  "etaMinutes": 7})
        assert started["state"] == "RUNNING" and started["acceptedAt"] < started["deadlineAt"]
        assert job_call(client, "codex_result", {"jobId": started["jobId"]})["state"] == "RUNNING"
        assert job_call(client, "codex_start", {"call": key, "prompt": "slow"})["jobId"] == started["jobId"]
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        assert (record["taskId"], record["route"], record["etaMinutes"]) == (
            "bridge-task", "subtle", 7)
        with socket.create_connection(("127.0.0.1", record["signalPort"]), timeout=4) as signal_connection:
            signal_connection.sendall(f"{started['jobId']}:{record['signalToken']}".encode("ascii"))
            assert signal_connection.recv(1) == b"1"
        result = finish(client, started["jobId"], "SUCCEEDED")
        assert (result["threadId"], result["content"]) == ("thread-123", "Reply from fake Codex")
        assert len(client.calls()) == 1
        assert codex._extract_codex_result(call(client, 4, "codex", {"prompt": "sync"})) == ("thread-123", "Reply from fake Codex")
    finally:
        client.close()


def test_thread_identity_and_config_are_durable_before_exit(area):
    release = area / "release-codex"
    client = Client(area, extra_env={"FAKE_RELEASE_AFTER_START": str(release)})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                   "config": {"model_reasoning_effort": "high"}})
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            record = read_job_record(path)
            thread = codex_exec_mcp._load_record(area / "state", "thread-123")
            if record.get("threadId") == "thread-123" and thread is not None:
                break
            time.sleep(0.05)
        assert record["state"] == "RUNNING"
        assert record["threadId"] == "thread-123"
        assert 'model_reasoning_effort="high"' in record["threadConfig"]["config_args"]
        assert 'model_reasoning_effort="high"' in thread["config_args"]
        release.touch()
        finish(client, started["jobId"], "SUCCEEDED")
    finally:
        release.touch()
        client.close()


def test_async_failure_timeout_cancel(area):
    failed = Client(area, extra_env={"FAKE_FAIL": "1"})
    try:
        job = job_call(failed, "codex_start", {"call": str(uuid.uuid4()), "prompt": "fail"})
        result = finish(failed, job["jobId"], "FAILED")
        assert result["errorCode"] == "CODEX_FAILED" and len(result["message"]) <= 200
    finally:
        failed.close()
    slow = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        job = job_call(slow, "codex_start", {"call": str(uuid.uuid4()), "prompt": "timeout", "timeoutSeconds": 0.1})
        assert finish(slow, job["jobId"], "TIMED_OUT")["errorCode"] == "DEADLINE_EXCEEDED"
        job = job_call(slow, "codex_start", {"call": str(uuid.uuid4()), "prompt": "cancel"})
        assert job_call(slow, "codex_cancel", {"jobId": job["jobId"]})["state"] == "CANCELLED"
        assert finish(slow, job["jobId"], "CANCELLED")["errorCode"] == "CANCELLED"
    finally:
        slow.close()


def test_async_local_cancel_signal(area):
    client = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        job = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "cancel"})
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"))
        with socket.create_connection(("127.0.0.1", record["signalPort"]), timeout=4) as connection:
            connection.sendall(f"{job['jobId']}:{record['signalToken']}:CANCEL".encode("ascii"))
            assert connection.recv(1) == b"1"
        assert finish(client, job["jobId"], "CANCELLED")["errorCode"] == "CANCELLED"
    finally:
        client.close()


def test_async_reply_and_restart_reconciliation(area):
    client = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        job = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "first"})
        finish(client, job["jobId"], "SUCCEEDED")
        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply", "threadId": "thread-123", "prompt": "second"})
        assert finish(client, reply["jobId"], "SUCCEEDED")["threadId"] == "thread-123"
        assert client.calls()[1]["argv"][:2] == ["exec", "resume"]
        pending = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "pending"})
        kill_bridge_tree(client)
        restarted = Client(area)
        try:
            assert job_call(restarted, "codex_result", {"jobId": pending["jobId"]})["state"] == "RUNNING"
            assert finish(restarted, pending["jobId"], "SUCCEEDED")["content"] == "Reply from fake Codex"
        finally:
            restarted.close()
    finally:
        if client.process.poll() is None:
            client.close()


def test_async_host_ack_duplicate_restart_and_payload_removal(area):
    first = Client(area, extra_env={"FAKE_SLOW": "1", "FAKE_SLEEP_SECONDS": "4"})
    key = str(uuid.uuid4())
    try:
        started = job_call(first, "codex_start", {"call": key, "prompt": "private slow prompt"})
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        record = read_job_record(path)
        assert record["state"] == "RUNNING" and record["hostPid"] > 0
        if os.name == "nt":
            assert windows_parent_pid(record["hostPid"]) != first.process.pid
        launch = path.parent / (started["jobId"] + ".launch")
        assert launch.exists()
        assert "private slow prompt" in launch.read_text(encoding="utf-8")
        assert "private slow prompt" not in codex_exec_mcp.purser_state_root.retry_permission_error(
            lambda: path.read_text(encoding="utf-8"))
        kill_bridge_tree(first)
        second = Client(area)
        try:
            assert job_call(second, "codex_result", {"jobId": started["jobId"]})["state"] == "RUNNING"
            duplicate = job_call(second, "codex_start", {"call": key, "prompt": "private slow prompt"})
            assert duplicate["jobId"] == started["jobId"]
            kill_bridge_tree(second)
            third = Client(area)
            try:
                assert job_call(third, "codex_result", {"jobId": started["jobId"]})["state"] == "RUNNING"
                result = finish(third, started["jobId"], "SUCCEEDED")
                assert not launch.exists()
                assert (result["threadId"], result["content"]) == ("thread-123", "Reply from fake Codex")
                assert len(third.calls()) == 1
            finally:
                third.close()
        finally:
            if second.process.poll() is None:
                second.close()
    finally:
        if first.process.poll() is None:
            first.close()


def test_async_cancel_from_replacement_bridge_confirms_child_exit(area):
    first = Client(area, extra_env={"FAKE_SLOW": "1", "FAKE_SLEEP_SECONDS": "20"})
    try:
        started = job_call(first, "codex_start", {"call": str(uuid.uuid4()), "prompt": "cancel after restart"})
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        deadline = time.monotonic() + 3
        record = read_job_record(path)
        while "pid" not in record and time.monotonic() < deadline:
            time.sleep(0.01)
            record = read_job_record(path)
        assert record["pid"] > 0
        kill_bridge_tree(first)

        second = Client(area)
        try:
            result = job_call(second, "codex_cancel", {"jobId": started["jobId"]})
            assert result["state"] == "CANCELLED"
            assert finish(second, started["jobId"], "CANCELLED")["errorCode"] == "CANCELLED"
            if os.name == "nt":
                assert windows_process_exited(record["pid"])
            else:
                with pytest.raises(ProcessLookupError):
                    os.kill(record["pid"], 0)
        finally:
            second.close()
    finally:
        if first.process.poll() is None:
            first.close()
        try:
            record = read_job_record(path)
            if record.get("state") not in codex_exec_mcp.TERMINAL and record.get("hostPid"):
                if os.name == "nt":
                    subprocess.run(["taskkill", "/T", "/F", "/PID", str(record["hostPid"])],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
                else:
                    os.kill(record["hostPid"], 9)
        except (OSError, ValueError, NameError):
            pass


def test_waiter_started_after_bridge_death_wakes_on_host_completion(area):
    first = Client(area, extra_env={"FAKE_SLOW": "1", "FAKE_SLEEP_SECONDS": "2"})
    try:
        started = job_call(first, "codex_start", {"call": str(uuid.uuid4()), "prompt": "finish after restart"})
        kill_bridge_tree(first)
        env = os.environ.copy()
        env["PURSER_STATE_DIR"] = str(area / "root")
        waiter = subprocess.run([sys.executable, str(ROOT / "purser_wait.py"), started["jobId"]],
                                capture_output=True, text=True, encoding="utf-8", env=env, timeout=8)
        assert waiter.returncode == 0
        assert waiter.stdout == f"purser-wait {started['jobId']} SUCCEEDED\n"
    finally:
        if first.process.poll() is None:
            first.close()


def test_async_dead_host_without_output_resumes_same_job(area):
    first = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        started = job_call(first, "codex_start", {"call": str(uuid.uuid4()), "prompt": "slow"})
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        deadline = time.monotonic() + 2
        while True:
            record = read_job_record(path)
            if "pid" in record or time.monotonic() >= deadline:
                break
            time.sleep(0.01)
        assert record["hostPid"] > 0
        assert record["pid"] > 0
        os.kill(record["hostPid"], 9)
        if os.name == "nt":
            deadline = time.monotonic() + 2
            while not windows_process_exited(record["pid"]) and time.monotonic() < deadline:
                time.sleep(0.02)
            assert windows_process_exited(record["pid"])
        else:
            os.killpg(record["pid"], 9)
        second = Client(area)
        try:
            env = os.environ.copy()
            env["PURSER_STATE_DIR"] = str(area / "root")
            waiter = subprocess.run([sys.executable, str(ROOT / "purser_wait.py"), started["jobId"]],
                                    capture_output=True, text=True, encoding="utf-8", env=env, timeout=8)
            assert waiter.returncode == 0
            assert waiter.stdout == f"purser-wait {started['jobId']} SUCCEEDED\n"
            assert read_job_record(path)["resumedAfterInterruption"] == 1
            result = job_call(second, "codex_result", {"jobId": started["jobId"]})
            assert result["state"] == "SUCCEEDED"
        finally:
            second.close()
    finally:
        first.close()
