"""Resolve the governed project root from a hook session."""

from __future__ import annotations

from collections.abc import Mapping
import hashlib
import os
from pathlib import Path
import subprocess
from typing import Any


PROJECT_KEY_UNRESOLVED = "unresolved"


def project_key(resolved_root: str | os.PathLike[str] | Path | None) -> str:
    """Return the short stable namespace for a resolved project root."""

    if resolved_root is None:
        return PROJECT_KEY_UNRESOLVED
    normalized = os.path.normcase(str(resolved_root))
    return hashlib.sha256(normalized.encode("utf-8")).hexdigest()[:16]


def resolve_candidate(
    candidate: str | os.PathLike[str] | None,
) -> tuple[Path | None, str]:
    """Return a candidate's git top-level or a stable failure reason."""

    if candidate is None or not str(candidate).strip():
        return None, "project root unresolved: hook input has no cwd"
    try:
        directory = Path(candidate).resolve()
    except (OSError, RuntimeError, ValueError):
        return None, "project root unresolved: cwd is invalid"
    try:
        completed = subprocess.run(
            ["git", "rev-parse", "--show-toplevel"],
            cwd=directory,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=10,
            check=False,
        )
    except FileNotFoundError:
        return None, "project root unresolved: git executable is unavailable"
    except (OSError, subprocess.SubprocessError):
        return None, "project root unresolved: git top-level lookup failed"
    output = completed.stdout.strip()
    if completed.returncode != 0 or not output:
        return None, "project root unresolved: cwd is not inside a git work tree"
    try:
        return Path(output).resolve(), ""
    except (OSError, RuntimeError, ValueError):
        return None, "project root unresolved: git returned an invalid top-level"


def resolve_project_root(
    event: Mapping[str, Any] | None = None,
    explicit_root: str | os.PathLike[str] | None = None,
) -> tuple[Path | None, str]:
    """Resolve an explicit root or the hook session's cwd to its git top-level."""

    if explicit_root is not None:
        try:
            return Path(explicit_root).resolve(), ""
        except (OSError, RuntimeError, ValueError):
            return None, "project root unresolved: explicit root is invalid"
    source = event if isinstance(event, Mapping) else {}
    tool_input = source.get("tool_input")
    candidate = tool_input.get("cwd") if isinstance(tool_input, Mapping) else None
    if not isinstance(candidate, (str, os.PathLike)) or not str(candidate).strip():
        candidate = source.get("cwd")
    if not isinstance(candidate, (str, os.PathLike)):
        candidate = None
    return resolve_candidate(candidate)


__all__ = [
    "PROJECT_KEY_UNRESOLVED",
    "project_key",
    "resolve_candidate",
    "resolve_project_root",
]
