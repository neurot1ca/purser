"""Warn about oversized state and prune only explicitly regenerable files."""

from __future__ import annotations

from dataclasses import dataclass
import fnmatch
import os
from pathlib import Path
import time

import telemetry


@dataclass(frozen=True)
class Policy:
    owner: str | None = None
    max_age_days: int | None = None
    max_files: int | None = None
    max_bytes: int | None = None
    delete_patterns: tuple[str, ...] = ()


MIB = 1024 * 1024
GIB = 1024 * MIB
STATE_ROOT_MAX_BYTES = 16 * GIB
DEFAULT_POLICY = Policy(max_age_days=30, max_bytes=64 * MIB)
CEILING_STAMP = ".state-retention-check"
CEILING_LOCK = ".state-retention-check.lock"

# Empty delete_patterns means count and warn. The cited writers create these
# entries, but none has a proven closed/regenerable deletion lifecycle here.
REGISTRY: dict[str, Policy] = {
    "preparations": Policy(owner="preparations"),  # ledger.prepare_job_replay
    "wt": Policy(owner="worktrees"),  # ledger.create_job_worktree
    "marketplace": Policy(owner="release"),  # scripts.release.export_marketplace
    "telemetry": Policy(owner="telemetry-distill"),  # telemetry._append_event; distill in milestone 4
    "projects": Policy(max_bytes=128 * MIB, delete_patterns=()),  # ledger.ScopeLeases, SessionLedger, project_profile
    "codex_exec_mcp": Policy(owner="jobs-cleanup"),  # codex_exec_mcp.Server job persistence
    "raw_tool_outputs": Policy(max_age_days=7, max_bytes=64 * MIB, delete_patterns=()),  # codex._persist_raw_output; recovery-linked
    "bundle_artifacts": Policy(max_age_days=7, max_bytes=32 * MIB, delete_patterns=()),  # codex._finalize_replacement; recovery-linked
    "async_jobs": Policy(owner="jobs-cleanup"),  # codex.py async index lifecycle
    "pending": Policy(owner="jobs-cleanup"),  # codex.py pending-call lifecycle
    "sessions": Policy(max_files=1024, max_bytes=16 * MIB, delete_patterns=()),  # ledger.SessionLedger.save
    "threads": Policy(max_age_days=90, max_files=1024, max_bytes=8 * MIB, delete_patterns=()),  # codex._record_thread_root
    "context_io": Policy(max_files=256, max_bytes=16 * MIB, delete_patterns=()),  # context_io_telemetry writers
    "active_tasks": Policy(max_files=256, max_bytes=8 * MIB, delete_patterns=()),  # claude_transcript_usage writers
    "maintenance": Policy(max_bytes=4 * MIB, delete_patterns=()),  # maintenance_reports writers
    "plans": Policy(max_age_days=90, max_files=64, max_bytes=16 * MIB, delete_patterns=()),  # codex._store_plan
    "opencode": Policy(max_files=128, max_bytes=16 * MIB, delete_patterns=()),  # mechanical_learning writers
    # model_catalog._atomic_write writes this cache; load_cached_catalog falls
    # back to the checked-in bootstrap document if the cache is absent.
    "model-catalog": Policy(max_age_days=30, max_files=1, max_bytes=32 * MIB,
                             delete_patterns=("catalog.json",)),
    "diagnostics.jsonl": Policy(max_bytes=4 * MIB, delete_patterns=()),  # codex._record_diagnostic and Go appenders; no shared lock
    "codex-cli-version.json": Policy(max_bytes=MIB, delete_patterns=()),  # codex CLI version-state writer; retain by default
    "model-version-notice.json": Policy(max_bytes=MIB, delete_patterns=()),  # model_version_notice.write_state; retain by default
    "plugin-version.json": Policy(max_bytes=MIB, delete_patterns=()),  # compaction_advisor._atomic_write; retain by default
    CEILING_STAMP: Policy(owner="retention"),
    CEILING_LOCK: Policy(owner="retention"),
}


def _warning(root: Path) -> None:
    try:
        telemetry.record_health("DEGRADED", error_code="STATE_RETENTION_LIMIT", state_root=root)
    except BaseException:
        pass


def _files(base: Path) -> list[Path]:
    if base.is_file():
        return [base]
    result: list[Path] = []
    for current, dirs, names in os.walk(base, followlinks=False):
        current_path = Path(current)
        dirs[:] = [name for name in dirs if not (current_path / name).is_symlink()]
        for name in names:
            path = current_path / name
            try:
                if not path.is_symlink() and path.is_file():
                    result.append(path)
            except OSError:
                continue
    return result


def _size(path: Path) -> int:
    try:
        return path.stat().st_size
    except OSError:
        return 0


def _eligible(path: Path, base: Path, policy: Policy, now: float) -> bool:
    if not policy.delete_patterns or path.name.endswith(".lock"):
        return False
    try:
        if now - path.stat().st_mtime < 600:
            return False
        relative = path.name if base.is_file() else path.relative_to(base).as_posix()
    except (OSError, ValueError):
        return False
    return any(fnmatch.fnmatchcase(relative, pattern) for pattern in policy.delete_patterns)


def _over_limit(files: list[Path], policy: Policy, now: float) -> bool:
    if policy.max_files is not None and len(files) > policy.max_files:
        return True
    if policy.max_bytes is not None and sum(_size(path) for path in files) > policy.max_bytes:
        return True
    if policy.max_age_days is not None:
        cutoff = now - policy.max_age_days * 86400
        for path in files:
            try:
                if path.stat().st_mtime < cutoff:
                    return True
            except OSError:
                return True
    return False


def _prune(base: Path, policy: Policy, now: float) -> bool:
    files = _files(base)
    cutoff = now - policy.max_age_days * 86400 if policy.max_age_days is not None else None
    candidates = sorted((path for path in files if _eligible(path, base, policy, now)),
                        key=lambda path: (path.stat().st_mtime, str(path)))
    # Expired generated files first, then oldest files until count/bytes fit.
    expired: list[Path] = []
    if cutoff is not None:
        for path in candidates:
            try:
                if path.stat().st_mtime < cutoff:
                    expired.append(path)
            except OSError:
                continue
    for path in expired:
        try:
            path.unlink(missing_ok=True)
            files.remove(path)
        except OSError:
            pass
    candidates = sorted((path for path in files if _eligible(path, base, policy, now)),
                        key=lambda path: (path.stat().st_mtime, str(path)))
    count = len(files)
    total = sum(_size(path) for path in files)
    for path in candidates:
        if ((policy.max_files is None or count <= policy.max_files)
                and (policy.max_bytes is None or total <= policy.max_bytes)):
            break
        size = _size(path)
        try:
            path.unlink(missing_ok=True)
            count -= 1
            total -= size
        except OSError:
            pass
    remaining = _files(base)
    return _over_limit(remaining, policy, now)


def _root_size(root: Path) -> int:
    return sum(_size(path) for path in _files(root))


def _check_ceiling(root: Path, now: float) -> None:
    lock_path = root / CEILING_LOCK
    try:
        with lock_path.open("a+b") as handle:
            handle.seek(0, os.SEEK_END)
            if handle.tell() == 0:
                handle.write(b"0")
                handle.flush()
            handle.seek(0)
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            try:
                stamp = root / CEILING_STAMP
                try:
                    if now - stamp.stat().st_mtime < 86400:
                        return
                except OSError:
                    pass
                if _root_size(root) > STATE_ROOT_MAX_BYTES:
                    _warning(root)
                stamp.touch()
                os.utime(stamp, (now, now))
            finally:
                if os.name == "nt":
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
    except (OSError, BlockingIOError):
        return None


def prune_state(state_root: str | os.PathLike[str], *, now: float | None = None,
                check_ceiling: bool = False) -> None:
    """Prune allowlisted files; run the daily deep check only on a background caller."""
    root = Path(state_root)
    current = time.time() if now is None else now
    try:
        for entry in root.iterdir():
            if entry.is_symlink():
                continue
            policy = REGISTRY.get(entry.name)
            if policy is None:
                policy = DEFAULT_POLICY
                _warning(root)
            if policy.owner is not None:
                continue
            try:
                if _prune(entry, policy, current):
                    _warning(root)
            except BaseException:
                continue
        if check_ceiling:
            _check_ceiling(root, current)
    except BaseException:
        return None


__all__ = ["CEILING_STAMP", "DEFAULT_POLICY", "Policy", "REGISTRY", "STATE_ROOT_MAX_BYTES", "prune_state"]
