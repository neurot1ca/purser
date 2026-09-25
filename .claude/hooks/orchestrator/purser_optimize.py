"""Read-only token-use report for recent Claude Code transcripts."""

from __future__ import annotations

import argparse
from collections import Counter, defaultdict
from datetime import datetime
import json
import re
import statistics
import sys
from pathlib import Path
from typing import Any

sys.dont_write_bytecode = True
import compaction_advisor


WEIGHTS = {"input_tokens": 1.0, "cache_creation_input_tokens": 1.25,
           "cache_read_input_tokens": 0.1, "output_tokens": 5.0}
COMMAND_TAG = re.compile(r"<command-name>(.*?)</command-name>", re.DOTALL)
SLASH_COMMAND = re.compile(r"(?<![\w])/(?:[\w.-]+:)?[\w.-]+")
MCP_TOOL = re.compile(r"^mcp__(.+?)__(.+)$")


def _number(value: Any) -> int:
    try:
        return max(0, int(value or 0))
    except (TypeError, ValueError, OverflowError):
        return 0


def _weight(usage: Any) -> float:
    if not isinstance(usage, dict):
        return 0.0
    return sum(_number(usage.get(key)) * factor for key, factor in WEIGHTS.items())


def _text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "".join(_text(part) for part in value)
    if isinstance(value, dict):
        if isinstance(value.get("text"), str):
            return value["text"]
        return _text(value.get("content"))
    return ""


def _rendered(record: dict[str, Any]) -> str:
    rendered = record.get("rendered")
    if isinstance(rendered, list):
        return "\n".join(_text(item) for item in rendered)
    return _text(rendered)


def _skill_rows(attachment: dict[str, Any]) -> list[tuple[str, int]]:
    content = attachment.get("content")
    if not isinstance(content, str):
        return []
    rows = []
    for line in content.splitlines():
        match = re.match(r"\s*-\s*([\w.-]+(?::[\w.-]+)*):", line)
        if match:
            rows.append((match.group(1).strip(), len(line)))
    names = attachment.get("names")
    if not rows and isinstance(names, list):
        rows = [(name, len(str(name))) for name in names if isinstance(name, str)]
    return rows


def _server_name(tool_name: str) -> str | None:
    match = MCP_TOOL.match(tool_name)
    return match.group(1) if match else None


def _same_server(left: str, right: str) -> bool:
    clean = lambda value: re.sub(r"[^a-z0-9]", "", value.lower())
    return clean(left) == clean(right) or clean(left).endswith(clean(right)) or clean(right).endswith(clean(left))


def _server_key(servers: dict[str, float], name: str) -> str:
    return next((known for known in servers if _same_server(known, name)), name)


def _command_text(message: Any) -> str:
    if isinstance(message, dict):
        content = message.get("content")
        if isinstance(content, list):
            return "".join(block.get("text", "") for block in content
                           if isinstance(block, dict) and block.get("type") == "text"
                           and isinstance(block.get("text"), str))
        return _text(content)
    return _text(message)


def _normalized_command(tool: dict[str, Any]) -> str:
    value = tool.get("input", {})
    command = value.get("command", "") if isinstance(value, dict) else ""
    command = str(command).strip()
    navigation = re.compile(
        r"^(?:cd|pushd|Set-Location)\s+(?:\"[^\"]*\"|'[^']*'|[^\s;&|]+)\s*(?:&&|;)\s*",
        re.IGNORECASE,
    )
    while match := navigation.match(command):
        command = command[match.end():].lstrip()
    return command


_READ_ONLY_COMMANDS = {
    "grep", "rg", "cat", "head", "tail", "ls", "find", "wc",
    "get-content", "select-string", "get-childitem",
}
_READ_ONLY_GIT_COMMANDS = {"status", "diff", "log", "show"}
_INLINE_INTERPRETER_COMMANDS = {
    "python": {"-", "-c"}, "python3": {"-", "-c"}, "py": {"-", "-c"},
    "node": {"-e"}, "powershell": {"-command"}, "pwsh": {"-c"},
}


def _eligible_signature(tools: list[dict[str, Any]]) -> bool:
    if not tools or any(tool.get("name") not in ("Bash", "PowerShell") for tool in tools):
        return False
    for tool in tools:
        command = _normalized_command(tool)
        words = re.findall(r"[^\s|;&]+", command)
        if not words:
            return False
        first = words[0].lower()
        second = words[1].lower() if len(words) > 1 else ""
        if second in _INLINE_INTERPRETER_COMMANDS.get(first, set()):
            return False
        if re.match(r"^(?:python(?:3)?|py)\s+(?:-\s+)?<<", command, re.IGNORECASE):
            return False
        if first in _READ_ONLY_COMMANDS:
            return False
        if first == "sed" and second == "-n":
            return False
        if first == "git" and second in _READ_ONLY_GIT_COMMANDS:
            return False
    return True


def _signature(tools: list[dict[str, Any]]) -> tuple[str, ...]:
    names = []
    for tool in tools:
        name = str(tool.get("name", "unknown"))
        if name in ("Bash", "PowerShell"):
            command = _normalized_command(tool)
            words = re.findall(r"[^\s|;&]+", command)[:2]
            if words:
                name += " " + " ".join(words)
        names.append(name)
    return tuple(sorted(set(names)))


def _genuine_typing(record: dict[str, Any]) -> bool:
    message = _command_text(record.get("message")).strip()
    if (not message or "<command-name>" in message or "<local-command-" in message
            or "<task-notification>" in message):
        return False
    origin = record.get("origin")
    if isinstance(origin, dict) and origin.get("kind") == "human":
        return True
    if record.get("turnOrigin") == "human":
        return True
    if record.get("promptSource") == "typed":
        return True
    return record.get("userType") == "external" and record.get("promptSource") not in ("sdk", "system")


def _session(path: Path) -> dict[str, Any]:
    requests: list[dict[str, Any]] = []
    request_ids: dict[str, int] = {}
    events: list[dict[str, Any]] = []
    tools_by_id: dict[str, dict[str, Any]] = {}
    entrypoints: Counter[str] = Counter()
    typed = False
    sidechain = path.parent.name.lower() == "subagents"
    cwd = ""
    latest_timestamp = ""
    first_prompts: dict[str, str] = {}
    skills: dict[str, float] = defaultdict(float)
    servers: dict[str, float] = defaultdict(float)
    loaded_names: set[str] = set()
    duplicate_sources: dict[str, set[str]] = defaultdict(set)
    seen_listings: set[str] = set()
    seen_mcp_deltas: set[str] = set()
    seen_deferred_names: set[str] = set()
    seen_instruction_servers: set[str] = set()
    active_skill_names: set[str] = set()
    active_instruction_servers: set[str] = set()
    active_deferred_names: set[str] = set()

    with path.open("r", encoding="utf-8", errors="replace") as stream:
        for line in stream:
            try:
                record = json.loads(line)
            except (ValueError, TypeError):
                continue
            if not isinstance(record, dict):
                continue
            if not cwd and isinstance(record.get("cwd"), str):
                cwd = record["cwd"]
            sidechain = sidechain or bool(record.get("isSidechain")) or bool(record.get("agentId"))
            if isinstance(record.get("timestamp"), str):
                latest_timestamp = record["timestamp"]
            entrypoint = str(record.get("entrypoint") or "unknown")
            message = record.get("message")
            role = message.get("role") if isinstance(message, dict) else None
            if entrypoint != "unknown":
                entrypoints[entrypoint] += 1
            if record.get("type") == "user" or role == "user":
                msg = _command_text(message)
                if (not first_prompts.get(entrypoint) and msg.strip()
                        and "<command-name>" not in msg and "<local-command-" not in msg
                        and "<task-notification>" not in msg):
                    clean = " ".join(msg.split())
                    first_prompts[entrypoint] = clean[:80]
                typed = typed or _genuine_typing(record)
                command_names = COMMAND_TAG.findall(msg)
                command_names.extend(SLASH_COMMAND.findall(msg))
                if msg.strip() and ("<command-name>" in msg or "<local-command-" not in msg
                                    or command_names):
                    events.append({"kind": "user", "entrypoint": entrypoint})
                if command_names:
                    events.append({"kind": "command", "names": command_names})

            if role == "assistant" and isinstance(message, dict):
                key = str(message.get("id") or record.get("requestId") or f"line:{len(requests)}")
                if key not in request_ids:
                    request_ids[key] = len(requests)
                    requests.append({"key": key, "weight": _weight(message.get("usage")),
                                     "tools": [], "text_chars": 0, "entrypoint": entrypoint})
                    events.append({"kind": "request", "index": request_ids[key]})
                req = requests[request_ids[key]]
                content = message.get("content")
                if isinstance(content, list):
                    for block in content:
                        if not isinstance(block, dict):
                            continue
                        if block.get("type") == "text":
                            req["text_chars"] += len(_text(block.get("text")))
                        elif block.get("type") == "tool_use":
                            tool = {"name": str(block.get("name", "unknown")), "input": block.get("input", {})}
                            tool_id = block.get("id")
                            if tool_id:
                                tools_by_id[str(tool_id)] = tool
                            if not any(t.get("_id") == tool_id and tool_id for t in req["tools"]):
                                tool["_id"] = tool_id
                                req["tools"].append(tool)
                if message.get("stop_reason") == "end_turn":
                    req["final"] = True

            if record.get("type") != "attachment" or not isinstance(record.get("attachment"), dict):
                continue
            attachment = record["attachment"]
            kind = attachment.get("type")
            if kind == "skill_listing":
                rows = _skill_rows(attachment)
                names = attachment.get("names")
                listing_names = ({name for name in names if isinstance(name, str)}
                                 if isinstance(names, list)
                                 else {name for name, _ in rows})
                if attachment.get("isInitial", True) is False:
                    active_skill_names.update(listing_names)
                else:
                    active_skill_names = listing_names
                    duplicate_sources.clear()
                listing_content = attachment.get("content", "")
                listing_key = str(listing_content)
                if listing_key in seen_listings:
                    continue
                seen_listings.add(listing_key)
                listing_size = max(1, sum(chars for _, chars in rows))
                for name, chars in rows:
                    loaded_names.add(name)
                    if name not in skills:
                        skills[name] = chars / listing_size * len(str(listing_content)) / 4.0
                    canonical = name.rsplit(":", 1)[-1]
                    source = name.rsplit(":", 1)[0] if ":" in name else "user"
                    duplicate_sources[canonical].add(source)
            elif kind == "mcp_instructions_delta":
                added = attachment.get("addedNames", [])
                blocks = attachment.get("addedBlocks", [])
                removed = attachment.get("removedNames", [])
                if isinstance(removed, list):
                    active_instruction_servers = {
                        known for known in active_instruction_servers
                        if not any(isinstance(name, str) and _same_server(known, name)
                                   for name in removed)
                    }
                if isinstance(added, list):
                    active_instruction_servers.update(name for name in added if isinstance(name, str))
                mcp_key = json.dumps([added, blocks], sort_keys=True, default=str)
                if mcp_key in seen_mcp_deltas:
                    continue
                seen_mcp_deltas.add(mcp_key)
                if isinstance(added, list):
                    for server in added:
                        if isinstance(server, str):
                            loaded_names.add(server)
                            server = _server_key(servers, server)
                            if server in seen_instruction_servers:
                                continue
                            seen_instruction_servers.add(server)
                            block_text = ""
                            if isinstance(blocks, list):
                                block_text = "\n".join(str(b) for b in blocks if isinstance(b, str) and _same_server(server, _heading(b)))
                            servers[server] += (len(block_text) if block_text else len(_rendered(record)) / max(1, len(added))) / 4.0
            elif kind == "deferred_tools_delta":
                names = attachment.get("addedNames", [])
                removed = attachment.get("removedNames", [])
                if isinstance(removed, list):
                    active_deferred_names.difference_update(name for name in removed if isinstance(name, str))
                if isinstance(names, list):
                    active_deferred_names.update(name for name in names if isinstance(name, str))
                if isinstance(names, list):
                    for name in names:
                        if not isinstance(name, str):
                            continue
                        server = _server_name(name)
                        if server:
                            loaded_names.add(server)
                            server = _server_key(servers, server)
                            if name not in seen_deferred_names:
                                seen_deferred_names.add(name)
                                servers[server] += len(name) / 4.0
            elif kind == "deferred_tools_record":
                entries = attachment.get("entries", [])
                if isinstance(entries, list):
                    for item in entries:
                        if isinstance(item, dict) and isinstance(item.get("name"), str):
                            server = _server_name(item["name"])
                            if server:
                                loaded_names.add(server)
                                server = _server_key(servers, server)
                                if item["name"] not in seen_deferred_names:
                                    seen_deferred_names.add(item["name"])
                                    servers[server] += len(item["name"]) / 4.0

    # Associate tool calls and slash commands with discovered startup entries.
    tool_calls = [tool for req in requests for tool in req["tools"]]
    command_names = []
    for event in events:
        if event["kind"] == "command":
            command_names.extend(event["names"])
    usage_events: list[dict[str, str]] = []
    for tool in tool_calls:
        name = tool["name"]
        if name == "Skill":
            value = tool.get("input", {})
            skill = value.get("skill") if isinstance(value, dict) else None
            if isinstance(skill, str):
                usage_events.append({"kind": "skill", "name": skill})
        server = _server_name(name)
        if server:
            usage_events.append({"kind": "mcp", "name": server})
    for command in command_names:
        usage_events.append({"kind": "command", "name": command.removeprefix("/")})

    requests_by_turn: list[list[int]] = []
    turn: list[int] = []
    for event in events:
        if event["kind"] == "user":
            if turn:
                requests_by_turn.append(turn)
                turn = []
        elif event["kind"] == "request":
            turn.append(event["index"])
    if turn:
        requests_by_turn.append(turn)
    active_servers = {name for name in active_instruction_servers}
    active_servers.update(server for name in active_deferred_names
                          if (server := _server_name(name)) is not None)
    servers = {name: cost for name, cost in servers.items()
               if any(_same_server(name, active) for active in active_servers)}
    skills = {name: cost for name, cost in skills.items() if name in active_skill_names}
    return {"path": path, "requests": requests, "request_turns": requests_by_turn,
            "entrypoint": next(iter(entrypoints), "unknown"), "entrypoints": entrypoints,
            "typed": typed, "sidechain": sidechain, "cwd": cwd,
            "mtime": path.stat().st_mtime, "timestamp": latest_timestamp,
            "first_prompts": first_prompts, "skills": dict(skills),
            "servers": dict(servers), "loaded_names": loaded_names, "usage": usage_events,
            "duplicates": {k: sorted(v) for k, v in duplicate_sources.items() if len(v) > 1}}


def _heading(block: str) -> str:
    match = re.search(r"(?m)^##\s+([^\n]+)", block)
    return match.group(1).strip() if match else ""


def _format_number(value: float) -> str:
    if value >= 1_000_000:
        return f"{value / 1_000_000:.1f}M"
    if value >= 1000:
        return f"{value / 1000:.1f}K"
    return str(round(value))


def _compact_tokens(value: float) -> str:
    return _format_number(value).replace(".0K", "K").replace(".0M", "M")


def analyze(projects_root: Path, days: int, home: Path | None = None,
            project_root: Path | None = None) -> dict[str, Any]:
    now = datetime.now().timestamp()
    cutoff = now - days * 86400
    sessions = []
    for path in projects_root.rglob("*.jsonl"):
        try:
            if path.stat().st_mtime >= cutoff:
                sessions.append(_session(path))
        except OSError:
            continue
    requests = [r for s in sessions for r in s["requests"]]
    total_weight = sum(r["weight"] for r in requests)
    all_entry = defaultdict(float)
    prompt_cost = defaultdict(lambda: defaultdict(float))
    interactive_entries = {"claude-desktop", "cli"}
    interactive_entries.update(entry for s in sessions if s["typed"] for entry in s["entrypoints"])
    interactive_sessions = [s for s in sessions if not s["sidechain"] and
                            (s["typed"] or any(ep in {"claude-desktop", "cli"} for ep in s["entrypoints"]))]
    current = max(interactive_sessions, key=lambda s: s["mtime"], default=None)
    if home is None:
        home = Path.home()
    if project_root is None and current and current["cwd"]:
        candidate = Path(current["cwd"])
        if candidate.is_dir():
            project_root = candidate
    request_count = len(current["requests"]) if current else 0

    usage_events = [event for session in sessions for event in session["usage"]]

    def skill_matches(candidate_name: str, target_name: str) -> bool:
        return (candidate_name == target_name or candidate_name.endswith(":" + target_name)
                or target_name.endswith(":" + candidate_name))

    def skill_uses(name: str) -> int:
        return sum(1 for event in usage_events if event["kind"] in ("skill", "command")
                   and skill_matches(event["name"], name))

    def local_skill_sources(name: str) -> set[str]:
        sources = set()
        if (home / ".claude" / "skills" / name).is_dir():
            sources.add("user")
        if project_root and (project_root / ".claude" / "skills" / name).is_dir():
            sources.add("project")
        return sources

    for s in sessions:
        ep = s["entrypoint"]
        weight = sum(r["weight"] for r in s["requests"])
        all_entry[ep] += weight
        if ep not in interactive_entries:
            prompt = next(iter(s["first_prompts"].values()), "(no first prompt recorded)")
            prompt_cost[ep][prompt] += weight

    startup_items = []
    duplicate_sources: dict[str, set[str]] = defaultdict(set)
    startup_skills = current["skills"] if current else {}
    startup_servers = current["servers"] if current else {}
    for name, cost in startup_skills.items():
        canonical = name.rsplit(":", 1)[-1]
        sources = local_skill_sources(canonical)
        if ":" in name:
            sources.add(name.split(":", 1)[0])
        duplicate_sources[canonical].update(sources)
        built_in = ":" not in name and not sources
        uses = skill_uses(name)
        startup_items.append({"name": "skill:" + name, "kind": "skill",
                              "tokens_per_request": cost, "requests_loaded": request_count,
                              "uses": uses, "built_in": built_in,
                              "total_tokens": cost * request_count,
                              "flagged": not built_in and uses == 0 and cost >= 100})
    for name, cost in startup_servers.items():
        uses = sum(1 for event in usage_events if event["kind"] == "mcp" and
                   _same_server(name, event["name"]))
        startup_items.append({"name": "mcp:" + name, "kind": "MCP",
                              "tokens_per_request": cost, "requests_loaded": request_count,
                              "uses": uses, "built_in": False,
                              "total_tokens": cost * request_count,
                              "flagged": uses == 0 and cost >= 100})
    plugins = sorted({name.split(":", 1)[0] for name in startup_skills if ":" in name})
    for plugin in plugins:
        plugin_skills = [(name, cost) for name, cost in startup_skills.items()
                         if name.startswith(plugin + ":")]
        cost = sum(value for _, value in plugin_skills)
        uses = sum(skill_uses(name) for name, _ in plugin_skills)
        uses += sum(1 for event in usage_events if event["kind"] == "mcp" and
                    _same_server(plugin, event["name"]))
        startup_items.append({"name": "plugin:" + plugin, "kind": "plugin",
                              "tokens_per_request": cost, "requests_loaded": request_count,
                              "uses": uses, "built_in": False,
                              "total_tokens": cost * request_count,
                              "flagged": uses == 0 and cost >= 100})
    startup_items.sort(key=lambda x: (not x["flagged"], -x["total_tokens"], x["name"]))
    duplicates = [{"name": name, "sources": sorted(sources)}
                  for name, sources in sorted(duplicate_sources.items()) if len(sources) >= 2]

    replies = []
    for s in sessions:
        if s not in interactive_sessions:
            continue
        for turn in s["request_turns"]:
            final = next((s["requests"][i] for i in reversed(turn) if s["requests"][i].get("final")), None)
            if final is None and turn:
                final = s["requests"][turn[-1]]
            if final:
                replies.append(final["text_chars"])
    median_reply = statistics.median(replies) if replies else 0
    claude_md = home / ".claude" / "CLAUDE.md"
    try:
        has_reply_rule = bool(re.search(r"(?m)^# Replies\s*$", claude_md.read_text(encoding="utf-8")))
    except (OSError, UnicodeError):
        has_reply_rule = False

    shown = _select_sequences(_sequence_candidates(sessions))

    settings = compaction_advisor._settings()
    return {"startup": startup_items, "startup_note": "System prompt and eager tool schemas are not recorded in transcripts and cannot be attributed.",
            "hidden": [{"entrypoint": ep, "share": value / total_weight if total_weight else 0,
                        "weighted_tokens": value,
                        "sessions": sum(1 for session in sessions if session["entrypoint"] == ep),
                        "prompts": [{"signature": prompt, "share": cost / total_weight if total_weight else 0}
                                    for prompt, cost in sorted(prompts.items(), key=lambda x: -x[1])[:3]]}
                       for ep, value in sorted(all_entry.items(), key=lambda x: -x[1])
                       if ep not in interactive_entries and value > 0
                       for prompts in [prompt_cost[ep]]],
            "duplicate_skills": duplicates, "median_reply_chars": median_reply,
            "reply_rule_present": has_reply_rule, "reply_count": len(replies), "sequences": shown,
            "compaction": {"enabled": settings["enabled"], "threshold_tokens": settings["threshold_tokens"]},
            "sessions": len(sessions), "requests": len(requests), "weighted_tokens": total_weight,
            "days": days}


def _contains_sequence(longer: tuple[Any, ...], shorter: tuple[Any, ...]) -> bool:
    return any(longer[i:i + len(shorter)] == shorter for i in range(len(longer) - len(shorter) + 1))


def _select_sequences(candidates: list[dict[str, Any]], limit: int = 5) -> list[dict[str, Any]]:
    """Keep the higher-saving member when one repeated sequence contains another."""
    shown = []
    for candidate in candidates:
        if any((_contains_sequence(prior["sequence"], candidate["sequence"])
                or _contains_sequence(candidate["sequence"], prior["sequence"]))
               for prior in shown):
            continue
        shown.append(candidate)
        if len(shown) >= limit:
            break
    return shown


def _sequence_candidates(sessions: list[dict[str, Any]]) -> list[dict[str, Any]]:
    sequences = defaultdict(lambda: {"occurrences": 0, "sessions": set(), "costs": []})
    for session in sessions:
        for turn in session["request_turns"]:
            runs = []
            run = []
            for i in turn:
                tools = session["requests"][i]["tools"]
                if not tools:
                    continue
                if not _eligible_signature(tools):
                    if run:
                        runs.append(run)
                        run = []
                    continue
                run.append((i, _signature(tools)))
            if run:
                runs.append(run)
            for run in runs:
                for n in range(3, 7):
                    next_start_by_signature = {}
                    for start in range(max(0, len(run) - n + 1)):
                        chunk = run[start:start + n]
                        signature = tuple(item[1] for item in chunk)
                        if len(set(signature)) < 2:
                            continue
                        if start < next_start_by_signature.get(signature, 0):
                            continue
                        row = sequences[signature]
                        row["occurrences"] += 1
                        row["sessions"].add(str(session["path"]))
                        row["costs"].extend(session["requests"][item[0]]["weight"] for item in chunk)
                        next_start_by_signature[signature] = start + n
    candidates = []
    for signature, row in sequences.items():
        if len(row["sessions"]) < 3:
            continue
        n = len(signature)
        median_cost = statistics.median(row["costs"]) if row["costs"] else 0
        candidates.append({"sequence": signature, "length": n,
                           "occurrences": row["occurrences"], "sessions": len(row["sessions"]),
                           "weighted_saving": (n - 1) * row["occurrences"] * median_cost})
    return sorted(candidates, key=lambda item: (-item["weighted_saving"], -item["length"], item["sequence"]))


def _display_name(value: str) -> str:
    """Make configured names readable without exposing opaque connector IDs."""
    if re.fullmatch(r"[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}", value):
        return "an unnamed claude.ai connector"
    return re.sub(r"[-_]+", " ", value).strip()


def _hidden_prompt_line(row: dict[str, Any]) -> str:
    prompt = row.get("prompts", [{}])[0].get("signature") if row.get("prompts") else None
    excerpt = _prompt_excerpt(prompt) if prompt else "an unrecorded prompt"
    return f"They start with “{excerpt}”. Look at the plugin that launches them."


def _prompt_excerpt(prompt: str, limit: int = 60) -> str:
    prompt = " ".join(prompt.split())
    sentence = re.search(r"[.!?](?=\s|$)", prompt)
    end = min(sentence.end(), limit) if sentence else min(len(prompt), limit)
    excerpt = prompt[:end].rstrip()
    if end < len(prompt):
        excerpt += "…"
    return excerpt


def _startup_candidates(report: dict[str, Any]) -> list[dict[str, Any]]:
    rows = [item for item in report.get("startup", []) if item.get("flagged")]
    doc_skills = []
    doc_plugins = set()
    for item in rows:
        if item["kind"] != "skill":
            continue
        parts = item["name"].removeprefix("skill:").split(":")
        if (parts[-1].lower() in {"pptx", "xlsx", "docx", "pdf"}
                and any(part.lower() in {"anthropic", "document-skills", "document-skills-plugin"}
                        for part in parts)):
            doc_skills.append(item)
            doc_plugins.add(parts[0])
    plugins = {item["name"][7:] for item in rows
               if item["kind"] == "plugin" and item["name"][7:] not in doc_plugins}
    candidates = []
    for item in rows:
        if item["kind"] == "plugin" and item["name"][7:] in doc_plugins:
            continue
        name = item["name"].split(":", 1)[-1]
        if item["kind"] == "skill":
            if item in doc_skills:
                continue
            if any(name.startswith(plugin + ":") for plugin in plugins):
                continue
        candidates.append(item)
    if doc_skills:
        candidates.append({"kind": "document_skills", "items": doc_skills,
                           "total_tokens": sum(i["total_tokens"] for i in doc_skills),
                           "tokens_per_request": sum(i["tokens_per_request"] for i in doc_skills)})
    return candidates


def render(report: dict[str, Any]) -> str:
    days = report.get("days", 30)
    entries = []
    for item in _startup_candidates(report):
        kind = item["kind"]
        per_500 = item["tokens_per_request"] * 500
        cost = f"~{_compact_tokens(per_500)} tokens per 500 messages"
        if kind == "MCP":
            name = item["name"].removeprefix("mcp:")
            if re.fullmatch(r"[0-9a-fA-F]{8}-[0-9a-fA-F-]{27,}", name):
                sentence = f"An unnamed claude.ai connector costs {cost} and had 0 uses in {item['requests_loaded']} requests."
            else:
                sentence = f"{_display_name(name)} connector costs {cost} and had 0 uses in {item['requests_loaded']} requests."
            entries.append((per_500, "🔌", sentence, "Remove the connector."))
        elif kind == "plugin":
            name = item["name"].removeprefix("plugin:")
            entries.append((per_500, "🧩", f"{_display_name(name)} plugin costs {cost} and had 0 uses in {item['requests_loaded']} requests.", "Disable the plugin."))
        elif kind == "document_skills":
            names = ", ".join(i["name"].split(":")[-1] for i in item["items"])
            entries.append((per_500, "📚", f"Unused document skills ({names}) cost {cost}.", "Remove the unused skills."))
        else:
            name = item["name"].removeprefix("skill:").split(":")[-1]
            entries.append((per_500, "📚", f"Unused {_display_name(name)} skill costs {cost}.", "Remove the unused skills."))
    for item in report.get("sequences", []):
        seq = " → ".join(" + ".join(step) for step in item["sequence"])
        saving = f"~{_compact_tokens(item['weighted_saving'])} tokens over the last {days} days"
        entries.append((item["weighted_saving"], "🔁", f"Repeated Bash work ({seq}) could have saved {saving}.", "Turn this sequence into one script."))
    for row in report.get("hidden", []):
        source = _hidden_prompt_line(row)
        sentence = (f"{row.get('sessions', 0)} hidden sessions used "
                    f"~{_compact_tokens(row.get('weighted_tokens', 0))} tokens "
                    f"in {days} days.")
        entries.append((row.get("weighted_tokens", 0), "👀", sentence,
                        source))
    if report.get("median_reply_chars", 0) > 800 and not report.get("reply_rule_present", False):
        reply_rule = "# Replies\nLead with the answer.\nKeep replies short, at most 5 lines unless asked for detail.\nNo tables unless asked."
        entries.append((report["median_reply_chars"], "📝",
                        f"Your replies average ~{round(report['median_reply_chars'])} characters.",
                        f"Add a short-reply rule to ~/.claude/CLAUDE.md:\n{reply_rule}"))
    entries.sort(key=lambda entry: -entry[0])
    lines = ["**⚓ Purser optimize**", "", "Biggest token drains"]
    for index, (_, emoji, sentence, action) in enumerate(entries[:7], 1):
        lines.append(f"{index}. {emoji} **{sentence}** {action}")
    if not entries:
        lines.append("✨ No clear token waste found in your recent sessions.")
    lines.extend(["", "🧠 Purser learns as it works in this project: it measures how long its work really takes, sharpens its estimates, and points out where tokens and time can be saved."])
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    reconfigure = getattr(sys.stdout, "reconfigure", None)
    if callable(reconfigure):
        try:
            reconfigure(encoding="utf-8", errors="replace")
        except (OSError, ValueError):
            pass
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--projects-root", type=Path, default=Path.home() / ".claude" / "projects")
    parser.add_argument("--days", type=int, default=30)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)
    if args.days < 0:
        parser.error("--days must be non-negative")
    report = analyze(args.projects_root, args.days)
    print(json.dumps(report, ensure_ascii=False, default=lambda value: list(value) if isinstance(value, set) else str(value)) if args.json else render(report))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
