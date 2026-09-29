"""Report observed worker use of per-project code maps."""

from __future__ import annotations

import argparse
from collections import defaultdict
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re


MAP_LINE = re.compile(r"(?m)^CODE_MAP=(.+?)\s+STATUS=(?:FRESH|STALE|UNAVAILABLE)\s*$")
READ_COMMAND = re.compile(r"\b(?:cat|type|more|gc|get-content|read_file|sed|grep|rg|select-string|head|tail)\b", re.I)
TARGETED_COMMAND = re.compile(r"\b(?:rg|grep|select-string)\b|\bsed\b[^\r\n]*(?:-n|\d+(?:,\d+)?[pP])|\b(?:get-content|gc)\b[^\r\n]*(?:-totalcount|-tail|-first|-skip)|\bselect-object\b[^\r\n]*(?:-first|-skip)|\b(?:head|tail)\b", re.I)
EXEC_COMMAND = re.compile(r'\bexec_command\s*\(\s*\{\s*cmd\s*:\s*("(?:\\.|[^"\\])*")', re.S)


def timestamp(value: str) -> datetime:
    if not isinstance(value, str):
        raise ValueError("timestamp must be a string")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.utcoffset() != timezone.utc.utcoffset(parsed):
        raise argparse.ArgumentTypeError("--since must be a UTC ISO 8601 timestamp")
    return parsed.astimezone(timezone.utc)


def rows(path: Path):
    with path.open(encoding="utf-8") as stream:
        for line in stream:
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(value, dict):
                yield value


def text_content(value) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(text_content(item) for item in value)
    if isinstance(value, dict):
        return "\n".join(text_content(value[key]) for key in ("text", "output", "content") if key in value)
    return ""


def command_text(payload: dict) -> list[str]:
    value = payload.get("arguments", payload.get("input", ""))
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError:
            value = {"script": value}
    if isinstance(value, dict):
        texts = [str(value[key]) for key in ("command", "cmd", "script") if key in value]
    else:
        texts = [str(value)]
    commands = []
    for text in texts:
        nested = []
        for match in EXEC_COMMAND.finditer(text):
            literal = re.sub(r'(?<!\\)\\(?!["\\/bfnrt]|u[0-9a-fA-F]{4})', r'\\\\', match.group(1))
            try:
                nested.append(json.loads(literal))
            except json.JSONDecodeError:
                continue
        commands.extend(nested or [text])
    return commands


def trace(path: Path) -> dict | None:
    meta = None
    prompt = []
    calls = {}
    operations = []
    valid_rows = 0
    try:
        with path.open(encoding="utf-8") as stream:
            for line in stream:
                try:
                    item = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(item, dict):
                    continue
                valid_rows += 1
                payload = item.get("payload") or {}
                if not isinstance(payload, dict):
                    continue
                kind = payload.get("type")
                if item.get("type") == "session_meta":
                    meta = payload
                elif item.get("type") == "response_item" and kind == "message" and payload.get("role") == "user":
                    prompt.append(text_content(payload.get("content")))
                elif item.get("type") == "event_msg" and kind == "item_completed":
                    event_item = payload.get("item") or {}
                    if isinstance(event_item, dict) and event_item.get("type") == "UserMessage":
                        prompt.append(text_content(event_item.get("content")))
                elif item.get("type") == "response_item" and kind in ("function_call", "custom_tool_call"):
                    calls[payload.get("call_id")] = command_text(payload)
                elif item.get("type") == "response_item" and kind in ("function_call_output", "custom_tool_call_output"):
                    output = text_content(payload.get("output"))
                    operations.extend((command, output) for command in calls.get(payload.get("call_id"), []))
    except (OSError, UnicodeError):
        return None
    if not valid_rows or not isinstance(meta, dict) or meta.get("originator") != "codex_exec":
        return None
    thread_id = meta.get("id") or meta.get("session_id")
    text = "\n".join(prompt)
    match = MAP_LINE.search(text)
    if not match:
        return {"thread_id": thread_id, "eligible": False, "operations": []}
    map_path = match.group(1).strip().strip("\"'")
    normalized_path = map_path.replace("\\", "/").rstrip("/").casefold()
    reads = []
    for command, output in operations:
        normalized_command = command.replace("\\", "/").casefold()
        if (normalized_path not in normalized_command or not READ_COMMAND.search(command)
                or re.search(r"\b(?:set-content|add-content|out-file|write-file)\b|>{1,2}", command, re.I)):
            continue
        reads.append({"kind": "targeted" if TARGETED_COMMAND.search(command) else "whole", "bytes": len(output.encode("utf-8")) if output else None})
    return {"thread_id": thread_id, "eligible": True, "operations": reads}


def provider_name(value) -> str:
    name = str(value or "unknown").casefold()
    if name.startswith("codex"):
        return "codex"
    if name.startswith(("agy", "gemini", "google")):
        return "agy"
    if name.startswith("opencode"):
        return "opencode"
    return name


def event_jobs(events: Path, since: datetime) -> list[dict]:
    starts = {}
    finish_by_job = {}
    for path in events.glob("**/session_*.jsonl"):
        for event in rows(path):
            kind = event.get("event_type")
            payload = event.get("payload") or {}
            if not isinstance(payload, dict):
                continue
            key = (event.get("session_id"), event.get("task_id"), event.get("agent_id"),
                   payload.get("phase", "implementation"))
            if kind == "IMPLEMENTER_FINISHED":
                finish_by_job[key] = {"provider": provider_name(payload.get("implementer")),
                                      "thread_id": payload.get("thread_id")}
            if kind != "IMPLEMENTER_STARTED" or payload.get("phase") not in (None, "implementation"):
                continue
            try:
                started = timestamp(event.get("ts_utc", ""))
            except (ValueError, TypeError, argparse.ArgumentTypeError):
                continue
            if started < since:
                continue
            starts[key] = {"provider": provider_name(payload.get("implementer")), "started": started}
    jobs = []
    for key, start in starts.items():
        finish = finish_by_job.get(key, {})
        provider = start["provider"]
        if provider == "unknown":
            provider = finish.get("provider", "unknown")
        jobs.append({"provider": provider,
                     "thread_id": finish.get("thread_id")})
    return jobs


def report(sessions: Path, events: Path, since: datetime) -> dict:
    jobs = event_jobs(events, since)
    traces = {}
    for path in sessions.glob("**/rollout-*.jsonl"):
        result = trace(path)
        if result is not None and result["thread_id"]:
            traces[result["thread_id"]] = result

    providers = defaultdict(lambda: {"jobs": 0, "eligible_jobs": None, "map_open_jobs": None,
                                     "whole_map_reads": None, "targeted_map_reads": None,
                                     "bytes_read": None, "bytes_measured_reads": None,
                                     "unobservable_jobs": 0, "ineligible_jobs": None,
                                     "eligible_no_open_jobs": None})
    for job in jobs:
        provider = providers[job["provider"]]
        provider["jobs"] += 1
        if job["provider"] != "codex":
            provider["unobservable_jobs"] += 1
            continue
        for field in ("eligible_jobs", "map_open_jobs", "whole_map_reads", "targeted_map_reads",
                      "bytes_read", "bytes_measured_reads", "ineligible_jobs", "eligible_no_open_jobs"):
            if provider[field] is None:
                provider[field] = 0
        observation = traces.get(job.get("thread_id"))
        if observation is None:
            provider["unobservable_jobs"] += 1
        elif not observation["eligible"]:
            provider["ineligible_jobs"] += 1
        else:
            provider["eligible_jobs"] += 1
            if observation["operations"]:
                provider["map_open_jobs"] += 1
                for operation in observation["operations"]:
                    provider[operation["kind"] + "_map_reads"] += 1
                    if operation["bytes"] is not None:
                        provider["bytes_read"] += operation["bytes"]
                        provider["bytes_measured_reads"] += 1
            else:
                provider["eligible_no_open_jobs"] += 1
    return {"since": since.isoformat().replace("+00:00", "Z"),
            "trace_root": str(sessions), "telemetry_root": str(events),
            "providers": dict(sorted(providers.items()))}


def markdown(result: dict) -> str:
    lines = ["# Code map use report", "", f"Since: {result['since']} UTC",
             f"Codex traces: `{result['trace_root']}` (recursive `rollout-*.jsonl`) ",
             f"Purser telemetry: `{result['telemetry_root']}`", "",
             "| Provider | Jobs | Eligible | Opened map | Whole reads | Targeted reads | Bytes read | Unobservable | Ineligible | Eligible, no open |",
             "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"]
    for name, values in result["providers"].items():
        fields = ("jobs", "eligible_jobs", "map_open_jobs", "whole_map_reads", "targeted_map_reads",
                  "bytes_read", "unobservable_jobs", "ineligible_jobs", "eligible_no_open_jobs")
        cells = ["unobservable" if values[field] is None else str(values[field]) for field in fields]
        lines.append("| " + name + " | " + " | ".join(cells) + " |")
    lines.extend(("", "Bytes count UTF-8 text returned by the worker trace; 0 means no measurable returned text."))
    return "\n".join(lines) + "\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--since", required=True, type=timestamp, help="UTC ISO 8601 start time")
    parser.add_argument("--out", type=Path, help="write report to this path instead of stdout")
    parser.add_argument("--json", action="store_true", help="write JSON instead of Markdown")
    parser.add_argument("--sessions", type=Path, default=Path.home() / ".codex" / "sessions", help=argparse.SUPPRESS)
    default_state = (Path(os.environ["PURSER_STATE_DIR"]).expanduser() if os.environ.get("PURSER_STATE_DIR") else
                     Path(os.environ.get("CLAUDE_CONFIG_DIR", Path.home() / ".claude")) / "purser")
    parser.add_argument("--events", type=Path, default=default_state / "telemetry" / "events", help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    result = report(args.sessions, args.events, args.since)
    content = json.dumps(result, indent=2) + "\n" if args.json else markdown(result)
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(content, encoding="utf-8")
    else:
        print(content, end="")


if __name__ == "__main__":
    main()
