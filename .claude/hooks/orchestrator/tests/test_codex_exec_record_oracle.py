"""Capture Python-created Codex job records for the Go migration oracle."""

import hashlib
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace

import pytest

ROOT = Path(__file__).parents[1]
sys.path.insert(0, str(ROOT))
import codex_exec_mcp as bridge
import ledger

FIXTURES = Path(__file__).parent / "fixtures" / "codex_exec_records"
STATES = ("queued", "running", "finished", "failed", "cancelled", "interrupted-host")


def _normalize(value, root, executable):
    if isinstance(value, str):
        return (value.replace(str(executable).replace("\\", "\\\\"), "__CODEX__")
                .replace(str(executable), "__CODEX__")
                .replace(str(root).replace("\\", "\\\\"), "__ROOT__")
                .replace(str(root), "__ROOT__"))
    if isinstance(value, list):
        return [_normalize(item, root, executable) for item in value]
    if isinstance(value, dict):
        return {key: _normalize(item, root, executable) for key, item in value.items()}
    return value


def _capture(root, state, monkeypatch):
    root.mkdir(parents=True)
    executable = root / "fake-codex"
    executable.write_text("never executed", encoding="utf-8")
    jobs = root / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    monkeypatch.setenv("PURSER_CODEX_SHIM_STATE", str(root / "threads"))
    monkeypatch.setattr(bridge, "_launch_host", lambda *args: None)
    monkeypatch.setattr(bridge.time, "time", lambda: 1700000000.0)
    counter = iter(range(1, 100))
    monkeypatch.setattr(bridge.uuid, "uuid4", lambda: SimpleNamespace(hex=f"{next(counter):032x}"))
    server = bridge.Server(str(executable), job_dir=jobs, agent="codex")
    server._await_host = lambda job_id: bridge._job_result({"jobId": job_id, "state": "QUEUED"})
    call = "00000000-0000-4000-8000-000000000141"
    job_id = hashlib.sha256(call.encode("utf-8")).hexdigest()
    result = server._start_job({"call": call, "prompt": "oracle prompt", "cwd": str(root),
                                "timeoutSeconds": 30})
    assert result.get("structuredContent", {}).get("jobId") == job_id, result
    job = server._read_job(job_id)
    if state == "running" or state == "interrupted-host":
        job.update(state="RUNNING", startedAt=1700000001.0, hostPid=4242,
                   hostCreatedAt=1699999999, hostContained=True, signalPort=32145, pid=4243)
        job["unknownGoField"] = {"kept": True}
        server._write_job(job)
        rewritten_job = server._read_job(job_id)
        server._write_job(rewritten_job)
        assert server._read_job(job_id)["unknownGoField"] == {"kept": True}
    if state == "running":
        thread = {"thread_id": "thread-oracle", "cwd": str(root), "config_args": ["model=x"],
                  "unknownGoField": {"kept": True}}
        bridge._save_record(server.state_dir, thread)
        rewritten_thread = bridge._load_record(server.state_dir, "thread-oracle")
        bridge._save_record(server.state_dir, rewritten_thread)
        assert bridge._load_record(server.state_dir, "thread-oracle")["unknownGoField"] == {"kept": True}
    if state in ("finished", "failed", "cancelled"):
        final = {"finished": "SUCCEEDED", "failed": "FAILED", "cancelled": "CANCELLED"}[state]
        server._finish_job(job_id, final, "CODEX_FAILED" if state == "failed" else None,
                           {"content": "oracle output", "threadId": "thread-oracle"}
                           if state == "finished" else None)
    if state == "interrupted-host":
        root_state = jobs.parent.parent
        project_key = "fixture-project"
        call_key = "fixture-owner"
        pending_dir = root_state / "pending"
        index_dir = root_state / "async_jobs"
        pending_dir.mkdir()
        index_dir.mkdir()
        (pending_dir / f"{call_key}.json").write_text(json.dumps({
            "job_id": job_id, "async_call": call, "lease_owner": call_key,
        }), encoding="utf-8")
        index = {"jobId": job_id, "call": call, "call_key": call_key,
                 "project_key": project_key, "stage": "RUNNING"}
        (index_dir / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
        lease = ledger.ScopeLeases(root_state / "projects" / project_key)
        assert lease.acquire(call_key, "fixture-task", ["src"], job_id=job_id)["status"] == ledger.OK
        job = server._read_job(job_id)
        job.update(threadId="thread-oracle", threadConfig={"cwd": str(root), "config_args": ["model=x"]})
        server._write_job(job)
        before_recovery = {}
        for path in sorted(root.rglob("*")):
            if path.is_file() and path != executable:
                before_recovery[path.relative_to(root).as_posix()] = path.read_text(encoding="utf-8")
        monkeypatch.setattr(server, "_classify_host", lambda _job: "DEAD")
        monkeypatch.setattr(bridge, "_posix_process_group_stopped", lambda _pid: True)
        assert server.recover_job(job_id, now=1700000002.0) == "RECOVERED"
    else:
        before_recovery = None

    # Exercise the real ownership lock for each captured case.
    with bridge._ownership_lock(jobs, job_id):
        pass
    artifacts = {}
    for path in sorted(root.rglob("*")):
        if path.is_file() and path != executable:
            rel = path.relative_to(root).as_posix()
            artifacts[rel] = path.read_text(encoding="utf-8")
    return {"files": _normalize(artifacts, root, executable),
            "recoveryInputs": _normalize(before_recovery, root, executable)}


@pytest.mark.parametrize("state", STATES)
def test_python_record_fixture(tmp_path, monkeypatch, state):
    actual = {"case": state, **_capture(tmp_path / state, state, monkeypatch)}
    fixture = FIXTURES / f"{state}.json"
    if os.environ.get("PURSER_REGEN_RECORD_FIXTURES") == "1":
        fixture.parent.mkdir(parents=True, exist_ok=True)
        fixture.write_text(json.dumps(actual, sort_keys=True, separators=(",", ":")) + "\n",
                           encoding="utf-8")
    assert fixture.exists(), f"missing fixture: {fixture} (set PURSER_REGEN_RECORD_FIXTURES=1 to capture)"
    assert json.loads(fixture.read_text(encoding="utf-8")) == actual
