"""Summarize narrowly classified mechanical edit repetitions from Purser telemetry."""

from __future__ import annotations

import argparse
from collections import defaultdict
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re


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


def report(state_root: Path, since: datetime) -> dict:
    since = timestamp(since) if isinstance(since, str) else since
    jobs: dict[tuple[str, str, str], dict] = {}
    for project_dir in sorted((state_root / "projects").glob("*")):
        event_dir = project_dir / "telemetry" / "events" / "by_task"
        for path in sorted(event_dir.glob("*.jsonl")):
            try:
                lines = path.read_text(encoding="utf-8").splitlines()
            except (OSError, UnicodeDecodeError):
                continue
            for line in lines:
                try:
                    event = json.loads(line)
                    if not isinstance(event, dict) or event.get("event_type") != "MECHANICAL_EDIT_OBSERVED":
                        continue
                    event_time = timestamp(event.get("ts_utc", ""))
                except (ValueError, TypeError, AttributeError, argparse.ArgumentTypeError):
                    continue
                if event_time < since:
                    continue
                task_id = event.get("task_id")
                payload = event.get("payload")
                if not isinstance(task_id, str) or not isinstance(payload, dict):
                    continue
                thread_id = payload.get("thread_id")
                if not isinstance(thread_id, str) or not thread_id:
                    continue
                key = (project_dir.name, task_id, thread_id)
                jobs.setdefault(key, {
                    "task_id": task_id,
                    "classification": payload.get("classification"),
                    "fingerprint": payload.get("fingerprint"),
                })

    classified = [job for job in jobs.values() if job["classification"] == "rename"
                  and isinstance(job["fingerprint"], str)
                  and re.fullmatch(r"[0-9a-f]{64}", job["fingerprint"])]
    unclassified_count = len(jobs) - len(classified)
    fingerprints: dict[str, list[str]] = defaultdict(list)
    for job in classified:
        fingerprints[job["fingerprint"]].append(job["task_id"])
    repeated = [
        {"fingerprint": fingerprint, "count": len(task_ids), "task_ids": sorted(set(task_ids))[:5]}
        for fingerprint, task_ids in sorted(fingerprints.items()) if len(task_ids) > 1
    ][:5]
    return {
        "since": since.isoformat().replace("+00:00", "Z"),
        "usable_diff_jobs": len(jobs),
        "classified_jobs": len(classified),
        "unclassified_jobs": unclassified_count,
        "repeated_fingerprints": repeated,
    }


def markdown(result: dict) -> str:
    lines = [
        "# Mechanical repeat report",
        "",
        f"Since (UTC): {result['since']}",
        f"Jobs with a usable review diff: {result['usable_diff_jobs']}",
        f"Classified identifier renames: {result['classified_jobs']}",
        f"Unclassified: {result['unclassified_jobs']}",
        "",
        "| Fingerprint (SHA-256) | Jobs | Task IDs (up to 5) |",
        "| --- | ---: | --- |",
    ]
    for item in result["repeated_fingerprints"]:
        task_ids = ", ".join(task_id.replace("|", "\\|") for task_id in item["task_ids"])
        lines.append(f"| `{item['fingerprint']}` | {item['count']} | {task_ids} |")
    if not result["repeated_fingerprints"]:
        lines.append("| No repeated fingerprints | 0 | |")
    return "\n".join(lines) + "\n"


def main(argv=None) -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--since", required=True, type=timestamp, help="Include events at or after this UTC time")
    parser.add_argument("--out", type=Path, required=True, help="Markdown report path")
    parser.add_argument("--json", action="store_true", help="Also write JSON beside the Markdown report")
    args = parser.parse_args(argv)
    root = default_state_root()
    result = report(root, args.since)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(markdown(result), encoding="utf-8")
    if args.json:
        args.out.with_suffix(".json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
