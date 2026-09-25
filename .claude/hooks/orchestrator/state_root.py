"""Resolve Purser's shared, per-user runtime state directory."""

from __future__ import annotations

import os
from pathlib import Path


def default_state_root() -> Path:
    """Return the configured state root as an absolute path without creating it."""

    override = os.environ.get("PURSER_STATE_DIR")
    if override:
        return Path(override).expanduser().resolve()

    claude_config = os.environ.get("CLAUDE_CONFIG_DIR")
    if claude_config:
        return (Path(claude_config).expanduser() / "purser").resolve()

    return (Path.home() / ".claude" / "purser").resolve()
