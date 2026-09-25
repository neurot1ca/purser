"""Resolve Purser's shared, per-user runtime state directory."""

from __future__ import annotations

import os
from pathlib import Path
import time
from typing import Callable, TypeVar


T = TypeVar("T")
_PERMISSION_RETRY_SECONDS = 1.0
_PERMISSION_RETRY_INTERVAL = 0.02


def retry_permission_error(operation: Callable[[], T]) -> T:
    """Retry transient Windows sharing violations for at most one second."""
    deadline = time.monotonic() + _PERMISSION_RETRY_SECONDS
    while True:
        try:
            return operation()
        except PermissionError:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise
            time.sleep(min(_PERMISSION_RETRY_INTERVAL, remaining))


def default_state_root() -> Path:
    """Return the configured state root as an absolute path without creating it."""

    override = os.environ.get("PURSER_STATE_DIR")
    if override:
        return Path(override).expanduser().resolve()

    claude_config = os.environ.get("CLAUDE_CONFIG_DIR")
    if claude_config:
        return (Path(claude_config).expanduser() / "purser").resolve()

    return (Path.home() / ".claude" / "purser").resolve()
