"""Best-effort quota snapshot from a Codex thread's own rollout."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
import json
import math
import os
from pathlib import Path


TAIL_BYTES = 256 * 1024


def _rollout(sessions: Path, thread_id: str) -> Path | None:
    today = datetime.now().date()
    for day in (today, today - timedelta(days=1)):
        folder = sessions / day.strftime("%Y/%m/%d")
        match = next(folder.glob(f"rollout-*-{thread_id}.jsonl"), None)
        if match is not None:
            return match
    # Two calendar years bound the fallback without walking the entire home.
    for year in (today.year, today.year - 1):
        match = next(sessions.glob(f"{year}/[0-1][0-9]/[0-3][0-9]/rollout-*-{thread_id}.jsonl"), None)
        if match is not None:
            return match
    return None


def _window(value: object, minutes: int) -> tuple[int | float, str] | None:
    if not isinstance(value, dict) or value.get("window_minutes") != minutes:
        return None
    used = value.get("used_percent")
    reset = value.get("resets_at")
    if (isinstance(used, bool) or not isinstance(used, (int, float))
            or not math.isfinite(used) or isinstance(reset, bool)
            or not isinstance(reset, (int, float)) or not math.isfinite(reset)):
        return None
    try:
        utc = datetime.fromtimestamp(reset, timezone.utc).isoformat().replace("+00:00", "Z")
    except (OverflowError, OSError, ValueError):
        return None
    return used, utc


def read_quota(thread_id: str, *, codex_home=None) -> dict | None:
    """Read the last rate-limit event from at most 256 KiB of a rollout."""
    try:
        if not isinstance(thread_id, str) or not thread_id or any(char in thread_id for char in "/\\*?[]"):
            return None
        home = Path(codex_home) if codex_home is not None else Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
        path = _rollout(home / "sessions", thread_id)
        if path is None:
            return None
        with path.open("rb") as stream:
            size = stream.seek(0, os.SEEK_END)
            offset = max(0, size - TAIL_BYTES)
            stream.seek(offset)
            tail = stream.read(TAIL_BYTES)
        if offset:
            tail = tail.partition(b"\n")[2]
        for line in reversed(tail.splitlines()):
            try:
                event = json.loads(line)
            except (UnicodeDecodeError, json.JSONDecodeError):
                continue
            if not isinstance(event, dict) or not isinstance(event.get("payload"), dict):
                continue
            limits = event["payload"].get("rate_limits")
            if not isinstance(limits, dict):
                continue
            result = {}
            for source, label, minutes in (("primary", "5h", 300), ("secondary", "7d", 10080)):
                window = _window(limits.get(source), minutes)
                if window is not None:
                    result[f"{label}_used_pct"] = window[0]
                    result[f"{label}_resets_at"] = window[1]
            return result or None
    except (OSError, ValueError, TypeError):
        return None
    return None
