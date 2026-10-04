import json
from datetime import datetime
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "active_task_start.json"
sys.path.insert(0, str(ORCHESTRATOR))
import claude_transcript_usage as usage


class _FrozenDateTime(datetime):
    value = None

    @classmethod
    def now(cls, tz=None):
        return cls.value.astimezone(tz) if tz else cls.value


def _replace_now(value, now):
    if value == "$NOW":
        return now
    if isinstance(value, dict):
        return {key: _replace_now(item, now) for key, item in value.items()}
    if isinstance(value, list):
        return [_replace_now(item, now) for item in value]
    return value


def test_p61_python_oracle_active_task_start(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "claude_transcript_usage.start_active_task"
    now = datetime.fromisoformat(fixture["now"].replace("Z", "+00:00"))
    _FrozenDateTime.value = now
    monkeypatch.setattr(usage, "datetime", _FrozenDateTime)
    events = []
    monkeypatch.setattr(usage.telemetry, "emit", lambda event, **payload: events.append((event, payload)))
    monkeypatch.setattr(usage.telemetry, "update_task", lambda *args, **kwargs: None)

    for case in fixture["cases"]:
        assert "p61" in case["name"]
        root = tmp_path / case["name"]
        root.mkdir()
        projects = root / "projects"
        projects.mkdir()
        now_text = now.isoformat(timespec="microseconds").replace("+00:00", "Z")
        expected_record = _replace_now(case["expected_record"], now_text)
        active_path = usage._active_task_path(root, case["session_id"])
        existing = _replace_now(case.get("existing"), now_text)
        if existing is not None:
            active_path.parent.mkdir(parents=True, exist_ok=True)
            active_path.write_text(json.dumps(existing), encoding="utf-8")

        transcript_path = None
        if "transcript" in case or case.get("oversized_transcript"):
            transcript_path = projects / f"{case['session_id']}.jsonl"
            content = case.get("transcript", "x" * (usage.MAX_LINE_LENGTH + 1) + "\n")
            transcript_path.write_text(content, encoding="utf-8")

        if case.get("lock_contention"):
            with usage.session_lock(root, case["session_id"], timeout_seconds=0.02) as held:
                assert held
                result = usage.start_active_task(
                    root, session_id=case["session_id"], task_id=case["task_id"],
                    tool_use_id=case["tool_use_id"], transcript_path=transcript_path,
                    allowed_projects_root=projects, lock_timeout_seconds=0.02,
                )
        else:
            result = usage.start_active_task(
                root, session_id=case["session_id"], task_id=case["task_id"],
                project_key=case.get("project_key"), tool_use_id=case["tool_use_id"],
                transcript_path=transcript_path, allowed_projects_root=projects,
            )

        assert result["status"] == case["expected_status"], case["name"]
        if "expected_reason" in case:
            assert result["reason"] == case["expected_reason"], case["name"]
        if expected_record is None:
            assert not active_path.exists(), case["name"]
            assert "active_task" not in result, case["name"]
        else:
            record = json.loads(active_path.read_text(encoding="utf-8"))
            assert record == expected_record, case["name"]
            if case["expected_status"] in ("STARTED", "RETAINED"):
                assert result["active_task"] == record, case["name"]
        if case["name"] == "p61-unknown-usage-update":
            assert any(
                event == "CLAUDE_TASK_USAGE_RECORDED"
                and payload.get("claude_task_usage_unknown_reason") == case["expected_reason"]
                and payload.get("claude_task_usage_known") is False
                for event, payload in events
            )
