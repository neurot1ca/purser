import hashlib
import json
import os
from pathlib import Path
import socket
import sys
from threading import Thread

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "enforce" / "testdata" / "post_async_cancel_python_fixture.json"
sys.path.insert(0, str(ORCH))
import codex


def _cancel_case(tmp_path, name):
    call = f"async-call:{name}"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True, exist_ok=True)
    if name in {"authenticated_cancel", "wrong_auth_cancel", "terminal_cancel"}:
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        seen = []

        def bridge():
            connection, _ = listener.accept()
            with connection:
                seen.append(connection.recv(1024).decode("ascii"))
                connection.sendall(b"1" if name in {"authenticated_cancel", "terminal_cancel"} else b"0")

        thread = Thread(target=bridge)
        thread.start()
    else:
        port, seen = 1, []
        listener = None
        thread = None
    job = {"jobId": job_id, "call": call, "state": "CANCELLED" if name == "terminal_cancel" else "RUNNING",
           "signalPort": port}
    if name != "absent_auth_cancel":
        job["signalToken"] = "bridge-secret"
    (jobs / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
    result = codex._cancel_start_job(tmp_path, call, job_id)
    if listener is not None:
        thread.join(timeout=3)
        listener.close()
        assert not thread.is_alive()
    return {"cancelled": result, "request": seen[0] if seen else None}


def _ownership_case(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir(parents=True)
    call = "async-call:cancel_response_other_project"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    original = {"tool_name": codex.TOOL if hasattr(codex, "TOOL") else "mcp__codex-main__codex",
                "session_id": "owner-session", "tool_use_id": "origin"}
    key = codex._call_key(original)
    project = "owned-project"
    index = {"jobId": job_id, "project_key": project, "call_key": key,
             "session_id": "owner-session", "call": call, "stage": "ACTIVE"}
    pending = {"tool_use_id": "origin", "job_id": job_id, "async_call": call,
               "session_id": "owner-session", "project_key": project,
               "task_id": "task", "baseline": {}}
    (tmp_path / "async_jobs").mkdir()
    (tmp_path / "pending").mkdir()
    (tmp_path / "codex_exec_mcp" / "jobs").mkdir(parents=True)
    (tmp_path / "async_jobs" / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
    (tmp_path / "pending" / f"{key}.json").write_text(json.dumps(pending), encoding="utf-8")
    (tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json").write_text(
        json.dumps({"jobId": job_id, "call": call}), encoding="utf-8")
    event = {"tool_name": "mcp__codex-main__codex_cancel", "session_id": "other-session",
             "cwd": str(repo), "tool_input": {"jobId": job_id}}
    try:
        codex._async_owned(tmp_path, event, repo)
    except codex.WiringError as error:
        return {"error": type(error).__name__, "message": str(error)}
    raise AssertionError("foreign cancel response was accepted")


def _oracle(tmp_path):
    cases = []
    for name in ("authenticated_cancel", "wrong_auth_cancel", "absent_auth_cancel", "terminal_cancel"):
        cases.append({"name": name, "expected": _cancel_case(tmp_path / name, name)})
    for name, record in (("terminal_job", {"state": "CANCELLED"}),
                         ("running_job", {"state": "RUNNING"}),
                         ("start_job_absent", None), ("malformed_job", "{")):
        root = tmp_path / name
        call = f"async-call:{name}"
        job_id = hashlib.sha256(call.encode()).hexdigest()
        job_path = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
        if record is not None:
            job_path.parent.mkdir(parents=True)
            data = {"jobId": job_id, "call": call, **record} if isinstance(record, dict) else record
            job_path.write_text(data if isinstance(data, str) else json.dumps(data), encoding="utf-8")
        cases.append({"name": name, "expected": codex._start_job_terminal_or_absent(root, {"async_call": call})})
    cases.append({"name": "cancel_response_other_project", "expected": _ownership_case(tmp_path / "foreign")})
    return {"oracle": "codex._cancel_start_job+_start_job_terminal_or_absent+_async_owned", "cases": cases}


def test_post_async_cancel_matches_python_fixture(tmp_path):
    actual = _oracle(tmp_path)
    if os.environ.get("POST_ASYNC_CANCEL_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == actual
    assert [case["name"] for case in actual["cases"]] == [
        "authenticated_cancel", "wrong_auth_cancel", "absent_auth_cancel", "terminal_cancel", "terminal_job", "running_job",
        "start_job_absent", "malformed_job", "cancel_response_other_project"]
