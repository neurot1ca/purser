"""Compare Purser-dispatched Codex threads across a pilot start time."""

from __future__ import annotations

import argparse
from collections import defaultdict
from datetime import datetime, timezone
import json
from pathlib import Path
import re


def timestamp(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamps need a timezone")
    return parsed.astimezone(timezone.utc)


def rows(path):
    with path.open(encoding="utf-8") as stream:
        for line in stream:
            try:
                yield json.loads(line)
            except json.JSONDecodeError:
                continue  # An active writer can leave an incomplete final line.


def category(command):
    if re.search(r"\b(pytest|unittest|tox|npm test|cargo test|go test)\b", command, re.I):
        return "test"
    if re.search(r"\b(rg|grep|Get-Content|Select-String|cat|head|tail|find|ls|Get-ChildItem)\b", command, re.I):
        return "read_search"
    return "other"


def output_chars(value):
    if isinstance(value, str):
        return len(value)
    if isinstance(value, list):
        return sum(output_chars(x) for x in value)
    if isinstance(value, dict):
        return sum(output_chars(value[key]) for key in ("text", "output", "content") if key in value)
    return 0


def rollout(path):
    meta = None
    cwd = None
    usage = None
    calls = {}
    sizes = defaultdict(int)
    for item in rows(path):
        payload = item.get("payload") or {}
        kind = payload.get("type")
        if item.get("type") == "session_meta":
            meta = payload
            cwd = cwd or payload.get("cwd")
        elif item.get("type") == "turn_context":
            cwd = cwd or payload.get("cwd")
        elif item.get("type") == "event_msg" and kind == "turn_context":
            cwd = cwd or payload.get("cwd")
        elif item.get("type") == "event_msg" and kind == "token_count":
            latest = (payload.get("info") or {}).get("total_token_usage")
            if latest:
                usage = latest
        elif item.get("type") == "response_item" and kind in ("function_call", "custom_tool_call"):
            calls[payload.get("call_id")] = category(str(payload.get("arguments") or payload.get("input") or ""))
        elif item.get("type") == "response_item" and kind in ("function_call_output", "custom_tool_call_output"):
            sizes[calls.get(payload.get("call_id"), "other")] += output_chars(payload.get("output"))
    if not meta or meta.get("originator") != "codex_exec" or not usage:
        return None
    return {"thread_id": meta.get("id") or meta.get("session_id"),
            "date": meta.get("timestamp"), "input": usage.get("input_tokens", 0),
            "project": Path(cwd).name if isinstance(cwd, str) and cwd.strip() else "unknown",
            "cached": usage.get("cached_input_tokens", 0),
            "output": usage.get("output_tokens", 0),
            "reasoning": usage.get("reasoning_output_tokens", 0),
            "read_search_chars": sizes["read_search"], "test_chars": sizes["test"],
            "other_chars": sizes["other"]}


def telemetry_index(root):
    routes = {}
    finished = {}
    for path in root.glob("*/session_*.jsonl"):
        for item in rows(path):
            key = (item.get("session_id"), item.get("task_id"))
            payload = item.get("payload") or {}
            if item.get("event_type") == "TASK_ROUTE_SELECTED":
                routes[key] = payload.get("route")
            elif item.get("event_type") == "IMPLEMENTER_FINISHED" and payload.get("thread_id"):
                thread = payload["thread_id"]
                previous = finished.get(thread, {})
                finished[thread] = {"task": item.get("task_id"),
                                    "route": payload.get("route") or routes.get(key),
                                    "duration_ms": (previous.get("duration_ms") or 0) + (payload.get("duration_ms") or 0),
                                    "eta_minutes": payload.get("eta_minutes")}
    return finished


def report(sessions, events, since, *, day=None, project=None):
    index = telemetry_index(events)
    jobs = []
    for path in sessions.glob("**/rollout-*.jsonl"):
        job = rollout(path)
        if not job or job["thread_id"] not in index:
            continue
        if project and job["project"].casefold() != project.casefold():
            continue
        job.update(index[job["thread_id"]])
        started = timestamp(job["date"])
        if day and started.date() != day:
            continue
        job["uncached"] = job["input"] - job["cached"]
        job["period"] = "after" if started >= since else "before"
        jobs.append(job)
    jobs.sort(key=lambda job: job["date"])
    fields = ("input", "cached", "uncached", "output", "reasoning", "duration_ms",
              "read_search_chars", "test_chars", "other_chars")
    summary = {}
    project_summary = {}
    for period in ("before", "after"):
        subset = [job for job in jobs if job["period"] == period]
        summary[period] = {"jobs": len(subset), **{field: sum(job[field] or 0 for job in subset)
                                                  for field in fields}}
        for name in sorted({job["project"] for job in subset}, key=str.casefold):
            project_jobs = [job for job in subset if job["project"] == name]
            project_summary.setdefault(name, {})[period] = {
                "jobs": len(project_jobs),
                **{field: sum(job[field] or 0 for job in project_jobs) for field in fields}}
    return {"since": since.isoformat(), "jobs": jobs, "summary": summary,
            "project_summary": project_summary}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--since", required=True, type=timestamp, help="Pilot start, ISO 8601 with timezone")
    parser.add_argument("--sessions", type=Path, default=Path.home() / ".codex/sessions")
    parser.add_argument("--events", type=Path, default=Path.home() / ".claude/purser/telemetry/events")
    parser.add_argument("--day", type=lambda value: datetime.fromisoformat(value).date())
    parser.add_argument("--project", help="Filter to a project folder name (case-insensitive)")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)
    result = report(args.sessions, args.events, args.since, day=args.day, project=args.project)
    if args.json:
        print(json.dumps(result, indent=2))
        return
    print("date UTC | project | task | route | input | cached | uncached | output | reasoning | duration ms | read/search chars | test chars | other chars")
    for job in result["jobs"]:
        print(" | ".join(str(job.get(field, "")) for field in
                         ("date", "project", "task", "route", "input", "cached", "uncached", "output",
                          "reasoning", "duration_ms", "read_search_chars", "test_chars", "other_chars")))
    for period, values in result["summary"].items():
        print(f"{period}: " + " ".join(f"{key}={value}" for key, value in values.items()))
    for name, periods in result["project_summary"].items():
        for period, values in periods.items():
            print(f"project={name} {period}: " + " ".join(f"{key}={value}" for key, value in values.items()))


if __name__ == "__main__":
    main()
