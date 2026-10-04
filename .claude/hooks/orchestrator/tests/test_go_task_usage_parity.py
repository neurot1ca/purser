import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "task_usage.json"
sys.path.insert(0, str(ORCHESTRATOR))
import claude_transcript_usage as usage


def test_p59_python_oracle_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"].startswith("claude_transcript_usage.")
    for case in fixture["cases"]:
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
