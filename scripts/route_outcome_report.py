"""Summarize recorded task outcomes by route, model, effort, and task type."""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import statistics


def timestamp(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.utcoffset() is None or parsed.utcoffset().total_seconds() != 0:
        raise argparse.ArgumentTypeError("timestamp must be UTC")
    return parsed.astimezone(timezone.utc)


def default_state_root() -> Path:
    override = os.environ.get("PURSER_STATE_DIR")
    if override:
        return Path(override).expanduser().resolve()
    config_dir = os.environ.get("CLAUDE_CONFIG_DIR")
    if config_dir:
        return (Path(config_dir).expanduser() / "purser").resolve()
    return (Path.home() / ".claude" / "purser").resolve()


def _number(value):
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) and value >= 0 else None


def _task_tokens(task):
    usage = task.get("usage")
    if not isinstance(usage, dict):
        return {"worker": None, "claude": None}
    worker_totals = []
    for provider in ("codex", "gemini", "deepseek"):
        if usage.get(f"{provider}_usage_known") is not True:
            continue
        total = _number(usage.get(f"{provider}_total_tokens"))
        if total is None:
            inputs = _number(usage.get(f"{provider}_input_tokens"))
            outputs = _number(usage.get(f"{provider}_output_tokens"))
            if inputs is None or outputs is None:
                continue
            total = inputs + outputs
        worker_totals.append(total)
    claude_total = None
    if usage.get("claude_task_usage_known") is True:
        claude_total = _number(usage.get("claude_task_total_observed_tokens"))
    return {"worker": sum(worker_totals) if worker_totals else None, "claude": claude_total}


def _percentile(values, fraction):
    ordered = sorted(values)
    if not ordered:
        return None
    return ordered[max(0, math.ceil(fraction * len(ordered)) - 1)]


def report(state_root, since, *, project_key=None, min_samples=5):
    since = timestamp(since) if isinstance(since, str) else since
    project_dirs = [state_root / "projects" / project_key] if project_key else sorted((state_root / "projects").glob("*"))
    groups = defaultdict(list)
    for project_dir in project_dirs:
        task_dir = project_dir / "telemetry" / "tasks"
        for path in sorted(task_dir.glob("*.json")):
            try:
                task = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                continue
            if not isinstance(task, dict):
                continue
            started = task.get("started_at")
            try:
                if not isinstance(started, str) or timestamp(started) < since:
                    continue
            except (ValueError, argparse.ArgumentTypeError):
                continue
            if project_key and task.get("project_key") != project_key:
                continue
            key = tuple(task.get(field) or "unknown" for field in ("route", "model", "effort", "task_type"))
            task["_source_project_key"] = task.get("project_key") or project_dir.name
            groups[key].append(task)

    results = []
    for key in sorted(groups, key=lambda row: tuple(str(value).casefold() for value in row)):
        tasks = groups[key]
        outcomes = Counter(
            task.get("claude_acceptance", {}).get("outcome", "unknown")
            if isinstance(task.get("claude_acceptance"), dict) else "unknown"
            for task in tasks
        )
        known = outcomes["accepted"] + outcomes["rejected"]
        accepted = [task for task in tasks if isinstance(task.get("claude_acceptance"), dict)
                    and task["claude_acceptance"].get("outcome") == "accepted"]
        applied = sum(task["claude_acceptance"].get("applied_as_is") is True for task in accepted)
        send_backs = sum(
            _number((task.get("claude_acceptance") or {}).get("send_backs")) or 0 for task in tasks
        )
        durations = [_number(task.get("duration_ms")) for task in accepted]
        durations = [value for value in durations if value is not None]
        tokens = [_task_tokens(task) for task in accepted]
        worker_tokens = [value["worker"] for value in tokens if value["worker"] is not None]
        claude_tokens = [value["claude"] for value in tokens if value["claude"] is not None]
        reviewers = Counter(
            (f"SKIPPED:{task['reviewer1_skipped_reason']}"
             if task.get("reviewer1_skipped_reason") else task.get("reviewer1_result") or "unknown")
            for task in tasks
        )
        adequate = len(tasks) >= min_samples
        results.append({
            "route": key[0], "model": key[1], "effort": key[2], "task_type": key[3],
            "sample_count": len(tasks), "outcome_coverage": known / len(tasks) if adequate and tasks else None,
            "accepted": outcomes["accepted"], "rejected": outcomes["rejected"], "unknown": outcomes["unknown"],
            "applied_as_is_rate": applied / len(accepted) if adequate and accepted else None,
            "send_backs": send_backs,
            "accepted_duration_ms": {"count": len(durations), "median": statistics.median(durations) if durations else None,
                                     "p90": _percentile(durations, .9)},
            "accepted_worker_tokens": {"count": len(worker_tokens), "median": statistics.median(worker_tokens) if worker_tokens else None,
                                       "p90": _percentile(worker_tokens, .9)},
            "accepted_claude_tokens": {"count": len(claude_tokens), "median": statistics.median(claude_tokens) if claude_tokens else None,
                                       "p90": _percentile(claude_tokens, .9)},
            "reviewer_outcomes": dict(sorted(reviewers.items())),
            "sources": [{"task_id": task["task_id"],
                         "project_key": task.get("_source_project_key")}
                        for task in tasks[:5] if task.get("task_id")],
            "insufficient_evidence": not adequate,
        })
    return {"since": since.isoformat().replace("+00:00", "Z"), "project_key": project_key,
            "minimum_sample_size": min_samples, "groups": results}


def markdown(result, output_path, state_root):
    lines = ["# Route outcome report", "", f"Since (UTC): {result['since']}",
             f"Minimum sample size: {result['minimum_sample_size']}", "",
             "| Route | Model | Effort | Task type | Samples | Coverage | Accepted | Rejected | Unknown | Applied as-is | Send-backs | Accepted duration ms (n / median / p90) | Accepted worker tokens (n / median / p90) | Accepted Claude tokens (n / median / p90) | Reviewer outcomes | Sources | Evidence |",
             "| --- | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- | --- | --- | --- | --- |"]
    for group in result["groups"]:
        def rate(value):
            return "—" if value is None else f"{value:.1%}"
        def dist(value):
            return f"{value['count']} / {value['median']} / {value['p90']}"
        sources = []
        for source in group["sources"]:
            task_id = source["task_id"]
            path = state_root / "projects" / source["project_key"] / "telemetry" / "tasks" / f"{task_id}.json"
            link = os.path.relpath(path, Path(output_path).parent).replace(os.sep, "/")
            label = str(task_id).replace("[", "\\[").replace("]", "\\]")
            sources.append(f"[{label}](<{link}>)")
        evidence = "insufficient evidence" if group["insufficient_evidence"] else "reported"
        values = [str(group[field]) for field in ("route", "model", "effort", "task_type")]
        values += [
            str(group["sample_count"]), rate(group["outcome_coverage"]), str(group["accepted"]),
            str(group["rejected"]), str(group["unknown"]), rate(group["applied_as_is_rate"]),
            str(group["send_backs"]), dist(group["accepted_duration_ms"]),
            dist(group["accepted_worker_tokens"]), dist(group["accepted_claude_tokens"]),
            ", ".join(f"{name}: {count}" for name, count in group["reviewer_outcomes"].items()),
            ", ".join(sources), evidence]
        lines.append("| " + " | ".join(values) + " |")
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--since", required=True, type=timestamp, help="Include tasks started at or after this UTC time")
    parser.add_argument("--project-key", help="Limit to one Purser project key")
    parser.add_argument("--min-samples", type=int, default=5, help="Minimum group size for rates (default: 5)")
    parser.add_argument("--out", type=Path, required=True, help="Markdown report path")
    parser.add_argument("--json", action="store_true", help="Also write JSON beside the Markdown report")
    args = parser.parse_args(argv)
    if args.min_samples < 1:
        parser.error("--min-samples must be at least 1")
    root = default_state_root()
    result = report(root, args.since, project_key=args.project_key, min_samples=args.min_samples)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(markdown(result, args.out, root), encoding="utf-8")
    if args.json:
        args.out.with_suffix(".json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
