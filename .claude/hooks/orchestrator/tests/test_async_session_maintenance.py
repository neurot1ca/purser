import json
import os
from pathlib import Path
import sys
from datetime import datetime, timezone

sys.path.insert(0, str(Path(__file__).parents[1]))
import async_session_maintenance as maintenance
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


def test_cleanup_removes_only_old_terminal_jobs_and_reports_unfetched_once(tmp_path, monkeypatch):
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    indices = tmp_path / "async_jobs"
    jobs.mkdir(parents=True)
    indices.mkdir()
    emitted = []
    monkeypatch.setattr(maintenance.telemetry, "emit", lambda *a, **kw: emitted.append((a, kw)))

    def make(job_id, state, finished, *, stage="PENDING"):
        output_name = job_id + ".output"
        (jobs / f"{job_id}.json").write_text(json.dumps({"jobId": job_id, "state": state,
            "finishedAt": finished, "outputFile": output_name}), encoding="utf-8")
        (jobs / output_name).write_text("result", encoding="utf-8")
        (indices / f"{job_id}.json").write_text(json.dumps({"jobId": job_id, "call": job_id,
            "call_key": job_id, "project_key": PROJECT_KEY, "stage": stage}), encoding="utf-8")

    old = 1000
    now = old + 8 * 86400
    make("a" * 64, "SUCCEEDED", old)
    make("b" * 64, "FAILED", now - 3600)
    make("c" * 64, "RUNNING", old)
    (tmp_path / "pending").mkdir()
    (tmp_path / "pending" / ("a" * 64 + ".json")).write_text(json.dumps({
        "task_id": "task-a", "route": "subtle", "project_key": PROJECT_KEY}), encoding="utf-8")

    maintenance.cleanup_jobs(tmp_path, now=now)
    maintenance.cleanup_jobs(tmp_path, now=now)

    assert not (jobs / ("a" * 64 + ".json")).exists()
    assert not (jobs / ("a" * 64 + ".output")).exists()
    assert (jobs / ("b" * 64 + ".json")).exists()
    assert (jobs / ("c" * 64 + ".json")).exists()
    assert len(emitted) == 1
    assert emitted[0][0][0] == "CODEX_RESULT_UNFETCHED"
    assert (indices / ("a" * 64 + ".json")).exists()
    diagnostic = telemetry_schema.build_event(
        "CODEX_RESULT_UNFETCHED", project_key=PROJECT_KEY,
        payload={"code": "RESULT_NOT_FETCHED", "route": "subtle"}
    )
    assert telemetry_schema.valid_event(diagnostic)
