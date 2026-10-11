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
import ledger
from mixed_owner_support import go_test_tools, launch_go_host

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
for item in json.loads(os.environ.get("FAKE_TEST_EVENTS", "[]")):
    print(json.dumps({"type": "item.completed", "item": {
        "type": "command_execution", **item}}), flush=True)
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
    for line in recorded[2:-1]:
        print(line, flush=True)
    print(recorded[-1], flush=True)
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

def test_post_start_dead_host_preserves_cancel_ownership_and_releases_lease(tmp_path, monkeypatch):
    import ledger

    root = tmp_path / "state"
    job_dir = root / "codex_exec_mcp" / "jobs"
    event = {"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
             "session_id": "dead-host-session", "tool_use_id": "dead-host-call"}
    call = str(uuid.uuid4())
    job_id = __import__("hashlib").sha256(call.encode()).hexdigest()
    server = codex_exec_mcp.Server(job_dir=job_dir)
    server._write_job({"jobId": job_id, "state": "QUEUED", "hostPid": 123, "hostCreatedAt": 456,
                       "hostContained": True, "acceptedAt": 1, "deadlineAt": 100})
    monkeypatch.setattr(server, "_classify_host", lambda _job: "DEAD")
    ticks = iter((0.0, 0.0, 0.0, 11.0))
    monkeypatch.setattr(codex_exec_mcp.time, "monotonic", lambda: next(ticks, 11.0))
    monkeypatch.setattr(codex_exec_mcp.time, "sleep", lambda _seconds: None)
    result = server._await_host(job_id)["structuredContent"]
    assert (result["state"], result["errorCode"]) == ("FAILED", "HOST_LAUNCH_UNCONFIRMED")
    assert "leaseQueued" not in server._read_job(job_id)
    owner = codex._call_key(event)
    project = "0123456789abcdef"
    pending = {"async_call": call, "async_ready": True, "session_id": event["session_id"],
               "tool_use_id": event["tool_use_id"], "project_key": project, "lease_owner": owner,
               "task_id": "retryable-task", "route": "normal", "model": "gpt-test", "effort": "medium",
               "baseline": {}}
    codex._atomic_json_write(root / "pending" / f"{owner}.json", pending)
    assert ledger.ScopeLeases(root / "projects" / project).acquire(owner, "retryable-task", ["src"],
                                                                   job_id=job_id)["status"] == ledger.OK
    event["tool_response"] = {"structuredContent": {"jobId": job_id, "state": "FAILED",
        "errorCode": "HOST_LAUNCH_UNCONFIRMED", "message": "dead host"}}
    monkeypatch.setattr(codex, "_load_config", lambda _config: {"post_tool_output_field": "updatedMCPToolOutput"})

    response = codex._post_start(event, {"post_tool_output_field": "updatedMCPToolOutput"}, root)

    assert (root / "pending" / f"{owner}.json").exists()
    index = json.loads((root / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
    assert index["call_key"] == owner and index["project_key"] == project
    assert ledger.ScopeLeases(root / "projects" / project).owner_state(owner) == "RELEASED"
    job_dir = root / "codex_exec_mcp" / "jobs"
    codex_exec_mcp.Server(job_dir=job_dir)._write_job({"jobId": job_id, "call": call, "state": "FAILED",
        "errorCode": "HOST_LAUNCH_UNCONFIRMED"})
    cancel_event = {**event, "tool_name": "mcp__codex-main__codex_cancel", "tool_input": {"jobId": job_id}}
    assert codex._async_pre_followup(cancel_event, root, None) == {}
    loaded = ledger.SessionLedger(root / "projects" / project).load("retryable-task")
    assert (loaded.get("record") or {}).get("last_outcome") is None
    assert response["hookSpecificOutput"]["hookEventName"] == "PostToolUse"
    assert "HOST_LAUNCH_UNCONFIRMED" in response["hookSpecificOutput"]["updatedMCPToolOutput"][0]["text"]

class Client:
    def __init__(self, area, *, bin_override=None, extra_env=None, server_command=None):
        fake = area / "fake_codex.py"
        fake.write_text(FAKE, encoding="utf-8")
        self.record = area / "calls.jsonl"
        env = os.environ.copy()
        env.update({"PURSER_CODEX_SHIM_STATE": str(area / "state"), "PURSER_STATE_DIR": str(area / "root"),
                    "PURSER_CODEX_TEST_PARENT_PID": str(os.getpid()), "FAKE_RECORD": str(self.record)})
        env.update(extra_env or {})
        self.env = env
        override = bin_override if bin_override is not None else json.dumps([sys.executable, str(fake)])
        command = ([*server_command, "--codex-bin", override] if server_command is not None else
                   [sys.executable, "-u", str(SCRIPT), "--codex-bin", override])
        self.process = subprocess.Popen(
            command,
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

    def send_notification(self, method, params=None):
        value = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            value["params"] = params
        self.send_raw(json.dumps(value, ensure_ascii=False) + "\n")

    def send_raw(self, text):
        self.process.stdin.write(text)
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

@pytest.fixture
def go_client(area, go_test_tools):
    instance = Client(area, bin_override=str(go_test_tools["fake"]),
                      server_command=[str(go_test_tools["hook"]), "mcp"],
                      extra_env={"FAKE_HANG_BEFORE_TERMINAL": "1", "PURSER_AGENT": "codex"})
    instance.go_hook = go_test_tools["hook"]
    try:
        yield instance
    finally:
        instance.close()

@pytest.fixture(params=["python", "alternate"])
def server_command(request):
    if request.param == "alternate":
        configured = os.environ.get("PURSER_MCP_SERVER_COMMAND")
        if not configured:
            pytest.skip("PURSER_MCP_SERVER_COMMAND is not configured")
        command = json.loads(configured)
        assert isinstance(command, list) and command and all(isinstance(item, str) for item in command)
        return command
    return None

@pytest.fixture
def oracle_client(area, server_command):
    instance = Client(area, server_command=server_command)
    try:
        yield instance
    finally:
        instance.close()

def call(client, request_id, name, arguments):
    client.send(request_id, "tools/call", {"name": name, "arguments": arguments})
    response = client.receive()
    assert response["id"] == request_id
    return response["result"]

def test_initialize_and_list(oracle_client):
    oracle_client.send(1, "initialize", {"protocolVersion": "2025-06-18"})
    assert oracle_client.receive() == {
        "jsonrpc": "2.0", "id": 1, "result": {
            "protocolVersion": "2025-06-18",
            "serverInfo": {"name": "codex-exec-mcp", "version": "1.0.0"},
            "capabilities": {"tools": {}},
        },
    }
    oracle_client.send(2, "tools/list")
    listed = oracle_client.receive()
    assert listed == {"jsonrpc": "2.0", "id": 2,
                      "result": {"tools": codex_exec_mcp.TOOL_SCHEMAS}}
    tools = listed["result"]["tools"]
    assert [tool["name"] for tool in tools] == ["codex", "codex-reply", "codex_start", "codex_result", "codex_cancel", "codex_apply"]
    start = next(tool for tool in tools if tool["name"] == "codex_start")
    assert "call" not in start["inputSchema"]["required"]
    apply = next(tool for tool in tools if tool["name"] == "codex_apply")
    assert apply["inputSchema"]["properties"]["resolved"]["type"] == "boolean"
    missing = call(oracle_client, 3, "codex_start", {"prompt": "work"})
    assert missing["structuredContent"]["errorCode"] == "INVALID_ARGUMENT"

def test_oracle_result_envelope_and_exact_text_bytes(oracle_client, area):
    arguments = {"prompt": "wire oracle", "cwd": str(area)}
    oracle_client.send(3, "tools/call", {"name": "codex", "arguments": arguments})
    response = oracle_client.receive()
    expected = codex_exec_mcp._tool_result("thread-123", content="Reply from fake Codex")
    assert response == {"jsonrpc": "2.0", "id": 3, "result": expected}
    assert response["result"]["content"][0]["text"].encode("utf-8") == (
        b'{"threadId": "thread-123", "content": "Reply from fake Codex"}')

def test_large_prompt_is_framed_and_preserved(oracle_client, area):
    prompt = ("frame 🧵\n" * 40000) + "end"
    oracle_client.send(10, "tools/call", {
        "name": "codex", "arguments": {"prompt": prompt, "cwd": str(area)},
    })
    response = oracle_client.receive()
    assert response["jsonrpc"] == "2.0" and response["id"] == 10
    assert response["result"]["content"][0]["text"].encode("utf-8") == (
        b'{"threadId": "thread-123", "content": "Reply from fake Codex"}')
    deadline = time.monotonic() + 2
    while not oracle_client.calls() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert bytes.fromhex(oracle_client.calls()[0]["stdin_hex"]).decode("utf-8") == prompt

def test_protocol_error_and_malformed_input_envelopes(oracle_client):
    oracle_client.send(21, "missing/method")
    assert oracle_client.receive() == {"jsonrpc": "2.0", "id": 21,
                                       "error": {"code": -32601, "message": "Method not found"}}
    oracle_client.send(22, "tools/call", [])
    assert oracle_client.receive() == {"jsonrpc": "2.0", "id": 22,
                                       "error": {"code": -32602, "message": "Invalid params"}}
    oracle_client.send_raw("{not json}\n")
    with pytest.raises(queue.Empty):
        oracle_client.receive(timeout=0.15)
    oracle_client.send(23, "ping")
    assert oracle_client.receive() == {"jsonrpc": "2.0", "id": 23, "result": {}}

def test_slow_call_does_not_block_ping_or_another_call(area, server_command):
    client = Client(area, extra_env={"FAKE_SLOW": "1", "FAKE_SLEEP_SECONDS": "1.5"},
                    server_command=server_command)
    try:
        client.send(31, "tools/call", {"name": "codex", "arguments": {"prompt": "slow", "cwd": str(area)}})
        client.send(32, "ping")
        client.send(33, "tools/call", {"name": "codex_start", "arguments": {}})
        responses = [client.receive(timeout=1) for _ in range(2)]
        by_id = {item["id"]: item for item in responses}
        assert [item["id"] for item in responses] in ([32, 33], [33, 32])
        assert by_id[32] == {"jsonrpc": "2.0", "id": 32, "result": {}}
        failed_start = {"state": "FAILED", "errorCode": "INVALID_ARGUMENT",
                        "message": "call and prompt are required"}
        assert by_id[33] == {"jsonrpc": "2.0", "id": 33,
                             "result": codex_exec_mcp._job_result(failed_start)}
        slow_result = codex_exec_mcp._tool_result("thread-123", content="Reply from fake Codex")
        assert client.receive(timeout=4) == {"jsonrpc": "2.0", "id": 31, "result": slow_result}
    finally:
        client.close()

def test_cancel_notification_suppresses_late_response_through_server_handle(area, server_command):
    client = Client(area, extra_env={"FAKE_SLOW": "1", "FAKE_SLEEP_SECONDS": "1"},
                    server_command=server_command)
    try:
        client.send(41, "tools/call", {"name": "codex", "arguments": {"prompt": "cancel", "cwd": str(area)}})
        deadline = time.monotonic() + 3
        while not client.calls() and time.monotonic() < deadline:
            time.sleep(0.01)
        assert client.calls()
        client.send_notification("notifications/cancelled", {"requestId": 41})
        with pytest.raises(queue.Empty):
            client.receive(timeout=1.5)
    finally:
        client.close()

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

def _go_running_job(go_client, area):
    import async_session_maintenance

    running = job_call(go_client, "codex_start", {
        "call": str(uuid.uuid4()), "prompt": "wait for Python cancellation",
        "timeoutSeconds": 30,
    })
    job_id = running["jobId"]
    record_path = area / "root" / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    launch_go_host(go_client.go_hook, record_path, go_client.env)
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        record = read_job_record(record_path)
        if record.get("state") == "RUNNING":
            break
        time.sleep(0.02)
    else:
        pytest.fail(f"Go host did not claim job: {record}")

    async_session_maintenance.cleanup_jobs(area / "root", now=time.time(), check_ceiling=True)
    after_cleanup = read_job_record(record_path)
    assert after_cleanup["generation"] == record["generation"]
    assert after_cleanup["ownershipToken"] == record["ownershipToken"]
    assert after_cleanup["hostPid"] == record["hostPid"]
    return job_id, record_path

def test_python_result_and_apply_read_go_created_job(go_client, area):
    """Python result/apply owners read a Go-created, Go-hosted active record."""
    job_id, record_path = _go_running_job(go_client, area)
    python_owner = codex_exec_mcp.Server(job_dir=record_path.parent)
    result = python_owner._result_job({"jobId": job_id})["structuredContent"]
    assert result["state"] == "RUNNING"
    apply_result = python_owner._apply_job({"jobId": job_id})["structuredContent"]
    assert apply_result["errorCode"] == "NOT_WORKTREE_JOB"

def test_python_cancel_reaches_go_created_job(go_client, area):
    """Python codex_cancel signals a Go-hosted job and requires terminal confirmation."""
    job_id, record_path = _go_running_job(go_client, area)
    python_owner = codex_exec_mcp.Server(job_dir=record_path.parent)
    result = python_owner._cancel_job({"jobId": job_id})["structuredContent"]
    assert result["state"] == "CANCELLED"

def test_python_cancel_skips_terminal_wait_after_local_host_finishes(area, monkeypatch):
    job_id = "c" * 64
    job_dir = area / "root" / "codex_exec_mcp" / "jobs"
    host = codex_exec_mcp.Server(host=True, job_dir=job_dir)
    assert host.listener is not None
    host.job_events[job_id] = threading.Event()
    now = time.time()
    host._write_job({"jobId": job_id, "state": "RUNNING", "signalToken": "local-token",
                     "signalPort": host.listener.getsockname()[1], "acceptedAt": now,
                     "deadlineAt": now + 60})
    finish_job = host._finish_job

    def finish_and_remove_event(*args, **kwargs):
        finish_job(*args, **kwargs)
        host.job_events.pop(job_id, None)

    monkeypatch.setattr(host, "_finish_job", finish_and_remove_event)
    python_owner = codex_exec_mcp.Server(job_dir=job_dir)
    try:
        result = python_owner._cancel_job({"jobId": job_id})["structuredContent"]
        assert result["state"] == "CANCELLED"
        assert host._read_job(job_id)["state"] == "CANCELLED"
    finally:
        host.listener.close()

def decision_events(area):
    events = []
    for path in (area / "root" / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return [event for event in events if event["event_type"] == "CLAUDE_DECISION_RECORDED"]

def scope_outcome_events(area):
    events = []
    for path in (area / "root" / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return [event for event in events if event["event_type"] == "SCOPE_CONFLICT_OUTCOME"]

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

def _readiness_worker_job(area):
    import ledger

    repo = _git_repo(area)
    job_id = uuid.uuid4().hex * 2
    worktree = ledger.create_job_worktree(repo, job_id, area / "wt")
    jobs = area / "jobs"
    jobs.mkdir()
    job = {
        "jobId": job_id, "call": str(uuid.uuid4()), "state": "QUEUED",
        "acceptedAt": time.time(), "deadlineAt": time.time() + 60,
        "worktreePath": worktree["path"], "snapshot": worktree["snapshot"],
        "repoRoot": str(repo.resolve()), "model": "claude-sonnet-4-5",
        "readinessCheck": {
            "grounded": True, "objective": "Validate names",
            "acceptance_criteria": "Reject an empty name",
            "agent": "agy", "model": "gemini-3.8-flash-high",
        },
    }
    server = codex_exec_mcp.Server(job_dir=jobs)
    server._write_job(job)
    return server, job_id, Path(worktree["path"])

def _thread_with_pending_reply(area, client):
    repo = _git_repo(area)
    tracked = repo / "tracked.txt"
    first = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "prompt": "first",
                                              "cwd": str(repo), "worktree": True})
    finish(client, first["jobId"], "SUCCEEDED")
    worktree = Path(read_job_record(area / "root" / "codex_exec_mcp" / "jobs" /
                                    (first["jobId"] + ".json"))["worktreePath"])
    (worktree / "tracked.txt").write_text("thread change\n", encoding="utf-8")
    reply = job_call(client, "codex_start", {"call": str(uuid.uuid4()), "mode": "reply",
                                              "threadId": "thread-123", "prompt": "reply"})
    finish(client, reply["jobId"], "SUCCEEDED")
    (worktree / "tracked.txt").write_text("thread change and reply\n", encoding="utf-8")
    return repo, tracked, worktree, first, reply

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
def _pid_alive(pid):
    if sys.platform.startswith("linux"):
        state, _created = codex_exec_mcp._posix_process_identity(pid)
        return state == "ALIVE"
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
# Kept: Python apply telemetry emitted queue_wait_ms in the bridge, but Go omitted it in the ported check.
def test_applied_scope_outcome_uses_recorded_queue_wait(area, monkeypatch):
    import ledger
    import telemetry

    repo = _git_repo(area)
    root = area / "root"
    project_key = codex_exec_mcp.project_root.project_key(repo.resolve())
    store = ledger.ScopeLeases(root / "projects" / project_key)
    job_id = "b" * 64
    store.acquire("blocker", "blocker-task", ["tracked.txt"], job_id="a" * 64)
    assert store.acquire("queued-owner", "queued-task", ["tracked.txt"], job_id=job_id,
                         queue_conflict=True)["code"] == "LEASE_QUEUED"
    record = store._read()
    record["owners"]["queued-owner"].update(state="ACTIVE", queued_at=10.0, acquired_at=13.25)
    store._write(record)
    index_path = root / "async_jobs" / f"{job_id}.json"
    index_path.parent.mkdir(parents=True)
    index_path.write_text(json.dumps({"jobId": job_id, "project_key": project_key,
                                      "call_key": "queued-owner"}), encoding="utf-8")
    captured = []
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: root)
    monkeypatch.setattr(telemetry, "emit", lambda *args, **kwargs: captured.append((args, kwargs)))

    codex_exec_mcp._record_scope_conflict_outcome(
        {"jobId": job_id, "taskId": "queued-task", "repoRoot": str(repo)}, "applied")

    args, payload = captured[0]
    assert args == ("SCOPE_CONFLICT_OUTCOME",)
    assert payload["action"] == "applied"
    assert payload["queue_wait_ms"] == 3250


# Kept: Python-only bridge acknowledgement and payload cleanup around Go host jobs.
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


# Kept: Python-only replacement-bridge supervision confirms child shutdown.
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
# Kept: Python-only mechanical routing selects measured OpenCode candidates.
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


# Kept: Python-only mechanical routing retries after a missing model.
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


# Kept: Python-only mechanical routing orders measured candidates before Codex fallback.
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


# Kept: Python-only mechanical routing records the Luna fallback identity.
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


# Kept: Python-only mechanical timeout handling does not take over Codex work.
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


# Kept: Python-only mechanical routing honors the configured fallback binding.
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


# Kept: Python-only mechanical retry policy keeps ambiguous OpenCode failures terminal.
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


# Kept: Python-only takeover logic attributes one Codex retry after an empty delta.
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


# Kept: Python-only mechanical retry policy preserves edits after failure.
def test_mechanical_failure_with_edits_is_not_retried(area, monkeypatch):
    server, job_id, calls = _mechanical_bridge(area, monkeypatch, changed=True)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": "vendor/free", "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"}

    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert len(calls) == 1
    assert result["isError"] is True


# Kept: Python-only mechanical takeover is limited to one Codex retry.
def test_mechanical_takeover_is_limited_to_one_codex_retry(area, monkeypatch):
    server, job_id, calls = _mechanical_bridge(area, monkeypatch, codex_fails=True)
    arguments = {"prompt": "same prompt", "cwd": str(area), "worktree": True,
                 "agent": "opencode", "model": "vendor/free", "mechanicalFallback": True,
                 "fallbackAgent": "codex", "fallbackModel": "gpt-6-sol", "fallbackEffort": "medium"}

    result = server._run_tool("codex", arguments, job_id, async_job=True)

    assert len(calls) == 2
    assert result["isError"] is True
# Kept: Python-only provider credit classification records HTTP 402 evidence.
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
# Kept: Python-only provider rate-limit parsing preserves optional retry timing.
def test_rate_limit_evidence_keeps_optional_wait(message, retry_after):
    # Assumed OpenCode ProviderError text; the error/data/message envelope is parser-compatible.
    event = {"type": "error", "error": {"name": "ProviderError", "data": {"message": message}}}
    evidence = codex_exec_mcp._launch_failure_evidence("opencode", [event], stdout_had_output=True)
    assert evidence == {"type": "RATE_LIMITED", "retryAfterSeconds": retry_after, "noWorkProof": False}


# Kept: Python-only Codex event evidence distinguishes lifecycle from item output.
def test_codex_lifecycle_before_402_is_credit_but_item_output_is_ambiguous():
    events = [
        {"type": "thread.started", "thread_id": "thread-123"},
        {"type": "turn.started"},
        {"type": "turn.failed", "error": {"message": "HTTP 402: insufficient balance; billing required"}},
    ]
    assert codex_exec_mcp._launch_failure_evidence("codex", events)["type"] == "CREDIT_EXHAUSTED"
    events.insert(2, {"type": "item.completed", "item": {"type": "agent_message", "text": "partial"}})
    assert codex_exec_mcp._launch_failure_evidence("codex", events)["type"] == "AMBIGUOUS"


# Kept: Python-only OpenCode event evidence distinguishes step starts from text.
def test_opencode_step_start_before_402_is_credit_but_text_is_ambiguous():
    events = [
        {"type": "step_start", "sessionID": "session-123"},
        {"type": "error", "error": {"name": "ProviderError", "data": {
            "message": "HTTP 402: insufficient balance; billing required"}}},
    ]
    assert codex_exec_mcp._launch_failure_evidence("opencode", events)["type"] == "CREDIT_EXHAUSTED"
    events.insert(1, {"type": "text", "sessionID": "session-123", "part": {"text": "partial"}})
    assert codex_exec_mcp._launch_failure_evidence("opencode", events)["type"] == "AMBIGUOUS"


# Kept: Python-only API retry policy uses bounded fixed retry offsets.
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


# Kept: Python-only API retry policy stops on ambiguous event evidence.
def test_ambiguous_event_never_retries():
    calls = []

    def run():
        calls.append(1)
        return {"typedEvidence": {"type": "AMBIGUOUS", "noWorkProof": False}}

    result = codex_exec_mcp._run_with_responsiveness_retries(run, clock=lambda: 0.0,
                                                              sleep=lambda _seconds: pytest.fail("unexpected retry"))
    assert calls == [1]
    assert result["responsivenessAttempts"] == 1


# Kept: Python-only API probe avoids failure evidence after eventual success.
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
# Kept: Python-only primary API probes invoke configured agent CLIs in scratch directories.
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


# Kept: Python-only API probe timeout remains an unhealthy result.
def test_api_primary_probe_timeout_is_not_healthy(area):
    fake = area / "slow_probe.py"
    fake.write_text("import time\ntime.sleep(30)\n", encoding="utf-8")
    result = codex_exec_mcp.probe_api_primary(
        "codex", "gpt-probe", codex_bin=json.dumps([sys.executable, str(fake)]), timeout=0.05)
    assert result["ok"] is False
    assert result["failure"]["type"] == "NO_RESPONSE"
# Kept: Python-only Anthropic spend-cap handling classifies provider credit exhaustion.
def test_anthropic_spend_cap_is_credit_exhaustion():
    # Assumed Anthropic error event wrapper; this structured spend-cap code is the classifier contract.
    event = {"type": "error", "error": {"type": "rate_limit_error", "details": {
        "error_code": "enforced_spend_limit_reached"}, "message": "Access resumes at midnight."}}
    evidence = codex_exec_mcp._launch_failure_evidence("opencode", [event])
    assert evidence == {"type": "CREDIT_EXHAUSTED", "retryAfterSeconds": None,
                        "noWorkProof": False}
