import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "stop_usage.json"
sys.path.insert(0, str(ORCHESTRATOR))
import claude_transcript_usage as usage


def _assistant(message_id, tool_use_id=None, input_tokens=0, output_tokens=0, sidechain=False):
    content = [{"type": "tool_use", "id": tool_use_id}] if tool_use_id else []
    message = {"id": message_id, "content": content,
               "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens,
                         "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2}}
    return {"type": "assistant", "message": message, "requestId": "req-" + message_id,
            "isSidechain": sidechain}


def _cases():
    return [
        {"name": "p129-normal", "transcript": [_assistant("anchor", "tool-129", 7, 3), _assistant("next", input_tokens=5, output_tokens=2)]},
        {"name": "p129-outside-root", "outside": True, "transcript": [_assistant("anchor", "tool-129", 7, 3)]},
        {"name": "p129-identity-changed", "identity_changed": True, "transcript": [_assistant("anchor", "tool-129", 7, 3)]},
        {"name": "p129-transcript-shrank", "shrank": True, "transcript": [_assistant("anchor", "tool-129", 7, 3)]},
        {"name": "p129-subagent-usage-included", "transcript": [_assistant("anchor", "tool-129", 7, 3), _assistant("subagent", input_tokens=11, output_tokens=4, sidechain=True)]},
        {"name": "p129-missing-usage-fields", "transcript": [{"type": "assistant", "requestId": "req-anchor", "message": {"id": "anchor", "content": [{"type": "tool_use", "id": "tool-129"}], "usage": {"input_tokens": 7}}}]},
        {"name": "p129-malformed-transcript-line", "raw_transcript": "not-json\n", "transcript": []},
    ]


def _oracle(root, case, monkeypatch):
    session = "p129-session"
    projects = root / "projects"
    projects.mkdir(parents=True)
    transcript_dir = root / ("outside" if case.get("outside") else "projects")
    if case.get("outside"):
        transcript_dir.mkdir()
    transcript = transcript_dir / f"{session}.jsonl"
    raw = case.get("raw_transcript")
    if raw is None:
        raw = "".join(json.dumps(row, separators=(",", ":")) + "\n" for row in case["transcript"])
    transcript.write_text(raw, encoding="utf-8")
    stat = transcript.stat()
    record = {
        "task_id": "task-129", "tool_use_id": "tool-129", "project_key": "fixture",
        "canonical_transcript_path": os.path.normcase(os.path.realpath(transcript)), "transcript_dev": stat.st_dev,
        "transcript_ino": stat.st_ino, "transcript_ctime_ns": stat.st_ctime_ns,
        "initial_size": stat.st_size, "anchor_byte_offset": 0, "anchor_search_start_offset": 0,
        "anchor_deferred": False, "created_at": "2026-10-05T00:00:00Z",
        "completed": False, "invalidated": False,
    }
    if case.get("identity_changed"):
        record["canonical_transcript_path"] = str(projects / "different.jsonl")
    if case.get("shrank"):
        record["initial_size"] += 1
    active = usage._active_task_path(root, session)
    active.parent.mkdir(parents=True, exist_ok=True)
    active.write_text(json.dumps(record), encoding="utf-8")

    monkeypatch.setattr(usage.telemetry, "emit", lambda *args, **kwargs: None)
    monkeypatch.setattr(usage.telemetry, "update_task", lambda *args, **kwargs: None)
    monkeypatch.setattr(usage, "_atomic_json_write", lambda *args, **kwargs: None)
    return usage.finalize_stop_task(root, session_id=session, transcript_path=transcript,
                                    allowed_projects_root=projects)


def test_p129_python_oracle_stop_usage(tmp_path, monkeypatch):
    writing = os.environ.get("STOP_USAGE_WRITE_FIXTURE") == "1"
    fixture = {"oracle": "claude_transcript_usage.finalize_stop_task", "cases": _cases()}
    if not writing:
        fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "claude_transcript_usage.finalize_stop_task"
    for case in fixture["cases"]:
        result = _oracle(tmp_path / case["name"], case, monkeypatch)
        if writing:
            case["expected"] = result
        else:
            assert result == case["expected"], case["name"]
    if writing:
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
