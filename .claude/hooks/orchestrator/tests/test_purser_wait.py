import json
import socket
import sys
import threading
import time
from pathlib import Path

import purser_wait


def write_job(root, job_id, **values):
    folder = root / "codex_exec_mcp" / "jobs"
    folder.mkdir(parents=True, exist_ok=True)
    job = {"jobId": job_id, "state": "RUNNING", "signalPort": 1,
           "signalToken": "secret", "deadlineAt": time.time() + 60}
    job.update(values)
    (folder / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")


def test_terminal_job_returns_immediately(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "terminal", state="SUCCEEDED")

    assert purser_wait.main(["terminal"]) == 0
    assert capsys.readouterr().out == "purser-wait terminal SUCCEEDED\n"


def test_terminal_failed_job_is_non_success(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "failed", state="FAILED")

    assert purser_wait.main(["failed"]) == 1
    assert capsys.readouterr().out == "purser-wait failed FAILED\n"


def test_job_record_read_retries_transient_permission_error(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "retry", state="SUCCEEDED")
    original = Path.read_text
    attempts = 0

    def flaky_read(path, *args, **kwargs):
        nonlocal attempts
        if path.name == "retry.json" and attempts < 2:
            attempts += 1
            raise PermissionError(13, "sharing violation", str(path))
        return original(path, *args, **kwargs)

    monkeypatch.setattr(Path, "read_text", flaky_read)
    assert purser_wait.main(["retry"]) == 0
    assert attempts == 2
    assert capsys.readouterr().out == "purser-wait retry SUCCEEDED\n"


def test_running_job_waits_for_listener_signal(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]
    write_job(tmp_path, "running", signalPort=port)
    received = []

    def serve():
        connection, _ = listener.accept()
        with connection:
            received.append(connection.recv(256))
            write_job(tmp_path, "running", state="SUCCEEDED", signalPort=port)
            connection.sendall(b"1")
        listener.close()

    thread = threading.Thread(target=serve)
    thread.start()
    assert purser_wait.main(["running"]) == 0
    thread.join(timeout=2)
    assert received == [b"running:secret"]
    assert capsys.readouterr().out == "purser-wait running SUCCEEDED\n"


def test_dead_host_reports_recorded_running_state(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    port = listener.getsockname()[1]
    listener.close()
    write_job(tmp_path, "closed", signalPort=port)

    assert purser_wait.main(["closed"]) == 3
    assert capsys.readouterr().out == "purser-wait closed RUNNING\n"


def test_wait_timeout_returns_four(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "timeout", deadlineAt=time.time() + 0.1)
    observed = {}

    class TimedOut:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def sendall(self, data):
            assert data == b"timeout:secret"

        def recv(self, size):
            raise socket.timeout

        def settimeout(self, value):
            observed["timeout"] = value

    monkeypatch.setattr(purser_wait.socket, "create_connection", lambda address, timeout: TimedOut())
    assert purser_wait.main(["timeout"]) == 3
    assert observed["timeout"] <= 61
    assert capsys.readouterr().out == "purser-wait timeout RUNNING\n"


def test_unknown_job_returns_two_and_one_line(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))

    assert purser_wait.main(["unknown"]) == 2
    assert capsys.readouterr().out == "purser-wait unknown UNKNOWN_JOB\n"
