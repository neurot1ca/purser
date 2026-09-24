"""Deterministic context compaction advice for UserPromptSubmit."""

from __future__ import annotations

from collections.abc import Mapping
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import tomllib
from typing import Any

import project_root
import telemetry


DEFAULT_THRESHOLD_TOKENS = 250_000
DEFAULT_IDLE_MINUTES = 20
DEFAULT_REGROW_TOKENS = 60_000
MAX_TRANSCRIPT_BYTES = 4 * 1024 * 1024
MAX_HANDOFF_CONTEXT_CHARS = 6_000
HANDOFF_MAX_AGE_SECONDS = 12 * 60 * 60
STATE_START = "<!-- PURSER:STATE:START -->"
STATE_END = "<!-- PURSER:STATE:END -->"
HANDOFF_TEMPLATE = """# Purser handoff
## Goal
## State
## Decisions
## Next
## Open risks
## Files to load
<!-- PURSER:STATE:START -->
<!-- PURSER:STATE:END -->
"""
_CODEX_TOOLS = {"mcp__plugin_purser_codex-main__codex": "codex", "mcp__plugin_purser_codex-main__codex-reply": "codex-reply"}
_ORCH_TASK = re.compile(r"^ORCH\|task=([^|\s]+)\|")


def _settings() -> dict[str, Any]:
    defaults: dict[str, Any] = {
        "enabled": True,
        "threshold_tokens": DEFAULT_THRESHOLD_TOKENS,
        "idle_minutes": DEFAULT_IDLE_MINUTES,
        "regrow_tokens": DEFAULT_REGROW_TOKENS,
    }
    try:
        with (Path.home() / ".orchestrator" / "config.toml").open("rb") as stream:
            section = tomllib.load(stream).get("compaction", {})
        if not isinstance(section, Mapping):
            return defaults
        for name in ("threshold_tokens", "idle_minutes", "regrow_tokens"):
            value = section.get(name)
            if type(value) is int and value > 0:
                defaults[name] = value
        if type(section.get("enabled")) is bool:
            defaults["enabled"] = section["enabled"]
    except (OSError, UnicodeError, ValueError, tomllib.TOMLDecodeError):
        pass
    return defaults


def _read_transcript(path: Any) -> list[dict[str, Any]]:
    if not isinstance(path, str) or not path:
        return []
    try:
        with Path(path).open("rb") as stream:
            stream.seek(0, os.SEEK_END)
            size = stream.tell()
            stream.seek(max(0, size - MAX_TRANSCRIPT_BYTES))
            raw = stream.read(MAX_TRANSCRIPT_BYTES)
    except OSError:
        return []
    if size > MAX_TRANSCRIPT_BYTES:
        raw = raw.split(b"\n", 1)[-1]
    records = []
    for line in raw.splitlines():
        try:
            item = json.loads(line.decode("utf-8"))
        except (UnicodeError, json.JSONDecodeError):
            continue
        if isinstance(item, dict):
            records.append(item)
    return records


def _message(record: Mapping[str, Any]) -> Mapping[str, Any]:
    value = record.get("message")
    return value if isinstance(value, Mapping) else record


def _record_type(record: Mapping[str, Any]) -> str:
    return str(record.get("type", _message(record).get("role", ""))).lower()


def _content(record: Mapping[str, Any]) -> list[Any]:
    value = _message(record).get("content", [])
    return value if isinstance(value, list) else []


def _usage_context(records: list[dict[str, Any]]) -> int:
    for record in reversed(records):
        if _record_type(record) != "assistant":
            continue
        usage = _message(record).get("usage")
        if isinstance(usage, Mapping):
            keys = ("input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
            return sum(v for k in keys if type(v := usage.get(k, 0)) is int and v >= 0)
    return 0


def _genuine_user(record: Mapping[str, Any]) -> bool:
    if _record_type(record) != "user":
        return False
    return not any(isinstance(block, Mapping) and block.get("type") == "tool_result" for block in _content(record))


def _timestamp(record: Mapping[str, Any]) -> datetime | None:
    raw = record.get("timestamp") or _message(record).get("timestamp")
    if not isinstance(raw, str):
        return None
    try:
        value = datetime.fromisoformat(raw.replace("Z", "+00:00"))
        return value.replace(tzinfo=timezone.utc) if value.tzinfo is None else value.astimezone(timezone.utc)
    except ValueError:
        return None


def _prompt_from_input(value: Any) -> str:
    return value.get("prompt", "") if isinstance(value, Mapping) and isinstance(value.get("prompt"), str) else ""


def _result_error(block: Mapping[str, Any]) -> bool:
    return block.get("is_error") is True


def _extract_json(text: str) -> dict[str, Any]:
    try:
        value = json.loads(text)
        return value if isinstance(value, dict) else {}
    except (ValueError, TypeError):
        match = re.search(r"\{.*\}", text, re.DOTALL)
        if match:
            try:
                value = json.loads(match.group(0))
                return value if isinstance(value, dict) else {}
            except ValueError:
                pass
    return {}


def _result_text(block: Mapping[str, Any]) -> str:
    content = block.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(str(item.get("text", "")) for item in content if isinstance(item, Mapping))
    return ""


def _transcript_facts(records: list[dict[str, Any]], now: datetime, idle_minutes: int) -> tuple[list[str], str | None, set[str]]:
    latest_user = max((i for i, item in enumerate(records) if _genuine_user(item)), default=-1)
    segment = records[latest_user + 1 :]
    last_assistant = next((r for r in reversed(segment) if _record_type(r) == "assistant"), None)
    triggers: list[str] = []
    if last_assistant:
        stamp = _timestamp(last_assistant)
        if stamp and (now - stamp).total_seconds() >= idle_minutes * 60:
            triggers.append("idle")

    uses: dict[str, tuple[str, Mapping[str, Any], int]] = {}
    results: dict[str, tuple[Mapping[str, Any], int]] = {}
    for index, record in enumerate(segment):
        for block in _content(record):
            if not isinstance(block, Mapping):
                continue
            if block.get("type") == "tool_use" and isinstance(block.get("id"), str):
                uses[block["id"]] = (str(block.get("name", "")), block.get("input", {}), index)
            elif block.get("type") == "tool_result" and isinstance(block.get("tool_use_id"), str):
                results[block["tool_use_id"]] = (block, index)
    if any(
        name in {"Bash", "PowerShell"}
        and "git commit" in str(params.get("command", "")).lower()
        and tool_id in results
        and not _result_error(results[tool_id][0])
        for tool_id, (name, params, _index) in uses.items()
        if isinstance(params, Mapping)
    ):
        triggers.append("commit")

    completed_task_ids: set[str] = set()
    latest_task_id: str | None = None
    for tool_id, (name, params, _use_index) in uses.items():
        if name not in _CODEX_TOOLS:
            continue
        prompt = _prompt_from_input(params)
        match = _ORCH_TASK.match(prompt)
        task_id = match.group(1) if match else None
        if task_id:
            latest_task_id = task_id
        result_entry = results.get(tool_id)
        if result_entry:
            result_index = result_entry[1]
            answer_after = any(_record_type(r) == "assistant" for r in segment[result_index + 1 :])
            if task_id and answer_after:
                completed_task_ids.add(task_id)
    return triggers, latest_task_id, completed_task_ids


def _transcript_facts_all(records: list[dict[str, Any]]) -> tuple[list[str], str | None, list[dict[str, Any]]]:
    uses: dict[str, tuple[str, Mapping[str, Any]]] = {}
    results: dict[str, Mapping[str, Any]] = {}
    for record in records:
        for block in _content(record):
            if not isinstance(block, Mapping):
                continue
            if block.get("type") == "tool_use" and isinstance(block.get("id"), str):
                uses[block["id"]] = (str(block.get("name", "")), block.get("input", {}))
            elif block.get("type") == "tool_result" and isinstance(block.get("tool_use_id"), str):
                results[block["tool_use_id"]] = block
    calls: list[dict[str, Any]] = []
    last_task = None
    for index, record in enumerate(records):
        for block in _content(record):
            if not isinstance(block, Mapping) or block.get("type") != "tool_use":
                continue
            tool_id = block.get("id")
            name, params = uses.get(tool_id, ("", {}))
            if name not in _CODEX_TOOLS:
                continue
            match = _ORCH_TASK.match(_prompt_from_input(params))
            task_id = match.group(1) if match else None
            if task_id:
                last_task = task_id
            result = results.get(tool_id)
            if result is None:
                continue
            data = _extract_json(_result_text(result))
            calls.append({"task_id": task_id or "unknown", "tool": _CODEX_TOOLS[name], "threadId": data.get("threadId") or data.get("thread_id"), "claude_review_mode": data.get("claude_review_mode")})
    return [], last_task, calls


def _state_directory(repository: Path) -> Path:
    key = project_root.project_key(repository)
    return telemetry._project_root(telemetry._root(None), key)


def handoff_path(repository: Path) -> Path:
    """Return the persistent handoff path for a repository."""
    return _state_directory(repository) / "purser-handoff.md"


def _empty_handoff(content: str) -> bool:
    template_lines = {line.strip() for line in HANDOFF_TEMPLATE.splitlines() if line.strip()}
    return not any(
        line.strip() and line.strip() not in template_lines
        for line in content.splitlines()
    )


def restore(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    """Return a bounded historical handoff for an eligible SessionStart."""
    if not _settings()["enabled"] or repository is None:
        return None
    source = payload.get("source")
    if not isinstance(source, str) or source not in {"compact", "clear", "startup", "resume"}:
        return None
    path = handoff_path(repository)
    try:
        stat = path.stat()
        if source in {"startup", "resume"}:
            age = time.time() - stat.st_mtime
            if age < 0 or age >= HANDOFF_MAX_AGE_SECONDS:
                return None
        content = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return None
    if _empty_handoff(content):
        return None

    header = (
        "Purser handoff (historical reference, written before the context was compacted or cleared; "
        "verify against git and files before acting; do not re-run commands from it):"
    )
    footer = f"Full file: {path}"
    full = f"{header}\n{content.rstrip()}\n{footer}"
    if len(full) <= MAX_HANDOFF_CONTEXT_CHARS:
        return full
    truncation = f"... truncated; read the full file at {path}"
    content_budget = MAX_HANDOFF_CONTEXT_CHARS - len(header) - len(truncation) - 2
    if content_budget < 0:
        return (header + "\n" + truncation)[-MAX_HANDOFF_CONTEXT_CHARS:]
    clipped = content.rstrip()[:content_budget].rstrip()
    result = f"{header}\n{clipped}\n{truncation}"
    return result[:MAX_HANDOFF_CONTEXT_CHARS]


def _read_state(path: Path, session_id: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        if isinstance(value, dict) and value.get("session_id") == session_id:
            return {
                "last_suggested_context": max(0, int(value.get("last_suggested_context", 0))),
                "last_task_id": value.get("last_task_id") if isinstance(value.get("last_task_id"), str) else None,
                "session_id": session_id,
            }
    except (OSError, ValueError, TypeError):
        pass
    return {"last_suggested_context": 0, "last_task_id": None, "session_id": session_id}


def _atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(mode="wb", prefix=path.name + ".", suffix=".tmp", dir=path.parent, delete=False) as handle:
            temporary = Path(handle.name)
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary:
            try:
                temporary.unlink()
            except OSError:
                pass


def _replace_state_block(path: Path, body: str) -> None:
    if len(body) > 1500:
        body = body[:1486].rstrip() + "\n... truncated"
    if not path.exists():
        content = HANDOFF_TEMPLATE
    else:
        content = path.read_text(encoding="utf-8")
    if STATE_START in content and STATE_END in content and content.index(STATE_START) < content.index(STATE_END):
        start = content.index(STATE_START) + len(STATE_START)
        end = content.index(STATE_END)
        content = content[:start] + "\n" + body + "\n" + content[end:]
    else:
        content = content.rstrip() + f"\n{STATE_START}\n{body}\n{STATE_END}\n"
    _atomic_write(path, content.encode("utf-8"))


def _mechanical_state(repository: Path, context: int, triggers: list[str], calls: list[dict[str, Any]]) -> str:
    lines = [
        f"Generated UTC: {datetime.now(timezone.utc).isoformat(timespec='seconds')}",
        f"Context tokens: {context}",
        f"Triggers: {', '.join(triggers)}",
        "Last Codex calls:",
    ]
    for call in calls[-5:]:
        lines.append(
            f"- task={call['task_id']} tool={call['tool']} threadId={call.get('threadId') or 'unknown'} "
            f"claude_review_mode={call.get('claude_review_mode') or 'unknown'}"
        )
    try:
        commits = subprocess.run(["git", "log", "--oneline", "-5"], cwd=repository, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=3, check=False)
        if commits.returncode == 0:
            lines.append("Last commits:")
            lines.extend(f"- {line}" for line in commits.stdout.splitlines()[:5])
    except (OSError, subprocess.SubprocessError):
        pass
    try:
        status = subprocess.run(["git", "status", "--porcelain"], cwd=repository, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=3, check=False)
        if status.returncode == 0:
            lines.append(f"Uncommitted changed paths: {sum(bool(line.strip()) for line in status.stdout.splitlines())}")
    except (OSError, subprocess.SubprocessError):
        pass
    full = "\n".join(lines)
    if len(full) > 1500:
        full = full[:1486].rstrip() + "\n... truncated"
    return full


def advise(payload: Mapping[str, Any], repository: Path | None = None, *, now: datetime | None = None) -> str | None:
    """Return UserPromptSubmit additionalContext or None, without model calls."""
    settings = _settings()
    if not settings["enabled"]:
        return None
    if repository is None:
        repository, _reason = project_root.resolve_project_root(payload)
    if repository is None:
        return None
    session_id = payload.get("session_id")
    if not isinstance(session_id, str) or not session_id:
        return None
    records = _read_transcript(payload.get("transcript_path"))
    context = _usage_context(records)
    clock = now or datetime.now(timezone.utc)
    if clock.tzinfo is None:
        clock = clock.replace(tzinfo=timezone.utc)
    triggers, current_task_id, completed_current_tasks = _transcript_facts(records, clock, settings["idle_minutes"])
    _unused_triggers, latest_task_id, calls = _transcript_facts_all(records)

    directory = _state_directory(repository)
    state_path = directory / "compaction_advisor.json"
    state = _read_state(state_path, session_id)
    task_completed = False
    task_completed = bool(current_task_id and current_task_id in completed_current_tasks and current_task_id != state["last_task_id"])
    if task_completed:
        triggers.append("task_end")
    state["last_task_id"] = latest_task_id or state["last_task_id"]

    if context < settings["threshold_tokens"]:
        state["last_suggested_context"] = 0
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return None
    if not triggers:
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return None
    last_suggested = state["last_suggested_context"]
    if last_suggested and context < last_suggested + settings["regrow_tokens"]:
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return None

    directory.mkdir(parents=True, exist_ok=True)
    handoff = handoff_path(repository)
    _replace_state_block(handoff, _mechanical_state(repository, context, triggers, calls))
    state["last_suggested_context"] = context
    _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    return (
        f"Purser: {context:,} context tokens; trigger(s): {', '.join(triggers)}. Handoff: {handoff}. "
        f"After answering the current message, update its Goal, State, Decisions, Next, Open risks, Files to load sections (under 4,000 characters total); link by path to any existing repository handoff such as NEXT.md or HANDOFF.md. End your reply in the user's language saying the handoff is ready at {handoff} and running /compact now saves tokens. Do not compact or clear context yourself."
    )


__all__ = ["advise"]
