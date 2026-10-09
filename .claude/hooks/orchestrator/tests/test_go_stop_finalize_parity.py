import json
import os
from datetime import datetime
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "stop_finalize.json"
sys.path.insert(0, str(ORCHESTRATOR))
import claude_transcript_usage as usage
import telemetry
import telemetry_schema


class _ReachedAggregation(Exception):
    pass


class _FrozenDateTime(datetime):
    value = None

    @classmethod
    def now(cls, tz=None):
        return cls.value.astimezone(tz) if tz else cls.value


def _oracle_result(root, case, monkeypatch):
    session = case["session_id"]
    path = usage._active_task_path(root, session)
    record = case.get("record")
    if record is not None:
        path.parent.mkdir(parents=True, exist_ok=True)
        if isinstance(record, str):
            path.write_text(record, encoding="utf-8")
        else:
            path.write_text(json.dumps(record), encoding="utf-8")

    monkeypatch.setattr(usage.telemetry, "emit", lambda *args, **kwargs: None)
    monkeypatch.setattr(usage.telemetry, "update_task", lambda *args, **kwargs: None)
    monkeypatch.setattr(usage, "_atomic_json_write", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        usage, "read_incremental_records_from_offset",
        lambda *args, **kwargs: (_ for _ in ()).throw(_ReachedAggregation()),
    )
    monkeypatch.setattr(usage, "validate_transcript_path", lambda *args, **kwargs: ("fixture.jsonl", None))
    monkeypatch.setattr(usage, "check_transcript_identity", lambda *args, **kwargs: None)

    if case.get("lock_contention"):
        with usage.session_lock(root, session, timeout_seconds=0.02) as acquired:
            assert acquired
            return usage.finalize_stop_task(root, session_id=session, lock_timeout_seconds=0.02)
    try:
        return usage.finalize_stop_task(root, session_id=session)
    except _ReachedAggregation:
        return {"status": "READY"}


def test_p128_python_oracle_stop_finalize(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "claude_transcript_usage.finalize_stop_task"
    _FrozenDateTime.value = datetime.fromisoformat(fixture["now"].replace("Z", "+00:00"))
    monkeypatch.setattr(usage, "datetime", _FrozenDateTime)
    for case in fixture["cases"]:
        root = tmp_path / case["name"]
        root.mkdir()
        expected = _oracle_result(root, case, monkeypatch)
        if os.environ.get("STOP_FINALIZE_WRITE_FIXTURE") == "1":
            case["expected"] = expected
        else:
            assert expected == case["expected"], case["name"]
    if os.environ.get("STOP_FINALIZE_WRITE_FIXTURE") == "1":
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8", newline="\n")


def _write_case(root, name):
    session, task_id, tool_id = f"p130-{name}", f"task-{name}", f"tool-{name}"
    projects = root / "projects_allowed"
    projects.mkdir(parents=True)
    transcript = projects / f"{session}.jsonl"
    row = {"type": "assistant", "requestId": "req-anchor", "message": {
        "id": "anchor", "content": [{"type": "tool_use", "id": tool_id}],
        "usage": {"input_tokens": 7, "output_tokens": 3,
                  "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2}}}
    transcript.write_text(json.dumps(row, separators=(",", ":")) + "\n", encoding="utf-8", newline="\n")
    stat = transcript.stat()
    record = {"task_id": task_id, "tool_use_id": tool_id, "project_key": "0123456789abcdef",
              "canonical_transcript_path": os.path.normcase(os.path.realpath(transcript)), "transcript_dev": stat.st_dev,
              "transcript_ino": stat.st_ino, "transcript_ctime_ns": stat.st_ctime_ns,
              "initial_size": stat.st_size, "anchor_byte_offset": 0,
              "anchor_search_start_offset": 0, "anchor_deferred": False,
              "created_at": "2026-10-05T11:00:00Z", "completed": False,
              "invalidated": False, "affected_task_ids": [task_id]}
    if name == "missing-ids":
        record.pop("task_id")
    elif name == "stale":
        record["created_at"] = "2026-10-03T11:59:59Z"
    elif name == "identity-failure":
        record["canonical_transcript_path"] = str(projects / "different.jsonl")
    active = usage._active_task_path(root, session)
    active.parent.mkdir(parents=True, exist_ok=True)
    active.write_text(json.dumps(record), encoding="utf-8")
    if name == "contention":
        usage._mark_contention(root, session, "task-competing")
    if name == "persist-failure":
        events = root / "telemetry" / "events"
        events.parent.mkdir(parents=True, exist_ok=True)
        events.write_text("blocked", encoding="utf-8")
    return session, transcript, projects, active, task_id


def test_p130_python_oracle_stop_finalize_writes(tmp_path, monkeypatch):
    writing = os.environ.get("STOP_FINALIZE_WRITE_FIXTURE") == "1"
    fixture_path = FIXTURE.with_name("stop_finalize_writes.json")
    fixture = {"oracle": "claude_transcript_usage.finalize_stop_task", "cases": []}
    names = ["completed", "contention", "missing-ids", "stale", "identity-failure", "repeat", "persist-failure"]
    if not writing:
        fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
    frozen = datetime.fromisoformat("2026-10-05T12:00:00+00:00")
    _FrozenDateTime.value = frozen
    monkeypatch.setattr(usage, "datetime", _FrozenDateTime)
    monkeypatch.setattr(telemetry, "datetime", _FrozenDateTime)
    monkeypatch.setattr(telemetry_schema, "utc_now", lambda: "2026-10-05T12:00:00Z")
    counter = iter(range(1, 1000))
    monkeypatch.setattr(telemetry_schema.uuid, "uuid4", lambda: __import__("uuid").UUID(int=next(counter)))
    for name in names:
        case_root = tmp_path / name
        case_root.mkdir()
        session, transcript, projects, active, task_id = _write_case(case_root, name)
        if name == "repeat":
            first = usage.finalize_stop_task(case_root, session_id=session, transcript_path=transcript,
                                             allowed_projects_root=projects)
            second = usage.finalize_stop_task(case_root, session_id=session, transcript_path=transcript,
                                              allowed_projects_root=projects)
            result = {"first": first, "second": second}
        else:
            result = usage.finalize_stop_task(case_root, session_id=session, transcript_path=transcript,
                                              allowed_projects_root=projects)
        active_record = json.loads(active.read_text(encoding="utf-8")) if active.exists() else None
        if active_record:
            active_record["canonical_transcript_path"] = "<transcript>"
            for field in ("transcript_dev", "transcript_ino", "transcript_ctime_ns"):
                active_record.pop(field, None)
        event_path = case_root / "telemetry" / "events" / "by_task" / f"{task_id}.jsonl"
        events = [json.loads(line) for line in event_path.read_text(encoding="utf-8").splitlines()] if event_path.exists() else []
        for event in events:
            event["event_id"], event["ts_utc"] = "00000000-0000-0000-0000-000000000001", "2026-10-05T12:00:00Z"
        projection_path = case_root / "projects" / "0123456789abcdef" / "telemetry" / "tasks" / f"{task_id}.json"
        projection = json.loads(projection_path.read_text(encoding="utf-8")) if projection_path.exists() else None
        if projection is not None:
            projection = {key: projection[key] for key in ("task_id", "project_key", "status", "usage")}
        observed = {"result": result, "active": active_record, "events": events, "projection": projection}
        if writing:
            fixture["cases"].append({"name": name, "expected": observed})
        else:
            expected = next(item["expected"] for item in fixture["cases"] if item["name"] == name)
            assert observed == expected, name
    if writing:
        fixture_path.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8", newline="\n")
