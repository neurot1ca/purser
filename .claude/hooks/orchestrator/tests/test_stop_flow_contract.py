"""Python oracle fixture for the standalone Go Stop flow helper."""
import json
import os
from pathlib import Path
import subprocess
import sys

PACKAGE = Path(__file__).resolve().parents[1]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "stop_flow.json"
sys.path.insert(0, str(PACKAGE))
import compaction_advisor


def _records(*items):
    return "".join(json.dumps(item, ensure_ascii=False) + "\n" for item in items)


def _flow_records(final="Proceed?"):
    return [
        {"type": "user", "message": {"content": "Continue the task."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "result-1",
            "name": "mcp__plugin_purser_codex-main__codex_result"}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "result-1",
            "content": "Completed successfully"}]}},
        {"type": "assistant", "message": {"content": final}},
    ]


def test_python_stop_flow_fixture(tmp_path, monkeypatch):
    cases = []
    repository = tmp_path / "repo"
    repository.mkdir()
    state_root = tmp_path / "state"
    monkeypatch.setattr(compaction_advisor.state_root, "default_state_root", lambda: state_root)
    monkeypatch.setattr(compaction_advisor, "_state_directory", lambda _repo: state_root)

    def run_case(name, records, blocked_user=-1, has_reasons=False, tail=""):
        transcript = tmp_path / f"{name}.jsonl"
        transcript.write_text(_records(*records) + tail, encoding="utf-8")
        session = "stop-flow-" + name
        state_path = state_root / "compaction_advisor.json"
        state_path.parent.mkdir(parents=True, exist_ok=True)
        state_path.write_text(json.dumps({"session_id": session, "flow_blocked_user": None if blocked_user < 0 else blocked_user}), encoding="utf-8")
        if has_reasons:
            compaction_advisor.queue_version_announcement({"session_id": session}, repository, "0.1.99")
        event = {"session_id": session, "transcript_path": str(transcript), "last_assistant_message": records[-1]["message"]["content"]}
        result = compaction_advisor.stop(event, repository)
        state = json.loads(state_path.read_text(encoding="utf-8"))
        reason = result["reason"].split("\n\nYour previous reply", 1)[0] if result else ""
        flow = "Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question."
        return {"name": name, "records": compaction_advisor._read_transcript(str(transcript)),
                "raw_transcript": _records(*records) + tail,
                "blocked_user": blocked_user, "has_reasons": has_reasons,
                "expected": flow if reason == flow else "",
                "reason": reason,
                "expected_blocked_user": state.get("flow_blocked_user") if state.get("flow_blocked_user") is not None else -1}

    cases.append(run_case("successful_result_handled", _flow_records("Complete.")))

    repeated_records = _flow_records()
    transcript = tmp_path / "repeated.jsonl"
    transcript.write_text(_records(*repeated_records), encoding="utf-8")
    session = "stop-flow-repeated"
    state_path = state_root / "compaction_advisor.json"
    state_path.write_text(json.dumps({"session_id": session}), encoding="utf-8")
    event = {"session_id": session, "transcript_path": str(transcript), "last_assistant_message": "Proceed?"}
    first = compaction_advisor.stop(event, repository)
    second = compaction_advisor.stop(event, repository)
    flow = "Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question."
    cases.append({"name": "same_user_turn_suppressed", "records": repeated_records, "blocked_user": -1,
                  "has_reasons": False, "expected": flow if first else "",
                  "expected_blocked_user": 0})
    cases.append({"name": "duplicate_reminder_suppressed", "records": repeated_records, "blocked_user": 0,
                  "has_reasons": False, "expected": "", "expected_blocked_user": 0})
    new_records = repeated_records + [{"type": "user", "message": {"content": "Next part."}}, *repeated_records[1:]]
    transcript.write_text(_records(*new_records), encoding="utf-8")
    third = compaction_advisor.stop({**event, "last_assistant_message": "Proceed?"}, repository)
    cases.append({"name": "new_user_turn_allowed", "records": new_records, "blocked_user": 0,
                  "has_reasons": False, "expected": flow if third else "",
                  "expected_blocked_user": 4})

    cases.append(run_case("advisor_reason_precedes_flow", _flow_records(), has_reasons=True))
    cases.append(run_case("malformed_transcript_tail", _flow_records(), tail='{"broken":',))
    failed = _flow_records()
    failed[2]["message"]["content"][0]["is_error"] = True
    cases.append(run_case("failed_result_does_not_flow_block", failed))

    denied = [
        {"type": "user", "message": {"content": "Retry this."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "denied",
            "name": "mcp__plugin_purser_codex-main__codex_start"}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "denied",
            "is_error": True, "content": "classifier denied permission"}]}},
        {"type": "assistant", "message": {"content": "Proceed?"}},
    ]
    result = run_case("retry_and_advisor_order", denied, has_reasons=True)
    reason = result["reason"]
    result.pop("reason")
    suffix = "Include one visible line containing both `⚓` and `0.1.99` to announce the Purser update."
    prefix = "Retry the denied codex_start call."
    assert reason.startswith(prefix + "\n\n"), reason
    advisor_reason = reason[len(prefix) + 2:]
    for case in cases:
        latest_user = max((i for i, record in enumerate(case["records"])
                           if compaction_advisor._genuine_user(record)), default=-1)
        case["retry_expected"] = compaction_advisor._codex_retry_reason(case["records"], latest_user, repository) or ""
    expected = {"oracle": "compaction_advisor.stop", "cases": cases,
                "ordering": {"retry": prefix, "advisor": [advisor_reason], "flow": "",
                             "expected": [prefix, advisor_reason]}}
    if os.environ.get("STOP_FLOW_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set STOP_FLOW_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected
    assert suffix in advisor_reason
    env = os.environ.copy()
    env["GOCACHE"] = str(Path(__file__).resolve().parents[4] / ".tmp" / "go-build-cache")
    result = subprocess.run(["go", "test", "./internal/contextio/", "-run", "^TestPythonStopFlowGoldenParity$", "-count=1"],
                            cwd=Path(__file__).resolve().parents[4] / "go", text=True, capture_output=True,
                            check=False, timeout=120, env=env)
    assert result.returncode == 0, result.stdout + result.stderr
