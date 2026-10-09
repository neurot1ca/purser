"""Python parser oracle for the Go jobs event-stream parser."""
import json
import os
from pathlib import Path
import sys

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURES = ORCH / "tests" / "fixtures" / "go_jobs_events"
sys.path.insert(0, str(ORCH))
import codex_exec_mcp


def _line(event):
    return json.dumps(event, ensure_ascii=False, separators=(",", ":"))


def _cases():
    huge_stderr = "start-" + "λ🙂" * 3000 + "-end"
    return {
        "codex_success": {
            "agent": "codex",
            "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-ok"}),
                                   _line({"type": "item.completed", "item": {"type": "agent_message", "text": "done"}}),
                                   _line({"type": "turn.completed", "usage": {"input_tokens": 3}}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_error_event": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-error"}),
                                                         _line({"type": "turn.failed", "error": {"message": "quota denied"}}))) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": False,
        },
        "codex_cut_mid_line": {
            "agent": "codex", "stdout": _line({"type": "turn.completed"}),
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_no_terminal": {
            "agent": "codex", "stdout": _line({"type": "thread.started", "thread_id": "t-open"}) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_thread_before_failure": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "resume-me"}),
                                                         _line({"type": "turn.failed", "error": {"message": "later failure"}}))) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": False,
        },
        "codex_huge_stderr": {
            "agent": "codex", "stdout": "", "stderr": huge_stderr,
            "exitCode": 1, "interrupted": False,
        },
        "codex_unicode": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-界🙂"}),
                                                       _line({"type": "item.completed", "item": {"type": "agent_message", "text": "héllo 世界 🌱"}}),
                                                       _line({"type": "turn.completed"}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_usage_absent": {
            "agent": "codex", "stdout": _line({"type": "turn.completed"}) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_usage_present": {
            "agent": "codex", "stdout": _line({"type": "turn.completed", "usage": {
                "input_tokens": 4, "cached_input_tokens": 2, "output_tokens": 1,
                "reasoning_output_tokens": 0, "ignored": 9, "bad": -1, "bool": True,
            }}) + "\n", "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_usage_latest_missing": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "turn.completed", "usage": {"output_tokens": 7}}),
                                                             _line({"type": "turn.completed"}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_interrupted": {
            "agent": "codex", "stdout": _line({"type": "thread.started", "thread_id": "t-stop"}) + "\n",
            "stderr": "", "exitCode": -1, "interrupted": True,
        },
        "opencode_success": {
            "agent": "opencode", "stdout": "\n".join((_line({"type": "text", "sessionID": "oc-1", "part": {"text": "hi "}}),
                                                             _line({"type": "text", "sessionID": "oc-1", "part": {"text": "there"}}),
                                                             _line({"type": "step_finish", "sessionID": "oc-1", "part": {"reason": "stop"}}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "opencode_error": {
            "agent": "opencode", "stdout": _line({"type": "error", "sessionID": "oc-err", "error": {"message": "工具失败"}}) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": False,
        },
        "opencode_failure_then_success": {
            "agent": "opencode", "stdout": "\n".join((
                _line({"type": "tool_use", "sessionID": "oc-order", "part": {"tool": "write", "state": {"status": "error", "error": "first tool failed"}}}),
                _line({"type": "step_finish", "sessionID": "oc-order", "part": {"reason": "stop"}}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "opencode_multiple_failures": {
            "agent": "opencode", "stdout": "\n".join((
                _line({"type": "tool_use", "sessionID": "oc-many", "part": {"tool": "write", "state": {"status": "error", "error": "first failure"}}}),
                _line({"type": "error", "sessionID": "oc-many", "error": {"message": "second failure"}}),
                _line({"type": "step_finish", "sessionID": "oc-many", "part": {"reason": "stop"}}))) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": False,
        },
        "agy_success": {
            "agent": "agy", "stdout": _line({"conversation_id": "agy-1", "status": "SUCCESS", "response": "完成 🙂"}) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "agy_error": {
            "agent": "agy", "stdout": _line({"conversation_id": "agy-err", "status": "FAILED", "error": "Antigravity failed"}) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": False,
        },
        "agy_nonterminal_without_status": {
            "agent": "agy", "stdout": _line({"type": "progress", "conversation_id": "agy-progress", "message": "still working"}) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": False,
        },
        "codex_interrupted_after_success": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-interrupt-success"}),
                                                         _line({"type": "item.completed", "item": {"type": "agent_message", "text": "done"}}),
                                                         _line({"type": "turn.completed"}))) + "\n",
            "stderr": "", "exitCode": 0, "interrupted": True,
        },
        "codex_interrupted_after_failure": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-interrupt-failure"}),
                                                         _line({"type": "turn.failed", "error": {"message": "provider failed"}}))) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": True,
        },
        "codex_interrupted_after_success_and_failure": {
            "agent": "codex", "stdout": "\n".join((_line({"type": "thread.started", "thread_id": "t-interrupt-both"}),
                                                         _line({"type": "turn.completed"}),
                                                         _line({"type": "turn.failed", "error": {"message": "late failure"}}))) + "\n",
            "stderr": "", "exitCode": 1, "interrupted": True,
        },
    }


def _expected(case):
    agent = case["agent"]
    events = []
    for line in case["stdout"].splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except (ValueError, UnicodeError):
            continue
        if isinstance(event, dict):
            events.append(event)
    thread_id = ""
    failures = []
    final_parts = []
    notes = []
    terminal = False
    success = False
    for event in events:
        session, marker, failure, final = codex_exec_mcp._agent_event(agent, event)
        if session and not thread_id:
            thread_id = session
        if failure:
            failures.append(failure)
        note = codex_exec_mcp._opencode_tool_note(event)
        if note:
            notes.append(note)
        if final is not None:
            final_parts.append(final)
        terminal |= marker
        if agent == "codex" and event.get("type") == "turn.completed":
            success = True
        elif agent == "opencode" and event.get("type") == "step_finish" and event.get("part", {}).get("reason") == "stop":
            success = True
        elif agent == "agy" and isinstance(event.get("status"), str) and event["status"].casefold() == "success":
            success = True
    status, failure = "incomplete", ""
    if case["interrupted"]:
        status = "interrupted"
        cancelled = json.loads(codex_exec_mcp._tool_result(None, error="cancelled")["content"][0]["text"])
        failure = cancelled["error"]
    elif failures:
        status, failure = "error", "\n".join(failures)
    elif case["exitCode"] != 0:
        status, failure = "error", "".join(list(case["stderr"])[-4000:]) or f"exit code {case['exitCode']}"
    elif terminal and success:
        status = "success"
    usage = codex_exec_mcp._codex_turn_usage(events) if agent == "codex" else {}
    final = "".join(final_parts) if agent == "opencode" else (final_parts[-1] if final_parts else "")
    result = {"status": status}
    if thread_id:
        result["threadId"] = thread_id
    if failure:
        result["failure"] = failure
    if final and status != "interrupted":
        result["final"] = final
    if notes:
        result["notes"] = notes
    if usage and status != "interrupted":
        result["usage"] = usage
    return result


def _write_fixtures():
    FIXTURES.mkdir(parents=True, exist_ok=True)
    cases = _cases()
    for stale in FIXTURES.glob("*.json"):
        stale.unlink()
    for name, case in cases.items():
        fixture = {**case, "expected": _expected(case)}
        (FIXTURES / f"{name}.json").write_text(
            json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


if os.environ.get("PURSER_REGEN_GO_JOBS_EVENTS") == "1":
    _write_fixtures()


@pytest.mark.parametrize("name,case", sorted(_cases().items()))
def test_go_jobs_event_fixture_matches_python_oracle(name, case):
    path = FIXTURES / f"{name}.json"
    assert path.is_file(), f"missing {path}; run with PURSER_REGEN_GO_JOBS_EVENTS=1"
    fixture = json.loads(path.read_text(encoding="utf-8"))
    assert {key: fixture[key] for key in case} == case
    assert fixture["expected"] == _expected(case)


def test_opencode_tool_error_note_does_not_mask_process_failure():
    event = {"type": "tool_use", "sessionID": "oc-fail", "part":
             {"tool": "write", "state": {"status": "error", "error": "Permission denied"}}}
    session, terminal, failure, final = codex_exec_mcp._agent_event("opencode", event)
    assert (session, terminal, failure, final) == ("oc-fail", False, None, None)
    assert codex_exec_mcp._opencode_tool_note(event) == "OpenCode tool error: write: Permission denied"
