"""Compare Purser-dispatched Codex threads across a pilot start time."""

from __future__ import annotations

import argparse
from collections import defaultdict
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import subprocess


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


def worktree_repositories(jobs_dir):
    mappings = []
    for path in jobs_dir.glob("*.json"):
        try:
            with path.open(encoding="utf-8") as stream:
                job = json.load(stream)
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(job, dict):
            continue
        worktree = job.get("worktreePath")
        repo = job.get("repoRoot")
        if not (isinstance(worktree, str) and worktree and isinstance(repo, str) and repo):
            continue
        mappings.append((Path(worktree), Path(repo)))
    return sorted(mappings, key=lambda item: len(item[0].parts), reverse=True)


def project_for_cwd(cwd, worktrees=()):
    if not isinstance(cwd, str) or not cwd.strip():
        return "unknown"

    cwd_path = Path(cwd)
    repo_path = None
    for worktree, repo in worktrees:
        try:
            cwd_path.relative_to(worktree)
        except ValueError:
            continue
        repo_path = repo
        break
    target = repo_path or cwd_path
    if target.exists():
        try:
            result = subprocess.run(
                ["git", "-C", str(target), "rev-parse", "--show-toplevel"],
                capture_output=True, text=True, check=False)
        except OSError:
            result = None
        if result and result.returncode == 0 and result.stdout.strip():
            return Path(result.stdout.strip()).name
    if repo_path is not None and repo_path.exists():
        return repo_path.name or "unknown"
    return cwd_path.name or "unknown"


def rollout(path, worktrees=()):
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
            "project": project_for_cwd(cwd, worktrees),
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
    worktrees = worktree_repositories(Path.home() / ".claude/purser/codex_exec_mcp/jobs")
    jobs = []
    for path in sessions.glob("**/rollout-*.jsonl"):
        job = rollout(path, worktrees)
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
    project_route_summary = {}
    for period in ("before", "after"):
        subset = [job for job in jobs if job["period"] == period]
        summary[period] = {"jobs": len(subset), **{field: sum(job[field] or 0 for job in subset)
                                                  for field in fields}}
        for name in sorted({job["project"] for job in subset}, key=str.casefold):
            project_jobs = [job for job in subset if job["project"] == name]
            project_summary.setdefault(name, {})[period] = {
                "jobs": len(project_jobs),
                **{field: sum(job[field] or 0 for job in project_jobs) for field in fields}}
            for route in sorted({job.get("route") or "unknown" for job in project_jobs}, key=str.casefold):
                route_jobs = [job for job in project_jobs if (job.get("route") or "unknown") == route]
                project_route_summary.setdefault(name, {}).setdefault(route, {})[period] = {
                    "jobs": len(route_jobs),
                    **{field: sum(job[field] or 0 for job in route_jobs) for field in fields}}
    return {"since": since.isoformat(), "jobs": jobs, "summary": summary,
            "project_summary": project_summary, "project_route_summary": project_route_summary}


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
    for name, routes in result["project_route_summary"].items():
        for route, periods in routes.items():
            for period, values in periods.items():
                print(f"project={name} route={route} {period}: " +
                      " ".join(f"{key}={value}" for key, value in values.items()))


if __name__ == "__main__":
    main()
