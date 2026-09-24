#!/usr/bin/env python3
"""Evaluate realized costs around real Claude Code compaction boundaries.

Reads ``~/.claude/projects/<slug>/*.jsonl`` transcripts for the requested roots
and writes only to stdout, except for an explicitly requested ``--json`` path.
The transcript may omit the compaction summary API call; such a call is reported
as ``not visible`` and is never synthesized. Savings are an intentionally
simple replay estimate: each later request receives the same cache-read credit
from the observed pre/post context difference, then first-post cache creation
and approximate reread-result costs are deducted. A request after a gap longer
than 60 minutes gets no positive saving credit because the prompt cache would
have expired anyway. Tool-result text and Claude Code file-attachment content
are approximate at four characters per token.
"""

from __future__ import annotations

import argparse
from collections import defaultdict
import glob
import json
import os
from pathlib import Path
import statistics
import sys
from typing import Any

try:
    from orchestration_token_audit_0 import _project_slug, _timestamp, _transcript_glob
except ImportError:  # Support both ``python scripts/compaction_eval.py`` and package imports.
    from scripts.orchestration_token_audit_0 import _project_slug, _timestamp, _transcript_glob


CHARS_PER_TOKEN = 4.0
WEIGHT_CACHE_READ = 0.10
WEIGHT_CACHE_WRITE = 1.25
WEIGHT_INPUT = 1.0
WEIGHT_OUTPUT = 5.0
MAX_CACHE_GAP_SECONDS = 60 * 60
DEFAULT_ROOTS = (Path(r"C:\Projects\purser"), Path(r"C:\Projects\agentpool"))


def _context(usage: dict[str, Any]) -> int:
    return sum(int(usage.get(key, 0) or 0) for key in (
        "input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"
    ))


def _text_chars(value: Any) -> int:
    if isinstance(value, str):
        return len(value)
    if isinstance(value, list):
        return sum(_text_chars(item) for item in value)
    if isinstance(value, dict):
        if isinstance(value.get("text"), str):
            return len(value["text"])
        if isinstance(value.get("content"), (str, list, dict)):
            return _text_chars(value["content"])
        return 0
    return 0


def _file_path(tool_input: Any) -> str | None:
    if not isinstance(tool_input, dict):
        return None
    path = tool_input.get("file_path") or tool_input.get("path")
    return str(path) if isinstance(path, str) and path else None


def _load_records(path: str | os.PathLike[str]) -> list[dict[str, Any]]:
    records = []
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            try:
                record = json.loads(line)
            except (ValueError, TypeError):
                continue
            if isinstance(record, dict):
                records.append(record)
    return records


def _additional_context(record: dict[str, Any]) -> str:
    attachment = record.get("attachment")
    if not isinstance(attachment, dict):
        return ""
    if attachment.get("type") == "hook_additional_context":
        return str(attachment.get("content", ""))
    if attachment.get("type") != "hook_success":
        return ""
    if not str(attachment.get("hookName", "")).startswith("SessionStart"):
        return ""
    try:
        output = json.loads(attachment.get("stdout", ""))
    except (ValueError, TypeError):
        return ""
    specific = output.get("hookSpecificOutput", {}) if isinstance(output, dict) else {}
    value = specific.get("additionalContext", "") if isinstance(specific, dict) else ""
    return value if isinstance(value, str) else ""


def analyze_session(path: str | os.PathLike[str], window: int = 20) -> dict[str, Any]:
    """Analyze boundaries and requests in one transcript, in serialized order."""
    records = _load_records(path)
    requests: list[dict[str, Any]] = []
    boundaries: list[dict[str, Any]] = []
    request_keys: dict[str, int] = {}
    tool_uses: dict[str, dict[str, Any]] = {}
    seen_files: set[str] = set()
    events: list[dict[str, Any]] = []

    for line_no, record in enumerate(records):
        timestamp = _timestamp(record.get("timestamp"))
        message = record.get("message")
        content = message.get("content") if isinstance(message, dict) else None
        if record.get("type") == "system" and record.get("subtype") == "compact_boundary":
            metadata = record.get("compactMetadata") or {}
            boundaries.append({
                "record_index": line_no,
                "time": record.get("timestamp", ""),
                "timestamp": timestamp,
                "trigger": metadata.get("trigger", "unknown"),
                "pre_tokens": metadata.get("preTokens"),
            })
            events.append({"kind": "boundary", "record_index": line_no,
                           "boundary_index": len(boundaries) - 1})

        is_assistant = isinstance(message, dict) and message.get("role") == "assistant"
        key = (record.get("requestId") or message.get("id")) if is_assistant else None
        request_index = None
        if key:
            key = str(key)
            if key not in request_keys:
                usage = message.get("usage") or {}
                input_tokens = int(usage.get("input_tokens", 0) or 0)
                cache_read = int(usage.get("cache_read_input_tokens", 0) or 0)
                cache_creation = int(usage.get("cache_creation_input_tokens", 0) or 0)
                output_tokens = int(usage.get("output_tokens", 0) or 0)
                request_index = len(requests)
                request_keys[key] = request_index
                requests.append({
                    "key": key, "time": record.get("timestamp", ""),
                    "timestamp": timestamp, "context": _context(usage),
                    "billable_weight": (input_tokens * WEIGHT_INPUT
                                        + cache_read * WEIGHT_CACHE_READ
                                        + cache_creation * WEIGHT_CACHE_WRITE
                                        + output_tokens * WEIGHT_OUTPUT),
                    "cache_creation": cache_creation,
                    "record_index": line_no, "tools": [],
                })
                events.append({"kind": "request", "record_index": line_no,
                               "request_index": request_index})
            else:
                request_index = request_keys[key]

        if isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    tool_id = block.get("id")
                    tool = {"name": block.get("name", "unknown"),
                            "input": block.get("input", {}), "request_index": request_index,
                            "record_index": line_no, "result_chars": 0}
                    if tool_id:
                        tool_uses[str(tool_id)] = tool
                    if request_index is not None:
                        requests[request_index]["tools"].append(tool)
                        events.append({"kind": "tool", "record_index": line_no,
                                       "request_index": request_index, "tool": tool})
                elif block.get("type") == "tool_result":
                    tool = tool_uses.get(str(block.get("tool_use_id", "")))
                    if tool is not None:
                        tool["result_chars"] += _text_chars(block.get("content"))

        # Read/Edit/Write before a compact boundary establish the session file set.
        for tool in (requests[request_index]["tools"] if request_index is not None else []):
            if line_no == tool["record_index"]:
                name = tool["name"]
                path_value = _file_path(tool["input"])
                if name in ("Read", "Edit", "Write") and path_value:
                    tool["file_path"] = path_value
                    events.append({"kind": "file_seen", "record_index": line_no,
                                   "request_index": request_index, "path": path_value})

        attachment = record.get("attachment")
        if record.get("type") == "attachment" and isinstance(attachment, dict):
            if attachment.get("type") == "file":
                nested = attachment.get("content", {})
                file_obj = nested.get("file", {}) if isinstance(nested, dict) else {}
                filepath = (file_obj.get("filePath") if isinstance(file_obj, dict) else None) or attachment.get("filename")
                file_content = file_obj.get("content", "") if isinstance(file_obj, dict) else nested
                events.append({"kind": "attached_file", "record_index": line_no,
                               "path": str(filepath or ""), "chars": _text_chars(file_content)})
            context = _additional_context(record)
            if context:
                events.append({"kind": "session_start_context", "record_index": line_no,
                               "content": context})

    # Tool call inventory is used to identify files known before each boundary.
    for boundary_i, boundary in enumerate(boundaries):
        boundary_line = boundary["record_index"]
        previous = [r for r in requests if r["record_index"] < boundary_line]
        later_all = [r for r in requests if r["record_index"] > boundary_line]
        boundary["c_pre"] = previous[-1]["context"] if previous else None
        boundary["c_post"] = later_all[0]["context"] if later_all else None
        next_boundary_line = (boundaries[boundary_i + 1]["record_index"]
                              if boundary_i + 1 < len(boundaries) else float("inf"))
        interval = [r for r in later_all if r["record_index"] < next_boundary_line]
        measured = interval[:window]

        prior_tools = [t for r in previous for t in r["tools"]]
        known_paths = {t.get("file_path") for t in prior_tools
                       if t["name"] in ("Read", "Edit", "Write") and t.get("file_path")}
        prior_searches = {(t["input"].get("pattern"), t["input"].get("path"))
                          for t in prior_tools if t["name"] in ("Grep", "Glob")
                          and isinstance(t.get("input"), dict)}

        rereads: list[dict[str, Any]] = []
        repeated_searches: list[dict[str, Any]] = []
        questions = 0
        for request in measured:
            for tool in request["tools"]:
                if tool["name"] == "Read" and tool.get("file_path") in known_paths:
                    rereads.append(tool)
                if tool["name"] in ("Grep", "Glob"):
                    pair = (tool["input"].get("pattern"), tool["input"].get("path"))
                    if pair in prior_searches:
                        repeated_searches.append({"name": tool["name"], "pattern": pair[0], "path": pair[1]})
                if tool["name"] == "AskUserQuestion":
                    questions += 1

        # Claude Code records files re-injected by compaction as attachment/file rows
        # adjacent to the boundary, before the first post-boundary API request.
        first_post_line = later_all[0]["record_index"] if later_all else float("inf")
        attached = [e for e in events if e["kind"] == "attached_file"
                    and boundary_line < e["record_index"] < first_post_line]
        hook_contexts = [e["content"] for e in events
                         if e["kind"] == "session_start_context"
                         and boundary_line < e["record_index"] < first_post_line]
        handoff_attached = any(Path(e["path"].replace("\\", "/")).name.lower() == "purser-handoff.md"
                               for e in attached)
        handoff_in_hook = any("purser-handoff.md" in value.lower() or "# purser handoff" in value.lower()
                              for value in hook_contexts)

        saving = 0.0
        if boundary["c_pre"] is not None and boundary["c_post"] is not None:
            credit_per_request = WEIGHT_CACHE_READ * max(0, boundary["c_pre"] - boundary["c_post"])
            previous_time = previous[-1]["timestamp"]
            for request in interval:
                gap = (request["timestamp"] - previous_time
                       if request["timestamp"] is not None and previous_time is not None else None)
                if gap is None or gap <= MAX_CACHE_GAP_SECONDS:
                    saving += credit_per_request
                previous_time = request["timestamp"]
            saving -= WEIGHT_CACHE_WRITE * (interval[0]["cache_creation"] if interval else 0)
            for tool in rereads:
                tool_req_index = tool.get("request_index")
                if tool_req_index is None:
                    continue
                position = next((i for i, req in enumerate(interval)
                                 if req is requests[tool_req_index]), None)
                if position is not None:
                    later_count = 0
                    for current, following in zip(interval[position:], interval[position + 1:]):
                        if current["timestamp"] is not None and following["timestamp"] is not None:
                            if following["timestamp"] - current["timestamp"] <= MAX_CACHE_GAP_SECONDS:
                                later_count += 1
                    saving -= (tool["result_chars"] / CHARS_PER_TOKEN) * WEIGHT_CACHE_READ * later_count

        boundary.update({
            "session": Path(path).stem,
            "reread_files": sorted({t.get("file_path", "") for t in rereads}),
            "reread_count": len(rereads),
            "repeated_searches": repeated_searches,
            "reattached_files": [e["path"] for e in attached],
            "reattached_count": len(attached),
            "reattached_approx_tokens": sum(e["chars"] / CHARS_PER_TOKEN for e in attached),
            "handoff_double_load": bool(handoff_attached and handoff_in_hook),
            "ask_user_questions": questions,
            "saving": saving,
            "summary_call": "not visible",
        })

    return {"session": Path(path).stem, "requests": requests, "boundaries": boundaries}


def collect_sessions(roots: list[Path], limit: int, window: int = 20) -> list[dict[str, Any]]:
    files: set[str] = set()
    for root in roots:
        for match in glob.glob(_transcript_glob(root.resolve())):
            files.add(str(Path(match)))
    ordered = sorted(files, key=lambda p: os.path.getmtime(p), reverse=True)[:limit]
    if not ordered:
        searched = ", ".join(_transcript_glob(root.resolve()) for root in roots)
        raise SystemExit(f"No transcripts matched: {searched}")
    return [analyze_session(path, window=window) for path in ordered]


def _render(sessions: list[dict[str, Any]], window: int) -> dict[str, Any]:
    boundaries = [boundary for session in sessions for boundary in session["boundaries"]]
    savings = [b["saving"] for b in boundaries]
    negatives = sum(value < 0 for value in savings)
    print(f"COMPACTION EVALUATION ({len(sessions)} sessions, window={window} requests)")
    print("Saving is weighted units: 0.1 * avoided cache-read tokens minus first-post cache creation and reread results.")
    print("Compaction summary API calls are reported as 'not visible' when absent from transcript records.")
    print("\nTotals: boundaries={count}; median_realized_saving={median:.1f}; total_realized_saving={total:.1f}; net_negative_share={negative_share:.1%}".format(
        count=len(boundaries), median=statistics.median(savings) if savings else 0.0,
        total=sum(savings), negative_share=negatives / len(savings) if savings else 0.0))
    for trigger in ("manual", "auto"):
        group = [b["saving"] for b in boundaries if b["trigger"] == trigger]
        if group:
            print("{trigger}: boundaries={count}; median_realized_saving={median:.1f}; total_realized_saving={total:.1f}; net_negative_share={negative_share:.1%}".format(
                trigger=trigger, count=len(group), median=statistics.median(group),
                total=sum(group), negative_share=sum(value < 0 for value in group) / len(group)))
    print("\n| session | time | trigger | C_pre | C_post | saving | rereads | searches | reattached (files/tokens) | handoff double load | questions | summary call |")
    print("|---|---|---:|---:|---:|---:|---:|---:|---:|---|---:|---|")
    for boundary in boundaries[:40]:
        print("| {session} | {time} | {trigger} | {cpre} | {cpost} | {saving:.1f} | {rereads} | {searches} | {files}/{tokens:.0f} | {handoff} | {questions} | {summary} |".format(
            session=boundary["session"], time=boundary["time"], trigger=boundary["trigger"],
            cpre=boundary["c_pre"] if boundary["c_pre"] is not None else "—",
            cpost=boundary["c_post"] if boundary["c_post"] is not None else "—",
            saving=boundary["saving"], rereads=boundary["reread_count"],
            searches=len(boundary["repeated_searches"]), files=boundary["reattached_count"],
            tokens=boundary["reattached_approx_tokens"], handoff="yes" if boundary["handoff_double_load"] else "no",
            questions=boundary["ask_user_questions"], summary=boundary["summary_call"]))
    if len(boundaries) > 40:
        print(f"... {len(boundaries) - 40} more boundaries omitted")
    return {"sessions": len(sessions), "boundaries": boundaries,
            "count": len(boundaries), "median_realized_saving": statistics.median(savings) if savings else 0.0,
            "total_realized_saving": sum(savings), "net_negative_share": negatives / len(savings) if savings else 0.0}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Measure observed economics around Claude Code compaction boundaries.")
    parser.add_argument("--root", action="append", type=Path, help="Project root; repeatable.")
    parser.add_argument("--window", type=int, default=20, help="Post-boundary request window for churn metrics (default: 20).")
    parser.add_argument("--limit", type=int, default=20, help="Most recently modified transcripts across roots (default: 20).")
    parser.add_argument("--json", dest="json_path", help="Also write the result to this JSON path.")
    args = parser.parse_args(argv)
    if args.window < 1 or args.limit < 1:
        raise SystemExit("--window and --limit must be >= 1")
    roots = args.root or list(DEFAULT_ROOTS)
    sessions = collect_sessions(roots, args.limit, args.window)
    result = _render(sessions, args.window)
    if args.json_path:
        Path(args.json_path).write_text(json.dumps(result, indent=2, sort_keys=True), encoding="utf-8")
    return 0


if __name__ == "__main__":
    sys.exit(main())
