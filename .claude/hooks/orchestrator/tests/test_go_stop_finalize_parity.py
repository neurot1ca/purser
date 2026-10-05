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
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
