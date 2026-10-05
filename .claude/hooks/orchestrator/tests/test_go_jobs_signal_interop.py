import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import sys
import threading
import time

import pytest


ROOT = Path(__file__).parents[1]
GO_ROOT = ROOT.parents[2] / "go"
sys.path.insert(0, str(ROOT))
import codex_exec_mcp
import purser_wait


@pytest.fixture(scope="module")
def signal_test_binary():
    scratch = GO_ROOT / ".tmp"
    scratch.mkdir(exist_ok=True)
    binary = scratch / ("jobs-signal.test.exe" if os.name == "nt" else "jobs-signal.test")
    subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/jobs"],
                   cwd=GO_ROOT, check=True, timeout=120)
    return binary


def _wait_for_file(path, process):
    until = time.monotonic() + 10
    while time.monotonic() < until:
        if path.exists():
            return
        if process.poll() is not None:
            raise AssertionError(f"Go signal host exited with {process.returncode}")
        time.sleep(0.01)
    process.kill()
    raise AssertionError("Go signal host did not publish its endpoint")


def _read_record(path):
    text = codex_exec_mcp.purser_state_root.retry_permission_error(
        lambda: path.read_text(encoding="utf-8"))
    return json.loads(text)


def test_python_waiter_to_go_host(signal_test_binary, tmp_path, monkeypatch):
    job_id, token = "a" * 64, "b" * 32
    state = tmp_path / "state"
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    record_path = jobs / f"{job_id}.json"
    ready = tmp_path / "ready"
    record = {"jobId": job_id, "state": "RUNNING", "signalPort": 1,
              "signalToken": token, "generation": 1, "deadlineAt": time.time() + 10}
    record_path.write_text(json.dumps(record), encoding="utf-8")
    env = os.environ.copy()
    env.update({"PURSER_SIGNAL_HOST_HELPER": "1", "PURSER_SIGNAL_JOB": job_id,
                "PURSER_SIGNAL_TOKEN": token, "PURSER_SIGNAL_RECORD": str(record_path),
                "PURSER_SIGNAL_READY": str(ready)})
    process = subprocess.Popen([str(signal_test_binary), "-test.run=^TestSignalHostProcess$"], env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        _wait_for_file(ready, process)
        port = int(ready.read_text(encoding="ascii"))
        record["signalPort"] = port
        record_path.write_text(json.dumps(record), encoding="utf-8")
        with socket.create_connection(("127.0.0.1", port), timeout=1) as connection:
            connection.sendall(f"{job_id}:{token}:PING".encode("ascii"))
            assert connection.recv(1) == b"1"
        with socket.create_connection(("127.0.0.1", port), timeout=1) as connection:
            connection.sendall(f"{job_id}:wrong:PING".encode("ascii"))
            assert connection.recv(1) == b""
        monkeypatch.setenv("PURSER_STATE_DIR", str(state))
        waiter_result = queue.Queue()
        waiter = threading.Thread(target=lambda: waiter_result.put(purser_wait.main([job_id])))
        waiter.start()
        time.sleep(0.05)
        with socket.create_connection(("127.0.0.1", port), timeout=1) as connection:
            connection.sendall(f"{job_id}:{token}:CANCEL".encode("ascii"))
            assert connection.recv(1) == b"1"

        waiter.join(timeout=3)
        assert not waiter.is_alive() and waiter_result.get_nowait() == 1
        durable = json.loads(record_path.read_text(encoding="utf-8"))
        assert durable["state"] == "CANCELLED"
        assert durable["waiterObservedTerminalState"] == "CANCELLED"
        assert process.wait(timeout=3) == 0
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=3)


def test_go_client_to_python_host(signal_test_binary, tmp_path):
    job_id, token = "c" * 64, "d" * 32
    jobs = tmp_path / "jobs"
    jobs.mkdir()
    fake = tmp_path / "fake_agent.py"
    fake.write_text(
        "import json, time\n"
        "print(json.dumps({'type':'thread.started','thread_id':'fake-thread'}), flush=True)\n"
        "time.sleep(30)\n",
        encoding="utf-8")
    record = {"jobId": job_id, "state": "QUEUED", "signalToken": token,
              "generation": 1, "ownershipToken": "owner", "acceptedAt": time.time(),
              "deadlineAt": time.time() + 20}
    (jobs / f"{job_id}.json").write_text(json.dumps(record), encoding="utf-8")
    (jobs / f"{job_id}.launch").write_text(json.dumps({
        "name": "codex", "codexBin": json.dumps([sys.executable, str(fake)]),
        "args": {"cwd": str(tmp_path), "prompt": "signal integration", "timeoutSeconds": 20},
    }), encoding="utf-8")
    env = os.environ.copy()
    env["PURSER_STATE_DIR"] = str(tmp_path / "state")
    host = subprocess.Popen([sys.executable, "-u", str(ROOT / "codex_exec_mcp.py"),
                             "--host-job", job_id, "--host-job-dir", str(jobs),
                             "--generation", "1", "--ownership-token", "owner"],
                            cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        record_path = jobs / f"{job_id}.json"
        until = time.monotonic() + 15
        while time.monotonic() < until:
            record = _read_record(record_path)
            if record.get("signalPort") and record.get("pid"):
                break
            if host.poll() is not None:
                raise AssertionError(f"Python host exited with {host.returncode}")
            time.sleep(0.02)
        else:
            raise AssertionError("Python host did not start its fake agent")
        env.update({"PURSER_SIGNAL_CLIENT_HELPER": "1", "PURSER_SIGNAL_JOB": job_id,
                    "PURSER_SIGNAL_TOKEN": token, "PURSER_SIGNAL_PORT": str(record["signalPort"])})
        result = subprocess.run([str(signal_test_binary), "-test.run=^TestSignalClientProcess$"],
                                env=env, check=True, capture_output=True, text=True, timeout=10)
        payload = json.loads(next(line for line in result.stdout.splitlines() if line.startswith("{")))
        assert payload == {"ping": True, "wrongRejected": True, "cancel": True, "wait": True}
        assert host.wait(timeout=10) == 0
        final = _read_record(record_path)
        assert final["state"] == "CANCELLED"
    finally:
        if host.poll() is None:
            host.kill()
            host.wait(timeout=3)
