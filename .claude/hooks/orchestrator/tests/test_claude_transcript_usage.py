"""Focused tests for Claude Desktop per-task JSONL token telemetry."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from typing import Any
from unittest import mock

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import claude_transcript_usage as ctu
import codex
import hook
import obsidian_projection
import telemetry
import telemetry_schema as schema


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


# --- 1. Single Desktop-style assistant usage record ---
def test_desktop_assistant_usage_record(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    rec = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 500, 200, 50, 100)
    _write_jsonl(transcript, [rec])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    assert len(records) == 1

    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert agg_err is None
    assert usage is not None
    assert usage["claude_task_usage_known"] is True
    assert usage["claude_task_usage_complete"] is True
    assert usage["claude_task_input_tokens"] == 500
    assert usage["claude_task_output_tokens"] == 200
    assert usage["claude_task_cache_creation_input_tokens"] == 50
    assert usage["claude_task_cache_read_input_tokens"] == 100
    assert usage["claude_task_total_observed_tokens"] == 850
    assert usage["claude_task_request_count"] == 1


# --- 2. Token category arithmetic ---
def test_token_category_arithmetic(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    r1 = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 100, 20, 10, 5)
    r2 = _make_assistant_record("test-session", "msg_2", "req_2", None, 200, 40, 20, 15)
    _write_jsonl(transcript, [r1, r2])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert agg_err is None
    assert usage["claude_task_input_tokens"] == 300
    assert usage["claude_task_output_tokens"] == 60
    assert usage["claude_task_cache_creation_input_tokens"] == 30
    assert usage["claude_task_cache_read_input_tokens"] == 20
    assert usage["claude_task_total_observed_tokens"] == 410
    assert usage["claude_task_request_count"] == 2


# --- 3. Repeated identical message.id + requestId counts once ---
def test_repeated_identical_message_and_request_id_counts_once(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    r1 = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 100, 50)
    r1_dup = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 100, 50)
    _write_jsonl(transcript, [r1, r1_dup])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert agg_err is None
    assert usage["claude_task_input_tokens"] == 100
    assert usage["claude_task_output_tokens"] == 50
    assert usage["claude_task_request_count"] == 1


# --- 4. Contradictory duplicate fails closed ---
def test_contradictory_duplicate_fails_closed(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    r1 = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 100, 50)
    r1_conflict = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 150, 50)
    _write_jsonl(transcript, [r1, r1_conflict])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert usage is None
    assert agg_err == "CONTRADICTORY_USAGE_RECORD"


# --- 5. Contradictory duplicate is terminal across later Stop and rebuild ---
def test_contradictory_duplicate_is_terminal_across_later_stop_and_rebuild(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-conflict.jsonl"
    r1 = _make_assistant_record("sess-conflict", "msg_1", "req_1", "call_c", 100, 50)
    r1_conflict = _make_assistant_record("sess-conflict", "msg_1", "req_1", "call_c", 150, 50)
    _write_jsonl(transcript, [r1, r1_conflict])

    ctu.start_active_task(
        state_root,
        session_id="sess-conflict",
        task_id="task-conflict",
        tool_use_id="call_c",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-conflict",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "CONTRADICTORY_USAGE_RECORD"

    agg = _read_aggregate(state_root, "task-conflict")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "CONTRADICTORY_USAGE_RECORD"
    assert agg["usage"]["claude_task_input_tokens"] is None
    # Verify rebuild preserves terminal failure
    _task_path(state_root, "task-conflict").unlink()
    telemetry.rebuild(state_root=state_root)
    agg_rebuilt = _read_aggregate(state_root, "task-conflict")
    assert agg_rebuilt["usage"]["claude_task_usage_known"] is False
    assert agg_rebuilt["usage"]["claude_task_usage_complete"] is False
    assert agg_rebuilt["usage"]["claude_task_usage_unknown_reason"] == "CONTRADICTORY_USAGE_RECORD"
    assert agg_rebuilt["usage"]["claude_task_input_tokens"] is None


# --- 6. Missing requestId follows documented fallback ---
def test_missing_request_id_uses_fallback_key(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    # Both records have requestId=None
    r1 = _make_assistant_record("test-session", "msg_noreq_1", None, "call_1", 200, 50)
    r1_dup = _make_assistant_record("test-session", "msg_noreq_1", None, "call_1", 200, 50)
    r2 = _make_assistant_record("test-session", "msg_noreq_2", None, None, 100, 25)
    _write_jsonl(transcript, [r1, r1_dup, r2])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert agg_err is None
    assert usage["claude_task_input_tokens"] == 300  # 200 + 100 (r1_dup deduplicated)
    assert usage["claude_task_output_tokens"] == 75
    assert usage["claude_task_request_count"] == 2

    # Different tokens for same msg_id with missing requestId fails with CONTRADICTORY_USAGE_RECORD
    r1_conflict = _make_assistant_record("test-session", "msg_noreq_1", None, "call_1", 250, 50)
    _write_jsonl(transcript, [r1, r1_conflict])
    records2, _ = ctu.parse_transcript_assistant_records(transcript)
    usage2, agg_err2 = ctu.aggregate_usage_from_records(records2, "call_1")
    assert usage2 is None
    assert agg_err2 == "CONTRADICTORY_USAGE_RECORD"


# --- 7. Multiple content-block lines for one API response do not double-count ---
def test_multiple_content_blocks_same_api_response_dedup(tmp_path):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    r1 = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 1000, 100)
    r1_block2 = _make_assistant_record("test-session", "msg_1", "req_1", None, 1000, 100)
    _write_jsonl(transcript, [r1, r1_block2])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert agg_err is None
    assert usage["claude_task_input_tokens"] == 1000
    assert usage["claude_task_output_tokens"] == 100
    assert usage["claude_task_request_count"] == 1


# --- 8. Incomplete trailing line deferred at PreToolUse ---
def test_incomplete_trailing_line_deferred_at_pre_tool_use(tmp_path):
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-tail.jsonl"
    r1 = _make_assistant_record("sess-tail", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1], trailing_incomplete='{"type": "assistant", "message": {"id": "msg_partial"')

    # PreToolUse scan defers incomplete tail safely without failure
    anchor_offset, cursor_offset, deferred, err = ctu.scan_for_anchor_at_pre_tool_use(transcript, "call_1")
    assert err is None
    assert deferred is False
    assert anchor_offset == 0
    assert cursor_offset > 0


# --- 9. Partial trailing record at Stop fails closed ---
def test_partial_trailing_record_at_stop_fails_closed(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-tail-stop.jsonl"
    r1 = _make_assistant_record("sess-tail-stop", "msg_1", "req_1", "call_ts", 100, 20)
    _write_jsonl(transcript, [r1])

    ctu.start_active_task(
        state_root,
        session_id="sess-tail-stop",
        task_id="task-tail-stop",
        tool_use_id="call_ts",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # Append incomplete trailing segment before Stop
    with transcript.open("ab") as h:
        h.write(b'{"type": "assistant", "message": {"id": "msg_trailing"')

    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-tail-stop",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "TRANSCRIPT_UNRESOLVED_TAIL"

    agg = _read_aggregate(state_root, "task-tail-stop")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "TRANSCRIPT_UNRESOLVED_TAIL"
    assert agg["usage"]["claude_task_input_tokens"] is None


# --- 10. Clean complete final line yields known and complete totals ---
def test_clean_complete_final_line_yields_known_complete(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-clean.jsonl"
    r1 = _make_assistant_record("sess-clean", "msg_1", "req_1", "call_clean", 250, 75, 10, 5)
    r2 = _make_assistant_record("sess-clean", "msg_2", "req_2", None, 350, 85, 0, 10)
    _write_jsonl(transcript, [r1, r2])

    ctu.start_active_task(
        state_root,
        session_id="sess-clean",
        task_id="task-clean",
        tool_use_id="call_clean",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-clean",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "COMPLETED"
    usage = stop_res["usage"]
    assert usage["claude_task_usage_known"] is True
    assert usage["claude_task_usage_complete"] is True
    assert usage["claude_task_input_tokens"] == 600
    assert usage["claude_task_output_tokens"] == 160
    assert usage["claude_task_cache_creation_input_tokens"] == 10
    assert usage["claude_task_cache_read_input_tokens"] == 15
    assert usage["claude_task_total_observed_tokens"] == 785
    assert usage["claude_task_request_count"] == 2


# --- 11. Invalid token values rejected ---
@pytest.mark.parametrize(
    "invalid_val",
    [-1, True, False, 1.5, "100", float("inf"), float("nan"), 100_000_001],
)
def test_invalid_token_values_rejected(tmp_path, invalid_val):
    transcript = tmp_path / "projects" / "test-session.jsonl"
    rec = _make_assistant_record("test-session", "msg_1", "req_1", "call_1", 100, 50)
    rec["message"]["usage"]["input_tokens"] = invalid_val
    _write_jsonl(transcript, [rec])

    records, err = ctu.parse_transcript_assistant_records(transcript)
    assert err is None
    usage, agg_err = ctu.aggregate_usage_from_records(records, "call_1")
    assert usage is None
    assert agg_err == "INVALID_USAGE_RECORD"


# --- 12. Path validation: escape, wrong extension, missing file, session mismatch ---
def test_path_validation_and_rejections(tmp_path):
    projects_root = tmp_path / "projects"
    projects_root.mkdir()
    valid_file = projects_root / "sess-1.jsonl"
    valid_file.write_text("{}\n", encoding="utf-8")

    # Valid
    p, err = ctu.validate_transcript_path(valid_file, "sess-1", allowed_projects_root=projects_root)
    assert err is None
    assert p == valid_file.resolve()

    # Wrong extension
    wrong_ext = projects_root / "sess-1.txt"
    p, err = ctu.validate_transcript_path(wrong_ext, "sess-1", allowed_projects_root=projects_root)
    assert err == "WRONG_EXTENSION"

    # Session mismatch (stem != session_id)
    p, err = ctu.validate_transcript_path(valid_file, "different-session", allowed_projects_root=projects_root)
    assert err == "SESSION_MISMATCH"

    # Missing file
    missing = projects_root / "sess-2.jsonl"
    p, err = ctu.validate_transcript_path(missing, "sess-2", allowed_projects_root=projects_root)
    assert err == "TRANSCRIPT_UNAVAILABLE"

    # Path escape outside allowed root
    outside = tmp_path / "outside" / "sess-escape.jsonl"
    outside.parent.mkdir()
    outside.write_text("{}\n", encoding="utf-8")
    p, err = ctu.validate_transcript_path(outside, "sess-escape", allowed_projects_root=projects_root)
    assert err == "TRANSCRIPT_PATH_ESCAPE"

    # Null byte in path
    p, err = ctu.validate_transcript_path(str(valid_file) + "\x00", "sess-1", allowed_projects_root=projects_root)
    assert err == "TRANSCRIPT_PATH_ESCAPE"


# --- 13. Independent Counterexample Fixture 1: Overlap Sequence & Terminal Invalidation ---
def test_counterexample_overlap_sequence_stop_and_rebuild_cannot_overwrite(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-overlap.jsonl"

    r1 = _make_assistant_record("sess-overlap", "msg_1", "req_1", "call_1", 200, 50)
    _write_jsonl(transcript, [r1])

    # 1. PreToolUse task-1 / call-1
    res1 = ctu.start_active_task(
        state_root,
        session_id="sess-overlap",
        task_id="task-1",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res1["status"] == "STARTED"

    # 2. PreToolUse task-2 / call-2 before Stop
    res2 = ctu.start_active_task(
        state_root,
        session_id="sess-overlap",
        task_id="task-2",
        tool_use_id="call_2",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res2["status"] == "DENIED"
    assert res2["reason"] == "OVERLAPPING_TASK_BOUNDARY"

    # 3. Append assistant records for both calls
    r2 = _make_assistant_record("sess-overlap", "msg_2", "req_2", "call_2", 300, 60)
    r3 = _make_assistant_record("sess-overlap", "msg_3", "req_3", None, 100, 20)
    _write_jsonl(transcript, [r1, r2, r3])

    # 4. Stop session
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-overlap",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    # Stop is denied/failed due to prior overlap terminal invalidation
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "OVERLAPPING_TASK_BOUNDARY"

    # 5. Verify both task aggregates remain unknown/incomplete with OVERLAPPING_TASK_BOUNDARY
    agg1 = _read_aggregate(state_root, "task-1")
    agg2 = _read_aggregate(state_root, "task-2")

    assert agg1["usage"]["claude_task_usage_known"] is False
    assert agg1["usage"]["claude_task_usage_complete"] is False
    assert agg1["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg1["usage"]["claude_task_input_tokens"] is None
    assert agg1["usage"]["claude_task_total_observed_tokens"] is None

    assert agg2["usage"]["claude_task_usage_known"] is False
    assert agg2["usage"]["claude_task_usage_complete"] is False
    assert agg2["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg2["usage"]["claude_task_input_tokens"] is None
    assert agg2["usage"]["claude_task_total_observed_tokens"] is None

    # 6. Verify rebuild preserves terminal invalidation across both tasks
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)

    agg1_reb = _read_aggregate(state_root, "task-1")
    agg2_reb = _read_aggregate(state_root, "task-2")
    assert agg1_reb["usage"]["claude_task_usage_known"] is False
    assert agg1_reb["usage"]["claude_task_usage_complete"] is False
    assert agg1_reb["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg1_reb["usage"]["claude_task_input_tokens"] is None

    assert agg2_reb["usage"]["claude_task_usage_known"] is False
    assert agg2_reb["usage"]["claude_task_usage_complete"] is False
    assert agg2_reb["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg2_reb["usage"]["claude_task_input_tokens"] is None


# --- 14. Independent Counterexample Fixture 2: Transcript Substitution Sequence ---
def test_counterexample_transcript_substitution_sequence(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"

    dir_a = projects_root / "a"
    dir_b = projects_root / "b"
    transcript_a = dir_a / "sess-sub.jsonl"
    transcript_b = dir_b / "sess-sub.jsonl"

    r_a = _make_assistant_record("sess-sub", "msg_a", "req_a", "call_sub", 100, 20)
    r_b = _make_assistant_record("sess-sub", "msg_b", "req_b", "call_sub", 500, 100)
    _write_jsonl(transcript_a, [r_a])
    _write_jsonl(transcript_b, [r_b])

    # PreToolUse uses projects/a/sess-sub.jsonl
    res = ctu.start_active_task(
        state_root,
        session_id="sess-sub",
        task_id="task-sub",
        tool_use_id="call_sub",
        transcript_path=transcript_a,
        allowed_projects_root=projects_root,
    )
    assert res["status"] == "STARTED"

    # Stop supplies projects/b/sess-sub.jsonl (same stem, different path)
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-sub",
        transcript_path=transcript_b,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "TRANSCRIPT_IDENTITY_MISMATCH"

    agg = _read_aggregate(state_root, "task-sub")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "TRANSCRIPT_IDENTITY_MISMATCH"
    assert agg["usage"]["claude_task_input_tokens"] is None


# --- 15. Same-path replacement / rotation detected ---
def test_same_path_replacement_or_rotation_detected(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-rot.jsonl"
    r1 = _make_assistant_record("sess-rot", "msg_1", "req_1", "call_rot", 100, 20)
    _write_jsonl(transcript, [r1])

    ctu.start_active_task(
        state_root,
        session_id="sess-rot",
        task_id="task-rot",
        tool_use_id="call_rot",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # Simulate rotation / replacement by unlinking and writing a new file with different ctime/ino
    transcript.unlink()
    r2 = _make_assistant_record("sess-rot", "msg_2", "req_2", "call_rot", 200, 30)
    _write_jsonl(transcript, [r2])

    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-rot",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] in ("TRANSCRIPT_IDENTITY_MISMATCH", "TRANSCRIPT_ROTATED_OR_TRUNCATED")


# --- 16. Transcript truncation or cursor beyond EOF rejected ---
def test_transcript_truncation_or_cursor_beyond_eof(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-trunc.jsonl"
    r1 = _make_assistant_record("sess-trunc", "msg_1", "req_1", "call_tr", 100, 20)
    r2 = _make_assistant_record("sess-trunc", "msg_2", "req_2", None, 100, 20)
    _write_jsonl(transcript, [r1, r2])

    ctu.start_active_task(
        state_root,
        session_id="sess-trunc",
        task_id="task-trunc",
        tool_use_id="call_tr",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # Truncate file to smaller than initial size
    with transcript.open("wb") as h:
        h.write(b"{}")

    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-trunc",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "TRANSCRIPT_ROTATED_OR_TRUNCATED"


# --- 17. Transcript larger than MAX_INCREMENTAL_BYTES succeeds when task segment is bounded ---
def test_transcript_larger_than_50mb_succeeds_with_small_segment(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    projects_root.mkdir(parents=True, exist_ok=True)
    transcript = projects_root / "sess-big.jsonl"

    # Create file with valid padded lines exceeding 50MB (60 lines * 900KB)
    padding_chunk = b'{"type":"padding","d":"' + b"x" * 900_000 + b'"}\n'
    r1 = _make_assistant_record("sess-big", "msg_1", "req_1", "call_big", 400, 100)
    line1 = (json.dumps(r1, ensure_ascii=False) + "\n").encode("utf-8")
    with transcript.open("wb") as h:
        for _ in range(60):
            h.write(padding_chunk)
        anchor_offset = h.tell()
        h.write(line1)

    assert transcript.stat().st_size > ctu.MAX_INCREMENTAL_BYTES

    res = ctu.start_active_task(
        state_root,
        session_id="sess-big",
        task_id="task-big",
        tool_use_id="call_big",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res["status"] == "STARTED"
    assert res["active_task"]["anchor_deferred"] is False
    assert res["active_task"]["anchor_byte_offset"] == anchor_offset

    # Append second record
    r2 = _make_assistant_record("sess-big", "msg_2", "req_2", None, 100, 25)
    line2 = (json.dumps(r2, ensure_ascii=False) + "\n").encode("utf-8")
    with transcript.open("ab") as h:
        h.write(line2)

    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-big",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "COMPLETED"
    assert stop_res["usage"]["claude_task_input_tokens"] == 500
    assert stop_res["usage"]["claude_task_output_tokens"] == 125


# --- 18. Bytes consumed from cursor enforces limit ---
def test_bytes_consumed_from_cursor_enforces_limit(tmp_path):
    transcript = tmp_path / "projects" / "sess-limit.jsonl"
    transcript.parent.mkdir(parents=True, exist_ok=True)
    r1 = _make_assistant_record("sess-limit", "msg_1", "req_1", "call_lim", 100, 20)
    line1 = (json.dumps(r1, ensure_ascii=False) + "\n").encode("utf-8")
    with transcript.open("wb") as h:
        h.write(line1)

    # Read records with simulated seek offset causing bytes_to_read > MAX_INCREMENTAL_BYTES
    with mock.patch.object(Path, "stat") as mock_st:
        original_stat = transcript.stat()
        fake_stat = os.stat_result((
            original_stat.st_mode,
            original_stat.st_ino,
            original_stat.st_dev,
            original_stat.st_nlink,
            original_stat.st_uid,
            original_stat.st_gid,
            ctu.MAX_INCREMENTAL_BYTES + 1000,
            original_stat.st_atime,
            original_stat.st_mtime,
            original_stat.st_ctime,
        ))
        mock_st.return_value = fake_stat
        records, err = ctu.read_incremental_records_from_offset(transcript, 0)
        assert err == "BOUNDS_EXCEEDED"


# --- 19. Line length and record count bounds enforced ---
def test_line_length_and_record_count_bounds_enforced(tmp_path):
    projects_root = tmp_path / "projects"
    projects_root.mkdir()

    # Line length > 1,000,000
    oversized_line_file = projects_root / "sess-line.jsonl"
    with oversized_line_file.open("wb") as h:
        h.write(b"{" + b" " * (ctu.MAX_LINE_LENGTH + 10) + b"}\n")
    records, err = ctu.read_incremental_records_from_offset(oversized_line_file, 0)
    assert err == "BOUNDS_EXCEEDED"


# --- 20. Duplicate Stop is idempotent ---
def test_duplicate_stop_is_idempotent(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-idem.jsonl"
    r1 = _make_assistant_record("sess-idem", "msg_1", "req_1", "call_i", 100, 20)
    _write_jsonl(transcript, [r1])

    ctu.start_active_task(
        state_root,
        session_id="sess-idem",
        task_id="task-idem",
        tool_use_id="call_i",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    first = ctu.finalize_stop_task(state_root, session_id="sess-idem", transcript_path=transcript, allowed_projects_root=projects_root)
    assert first["status"] == "COMPLETED"

    # Second Stop call
    second = ctu.finalize_stop_task(state_root, session_id="sess-idem", transcript_path=transcript, allowed_projects_root=projects_root)
    assert second is None

    index_path = state_root / "telemetry" / "events" / "by_task" / "task-idem.jsonl"
    events = [json.loads(line) for line in index_path.read_text(encoding="utf-8").splitlines()]
    task_usage_events = [e for e in events if e["event_type"] == "CLAUDE_TASK_USAGE_RECORDED"]
    assert len(task_usage_events) == 1


# --- 21. Repeated same-task Codex reply does not reset or double-count ---
def test_repeated_same_task_codex_reply_retains_baseline(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-reply.jsonl"
    r1 = _make_assistant_record("sess-reply", "msg_1", "req_1", "call_reply_1", 200, 50)
    _write_jsonl(transcript, [r1])

    res1 = ctu.start_active_task(
        state_root,
        session_id="sess-reply",
        task_id="task-reply",
        tool_use_id="call_reply_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res1["status"] == "STARTED"

    res2 = ctu.start_active_task(
        state_root,
        session_id="sess-reply",
        task_id="task-reply",
        tool_use_id="call_reply_2",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res2["status"] == "RETAINED"
    assert res2["active_task"]["tool_use_id"] == "call_reply_1"

    r2 = _make_assistant_record("sess-reply", "msg_2", "req_2", None, 300, 70)
    _write_jsonl(transcript, [r1, r2])

    stop_res = ctu.finalize_stop_task(state_root, session_id="sess-reply", transcript_path=transcript, allowed_projects_root=projects_root)
    assert stop_res["status"] == "COMPLETED"
    assert stop_res["usage"]["claude_task_input_tokens"] == 500
    assert stop_res["usage"]["claude_task_output_tokens"] == 120
    assert stop_res["usage"]["claude_task_request_count"] == 2


# --- 22. Stale active task expires explicitly ---
def test_stale_active_task_expires_explicitly(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    active_path = state_root / "active_tasks" / "sess-stale.json"
    active_path.parent.mkdir(parents=True, exist_ok=True)

    stale_time = (datetime.now(timezone.utc) - timedelta(seconds=ctu.MAX_ACTIVE_TASK_AGE_SECONDS + 100)).isoformat().replace("+00:00", "Z")
    stale_record = {
        "session_id": "sess-stale",
        "task_id": "task-stale",
        "tool_use_id": "call-stale",
        "canonical_transcript_path": str(projects_root / "sess-stale.jsonl"),
        "created_at": stale_time,
        "completed": False,
        "invalidated": False,
    }
    active_path.write_text(json.dumps(stale_record), encoding="utf-8")

    res = ctu.start_active_task(
        state_root,
        session_id="sess-stale",
        task_id="task-fresh",
        tool_use_id="call-fresh",
        transcript_path=projects_root / "sess-stale.jsonl",
        allowed_projects_root=projects_root,
    )
    assert res["status"] == "STARTED"
    assert res["active_task"]["task_id"] == "task-fresh"

    stale_agg = _read_aggregate(state_root, "task-stale")
    assert stale_agg["usage"]["claude_task_usage_known"] is False
    assert stale_agg["usage"]["claude_task_usage_unknown_reason"] == "STALE_ACTIVE_TASK"


# --- 23. Unexpected parser or import failure records safe health ---
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


# --- 24. Canonical event rebuild reproduces aggregate and projection ---
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


# --- 25. Full hook lifecycle synthetic e2e ---
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


# --- 26. Strict privacy: no leakage in events, aggregates, health, or Obsidian ---
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


# --- 27. Isolation: no test touches global statusline, .ctx, or LOCALAPPDATA ---
def test_no_global_user_directory_touched():
    assert "statusline.py" not in sys.modules


# --- 28. Session recovery after overlap (Finding 1) ---
def test_session_recovery_after_overlap(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-recov.jsonl"

    r1 = _make_assistant_record("sess-recov", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    # 1. task-1 starts
    res1 = ctu.start_active_task(
        state_root,
        session_id="sess-recov",
        task_id="task-1",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res1["status"] == "STARTED"

    # 2. task-2 overlaps before Stop
    res2 = ctu.start_active_task(
        state_root,
        session_id="sess-recov",
        task_id="task-2",
        tool_use_id="call_2",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res2["status"] == "DENIED"
    assert res2["reason"] == "OVERLAPPING_TASK_BOUNDARY"

    # 3. Stop remains failed for contaminated boundary
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-recov",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "OVERLAPPING_TASK_BOUNDARY"

    # 4. New independent task-3 in the same session starts successfully!
    r3 = _make_assistant_record("sess-recov", "msg_3", "req_3", "call_3", 300, 75)
    _write_jsonl(transcript, [r1, r3])

    res3 = ctu.start_active_task(
        state_root,
        session_id="sess-recov",
        task_id="task-3",
        tool_use_id="call_3",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res3["status"] == "STARTED"

    # 5. task-3 finishes with another assistant response and Stop succeeds
    r4 = _make_assistant_record("sess-recov", "msg_4", "req_4", None, 150, 25)
    _write_jsonl(transcript, [r1, r3, r4])

    stop_res3 = ctu.finalize_stop_task(
        state_root,
        session_id="sess-recov",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res3["status"] == "COMPLETED"
    assert stop_res3["usage"]["claude_task_input_tokens"] == 450
    assert stop_res3["usage"]["claude_task_output_tokens"] == 100

    # 6. Verify task-1 and task-2 remain terminal unknown, while task-3 alone is known/complete
    agg1 = _read_aggregate(state_root, "task-1")
    agg2 = _read_aggregate(state_root, "task-2")
    agg3 = _read_aggregate(state_root, "task-3")

    assert agg1["usage"]["claude_task_usage_known"] is False
    assert agg1["usage"]["claude_task_usage_complete"] is False
    assert agg1["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg1["usage"]["claude_task_input_tokens"] is None

    assert agg2["usage"]["claude_task_usage_known"] is False
    assert agg2["usage"]["claude_task_usage_complete"] is False
    assert agg2["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg2["usage"]["claude_task_input_tokens"] is None

    assert agg3["usage"]["claude_task_usage_known"] is True
    assert agg3["usage"]["claude_task_usage_complete"] is True
    assert agg3["usage"]["claude_task_input_tokens"] == 450
    assert agg3["usage"]["claude_task_output_tokens"] == 100

    # 7. Full rebuild preserves all three outcomes
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)

    agg1_reb = _read_aggregate(state_root, "task-1")
    agg2_reb = _read_aggregate(state_root, "task-2")
    agg3_reb = _read_aggregate(state_root, "task-3")

    assert agg1_reb["usage"]["claude_task_usage_known"] is False
    assert agg1_reb["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg2_reb["usage"]["claude_task_usage_known"] is False
    assert agg2_reb["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"
    assert agg3_reb["usage"]["claude_task_usage_known"] is True
    assert agg3_reb["usage"]["claude_task_usage_complete"] is True
    assert agg3_reb["usage"]["claude_task_input_tokens"] == 450


# --- 29. Same terminal task cannot recover accidentally ---
def test_same_terminal_task_cannot_recover_accidentally(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-retry.jsonl"
    r1 = _make_assistant_record("sess-retry", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    # task-1 starts and overlaps with task-2
    ctu.start_active_task(state_root, session_id="sess-retry", task_id="task-1", tool_use_id="call_1", transcript_path=transcript, allowed_projects_root=projects_root)
    ctu.start_active_task(state_root, session_id="sess-retry", task_id="task-2", tool_use_id="call_2", transcript_path=transcript, allowed_projects_root=projects_root)
    ctu.finalize_stop_task(state_root, session_id="sess-retry", transcript_path=transcript, allowed_projects_root=projects_root)

    # Retrying task-1 in same session is rejected
    res_retry = ctu.start_active_task(state_root, session_id="sess-retry", task_id="task-1", tool_use_id="call_1_retry", transcript_path=transcript, allowed_projects_root=projects_root)
    assert res_retry["status"] == "DENIED"

    agg1 = _read_aggregate(state_root, "task-1")
    assert agg1["usage"]["claude_task_usage_known"] is False
    assert agg1["usage"]["claude_task_usage_unknown_reason"] == "OVERLAPPING_TASK_BOUNDARY"


# --- 30. Stale invalid active record is recoverable ---
def test_stale_invalid_active_record_is_recoverable(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    active_path = state_root / "active_tasks" / "sess-stale-inv.json"
    active_path.parent.mkdir(parents=True, exist_ok=True)

    stale_time = (datetime.now(timezone.utc) - timedelta(seconds=ctu.MAX_ACTIVE_TASK_AGE_SECONDS + 100)).isoformat().replace("+00:00", "Z")
    stale_record = {
        "session_id": "sess-stale-inv",
        "task_id": "task-old",
        "tool_use_id": "call-old",
        "canonical_transcript_path": str(projects_root / "sess-stale-inv.jsonl"),
        "created_at": stale_time,
        "completed": True,
        "invalidated": True,
        "invalidated_reason": "OVERLAPPING_TASK_BOUNDARY",
        "affected_task_ids": ["task-old"],
    }
    active_path.write_text(json.dumps(stale_record), encoding="utf-8")

    # New task claims session after stale expiration
    res = ctu.start_active_task(
        state_root,
        session_id="sess-stale-inv",
        task_id="task-new",
        tool_use_id="call-new",
        transcript_path=projects_root / "sess-stale-inv.jsonl",
        allowed_projects_root=projects_root,
    )
    assert res["status"] == "STARTED"
    assert res["active_task"]["task_id"] == "task-new"


# --- 31. Deterministic different-task cross-process race ---
def test_deterministic_different_task_race(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-race-diff.jsonl"
    r1 = _make_assistant_record("sess-race-diff", "msg_1", "req_1", "call_race", 100, 20)
    _write_jsonl(transcript, [r1])

    sync_file = tmp_path / "sync.trigger"
    out1 = tmp_path / "out1.json"
    out2 = tmp_path / "out2.json"
    orch_dir = str(Path(__file__).parents[1])

    worker_code = """
import sys, json, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = sys.argv[2]
session_id = sys.argv[3]
task_id = sys.argv[4]
tool_use_id = sys.argv[5]
transcript = sys.argv[6]
sync_file = Path(sys.argv[7])
out_file = Path(sys.argv[8])
projects_root = sys.argv[9]

while not sync_file.exists():
    time.sleep(0.002)

res = ctu.start_active_task(
    state_root,
    session_id=session_id,
    task_id=task_id,
    tool_use_id=tool_use_id,
    transcript_path=transcript,
    allowed_projects_root=projects_root,
)
out_file.write_text(json.dumps(res), encoding="utf-8")
"""

    import subprocess
    p1 = subprocess.Popen([sys.executable, "-c", worker_code, orch_dir, str(state_root), "sess-race-diff", "task-r1", "call-r1", str(transcript), str(sync_file), str(out1), str(projects_root)])
    p2 = subprocess.Popen([sys.executable, "-c", worker_code, orch_dir, str(state_root), "sess-race-diff", "task-r2", "call-r2", str(transcript), str(sync_file), str(out2), str(projects_root)])

    try:
        time.sleep(0.05)
        sync_file.touch()

        p1.wait(timeout=10)
        p2.wait(timeout=10)
    finally:
        try:
            p1.kill()
        except OSError:
            pass
        try:
            p2.kill()
        except OSError:
            pass

    res1 = json.loads(out1.read_text(encoding="utf-8"))
    res2 = json.loads(out2.read_text(encoding="utf-8"))

    statuses = {res1["status"], res2["status"]}
    # Critical invariant: they CANNOT both return STARTED
    assert not (res1["status"] == "STARTED" and res2["status"] == "STARTED")
    assert "DENIED" in statuses

    # Require the losing path's exact safe failure reason
    losing = res1 if res1["status"] == "DENIED" else res2
    assert losing["reason"] in {"OVERLAPPING_TASK_BOUNDARY", "ACTIVE_TASK_STATE_CONTENTION"}

    # Execute Stop after both processes exit
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-race-diff",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] in {"OVERLAPPING_TASK_BOUNDARY", "ACTIVE_TASK_STATE_CONTENTION"}

    # Both task aggregates must be terminal unknown with null numeric totals
    agg1 = _read_aggregate(state_root, "task-r1")
    agg2 = _read_aggregate(state_root, "task-r2")
    assert agg1.get("usage", {}).get("claude_task_usage_known") is False
    assert agg1.get("usage", {}).get("claude_task_usage_complete") is False
    assert agg1.get("usage", {}).get("claude_task_input_tokens") is None
    assert agg1.get("usage", {}).get("claude_task_output_tokens") is None
    assert agg2.get("usage", {}).get("claude_task_usage_known") is False
    assert agg2.get("usage", {}).get("claude_task_usage_complete") is False
    assert agg2.get("usage", {}).get("claude_task_input_tokens") is None
    assert agg2.get("usage", {}).get("claude_task_output_tokens") is None

    # Stop cannot create a successful event
    events_files = list((state_root / "telemetry" / "events").glob("????-??/session_*.jsonl"))
    if events_files:
        events = [json.loads(line) for line in events_files[0].read_text(encoding="utf-8").splitlines() if line.strip()]
        for ev in events:
            if ev.get("event_type") == "CLAUDE_TASK_USAGE_RECORDED":
                assert ev.get("payload", {}).get("claude_task_usage_known") is not True

    # Rebuild and confirm the result
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)
    agg1_reb = _read_aggregate(state_root, "task-r1")
    agg2_reb = _read_aggregate(state_root, "task-r2")
    assert agg1_reb.get("usage", {}).get("claude_task_usage_known") is False
    assert agg2_reb.get("usage", {}).get("claude_task_usage_known") is False


# --- 32. Deterministic same-task cross-process race ---
def test_deterministic_same_task_race(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-race-same.jsonl"
    r1 = {
        "type": "assistant",
        "sessionId": "sess-race-same",
        "timestamp": "2026-09-07T12:00:00.000Z",
        "message": {
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "content": [
                {"type": "text", "text": "parallel tools"},
                {"type": "tool_use", "id": "call_same_1", "name": "mcp__codex__codex", "input": {"prompt": "p1"}},
                {"type": "tool_use", "id": "call_same_2", "name": "mcp__codex__codex", "input": {"prompt": "p2"}},
            ],
            "usage": {
                "input_tokens": 200,
                "output_tokens": 50,
                "cache_creation_input_tokens": 0,
                "cache_read_input_tokens": 0,
            },
        },
        "requestId": "req_1",
    }
    _write_jsonl(transcript, [r1])

    sync_file = tmp_path / "sync_same.trigger"
    out1 = tmp_path / "out_same1.json"
    out2 = tmp_path / "out_same2.json"
    orch_dir = str(Path(__file__).parents[1])

    worker_code = """
import sys, json, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = sys.argv[2]
session_id = sys.argv[3]
task_id = sys.argv[4]
tool_use_id = sys.argv[5]
transcript = sys.argv[6]
sync_file = Path(sys.argv[7])
out_file = Path(sys.argv[8])
projects_root = sys.argv[9]

while not sync_file.exists():
    time.sleep(0.002)

res = ctu.start_active_task(
    state_root,
    session_id=session_id,
    task_id=task_id,
    tool_use_id=tool_use_id,
    transcript_path=transcript,
    allowed_projects_root=projects_root,
)
out_file.write_text(json.dumps(res), encoding="utf-8")
"""

    import subprocess
    p1 = subprocess.Popen([sys.executable, "-c", worker_code, orch_dir, str(state_root), "sess-race-same", "task-same", "call_same_1", str(transcript), str(sync_file), str(out1), str(projects_root)])
    p2 = subprocess.Popen([sys.executable, "-c", worker_code, orch_dir, str(state_root), "sess-race-same", "task-same", "call_same_2", str(transcript), str(sync_file), str(out2), str(projects_root)])

    try:
        time.sleep(0.05)
        sync_file.touch()

        p1.wait(timeout=10)
        p2.wait(timeout=10)
    finally:
        try:
            p1.kill()
        except OSError:
            pass
        try:
            p2.kill()
        except OSError:
            pass

    res1 = json.loads(out1.read_text(encoding="utf-8"))
    res2 = json.loads(out2.read_text(encoding="utf-8"))

    statuses = {res1["status"], res2["status"]}
    # Require one STARTED and one RETAINED result
    assert statuses == {"STARTED", "RETAINED"}

    started_res = res1 if res1["status"] == "STARTED" else res2
    retained_res = res2 if res1["status"] == "STARTED" else res1

    # Verify both refer to the same authoritative original anchor
    assert started_res["active_task"]["tool_use_id"] == retained_res["active_task"]["tool_use_id"]
    assert started_res["active_task"]["anchor_byte_offset"] == retained_res["active_task"]["anchor_byte_offset"]

    # Append the final assistant record
    r2 = _make_assistant_record("sess-race-same", "msg_2", "req_2", None, 300, 100)
    _write_jsonl(transcript, [r1, r2])

    # Execute Stop
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-race-same",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "COMPLETED"

    # Assert exact category totals and deduplicated request count
    agg = _read_aggregate(state_root, "task-same")
    assert agg["usage"]["claude_task_usage_known"] is True
    assert agg["usage"]["claude_task_usage_complete"] is True
    assert agg["usage"]["claude_task_input_tokens"] == 500
    assert agg["usage"]["claude_task_output_tokens"] == 150
    assert agg["usage"]["claude_task_request_count"] == 2

    # Assert no double count and exactly one successful task-usage event
    events_files = list((state_root / "telemetry" / "events").glob("????-??/session_*.jsonl"))
    assert len(events_files) == 1
    events = [json.loads(line) for line in events_files[0].read_text(encoding="utf-8").splitlines() if line.strip()]
    success_events = [
        ev for ev in events
        if ev.get("event_type") == "CLAUDE_TASK_USAGE_RECORDED" and ev.get("payload", {}).get("claude_task_usage_known") is True
    ]
    assert len(success_events) == 1

    # Rebuild and confirm the same totals
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)
    agg_reb = _read_aggregate(state_root, "task-same")
    assert agg_reb["usage"]["claude_task_usage_known"] is True
    assert agg_reb["usage"]["claude_task_input_tokens"] == 500
    assert agg_reb["usage"]["claude_task_output_tokens"] == 150
    assert agg_reb["usage"]["claude_task_request_count"] == 2


# --- 33. Lock contention / timeout fails closed (Finding 2) ---
def test_lock_contention_and_timeout_fail_closed(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-contention.jsonl"
    r1 = _make_assistant_record("sess-contention", "msg_1", "req_1", "call_cont", 100, 20)
    _write_jsonl(transcript, [r1])

    ready_file = tmp_path / "ready.lock"
    release_file = tmp_path / "release.lock"
    orch_dir = str(Path(__file__).parents[1])

    locker_code = """
import sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = Path(sys.argv[2])
session_id = sys.argv[3]
ready_file = Path(sys.argv[4])
release_file = Path(sys.argv[5])

with ctu.session_lock(state_root, session_id, timeout_seconds=10.0) as acquired:
    if acquired:
        ready_file.touch()
        while not release_file.exists():
            time.sleep(0.01)
"""

    import subprocess
    proc = subprocess.Popen([sys.executable, "-c", locker_code, orch_dir, str(state_root), "sess-contention", str(ready_file), str(release_file)])

    # Wait until subprocess holds the lock
    deadline = time.monotonic() + 5.0
    while not ready_file.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert ready_file.exists()

    try:
        # Call start_active_task with a short timeout
        res = ctu.start_active_task(
            state_root,
            session_id="sess-contention",
            task_id="task-blocked",
            tool_use_id="call_cont",
            transcript_path=transcript,
            allowed_projects_root=projects_root,
            lock_timeout_seconds=0.05,
        )
        assert res["status"] == "DENIED"
        assert res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

        agg = _read_aggregate(state_root, "task-blocked")
        assert agg["usage"]["claude_task_usage_known"] is False
        assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    finally:
        # Signal subprocess to release lock
        release_file.touch()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    # Releasing the lock permits later unrelated work
    res_after = ctu.start_active_task(
        state_root,
        session_id="sess-contention",
        task_id="task-after",
        tool_use_id="call_after",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res_after["status"] == "STARTED"


# --- 34. PreToolUse anchor scan enforces MAX_RECORDS bound (Finding 3) ---
def test_anchor_scan_record_bound_enforced(tmp_path, monkeypatch):
    projects_root = tmp_path / "projects"
    projects_root.mkdir(parents=True, exist_ok=True)
    transcript = projects_root / "sess-bound.jsonl"

    # Create 3 complete lines
    r1 = _make_assistant_record("sess-bound", "msg_1", "req_1", None, 100, 20)
    r2 = _make_assistant_record("sess-bound", "msg_2", "req_2", "tool_target", 200, 40)
    r3 = _make_assistant_record("sess-bound", "msg_3", "req_3", None, 300, 60)

    _write_jsonl(transcript, [r1, r2, r3])

    # 1. With MAX_RECORDS = 2: scanning 3 lines must return BOUNDS_EXCEEDED
    monkeypatch.setattr(ctu, "MAX_RECORDS", 2)
    offset, cursor, deferred, err = ctu.scan_for_anchor_at_pre_tool_use(transcript, "tool_target")
    assert err == "BOUNDS_EXCEEDED"

    # 2. When anchor appears on line 1 within MAX_RECORDS = 2
    r_first = _make_assistant_record("sess-bound2", "msg_f", "req_f", "tool_first", 100, 20)
    transcript2 = projects_root / "sess-bound2.jsonl"
    _write_jsonl(transcript2, [r_first])
    offset2, cursor2, deferred2, err2 = ctu.scan_for_anchor_at_pre_tool_use(transcript2, "tool_first")
    assert err2 is None
    assert deferred2 is False
    assert offset2 == 0


# --- 35. Active-boundary contention followed by Stop (Mandatory Counterexample) ---
def test_active_boundary_contention_followed_by_stop(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-c-stop.jsonl"
    orch_dir = str(Path(__file__).parents[1])

    # 1. Valid transcript and task-1 starts
    r1 = _make_assistant_record("sess-c-stop", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    res1 = ctu.start_active_task(
        state_root,
        session_id="sess-c-stop",
        task_id="task-1",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res1["status"] == "STARTED"

    ready_file = tmp_path / "hold.ready"
    release_file = tmp_path / "hold.release"
    locker_code = """
import sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = Path(sys.argv[2])
session_id = sys.argv[3]
ready_file = Path(sys.argv[4])
release_file = Path(sys.argv[5])

with ctu.session_lock(state_root, session_id, timeout_seconds=10.0) as acquired:
    if acquired:
        ready_file.touch()
        while not release_file.exists():
            time.sleep(0.01)
"""
    import subprocess
    proc = subprocess.Popen([sys.executable, "-c", locker_code, orch_dir, str(state_root), "sess-c-stop", str(ready_file), str(release_file)])

    deadline = time.monotonic() + 5.0
    while not ready_file.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert ready_file.exists()

    try:
        # 3. Attempt task-2 with short bounded lock timeout -> DENIED / ACTIVE_TASK_STATE_CONTENTION
        res2 = ctu.start_active_task(
            state_root,
            session_id="sess-c-stop",
            task_id="task-2",
            tool_use_id="call_2",
            transcript_path=transcript,
            allowed_projects_root=projects_root,
            lock_timeout_seconds=0.05,
        )
        assert res2["status"] == "DENIED"
        assert res2["reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    finally:
        # 4. Release and join lock-holder process
        release_file.touch()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    # 5. Append complete synthetic assistant response
    r2 = _make_assistant_record("sess-c-stop", "msg_2", "req_2", None, 150, 4)
    _write_jsonl(transcript, [r1, r2])

    # 6. Call Stop
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-c-stop",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # 7. Assert Stop returns FAILED / ACTIVE_TASK_STATE_CONTENTION
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    # 8. Assert task-1 and task-2 both have known=false, complete=false, reason=CONTENTION, numeric null
    agg1 = _read_aggregate(state_root, "task-1")
    agg2 = _read_aggregate(state_root, "task-2")

    assert agg1["usage"]["claude_task_usage_known"] is False
    assert agg1["usage"]["claude_task_usage_complete"] is False
    assert agg1["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg1["usage"]["claude_task_input_tokens"] is None
    assert agg1["usage"]["claude_task_output_tokens"] is None

    assert agg2["usage"]["claude_task_usage_known"] is False
    assert agg2["usage"]["claude_task_usage_complete"] is False
    assert agg2["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg2["usage"]["claude_task_input_tokens"] is None
    assert agg2["usage"]["claude_task_output_tokens"] is None

    # 9. Rebuild canonical telemetry and assert identical outcomes
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)

    agg1_reb = _read_aggregate(state_root, "task-1")
    agg2_reb = _read_aggregate(state_root, "task-2")
    assert agg1_reb["usage"]["claude_task_usage_known"] is False
    assert agg1_reb["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg1_reb["usage"]["claude_task_input_tokens"] is None
    assert agg2_reb["usage"]["claude_task_usage_known"] is False
    assert agg2_reb["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg2_reb["usage"]["claude_task_input_tokens"] is None

    # 10. Non-vacuous terminal event and rebuild assertion (Finding B)
    events_files = list((state_root / "telemetry" / "events").glob("????-??/session_*.jsonl"))
    assert len(events_files) == 1
    events = [json.loads(line) for line in events_files[0].read_text(encoding="utf-8").splitlines() if line.strip()]

    # 10a. Demonstrate old payload-level lookup fails to find any affected tasks
    old_payload_matches = [
        ev for ev in events
        if ev.get("event_type") == "CLAUDE_TASK_USAGE_RECORDED"
        and ev.get("payload", {}).get("task_id") in ("task-1", "task-2")
    ]
    # Structure assertion so relying on old payload lookup fails because zero matches is unacceptable
    assert len(old_payload_matches) == 0, "task_id must live on event envelope, not in payload"

    # 10b. Filter using top-level event["task_id"] and require terminal usage events for both tasks
    numeric_token_fields = [
        "claude_task_input_tokens",
        "claude_task_output_tokens",
        "claude_task_cache_creation_input_tokens",
        "claude_task_cache_read_input_tokens",
        "claude_task_total_observed_tokens",
        "claude_task_request_count",
    ]
    affected_tasks = {"task-1", "task-2"}
    matched_by_task = {tid: [] for tid in affected_tasks}
    for ev in events:
        if ev.get("event_type") == "CLAUDE_TASK_USAGE_RECORDED":
            tid = ev.get("task_id")
            if tid in affected_tasks:
                matched_by_task[tid].append(ev)

    for tid in affected_tasks:
        assert len(matched_by_task[tid]) >= 1, f"Expected terminal usage events for {tid}, found none"
        for ev in matched_by_task[tid]:
            payload = ev.get("payload", {})
            assert payload.get("claude_task_usage_known") is False
            assert payload.get("claude_task_usage_complete") is False
            assert payload.get("claude_task_usage_unknown_reason") == "ACTIVE_TASK_STATE_CONTENTION"
            for f in numeric_token_fields:
                assert payload.get(f) is None, f"Expected {f} to be None for {tid}, got {payload.get(f)}"

    # 10c. Explicitly assert no later affected-task usage event exists with known=True or complete=True
    for ev in events:
        if ev.get("task_id") in affected_tasks and ev.get("event_type") == "CLAUDE_TASK_USAGE_RECORDED":
            payload = ev.get("payload", {})
            assert payload.get("claude_task_usage_known") is not True
            assert payload.get("claude_task_usage_complete") is not True

    # 10d. Rebuild telemetry and verify both task aggregates remain terminal unknown
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)
    for tid in ("task-1", "task-2"):
        agg = _read_aggregate(state_root, tid)
        assert agg["usage"]["claude_task_usage_known"] is False
        assert agg["usage"]["claude_task_usage_complete"] is False
        assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
        for f in numeric_token_fields:
            assert agg["usage"].get(f) is None, f"Expected {f} to be None after rebuild for {tid}"


# --- 36. Session recovery after contention ---
def test_session_recovery_after_contention(tmp_path):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-c-recov.jsonl"
    orch_dir = str(Path(__file__).parents[1])

    # 1. task-1 starts
    r1 = _make_assistant_record("sess-c-recov", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    ctu.start_active_task(
        state_root,
        session_id="sess-c-recov",
        task_id="task-1",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # 2. task-2 experiences lock contention
    ready_file = tmp_path / "recov.ready"
    release_file = tmp_path / "recov.release"
    locker_code = """
import sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = Path(sys.argv[2])
session_id = sys.argv[3]
ready_file = Path(sys.argv[4])
release_file = Path(sys.argv[5])

with ctu.session_lock(state_root, session_id, timeout_seconds=10.0) as acquired:
    if acquired:
        ready_file.touch()
        while not release_file.exists():
            time.sleep(0.01)
"""
    import subprocess
    proc = subprocess.Popen([sys.executable, "-c", locker_code, orch_dir, str(state_root), "sess-c-recov", str(ready_file), str(release_file)])

    deadline = time.monotonic() + 5.0
    while not ready_file.exists() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert ready_file.exists()

    try:
        res2 = ctu.start_active_task(
            state_root,
            session_id="sess-c-recov",
            task_id="task-2",
            tool_use_id="call_2",
            transcript_path=transcript,
            allowed_projects_root=projects_root,
            lock_timeout_seconds=0.05,
        )
        assert res2["status"] == "DENIED"
    finally:
        release_file.touch()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    # Finalize Stop for the contended boundary
    ctu.finalize_stop_task(
        state_root,
        session_id="sess-c-recov",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )

    # 3. Genuinely new task-3 in the same session starts successfully
    r3 = _make_assistant_record("sess-c-recov", "msg_3", "req_3", "call_3", 250, 50)
    _write_jsonl(transcript, [r1, r3])

    res3 = ctu.start_active_task(
        state_root,
        session_id="sess-c-recov",
        task_id="task-3",
        tool_use_id="call_3",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    # Require exactly STARTED
    assert res3["status"] == "STARTED"

    # Complete task-3 through Stop
    r4 = _make_assistant_record("sess-c-recov", "msg_4", "req_4", None, 100, 30)
    _write_jsonl(transcript, [r1, r3, r4])

    stop_res3 = ctu.finalize_stop_task(
        state_root,
        session_id="sess-c-recov",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res3["status"] == "COMPLETED"

    # Verify task-3 alone is known/complete with exact synthetic totals
    agg3 = _read_aggregate(state_root, "task-3")
    assert agg3["usage"]["claude_task_usage_known"] is True
    assert agg3["usage"]["claude_task_usage_complete"] is True
    assert agg3["usage"]["claude_task_input_tokens"] == 350
    assert agg3["usage"]["claude_task_output_tokens"] == 80

    # Verify task-1 and task-2 remain terminal unknown
    agg1 = _read_aggregate(state_root, "task-1")
    agg2 = _read_aggregate(state_root, "task-2")
    assert agg1["usage"]["claude_task_usage_known"] is False
    assert agg1["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg2["usage"]["claude_task_usage_known"] is False
    assert agg2["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    # Rebuild and recheck all three
    for p in _task_files(state_root):
        p.unlink()
    telemetry.rebuild(state_root=state_root)

    assert _read_aggregate(state_root, "task-1")["usage"]["claude_task_usage_known"] is False
    assert _read_aggregate(state_root, "task-2")["usage"]["claude_task_usage_known"] is False
    assert _read_aggregate(state_root, "task-3")["usage"]["claude_task_usage_known"] is True

    # Retrying either affected terminal task ID cannot recover
    retry1 = ctu.start_active_task(
        state_root,
        session_id="sess-c-recov",
        task_id="task-1",
        tool_use_id="call_retry1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert retry1["status"] == "DENIED"

    retry2 = ctu.start_active_task(
        state_root,
        session_id="sess-c-recov",
        task_id="task-2",
        tool_use_id="call_retry2",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert retry2["status"] == "DENIED"


# --- 37. Marker edge cases ---
def test_marker_edge_cases(tmp_path, monkeypatch):
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    transcript = projects_root / "sess-marker.jsonl"
    r1 = _make_assistant_record("sess-marker", "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    # 1. Marker with active boundary invalidates active boundary before marker removal
    ctu.start_active_task(
        state_root,
        session_id="sess-marker",
        task_id="task-m1",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    # Simulate contention marker created by competing task
    ctu._mark_contention(state_root, "sess-marker", task_id="task-competing")
    marker_file = ctu._contention_marker_path(state_root, "sess-marker")
    assert marker_file.exists()

    # Capture active state at moment of marker removal to verify invalidation happened before removal
    active_path = ctu._active_task_path(state_root, "sess-marker")
    state_before_unlink = {}
    orig_unlink = Path.unlink

    def checking_unlink(self, *args, **kwargs):
        if self == marker_file and active_path.exists():
            state_before_unlink.update(json.loads(active_path.read_text(encoding="utf-8")))
        return orig_unlink(self, *args, **kwargs)

    monkeypatch.setattr(Path, "unlink", checking_unlink)
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id="sess-marker",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    # Proves invalidation was written to disk before marker unlink was called!
    assert state_before_unlink.get("invalidated") is True
    assert state_before_unlink.get("invalidated_reason") == "ACTIVE_TASK_STATE_CONTENTION"

    # 2. Marker removal failure cannot lead to COMPLETED
    transcript_rem = projects_root / "sess-rem-fail.jsonl"
    _write_jsonl(transcript_rem, [r1])
    ctu.start_active_task(
        state_root,
        session_id="sess-rem-fail",
        task_id="task-rem1",
        tool_use_id="call_1",
        transcript_path=transcript_rem,
        allowed_projects_root=projects_root,
    )
    ctu._mark_contention(state_root, "sess-rem-fail", task_id="task-c2")

    # Monkeypatch _remove_contention_marker to simulate OS error deleting marker
    monkeypatch.setattr(ctu, "_remove_contention_marker", lambda root, sid: False)
    stop_fail_rem = ctu.finalize_stop_task(
        state_root,
        session_id="sess-rem-fail",
        transcript_path=transcript_rem,
        allowed_projects_root=projects_root,
    )
    assert stop_fail_rem["status"] == "FAILED"
    assert stop_fail_rem["reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    # Never COMPLETED!
    agg_rem = _read_aggregate(state_root, "task-rem1")
    assert agg_rem["usage"]["claude_task_usage_known"] is False

    # 3. Marker without active record does not permanently poison a later unrelated task
    monkeypatch.undo()
    ctu._mark_contention(state_root, "sess-orphan-marker", task_id="task-old")
    assert ctu._contention_marker_path(state_root, "sess-orphan-marker").exists()
    assert not ctu._active_task_path(state_root, "sess-orphan-marker").exists()

    transcript_orphan = projects_root / "sess-orphan.jsonl"
    r_orph = _make_assistant_record("sess-orphan-marker", "msg_o", "req_o", "call_o", 100, 20)
    _write_jsonl(transcript_orphan, [r_orph])

    res_unrelated = ctu.start_active_task(
        state_root,
        session_id="sess-orphan-marker",
        task_id="task-unrelated",
        tool_use_id="call_o",
        transcript_path=transcript_orphan,
        allowed_projects_root=projects_root,
    )
    assert res_unrelated["status"] == "STARTED"

    # 4. Duplicate marker / duplicate Stop behavior is idempotent
    ctu._mark_contention(state_root, "sess-marker", task_id="task-dup")
    dup_stop = ctu.finalize_stop_task(
        state_root,
        session_id="sess-marker",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert dup_stop["status"] == "FAILED"
    assert dup_stop["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    # 5. No locks or markers remain active
    assert not ctu._contention_marker_path(state_root, "sess-marker").exists()


# --- 38. Atomic contention marker regression tests (P2) ---
def test_atomic_contention_marker_multiprocess_race(tmp_path):
    """Multiple synchronized processes race to claim the contention marker.

    Exactly one process must create the marker ('CREATED'), all other racing
    processes must receive 'EXISTS', and the marker on disk must contain exactly
    one intact submitted task ID without corruption, truncation, or interleaving.
    """
    state_root = tmp_path / "state"
    session_id = "sess-atomic-race"
    orch_dir = str(Path(__file__).parents[1])

    num_workers = 8
    sync_dir = tmp_path / "sync"
    sync_dir.mkdir(parents=True, exist_ok=True)
    start_file = sync_dir / "race.start"

    worker_code = """
import sys, time
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import claude_transcript_usage as ctu

state_root = Path(sys.argv[2])
session_id = sys.argv[3]
task_id = sys.argv[4]
ready_file = Path(sys.argv[5])
start_file = Path(sys.argv[6])
result_file = Path(sys.argv[7])

ready_file.touch()
deadline = time.monotonic() + 10.0
while not start_file.exists() and time.monotonic() < deadline:
    time.sleep(0.001)

outcome = ctu._mark_contention(state_root, session_id, task_id=task_id)
result_file.write_text(outcome, encoding="utf-8")
"""

    processes = []
    ready_files = []
    result_files = []
    task_ids = [f"task-race-{i}" for i in range(num_workers)]

    try:
        for i, tid in enumerate(task_ids):
            rf = sync_dir / f"ready_{i}.flag"
            res_f = sync_dir / f"result_{i}.txt"
            ready_files.append(rf)
            result_files.append(res_f)
            p = subprocess.Popen(
                [sys.executable, "-c", worker_code, orch_dir, str(state_root), session_id, tid, str(rf), str(start_file), str(res_f)]
            )
            processes.append(p)

        # Wait for all workers to signal readiness
        ready_deadline = time.monotonic() + 10.0
        while not all(rf.exists() for rf in ready_files) and time.monotonic() < ready_deadline:
            time.sleep(0.01)
        assert all(rf.exists() for rf in ready_files), "Not all workers signaled readiness"

        # Unleash all workers simultaneously
        start_file.touch()

        # Wait for all processes to complete
        for p in processes:
            try:
                p.wait(timeout=10.0)
            except subprocess.TimeoutExpired:
                p.kill()
    finally:
        for p in processes:
            if p.poll() is None:
                p.kill()
                p.wait()

    # Verify return codes of all workers
    for p in processes:
        assert p.returncode == 0, f"Worker exited with non-zero code {p.returncode}"

    # Collect outcomes
    outcomes = [res_f.read_text(encoding="utf-8").strip() for res_f in result_files]
    assert outcomes.count("CREATED") == 1, f"Expected exactly one CREATED, got {outcomes}"
    assert outcomes.count("EXISTS") == num_workers - 1, f"Expected {num_workers - 1} EXISTS, got {outcomes}"

    # Check marker on disk
    winner_index = outcomes.index("CREATED")
    winner_task_id = task_ids[winner_index]

    present, competing_tid = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert competing_tid == winner_task_id

    # Verify raw file content is strictly identical to winner_task_id
    raw_content = ctu._contention_marker_path(state_root, session_id).read_text(encoding="utf-8")
    assert raw_content == winner_task_id


def test_later_contender_cannot_overwrite_marker(tmp_path):
    """Subsequent calls to _mark_contention cannot overwrite or truncate an existing marker."""
    state_root = tmp_path / "state"
    session_id = "sess-no-overwrite"

    # First caller creates marker
    res1 = ctu._mark_contention(state_root, session_id, task_id="task-first-claimant")
    assert res1 == "CREATED"
    present, tid = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert tid == "task-first-claimant"

    # Second contender attempts claim with different task ID
    res2 = ctu._mark_contention(state_root, session_id, task_id="task-second-contender")
    assert res2 == "EXISTS"
    present, tid = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert tid == "task-first-claimant"

    # Third contender attempts claim with None task ID
    res3 = ctu._mark_contention(state_root, session_id, task_id=None)
    assert res3 == "EXISTS"
    present, tid = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert tid == "task-first-claimant"

    # Marker content on disk remains completely intact
    raw_content = ctu._contention_marker_path(state_root, session_id).read_text(encoding="utf-8")
    assert raw_content == "task-first-claimant"


def test_empty_marker_invalidates_active_boundary_at_stop(tmp_path):
    """An empty contention marker (e.g. created with task_id=None or 0 bytes) forces fail-closed Stop."""
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    session_id = "sess-empty-marker"
    transcript = projects_root / f"{session_id}.jsonl"

    r1 = _make_assistant_record(session_id, "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    res_start = ctu.start_active_task(
        state_root,
        session_id=session_id,
        task_id="task-empty-target",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res_start["status"] == "STARTED"

    # Explicitly create an empty contention marker (0 bytes)
    res_mark = ctu._mark_contention(state_root, session_id, task_id=None)
    assert res_mark == "CREATED"
    marker_path = ctu._contention_marker_path(state_root, session_id)
    assert marker_path.exists()
    assert marker_path.stat().st_size == 0

    # _read_contention_marker tolerates empty body, returning (True, None)
    present, comp_tid = ctu._read_contention_marker(state_root, session_id)
    assert present is True
    assert comp_tid is None

    # finalize_stop_task must fail closed with ACTIVE_TASK_STATE_CONTENTION
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id=session_id,
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    # Verify task aggregate is terminal unknown with null token metrics
    agg = _read_aggregate(state_root, "task-empty-target")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg["usage"]["claude_task_input_tokens"] is None
    assert agg["usage"]["claude_task_output_tokens"] is None
    assert agg["usage"]["claude_task_total_observed_tokens"] is None


def test_simulated_write_fsync_failure_leaves_marker_present_and_fails_stop(tmp_path, monkeypatch):
    """When marker creation succeeds but os.write/os.fsync raises OSError, empty marker remains and Stop fails."""
    state_root = tmp_path / "state"
    projects_root = tmp_path / "projects"
    session_id = "sess-fsync-fail"
    transcript = projects_root / f"{session_id}.jsonl"

    r1 = _make_assistant_record(session_id, "msg_1", "req_1", "call_1", 100, 20)
    _write_jsonl(transcript, [r1])

    res_start = ctu.start_active_task(
        state_root,
        session_id=session_id,
        task_id="task-fsync-target",
        tool_use_id="call_1",
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert res_start["status"] == "STARTED"

    # Simulate fsync raising OSError
    def failing_fsync(fd):
        raise OSError("Simulated disk I/O failure during fsync")

    monkeypatch.setattr(os, "fsync", failing_fsync)

    # Calling _mark_contention returns FAILED because fsync failed
    outcome = ctu._mark_contention(state_root, session_id, task_id="task-competing")
    assert outcome == "FAILED"

    # Crucial assertion: the marker file was NOT deleted; it exists on disk
    marker_path = ctu._contention_marker_path(state_root, session_id)
    assert marker_path.exists()

    # Stop finalization cannot succeed: it observes the marker and invalidates
    monkeypatch.undo()
    stop_res = ctu.finalize_stop_task(
        state_root,
        session_id=session_id,
        transcript_path=transcript,
        allowed_projects_root=projects_root,
    )
    assert stop_res["status"] == "FAILED"
    assert stop_res["reason"] == "ACTIVE_TASK_STATE_CONTENTION"

    agg = _read_aggregate(state_root, "task-fsync-target")
    assert agg["usage"]["claude_task_usage_known"] is False
    assert agg["usage"]["claude_task_usage_complete"] is False
    assert agg["usage"]["claude_task_usage_unknown_reason"] == "ACTIVE_TASK_STATE_CONTENTION"
    assert agg["usage"]["claude_task_input_tokens"] is None

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
