"""Locked per-project active-coder state for fallback orchestration."""

from __future__ import annotations

from contextlib import contextmanager
import json
import os
from pathlib import Path
import tempfile
import time

import project_root
import state_root


def state_path(project: str | os.PathLike[str], root: Path | None = None) -> Path:
    key = project_root.project_key(Path(project).resolve())
    return (root if root is not None else state_root.default_state_root()) / "projects" / key / "active-coder.json"


def _validate(value: object) -> dict:
    fields = {"active_coder", "triggering_evidence", "reset_cycle_marker", "last_probe_time"}
    if not isinstance(value, dict) or set(value) != fields:
        raise ValueError("active-coder state has invalid fields")
    if value["active_coder"] not in ("primary", "fallback"):
        raise ValueError("active-coder state has an invalid active coder")
    if value["triggering_evidence"] is not None and not isinstance(value["triggering_evidence"], (str, dict)):
        raise ValueError("active-coder state has invalid triggering evidence")
    for key in ("reset_cycle_marker", "last_probe_time"):
        if value[key] is not None and not isinstance(value[key], str):
            raise ValueError(f"active-coder state has an invalid {key}")
    return value


@contextmanager
def _lock(path: Path):
    lock_path = path.with_name(path.name + ".lock")
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    handle = lock_path.open("a+b")
    handle.seek(0, os.SEEK_END)
    if handle.tell() == 0:
        handle.write(b"\0")
        handle.flush()
    acquired = False
    try:
        while not acquired:
            try:
                handle.seek(0)
                if os.name == "nt":
                    import msvcrt
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                acquired = True
            except OSError:
                time.sleep(0.01)
        yield
    finally:
        if acquired:
            handle.seek(0)
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
        handle.close()


def read(project: str | os.PathLike[str], root: Path | None = None) -> dict:
    path = state_path(project, root)
    with _lock(path):
        if not path.exists():
            return {"active_coder": "primary", "triggering_evidence": None,
                    "reset_cycle_marker": None, "last_probe_time": None}
        return _validate(json.loads(path.read_text(encoding="utf-8")))


def write(project: str | os.PathLike[str], value: dict, root: Path | None = None) -> None:
    path = state_path(project, root)
    content = json.dumps(_validate(value), sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
    with _lock(path):
        temporary = None
        try:
            with tempfile.NamedTemporaryFile(mode="wb", prefix=path.name + ".", suffix=".tmp",
                                             dir=path.parent, delete=False) as handle:
                temporary = Path(handle.name)
                handle.write(content)
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


__all__ = ["read", "state_path", "write"]
