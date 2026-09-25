"""Focused protocol tests; the real Codex executable is never launched."""

import importlib.util
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
    time.sleep(2)
thread = os.environ.get("FAKE_THREAD", "thread-123")
print(json.dumps({"type": "thread.started", "thread_id": thread}), flush=True)
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
        shutil.rmtree(path)


class Client:
    def __init__(self, area, *, bin_override=None, extra_env=None):
        fake = area / "fake_codex.py"
        fake.write_text(FAKE, encoding="utf-8")
        self.record = area / "calls.jsonl"
        env = os.environ.copy()
        env.update({"PURSER_CODEX_SHIM_STATE": str(area / "state"), "PURSER_STATE_DIR": str(area / "root"), "FAKE_RECORD": str(self.record)})
        env.update(extra_env or {})
        override = bin_override if bin_override is not None else json.dumps([sys.executable, str(fake)])
        self.process = subprocess.Popen(
            [sys.executable, "-u", str(SCRIPT), "--codex-bin", override],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, encoding="utf-8", env=env,
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


def test_async_start_result_idempotency_and_sync(area):
    client = Client(area, extra_env={"FAKE_SLOW": "1"})
    try:
        key = str(uuid.uuid4())
        started = job_call(client, "codex_start", {"call": key, "prompt": "slow"})
        assert started["state"] == "RUNNING" and started["acceptedAt"] < started["deadlineAt"]
        assert job_call(client, "codex_result", {"jobId": started["jobId"]})["state"] == "RUNNING"
        assert job_call(client, "codex_start", {"call": key, "prompt": "slow"})["jobId"] == started["jobId"]
        record = json.loads((area / "root" / "codex_exec_mcp" / "jobs" / (started["jobId"] + ".json")).read_text(encoding="utf-8"))
        with socket.create_connection(("127.0.0.1", record["signalPort"]), timeout=4) as signal_connection:
            signal_connection.sendall(f"{started['jobId']}:{record['signalToken']}".encode("ascii"))
            assert signal_connection.recv(1) == b"1"
        result = finish(client, started["jobId"], "SUCCEEDED")
        assert (result["threadId"], result["content"]) == ("thread-123", "Reply from fake Codex")
        assert len(client.calls()) == 1
        assert codex._extract_codex_result(call(client, 4, "codex", {"prompt": "sync"})) == ("thread-123", "Reply from fake Codex")
    finally:
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
        record = json.loads((area / "root" / "codex_exec_mcp" / "jobs" / (job["jobId"] + ".json")).read_text(encoding="utf-8"))
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
        client.process.kill()
        client.process.wait()
        restarted = Client(area)
        try:
            assert job_call(restarted, "codex_result", {"jobId": pending["jobId"]})["state"] == "UNKNOWN_OR_IN_PROGRESS"
        finally:
            restarted.close()
    finally:
        if client.process.poll() is None:
            client.close()
