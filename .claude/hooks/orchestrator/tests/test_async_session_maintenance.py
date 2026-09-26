import json
import os
import hashlib
from pathlib import Path
import sys
from datetime import datetime, timezone

sys.path.insert(0, str(Path(__file__).parents[1]))
import async_session_maintenance as maintenance
import codex_exec_mcp
import ledger
import telemetry_schema


PROJECT_KEY = "0123456789abcdef"


def event(kind, route=None, duration=None, project=PROJECT_KEY, stamp="2026-09-01T00:00:00Z"):
    payload = {}
    if route is not None:
        payload["route"] = route
    if duration is not None:
        payload["duration_ms"] = duration
    return {"event_type": kind, "project_key": project, "payload": payload, "ts_utc": stamp}


def test_duration_hint_reads_real_layout_and_skips_irrelevant_records(tmp_path):
    events_root = tmp_path / "telemetry" / "events"
    september = events_root / "2026-09"
    september.mkdir(parents=True)
    sample_events = [event("IMPLEMENTER_FINISHED", "mechanical", 120_000) for _ in range(10)]
    sample_events += [event("IMPLEMENTER_FINISHED", duration=600_000)]
    sample_events += [event("IMPLEMENTER_FINISHED", "subtle", 600_000, project="fedcba9876543210")]
    sample_events += [event("IMPLEMENTER_FAILED", "subtle", 900_000)]
    contents = "{malformed json\n" + "\n".join(json.dumps(item) for item in sample_events)
    for index in range(36):
        path = september / f"session_{index:03}.jsonl"
        path.write_text(contents if index == 35 else "{}\n", encoding="utf-8")
        os.utime(path, (index + 1, index + 1))

    july = events_root / "2026-07"
    july.mkdir()
    (july / "session_boundary.jsonl").write_text(
        json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 9_000_000, stamp="2026-07-02T23:59:00Z")) + "\n"
        + json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 9_000_000, stamp="2026-07-04T00:00:00Z")),
        encoding="utf-8",
    )
    by_task = events_root / "by_task"
    by_task.mkdir()
    (by_task / "duplicate.jsonl").write_text(
        "\n".join(json.dumps(item) for item in sample_events[:10]), encoding="utf-8"
    )

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 10, 1, tzinfo=timezone.utc).timestamp()
    )

    assert hint == "Purser: typical Codex durations here (median/p90, n): mechanical 2/2 min (n=10)."


def test_duration_hint_uses_newest_files_when_file_byte_and_line_caps_hit(tmp_path, monkeypatch):
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    older = month / "session_older.jsonl"
    older.write_text("\n".join(json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 600_000)) for _ in range(10)),
                     encoding="utf-8")
    newest = month / "session_newest.jsonl"
    latest_lines = [json.dumps(event("IMPLEMENTER_FINISHED", "mechanical", 120_000)).encode() + b"\n"
                    for _ in range(11)]
    newest.write_bytes(b"".join(latest_lines))
    os.utime(older, (1, 1))
    os.utime(newest, (2, 2))
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_FILES", 1)
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_LINES", 10)
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_BYTES", sum(map(len, latest_lines[:10])))

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 10, 1, tzinfo=timezone.utc).timestamp()
    )

    assert hint == "Purser: typical Codex durations here (median/p90, n): mechanical 2/2 min (n=10)."


def test_cleanup_live_job_preserves_pending_and_lease(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "RUNNING", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: True)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 9 * 86400)
    assert (tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json").exists()
    assert pending.exists()
    assert lease._read()["owners"][job_id]["state"] == "ACTIVE"


def _job_fixture(root, state, finished, *, stage="PENDING"):
    call = "job-" + state + "-" + stage
    job_id = hashlib.sha256(call.encode()).hexdigest()
    jobs = root / "codex_exec_mcp" / "jobs"
    indices = root / "async_jobs"
    pending_dir = root / "pending"
    for directory in (jobs, indices, pending_dir):
        directory.mkdir(parents=True, exist_ok=True)
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "call": call, "state": state, "acceptedAt": 1,
        "finishedAt": finished, "outputFile": f"{job_id}.output"}), encoding="utf-8")
    (jobs / f"{job_id}.output").write_text("result", encoding="utf-8")
    (jobs / f"{job_id}.launch").write_text("secret prompt", encoding="utf-8")
    (indices / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "call": call, "call_key": job_id,
        "project_key": PROJECT_KEY, "stage": stage}), encoding="utf-8")
    pending = pending_dir / f"{job_id}.json"
    pending.write_text(json.dumps({"job_id": job_id, "async_call": call,
        "lease_owner": job_id, "route": "subtle", "project_key": PROJECT_KEY}), encoding="utf-8")
    lease = ledger.ScopeLeases(root / "projects" / PROJECT_KEY)
    assert lease.acquire(job_id, "task-a", ["file.py"])["status"] == ledger.OK
    return job_id, pending, lease


def test_cleanup_dead_host_reconciles_once_to_ambiguous(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "RUNNING", 1)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=1000)
    record = tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    first = json.loads(record.read_text())
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert json.loads(record.read_text()) == first
    assert first["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert first["errorCode"] == "PROCESS_UNCONFIRMED"
    assert "unconfirmed" in first["message"]
    assert pending.exists() and lease._read()["owners"][job_id]["state"] == "ACTIVE"
    assert not record.with_suffix(".launch").exists()


def test_cleanup_consumed_job_does_not_refinalize(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000, stage="CONSUMED")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    emitted = []
    monkeypatch.setattr(maintenance.telemetry, "emit", lambda *a, **kw: emitted.append(a))
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert (tmp_path / "async_jobs" / f"{job_id}.json").exists()
    assert pending.exists() and not emitted


def test_cleanup_removes_expired_terminal_evidence_pending_and_lease_once(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    emitted = []
    monkeypatch.setattr(maintenance.telemetry, "emit", lambda *a, **kw: emitted.append(a))
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    assert not (jobs / f"{job_id}.json").exists()
    assert not (jobs / f"{job_id}.output").exists()
    assert not (tmp_path / "async_jobs" / f"{job_id}.json").exists()
    assert not pending.exists()
    assert lease._read()["owners"][job_id]["state"] == "RELEASED"
    assert len(emitted) == 1 and emitted[0][0] == "CODEX_RESULT_UNFETCHED"


def test_cleanup_removes_launch_payload_for_recent_terminal_job(tmp_path, monkeypatch):
    job_id, pending, _ = _job_fixture(tmp_path, "FAILED", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    assert (jobs / f"{job_id}.json").exists() and pending.exists()
    assert not (jobs / f"{job_id}.launch").exists()


def test_cleanup_preserves_finalizing_evidence_for_retry(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000, stage="FINALIZING")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    assert (tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json").exists()
    assert pending.exists() and lease._read()["owners"][job_id]["state"] == "ACTIVE"
