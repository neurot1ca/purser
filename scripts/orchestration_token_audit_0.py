#!/usr/bin/env python3
"""Measure Claude session token cost and the tool-batching ratio from transcripts.

Read-only analysis script. It reads Claude Code session transcripts and prints
counters. It does not launch observers, trading code, paper/shadow/live
behavior, DB writes, deploys, or any execution path, and it writes nothing
outside an explicitly requested ``--json`` output file.

Why this exists
---------------
Per-request context re-read dominates token cost in this repository: context
saturates near ~300K tokens, so total cost is approximately linear in the
number of assistant requests. The single largest controllable term is therefore
how many tool calls each request carries. ``CLAUDE.md`` sets the target at
>= 1.5; the measured baseline over the 15 sessions preceding 2026-08-31 was
**1.08** (92% of tool-bearing requests issued exactly one tool call).

Counting contract (do not "simplify" this)
------------------------------------------
A single API response is written to the transcript as **one JSONL line per
content block**, and the same ``usage`` object is repeated on each of those
lines. Summing ``usage`` per line therefore double-counts every multi-block
response -- roughly 1.9x on this corpus. Every figure below is deduplicated by
``requestId`` (falling back to ``message.id``). A prior audit that skipped this
step reported 7,509 requests and 2.23B cache-read tokens where the true values
were 3,865 and 1.16B.

Token figures taken from ``usage`` are exact. Figures derived from tool-result
payloads are approximate (``len(text) / 4``) and are labelled as such.
"""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import datetime
import glob
import json
import os
from pathlib import Path
import re
import statistics
import sys
from typing import Any

_ORCHESTRATOR_PACKAGE = (
    Path(__file__).resolve().parent.parent / ".claude" / "hooks" / "orchestrator"
)
if str(_ORCHESTRATOR_PACKAGE) not in sys.path:
    sys.path.insert(0, str(_ORCHESTRATOR_PACKAGE))

import project_root


def _project_slug(root: str | os.PathLike[str]) -> str:
    """Return Claude Code's transcript-directory slug for an absolute path."""

    return re.sub(r"[^A-Za-z0-9-]", "-", str(root))


def _transcript_glob(root: str | os.PathLike[str]) -> str:
    return str(
        Path.home() / ".claude" / "projects" / _project_slug(root) / "*.jsonl"
    )

# Billing weights relative to an uncached input token.
WEIGHT_INPUT = 1.0
WEIGHT_CACHE_READ = 0.10
WEIGHT_CACHE_WRITE = 1.25
WEIGHT_OUTPUT = 5.0

BATCHING_BASELINE = 1.08
BATCHING_TARGET = 1.5

# Gaps longer than this are treated as the operator being away, not model time.
IDLE_GAP_SECONDS = 600.0
CHARS_PER_TOKEN = 4.0


def _timestamp(value: Any) -> float | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def _payload_text(block: dict[str, Any]) -> str:
    content = block.get("content")
    if isinstance(content, str):
        return content
    return json.dumps(content, ensure_ascii=False)


def _analyze_session(path: str) -> dict[str, Any]:
    """Return per-session counters, deduplicated by request."""

    seen_requests: set[str] = set()
    request_times: list[float] = []
    tools_per_request: Counter[int] = Counter()
    tool_calls_by_request: defaultdict[str, int] = defaultdict(int)
    tool_names: Counter[str] = Counter()
    # Sequence of ("REQ", None) and ("RES", (tool_name, approx_tokens)) in file order,
    # used to weight each payload by how many later requests re-read it.
    timeline: list[tuple[str, Any]] = []
    tool_use_names: dict[str, str] = {}
    totals = {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}
    first_context = 0

    with open(path, encoding="utf-8") as handle:
        for line in handle:
            try:
                record = json.loads(line)
            except (ValueError, TypeError):
                continue
            message = record.get("message")
            if not isinstance(message, dict):
                continue

            key = record.get("requestId") or message.get("id")
            if message.get("role") == "assistant" and key and key not in seen_requests:
                seen_requests.add(key)
                usage = message.get("usage") or {}
                totals["input"] += usage.get("input_tokens", 0)
                totals["output"] += usage.get("output_tokens", 0)
                totals["cache_read"] += usage.get("cache_read_input_tokens", 0)
                totals["cache_write"] += usage.get("cache_creation_input_tokens", 0)
                if not first_context:
                    first_context = (
                        usage.get("input_tokens", 0)
                        + usage.get("cache_read_input_tokens", 0)
                        + usage.get("cache_creation_input_tokens", 0)
                    )
                moment = _timestamp(record.get("timestamp"))
                if moment is not None:
                    request_times.append(moment)
                timeline.append(("REQ", None))

            content = message.get("content")
            if not isinstance(content, list):
                continue
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    name = block.get("name", "unknown")
                    tool_use_names[block.get("id")] = name
                    tool_names[name] += 1
                    if key:
                        tool_calls_by_request[key] += 1
                elif block.get("type") == "tool_result":
                    name = tool_use_names.get(block.get("tool_use_id"), "unknown")
                    approx = len(_payload_text(block)) / CHARS_PER_TOKEN
                    timeline.append(("RES", (name, approx)))

    for count in tool_calls_by_request.values():
        tools_per_request[count] += 1

    request_count = sum(1 for kind, _ in timeline if kind == "REQ")
    amplified: defaultdict[str, float] = defaultdict(float)
    raw: defaultdict[str, float] = defaultdict(float)
    elapsed_requests = 0
    for kind, payload in timeline:
        if kind == "REQ":
            elapsed_requests += 1
            continue
        name, approx = payload
        raw[name] += approx
        amplified[name] += approx * max(request_count - elapsed_requests, 0)

    request_times.sort()
    gaps = [
        later - earlier
        for earlier, later in zip(request_times, request_times[1:])
        if 0 < later - earlier < IDLE_GAP_SECONDS
    ]

    return {
        "session": Path(path).stem,
        "requests": len(seen_requests),
        "totals": totals,
        "first_context": first_context,
        "tools_per_request": tools_per_request,
        "tool_names": tool_names,
        "amplified": amplified,
        "raw": raw,
        "gaps": gaps,
    }


def _collect(pattern: str, limit: int) -> list[dict[str, Any]]:
    files = sorted(glob.glob(pattern), key=os.path.getmtime, reverse=True)[:limit]
    if not files:
        raise SystemExit(f"No transcripts matched: {pattern}")
    return [_analyze_session(path) for path in files]


def _aggregate(sessions: list[dict[str, Any]]) -> dict[str, Any]:
    totals = {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}
    requests = 0
    tools_per_request: Counter[int] = Counter()
    tool_names: Counter[str] = Counter()
    amplified: defaultdict[str, float] = defaultdict(float)
    raw: defaultdict[str, float] = defaultdict(float)
    gaps: list[float] = []
    first_contexts: list[int] = []

    for session in sessions:
        requests += session["requests"]
        for bucket, value in session["totals"].items():
            totals[bucket] += value
        tools_per_request.update(session["tools_per_request"])
        tool_names.update(session["tool_names"])
        for name, value in session["amplified"].items():
            amplified[name] += value
        for name, value in session["raw"].items():
            raw[name] += value
        gaps.extend(session["gaps"])
        if session["first_context"]:
            first_contexts.append(session["first_context"])

    tool_bearing = sum(tools_per_request.values())
    total_tool_calls = sum(count * n for count, n in tools_per_request.items())
    weighted = {
        "input": totals["input"] * WEIGHT_INPUT,
        "cache_read": totals["cache_read"] * WEIGHT_CACHE_READ,
        "cache_write": totals["cache_write"] * WEIGHT_CACHE_WRITE,
        "output": totals["output"] * WEIGHT_OUTPUT,
    }

    return {
        "sessions": len(sessions),
        "requests": requests,
        "totals": totals,
        "weighted": weighted,
        "weighted_total": sum(weighted.values()),
        "tools_per_request": tools_per_request,
        "tool_bearing_requests": tool_bearing,
        "total_tool_calls": total_tool_calls,
        "tools_per_request_avg": (total_tool_calls / tool_bearing) if tool_bearing else 0.0,
        "tool_names": tool_names,
        "amplified": dict(amplified),
        "raw": dict(raw),
        "gaps": gaps,
        "median_first_context": (
            statistics.median(first_contexts) if first_contexts else 0
        ),
    }


def _report(summary: dict[str, Any], sessions: list[dict[str, Any]]) -> None:
    requests = summary["requests"] or 1
    totals = summary["totals"]
    cache_read = totals["cache_read"] or 1

    print("=" * 72)
    print(f"ORCHESTRATION TOKEN AUDIT  ({summary['sessions']} sessions, deduplicated by requestId)")
    print("=" * 72)

    print("\n-- BATCHING RATIO (the headline metric) --")
    average = summary["tools_per_request_avg"]
    for count in sorted(summary["tools_per_request"]):
        n = summary["tools_per_request"][count]
        share = 100 * n / max(summary["tool_bearing_requests"], 1)
        print(f"   {count} tool call(s): {n:6d} requests  {share:5.1f}%")
    print(f"   average          : {average:.2f}   (baseline {BATCHING_BASELINE}, target {BATCHING_TARGET})")
    if average >= BATCHING_TARGET:
        verdict = "AT/ABOVE TARGET"
    elif average > BATCHING_BASELINE + 0.05:
        verdict = "improving"
    else:
        verdict = "NOT MOVING -- the CLAUDE.md batching rule is not being applied"
    print(f"   status           : {verdict}")
    if average > 0 and summary["tool_bearing_requests"]:
        at_target = summary["total_tool_calls"] / BATCHING_TARGET
        removed = summary["tool_bearing_requests"] - at_target
        if removed > 0:
            saved = removed * (cache_read / requests)
            print(
                f"   reaching {BATCHING_TARGET} would remove {removed:,.0f} requests "
                f"= {saved:,.0f} cache-read tokens ({100 * saved / cache_read:.1f}%)"
            )

    print("\n-- GROUND TRUTH --")
    print(f"   assistant requests      : {requests:,}")
    print(f"   input (uncached)        : {totals['input']:,}")
    print(f"   output                  : {totals['output']:,}")
    print(f"   cache read              : {totals['cache_read']:,}")
    print(f"   cache write             : {totals['cache_write']:,}")
    print(f"   avg context per request : {(totals['input'] + totals['cache_read'] + totals['cache_write']) // requests:,}")
    print(f"   cost of one round trip  : {cache_read // requests:,} cache-read tokens")

    print(f"\n-- BILLABLE WEIGHT (input 1x, cache_read {WEIGHT_CACHE_READ}x, cache_write {WEIGHT_CACHE_WRITE}x, output {WEIGHT_OUTPUT}x) --")
    weighted_total = summary["weighted_total"] or 1
    for bucket in ("cache_read", "cache_write", "output", "input"):
        value = summary["weighted"][bucket]
        print(f"   {bucket:12s} {value:16,.0f}  {100 * value / weighted_total:5.1f}%")

    prefix = summary["median_first_context"]
    if prefix:
        print(
            f"\n-- STATIC PREFIX --\n"
            f"   median first-request context: {prefix:,.0f} tokens\n"
            f"   re-read on every request    : {prefix * requests:,.0f} = "
            f"{100 * prefix * requests / cache_read:.1f}% of cache_read"
        )

    print("\n-- TOOL RESULT COST (approximate: chars/4, weighted by later re-reads) --")
    amplified = summary["amplified"]
    for name in sorted(amplified, key=lambda k: -amplified[k])[:10]:
        calls = summary["tool_names"].get(name, 0)
        print(
            f"   {name[:32]:32s} {calls:5d} calls  "
            f"amplified {amplified[name]:14,.0f}  {100 * amplified[name] / cache_read:5.2f}%"
        )

    gaps = summary["gaps"]
    if gaps:
        ordered = sorted(gaps)
        print("\n-- WALL CLOCK (gap between assistant requests, idle excluded) --")
        print(
            f"   median {statistics.median(gaps):.1f}s   mean {statistics.mean(gaps):.1f}s   "
            f"p90 {ordered[int(len(ordered) * 0.9)]:.1f}s"
        )
        print(f"   active time across these sessions: {sum(gaps) / 3600:.1f}h")

    print("\n-- PER SESSION --")
    for session in sessions:
        per_session_calls = sum(
            count * n for count, n in session["tools_per_request"].items()
        )
        bearing = sum(session["tools_per_request"].values())
        avg = per_session_calls / bearing if bearing else 0.0
        print(
            f"   {session['session'][:8]}  requests={session['requests']:5d}  "
            f"cache_read={session['totals']['cache_read']:14,}  tools/req={avg:.2f}"
        )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Measure session token cost and the tool-batching ratio.",
    )
    parser.add_argument(
        "--sessions",
        type=int,
        default=15,
        help="Number of most recent transcripts to read (default: 15).",
    )
    parser.add_argument(
        "--transcripts",
        default=None,
        help="Glob for session .jsonl transcripts.",
    )
    parser.add_argument(
        "--project-root",
        type=Path,
        help="Governed project root (default: git top-level of the current directory).",
    )
    parser.add_argument(
        "--json",
        dest="json_out",
        help="Also write the summary to this path as JSON.",
    )
    args = parser.parse_args(argv)

    if args.sessions < 1:
        raise SystemExit("--sessions must be >= 1")

    if args.project_root is None:
        resolved_root, reason = project_root.resolve_candidate(Path.cwd())
        if resolved_root is None:
            raise SystemExit(reason)
    else:
        resolved_root = args.project_root.resolve()
    transcripts = args.transcripts or _transcript_glob(resolved_root)

    sessions = _collect(transcripts, args.sessions)
    summary = _aggregate(sessions)
    _report(summary, sessions)

    if args.json_out:
        serializable = {
            "sessions": summary["sessions"],
            "requests": summary["requests"],
            "totals": summary["totals"],
            "weighted": summary["weighted"],
            "tools_per_request": {
                str(k): v for k, v in sorted(summary["tools_per_request"].items())
            },
            "tools_per_request_avg": summary["tools_per_request_avg"],
            "batching_baseline": BATCHING_BASELINE,
            "batching_target": BATCHING_TARGET,
            "median_first_context": summary["median_first_context"],
            "amplified_tool_tokens": summary["amplified"],
            "tool_call_counts": dict(summary["tool_names"]),
        }
        Path(args.json_out).write_text(
            json.dumps(serializable, indent=2, sort_keys=True), encoding="utf-8"
        )
        print(f"\nwrote {args.json_out}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
