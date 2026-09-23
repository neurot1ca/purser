"""Move global manual Purser registrations to the Purser Claude Code plugin."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SCRIPT_TARGET = re.compile(
    r"(?i)(?:^|[\\/\s\"'])[^\"'\r\n]*?[\\/]orchestrator[\\/](hook\.py|context_io_hook\.py)(?=$|[\s\"'])"
)
MCP_TARGET = re.compile(r"(?i)(?:^|[\\/])orchestrator[\\/]codex_exec_mcp\.py(?=$|[\\/\s\"'])")
BACKUP_RE = re.compile(r"\.bak-purser-cutover-(\d{8}T\d{6}\.\d{6}Z)$")


class CutoverError(ValueError):
    """A settings file cannot safely be changed or restored."""


def _load(path: Path) -> tuple[dict[str, Any], bytes]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise CutoverError(f"cannot read {path}: {exc}") from exc
    try:
        value = json.loads(raw.decode("utf-8"))
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise CutoverError(f"{path} is not valid UTF-8 JSON") from exc
    if not isinstance(value, dict):
        raise CutoverError(f"{path} root must be a JSON object")
    return value, raw


def _render(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode("utf-8")


def _atomic_replace(path: Path, payload: bytes) -> None:
    temp_path: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", prefix=path.name + ".", suffix=".tmp", dir=path.parent, delete=False
        ) as handle:
            temp_path = Path(handle.name)
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        # Validate the exact payload before replacing the original.
        try:
            parsed = json.loads(payload.decode("utf-8"))
        except (UnicodeError, json.JSONDecodeError) as exc:
            raise CutoverError(f"refusing to write invalid JSON to {path}") from exc
        if not isinstance(parsed, dict):
            raise CutoverError(f"refusing to write non-object JSON to {path}")
        os.replace(temp_path, path)
    finally:
        if temp_path is not None and temp_path.exists():
            temp_path.unlink()


def _is_purser_command(command: Any) -> bool:
    return isinstance(command, str) and SCRIPT_TARGET.search(command) is not None


def _clean_settings(settings: dict[str, Any]) -> tuple[dict[str, Any], list[tuple[str, Any, str]]]:
    updated = dict(settings)
    hooks = updated.get("hooks")
    if not isinstance(hooks, dict):
        return updated, []

    removed: list[tuple[str, Any, str]] = []
    new_hooks = dict(hooks)
    for event, event_entries in hooks.items():
        if not isinstance(event_entries, list):
            continue
        kept_entries = []
        removed_from_event = False
        for entry in event_entries:
            if not isinstance(entry, dict) or not isinstance(entry.get("hooks"), list):
                kept_entries.append(entry)
                continue
            kept_commands = []
            removed_from_entry = False
            for command_entry in entry["hooks"]:
                command = command_entry.get("command") if isinstance(command_entry, dict) else None
                if _is_purser_command(command):
                    removed.append((event, entry.get("matcher"), command))
                    removed_from_entry = True
                    removed_from_event = True
                else:
                    kept_commands.append(command_entry)
            if kept_commands:
                revised = dict(entry)
                revised["hooks"] = kept_commands
                kept_entries.append(revised if len(kept_commands) != len(entry["hooks"]) else entry)
            elif not removed_from_entry:
                kept_entries.append(entry)
        if kept_entries:
            new_hooks[event] = kept_entries
        elif removed_from_event:
            new_hooks.pop(event, None)
    if new_hooks != hooks:
        updated["hooks"] = new_hooks
    return updated, removed

def _mcp_matches(entry: Any) -> bool:
    if not isinstance(entry, dict):
        return False
    values = [entry.get("command")]
    args = entry.get("args")
    if isinstance(args, list):
        values.extend(args)
    return any(isinstance(value, str) and MCP_TARGET.search(value) for value in values)


def _clean_mcp(config: dict[str, Any]) -> tuple[dict[str, Any], bool]:
    updated = dict(config)
    servers = updated.get("mcpServers")
    if not isinstance(servers, dict) or "codex-main" not in servers:
        return updated, False
    if not _mcp_matches(servers["codex-main"]):
        return updated, False
    new_servers = dict(servers)
    del new_servers["codex-main"]
    updated["mcpServers"] = new_servers
    return updated, True


def _backup_path(path: Path, stamp: str) -> Path:
    return path.with_name(path.name + ".bak-purser-cutover-" + stamp)


def _backup_pairs(settings_path: Path, claude_path: Path) -> list[tuple[str, Path, Path]]:
    names: dict[str, dict[str, Path]] = {}
    for label, path in (("settings", settings_path), ("claude", claude_path)):
        for backup in path.parent.glob(path.name + ".bak-purser-cutover-*"):
            match = BACKUP_RE.search(backup.name)
            if match:
                names.setdefault(match.group(1), {})[label] = backup
    return [
        (stamp, pair["settings"], pair["claude"])
        for stamp, pair in names.items()
        if "settings" in pair and "claude" in pair
    ]


def _display(entry: tuple[str, Any, str]) -> str:
    event, matcher, command = entry
    return f"event={event!r}, matcher={matcher!r}, command={command!r}"


def run(mode: str, home: Path, repo_root: Path) -> int:
    settings_path = home / ".claude" / "settings.json"
    claude_path = home / ".claude.json"
    marketplace_path = repo_root / ".claude-plugin" / "marketplace.json"
    if mode == "--rollback":
        pairs = _backup_pairs(settings_path, claude_path)
        if not pairs:
            print("ERROR: no complete Purser cutover backup pair found", file=sys.stderr)
            return 2
        _, settings_backup, claude_backup = max(pairs, key=lambda pair: pair[0])
        try:
            _atomic_replace(settings_path, settings_backup.read_bytes())
            _atomic_replace(claude_path, claude_backup.read_bytes())
        except (OSError, CutoverError) as exc:
            print(f"ERROR: rollback failed: {exc}", file=sys.stderr)
            return 2
        print(f"Restored settings from {settings_backup}")
        print(f"Restored Claude config from {claude_backup}")
        return 0

    try:
        settings, settings_raw = _load(settings_path)
        claude, claude_raw = _load(claude_path)
    except CutoverError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    clean_settings, removed_hooks = _clean_settings(settings)
    clean_claude, removed_mcp = _clean_mcp(claude)
    changed_settings = clean_settings != settings
    changed_claude = clean_claude != claude

    print("Purser hook entries to remove:")
    if removed_hooks:
        for hook in removed_hooks:
            print("  " + _display(hook))
    else:
        print("  (none)")
    print("Purser MCP entry to remove:")
    mcp = claude.get("mcpServers", {}).get("codex-main") if isinstance(claude.get("mcpServers"), dict) else None
    if removed_mcp:
        print(f"  name='codex-main', command={mcp.get('command')!r}, args={mcp.get('args')!r}")
    else:
        print("  (none)")
    print("Everything else will remain unchanged.")
    if mode == "--plan":
        return 0
    if not changed_settings and not changed_claude:
        print("No changes needed; no backup created.")
        return 0

    try:
        if not settings_path.is_file() or not claude_path.is_file():
            raise CutoverError("both settings files must exist before apply")
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
        settings_backup = _backup_path(settings_path, stamp)
        claude_backup = _backup_path(claude_path, stamp)
        shutil.copy2(settings_path, settings_backup)
        try:
            shutil.copy2(claude_path, claude_backup)
        except OSError:
            settings_backup.unlink(missing_ok=True)
            raise
        if changed_settings:
            _atomic_replace(settings_path, _render(clean_settings))
        if changed_claude:
            _atomic_replace(claude_path, _render(clean_claude))
    except (OSError, CutoverError) as exc:
        print(f"ERROR: apply failed: {exc}", file=sys.stderr)
        return 2
    print(f"Backups: {settings_backup} and {claude_backup}")
    print("Next steps in Claude Code:")
    print(f"/plugin marketplace add {repo_root}")
    try:
        marketplace = json.loads(marketplace_path.read_text(encoding="utf-8"))
        marketplace_name = marketplace["name"]
        if not isinstance(marketplace_name, str) or not marketplace_name:
            raise ValueError("marketplace name is missing")
    except (OSError, UnicodeError, json.JSONDecodeError, KeyError, TypeError, ValueError) as exc:
        print(f"ERROR: cannot read marketplace name: {exc}", file=sys.stderr)
        return 2
    print(f"/plugin install purser@{marketplace_name}")
    print("Restart Claude Code")
    print("/purser:check (expect plugin-only registration)")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument("--plan", action="store_true")
    modes.add_argument("--apply", action="store_true")
    modes.add_argument("--rollback", action="store_true")
    parser.add_argument("--home", type=Path, default=Path.home(), help=argparse.SUPPRESS)
    args = parser.parse_args(argv)
    mode = "--plan" if args.plan else "--apply" if args.apply else "--rollback"
    return run(mode, args.home, Path(__file__).resolve().parents[3])


if __name__ == "__main__":
    raise SystemExit(main())
