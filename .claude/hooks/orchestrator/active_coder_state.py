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


def _read_unlocked(path: Path) -> dict:
    if not path.exists():
        return {"active_coder": "primary", "triggering_evidence": None,
                "reset_cycle_marker": None, "last_probe_time": None}
    return _validate(json.loads(path.read_text(encoding="utf-8")))


def _write_unlocked(path: Path, value: dict) -> None:
    content = json.dumps(_validate(value), sort_keys=True, ensure_ascii=False,
                         separators=(",", ":")).encode("utf-8")
    path.parent.mkdir(parents=True, exist_ok=True)
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


def read(project: str | os.PathLike[str], root: Path | None = None) -> dict:
    path = state_path(project, root)
    with _lock(path):
        return _read_unlocked(path)


def write(project: str | os.PathLike[str], value: dict, root: Path | None = None) -> None:
    path = state_path(project, root)
    with _lock(path):
        _write_unlocked(path, value)


def update(project: str | os.PathLike[str], change, root: Path | None = None) -> dict:
    path = state_path(project, root)
    with _lock(path):
        value = _read_unlocked(path)
        change(value)
        _write_unlocked(path, value)
        return value


def record_fallback_question(project: str | os.PathLike[str], evidence: dict,
                             session_id: str, root: Path | None = None) -> bool:
    created = False
    def change(value):
        nonlocal created
        current = value.get("triggering_evidence")
        if (value["active_coder"] == "primary"
                and not (isinstance(current, dict) and "question_session" in current)):
            value["triggering_evidence"] = {
                **evidence, "question_session": session_id,
                "question_shown": False, "answerable": False,
            }
            created = True
    update(project, change, root)
    return created


def update_fallback_question(project: str | os.PathLike[str], session_id: str,
                              *, shown: bool = False, answerable: bool = False,
                              root: Path | None = None) -> None:
    def change(value):
        evidence = value.get("triggering_evidence")
        if isinstance(evidence, dict) and evidence.get("question_session") == session_id:
            if shown:
                evidence["question_shown"] = True
            if answerable and evidence.get("question_shown") is True:
                evidence["answerable"] = True
    update(project, change, root)


def approve_fallback(project: str | os.PathLike[str], session_id: str,
                    root: Path | None = None, *, reset_cycle_marker: str | None = None,
                    switched_at: float | None = None) -> bool:
    approved = False
    def change(value):
        nonlocal approved
        evidence = value.get("triggering_evidence")
        if (value["active_coder"] == "primary" and isinstance(evidence, dict)
                and evidence.get("question_session") == session_id
                and evidence.get("question_shown") is True
                and evidence.get("answerable") is True):
            value["active_coder"] = "fallback"
            value["reset_cycle_marker"] = reset_cycle_marker
            if switched_at is not None:
                evidence["switched_at"] = switched_at
            value["last_probe_time"] = None
            for key in ("question", "question_session", "question_shown", "answerable"):
                evidence.pop(key, None)
            approved = True
    update(project, change, root)
    return approved


__all__ = ["read", "state_path", "write"]
