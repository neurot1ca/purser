"""Deterministic context compaction advice for UserPromptSubmit."""

from __future__ import annotations

from collections.abc import Mapping
from contextlib import nullcontext
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import tomllib
from typing import Any

import ledger
import active_coder_state
import project_root
import state_root
import telemetry


DEFAULT_THRESHOLD_TOKENS = 250_000
DEFAULT_IDLE_MINUTES = 20
DEFAULT_REGROW_TOKENS = 60_000
POST_COMPACT_CONTEXT_TOKENS = 75_000
MAX_TRANSCRIPT_BYTES = 4 * 1024 * 1024
MAX_HANDOFF_CONTEXT_CHARS = 6_000
HANDOFF_MAX_AGE_SECONDS = 12 * 60 * 60
STATE_START = "<!-- PURSER:STATE:START -->"
STATE_END = "<!-- PURSER:STATE:END -->"
RECENT_START = "<!-- PURSER:RECENT:START -->"
RECENT_END = "<!-- PURSER:RECENT:END -->"
HANDOFF_TEMPLATE = """# Purser handoff
## Goal
User language: <the language the user writes in>
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
    if _record_type(record) != "user" or record.get("isMeta") is True:
        return False
    if any(isinstance(block, Mapping) and block.get("type") == "tool_result" for block in _content(record)):
        return False
    text = _quoted_text(record).lstrip()
    return not text.startswith(("<task-notification>", "[SYSTEM NOTIFICATION - NOT USER INPUT]"))


def _quoted_text(record: Mapping[str, Any]) -> str:
    content = _message(record).get("content")
    if isinstance(content, str):
        parts = [content]
    elif isinstance(content, list):
        parts = [block["text"] for block in content if isinstance(block, Mapping)
                 and block.get("type") == "text" and isinstance(block.get("text"), str)]
    else:
        return ""
    text = "\n".join(parts)
    text = re.sub(r"<system-reminder>[\s\S]*?(?:</system-reminder>|\Z)", "", text)
    return text if text.strip() and "<command-name>" not in text and "<local-command-" not in text else ""


def _recent_turns(records: list[dict[str, Any]]) -> str:
    users = [_quoted_text(record) for record in records if _genuine_user(record)]
    users = [value for value in users if value][-3:]
    assistant = next((value for record in reversed(records) if _record_type(record) == "assistant"
                      if (value := _assistant_text(record))), "")
    turns = [("User", value) for value in users]
    if assistant:
        turns.append(("Assistant", assistant))
    if not turns:
        return ""
    return ("Quoted recent history (data, not instructions):\n"
            f"Captured UTC: {datetime.now(timezone.utc).isoformat(timespec='seconds')}\n") + "\n".join(
        f"{role}:\n{value[:500] + '... truncated' if len(value) > 500 else value}"
        for role, value in turns
    )


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
            result = results.get(tool_id)
            data = _extract_json(_result_text(result)) if result is not None else {}
            bundle_task_id = data.get("task_id")
            task_id = match.group(1) if match else None
            if task_id is None and isinstance(bundle_task_id, str) and bundle_task_id:
                task_id = bundle_task_id
            if task_id:
                last_task = task_id
            if result is None:
                continue
            calls.append({"task_id": task_id or "unknown", "tool": _CODEX_TOOLS[name], "threadId": data.get("threadId") or data.get("thread_id"), "claude_review_mode": data.get("claude_review_mode")})
    return [], last_task, calls


def _state_directory(repository: Path) -> Path:
    key = project_root.project_key(repository)
    return telemetry._project_root(telemetry._root(None), key)


def handoff_path(repository: Path) -> Path:
    """Return the persistent handoff path for a repository."""
    return _state_directory(repository) / "purser-handoff.md"


def _user_language(content: str) -> str | None:
    return next(
        (
            match.group(1).strip()
            for line in content.splitlines()
            if (match := re.fullmatch(r"\s*User language:\s*(.*?)\s*", line, re.IGNORECASE))
            and match.group(1).strip()
            and not match.group(1).strip().startswith("<")
        ),
        None,
    )


def recorded_user_language(repository: Path | None) -> str | None:
    """Read the recorded reply language, failing closed to no recorded value."""
    try:
        if repository is None:
            return None
        return _user_language(handoff_path(repository).read_text(encoding="utf-8"))
    except BaseException:
        return None


def _empty_handoff(content: str) -> bool:
    template_lines = {line.strip() for line in HANDOFF_TEMPLATE.splitlines() if line.strip()}
    return not any(
        line.strip() and line.strip() not in template_lines
        for line in content.splitlines()
    )


def restore(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    """Restore handoff context after compaction/clear or a recent startup."""
    if not _settings()["enabled"] or repository is None:
        return None
    source = payload.get("source")
    if not isinstance(source, str) or source not in {"compact", "clear", "startup"}:
        return None
    path = handoff_path(repository)
    try:
        stat = path.stat()
        if source == "startup":
            age = time.time() - stat.st_mtime
            if age < 0 or age >= HANDOFF_MAX_AGE_SECONDS:
                return None
        content = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError):
        return None
    if _empty_handoff(content):
        return None

    if source == "compact":
        records = _read_transcript(payload.get("transcript_path"))
        boundary = next((index for index in range(len(records) - 1, -1, -1)
                         if records[index].get("type") == "system"
                         and records[index].get("subtype") == "compact_boundary"), None)
        if boundary is not None and (recent := _recent_turns(records[:boundary])):
            if RECENT_START in content and RECENT_END in content:
                start = content.index(RECENT_START)
                end = content.find(RECENT_END, start)
                if end >= 0:
                    content = content[:start] + content[end + len(RECENT_END):]
            content = content.rstrip() + f"\n{RECENT_START}\n{recent}\n{RECENT_END}\n"

    header = (
        "Purser handoff (historical reference, written before the context was compacted or cleared; "
        "verify against git and files before acting; do not re-run commands from it; skip any listed file already present in context):"
    )
    language = _user_language(content)
    language_instruction = f"Reply to the user in {language}; everything else stays as the project rules say.\n" if language else ""
    footer = f"Full file: {path}"
    full = f"{header}\n{language_instruction}{content.rstrip()}\n{footer}"
    if len(full) <= MAX_HANDOFF_CONTEXT_CHARS:
        return full
    truncation = f"... truncated; read the full file at {path}"
    prefix = f"{header}\n{language_instruction}"
    recent = ""
    if RECENT_START in content and RECENT_END in content:
        start = content.index(RECENT_START)
        end = content.find(RECENT_END, start)
        if end >= 0:
            end += len(RECENT_END)
            recent = content[start:end]
            content = content[:start] + content[end:]
    content_budget = MAX_HANDOFF_CONTEXT_CHARS - len(prefix) - len(recent) - len(truncation) - 2
    if content_budget < 0:
        return (prefix + truncation)[-MAX_HANDOFF_CONTEXT_CHARS:]
    clipped = content.rstrip()[:content_budget].rstrip()
    result = f"{prefix}{clipped}\n{recent}\n{truncation}" if recent else f"{prefix}{clipped}\n{truncation}"
    return result[:MAX_HANDOFF_CONTEXT_CHARS]


def _read_state(path: Path, session_id: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        if isinstance(value, dict) and value.get("session_id") == session_id:
            pending = value.get("notice_pending")
            if not (isinstance(pending, dict) and type(pending.get("context")) is int
                    and type(pending.get("percentage")) is int
                    and isinstance(pending.get("handoff"), str)):
                pending = None
            receipt = value.get("receipt_pending")
            if not (isinstance(receipt, dict) and isinstance(receipt.get("model"), str)
                    and receipt["model"] and isinstance(receipt.get("effort"), str)):
                receipt = None
            dispatch = value.get("dispatch_pending")
            if not isinstance(dispatch, dict):
                dispatch = None
            else:
                lines = dispatch.get("lines")
                board = dispatch.get("board")
                board_job_ids = dispatch.get("board_job_ids", [])
                dispatch = ({"lines": lines, "board": board, "board_job_ids": board_job_ids,
                             "blocked": dispatch.get("blocked") is True}
                            if isinstance(lines, list) and all(isinstance(line, str) and line for line in lines)
                            and (board is None or isinstance(board, str))
                            and isinstance(board_job_ids, list)
                            and all(isinstance(job_id, str) for job_id in board_job_ids) else None)
            version_pending = value.get("version_pending")
            if not (isinstance(version_pending, dict) and isinstance(version_pending.get("version"), str)
                    and version_pending["version"]):
                version_pending = None
            elif version_pending is not None:
                version_pending = {"version": version_pending["version"], "blocked": version_pending.get("blocked") is True}
            leader_pending = value.get("leader_pending")
            if not (isinstance(leader_pending, dict) and isinstance(leader_pending.get("model"), str)
                    and leader_pending["model"] and type(leader_pending.get("successes")) is int
                    and type(leader_pending.get("sample_count")) is int):
                leader_pending = None
            elif leader_pending is not None:
                leader_pending = {"model": leader_pending["model"], "successes": leader_pending["successes"],
                                  "sample_count": leader_pending["sample_count"],
                                  "blocked": leader_pending.get("blocked") is True}
            fallback_pending = value.get("fallback_pending")
            if (not isinstance(fallback_pending, dict)
                    or not isinstance(fallback_pending.get("line"), str)
                    or not isinstance(fallback_pending.get("project"), str)
                    or not isinstance(fallback_pending.get("model"), str)):
                fallback_pending = None
            else:
                fallback_pending = {"line": fallback_pending["line"],
                                    "project": fallback_pending["project"],
                                    "model": fallback_pending["model"],
                                    "blocked": fallback_pending.get("blocked") is True,
                                    "notice": fallback_pending.get("notice") is True}
            return {
                "last_suggested_context": max(0, int(value.get("last_suggested_context", 0))),
                "reminder_messages": max(0, int(value.get("reminder_messages", 0))),
                "reminder_wasted_tokens": max(0, int(value.get("reminder_wasted_tokens", 0))),
                "last_task_id": value.get("last_task_id") if isinstance(value.get("last_task_id"), str) else None,
                "session_id": session_id,
                "notice_pending": pending,
                "receipt_pending": receipt,
                "dispatch_pending": dispatch,
                "version_pending": version_pending,
                "leader_pending": leader_pending,
                "fallback_pending": fallback_pending,
                "flow_blocked_user": value.get("flow_blocked_user") if type(value.get("flow_blocked_user")) is int else None,
            }
    except (OSError, ValueError, TypeError):
        pass
    return {"last_suggested_context": 0, "reminder_messages": 0, "reminder_wasted_tokens": 0,
            "last_task_id": None, "session_id": session_id,
            "notice_pending": None, "receipt_pending": None, "dispatch_pending": None,
            "version_pending": None,
            "leader_pending": None,
            "fallback_pending": None,
            "flow_blocked_user": None}


def plugin_version_state_path() -> Path:
    return state_root.default_state_root() / "plugin-version.json"


def announced_plugin_version() -> str | None:
    try:
        value = json.loads(plugin_version_state_path().read_text(encoding="utf-8"))
        version = value.get("announced_version") if isinstance(value, dict) else None
        return version if isinstance(version, str) and version else None
    except BaseException:
        return None


def record_announced_plugin_version(version: str) -> None:
    _atomic_write(plugin_version_state_path(), json.dumps({"announced_version": version}, sort_keys=True).encode("utf-8"))


def queue_version_announcement(payload: Mapping[str, Any], repository: Path | None, version: str) -> bool:
    try:
        session_id = payload.get("session_id")
        if repository is None or not isinstance(session_id, str) or not session_id:
            return False
        state_path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(state_path, session_id)
        state["version_pending"] = {"version": version, "blocked": False}
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return True
    except BaseException:
        return False


def queue_leader_announcement(payload: Mapping[str, Any], repository: Path | None,
                               model: str, successes: int, sample_count: int) -> bool:
    try:
        session_id = payload.get("session_id")
        if (repository is None or not isinstance(session_id, str) or not session_id
                or not model or type(successes) is not int or type(sample_count) is not int):
            return False
        state_path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(state_path, session_id)
        state["leader_pending"] = {"model": model, "successes": successes,
                                   "sample_count": sample_count, "blocked": False}
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return True
    except BaseException:
        return False


def record_announced_mechanical_leader(model: str) -> None:
    try:
        path = state_root.default_state_root() / "opencode" / "leader.json"
        value = json.loads(path.read_text(encoding="utf-8"))
        if isinstance(value, dict) and value.get("model") == model:
            value["announced"] = True
            _atomic_write(path, json.dumps(value, sort_keys=True).encode("utf-8"))
    except BaseException:
        pass


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


def _replace_state_block(path: Path, body: str, recent: str = "") -> None:
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
    if RECENT_START in content and RECENT_END in content and content.index(RECENT_START) < content.index(RECENT_END):
        start = content.index(RECENT_START)
        end = content.index(RECENT_END) + len(RECENT_END)
        content = content[:start] + content[end:]
    if recent:
        content = content.rstrip() + f"\n{RECENT_START}\n{recent}\n{RECENT_END}\n"
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
        commits = subprocess.run(["git", "log", "--oneline", "-5"], cwd=repository, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=3, check=False,
                                 creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
        if commits.returncode == 0:
            lines.append("Last commits:")
            lines.extend(f"- {line}" for line in commits.stdout.splitlines()[:5])
    except (OSError, subprocess.SubprocessError):
        pass
    try:
        status = subprocess.run(["git", "status", "--porcelain"], cwd=repository, capture_output=True, text=True, encoding="utf-8", errors="replace", timeout=3, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
        if status.returncode == 0:
            lines.append(f"Uncommitted changed paths: {sum(bool(line.strip()) for line in status.stdout.splitlines())}")
    except (OSError, subprocess.SubprocessError):
        pass
    full = "\n".join(lines)
    if len(full) > 1500:
        full = full[:1486].rstrip() + "\n... truncated"
    return full


def _blockquote(notice: Mapping[str, Any], *, bold: bool = False) -> str:
    emphasis = "**" if bold else "*"
    return (
        f"> ⚓ **Context passed {notice['context'] // 1000}K. Purser prepared your handoff.**\n"
        f"> {emphasis}Type `/compact` now to save **~{notice['percentage']}%** on every later message!{emphasis}\n"
        f"> {emphasis}The ship stays dry: decisions and the next step will be read from the handoff Purser prepared. Nothing is lost.{emphasis}\n"
        f"> {emphasis}Handoff: `{notice['handoff']}`{emphasis}"
    )


def _notice_advice(notice: Mapping[str, Any], triggers: list[str]) -> str:
    context = notice["context"]
    handoff = notice["handoff"]
    return (
        f"Purser: {context:,} context tokens; trigger(s): {', '.join(triggers)}. Handoff: {handoff}. "
        "After answering the current message, update its Goal, State, Decisions, Next, Open risks, Files to load sections (under 4,000 characters total). Keep the first Goal line `User language: <language>` set to the language the user writes in. Link by path to any existing repository handoff such as NEXT.md or HANDOFF.md. If your reply asks the user a question, also write that question under Next in the handoff so it survives compaction. End your turn with this as the very last text, after every tool call and everything else including any question: a line `---` and then this four-line blockquote translated into the user's language (keep the markdown exactly):\n"
        + _blockquote(notice)
        + "\nThe percentage is an estimate; keep the ~. Do not compact or clear context yourself."
    )


def _reminder_advice(notice: Mapping[str, Any], wasted_tokens: int, receipt_advice: str | None) -> str:
    reminder = "".join(character + "\u0332" for character in "Reminder")
    return "\n".join(part for part in (
        "Translate this reminder into the user's language, keeping the markdown exactly and the underline on every character of the translated word (markdown has no underline). Put it as the very last text of the turn after a `---` line:",
        f"> **{reminder}: Tokens are leaking; not compacting has cost ~{wasted_tokens // 1000}K tokens so far.**\n" + _blockquote(notice, bold=True),
        receipt_advice,
    ) if part)


def _assistant_text(record: Mapping[str, Any]) -> str:
    return _quoted_text(record)


def queue_receipt(payload: Mapping[str, Any], repository: Path | None,
                  bundle: Mapping[str, Any]) -> None:
    """Replace the session's receipt with the latest finalized Codex bundle."""
    try:
        session_id = payload.get("session_id")
        model = bundle.get("model")
        if repository is None or not isinstance(session_id, str) or not session_id or not isinstance(model, str) or not model:
            return
        receipt: dict[str, Any] = {"model": model, "effort": str(bundle.get("effort") or "unknown"), "blocked": False}
        quota = bundle.get("codex_quota")
        if isinstance(quota, Mapping):
            for key in ("5h_used_pct", "5h_resets_at", "7d_used_pct", "snapshot_at"):
                value = quota.get(key)
                if key.endswith("pct") and type(value) in (int, float):
                    receipt[key] = round(value)
                elif key in ("5h_resets_at", "snapshot_at") and isinstance(value, str):
                    receipt[key] = value
        path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(path, session_id)
        state["receipt_pending"] = receipt
        _atomic_write(path, json.dumps(state, sort_keys=True).encode("utf-8"))
    except BaseException:
        pass


def queue_dispatch(payload: Mapping[str, Any], repository: Path | None, *,
                   line: str | None = None, board: str | None = None,
                   board_job_ids: list[str] | None = None) -> None:
    """Require rendered dispatch text in the next user facing assistant reply."""
    try:
        session_id = payload.get("session_id")
        if repository is None or not isinstance(session_id, str) or not session_id:
            return
        path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(path, session_id)
        pending = state["dispatch_pending"] or {"lines": [], "board": None, "blocked": False}
        if isinstance(line, str) and line and line not in pending["lines"]:
            pending["lines"].append(line)
        if isinstance(board, str) and board:
            pending["board"] = board
            pending["board_job_ids"] = board_job_ids if isinstance(board_job_ids, list) else []
        if line or board:
            pending["blocked"] = False
            state["dispatch_pending"] = pending
            _atomic_write(path, json.dumps(state, sort_keys=True).encode("utf-8"))
    except BaseException:
        pass


def queue_fallback_question(payload: Mapping[str, Any], repository: Path | None,
                            line: str, model: str, *, notice: bool = False) -> bool:
    session_id = payload.get("session_id")
    if repository is None or not isinstance(session_id, str) or not session_id:
        return False
    try:
        path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(path, session_id)
        state["fallback_pending"] = {"line": line, "project": str(repository),
                                      "model": model, "blocked": False, "notice": notice}
        _atomic_write(path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return True
    except BaseException:
        return False


def refresh_dispatch_board(payload: Mapping[str, Any], repository: Path | None, board: str | None,
                           *, board_job_ids: list[str] | None = None) -> None:
    """Replace a board only when this session already has a pending dispatch."""
    try:
        session_id = payload.get("session_id")
        if repository is None or not isinstance(session_id, str) or not session_id:
            return
        path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(path, session_id)
        pending = state["dispatch_pending"]
        if isinstance(pending, dict):
            pending["board"] = board
            pending["board_job_ids"] = board_job_ids if isinstance(board_job_ids, list) else []
            pending["blocked"] = False
            _atomic_write(path, json.dumps(state, sort_keys=True).encode("utf-8"))
    except BaseException:
        pass


def _receipt_quota_current(receipt: Mapping[str, Any], now: datetime | None = None) -> bool:
    captured_at = receipt.get("snapshot_at")
    if isinstance(captured_at, str):
        try:
            captured = datetime.fromisoformat(captured_at.replace("Z", "+00:00"))
            if captured.tzinfo is not None:
                clock = now or datetime.now(timezone.utc)
                if clock.tzinfo is None:
                    clock = clock.replace(tzinfo=timezone.utc)
                if clock.astimezone(captured.tzinfo) - captured > timedelta(hours=1):
                    return False
        except ValueError:
            pass
    reset = receipt.get("5h_resets_at")
    if not isinstance(reset, str):
        return True
    try:
        parsed = datetime.fromisoformat(reset.replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            return True
        clock = now or datetime.now(timezone.utc)
        if clock.tzinfo is None:
            clock = clock.replace(tzinfo=timezone.utc)
        return parsed > clock.astimezone(parsed.tzinfo)
    except ValueError:
        return True


def _receipt_advice(receipt: Mapping[str, Any], now: datetime | None = None) -> str:
    parts = [f"model {receipt['model']}", f"effort {receipt['effort']}"]
    stale = not _receipt_quota_current(receipt, now)
    if stale:
        return ("The quota snapshot is stale. Write one bold line between `---` rules with blank lines around it, "
                "in the user's language, showing " + ", ".join(parts) + ".")
    if "5h_used_pct" in receipt:
        parts.append(f"5h used {receipt['5h_used_pct']}%")
    reset = receipt.get("5h_resets_at")
    if isinstance(reset, str):
        try:
            parsed = datetime.fromisoformat(reset.replace("Z", "+00:00"))
            if parsed.tzinfo is not None:
                clock = now or datetime.now(timezone.utc)
                if clock.tzinfo is None:
                    clock = clock.replace(tzinfo=timezone.utc)
                minutes = max(0, int((parsed - clock.astimezone(parsed.tzinfo)).total_seconds() // 60))
                parts.append(f"time until 5h reset {minutes // 60}h {minutes % 60}m")
        except ValueError:
            pass
    if "7d_used_pct" in receipt:
        parts.append(f"7d used {receipt['7d_used_pct']}%")
    return ("Write one bold line between `---` rules with blank lines around it, in the user's language, "
            "showing " + ", ".join(parts) + ".")


def _receipt_satisfied(receipt: Mapping[str, Any], message: str, now: datetime) -> bool:
    if receipt["model"] not in message or receipt["effort"] not in message:
        return False
    if not _receipt_quota_current(receipt, now):
        return True
    if "7d_used_pct" in receipt:
        percentage = re.escape(str(receipt["7d_used_pct"]))
        if not re.search(r"(?<!\d)(?:" + percentage + r"\s*%|%\s*" + percentage + r")(?!\d)", message):
            return False
    return True


def _dispatch_advice(dispatch: Mapping[str, Any]) -> str:
    lines = dispatch["lines"]
    board = dispatch.get("board")
    if not lines:
        return "Include this latest job board verbatim:\n" + board if isinstance(board, str) else ""
    advice = ("Write one bold line between `---` rules with blank lines around it, in the user's language, "
              "including every exact dispatch receipt line below verbatim:\n" + "\n".join(lines))
    return advice + ("\nInclude this latest job board verbatim:\n" + board if isinstance(board, str) else "")


def stop(payload: Mapping[str, Any], repository: Path | None) -> dict[str, str] | None:
    """Ask for one retry when a pending notice is absent or flow stalls after Codex."""
    try:
        session_id = payload.get("session_id")
        if repository is None or not isinstance(session_id, str) or not session_id:
            return None
        state_path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(state_path, session_id)
        notice = state["notice_pending"]
        receipt = state["receipt_pending"]
        dispatch = state["dispatch_pending"]
        version_pending = state["version_pending"]
        leader_pending = state["leader_pending"]
        fallback_pending = state["fallback_pending"]
        records = _read_transcript(payload.get("transcript_path"))
        flow_message = next((text for record in reversed(records)
                             if _record_type(record) == "assistant"
                             if (text := _assistant_text(record))), "")
        if "last_assistant_message" in payload:
            last = payload["last_assistant_message"]
            message = last if isinstance(last, str) else ""
        else:
            message = next((text for record in reversed(records)
                            if _record_type(record) == "assistant"
                            if (text := _assistant_text(record))), "")
        latest_user = max((i for i, record in enumerate(records) if _genuine_user(record)), default=-1)
        turn_message = "\n".join(
            text for record in records[latest_user + 1:]
            if _record_type(record) == "assistant"
            if (text := _assistant_text(record))
        )
        if message and message not in turn_message:
            turn_message = f"{turn_message}\n{message}"
        changed = False
        if notice is not None and any(line.startswith("> ⚓") for line in turn_message.splitlines()):
            state["notice_pending"] = None
            changed = True
        clock = datetime.now(timezone.utc)
        if receipt is not None and _receipt_satisfied(receipt, turn_message, clock):
            state["receipt_pending"] = None
            changed = True
        if dispatch is not None and all(line in turn_message for line in dispatch["lines"]):
            if dispatch.get("board") is None or dispatch["board"] in turn_message:
                if isinstance(dispatch.get("board"), str) and dispatch["board"] in turn_message:
                    jobs_directory = state_root.default_state_root() / "codex_exec_mcp" / "jobs"
                    for job_id in dispatch.get("board_job_ids", []):
                        if not isinstance(job_id, str) or not re.fullmatch(r"[0-9a-f]{64}", job_id):
                            continue
                        job_path = jobs_directory / f"{job_id}.json"
                        try:
                            job = json.loads(job_path.read_text(encoding="utf-8"))
                            if not isinstance(job, dict) or job.get("jobId") != job_id:
                                continue
                            lock = nullcontext()
                            if isinstance(job.get("repoRoot"), str):
                                repo_root = Path(job["repoRoot"]).resolve()
                                lock_key = hashlib.sha256(os.fspath(repo_root).encode("utf-8")).hexdigest()
                                lock_path = jobs_directory.parent / "apply" / (lock_key + ".lock")
                                lock = ledger._lease_lock(lock_path)
                            with lock:
                                job = json.loads(job_path.read_text(encoding="utf-8"))
                                if (not isinstance(job, dict) or job.get("jobId") != job_id
                                        or job.get("state") in {"QUEUED", "RUNNING"}):
                                    continue
                                count = job.get("boardDisplayCount", 0)
                                if type(count) is not int or count < 0:
                                    count = 0
                                job["boardDisplayCount"] = count + 1
                                _atomic_write(job_path, json.dumps(job, sort_keys=True).encode("utf-8"))
                        except (OSError, ValueError, TypeError):
                            continue
                state["dispatch_pending"] = None
                changed = True
        if version_pending is not None and "⚓" in turn_message and version_pending["version"] in turn_message:
            state["version_pending"] = None
            changed = True
            record_announced_plugin_version(version_pending["version"])
        if leader_pending is not None and any(
                "⚓" in line and leader_pending["model"] in line for line in turn_message.splitlines()):
            state["leader_pending"] = None
            changed = True
            record_announced_mechanical_leader(leader_pending["model"])
        anchored_lines = re.findall(r"^\*\*⚓ .+\*\*$", turn_message, re.MULTILINE)
        if (fallback_pending is not None and len(anchored_lines) == 1
                and fallback_pending["model"] in anchored_lines[0]):
            active_coder_state.update_fallback_question(
                fallback_pending["project"], session_id, shown=True
            )
            state["fallback_pending"] = None
            changed = True
        notice = state["notice_pending"]
        receipt = state["receipt_pending"]
        dispatch = state["dispatch_pending"]
        version_pending = state["version_pending"]
        leader_pending = state["leader_pending"]
        fallback_pending = state["fallback_pending"]
        if payload.get("stop_hook_active") is True:
            if changed:
                _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
            return None
        reasons = []
        if notice is not None and not notice.get("blocked"):
            notice["blocked"] = True
            reasons.append("End your reply with a line `---` followed by this exact four-line blockquote, translated into the user's language while keeping the markdown exactly:\n" + _blockquote(notice))
        if receipt is not None and not receipt.get("blocked"):
            receipt["blocked"] = True
            reasons.append(_receipt_advice(receipt))
        if dispatch is not None and not dispatch.get("blocked"):
            advice = _dispatch_advice(dispatch)
            if advice:
                dispatch["blocked"] = True
                reasons.append(advice)
        if version_pending is not None and not version_pending.get("blocked"):
            version_pending["blocked"] = True
            reasons.append(
                f"Include one visible line containing both `⚓` and `{version_pending['version']}` to announce the Purser update."
            )
        if leader_pending is not None and not leader_pending.get("blocked"):
            leader_pending["blocked"] = True
            reasons.append(
                f"Include one visible line containing both `⚓` and `{leader_pending['model']}` to announce the new free mechanical model leader."
            )
        if (fallback_pending is not None
                and (fallback_pending.get("notice") or not fallback_pending.get("blocked"))):
            fallback_pending["blocked"] = True
            if fallback_pending.get("notice"):
                reasons.append(
                    "On your next reply, render exactly one bold line in the user's language. "
                    "Translate the line below while keeping ⚓ and the model name; do not add another notice line.\n\n"
                    + fallback_pending["line"]
                )
            else:
                reasons.append(
                    "On your next reply, render exactly one bold question line in the user's language. "
                    "Translate the line below while keeping ⚓ and the model name; include no other question line.\n\n"
                    + fallback_pending["line"]
                )

        if not reasons and flow_message.rstrip().endswith(("?", "؟", "？")):
            latest_user = max((i for i, record in enumerate(records) if _genuine_user(record)), default=-1)
            if latest_user >= 0 and state["flow_blocked_user"] != latest_user:
                uses: dict[str, str] = {}
                successful_result = False
                for record in records[latest_user + 1:]:
                    for block in _content(record):
                        if not isinstance(block, Mapping):
                            continue
                        if block.get("type") == "tool_use" and isinstance(block.get("id"), str):
                            uses[block["id"]] = str(block.get("name", ""))
                        elif block.get("type") == "tool_result" and isinstance(block.get("tool_use_id"), str):
                            name = uses.pop(block["tool_use_id"], "")
                            if name.endswith("__codex_result") and not _result_error(block):
                                successful_result = True
                if successful_result:
                    state["flow_blocked_user"] = latest_user
                    reasons.append("Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question.")
        if changed or reasons:
            _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        if version_pending is not None and version_pending.get("blocked") and reasons:
            record_announced_plugin_version(version_pending["version"])
        return {"decision": "block", "reason": "\n\n".join(reasons)} if reasons else None
    except BaseException:
        return None


def clear_pending(payload: Mapping[str, Any], repository: Path | None) -> None:
    """Discard a notice after context is compacted or cleared."""
    try:
        session_id = payload.get("session_id")
        if repository is None or payload.get("source") not in {"compact", "clear"} or not isinstance(session_id, str) or not session_id:
            return
        state_path = _state_directory(repository) / "compaction_advisor.json"
        state = _read_state(state_path, session_id)
        if state["notice_pending"] is not None:
            state["notice_pending"] = None
            _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    except BaseException:
        pass


def advise(payload: Mapping[str, Any], repository: Path | None = None, *, now: datetime | None = None) -> str | None:
    """Return UserPromptSubmit additionalContext or None, without model calls."""
    settings = _settings()
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
    try:
        active_coder_state.update_fallback_question(repository, session_id, answerable=True)
    except BaseException:
        pass
    if not settings["enabled"]:
        return None
    if context < settings["threshold_tokens"]:
        state["reminder_messages"] = 0
        state["reminder_wasted_tokens"] = 0
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    receipt = state["receipt_pending"]
    receipt_advice = _receipt_advice(receipt) if receipt is not None else None
    if receipt is not None:
        state["receipt_pending"] = None
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    if state["notice_pending"] is not None:
        pending = state["notice_pending"]
        return "\n".join(part for part in (_notice_advice(pending, pending.get("triggers", [])), receipt_advice) if part)
    task_completed = False
    task_completed = bool(current_task_id and current_task_id in completed_current_tasks and current_task_id != state["last_task_id"])
    if task_completed:
        triggers.append("task_end")
    state["last_task_id"] = latest_task_id or state["last_task_id"]

    if context < settings["threshold_tokens"]:
        state["last_suggested_context"] = 0
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        return receipt_advice
    last_suggested = state["last_suggested_context"]
    if last_suggested == 0 and not triggers:
        triggers.append("threshold")
    reminder_due = False
    if last_suggested:
        state["reminder_messages"] += 1
        state["reminder_wasted_tokens"] += max(0, context - POST_COMPACT_CONTEXT_TOKENS)
        reminder_due = state["reminder_messages"] >= 15
        if reminder_due:
            state["reminder_messages"] = 0
    if not triggers:
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        if reminder_due:
            pct = max(0, round(100 * (1 - POST_COMPACT_CONTEXT_TOKENS / context)))
            reminder = {"context": context, "percentage": pct, "handoff": str(handoff_path(repository))}
            return _reminder_advice(reminder, state["reminder_wasted_tokens"], receipt_advice)
        return receipt_advice
    if last_suggested and context < last_suggested + settings["regrow_tokens"]:
        _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
        if reminder_due:
            pct = max(0, round(100 * (1 - POST_COMPACT_CONTEXT_TOKENS / context)))
            reminder = {"context": context, "percentage": pct, "handoff": str(handoff_path(repository))}
            return _reminder_advice(reminder, state["reminder_wasted_tokens"], receipt_advice)
        return receipt_advice

    directory.mkdir(parents=True, exist_ok=True)
    handoff = handoff_path(repository)
    _replace_state_block(handoff, _mechanical_state(repository, context, triggers, calls), _recent_turns(records))
    state["last_suggested_context"] = context
    state["reminder_messages"] = 0
    pct = max(0, round(100 * (1 - POST_COMPACT_CONTEXT_TOKENS / context)))
    state["notice_pending"] = {"context": context, "percentage": pct, "handoff": str(handoff), "triggers": triggers, "blocked": False}
    _atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    return "\n".join(part for part in (_notice_advice(state["notice_pending"], triggers), receipt_advice) if part)


__all__ = ["advise"]
