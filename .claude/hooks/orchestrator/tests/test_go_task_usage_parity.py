import json
import os
from pathlib import Path
import sys
import telemetry

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "task_usage.json"
sys.path.insert(0, str(ORCHESTRATOR))
import claude_transcript_usage as usage


def test_p59_python_oracle_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"].startswith("claude_transcript_usage.")
    for case in fixture["cases"]:
        if "p59" not in case["name"]:
            continue
        assert "p59" in case["name"]
        projects = tmp_path / case["name"] / "projects"
        projects.mkdir(parents=True)
        if case["op"] == "path":
            path = projects / case["file"] if case["file"] else ""
            if case["file"] and case["file"] != "missing.jsonl":
                target = tmp_path / case["name"] / case["file"] if case.get("outside") else path
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(b"")
                if case.get("outside"):
                    path = target
            _, reason = usage.validate_transcript_path(path, case["session"], projects)
        else:
            path = projects / "session-1.jsonl"
            path.write_bytes(case["content"].encode("utf-8"))
            if case["op"].startswith("read"):
                offset = case.get("offset", 0)
                records, reason = usage.read_incremental_records_from_offset(
                    path, offset, allow_incomplete_tail=case["op"] == "read-allow"
                )
                if not reason:
                    assert len(records) == case["records"], case["name"]
            else:
                anchor, cursor, deferred, reason = usage.scan_for_anchor_at_pre_tool_use(path, case["tool"])
                assert anchor == case["anchor"], case["name"]
                assert cursor == case["cursor"], case["name"]
                assert deferred is case["deferred"], case["name"]
        assert (reason or "") == case["expected"], case["name"]


def test_p60_python_oracle_contention_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert "session_lock" in fixture["oracle"]
    for case in fixture["cases"]:
        if "p60" not in case["name"]:
            continue
        root = tmp_path / case["name"]
        root.mkdir()
        session = case.get("session", "p60-session")
        if case["op"] == "lock":
            with usage.session_lock(root, session, timeout_seconds=0.02) as acquired:
                assert acquired is (case["expected"] == "true")
        elif case["op"] == "lock-timeout":
            with usage.session_lock(root, session, timeout_seconds=0.02) as held:
                assert held
                with usage.session_lock(root, session, timeout_seconds=0.02) as acquired:
                    assert acquired is (case["expected"] == "true")
        elif case["op"] == "marker-compete":
            first = usage._mark_contention(root, session, case["task"])
            second = usage._mark_contention(root, session, case["other"])
            present, task_id = usage._read_contention_marker(root, session)
            assert f"{first},{second}" == case["expected"]
            assert present and task_id == case["marker_id"]
        elif case["op"] == "marker-existing":
            marker = usage._contention_marker_path(root, session)
            marker.parent.mkdir(parents=True)
            marker.write_text(case["task"], encoding="utf-8")
            present, task_id = usage._read_contention_marker(root, session)
            assert str(present).lower() == case["expected"]
            assert task_id == case["marker_id"]
        elif case["op"] == "marker-clear-failure":
            marker = usage._contention_marker_path(root, session)
            marker.mkdir(parents=True)
            assert usage._read_contention_marker(root, session) == (True, None)
            assert usage._remove_contention_marker(root, session) is (case["expected"] == "true")
        elif case["op"] == "terminal-preserved":
            aggregate = {"repo": None, "branch": None, "usage": {
                "claude_task_usage_unknown_reason": case["reason"],
                "claude_task_usage_known": False,
                "claude_task_usage_complete": False,
            }}
            telemetry._fold_event(aggregate, {"event_type": "CLAUDE_TASK_USAGE_RECORDED", "payload": {
                "claude_task_usage_known": True, "claude_task_usage_complete": True,
                "claude_task_input_tokens": 5,
            }})
            assert aggregate["usage"]["claude_task_usage_unknown_reason"] == case["expected"]
            assert aggregate["usage"]["claude_task_usage_known"] is False
        else:
            raise AssertionError(f"unknown p60 op {case['op']}")


def test_p59_identity_and_incremental_bounds(tmp_path):
    path = tmp_path / "session.jsonl"
    path.write_bytes(b"{}\n")
    identity = usage.get_transcript_identity(path)
    assert usage.check_transcript_identity(path, identity, None, None) is None
    assert usage.check_transcript_identity(path, {**identity, "canonical_path": "elsewhere"}, None, None) == "TRANSCRIPT_IDENTITY_MISMATCH"
    assert usage.check_transcript_identity(path, identity, 4, None) == "TRANSCRIPT_ROTATED_OR_TRUNCATED"
    path.write_bytes(b"")
    assert usage.check_transcript_identity(path, identity, None, None) == "TRANSCRIPT_ROTATED_OR_TRUNCATED"
    path.write_bytes(b"x" * (usage.MAX_INCREMENTAL_BYTES + 1))
    _, reason = usage.read_incremental_records_from_offset(path, 0)
    assert reason == "BOUNDS_EXCEEDED"
    path.write_bytes(b"x" * (usage.MAX_LINE_LENGTH + 1) + b"\n")
    _, reason = usage.read_incremental_records_from_offset(path, 0)
    assert reason == "BOUNDS_EXCEEDED"
