"""Focused synthetic coverage for the compaction transcript evaluator."""

from __future__ import annotations

import json
import tempfile
from pathlib import Path

import pytest

from compaction_eval import analyze_session


ROOT = Path(__file__).resolve().parents[1]


def _record(kind: str, *, timestamp: str, request: str | None = None,
            usage: dict | None = None, content: list | None = None,
            subtype: str | None = None, compact: dict | None = None,
            attachment: dict | None = None) -> dict:
    record = {"type": kind, "timestamp": timestamp}
    if subtype:
        record["subtype"] = subtype
    if compact is not None:
        record["compactMetadata"] = compact
    if attachment is not None:
        record["attachment"] = attachment
    if request:
        record["requestId"] = request
        record["message"] = {
            "id": request, "role": "assistant", "usage": usage or {},
            "content": content or [],
        }
    elif content is not None:
        record["message"] = {"role": "user", "content": content}
    return record


def _write(rows: list[dict]) -> Path:
    path = ROOT / ".tmp" / "compaction_eval_pytest" / f"{id(rows)}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")
    return path


def _usage(input_tokens: int = 0, cache_read: int = 0,
           cache_creation: int = 0, output: int = 0) -> dict:
    return {"input_tokens": input_tokens, "cache_read_input_tokens": cache_read,
            "cache_creation_input_tokens": cache_creation, "output_tokens": output}


def test_deduplicates_usage_by_request_id() -> None:
    path = _write([
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="r1",
                usage=_usage(10, 20, 3, 4)),
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="r1",
                usage=_usage(10, 20, 3, 4), content=[{"type": "text", "text": "block"}]),
    ])
    result = analyze_session(path, window=20)
    assert len(result["requests"]) == 1
    assert result["requests"][0]["context"] == 33


def test_detects_manual_compact_boundary_and_neighbor_contexts() -> None:
    path = _write([
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="before",
                usage=_usage(100, 900)),
        _record("system", timestamp="2026-09-01T00:01:00Z", subtype="compact_boundary",
                compact={"trigger": "manual", "preTokens": 950}),
        _record("assistant", timestamp="2026-09-01T00:02:00Z", request="after",
                usage=_usage(20, 30, 10)),
    ])
    boundary = analyze_session(path, window=20)["boundaries"][0]
    assert (boundary["trigger"], boundary["c_pre"], boundary["c_post"]) == ("manual", 1000, 60)


def test_counts_only_rereads_of_files_seen_before_boundary() -> None:
    path = _write([
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="r0",
                usage=_usage(0, 1000), content=[{"type": "tool_use", "id": "a", "name": "Read",
                                                 "input": {"file_path": "known.py"}}]),
        _record("user", timestamp="2026-09-01T00:00:01Z",
                content=[{"type": "tool_result", "tool_use_id": "a", "content": "old"}]),
        _record("system", timestamp="2026-09-01T00:01:00Z", subtype="compact_boundary",
                compact={"trigger": "auto", "preTokens": 1000}),
        _record("assistant", timestamp="2026-09-01T00:02:00Z", request="r1",
                usage=_usage(0, 900), content=[
                    {"type": "tool_use", "id": "b", "name": "Read", "input": {"file_path": "known.py"}},
                    {"type": "tool_use", "id": "c", "name": "Read", "input": {"file_path": "new.py"}},
                ]),
        _record("user", timestamp="2026-09-01T00:02:01Z", content=[
            {"type": "tool_result", "tool_use_id": "b", "content": "x" * 400},
            {"type": "tool_result", "tool_use_id": "c", "content": "y" * 400},
        ]),
    ])
    boundary = analyze_session(path, window=20)["boundaries"][0]
    assert boundary["reread_files"] == ["known.py"]
    assert boundary["reread_count"] == 1


def test_request_after_gap_over_60_minutes_gets_no_saving_credit() -> None:
    path = _write([
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="r0",
                usage=_usage(0, 1000)),
        _record("system", timestamp="2026-09-01T00:01:00Z", subtype="compact_boundary",
                compact={"trigger": "manual", "preTokens": 1000}),
        _record("assistant", timestamp="2026-09-01T02:00:00Z", request="r1",
                usage=_usage(0, 100, 0)),
    ])
    boundary = analyze_session(path, window=20)["boundaries"][0]
    assert boundary["saving"] == pytest.approx(0.0)


def test_first_post_cache_creation_can_make_boundary_net_negative() -> None:
    path = _write([
        _record("assistant", timestamp="2026-09-01T00:00:00Z", request="r0",
                usage=_usage(0, 1000)),
        _record("system", timestamp="2026-09-01T00:01:00Z", subtype="compact_boundary",
                compact={"trigger": "manual", "preTokens": 1000}),
        _record("assistant", timestamp="2026-09-01T00:02:00Z", request="r1",
                usage=_usage(0, 100, 1000)),
    ])
    boundary = analyze_session(path, window=20)["boundaries"][0]
    assert boundary["saving"] < 0
