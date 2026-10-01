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


def _remove_readonly(function, path, _error):
    os.chmod(path, 0o700)
    function(path)


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
if os.environ.get("FAKE_WRITE_FILE"):
    Path(os.environ["FAKE_WRITE_FILE"]).write_text(os.environ.get("FAKE_WRITE_CONTENT", "job change"), encoding="utf-8")
if os.environ.get("FAKE_HTTP_402") == "1":
    # Assumed provider text; the Codex JSON error envelope follows the bridge parser.
    print(json.dumps({"type": "error", "message": "HTTP 402: You exceeded your current quota; check billing details."}), flush=True)
    sys.exit(1)
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
if os.environ.get("FAKE_EVENT_GAP_SECONDS"):
    time.sleep(float(os.environ["FAKE_EVENT_GAP_SECONDS"]))
if os.environ.get("FAKE_FAIL") == "1":
    print(json.dumps({"type": "turn.failed", "error": {"message": "Quota exceeded: try again tomorrow"}}), flush=True)
    if os.environ.get("FAKE_HANG_AFTER_TERMINAL") == "1":
        time.sleep(60)
    sys.exit(2)
if os.environ.get("FAKE_HANG_BEFORE_TERMINAL") == "1":
    time.sleep(60)
message = "Reply from fake Codex"
print(json.dumps({"type": "item.completed", "item": {"type": "agent_message", "text": message}}), flush=True)
if os.environ.get("FAKE_NO_COMPLETION_EVENT") != "1":
    print(json.dumps({"type": "turn.completed", "usage": {}}), flush=True)
if os.environ.get("FAKE_NO_OUTPUT") != "1":
    Path(args[args.index("-o") + 1]).write_text(message, encoding="utf-8")
if os.environ.get("FAKE_EXIT_DELAY_AFTER_TERMINAL"):
    time.sleep(float(os.environ["FAKE_EXIT_DELAY_AFTER_TERMINAL"]))
if os.environ.get("FAKE_HANG_AFTER_TERMINAL") == "1":
    import subprocess
    child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"],
                             stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL)
    Path(os.environ["FAKE_CHILD_PID_FILE"]).write_text(str(child.pid), encoding="ascii")
    time.sleep(60)
'''


FAKE_OPENCODE = r'''
import json
import os
import sys

args = sys.argv[1:]
with open(os.environ["FAKE_RECORD"], "a", encoding="utf-8") as stream:
    stream.write(json.dumps({"argv": args, "cwd": os.getcwd()}) + "\n")
session = "ses_f1dcef146ffez46V9C0TN9cmiZ"
def emit(kind, **data):
    print(json.dumps({"type": kind, "timestamp": 1, "sessionID": session, **data}), flush=True)
if os.environ.get("FAKE_NO_SESSION") == "1":
    sys.exit(0)
if os.environ.get("FAKE_OC_HANG_BEFORE_EVENTS") == "1":
    import time
    time.sleep(60)
model = args[args.index("--model") + 1]
if model in os.environ.get("FAKE_OC_MISSING_MODELS", "").split(","):
    emit("error", error={"name": "ModelNotFoundError", "data": {
        "message": f"Model `{model}` not found", "modelID": model}})
    sys.exit(1)
# Exact nonblank lines from .tmp/oc-events-success.jsonl (OpenCode 1.18.31).
recorded = [
    '{"type":"step_start","timestamp":1790501264681,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"id":"prt_0e2311d23001zDJKOvYZwjIh1K","messageID":"msg_0e23110af001DJoi4gGP5T8WRU","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","snapshot":"4b825dc642cb6eb9a060e54bf8d69288fbee4904","type":"step-start"}}',
    '{"type":"tool_use","timestamp":1790501264895,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"type":"tool","tool":"write","callID":"call_function_p4kswnp5o4wr_1","state":{"status":"completed","input":{"filePath":"C:\\\\Projects\\\\purser\\\\.tmp\\\\oc-probe\\\\hello.txt","content":"hi\\n"},"output":"Wrote file successfully.","metadata":{"diagnostics":{},"filepath":"C:\\\\Projects\\\\purser\\\\.tmp\\\\oc-probe\\\\hello.txt","exists":false,"truncated":false},"title":"hello.txt","time":{"start":1790501264884,"end":1790501264893}},"id":"prt_0e2311d26001xs61fjZw3wbMnd","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","messageID":"msg_0e23110af001DJoi4gGP5T8WRU"}}',
    '{"type":"step_finish","timestamp":1790501265221,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"id":"prt_0e2311f40001LqLMagXeJMpI8V","reason":"tool-calls","snapshot":"7a2871192d49caaff5451df37b27afc373d8298b","messageID":"msg_0e23110af001DJoi4gGP5T8WRU","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","type":"step-finish","tokens":{"total":9959,"input":7970,"output":51,"reasoning":0,"cache":{"write":0,"read":1938}},"cost":0}}',
    '{"type":"step_start","timestamp":1790501266456,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"id":"prt_0e2312413001seTlI250dukfBW","messageID":"msg_0e23120150015B4P6wWnI2TE19","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","snapshot":"7a2871192d49caaff5451df37b27afc373d8298b","type":"step-start"}}',
    '{"type":"text","timestamp":1790501266468,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"id":"prt_0e2312416001cdRmB7ifQl7PvZ","messageID":"msg_0e23120150015B4P6wWnI2TE19","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","type":"text","text":"DONE","time":{"start":1790501266454,"end":1790501266465}}}',
    '{"type":"step_finish","timestamp":1790501266628,"sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","part":{"id":"prt_0e23124c0001zSb2nL5x72t5cn","reason":"stop","snapshot":"7a2871192d49caaff5451df37b27afc373d8298b","messageID":"msg_0e23120150015B4P6wWnI2TE19","sessionID":"ses_f1dcef146ffez46V9C0TN9cmiZ","type":"step-finish","tokens":{"total":9982,"input":22,"output":3,"reasoning":0,"cache":{"write":0,"read":9957}},"cost":0}}',
]
print(recorded[0], flush=True)
if os.environ.get("FAKE_OC_ERROR") == "1":
    emit("error", error={"name": "ProviderError", "data": {"message": "model failed"}})
    sys.exit(1)
if os.environ.get("FAKE_OC_DENIED") == "1":
    denied = json.loads(recorded[1])
    denied["part"]["state"] = {"status": "error", "error": "Permission denied"}
    print(json.dumps(denied), flush=True)
    sys.exit(0)
for line in recorded[1:-1]:
    print(line, flush=True)
if os.environ.get("FAKE_OC_NO_STOP") != "1":
    print(recorded[-1], flush=True)
if os.environ.get("FAKE_OC_HANG_AFTER_STOP") == "1":
    import time
    time.sleep(60)
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
                shutil.rmtree(path, onerror=_remove_readonly)
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


def test_queued_job_deadline_never_launches(area, monkeypatch):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = "1" * 64
    server._write_job({"jobId": job_id, "state": "QUEUED", "leaseQueued": True,
                       "acceptedAt": time.time(), "deadlineAt": time.time() + 0.05})
    monkeypatch.setattr(codex, "bind_queued_baseline", lambda *_args: False)
    monkeypatch.setattr(server, "_run_tool", lambda *_args, **_kwargs: pytest.fail("queued worker launched"))
    server._job_worker(job_id, "codex", {"leaseQueued": True})
    assert server._read_job(job_id)["state"] == "TIMED_OUT"


def test_queued_cancel_never_launches(area, monkeypatch):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = "2" * 64
    server._write_job({"jobId": job_id, "state": "QUEUED", "leaseQueued": True,
                       "acceptedAt": time.time(), "deadlineAt": time.time() + 30})
    monkeypatch.setattr(server, "_run_tool", lambda *_args, **_kwargs: pytest.fail("cancelled worker launched"))
    assert server._cancel_job({"jobId": job_id})["structuredContent"]["state"] == "CANCELLED"
    server._job_worker(job_id, "codex", {"leaseQueued": True})
    assert server._read_job(job_id)["state"] == "CANCELLED"


def test_queued_baseline_failure_is_uncertain_without_launch(area, monkeypatch):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = "3" * 64
    server._write_job({"jobId": job_id, "state": "QUEUED", "leaseQueued": True,
                       "acceptedAt": time.time(), "deadlineAt": time.time() + 30})
    monkeypatch.setattr(codex, "bind_queued_baseline", lambda *_args: (_ for _ in ()).throw(
        codex.WiringError("baseline unavailable")))
    monkeypatch.setattr(server, "_run_tool", lambda *_args, **_kwargs: pytest.fail("uncertain worker launched"))
    server._job_worker(job_id, "codex", {"leaseQueued": True})
    job = server._read_job(job_id)
    assert (job["state"], job["errorCode"]) == ("UNKNOWN_OR_IN_PROGRESS", "ADMISSION_BASELINE_UNAVAILABLE")


def test_never_launched_queue_recovery_fences_old_generation(area, monkeypatch):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = "4" * 64
    server._write_job({"jobId": job_id, "state": "QUEUED", "leaseQueued": True,
                       "generation": 1, "ownershipToken": "old",
                       "acceptedAt": time.time(), "deadlineAt": time.time() + 30})
    launched = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launched.append(args))
    assert server.recover_job(job_id) == "RECOVERED"
    job = server._read_job(job_id)
    assert job["state"] == "QUEUED" and job["generation"] == 2
    assert len(launched) == 1 and launched[0][2:] == (2, job["ownershipToken"])


def test_queued_job_with_host_pid_recovers(area, monkeypatch):
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = "5" * 64
    server._write_job({"jobId": job_id, "state": "QUEUED", "leaseQueued": True,
                       "generation": 1, "ownershipToken": "old", "hostPid": 123,
                       "hostCreatedAt": 1, "hostContained": True,
                       "acceptedAt": 1000.0, "deadlineAt": 1100.0})
    monkeypatch.setattr(server, "_classify_host", lambda _job: "DEAD")
    launched = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launched.append(args))
    assert server.recover_job(job_id, now=1001.0) == "RECOVERED"
    job = server._read_job(job_id)
    assert job["generation"] == 2 and "hostPid" not in job
    assert len(launched) == 1 and launched[0][2:] == (2, job["ownershipToken"])


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

    def receive(self, timeout=120):
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
    assert [tool["name"] for tool in tools] == ["codex", "codex-reply", "codex_start", "codex_result", "codex_cancel", "codex_apply"]
    start = next(tool for tool in tools if tool["name"] == "codex_start")
    assert "call" not in start["inputSchema"]["required"]
    apply = next(tool for tool in tools if tool["name"] == "codex_apply")
    assert apply["inputSchema"]["properties"]["resolved"]["type"] == "boolean"
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
        deadline = time.monotonic() + 10
        while not client.calls() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert client.calls()
        client.send(2, "ping")
        assert client.receive(timeout=5) == {"jsonrpc": "2.0", "id": 2, "result": {}}
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


def finish(client, job_id, wanted, *, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = job_call(client, "codex_result", {"jobId": job_id})
        if result["state"] == wanted:
            return result
        time.sleep(0.02)
    pytest.fail(f"expected {wanted}, got {result}")


def test_stream_activity_updates_and_checkpoints_are_throttled(tmp_path, monkeypatch):
    job_id = "e" * 64
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    server._write_job({"jobId": job_id, "state": "RUNNING", "generation": 2,
                       "ownershipToken": "current", "route": "implementation"})
    activity = {"route": "implementation", "agent": "codex", "lastActivityAt": 100.0,
                "processAlive": True, "processAliveAt": 100.0,
                "longestSilentGapSeconds": 0.0, "checkpointAt": 100.0}
    writes = []
    original = server._write_job
    monkeypatch.setattr(server, "_write_job", lambda job: (writes.append(job.copy()), original(job)))

    for now in (103.0, 106.0):
        codex_exec_mcp._record_activity(activity, now)
        assert not server._checkpoint_liveness(job_id, 2, "current", activity,
                                               now=now, process_alive=True)
    assert activity["lastActivityAt"] == 106.0
    assert activity["longestSilentGapSeconds"] == 3.0
    assert writes == []

    codex_exec_mcp._record_activity(activity, 161.0)
    assert server._checkpoint_liveness(job_id, 2, "current", activity,
                                        now=161.0, process_alive=True)
    assert len(writes) == 1
    saved = server._read_job(job_id)
    assert saved["activeAgent"] == "codex"
    assert saved["liveness"]["lastActivityAt"] == 161.0
    assert saved["liveness"]["processAliveAt"] == 161.0
    assert saved["liveness"]["route"] == "implementation"


def test_stall_threshold_cold_start_uses_bounded_route_eta(tmp_path):
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    assert server._stall_threshold({"route": "implementation", "activeAgent": "codex",
                                    "etaMinutes": 8}) == 16 * 60
    assert server._stall_threshold({"route": "implementation", "activeAgent": "codex",
                                    "etaMinutes": 60}) == 30 * 60
    assert server._stall_threshold({"route": "implementation", "activeAgent": "agy",
                                    "etaMinutes": 2}) == 30 * 60


def test_stall_threshold_learns_from_five_matching_successes(tmp_path):
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    for index, gap in enumerate((1000, 1100, 1200, 1300)):
        server._write_job({"jobId": str(index) * 64, "state": "SUCCEEDED",
                           "route": "implementation", "activeAgent": "codex",
                           "liveness": {"agent": "codex", "longestSilentGapSeconds": gap}})
    job = {"route": "implementation", "activeAgent": "codex", "etaMinutes": 8}
    assert server._stall_threshold(job) == 16 * 60
    server._write_job({"jobId": "5" * 64, "state": "SUCCEEDED",
                       "route": "implementation", "activeAgent": "codex",
                       "liveness": {"agent": "codex", "longestSilentGapSeconds": 1400}})
    assert server._stall_threshold(job) == 2100  # ceil(P90 * 1.5)


def test_stall_episode_survives_restart_and_clears_on_activity(tmp_path):
    job_id = "6" * 64
    server = codex_exec_mcp.Server(job_dir=tmp_path)
    server._write_job({"jobId": job_id, "state": "RUNNING", "generation": 2,
                       "ownershipToken": "current", "activeAgent": "codex"})
    activity = {"route": "implementation", "agent": "codex", "lastActivityAt": 0.0,
                "stdoutBytes": 0, "processAlive": True, "processAliveAt": 0.0,
                "longestSilentGapSeconds": 0.0, "stallThresholdSeconds": 900,
                "checkpointAt": 0.0}

    # Periodic output can sustain arbitrarily long work beyond the threshold.
    for now in (600.0, 1200.0, 1800.0, 2400.0):
        activity["stdoutBytes"] += 1
        codex_exec_mcp._record_activity(activity, now)
        server._checkpoint_liveness(job_id, 2, "current", activity,
                                    now=now, process_alive=True)
        assert server._read_job(job_id).get("errorCode") != "STALLED"

    server._checkpoint_liveness(job_id, 2, "current", activity,
                                now=3301.0, process_alive=True)
    first_marker = server._read_job(job_id)["stallDetectedAt"]
    assert server._read_job(job_id)["state"] == "RUNNING"
    assert server._read_job(job_id)["errorCode"] == "STALLED"

    restarted = codex_exec_mcp.Server(job_dir=tmp_path)
    recovered = dict(server._read_job(job_id)["liveness"], stallThresholdSeconds=900,
                     checkpointAt=3301.0)
    restarted._checkpoint_liveness(job_id, 2, "current", recovered,
                                   now=3400.0, process_alive=True)
    assert restarted._read_job(job_id)["stallDetectedAt"] == first_marker

    codex_exec_mcp._record_activity(recovered, 3401.0)
    restarted._checkpoint_liveness(job_id, 2, "current", recovered,
                                   now=3401.0, process_alive=True)
    assert "stallDetectedAt" not in restarted._read_job(job_id)
    assert "errorCode" not in restarted._read_job(job_id)

    restarted._checkpoint_liveness(job_id, 2, "current", recovered,
                                   now=4302.0, process_alive=True)
    assert restarted._read_job(job_id)["stallDetectedAt"] == 4302.0


def test_async_silent_process_gap_is_recorded(area):
    client = Client(area, extra_env={"FAKE_EVENT_GAP_SECONDS": "0.15"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "measure silence",
                                                    "route": "implementation"})
        assert finish(client, started["jobId"], "SUCCEEDED")["threadId"] == "thread-123"
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                 (started["jobId"] + ".json"))
        assert record["liveness"]["longestSilentGapSeconds"] >= 0.1
        assert record["liveness"]["stdoutBytes"] > 0
        assert record["liveness"]["processAlive"] is False
        assert record["liveness"]["route"] == "implementation"
        assert record["liveness"]["agent"] == record["activeAgent"] == "codex"
    finally:
        client.close()


def test_liveness_checkpoint_refuses_stale_generation(area):
    job_id = "f" * 64
    jobs = area / "root" / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    server._write_job({"jobId": job_id, "state": "RUNNING", "generation": 2,
                       "ownershipToken": "recovered", "liveness": {"lastActivityAt": 200.0}})
    stale_activity = {"route": "implementation", "agent": "codex", "lastActivityAt": 100.0,
                      "processAlive": True, "processAliveAt": 100.0,
                      "longestSilentGapSeconds": 0.0, "checkpointAt": 0.0}

    assert not server._checkpoint_liveness(job_id, 1, "earlier", stale_activity,
                                           now=201.0, process_alive=True, force=True)
    record = server._read_job(job_id)
    assert record["generation"] == 2
    assert record["ownershipToken"] == "recovered"
    assert record["liveness"]["lastActivityAt"] == 200.0

    record["state"] = "SUCCEEDED"
    server._write_job(record)
    assert not server._checkpoint_liveness(job_id, 2, "recovered", stale_activity,
                                           now=202.0, process_alive=False, force=True)
    assert server._read_job(job_id)["state"] == "SUCCEEDED"


def decision_events(area):
    events = []
    for path in (area / "root" / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return [event for event in events if event["event_type"] == "CLAUDE_DECISION_RECORDED"]


def wait_for_path(path, exists, description, timeout=30):
    deadline = time.monotonic() + timeout
    while path.exists() != exists and time.monotonic() < deadline:
        time.sleep(0.02)
    assert path.exists() == exists, f"timed out waiting for {description}: {path}"


def _git_repo(area):
    repo = area / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "Test"], check=True)
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "base"], check=True)
    return repo


def test_worktree_job_records_snapshot_and_reply_reuses_it(area):
    repo = _git_repo(area)
    client = Client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        result = finish(client, started["jobId"], "SUCCEEDED")
        worktree = result["worktreePath"]
        assert result["threadId"] == "thread-123"
        assert Path(worktree).is_dir() and Path(worktree).parent == area / "root" / "wt"
        assert json.loads((area / "calls.jsonl").read_text(encoding="utf-8").splitlines()[0])["cwd"] == worktree
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        assert (job["worktreePath"], job["snapshot"], job["head"], job["repoRoot"]) == (
            worktree, job["snapshot"], job["head"], str(repo.resolve()))
        assert job["snapshot"] and job["head"]
        thread = codex_exec_mcp._load_record(area / "state", "thread-123")
        assert {key: thread[key] for key in ("worktreePath", "snapshot", "head", "repoRoot")} == {
            key: job[key] for key in ("worktreePath", "snapshot", "head", "repoRoot")}
        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": "thread-123", "prompt": "again"})
        assert finish(client, reply["jobId"], "SUCCEEDED")["worktreePath"] == worktree
        calls = [json.loads(line) for line in (area / "calls.jsonl").read_text(encoding="utf-8").splitlines()]
        assert [call["cwd"] for call in calls] == [worktree, worktree]
        shutil.rmtree(worktree)
        missing = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                    "threadId": "thread-123", "prompt": "missing"})
        assert finish(client, missing["jobId"], "FAILED")["errorCode"] == "WORKTREE_MISSING"
        assert len((area / "calls.jsonl").read_text(encoding="utf-8").splitlines()) == 2
    finally:
        client.close()


def test_reply_worktree_reuses_thread_baseline_for_full_delta(area):
    import ledger

    repo = _git_repo(area)
    tracked = repo / "tracked.txt"
    baseline = "first\nc1\nc2\nc3\nc4\nc5\nsecond\nc6\nc7\nc8\nc9\nc10\nthird\n"
    first_content = baseline.replace("first\n", "first A\n")
    tracked.write_text(baseline, encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "set fixture"], check=True)
    client = Client(area)
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "first",
                                                  "cwd": str(repo), "worktree": True})
        first_result = finish(client, first["jobId"], "SUCCEEDED")
        worktree = Path(first_result["worktreePath"])
        (worktree / "tracked.txt").write_text(first_content, encoding="utf-8")
        first_record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                       (first["jobId"] + ".json"))

        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": "thread-123", "prompt": "reply"})
        finish(client, reply["jobId"], "SUCCEEDED")
        reply_record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                       (reply["jobId"] + ".json"))
        assert reply_record["snapshot"] == first_record["snapshot"]
        reply_content = first_content.replace("first A\n", "first A reply\n").replace(
            "second\n", "second B\n")
        (worktree / "tracked.txt").write_text(reply_content, encoding="utf-8")
        tracked.write_text(baseline.replace("third\n", "third unrelated\n"), encoding="utf-8")
        subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
        subprocess.run(["git", "-C", str(repo), "commit", "-qm", "unrelated change"], check=True)
        prepared = ledger.prepare_job_replay(repo, worktree, reply_record["snapshot"],
                                             area / "reply-preparation")
        assert prepared["status"] == "prepared"
        assert (worktree / "tracked.txt").read_text(encoding="utf-8") == reply_content.replace(
            "third\n", "third unrelated\n")
        reply_record["snapshot"] = prepared["snapshot"]
        (area / "root" / "codex_exec_mcp" / "jobs" / (reply["jobId"] + ".json")).write_text(
            json.dumps(reply_record), encoding="utf-8")
        applied = job_call(client, "codex_apply", {"jobId": reply["jobId"]})
        assert applied == {"applied": True, "paths": ["tracked.txt"]}
        assert tracked.read_text(encoding="utf-8") == reply_content.replace(
            "third\n", "third unrelated\n")
    finally:
        client.close()


def test_original_worktree_job_applies_combined_reply_delta(area):
    import ledger

    repo = _git_repo(area)
    tracked = repo / "tracked.txt"
    baseline = "first\nc1\nc2\nc3\nc4\nc5\nsecond\nc6\nc7\nc8\nc9\nc10\nthird\n"
    tracked.write_text(baseline, encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "set fixture"], check=True)
    client = Client(area)
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "first",
                                                  "cwd": str(repo), "worktree": True})
        finish(client, first["jobId"], "SUCCEEDED")
        worktree = Path(read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                        (first["jobId"] + ".json"))["worktreePath"])
        (worktree / "tracked.txt").write_text(baseline.replace("first\n", "first A\n"), encoding="utf-8")
        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": "thread-123", "prompt": "reply"})
        finish(client, reply["jobId"], "SUCCEEDED")
        combined = baseline.replace("first\n", "first A reply\n").replace("second\n", "second B\n")
        (worktree / "tracked.txt").write_text(combined, encoding="utf-8")

        applied = job_call(client, "codex_apply", {"jobId": first["jobId"]})

        assert applied == {"applied": True, "paths": ["tracked.txt"]}
        assert tracked.read_text(encoding="utf-8") == combined
    finally:
        client.close()


def test_reply_replay_reports_conflict_against_main_same_line_change(area):
    import ledger

    repo = _git_repo(area)
    tracked = repo / "tracked.txt"
    baseline = "first\nc1\nc2\nc3\nc4\nc5\nsecond\nc6\nc7\nc8\nc9\nc10\nthird\n"
    tracked.write_text(baseline, encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "set fixture"], check=True)
    client = Client(area)
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "first",
                                                  "cwd": str(repo), "worktree": True})
        finish(client, first["jobId"], "SUCCEEDED")
        first_record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                       (first["jobId"] + ".json"))
        worktree = Path(first_record["worktreePath"])
        (worktree / "tracked.txt").write_text(baseline.replace("first\n", "first worker\n"),
                                               encoding="utf-8")
        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": "thread-123", "prompt": "reply"})
        finish(client, reply["jobId"], "SUCCEEDED")
        (worktree / "tracked.txt").write_text(baseline.replace("first\n", "first worker reply\n"),
                                               encoding="utf-8")
        tracked.write_text(baseline.replace("first\n", "first main\n"), encoding="utf-8")
        subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
        subprocess.run(["git", "-C", str(repo), "commit", "-qm", "main conflict"], check=True)

        prepared = ledger.prepare_job_replay(repo, worktree, first_record["snapshot"],
                                             area / "conflicting-preparation")

        assert prepared["status"] == "conflict"
        assert prepared["paths"] == ["tracked.txt"]
        next_reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                       "threadId": "thread-123", "prompt": "retry"})
        assert next_reply.get("jobId")
    finally:
        client.close()


def test_worktree_job_falls_back_when_requested_cwd_is_missing(area):
    repo = _git_repo(area)
    (repo / ".gitignore").write_text(".tmp/\n", encoding="utf-8")
    requested_cwd = repo / ".tmp" / "opencode-probe"
    requested_cwd.mkdir(parents=True)
    subprocess.run(["git", "-C", str(repo), "add", ".gitignore"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "ignore scratch"], check=True)
    client = Client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(requested_cwd), "worktree": True})
        result = finish(client, started["jobId"], "SUCCEEDED")
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        call_record = client.calls()[0]
        assert call_record["cwd"] == result["worktreePath"]
        assert job["cwdFallback"] == os.fspath(Path(".tmp") / "opencode-probe")
    finally:
        client.close()


def test_worktree_job_keeps_existing_relative_cwd(area):
    repo = _git_repo(area)
    requested_cwd = repo / "nested"
    requested_cwd.mkdir()
    (requested_cwd / "tracked.txt").write_text("nested\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "nested/tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "nested cwd"], check=True)
    client = Client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(requested_cwd), "worktree": True,
                                                    "config": {"sandbox_workspace_write": {
                                                        "writable_roots": [str(repo.parent)]}}})
        result = finish(client, started["jobId"], "SUCCEEDED")
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        call_record = client.calls()[0]
        worktree = Path(result["worktreePath"]).resolve()
        assert call_record["cwd"] == str(worktree / "nested")
        argv = call_record["argv"]
        assert argv[argv.index("-C") + 1] == str(worktree / "nested")
        config_args = [argv[index + 1] for index, value in enumerate(argv[:-1]) if value == "-c"]
        assert (f"sandbox_workspace_write.writable_roots="
                f"{codex_exec_mcp._toml([str(worktree)])}") in config_args
        assert all(str(repo.parent) not in value for value in config_args)
        assert "cwdFallback" not in job
    finally:
        client.close()


def test_worktree_snapshot_failure_does_not_launch_worker(area):
    client = Client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "fail",
                                                    "cwd": str(area / "not-a-repo"), "worktree": True})
        result = finish(client, started["jobId"], "FAILED")
        assert result["errorCode"] == "WORKTREE_SNAPSHOT_FAILED"
        assert not (area / "calls.jsonl").exists()
    finally:
        client.close()


def _prepare_host_worktree_job(area, worktree_path):
    job_id = "a" * 64
    job_dir = area / "jobs"
    job_dir.mkdir()
    server = codex_exec_mcp.Server(host=True, job_dir=job_dir)
    server._write_job({"jobId": job_id, "state": "QUEUED", "generation": 2,
                       "ownershipToken": "owner", "worktreePath": str(worktree_path),
                       "snapshot": "saved-snapshot", "head": "saved-head",
                       "repoRoot": "saved-repo-root"})
    (job_dir / f"{job_id}.launch").write_text(json.dumps({
        "name": "codex", "codexBin": "codex", "args": {"cwd": str(area), "worktree": True}
    }), encoding="utf-8")
    return job_id, job_dir


def _run_host_without_child_process(monkeypatch, job_id, job_dir, worker_args):
    monkeypatch.setattr(codex_exec_mcp, "_contain_host_children", lambda: True)
    if os.name == "nt":
        monkeypatch.setattr(codex_exec_mcp, "_windows_process_identity",
                            lambda _pid: ("ALIVE", 1))
    monkeypatch.setattr(codex_exec_mcp.Server, "_job_worker",
                        lambda _self, _job_id, _name, args: worker_args.append(args))
    codex_exec_mcp._host_main(job_id, job_dir, 2, "owner")


def test_relaunched_worktree_job_reuses_recorded_worktree(area, monkeypatch):
    worktree = area / "existing-worktree"
    worktree.mkdir()
    job_id, job_dir = _prepare_host_worktree_job(area, worktree)
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root",
                        lambda: area / "root")
    worker_args = []

    _run_host_without_child_process(monkeypatch, job_id, job_dir, worker_args)

    job = read_job_record(job_dir / f"{job_id}.json")
    assert worker_args[0]["cwd"] == str(worktree)
    assert {key: job[key] for key in ("worktreePath", "snapshot", "head", "repoRoot")} == {
        "worktreePath": str(worktree), "snapshot": "saved-snapshot",
        "head": "saved-head", "repoRoot": "saved-repo-root"}


def test_relaunched_worktree_job_with_missing_directory_fails(area, monkeypatch):
    missing_worktree = area / "missing-worktree"
    job_id, job_dir = _prepare_host_worktree_job(area, missing_worktree)
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root",
                        lambda: area / "root")
    worker_args = []

    _run_host_without_child_process(monkeypatch, job_id, job_dir, worker_args)

    job = read_job_record(job_dir / f"{job_id}.json")
    assert job["state"] == "FAILED"
    assert job["errorCode"] == "WORKTREE_MISSING"
    assert not worker_args


def test_worktree_setup_failure_is_recorded_and_does_not_launch_worker(area, monkeypatch):
    import ledger

    repo = _git_repo(area)
    job_id, job_dir = _prepare_host_worktree_job(area, repo)
    job_path = job_dir / f"{job_id}.json"
    job = read_job_record(job_path)
    for key in ("worktreePath", "snapshot", "head", "repoRoot"):
        job.pop(key)
    codex_exec_mcp.Server(host=True, job_dir=job_dir)._write_job(job)
    (job_dir / f"{job_id}.launch").write_text(json.dumps({
        "name": "codex", "codexBin": "codex", "args": {"cwd": str(repo), "worktree": True}
    }), encoding="utf-8")
    worktree = area / "root" / "wt" / job_id[:12]
    output = "setup failed after installing dependencies"

    def fail_setup(repo_root, requested_job_id, parent):
        assert Path(repo_root) == repo.resolve()
        assert requested_job_id == job_id
        assert Path(parent) == area / "root" / "wt"
        worktree.mkdir(parents=True)
        raise ledger.WorktreeSetupError(17, output)

    monkeypatch.setattr(ledger, "create_job_worktree", fail_setup)
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root",
                        lambda: area / "root")
    worker_args = []
    _run_host_without_child_process(monkeypatch, job_id, job_dir, worker_args)

    assert not (job_dir / f"{job_id}.host-error").exists(), (job_dir / f"{job_id}.host-error").read_text()
    result = read_job_record(job_path)
    assert result["state"] == "FAILED"
    assert result["errorCode"] == "WORKTREE_SETUP_FAILED"
    assert "17" in result["message"] and output in result["message"]
    assert result["worktreePath"] == str(worktree)
    assert result["repoRoot"] == str(repo.resolve())
    assert result["setupError"] == {"exitCode": 17, "timedOut": False, "output": output}
    assert worktree.is_dir()
    assert worker_args == []


def test_codex_apply_is_idempotent_and_reports_conflicts(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        result = finish(client, started["jobId"], "SUCCEEDED")
        first = job_call(client, "codex_apply", {"jobId": started["jobId"]})
        second = job_call(client, "codex_apply", {"jobId": started["jobId"]})
        assert first == {"applied": True, "paths": ["tracked.txt"]}
        assert second == first
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        assert job["applyState"] == "applied"
        assert Path(result["worktreePath"]).is_dir()
        assert (repo / "tracked.txt").read_text(encoding="utf-8") == "job\n"

        (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
        other = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                  "cwd": str(repo), "worktree": True})
        other_result = finish(client, other["jobId"], "SUCCEEDED")
        (repo / "tracked.txt").write_text("repo concurrent change\n", encoding="utf-8")
        conflict = job_call(client, "codex_apply", {"jobId": other["jobId"]})
        assert conflict == {"applied": False, "conflicts": ["tracked.txt"],
                            "applicable": [],
                            "worktreePath": other_result["worktreePath"]}
        conflict_job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (other["jobId"] + ".json"))
        assert conflict_job["applyState"] == "conflict"
        lease_path = (area / "root" / "projects" /
                      codex_exec_mcp.project_root.project_key(repo.resolve()) / "scope_leases.json")
        learned = json.loads(lease_path.read_text(encoding="utf-8"))["conflicts"]
        assert [item["path"] for item in learned] == ["tracked.txt"]
        assert isinstance(learned[0]["recorded_at"], (int, float))
        assert Path(other_result["worktreePath"]).is_dir()
    finally:
        client.close()


def test_apply_and_reply_emit_decisions_but_conflict_does_not(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "work", "cwd": str(repo), "worktree": True,
            "taskId": "telemetry-task", "jobPhase": "implementation",
        })
        finished = finish(client, started["jobId"], "SUCCEEDED")
        reply = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "repair", "mode": "reply",
            "threadId": finished["threadId"], "taskId": "telemetry-task",
            "jobPhase": "implementation",
        })
        events = decision_events(area)
        assert [(event["payload"]["decision"], event["payload"]["source"])
                for event in events] == [("send_back", {"kind": "reply", "job_id": reply["jobId"]})]

        finish(client, reply["jobId"], "SUCCEEDED")
        applied = job_call(client, "codex_apply", {"jobId": reply["jobId"]})
        assert applied["applied"] is True
        accepted = decision_events(area)
        assert [(event["payload"]["decision"], event["payload"]["source"])
                for event in accepted] == [
                    ("send_back", {"kind": "reply", "job_id": reply["jobId"]}),
                    ("accepted", {"kind": "apply", "job_id": reply["jobId"]}),
                ]

        (repo / "tracked.txt").write_text("concurrent\n", encoding="utf-8")
        conflict = job_call(client, "codex_apply", {"jobId": started["jobId"]})
        assert conflict["applied"] is False
        assert len(decision_events(area)) == 2
    finally:
        client.close()


def test_result_records_non_worktree_acceptance_once(area):
    client = Client(area)
    try:
        started = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "operation", "taskId": "operation-result",
            "jobPhase": "operation",
        })
        finish(client, started["jobId"], "SUCCEEDED")
        for _ in range(2):
            result = job_call(client, "codex_result", {"jobId": started["jobId"]})
            assert result["state"] == "SUCCEEDED"
        event, = decision_events(area)
        assert (event["payload"]["decision"], event["payload"]["source"]) == (
            "accepted", {"kind": "result", "job_id": started["jobId"]})
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                              (started["jobId"] + ".json"))
        assert job["decisionRecorded"] is True
    finally:
        client.close()


def test_new_task_redispatch_rejects_prior_unapplied_worktree_job(area):
    repo = _git_repo(area)
    client = Client(area)
    try:
        earlier = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "first", "cwd": str(repo), "worktree": True,
            "taskId": "redispatch-task", "jobPhase": "implementation",
        })
        finish(client, earlier["jobId"], "SUCCEEDED")
        newer = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "redispatched", "cwd": str(repo),
            "taskId": "redispatch-task", "jobPhase": "implementation",
        })
        event, = decision_events(area)
        assert (event["payload"]["decision"], event["payload"]["source"]) == (
            "rejected", {"kind": "redispatch", "job_id": newer["jobId"]})
        old_job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                  (earlier["jobId"] + ".json"))
        assert old_job["decisionRecorded"] is True
        finish(client, newer["jobId"], "SUCCEEDED")
    finally:
        client.close()


def test_redispatch_does_not_reject_applied_job(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        earlier = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "first", "cwd": str(repo), "worktree": True,
            "taskId": "applied-redispatch-task", "jobPhase": "implementation",
        })
        finish(client, earlier["jobId"], "SUCCEEDED")
        assert job_call(client, "codex_apply", {"jobId": earlier["jobId"]})["applied"] is True
        newer = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "redispatched", "cwd": str(repo),
            "taskId": "applied-redispatch-task", "jobPhase": "implementation",
        })
        events = decision_events(area)
        assert [(event["payload"]["decision"], event["payload"]["source"])
                for event in events] == [
                    ("accepted", {"kind": "apply", "job_id": earlier["jobId"]})]
        old_job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                  (earlier["jobId"] + ".json"))
        assert old_job["decisionRecorded"] is True
        finish(client, newer["jobId"], "SUCCEEDED")
    finally:
        client.close()


def test_terminal_unapplied_cancel_emits_rejected(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "work", "cwd": str(repo), "worktree": True,
            "taskId": "cancel-task", "jobPhase": "operation",
        })
        finish(client, started["jobId"], "SUCCEEDED")
        job_call(client, "codex_cancel", {"jobId": started["jobId"]})
        event, = decision_events(area)
        assert event["payload"]["decision"] == "rejected"
        assert event["payload"]["task_type"] == "operation"
        assert event["payload"]["source"] == {"kind": "discard", "job_id": started["jobId"]}
    finally:
        client.close()


def test_apply_telemetry_failure_does_not_break_apply(area, monkeypatch):
    import telemetry

    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {
            "call": str(uuid.uuid4()), "prompt": "work", "cwd": str(repo), "worktree": True,
            "taskId": "fail-open-task", "jobPhase": "implementation",
        })
        finish(client, started["jobId"], "SUCCEEDED")
        monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: area / "root")
        monkeypatch.setattr(telemetry, "emit", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("telemetry down")))
        server = codex_exec_mcp.Server(job_dir=area / "root" / "codex_exec_mcp" / "jobs")
        result = server._apply_job({"jobId": started["jobId"]})
        assert result["structuredContent"]["applied"] is True
    finally:
        client.close()


def test_codex_apply_refuses_stale_delta_and_reprepares_for_review(area):
    repo = _git_repo(area)
    (repo / "tracked.txt").write_text("one\ntwo\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "two lines"], check=True)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt",
                                     "FAKE_WRITE_CONTENT": "one\ntwo\njob\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        job = read_job_record(path)
        reviewed_snapshot = job["snapshot"]
        job["reviewedSnapshot"] = job["snapshot"]
        codex_exec_mcp.Server(host=True, job_dir=path.parent)._write_job(job)
        (area / "root" / "async_jobs").mkdir(parents=True, exist_ok=True)
        (area / "root" / "async_jobs" / f"{started['jobId']}.json").write_text(json.dumps({
            "jobId": started["jobId"], "call_key": "lease-owner", "project_key": "sample",
            "stage": "CONSUMED",
        }), encoding="utf-8")
        (repo / "tracked.txt").write_text("one\ncheckout\ntwo\n", encoding="utf-8")

        refused = job_call(client, "codex_apply", {"jobId": started["jobId"]})

        assert refused["errorCode"] == "REVIEW_STALE_REPREPARE_REQUIRED"
        updated = read_job_record(path)
        assert updated["snapshot"] == reviewed_snapshot
        assert updated["reviewRefreshPending"] is True
        assert updated["snapshotRevision"] == 1
        index = json.loads((area / "root" / "async_jobs" / f"{started['jobId']}.json").read_text())
        assert index["stage"] == "REVIEW_REFRESH"
        assert job_call(client, "codex_result", {"jobId": started["jobId"]})["state"] == "SUCCEEDED"
    finally:
        client.close()


def test_codex_apply_allows_drift_outside_job_delta(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        job = read_job_record(path)
        job["reviewedSnapshot"] = job["snapshot"]
        codex_exec_mcp.Server(host=True, job_dir=path.parent)._write_job(job)
        import ledger
        lease = ledger.ScopeLeases(area / "root" / "projects" / "sample")
        lease.acquire("lease-owner", "task", ["tracked.txt"], job_id=started["jobId"], allow_overlap=True)
        (area / "root" / "async_jobs").mkdir(parents=True, exist_ok=True)
        (area / "root" / "codex_exec_mcp" / "jobs").mkdir(parents=True, exist_ok=True)
        (area / "root" / "async_jobs" / f"{started['jobId']}.json").write_text(json.dumps({
            "jobId": started["jobId"], "call_key": "lease-owner", "project_key": "sample",
            "stage": "CONSUMED",
        }), encoding="utf-8")
        reply_id = "b" * 64
        reply_owner = "reply-lease"
        lease.acquire(reply_owner, "reply", ["tracked.txt"], job_id=reply_id, allow_overlap=True)
        (area / "root" / "codex_exec_mcp" / "jobs" / f"{reply_id}.json").write_text(json.dumps({
            "jobId": reply_id, "worktreePath": job["worktreePath"],
        }), encoding="utf-8")
        (area / "root" / "async_jobs" / f"{reply_id}.json").write_text(json.dumps({
            "jobId": reply_id, "call_key": reply_owner, "project_key": "sample",
            "stage": "CONSUMED",
        }), encoding="utf-8")
        (repo / "unrelated.txt").write_text("outside delta\n", encoding="utf-8")

        applied = job_call(client, "codex_apply", {"jobId": started["jobId"]})

        assert applied == {"applied": True, "paths": ["tracked.txt"]}
        assert (repo / "tracked.txt").read_text(encoding="utf-8") == "job\n"
        assert lease.owner_state("lease-owner") == "RELEASED"
        assert lease.owner_state(reply_owner) == "RELEASED"
    finally:
        client.close()


def test_codex_cancel_releases_unapplied_finished_worktree_lease(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        project = codex_exec_mcp.project_root.project_key(repo.resolve())
        lease = __import__("ledger").ScopeLeases(area / "root" / "projects" / project)
        owner = "cancel-owner"
        lease.acquire(owner, "task", ["tracked.txt"], job_id=started["jobId"], allow_overlap=True)
        (area / "root" / "async_jobs").mkdir(parents=True, exist_ok=True)
        (area / "root" / "async_jobs" / f"{started['jobId']}.json").write_text(json.dumps({
            "jobId": started["jobId"], "call_key": owner, "project_key": project,
            "stage": "CONSUMED",
        }), encoding="utf-8")

        cancelled = job_call(client, "codex_cancel", {"jobId": started["jobId"]})

        assert cancelled["state"] == "SUCCEEDED"
        assert lease.owner_state(owner) == "RELEASED"
    finally:
        client.close()


def test_codex_apply_records_hand_resolved_conflict(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        (repo / "tracked.txt").write_text("repository change\n", encoding="utf-8")
        conflict = job_call(client, "codex_apply", {"jobId": started["jobId"]})
        assert conflict["applied"] is False

        (repo / "tracked.txt").write_text("hand merged result\n", encoding="utf-8")
        subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
        subprocess.run(["git", "-C", str(repo), "commit", "-qm", "resolve job conflict"], check=True)
        resolved = job_call(client, "codex_apply", {"jobId": started["jobId"], "resolved": True})
        assert job_call(client, "codex_apply", {"jobId": started["jobId"], "resolved": True}) == resolved

        assert resolved == {"applied": True, "resolvedByHand": True, "paths": ["tracked.txt"]}
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                              (started["jobId"] + ".json"))
        assert job["applyState"] == "applied"
        assert job["applyResult"] == resolved
        assert isinstance(job["appliedAt"], (int, float))
    finally:
        client.close()


def test_codex_apply_refuses_hand_resolution_with_conflict_markers(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        (repo / "tracked.txt").write_text("repository change\n", encoding="utf-8")
        assert job_call(client, "codex_apply", {"jobId": started["jobId"]})["applied"] is False
        (repo / "tracked.txt").write_text("<<<<<<< ours\nleft\n=======\nright\n>>>>>>> theirs\n",
                                           encoding="utf-8")

        result = job_call(client, "codex_apply", {"jobId": started["jobId"], "resolved": True})

        assert result["applied"] is False
        assert "Conflict markers remain" in result["reason"]
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                              (started["jobId"] + ".json"))
        assert job["applyState"] == "conflict"
    finally:
        client.close()


def test_codex_apply_refuses_hand_resolution_with_unmerged_paths(area):
    repo = _git_repo(area)
    main_branch = subprocess.run(["git", "-C", str(repo), "branch", "--show-current"],
                                 capture_output=True, check=True, text=True).stdout.strip()
    subprocess.run(["git", "-C", str(repo), "checkout", "-qb", "side"], check=True)
    (repo / "tracked.txt").write_text("theirs\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "side change"], check=True)
    subprocess.run(["git", "-C", str(repo), "checkout", "-q", main_branch], check=True)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")
        (repo / "tracked.txt").write_text("repository change\n", encoding="utf-8")
        subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
        subprocess.run(["git", "-C", str(repo), "commit", "-qm", "repository change"], check=True)
        assert job_call(client, "codex_apply", {"jobId": started["jobId"]})["applied"] is False
        merge = subprocess.run(["git", "-C", str(repo), "merge", "side"],
                               capture_output=True, check=False)
        assert merge.returncode != 0
        result = job_call(client, "codex_apply", {"jobId": started["jobId"], "resolved": True})

        assert result == {"applied": False, "reason": "Repository has unmerged paths."}
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                              (started["jobId"] + ".json"))
        assert job["applyState"] == "conflict"
    finally:
        client.close()


def test_codex_apply_refuses_hand_resolution_outside_conflict_state(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "job\n"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                    "cwd": str(repo), "worktree": True})
        finish(client, started["jobId"], "SUCCEEDED")

        result = job_call(client, "codex_apply", {"jobId": started["jobId"], "resolved": True})

        assert result == {"applied": False, "reason": "Job is not in conflict state."}
        job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                              (started["jobId"] + ".json"))
        assert "applyState" not in job
    finally:
        client.close()


def test_reply_apply_marks_delta_already_in_repository_as_applied(area):
    repo = _git_repo(area)
    client = Client(area, extra_env={"FAKE_WRITE_FILE": "tracked.txt", "FAKE_WRITE_CONTENT": "first\n"})
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "work",
                                                   "cwd": str(repo), "worktree": True})
        finish(client, first["jobId"], "SUCCEEDED")
        first_job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                    (first["jobId"] + ".json"))

        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                   "threadId": "thread-123", "prompt": "continue"})
        finish(client, reply["jobId"], "SUCCEEDED")
        worktree = Path(first_job["worktreePath"])
        (worktree / "tracked.txt").write_text("reply delta\n", encoding="utf-8")

        assert job_call(client, "codex_apply", {"jobId": first["jobId"]}) == {
            "applied": True, "paths": ["tracked.txt"]}
        assert job_call(client, "codex_apply", {"jobId": reply["jobId"]}) == {
            "applied": True, "paths": ["tracked.txt"]}
        reply_job = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                    (reply["jobId"] + ".json"))
        assert reply_job["applyState"] == "applied"
        assert (repo / "tracked.txt").read_text(encoding="utf-8") == "reply delta\n"
    finally:
        client.close()


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
                                                  "etaMinutes": 7, "jobModel": "gpt-6-sol",
                                                  "jobEffort": "high", "jobLabel": "Bridge task",
                                                  "jobPhase": "implementation"})
        assert started["state"] == "RUNNING" and started["acceptedAt"] < started["deadlineAt"]
        assert started["deadlineAt"] - started["acceptedAt"] == 24 * 60 * 60
        assert job_call(client, "codex_result", {"jobId": started["jobId"]})["state"] == "RUNNING"
        assert job_call(client, "codex_start", {"call": key, "prompt": "slow"})["jobId"] == started["jobId"]
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        assert (record["taskId"], record["route"], record["etaMinutes"]) == (
            "bridge-task", "subtle", 7)
        assert (record["model"], record["effort"], record["label"]) == (
            "gpt-6-sol", "high", "Bridge task")
        assert record["phase"] == "implementation"
        with socket.create_connection(("127.0.0.1", record["signalPort"]), timeout=4) as signal_connection:
            signal_connection.sendall(f"{started['jobId']}:{record['signalToken']}".encode("ascii"))
            assert signal_connection.recv(1) == b"1"
        result = finish(client, started["jobId"], "SUCCEEDED")
        assert (result["threadId"], result["content"]) == ("thread-123", "Reply from fake Codex")
        assert "worktreePath" not in result
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


def test_completed_turn_hang_succeeds_and_stops_child_tree(area, monkeypatch):
    fake = area / "fake_codex.py"
    fake.write_text(FAKE, encoding="utf-8")
    child_pid_file = area / "child.pid"
    monkeypatch.setenv("FAKE_RECORD", str(area / "calls.jsonl"))
    monkeypatch.setenv("FAKE_HANG_AFTER_TERMINAL", "1")
    monkeypatch.setenv("FAKE_CHILD_PID_FILE", str(child_pid_file))
    monkeypatch.setattr(codex_exec_mcp, "CODEX_TERMINAL_GRACE_SECONDS", 0.2, raising=False)
    if os.name == "nt":
        # The restricted runner denies taskkill; exercise the host's confirmed
        # tree-stop branch while still checking that both real processes die.
        def stop_tree(process):
            child_pid = int(child_pid_file.read_text(encoding="ascii"))
            os.kill(child_pid, 9)
            process.kill()
            return True
        monkeypatch.setattr(codex_exec_mcp, "_stop_process", stop_tree)
    jobs = area / "jobs"
    jobs.mkdir()
    server = codex_exec_mcp.Server(codex_bin=json.dumps([sys.executable, str(fake)]), job_dir=jobs)
    server.state_dir = area / "state"
    codex_exec_mcp._save_record(server.state_dir, {"thread_id": "thread-123", "cwd": str(area),
                                                   "model": None, "config_args": [], "sandbox": None})
    job_id = uuid.uuid4().hex
    server._write_job({"jobId": job_id, "state": "QUEUED", "deadlineAt": time.time() + 5})
    try:
        server._job_worker(job_id, "codex-reply", {"threadId": "thread-123", "prompt": "complete then hang"})
        record = server._read_job(job_id)
        assert record["state"] == "SUCCEEDED"
        assert record["exitForcedAfterComplete"] is True
        assert record["threadId"] == "thread-123"
        assert (jobs / record["outputFile"]).read_text(encoding="utf-8") == "Reply from fake Codex"
        child_pid = int(child_pid_file.read_text(encoding="ascii"))
        assert windows_process_exited(child_pid) if os.name == "nt" else not _pid_alive(child_pid)
    finally:
        if child_pid_file.exists():
            try:
                os.kill(int(child_pid_file.read_text(encoding="ascii")), 9)
            except OSError:
                pass


def _pid_alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False


def _run_local_fake_job(area, monkeypatch, *, deadline=5):
    fake = area / "fake_codex.py"
    fake.write_text(FAKE, encoding="utf-8")
    monkeypatch.setenv("FAKE_RECORD", str(area / "calls.jsonl"))
    monkeypatch.setattr(codex_exec_mcp, "CODEX_TERMINAL_GRACE_SECONDS", 0.2)
    jobs = area / "jobs"
    jobs.mkdir()
    server = codex_exec_mcp.Server(codex_bin=json.dumps([sys.executable, str(fake)]), job_dir=jobs)
    server.state_dir = area / "state"
    job_id = uuid.uuid4().hex
    server._write_job({"jobId": job_id, "state": "QUEUED", "deadlineAt": time.time() + deadline})
    server._job_worker(job_id, "codex", {"prompt": "test terminal handling", "cwd": str(area)})
    return server._read_job(job_id)


def test_normal_exit_after_completed_turn_has_no_forced_flag(area, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT_DELAY_AFTER_TERMINAL", "0.05")
    record = _run_local_fake_job(area, monkeypatch)
    assert record["state"] == "SUCCEEDED"
    assert record["threadId"] == "thread-123"
    assert "exitForcedAfterComplete" not in record


def test_hang_without_completed_turn_keeps_deadline_failure(area, monkeypatch):
    monkeypatch.setenv("FAKE_HANG_BEFORE_TERMINAL", "1")
    record = _run_local_fake_job(area, monkeypatch, deadline=0.5)
    assert record["state"] == "TIMED_OUT"
    assert record["errorCode"] == "DEADLINE_EXCEEDED"
    assert "exitForcedAfterComplete" not in record


def test_normal_exit_without_completed_turn_is_not_success(area, monkeypatch):
    monkeypatch.setenv("FAKE_NO_COMPLETION_EVENT", "1")
    record = _run_local_fake_job(area, monkeypatch)
    assert record["state"] == "FAILED"
    assert "exitForcedAfterComplete" not in record


def test_failed_turn_hang_stays_failed(area, monkeypatch):
    monkeypatch.setenv("FAKE_FAIL", "1")
    monkeypatch.setenv("FAKE_HANG_AFTER_TERMINAL", "1")
    if os.name == "nt":
        def stop_root(process):
            process.kill()
            return True
        monkeypatch.setattr(codex_exec_mcp, "_stop_process", stop_root)
    record = _run_local_fake_job(area, monkeypatch)
    assert record["state"] == "FAILED"
    assert record["errorCode"] == "CODEX_FAILED"
    assert "exitForcedAfterComplete" not in record


def test_async_failure_timeout_cancel(area):
    failed = Client(area, extra_env={"FAKE_FAIL": "1"})
    try:
        job = job_call(failed, "codex_start", {"call": str(uuid.uuid4()), "prompt": "fail"})
        result = finish(failed, job["jobId"], "FAILED")
        assert result["errorCode"] == "CODEX_FAILED" and len(result["message"]) <= 200
        assert read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"))[
            "boardDisplayCount"] == 0
    finally:
        failed.close()
    slow = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        job = job_call(slow, "codex_start", {"call": str(uuid.uuid4()), "prompt": "timeout", "timeoutSeconds": 0.1})
        assert finish(slow, job["jobId"], "TIMED_OUT")["errorCode"] == "DEADLINE_EXCEEDED"
        assert read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"))[
            "boardDisplayCount"] == 0
        calls_before_cancel = len(slow.calls())
        job = job_call(slow, "codex_start", {"call": str(uuid.uuid4()), "prompt": "cancel"})
        deadline = time.monotonic() + 5
        while len(slow.calls()) == calls_before_cancel and time.monotonic() < deadline:
            time.sleep(0.02)
        assert len(slow.calls()) > calls_before_cancel, "fake Codex did not start before cancellation"
        assert job_call(slow, "codex_cancel", {"jobId": job["jobId"]})["state"] == "CANCELLED"
        assert finish(slow, job["jobId"], "CANCELLED")["errorCode"] == "CANCELLED"
        assert read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"))[
            "boardDisplayCount"] == 0
    finally:
        slow.close()


def test_async_codex_launch_failure_includes_cause(area):
    missing_codex = area / "missing-codex.exe"
    client = Client(area, bin_override=str(missing_codex))
    try:
        job = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "launch failure"})
        result = finish(client, job["jobId"], "FAILED")
        assert result["errorCode"] == "CODEX_FAILED"
        assert "Codex failed" in result["message"]
        assert "WinError 2" in result["message"] or "No such file or directory" in result["message"]
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json"))
        assert record["launchFailureEvidence"]["type"] == "AMBIGUOUS"
        assert record["responsivenessAttempts"] == 1
    finally:
        client.close()


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
    release = area / "release-codex"
    first = Client(area, extra_env={"FAKE_RELEASE_AFTER_START": str(release)})
    key = str(uuid.uuid4())
    try:
        started = job_call(first, "codex_start", {"call": key, "prompt": "private slow prompt"})
        path = area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")
        record = read_job_record(path)
        assert record["state"] == "RUNNING" and record["hostPid"] > 0
        if os.name == "nt":
            assert windows_parent_pid(record["hostPid"]) != first.process.pid
        launch = path.parent / (started["jobId"] + ".launch")
        wait_for_path(launch, True, "launch payload creation")
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
                release.touch()
                result = finish(third, started["jobId"], "SUCCEEDED")
                wait_for_path(launch, False, "launch payload removal")
                assert (result["threadId"], result["content"]) == ("thread-123", "Reply from fake Codex")
                assert len(third.calls()) == 1
            finally:
                third.close()
        finally:
            if second.process.poll() is None:
                second.close()
    finally:
        release.touch()
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


def _opencode_client(area, **flags):
    fake = area / "fake_opencode.py"
    fake.write_text(FAKE_OPENCODE, encoding="utf-8")
    return Client(area, bin_override=json.dumps([sys.executable, str(fake)]),
                  extra_env={"PURSER_AGENT": "opencode", **flags})


def _candidate_inventory(area, *models):
    path = area / "root" / "opencode" / "candidates.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"refreshed_at": "2026-09-27T00:00:00Z", "candidates": [
        {"id": model, "status": "measured", "added_at": "2026-09-27T00:00:00Z"}
        for model in models]}), encoding="utf-8")
    return path


FAKE_AGY = r'''
import json
import os
import sys

args = sys.argv[1:]
with open(os.environ["FAKE_RECORD"], "a", encoding="utf-8") as stream:
    stream.write(json.dumps({"argv": args, "cwd": os.getcwd()}) + "\n")
if os.environ.get("FAKE_AGY_MISSING") == "1":
    print("{}")
elif os.environ.get("FAKE_AGY_FAIL") == "1":
    print(json.dumps({"conversation_id": "agy-thread", "status": "FAILED", "error": "quota denied"}))
else:
    session = "new-thread" if os.path.exists(os.path.join(os.getcwd(), "agy-new-id")) else "agy-thread"
    print(json.dumps({"conversation_id": session, "status": "SUCCESS", "response": "DONE"}))
'''


def _agy_client(area, **flags):
    fake = area / "fake_agy.py"
    fake.write_text(FAKE_AGY, encoding="utf-8")
    return Client(area, bin_override=json.dumps([sys.executable, str(fake)]),
                  extra_env={"PURSER_AGENT": "agy", **flags})


def test_agy_job_command_completion_and_resume(area):
    client = _agy_client(area)
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
                                                 "cwd": str(area), "model": "gemini-3.8-flash-low",
                                                 "config": {"model_reasoning_effort": "medium"}})
        assert finish(client, first["jobId"], "SUCCEEDED")["threadId"] == "agy-thread"
        second = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": "agy-thread", "prompt": "two"})
        assert finish(client, second["jobId"], "SUCCEEDED")["content"] == "DONE"
        calls = client.calls()
        assert calls[0] == {"argv": ["--print", "one", "--output-format", "json", "--model",
                                      "gemini-3.8-flash-low", "--mode", "accept-edits",
                                      "--dangerously-skip-permissions", "--effort", "medium",
                                      "--new-project", "--add-dir", str(area)], "cwd": str(area)}
        assert calls[1]["argv"] == ["--print", "two", "--output-format", "json", "--model",
                                     "gemini-3.8-flash-low", "--mode", "accept-edits",
                                     "--dangerously-skip-permissions", "--effort", "medium",
                                     "--conversation", "agy-thread"]
    finally:
        client.close()


@pytest.mark.parametrize("flag", ["FAKE_AGY_MISSING", "FAKE_AGY_FAIL"])
def test_agy_missing_completion_or_failure_fails(area, flag):
    client = _agy_client(area, **{flag: "1"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
                                                   "cwd": str(area), "model": "gemini-3.8-flash-low",
                                                   "config": {"model_reasoning_effort": "medium"}})
        assert finish(client, started["jobId"], "FAILED")["errorCode"] == "CODEX_FAILED"
    finally:
        client.close()


def test_agy_reply_denied_when_cli_starts_new_conversation(area):
    client = _agy_client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
                                                   "cwd": str(area), "model": "gemini-3.8-flash-low",
                                                   "config": {"model_reasoning_effort": "medium"}})
        assert finish(client, started["jobId"], "SUCCEEDED")["threadId"] == "agy-thread"
        (area / "agy-new-id").touch()
        reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                 "threadId": "agy-thread", "prompt": "two"})
        assert finish(client, reply["jobId"], "FAILED")["errorCode"] == "CODEX_FAILED"
    finally:
        client.close()


def test_opencode_job_session_resume_and_command(area):
    client = _opencode_client(area)
    try:
        first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
                                                 "cwd": str(area), "model": "opencode/test",
                                                 "config": {"model_reasoning_effort": "high"}})
        result = finish(client, first["jobId"], "SUCCEEDED")
        assert (result["threadId"], result["content"]) == ("ses_f1dcef146ffez46V9C0TN9cmiZ", "DONE")
        second = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                                  "threadId": result["threadId"], "prompt": "two"})
        assert finish(client, second["jobId"], "SUCCEEDED")["threadId"] == "ses_f1dcef146ffez46V9C0TN9cmiZ"
        calls = client.calls()
        assert calls[0]["argv"] == ["run", "--format", "json", "--model", "opencode/test",
                                    "--variant", "high", "--", "one"]
        assert calls[1]["argv"] == ["run", "--format", "json", "--model", "opencode/test",
                                    "--variant", "high", "--session", "ses_f1dcef146ffez46V9C0TN9cmiZ", "--", "two"]
    finally:
        client.close()


@pytest.mark.parametrize("flag", ["FAKE_OC_ERROR", "FAKE_OC_DENIED", "FAKE_OC_NO_STOP"])
def test_opencode_job_fails_on_event_or_missing_session(area, flag):
    client = _opencode_client(area, **{flag: "1"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
                                                   "cwd": str(area), "model": "opencode/test"})
        assert finish(client, started["jobId"], "FAILED", timeout=120)["errorCode"] == "CODEX_FAILED"
    finally:
        client.close()


def test_mechanical_first_measured_opencode_candidate_serves(area):
    model = "vendor/first-free"
    _candidate_inventory(area, model)
    client = _opencode_client(area)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(area), "agent": "opencode", "model": model, "mechanicalFallback": True,
            "fallbackAgent": "opencode", "fallbackModel": "configured/model", "fallbackEffort": "none"})
        result = finish(client, started["jobId"], "SUCCEEDED")
        assert (result["attemptedModels"], result["servedAgent"], result["servedModel"]) == ([model], "opencode", model)
        assert len(client.calls()) == 1
    finally:
        client.close()


def test_mechanical_missing_model_retries_next_measured_candidate(area):
    first, second = "vendor/missing-free", "vendor/second-free"
    path = _candidate_inventory(area, first, second)
    repo = _git_repo(area)
    client = _opencode_client(area, FAKE_OC_MISSING_MODELS=first)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(repo), "worktree": True, "agent": "opencode", "model": first, "mechanicalFallback": True,
            "fallbackAgent": "opencode", "fallbackModel": "configured/model", "fallbackEffort": "none"})
        result = finish(client, started["jobId"], "SUCCEEDED")
        inventory = json.loads(path.read_text(encoding="utf-8"))["candidates"]
        assert inventory[0]["status"] == "failed"
        assert result["attemptedModels"] == [first, second]
        assert (result["servedAgent"], result["servedModel"]) == ("opencode", second)
    finally:
        client.close()


def test_mechanical_missing_model_with_codex_fallback_tries_next_candidate_first(area):
    first, second = "vendor/missing-free", "vendor/second-free"
    _candidate_inventory(area, first, second)
    repo = _git_repo(area)
    client = _opencode_client(area, FAKE_OC_MISSING_MODELS=first)
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(repo), "worktree": True, "agent": "opencode", "model": first, "mechanicalFallback": True,
            "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"})
        result = finish(client, started["jobId"], "SUCCEEDED")

        assert result["attemptedModels"] == [first, second]
        assert (result["servedAgent"], result["servedModel"]) == ("opencode", second)
        assert [call["argv"][call["argv"].index("--model") + 1] for call in client.calls()] == [first, second]
    finally:
        client.close()


def test_mechanical_missing_candidates_fall_back_to_luna_with_served_identity(area, monkeypatch):
    first, second = "vendor/missing-free", "vendor/second-missing-free"
    server, job_id, calls = _mechanical_bridge(area, monkeypatch)
    _candidate_inventory(area, first, second)

    def run_once(_name, arguments, _request_id, _async_job):
        calls.append(dict(arguments))
        if arguments.get("agent") == "opencode":
            return {"isError": True, "errorCode": "CODEX_FAILED", "retryableModelMissing": True,
                    "typedEvidence": {"type": "MODEL_MISSING", "noWorkProof": True}}
        return codex_exec_mcp._tool_result("luna-thread", content="DONE")

    monkeypatch.setattr(server, "_run_tool_once", run_once)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": first, "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-luna", "fallbackEffort": "high"}
    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert [call["model"] for call in calls] == [first, second, "gpt-6-luna"]
    assert (result["structuredContent"]["servedAgent"],
            result["structuredContent"]["servedModel"]) == ("codex", "gpt-6-luna")


def test_mechanical_timeout_finishes_timed_out_without_takeover(area):
    client = _opencode_client(area, FAKE_OC_HANG_BEFORE_EVENTS="1")
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(area), "timeoutSeconds": 1, "agent": "opencode", "model": "vendor/free",
            "mechanicalFallback": True, "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol",
            "fallbackEffort": "medium"})
        result = finish(client, started["jobId"], "TIMED_OUT", timeout=20)

        assert result["errorCode"] == "DEADLINE_EXCEEDED"
        assert len(client.calls()) == 1
    finally:
        client.close()


def test_mechanical_all_missing_uses_configured_fallback_binding(area):
    first, second, fallback = "vendor/missing-a", "vendor/missing-b", "configured/model"
    _candidate_inventory(area, first, second)
    repo = _git_repo(area)
    client = _opencode_client(area, FAKE_OC_MISSING_MODELS=f"{first},{second}")
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(repo), "worktree": True, "agent": "opencode", "model": first, "mechanicalFallback": True,
            "fallbackAgent": "opencode", "fallbackModel": fallback, "fallbackEffort": "none"})
        result = finish(client, started["jobId"], "SUCCEEDED")
        assert result["attemptedModels"] == [first, second, fallback]
        assert (result["servedAgent"], result["servedModel"]) == ("opencode", fallback)
        assert [call["argv"][call["argv"].index("--model") + 1] for call in client.calls()] == [first, second, fallback]
    finally:
        client.close()


def test_ambiguous_opencode_error_does_not_retry(area):
    first, second = "vendor/ambiguous", "vendor/next"
    path = _candidate_inventory(area, first, second)
    client = _opencode_client(area, FAKE_OC_ERROR="1")
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one",
            "cwd": str(area), "agent": "opencode", "model": first, "mechanicalFallback": True,
            "fallbackAgent": "opencode", "fallbackModel": "configured/model", "fallbackEffort": "none"})
        result = finish(client, started["jobId"], "FAILED")
        assert result["errorCode"] == "CODEX_FAILED"
        assert result["attemptedModels"] == [first]
        assert len(client.calls()) == 1
        assert json.loads(path.read_text(encoding="utf-8"))["candidates"][0]["status"] == "measured"
    finally:
        client.close()


def _mechanical_bridge(area, monkeypatch, *, changed=False, codex_fails=False):
    repo = _git_repo(area)
    snapshot = subprocess.run(["git", "-C", str(repo), "rev-parse", "HEAD^{tree}"],
                              check=True, capture_output=True, text=True).stdout.strip()
    state = area / "root"
    _candidate_inventory(area, "vendor/free")
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: state)
    jobs = state / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    server.state_dir = area / "state"
    job_id = uuid.uuid4().hex
    server._write_job({"jobId": job_id, "state": "RUNNING", "deadlineAt": time.time() + 30,
                       "worktreePath": str(repo), "snapshot": snapshot})
    if changed:
        (repo / "worker-edit.txt").write_text("partial edit\n", encoding="utf-8")
    calls = []

    def run_once(_name, arguments, _request_id, _async_job):
        calls.append(dict(arguments))
        if arguments.get("agent") == "opencode":
            return {"isError": True, "errorCode": "CODEX_FAILED", "typedEvidence": {
                "type": "PROVIDER_ERROR", "noWorkProof": True}}
        if codex_fails:
            return {"isError": True, "errorCode": "CODEX_FAILED", "typedEvidence": {
                "type": "AMBIGUOUS", "noWorkProof": False}}
        return codex_exec_mcp._tool_result("codex-thread", content="DONE")

    monkeypatch.setattr(server, "_run_tool_once", run_once)
    return server, job_id, calls


def test_mechanical_empty_delta_takes_over_on_codex_once_and_attributes_result(area, monkeypatch):
    server, job_id, calls = _mechanical_bridge(area, monkeypatch)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": "vendor/free", "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"}

    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert len(calls) == 2
    assert calls[0]["agent"] == "opencode" and calls[1]["agent"] == "codex"
    assert calls[0]["prompt"] == calls[1]["prompt"]
    assert calls[0]["cwd"] == calls[1]["cwd"]
    value = result["structuredContent"]
    assert (value["servedAgent"], value["servedModel"]) == ("codex", "gpt-6-sol")
    inventory = json.loads((area / "root" / "opencode" / "candidates.json").read_text(encoding="utf-8"))
    assert inventory["candidates"][0]["mechanical_samples"] == [False]


def test_mechanical_failure_with_edits_is_not_retried(area, monkeypatch):
    server, job_id, calls = _mechanical_bridge(area, monkeypatch, changed=True)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": "vendor/free", "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"}

    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert len(calls) == 1
    assert result["isError"] is True


def test_mechanical_takeover_is_limited_to_one_codex_retry(area, monkeypatch):
    server, job_id, calls = _mechanical_bridge(area, monkeypatch, codex_fails=True)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": "vendor/free", "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"}

    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert len(calls) == 2
    assert result["isError"] is True


def test_opencode_rejects_unmapped_launch_options(area):
    client = _opencode_client(area)
    try:
        for invalid in ({"model": "unqualified"}, {"model": "opencode/test", "sandbox": "workspace-write"},
                        {"model": "opencode/test", "approval-policy": "never"}):
            result = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "one", **invalid})
            assert result["state"] == "FAILED"
        assert client.calls() == []
    finally:
        client.close()


@pytest.mark.parametrize(("events", "kwargs", "failure_type"), [
    ([{"type": "error", "error": {"name": "ProviderError"}}], {"process_returncode": 1}, "PROVIDER_ERROR"),
    ([{"type": "step_start"}], {"process_returncode": 2}, "NONZERO_EXIT"),
])
def test_opencode_failure_classification_proves_no_work(events, kwargs, failure_type):
    evidence = codex_exec_mcp._launch_failure_evidence("opencode", events, **kwargs)

    assert evidence["type"] == failure_type
    assert evidence["noWorkProof"] is True


def test_opencode_final_stop_triggers_existing_exit_grace(area, monkeypatch):
    fake = area / "fake_opencode.py"
    fake.write_text(FAKE_OPENCODE, encoding="utf-8")
    monkeypatch.setenv("FAKE_RECORD", str(area / "calls.jsonl"))
    monkeypatch.setenv("FAKE_OC_HANG_AFTER_STOP", "1")
    monkeypatch.setattr(codex_exec_mcp, "CODEX_TERMINAL_GRACE_SECONDS", 0.1)
    monkeypatch.setattr(codex_exec_mcp, "_stop_process", lambda process: (process.kill(), True)[1])
    jobs = area / "jobs"
    jobs.mkdir()
    server = codex_exec_mcp.Server(codex_bin=json.dumps([sys.executable, str(fake)]),
                                    job_dir=jobs, agent="opencode")
    server.state_dir = area / "state"
    job_id = uuid.uuid4().hex
    server._write_job({"jobId": job_id, "state": "QUEUED", "deadlineAt": time.time() + 5})
    server._job_worker(job_id, "codex", {"prompt": "one", "cwd": str(area), "model": "opencode/test"})
    record = server._read_job(job_id)
    assert record["state"] == "SUCCEEDED"
    assert record["boardDisplayCount"] == 0
    assert record["exitForcedAfterComplete"] is True


def test_http_402_error_event_is_recorded_as_credit_exhausted(area):
    client = Client(area, extra_env={"FAKE_HTTP_402": "1"})
    try:
        started = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "probe"})
        assert finish(client, started["jobId"], "FAILED")["errorCode"] == "CODEX_FAILED"
        record = read_job_record(area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json"))
        assert record["launchFailureEvidence"]["type"] == "CREDIT_EXHAUSTED"
    finally:
        client.close()


@pytest.mark.parametrize(("message", "retry_after"), [
    ("HTTP 429 Too Many Requests; rate limit exceeded. Retry-After: 17", 17.0),
    ("HTTP 429 Too Many Requests; rate limit exceeded.", None),
])
def test_rate_limit_evidence_keeps_optional_wait(message, retry_after):
    # Assumed OpenCode ProviderError text; the error/data/message envelope is parser-compatible.
    event = {"type": "error", "error": {"name": "ProviderError", "data": {"message": message}}}
    evidence = codex_exec_mcp._launch_failure_evidence("opencode", [event], stdout_had_output=True)
    assert evidence == {"type": "RATE_LIMITED", "retryAfterSeconds": retry_after, "noWorkProof": False}


def test_codex_lifecycle_before_402_is_credit_but_item_output_is_ambiguous():
    events = [
        {"type": "thread.started", "thread_id": "thread-123"},
        {"type": "turn.started"},
        {"type": "turn.failed", "error": {"message": "HTTP 402: insufficient balance; billing required"}},
    ]
    assert codex_exec_mcp._launch_failure_evidence("codex", events)["type"] == "CREDIT_EXHAUSTED"
    events.insert(2, {"type": "item.completed", "item": {"type": "agent_message", "text": "partial"}})
    assert codex_exec_mcp._launch_failure_evidence("codex", events)["type"] == "AMBIGUOUS"


def test_opencode_step_start_before_402_is_credit_but_text_is_ambiguous():
    events = [
        {"type": "step_start", "sessionID": "session-123"},
        {"type": "error", "error": {"name": "ProviderError", "data": {
            "message": "HTTP 402: insufficient balance; billing required"}}},
    ]
    assert codex_exec_mcp._launch_failure_evidence("opencode", events)["type"] == "CREDIT_EXHAUSTED"
    events.insert(1, {"type": "text", "sessionID": "session-123", "part": {"text": "partial"}})
    assert codex_exec_mcp._launch_failure_evidence("opencode", events)["type"] == "AMBIGUOUS"


def test_no_response_retries_five_times_at_fixed_offsets():
    now = [100.0]
    calls = []

    def run():
        calls.append(now[0] - 100.0)
        return {"typedEvidence": codex_exec_mcp._launch_failure_evidence(
            "opencode", [], process_started=True)}

    def sleep(seconds):
        now[0] += seconds

    result = codex_exec_mcp._run_with_responsiveness_retries(run, clock=lambda: now[0], sleep=sleep)
    assert calls == [0.0, 30.0, 60.0, 120.0, 240.0]
    assert result["responsivenessAttempts"] == 5


def test_ambiguous_event_never_retries():
    calls = []

    def run():
        calls.append(1)
        return {"typedEvidence": {"type": "AMBIGUOUS", "noWorkProof": False}}

    result = codex_exec_mcp._run_with_responsiveness_retries(run, clock=lambda: 0.0,
                                                              sleep=lambda _seconds: pytest.fail("unexpected retry"))
    assert calls == [1]
    assert result["responsivenessAttempts"] == 1


def test_no_response_then_success_does_not_record_failure_evidence(area, monkeypatch):
    jobs = area / "jobs"
    jobs.mkdir()
    server = codex_exec_mcp.Server(job_dir=jobs)
    job_id = uuid.uuid4().hex
    server._write_job({"jobId": job_id, "state": "QUEUED", "deadlineAt": time.time() + 60})
    attempts = []

    def run_tool(*_args, **_kwargs):
        attempts.append(1)
        if len(attempts) == 1:
            return {"isError": True, "typedEvidence": {"type": "NO_RESPONSE", "noWorkProof": True}}
        return codex_exec_mcp._tool_result("probe-thread", content="OK")

    monkeypatch.setattr(server, "_run_tool", run_tool)
    run_retries = codex_exec_mcp._run_with_responsiveness_retries
    now = [50.0]

    def fast_retries(run):
        return run_retries(run, clock=lambda: now[0], sleep=lambda seconds: now.__setitem__(0, now[0] + seconds))

    monkeypatch.setattr(codex_exec_mcp, "_run_with_responsiveness_retries", fast_retries)
    server._job_worker(job_id, "codex", {"prompt": "probe", "cwd": str(area)})
    record = server._read_job(job_id)
    assert len(attempts) == 2
    assert record["state"] == "SUCCEEDED"
    assert "launchFailureEvidence" not in record


@pytest.mark.parametrize(("agent", "model"), [("codex", "gpt-probe"), ("opencode", "deepseek/probe")])
def test_api_primary_probe_uses_agent_cli_in_temporary_directory(area, monkeypatch, agent, model):
    fake = area / "probe_cli.py"
    fake.write_text(r'''
import json
import os
from pathlib import Path
import sys

args = sys.argv[1:]
prompt = ""
if args[0] == "run":
    events = [
        {"type": "step_start", "sessionID": "probe-session"},
        {"type": "text", "sessionID": "probe-session", "part": {"text": "OK"}},
        {"type": "step_finish", "sessionID": "probe-session", "part": {"reason": "stop"}},
    ]
else:
    prompt = sys.stdin.buffer.read().decode("utf-8")
    events = [
        {"type": "thread.started", "thread_id": "probe-thread"},
        {"type": "turn.completed"},
    ]
Path(os.environ["FAKE_PROBE_RECORD"]).write_text(json.dumps({"args": args, "cwd": os.getcwd(), "prompt": prompt}))
for event in events:
    print(json.dumps(event), flush=True)
''', encoding="utf-8")
    record_path = area / "probe.json"
    monkeypatch.setenv("FAKE_PROBE_RECORD", str(record_path))
    result = codex_exec_mcp.probe_api_primary(
        agent, model, codex_bin=json.dumps([sys.executable, str(fake)]), timeout=2)
    assert result == {"ok": True}, (result, record_path.read_text(encoding="utf-8") if record_path.exists() else "CLI did not record invocation")
    invocation = json.loads(record_path.read_text(encoding="utf-8"))
    assert Path(invocation["cwd"]) != area
    assert model in invocation["args"]
    assert "Reply with OK." in invocation["args"] or invocation["prompt"] == "Reply with OK."
    if agent == "codex":
        assert invocation["args"][invocation["args"].index("-s") + 1] == "read-only"


def test_api_primary_probe_timeout_is_not_healthy(area):
    fake = area / "slow_probe.py"
    fake.write_text("import time\ntime.sleep(30)\n", encoding="utf-8")
    result = codex_exec_mcp.probe_api_primary(
        "codex", "gpt-probe", codex_bin=json.dumps([sys.executable, str(fake)]), timeout=0.05)
    assert result["ok"] is False
    assert result["failure"]["type"] == "NO_RESPONSE"


def test_codex_bridge_extracts_usage_from_completed_turn():
    usage = codex_exec_mcp._codex_turn_usage([
        {"type": "turn.completed", "usage": {
            "input_tokens": 23, "output_tokens": 7, "total_tokens": 30,
            "private_field": "ignored",
        }},
    ])
    assert usage == {"input_tokens": 23, "output_tokens": 7, "total_tokens": 30}
    response = codex_exec_mcp._tool_result("thread-1", content="Done", usage=usage)
    assert codex._codex_usage(response) == usage


def test_anthropic_spend_cap_is_credit_exhaustion():
    # Assumed Anthropic error event wrapper; this structured spend-cap code is the classifier contract.
    event = {"type": "error", "error": {"type": "rate_limit_error", "details": {
        "error_code": "enforced_spend_limit_reached"}, "message": "Access resumes at midnight."}}
    evidence = codex_exec_mcp._launch_failure_evidence("opencode", [event])
    assert evidence == {"type": "CREDIT_EXHAUSTED", "retryAfterSeconds": None,
                        "noWorkProof": False}
