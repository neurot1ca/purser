"""Find and track newer pinned-route model versions for SessionStart notices."""

from __future__ import annotations

from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import tempfile

import state_root


_VERSIONED_MODEL = re.compile(r"^(.+?)(\d+(?:\.\d+)*)([^0-9]*)$")


def _parts(model: str) -> tuple[str, tuple[int, ...], str] | None:
    match = _VERSIONED_MODEL.fullmatch(model)
    if not match:
        return None
    prefix, version, suffix = match.groups()
    if not prefix or not suffix or not any(char.isalpha() for char in prefix + suffix):
        return None
    return prefix, tuple(int(part) for part in version.split(".")), suffix


def newer_version(current: str, candidates: list[str]) -> str | None:
    """Return the highest strictly newer version with an identical model stem."""
    parts = _parts(current)
    if parts is None:
        return None
    prefix, version, suffix = parts
    newer = []
    for candidate in candidates:
        candidate_parts = _parts(candidate) if isinstance(candidate, str) else None
        if candidate_parts is None:
            continue
        other_prefix, other_version, other_suffix = candidate_parts
        if (other_prefix, other_suffix) == (prefix, suffix) and other_version > version:
            newer.append((other_version, candidate))
    return max(newer)[1] if newer else None


def is_newer(current: str, candidate: str) -> bool:
    return newer_version(current, [candidate]) == candidate


def state_path() -> Path:
    return state_root.default_state_root() / "model-version-notice.json"


def read_state() -> dict:
    try:
        value = json.loads(state_path().read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise ValueError("invalid model-version state")
        pending = value.get("pending")
        if not (isinstance(pending, dict) and isinstance(pending.get("current"), str)
                and isinstance(pending.get("new"), str)):
            pending = None
        else:
            pending = {"current": pending["current"], "new": pending["new"],
                       "blocked": pending.get("blocked") is True,
                       "delivered": pending.get("delivered") is True}
        return {
            "checked_on": value.get("checked_on") if isinstance(value.get("checked_on"), str) else None,
            "asked": list(dict.fromkeys(item for item in value.get("asked", []) if isinstance(item, str)))
            if isinstance(value.get("asked", []), list) else [],
            "declined": list(dict.fromkeys(item for item in value.get("declined", []) if isinstance(item, str)))
            if isinstance(value.get("declined", []), list) else [],
            "pending": pending,
        }
    except (OSError, ValueError, TypeError):
        return {"checked_on": None, "asked": [], "declined": [], "pending": None}


def write_state(value: dict) -> None:
    path = state_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=path.name + ".", suffix=".tmp", delete=False) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, separators=(",", ":"))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass


def decide(version: str, *, decline: bool) -> None:
    value = read_state()
    key = "declined" if decline else "asked"
    if version not in value[key]:
        value[key].append(version)
    pending = value.get("pending")
    if isinstance(pending, dict) and pending.get("new") == version:
        value["pending"] = None
    write_state(value)


def mark_shown(version: str) -> None:
    decide(version, decline=False)


def due_today(*, now: datetime | None = None) -> bool:
    clock = now or datetime.now(timezone.utc)
    return read_state().get("checked_on") != clock.date().isoformat()


def record_check(*, now: datetime | None = None) -> None:
    value = read_state()
    clock = now or datetime.now(timezone.utc)
    value["checked_on"] = clock.date().isoformat()
    write_state(value)
