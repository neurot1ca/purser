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


def read_job(root, job_id):
    return json.loads((root / "codex_exec_mcp" / "jobs" / f"{job_id}.json").read_text(encoding="utf-8"))


def test_terminal_job_returns_immediately(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "terminal", state="SUCCEEDED")

    assert purser_wait.main(["terminal"]) == 0
    assert capsys.readouterr().out == "purser-wait terminal SUCCEEDED\n"
    assert read_job(tmp_path, "terminal")["waiterObservedTerminalState"] == "SUCCEEDED"
    assert isinstance(read_job(tmp_path, "terminal")["waiterObservedTerminalAt"], float)


def test_terminal_failed_job_is_non_success(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "failed", state="FAILED")

    assert purser_wait.main(["failed"]) == 1
    assert capsys.readouterr().out == "purser-wait failed FAILED\n"
    assert read_job(tmp_path, "failed")["waiterObservedTerminalState"] == "FAILED"


def test_terminal_observation_write_failure_preserves_waiter_result(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    write_job(tmp_path, "failed", state="FAILED")
    monkeypatch.setattr(purser_wait.codex_exec_mcp, "_atomic_json",
                        lambda *_args: (_ for _ in ()).throw(OSError("disk full")))

    assert purser_wait.main(["failed"]) == 1
    assert capsys.readouterr().out == "purser-wait failed FAILED\n"
    assert "waiterObservedTerminalAt" not in read_job(tmp_path, "failed")


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
    write_job(tmp_path, "closed", signalPort=port, deadlineAt=time.time() - 1)

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


def test_wait_follows_replacement_generation_endpoint_and_deadline(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    old = socket.socket()
    old.bind(("127.0.0.1", 0))
    old.listen()
    replacement = socket.socket()
    replacement.bind(("127.0.0.1", 0))
    replacement.listen()
    write_job(tmp_path, "replaced", signalPort=old.getsockname()[1], generation=1,
              deadlineAt=time.time() + 0.2)
    received = []

    def serve():
        connection, _ = old.accept()
        with connection:
            received.append(connection.recv(256))
            write_job(tmp_path, "replaced", state="QUEUED", generation=2,
                      signalPort=replacement.getsockname()[1], deadlineAt=time.time() + 2)
        old.close()
        connection, _ = replacement.accept()
        with connection:
            received.append(connection.recv(256))
            write_job(tmp_path, "replaced", state="RUNNING", generation=2,
                      signalPort=replacement.getsockname()[1], deadlineAt=time.time() + 2)
            time.sleep(0.25)
            write_job(tmp_path, "replaced", state="SUCCEEDED", generation=2,
                      signalPort=replacement.getsockname()[1])
            connection.sendall(b"1")
        replacement.close()

    thread = threading.Thread(target=serve)
    thread.start()
    assert purser_wait.main(["replaced"]) == 0
    thread.join(timeout=2)
    assert received == [b"replaced:secret", b"replaced:secret"]
    assert capsys.readouterr().out == "purser-wait replaced SUCCEEDED\n"


def test_unknown_job_returns_two_and_one_line(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))

    assert purser_wait.main(["unknown"]) == 2
    assert capsys.readouterr().out == "purser-wait unknown UNKNOWN_JOB\n"
