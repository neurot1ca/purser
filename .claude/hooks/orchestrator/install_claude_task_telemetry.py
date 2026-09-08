"""Install the repository-local Claude task telemetry hooks safely.

The installer preserves unrelated settings, replaces only hook entries that
target this repository's orchestrator hook, and writes settings atomically.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping


HOOK_SPECS: tuple[tuple[str, str | None, int], ...] = (
    ("PreToolUse", r"^mcp__codex-main__(codex|codex-reply)$", 60),
    ("PostToolUse", r"^mcp__codex-main__(codex|codex-reply)$", 300),
    ("PostToolUseFailure", r"^mcp__codex-main__(codex|codex-reply)$", 60),
    ("Stop", None, 60),
)


class InstallError(ValueError):
    """The settings file cannot be changed without violating the contract."""


def _within(path: Path, root: Path) -> Path:
    resolved = path.resolve()
    try:
        resolved.relative_to(root.resolve())
    except ValueError as exc:
        raise InstallError("settings path must remain inside the repository") from exc
    return resolved


def _hook_command(repo_root: Path, python_executable: Path) -> tuple[str, Path]:
    hook_path = (repo_root / ".claude" / "hooks" / "orchestrator" / "hook.py").resolve()
    if not hook_path.is_file():
        raise InstallError("orchestrator hook.py is unavailable")
    python_path = python_executable.resolve()
    if not python_path.is_file():
        raise InstallError("Python executable is unavailable")
    return subprocess.list2cmdline([str(python_path), str(hook_path)]), hook_path


def _managed_entry(entry: Any, hook_path: Path) -> bool:
    if not isinstance(entry, Mapping):
        return False
    hooks = entry.get("hooks")
    if not isinstance(hooks, list) or len(hooks) != 1 or not isinstance(hooks[0], Mapping):
        return False
    command = hooks[0].get("command")
    return isinstance(command, str) and str(hook_path).casefold() in command.casefold()


def _expected_entry(matcher: str | None, timeout: int, command: str) -> dict[str, Any]:
    entry: dict[str, Any] = {
        "hooks": [{"type": "command", "command": command, "timeout": timeout}],
    }
    if matcher is not None:
        entry["matcher"] = matcher
    return entry


def merge_settings(
    settings: Mapping[str, Any],
    *,
    repo_root: Path,
    python_executable: Path,
) -> tuple[dict[str, Any], bool]:
    """Return settings with exactly one managed entry for each lifecycle hook."""
    if not isinstance(settings, Mapping):
        raise InstallError("settings root must be a JSON object")
    result = dict(settings)
    hooks_value = result.get("hooks", {})
    if not isinstance(hooks_value, Mapping):
        raise InstallError("settings hooks must be a JSON object")
    hooks = dict(hooks_value)
    command, hook_path = _hook_command(repo_root, python_executable)

    for event_name, matcher, timeout in HOOK_SPECS:
        entries_value = hooks.get(event_name, [])
        if not isinstance(entries_value, list):
            raise InstallError(f"settings hook {event_name} must be a JSON array")
        unrelated = [entry for entry in entries_value if not _managed_entry(entry, hook_path)]
        hooks[event_name] = unrelated + [_expected_entry(matcher, timeout, command)]

    result["hooks"] = hooks
    return result, result != dict(settings)


def load_settings(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise InstallError("settings file is not valid UTF-8 JSON") from exc
    if not isinstance(value, dict):
        raise InstallError("settings root must be a JSON object")
    return value


def _atomic_write(path: Path, value: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            newline="\n",
            prefix=path.name + ".",
            suffix=".tmp",
            dir=path.parent,
            delete=False,
        ) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    finally:
        if temporary is not None and temporary.exists():
            temporary.unlink()


def install(
    *,
    repo_root: Path,
    settings_path: Path,
    python_executable: Path,
    apply: bool,
) -> str:
    root = repo_root.resolve()
    settings_target = _within(settings_path, root)
    current = load_settings(settings_target)
    updated, changed = merge_settings(current, repo_root=root, python_executable=python_executable)
    if not changed:
        return "ALREADY_INSTALLED"
    if not apply:
        return "INSTALL_REQUIRED"

    if settings_target.exists():
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        backup = settings_target.with_name(settings_target.name + f".bak-{stamp}")
        shutil.copy2(settings_target, backup)
    _atomic_write(settings_target, updated)
    return "INSTALLED"


def _parser() -> argparse.ArgumentParser:
    default_root = Path(__file__).resolve().parents[3]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=Path, default=default_root)
    parser.add_argument("--settings-path", type=Path)
    parser.add_argument("--python-executable", type=Path, default=Path(sys.executable))
    parser.add_argument("--apply", action="store_true", help="write settings; default is check-only")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    settings_path = args.settings_path or args.repo_root / ".claude" / "settings.local.json"
    try:
        status = install(
            repo_root=args.repo_root,
            settings_path=settings_path,
            python_executable=args.python_executable,
            apply=args.apply,
        )
    except InstallError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2
    print(status)
    return 0 if status in {"INSTALLED", "ALREADY_INSTALLED"} else 1


if __name__ == "__main__":
    raise SystemExit(main())
