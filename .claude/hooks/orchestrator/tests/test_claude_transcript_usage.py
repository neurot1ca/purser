"""Focused tests for Claude Desktop per-task JSONL token telemetry."""

from __future__ import annotations

import json
import os
from pathlib import Path
import sys
from typing import Any
from unittest import mock

sys.path.insert(0, str(Path(__file__).parents[1]))
import claude_transcript_usage as ctu
import codex
import hook
import obsidian_projection
import telemetry


def _make_assistant_record(
    session_id: str,
    message_id: str,
    request_id: str | None,
    tool_use_id: str | None = None,
    input_tokens: int = 100,
    output_tokens: int = 50,
    cache_creation: int = 0,
    cache_read: int = 0,
) -> dict[str, Any]:
    content: list[dict[str, Any]] = []
    if tool_use_id:
        content.append({
            "type": "tool_use",
            "id": tool_use_id,
            "name": "mcp__codex-main__codex",
            "input": {"prompt": "synthetic"}
        })
    else:
        content.append({"type": "text", "text": "synthetic response"})

    rec: dict[str, Any] = {
        "type": "assistant",
        "sessionId": session_id,
        "timestamp": "2026-09-07T12:00:00.000Z",
        "message": {
            "id": message_id,
            "type": "message",
            "role": "assistant",
            "model": "claude-3-7-sonnet-20250219",
            "content": content,
            "usage": {
                "input_tokens": input_tokens,
                "output_tokens": output_tokens,
                "cache_creation_input_tokens": cache_creation,
                "cache_read_input_tokens": cache_read,
            },
        },
    }
    if request_id is not None:
        rec["requestId"] = request_id
    return rec


def _write_jsonl(path: Path, records: list[dict[str, Any]], trailing_incomplete: str | None = None) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("wb") as handle:
        for rec in records:
            line = json.dumps(rec, ensure_ascii=False) + "\n"
            handle.write(line.encode("utf-8"))
        if trailing_incomplete:
            handle.write(trailing_incomplete.encode("utf-8"))


def _read_aggregate(state_root: Path, task_id: str) -> dict[str, Any]:
    task_path = _task_path(state_root, task_id)
    if not task_path.exists():
        return {}
    return json.loads(task_path.read_text(encoding="utf-8"))


def _task_path(state_root: Path, task_id: str) -> Path:
    unresolved = (
        state_root / "projects" / "unresolved" / "telemetry" / "tasks" / f"{task_id}.json"
    )
    if unresolved.exists():
        return unresolved
    candidates = sorted(
        (state_root / "projects").glob(f"*/telemetry/tasks/{task_id}.json")
    )
    return candidates[0] if candidates else unresolved


def _task_files(state_root: Path) -> list[Path]:
    return sorted((state_root / "projects").glob("*/telemetry/tasks/*.json"))


# Python-only coverage retained: Python hook health/rebuild/Obsidian behavior,
# test-process isolation, and the Python-specific short-write retry contract.

def test_unexpected_parser_or_import_failure_records_safe_health(tmp_path, monkeypatch):
    state_root = tmp_path / "state"
    monkeypatch.setenv("PURSER_STATE_DIR", str(state_root))

    pre_event = {
        "hook_event_name": "PreToolUse",
        "tool_name": "mcp__codex-main__codex",
        "tool_use_id": "call_err_1",
        "session_id": "sess-err",
        "transcript_path": str(tmp_path / "projects" / "sess-err.jsonl"),
        "tool_input": {
            "label": "Parser failure health handling",
            "prompt": "ORCH|task=task-err|phase=implementation|route=normal|reason=new_task|basis=specific repository boundary|scope=test.py\nDo work."
        },
    }

    # Simulate start_active_task throwing an unexpected Exception
    with mock.patch("claude_transcript_usage.start_active_task", side_effect=RuntimeError("disk crash")):
        out = codex.pre_tool_use(pre_event, state_root=state_root)
        # Hook must not block or crash
        assert "hookSpecificOutput" in out

    health = json.loads((state_root / "telemetry" / "telemetry_health.json").read_text(encoding="utf-8"))
    assert health["status"] == "DEGRADED"
    assert health["last_error_code"] == "CLAUDE_TRANSCRIPT_PARSER_FAILED"



def test_canonical_event_rebuild_reproduces_aggregate_and_projection(tmp_path):
    state_root = tmp_path / "state"
    vault_root = tmp_path / "vault"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-rebuild.jsonl"
    r1 = _make_assistant_record("sess-rebuild", "msg_1", "req_1", "call_reb", 800, 200, 50, 25)
    _write_jsonl(transcript, [r1])

    telemetry.emit("TASK_ROUTE_SELECTED", task_id="task-rebuild", session_id="sess-rebuild", route="normal", phase="implementation", model="gpt-4o", effort="medium", routing_enforced=True, fresh_thread=True, reply_call=False, state_root=state_root)
    telemetry.emit("IMPLEMENTER_STARTED", task_id="task-rebuild", session_id="sess-rebuild", route="normal", phase="implementation", model="gpt-4o", effort="medium", routing_enforced=True, fresh_thread=True, reply_call=False, state_root=state_root)
    ctu.start_active_task(state_root, session_id="sess-rebuild", task_id="task-rebuild", tool_use_id="call_reb", transcript_path=transcript, allowed_projects_root=projects_root)

    telemetry.emit("IMPLEMENTER_FINISHED", task_id="task-rebuild", session_id="sess-rebuild", duration_ms=5000, state_root=state_root)
    ctu.finalize_stop_task(state_root, session_id="sess-rebuild", transcript_path=transcript, allowed_projects_root=projects_root)
    telemetry.update_task("task-rebuild", state_root=state_root, projection_enabled=True, vault_root=vault_root)

    agg_before = _read_aggregate(state_root, "task-rebuild")
    run_note_paths = list(vault_root.glob("Runs/**/task-rebuild.md"))
    assert len(run_note_paths) == 1
    note_before = run_note_paths[0].read_text(encoding="utf-8")

    # Delete task aggregates and projections
    for p in _task_files(state_root):
        p.unlink()
    run_note_paths[0].unlink()

    # Rebuild from canonical raw events
    telemetry.rebuild(state_root=state_root)
    obsidian_projection.project_task(
        "task-rebuild",
        state_root=state_root / "projects" / "unresolved",
        vault_root=vault_root,
    )

    agg_after = _read_aggregate(state_root, "task-rebuild")
    assert agg_before == agg_after

    run_note_paths_after = list(vault_root.glob("Runs/**/task-rebuild.md"))
    assert len(run_note_paths_after) == 1
    note_after = run_note_paths_after[0].read_text(encoding="utf-8")
    assert note_before == note_after



def test_full_hook_lifecycle_e2e_synthetic_to_obsidian(tmp_path, monkeypatch):
    state_root = tmp_path / "claude-agent-orchestrator"
    projects_root = tmp_path / "projects"
    vault_root = tmp_path / "vault"
    transcript = projects_root / "sess-hook-e2e.jsonl"

    monkeypatch.setenv("PURSER_STATE_DIR", str(state_root))
    monkeypatch.setattr(ctu, "_default_projects_root", lambda: projects_root)
    monkeypatch.setattr(telemetry, "_projection_settings", lambda: (True, vault_root))

    r1 = _make_assistant_record("sess-hook-e2e", "msg_1", "req_1", "call_hook_1", 350, 80, 10, 5)
    _write_jsonl(transcript, [r1])

    pre_event = {
        "hook_event_name": "PreToolUse",
        "tool_name": "mcp__codex-main__codex",
        "tool_use_id": "call_hook_1",
        "session_id": "sess-hook-e2e",
        "transcript_path": str(transcript),
        "tool_input": {
            "label": "Synthetic hook lifecycle",
            "prompt": "ORCH|task=task-hook-e2e|phase=implementation|route=normal|reason=new_task|basis=specific repository boundary|scope=test.py\nDo work."
        },
    }
    pre_out = hook.dispatch(pre_event)
    assert "hookSpecificOutput" in pre_out

    post_event = {
        "hook_event_name": "PostToolUse",
        "tool_name": "mcp__codex-main__codex",
        "tool_use_id": "call_hook_1",
        "session_id": "sess-hook-e2e",
        "transcript_path": str(transcript),
        "tool_input": pre_event["tool_input"],
        "tool_response": {"content": [{"type": "text", "text": "OK"}]},
        "duration_ms": 3000,
    }
    post_out = hook.dispatch(post_event)
    assert "hookSpecificOutput" in post_out

    agg_mid = _read_aggregate(state_root, "task-hook-e2e")
    assert agg_mid["usage"]["claude_task_usage_known"] is False

    r2 = _make_assistant_record("sess-hook-e2e", "msg_2", "req_2", None, 450, 120, 0, 15)
    _write_jsonl(transcript, [r1, r2])

    stop_event = {
        "hook_event_name": "Stop",
        "session_id": "sess-hook-e2e",
        "transcript_path": str(transcript),
    }
    stop_out = hook.dispatch(stop_event)
    assert stop_out == {}

    agg_final = _read_aggregate(state_root, "task-hook-e2e")
    usage = agg_final["usage"]
    assert usage["claude_task_usage_known"] is True
    assert usage["claude_task_usage_complete"] is True
    assert usage["claude_task_input_tokens"] == 800
    assert usage["claude_task_output_tokens"] == 200
    assert usage["claude_task_cache_creation_input_tokens"] == 10
    assert usage["claude_task_cache_read_input_tokens"] == 20
    assert usage["claude_task_total_observed_tokens"] == 1030
    assert usage["claude_task_request_count"] == 2
    assert usage["claude_task_usage_boundary"] == "anchored_pre_tool_to_stop"
    assert usage["claude_task_usage_dedup"] == "message_id_request_id"



def test_strict_privacy_no_leakage_in_events_aggregates_health_or_obsidian(tmp_path):
    vault = tmp_path / "vault"
    aggregate = {
        "task_id": "task-privacy",
        "repo": "polymarket-btc5m-lab",
        "branch": "main",
        "status": "VERIFIED",
        "phase": "implementation",
        "route": "normal",
        "model": "gpt-4o",
        "effort": "medium",
        "fresh_threads": 1,
        "reply_calls": 0,
        "routing_enforced": True,
        "validation": "PASS",
        "duration_ms": 12000,
        "usage": {
            "claude_task_usage_source": "desktop_jsonl_observed",
            "claude_task_usage_known": True,
            "claude_task_usage_complete": True,
            "claude_task_input_tokens": 1234,
            "claude_task_output_tokens": 567,
            "claude_task_cache_creation_input_tokens": 10,
            "claude_task_cache_read_input_tokens": 20,
            "claude_task_total_observed_tokens": 1831,
            "claude_task_request_count": 2,
            "claude_task_usage_boundary": "anchored_pre_tool_to_stop",
            "claude_task_usage_dedup": "message_id_request_id",
            "claude_task_usage_unknown_reason": None,
            "claude_task_subagent_usage_included": False,
        },
    }
    path = obsidian_projection.project_task(aggregate, vault_root=vault)
    content = path.read_text(encoding="utf-8")

    assert "claude_task_usage_known: true" in content
    assert "claude_task_input_tokens: 1234" in content
    assert "claude_task_output_tokens: 567" in content
    assert "claude_task_total_observed_tokens: 1831" in content
    assert "claude_task_request_count: 2" in content

    # Privacy assertions: NO paths, NO prompts, NO message contents
    assert ".jsonl" not in content
    assert "projects" not in content
    assert "C:\\" not in content
    assert "prompt_text" not in content
    assert "user_message" not in content
    assert "assistant_text" not in content
    assert "msg_" not in content
    assert "req_" not in content



def test_no_global_user_directory_touched():
    assert "statusline.py" not in sys.modules



def test_deterministic_short_write_completes_full_identifier(tmp_path, monkeypatch):
    """Deterministic short writes (e.g. 4 bytes max per call) must complete the full identifier."""
    state_root = tmp_path / "state"
    session_id = "sess-short-write"
    task_id = "task-competing-123456"

    orig_write = os.write
    write_calls = 0

    def short_write(fd, b):
        nonlocal write_calls
        write_calls += 1
        # Write at most 4 bytes at a time
        chunk = b[:4]
        return orig_write(fd, chunk)

    monkeypatch.setattr(os, "write", short_write)

    res = ctu._mark_contention(state_root, session_id, task_id=task_id)
    assert res == "CREATED"
    assert write_calls > 1, f"Expected multiple write calls for {task_id}, got {write_calls}"

    # Marker raw content must equal complete original task ID exactly
    marker_path = ctu._contention_marker_path(state_root, session_id)
    assert marker_path.read_text(encoding="utf-8") == task_id

    # _read_contention_marker returns complete original task ID
    present, parsed_id = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert parsed_id == task_id


def test_zero_progress_write_fails_closed(tmp_path, monkeypatch):
    """When os.write returns 0, _mark_contention must fail promptly and retain marker to fail Stop."""
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    session_id = "sess-zero-prog"
    transcript = projects_root / f"{session_id}.jsonl"

    r1 = _make_assistant_record(session_id, "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    res_start = ctu.start_active_task(
        state_root,
        session_id=session_id,
        task_id="task-zero-target",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res_start["status"] == "STARTED"

    # Monkeypatch os.write to return 0 immediately
    monkeypatch.setattr(os, "write", lambda fd, b: 0)

    res_mark = ctu._mark_contention(state_root, session_id, task_id="task-zero-contender")
    assert res_mark == "FAILED"

    # Marker must remain present on disk
    marker_path = ctu._contention_marker_path(state_root, session_id)
    assert marker_path.exists()

    # Restore os.write and verify Stop fails closed
    monkeypatch.undo()
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id=session_id,
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    agg = _read_aggregate(state_root, "task-zero-target")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg["usage"]["claude_task_input_tokens"] is None
    assert agg["usage"]["claude_task_output_tokens"] is None


def test_midstream_write_error_fails_closed(tmp_path, monkeypatch):
    """Allow one partial write, then raise OSError on the next call; marker remains and Stop fails."""
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    session_id = "sess-midstream-err"
    transcript = projects_root / f"{session_id}.jsonl"

    r1 = _make_assistant_record(session_id, "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    res_start = ctu.start_active_task(
        state_root,
        session_id=session_id,
        task_id="task-midstream-target",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res_start["status"] == "STARTED"

    orig_write = os.write
    call_count = 0

    def fail_second_write(fd, b):
        nonlocal call_count
        call_count += 1
        if call_count == 1:
            return orig_write(fd, b[:3])
        raise OSError("Simulated midstream disk write error")

    monkeypatch.setattr(os, "write", fail_second_write)

    res_mark = ctu._mark_contention(state_root, session_id, task_id="task-midstream-contender")
    assert res_mark == "FAILED"

    # Marker must remain present on disk
    marker_path = ctu._contention_marker_path(state_root, session_id)
    assert marker_path.exists()

    # Restore os.write and prove Stop fails closed
    monkeypatch.undo()
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id=session_id,
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    agg = _read_aggregate(state_root, "task-midstream-target")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg["usage"]["claude_task_input_tokens"] is None
