"""Git baselines, reviewable deltas, and durable orchestrator session records.

All public results are plain JSON-serializable mappings.  Inspection failures
are explicit UNKNOWN results and deliberately omit numeric delta fields, so a
caller cannot accidentally interpret a failed inspection as an empty change.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping, Sequence
from contextlib import contextmanager
import hashlib
import hmac
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import tempfile
import time
from typing import Any

import json_transport
import project_profile
import state_root as purser_state_root


OK = "OK"
UNKNOWN = "UNKNOWN"
PROCEED = "PROCEED"
UNKNOWN_CONCURRENT_WRITER = "UNKNOWN_CONCURRENT_WRITER"
MISSING = "MISSING"
CORRUPT = "CORRUPT"
MANIFEST_BUDGET_EXCEEDED = "MANIFEST_BUDGET_EXCEEDED"
DEFAULT_MANIFEST_BUDGET_SECONDS = 3.0
_INDEX_PATHSPEC_BYTE_BUDGET = 8000

_LEDGER_SCHEMA_VERSION = 1
SYNC_LEASE_FALLBACK_SECONDS = 2 * 60 * 60
LEASE_TIMEOUT_MARGIN_SECONDS = 60
ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS = 2700
JOB_RETENTION_SECONDS = 7 * 24 * 60 * 60
RELEASED_LEASE_RETENTION_SECONDS = 7 * 24 * 60 * 60


@contextmanager
def _lease_lock(path: Path):
    """Serialize admissions across hook processes, including on Windows."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a+b") as handle:
        deadline = time.monotonic() + 10
        while True:
            try:
                if os.name == "nt":
                    import msvcrt
                    handle.seek(0)
                    if not handle.read(1):
                        handle.write(b"0")
                        handle.flush()
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.02)
        try:
            yield
        finally:
            if os.name == "nt":
                handle.seek(0)
                msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def _scopes_overlap(left_scope: Sequence[str], right_scope: Sequence[str]) -> bool:
    for left in left_scope:
        for right in right_scope:
            a, b = (left.casefold(), right.casefold()) if os.name == "nt" else (left, right)
            if a == b or a.startswith(b + "/") or b.startswith(a + "/"):
                return True
    return False


class ScopeLeases:
    """Durable per-repository ownership; uncertain owners stay active."""

    def __init__(self, project_state_root: str | os.PathLike[str]) -> None:
        root = Path(project_state_root)
        self.path = root / "scope_leases.json"
        self.lock_path = root / "scope_leases.lock"
        self.jobs_root = root.parent.parent / "codex_exec_mcp" / "jobs"

    def _read(self) -> dict[str, Any]:
        try:
            value = json.loads(self.path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return {"version": 1, "owners": {}}
        if (not isinstance(value, dict) or value.get("version") != 1
                or not isinstance(value.get("owners"), dict)):
            raise ValueError("scope lease record is invalid")
        for key, owner in value["owners"].items():
            if (not isinstance(key, str) or not isinstance(owner, dict)
                    or owner.get("state") not in {"ACTIVE", "RELEASED"}
                    or not isinstance(owner.get("task_id"), str)
                    or not isinstance(owner.get("scope"), list)
                    or not owner["scope"]
                    or any(not isinstance(path, str) or not path for path in owner["scope"])):
                raise ValueError("scope lease owner is invalid")
        return value

    def _write(self, value: dict[str, Any]) -> None:
        cutoff = time.time() - RELEASED_LEASE_RETENTION_SECONDS
        active_starts = [owner.get("acquired_at") for owner in value["owners"].values()
                         if owner["state"] == "ACTIVE"]
        value["owners"] = {
            key: owner for key, owner in value["owners"].items()
            if owner["state"] != "RELEASED"
            or not isinstance(owner.get("released_at"), (int, float))
            or owner["released_at"] >= cutoff
            or any(isinstance(start, (int, float)) and start <= owner["released_at"]
                   for start in active_starts)
        }
        self.path.parent.mkdir(parents=True, exist_ok=True)
        temporary: Path | None = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=self.path.parent,
                                             prefix="scope_leases.", suffix=".tmp", delete=False) as handle:
                temporary = Path(handle.name)
                handle.write(json_transport.dumps(value, sort_keys=True, separators=(",", ":")))
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, self.path)
            temporary = None
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)

    def _job_state(self, job_id: str) -> str:
        try:
            job = json.loads((self.jobs_root / f"{job_id}.json").read_text(encoding="utf-8"))
        except FileNotFoundError:
            return "MISSING"
        except (OSError, ValueError, TypeError):
            return "UNREADABLE"
        if not isinstance(job, dict) or job.get("jobId") != job_id:
            return "UNREADABLE"
        call = job.get("call")
        if not isinstance(call, str) or hashlib.sha256(call.encode("utf-8")).hexdigest() != job_id:
            return "UNREADABLE"
        state = job.get("state")
        if state == "EXPIRED" or (state == "FAILED" and job.get("errorCode") == "JOB_EXPIRED"):
            return "JOB_EXPIRED"
        return state if isinstance(state, str) else "UNREADABLE"

    def _reconcile(self, value: dict[str, Any], now: float) -> bool:
        changed = False
        for owner in value["owners"].values():
            if owner["state"] != "ACTIVE":
                continue
            job_id = owner.get("job_id")
            if job_id is None:
                acquired = owner.get("acquired_at")
                deadline = owner.get("deadline_at")
                if deadline is None and isinstance(acquired, (int, float)):
                    deadline = acquired + SYNC_LEASE_FALLBACK_SECONDS
                expired = isinstance(deadline, (int, float)) and now >= deadline
            elif isinstance(job_id, str):
                state = self._job_state(job_id)
                cutoff = owner.get("retention_after")
                expired = (state == "JOB_EXPIRED" or
                           (state == "MISSING" and isinstance(cutoff, (int, float))
                            and now >= cutoff))
            else:
                expired = False
            if expired:
                owner["state"] = "RELEASED"
                owner["released_at"] = now
                changed = True
        return changed

    def acquire(self, owner_key: str, task_id: str, scope: Sequence[str],
                *, job_id: str | None = None, timeout_seconds: float | None = None) -> dict[str, Any]:
        if not owner_key or not task_id or not scope:
            return {"status": UNKNOWN, "code": "LEASE_INVALID"}
        try:
            with _lease_lock(self.lock_path):
                value = self._read()
                now = time.time()
                if self._reconcile(value, now):
                    self._write(value)
                prior = value["owners"].get(owner_key)
                if prior is not None:
                    return ({"status": OK, "code": "LEASE_ACQUIRED"}
                            if prior["state"] == "ACTIVE" and prior["task_id"] == task_id
                            and prior["scope"] == list(scope) else
                            {"status": UNKNOWN, "code": "LEASE_OWNER_CONFLICT"})
                for other in value["owners"].values():
                    if other["state"] != "ACTIVE":
                        continue
                    if _scopes_overlap(scope, other["scope"]):
                        return {"status": UNKNOWN, "code": "SCOPE_LEASE_CONFLICT",
                                "task_id": other["task_id"]}
                owner = {
                    "task_id": task_id, "scope": list(scope), "state": "ACTIVE",
                    "job_id": job_id, "acquired_at": now,
                }
                if job_id is None:
                    owner["deadline_at"] = now + (
                        timeout_seconds + LEASE_TIMEOUT_MARGIN_SECONDS
                        if isinstance(timeout_seconds, (int, float)) and not isinstance(timeout_seconds, bool)
                        and math.isfinite(timeout_seconds) and timeout_seconds > 0
                        else SYNC_LEASE_FALLBACK_SECONDS
                    )
                else:
                    job_timeout = (timeout_seconds if isinstance(timeout_seconds, (int, float))
                                   and not isinstance(timeout_seconds, bool)
                                   and math.isfinite(timeout_seconds) and timeout_seconds > 0
                                   else ASYNC_JOB_DEFAULT_TIMEOUT_SECONDS)
                    owner["retention_after"] = now + job_timeout + JOB_RETENTION_SECONDS
                value["owners"][owner_key] = owner
                self._write(value)
                return {"status": OK, "code": "LEASE_ACQUIRED"}
        except (OSError, ValueError, TypeError):
            return {"status": UNKNOWN, "code": "LEASE_ADMISSION_UNCERTAIN"}

    def release(self, owner_key: str) -> dict[str, Any]:
        try:
            with _lease_lock(self.lock_path):
                value = self._read()
                owner = value["owners"].get(owner_key)
                if owner is None:
                    return {"status": UNKNOWN, "code": "LEASE_OWNER_MISSING"}
                if owner["state"] == "ACTIVE":
                    owner["state"] = "RELEASED"
                    owner["released_at"] = time.time()
                    self._write(value)
                return {"status": OK, "code": "LEASE_RELEASED"}
        except (OSError, ValueError, TypeError):
            return {"status": UNKNOWN, "code": "LEASE_RELEASE_UNCERTAIN"}

    def active(self) -> list[dict[str, Any]]:
        with _lease_lock(self.lock_path):
            value = self._read()
            now = time.time()
            if self._reconcile(value, now):
                self._write(value)
            return [
                {"task_id": owner["task_id"], "scope": list(owner["scope"]),
                 "age_seconds": max(0, int(now - owner["acquired_at"])),
                 "job_state": self._job_state(owner["job_id"]) if owner.get("job_id") else None}
                for owner in value["owners"].values() if owner["state"] == "ACTIVE"
            ]

    def overlapping_owners(self, owner_key: str) -> list[dict[str, Any]]:
        """Read leases whose lifetimes intersect this call."""
        with _lease_lock(self.lock_path):
            value = self._read()
            now = time.time()
            if self._reconcile(value, now):
                self._write(value)
            own = value["owners"].get(owner_key)
            if own is None or not isinstance(own.get("acquired_at"), (int, float)):
                raise ValueError("current scope lease history is missing")
            start = own["acquired_at"]
            end = own.get("released_at", now)
            if not isinstance(end, (int, float)):
                raise ValueError("current scope lease release time is missing")
            result = []
            for key, owner in value["owners"].items():
                if key == owner_key:
                    continue
                acquired = owner.get("acquired_at")
                released = owner.get("released_at", now)
                if not isinstance(acquired, (int, float)) or not isinstance(released, (int, float)):
                    raise ValueError("scope lease timing history is missing")
                if acquired < end and released > start:
                    result.append({"task_id": owner["task_id"], "scope": list(owner["scope"])})
            return result

    def release_task(self, task_id: str) -> list[dict[str, Any]]:
        with _lease_lock(self.lock_path):
            value = self._read()
            now = time.time()
            released = []
            for owner in value["owners"].values():
                if owner["state"] == "ACTIVE" and owner["task_id"] == task_id:
                    released.append({"task_id": task_id, "scope": list(owner["scope"]),
                                     "job_id": owner.get("job_id")})
                    owner["state"] = "RELEASED"
                    owner["released_at"] = now
            if released:
                self._write(value)
            return released
_SECRET_KEY_PARTS = (
    "api_key",
    "apikey",
    "credential",
    "password",
    "private_key",
    "secret",
    "token",
    "wallet",
)
_SECRET_VALUE_PATTERNS = (
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"\bsk-[A-Za-z0-9_-]{20,}\b"),
    re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}\b"),
    re.compile(r"\bAKIA[A-Z0-9]{16}\b"),
    re.compile(r"\bBearer\s+[A-Za-z0-9._~+/-]{12,}", re.IGNORECASE),
)
# Git snapshots use repository paths as identifiers; nested snapshot keys remain fields.
_PATH_KEYED_RECORD_LOCATIONS = frozenset(
    {
        ("pending_call_baseline", "tracked_snapshots"),
        ("live_thread", "origin_baseline", "tracked_snapshots"),
        ("live_thread", "post_state", "tracked_snapshots"),
    }
)


def is_transient_bytecode_cache(path: str | os.PathLike[str]) -> bool:
    """Return True if path points to transient Python bytecode or interpreter cache."""
    norm = os.fspath(path).replace("\\", "/")
    parts = norm.split("/")
    return "__pycache__" in parts or norm.endswith((".pyc", ".pyo"))


class _InspectionError(Exception):
    def __init__(self, code: str, message: str) -> None:
        self.code = code
        self.message = message
        super().__init__(f"{code}: {message}")


def _unknown(code: str, message: str, **context: Any) -> dict[str, Any]:
    return {
        "status": UNKNOWN,
        "code": code,
        "message": message,
        "changed_paths": None,
        **context,
    }


def _run_git(
    repo_root: Path,
    arguments: Sequence[str],
    timeout: float,
    operation: str,
) -> bytes:
    try:
        completed = subprocess.run(
            ["git", *arguments],
            cwd=repo_root,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
        )
    except subprocess.TimeoutExpired as error:
        raise _InspectionError(
            "GIT_TIMEOUT", f"Git timed out while {operation}."
        ) from error
    except OSError as error:
        raise _InspectionError(
            "GIT_UNAVAILABLE", f"Git could not run while {operation}."
        ) from error
    if completed.returncode != 0:
        raise _InspectionError(
            "GIT_FAILED",
            f"Git failed with exit code {completed.returncode} while {operation}.",
        )
    return completed.stdout


def _decode_path(value: bytes) -> str:
    return os.fsdecode(value).replace("\\", "/")


def _normalize_scope(scope: Iterable[str | os.PathLike[str]]) -> list[str]:
    if isinstance(scope, (str, bytes, os.PathLike)):
        scope = [scope]  # type: ignore[list-item]
    normalized: list[str] = []
    for raw in scope:
        value = os.fspath(raw)
        if not isinstance(value, str) or not value.strip():
            raise _InspectionError("INVALID_SCOPE", "Scope paths must be non-empty strings.")
        value = value.strip().replace("\\", "/")
        path = PurePosixPath(value)
        if (
            "\x00" in value
            or path.is_absolute()
            or ".." in path.parts
            or re.match(r"^[A-Za-z]:", value)
        ):
            raise _InspectionError(
                "INVALID_SCOPE", "Scope paths must be literal repository-relative paths."
            )
        canonical = path.as_posix()
        if canonical in ("", "."):
            raise _InspectionError(
                "UNBOUNDED_SCOPE",
                "A whole-repository manifest scope is forbidden.",
            )
        normalized.append(canonical)
    if not normalized:
        raise _InspectionError("INVALID_SCOPE", "At least one scoped path is required.")
    return sorted(set(normalized))


def _normalize_optional_scope(
    scope: Iterable[str | os.PathLike[str]] | None,
) -> list[str]:
    if scope is None:
        return []
    if not isinstance(scope, (str, bytes, os.PathLike)):
        scope = list(scope)
        if not scope:
            return []
    return _normalize_scope(scope)


def _literal_pathspecs(paths: Iterable[str]) -> list[str]:
    return [f":(literal){path}" for path in paths]


def _parse_status(raw: bytes) -> list[dict[str, str]]:
    records = raw.split(b"\0")
    entries: list[dict[str, str]] = []
    index = 0
    while index < len(records) and records[index]:
        record = records[index]
        if len(record) < 4 or record[2:3] != b" ":
            raise _InspectionError("GIT_OUTPUT_INVALID", "Git status output was malformed.")
        xy = record[:2].decode("ascii", "strict")
        entry = {"path": _decode_path(record[3:]), "state": xy}
        index += 1
        if "R" in xy or "C" in xy:
            if index >= len(records) or not records[index]:
                raise _InspectionError("GIT_OUTPUT_INVALID", "Git rename output was malformed.")
            entry["original_path"] = _decode_path(records[index])
            index += 1
        entries.append(entry)
    return sorted(entries, key=lambda item: (item["path"], item["state"]))


def _status_entries(repo_root: Path, timeout: float) -> list[dict[str, str]]:
    raw = _run_git(
        repo_root,
        ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
        timeout,
        "reading repository status",
    )
    return _parse_status(raw)


def _manifest_budget_error() -> _InspectionError:
    return _InspectionError(
        MANIFEST_BUDGET_EXCEEDED,
        "The scoped untracked/ignored manifest exceeded its wall-clock budget.",
    )


def _check_manifest_deadline(deadline: float | None) -> None:
    if deadline is not None and time.monotonic() >= deadline:
        raise _manifest_budget_error()


def _hash_file(path: Path, *, deadline: float | None = None) -> tuple[str, int]:
    digest = hashlib.sha256()
    lines = 0
    last = b""
    with path.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            _check_manifest_deadline(deadline)
            digest.update(chunk)
            lines += chunk.count(b"\n")
            last = chunk[-1:]
    _check_manifest_deadline(deadline)
    size = path.stat().st_size
    if size and last != b"\n":
        lines += 1
    return digest.hexdigest(), lines


def _filesystem_fingerprint(path: Path) -> dict[str, Any]:
    try:
        info = path.lstat()
    except FileNotFoundError:
        return {"kind": "missing"}
    if stat.S_ISLNK(info.st_mode):
        target = os.readlink(path).encode("utf-8", "surrogatepass")
        return {
            "kind": "symlink",
            "sha256": hashlib.sha256(target).hexdigest(),
            "mode": stat.S_IMODE(info.st_mode),
        }
    if stat.S_ISREG(info.st_mode):
        digest, _ = _hash_file(path)
        return {
            "kind": "file",
            "sha256": digest,
            "mode": stat.S_IMODE(info.st_mode),
        }
    return {"kind": "other", "mode": stat.S_IMODE(info.st_mode)}


def _index_entries(repo_root: Path, paths: Sequence[str], timeout: float) -> dict[str, list[str]]:
    result = {path: [] for path in paths}
    if not paths:
        return result
    pathspecs = _literal_pathspecs(paths)
    batches: list[list[str]] = []
    batch: list[str] = []
    batch_bytes = 0
    for pathspec in pathspecs:
        argument_bytes = len(os.fsencode(pathspec)) + 1
        if batch and batch_bytes + argument_bytes > _INDEX_PATHSPEC_BYTE_BUDGET:
            batches.append(batch)
            batch = []
            batch_bytes = 0
        batch.append(pathspec)
        batch_bytes += argument_bytes
    batches.append(batch)

    for batch in batches:
        raw = _run_git(
            repo_root,
            ["ls-files", "--stage", "-z", "--", *batch],
            timeout,
            "fingerprinting index entries",
        )
        for record in raw.split(b"\0"):
            if not record:
                continue
            metadata, separator, raw_path = record.partition(b"\t")
            fields = metadata.split(b" ")
            if not separator or len(fields) != 3:
                raise _InspectionError("GIT_OUTPUT_INVALID", "Git index output was malformed.")
            path = _decode_path(raw_path)
            result.setdefault(path, []).append(metadata.decode("ascii", "strict"))
    for entries in result.values():
        entries.sort()
    return result


def _tracked_snapshots(
    repo_root: Path,
    status_entries: Sequence[Mapping[str, str]],
    timeout: float,
    extra_paths: Iterable[str] = (),
) -> dict[str, dict[str, Any]]:
    paths = set(extra_paths)
    states: dict[str, list[str]] = {}
    for entry in status_entries:
        for key in ("path", "original_path"):
            path = entry.get(key)
            if path:
                paths.add(path)
        states.setdefault(entry["path"], []).append(entry["state"])
    ordered = sorted(paths)
    index = _index_entries(repo_root, ordered, timeout)
    snapshots: dict[str, dict[str, Any]] = {}
    for path in ordered:
        snapshots[path] = {
            "status": sorted(states.get(path, [])),
            "index": index.get(path, []),
            "worktree": _filesystem_fingerprint(repo_root / Path(path)),
        }
    return snapshots


def _manifest_git(
    repo_root: Path,
    arguments: Sequence[str],
    timeout: float,
    operation: str,
    deadline: float,
) -> bytes:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise _manifest_budget_error()
    budget_limited = remaining <= timeout
    try:
        return _run_git(repo_root, arguments, min(timeout, remaining), operation)
    except _InspectionError as error:
        if error.code == "GIT_TIMEOUT" and budget_limited:
            raise _manifest_budget_error() from error
        raise


def _manifest_paths(raw: bytes, deadline: float) -> set[str]:
    paths: set[str] = set()
    for index, value in enumerate(raw.split(b"\0")):
        if index % 256 == 0:
            _check_manifest_deadline(deadline)
        if value:
            paths.add(_decode_path(value))
    _check_manifest_deadline(deadline)
    return paths


def _scoped_manifest(
    repo_root: Path,
    scope: Sequence[str],
    timeout: float,
    manifest_budget_seconds: float,
) -> list[dict[str, Any]]:
    if (
        not isinstance(manifest_budget_seconds, (int, float))
        or isinstance(manifest_budget_seconds, bool)
        or not math.isfinite(manifest_budget_seconds)
        or manifest_budget_seconds <= 0
    ):
        raise _InspectionError(
            "INVALID_MANIFEST_BUDGET",
            "The manifest wall-clock budget must be a positive number.",
        )
    deadline = time.monotonic() + float(manifest_budget_seconds)
    pathspecs = _literal_pathspecs(scope)
    untracked = _manifest_git(
        repo_root,
        ["ls-files", "--others", "--exclude-standard", "-z", "--", *pathspecs],
        timeout,
        "listing scoped untracked files",
        deadline,
    )
    ignored = _manifest_git(
        repo_root,
        [
            "ls-files",
            "--others",
            "--ignored",
            "--exclude-standard",
            "-z",
            "--",
            *pathspecs,
        ],
        timeout,
        "listing scoped ignored files",
        deadline,
    )
    paths = [
        path
        for path in sorted(_manifest_paths(untracked, deadline) | _manifest_paths(ignored, deadline))
        if not is_transient_bytecode_cache(path)
    ]
    manifest: list[dict[str, Any]] = []
    try:
        for relative in paths:
            _check_manifest_deadline(deadline)
            path = repo_root / Path(relative)
            info = path.lstat()
            if stat.S_ISLNK(info.st_mode):
                target = os.readlink(path).encode("utf-8", "surrogatepass")
                manifest.append(
                    {
                        "path": relative,
                        "sha256": hashlib.sha256(target).hexdigest(),
                        "line_count": 0,
                        "kind": "symlink",
                    }
                )
            elif stat.S_ISREG(info.st_mode):
                digest, lines = _hash_file(path, deadline=deadline)
                manifest.append(
                    {
                        "path": relative,
                        "sha256": digest,
                        "line_count": lines,
                        "kind": "file",
                    }
                )
    except OSError as error:
        raise _InspectionError(
            "MANIFEST_IO_FAILED", "A scoped manifest file could not be fingerprinted."
        ) from error
    return manifest


def _diff_fingerprint(repo_root: Path, cached: bool, timeout: float) -> str:
    arguments = ["diff"]
    if cached:
        arguments.append("--cached")
    arguments.extend(["--binary", "--no-ext-diff", "--no-textconv", "--"])
    raw = _run_git(
        repo_root,
        arguments,
        timeout,
        "fingerprinting the staged diff" if cached else "fingerprinting the unstaged diff",
    )
    return hashlib.sha256(raw).hexdigest()


def capture_baseline(
    repo_root: str | os.PathLike[str],
    scope: Iterable[str | os.PathLike[str]] | None,
    *,
    timeout: float = 10.0,
    manifest_budget_seconds: float = DEFAULT_MANIFEST_BUDGET_SECONDS,
) -> dict[str, Any]:
    """Capture the pre-call Git and scoped-file state without storing contents."""

    root = Path(repo_root).resolve()
    try:
        normalized_scope = _normalize_optional_scope(scope)
        head = _decode_path(
            _run_git(root, ["rev-parse", "--verify", "HEAD"], timeout, "reading HEAD")
        ).strip()
        status_entries = _status_entries(root, timeout)
        return {
            "status": OK,
            "code": "BASELINE_CAPTURED",
            "head": head,
            "scope": normalized_scope,
            "manifest_enabled": bool(normalized_scope),
            "status_entries": status_entries,
            "staged_diff_fingerprint": _diff_fingerprint(root, True, timeout),
            "unstaged_diff_fingerprint": _diff_fingerprint(root, False, timeout),
            "tracked_snapshots": _tracked_snapshots(root, status_entries, timeout),
            "scoped_manifest": (
                _scoped_manifest(
                    root,
                    normalized_scope,
                    timeout,
                    manifest_budget_seconds,
                )
                if normalized_scope
                else []
            ),
        }
    except _InspectionError as error:
        return _unknown(error.code, error.message, baseline_head=None, current_head=None)
    except OSError:
        return _unknown(
            "REPOSITORY_IO_FAILED",
            "The repository could not be inspected while capturing the baseline.",
            baseline_head=None,
            current_head=None,
        )


def _parse_name_list(raw: bytes) -> set[str]:
    return {_decode_path(value) for value in raw.split(b"\0") if value}


def _parse_numstat(raw: bytes) -> dict[str, int]:
    counts: dict[str, int] = {}
    for record in raw.split(b"\0"):
        if not record:
            continue
        added, separator, remainder = record.partition(b"\t")
        deleted, separator_two, raw_path = remainder.partition(b"\t")
        if not separator or not separator_two:
            raise _InspectionError("GIT_OUTPUT_INVALID", "Git numstat output was malformed.")
        path = _decode_path(raw_path)
        if added == b"-" or deleted == b"-":
            counts[path] = max(counts.get(path, 0), 1)
        else:
            try:
                counts[path] = counts.get(path, 0) + int(added) + int(deleted)
            except ValueError as error:
                raise _InspectionError(
                    "GIT_OUTPUT_INVALID", "Git numstat counts were malformed."
                ) from error
    return counts


def _diff_stats(
    repo_root: Path,
    transition: Sequence[str],
    timeout: float,
    operation: str,
) -> tuple[set[str], dict[str, int]]:
    common = ["--no-renames", "--no-ext-diff", "--no-textconv"]
    names = _parse_name_list(
        _run_git(
            repo_root,
            ["diff", *transition, *common, "--name-only", "-z", "--"],
            timeout,
            f"listing {operation} paths",
        )
    )
    counts = _parse_numstat(
        _run_git(
            repo_root,
            ["diff", *transition, *common, "--numstat", "-z", "--"],
            timeout,
            f"counting {operation} lines",
        )
    )
    for path in names:
        counts.setdefault(path, 0)
    return names, counts


def _manifest_delta(
    baseline: Sequence[Mapping[str, Any]], current: Sequence[Mapping[str, Any]]
) -> tuple[set[str], dict[str, int]]:
    before = {entry["path"]: entry for entry in baseline}
    after = {entry["path"]: entry for entry in current}
    paths: set[str] = set()
    counts: dict[str, int] = {}
    for path in sorted(set(before) | set(after)):
        old = before.get(path)
        new = after.get(path)
        if old is not None and new is not None and old.get("sha256") == new.get("sha256"):
            continue
        paths.add(path)
        old_lines = old.get("line_count", 0) if old else 0
        new_lines = new.get("line_count", 0) if new else 0
        if not isinstance(old_lines, int) or not isinstance(new_lines, int):
            raise _InspectionError("BASELINE_INVALID", "Manifest line counts were invalid.")
        counts[path] = max(old_lines, new_lines, 1)
    return paths, counts


def _baseline_valid(baseline: Mapping[str, Any]) -> bool:
    return (
        baseline.get("status") == OK
        and isinstance(baseline.get("head"), str)
        and bool(baseline["head"])
        and isinstance(baseline.get("scope"), list)
        and isinstance(baseline.get("manifest_enabled"), bool)
        and isinstance(baseline.get("status_entries"), list)
        and isinstance(baseline.get("tracked_snapshots"), Mapping)
        and isinstance(baseline.get("scoped_manifest"), list)
    )


def compute_delta(
    repo_root: str | os.PathLike[str],
    baseline: Mapping[str, Any],
    scope: Iterable[str | os.PathLike[str]] | None = None,
    *,
    timeout: float = 10.0,
    manifest_budget_seconds: float = DEFAULT_MANIFEST_BUDGET_SECONDS,
) -> dict[str, Any]:
    """Compute the committed, staged, unstaged, and scoped-manifest union.

    Line counts are the conservative sum of each Git transition's numstat.  A
    path is counted once as a file, even when it appears in multiple sources.
    Changed untracked/ignored files use the larger before/after line count.
    """

    if not isinstance(baseline, Mapping) or not _baseline_valid(baseline):
        return _unknown(
            "BASELINE_INVALID",
            "A trustworthy baseline was not supplied.",
            baseline_head=baseline.get("head") if isinstance(baseline, Mapping) else None,
            current_head=None,
        )
    root = Path(repo_root).resolve()
    baseline_head = baseline["head"]
    try:
        normalized_scope = _normalize_optional_scope(
            scope if scope is not None else baseline["scope"]
        )
        if normalized_scope != baseline["scope"]:
            raise _InspectionError(
                "SCOPE_MISMATCH", "The delta scope does not match the captured baseline scope."
            )
        current_head = _decode_path(
            _run_git(root, ["rev-parse", "--verify", "HEAD"], timeout, "reading current HEAD")
        ).strip()
        current_status = _status_entries(root, timeout)
        baseline_snapshots = baseline["tracked_snapshots"]
        current_snapshots = _tracked_snapshots(
            root, current_status, timeout, extra_paths=baseline_snapshots.keys()
        )

        committed_paths, committed_counts = _diff_stats(
            root, [baseline_head, current_head], timeout, "committed delta"
        )
        staged_paths, staged_counts = _diff_stats(root, ["--cached"], timeout, "staged delta")
        unstaged_paths, unstaged_counts = _diff_stats(root, [], timeout, "unstaged delta")

        unchanged_preexisting = {
            path
            for path, snapshot in baseline_snapshots.items()
            if current_snapshots.get(path) == snapshot
        }
        mutated_preexisting = {
            path
            for path, snapshot in baseline_snapshots.items()
            if current_snapshots.get(path) != snapshot
        }
        staged_paths.difference_update(unchanged_preexisting)
        unstaged_paths.difference_update(unchanged_preexisting)
        staged_counts = {path: count for path, count in staged_counts.items() if path in staged_paths}
        unstaged_counts = {
            path: count for path, count in unstaged_counts.items() if path in unstaged_paths
        }

        current_manifest = (
            _scoped_manifest(
                root,
                normalized_scope,
                timeout,
                manifest_budget_seconds,
            )
            if baseline["manifest_enabled"]
            else []
        )
        manifest_paths, manifest_counts = _manifest_delta(
            baseline["scoped_manifest"], current_manifest
        )
        sources = {
            "committed": (committed_paths, committed_counts),
            "staged": (staged_paths, staged_counts),
            "unstaged": (unstaged_paths, unstaged_counts),
            "scoped_manifest": (manifest_paths, manifest_counts),
        }
        changed_paths = sorted(set().union(*(paths for paths, _ in sources.values())))
        path_line_counts = {path: 0 for path in changed_paths}
        source_result: dict[str, dict[str, Any]] = {}
        for name, (paths, counts) in sources.items():
            for path in paths:
                path_line_counts[path] += counts.get(path, 0)
            source_result[name] = {
                "paths": sorted(paths),
                "changed_line_count": sum(counts.get(path, 0) for path in paths),
                "path_line_counts": {path: counts.get(path, 0) for path in paths},
            }

        preexisting_paths = sorted(baseline_snapshots)
        return {
            "status": OK,
            "code": "DELTA_COMPUTED",
            "baseline_head": baseline_head,
            "current_head": current_head,
            "changed_paths": changed_paths,
            "changed_file_count": len(changed_paths),
            "changed_line_count": sum(path_line_counts.values()),
            "path_line_counts": path_line_counts,
            "sources": source_result,
            "preexisting_paths": preexisting_paths,
            "ambiguous_paths": sorted(set(changed_paths).intersection(mutated_preexisting)),
            "current_scoped_manifest": current_manifest,
        }
    except _InspectionError as error:
        return _unknown(
            error.code,
            error.message,
            baseline_head=baseline_head,
            current_head=locals().get("current_head"),
        )
    except (KeyError, TypeError, OSError):
        return _unknown(
            "DELTA_INSPECTION_FAILED",
            "The delta could not be determined trustworthily.",
            baseline_head=baseline_head,
            current_head=locals().get("current_head"),
        )


def _inside(path: str, scope: str) -> bool:
    return scope == "." or path == scope or path.startswith(scope.rstrip("/") + "/")


def attribute_known_concurrent_scopes(
    delta: Mapping[str, Any], own_scope: Iterable[str],
    foreign_owners: Iterable[Mapping[str, Any]], *, protected_paths: Iterable[str] | None = None,
    additional_paths: Iterable[str] = (),
) -> tuple[dict[str, Any], list[dict[str, str]]]:
    """Remove only unambiguous foreign leased paths from the review delta."""
    if delta.get("status") != OK:
        return dict(delta), []
    owners = list(foreign_owners)
    own = _normalize_scope(own_scope)
    ambiguous = set(delta.get("ambiguous_paths", []))
    candidates = sorted(set(delta["changed_paths"]).union(additional_paths))
    protected = set(detect_protected_paths(candidates, protected_paths=protected_paths))
    known = []
    for path in candidates:
        matches = [owner for owner in owners if any(
            _inside(path, item) for item in owner["scope"])]
        if (len(matches) == 1 and path not in ambiguous and path not in protected
                and not any(_inside(path, item) for item in own)):
            known.append({"path": path, "task_id": matches[0]["task_id"]})
    omitted = {item["path"] for item in known}
    result = dict(delta)
    result["changed_paths"] = [path for path in delta["changed_paths"] if path not in omitted]
    result["changed_file_count"] = len(result["changed_paths"])
    result["path_line_counts"] = {path: count for path, count in delta["path_line_counts"].items()
                                  if path not in omitted}
    result["changed_line_count"] = sum(result["path_line_counts"].values())
    result["sources"] = {
        name: {**source, "paths": [path for path in source["paths"] if path not in omitted],
               "changed_line_count": sum(source["path_line_counts"].get(path, 0)
                                         for path in source["paths"] if path not in omitted),
               "path_line_counts": {path: count for path, count in source["path_line_counts"].items()
                                    if path not in omitted}}
        for name, source in delta["sources"].items()
    }
    return result, known


def classify_concurrent_writers(
    delta: Mapping[str, Any],
    delegated_scope: Iterable[str | os.PathLike[str]],
    *,
    material_paths: Iterable[str | os.PathLike[str]] = (),
    verification_paths: Iterable[str | os.PathLike[str]] = (),
    protected_paths: Iterable[str] | None = None,
) -> dict[str, Any]:
    """Classify paths without assigning any path to a particular author."""

    if not isinstance(delta, Mapping) or delta.get("status") != OK:
        return {
            "decision": UNKNOWN_CONCURRENT_WRITER,
            "code": "UNKNOWN_DELTA",
            "concurrent_writer": None,
            "in_scope_paths": None,
            "out_of_scope_paths": None,
            "review_changed_file_count": None,
            "review_changed_line_count": None,
        }
    try:
        scope = _normalize_scope(delegated_scope)
        material_values = list(material_paths)
        verification_values = list(verification_paths)
        material = _normalize_scope(material_values) if material_values else []
        verification = _normalize_scope(verification_values) if verification_values else []
    except _InspectionError as error:
        return {
            "decision": UNKNOWN_CONCURRENT_WRITER,
            "code": error.code,
            "concurrent_writer": None,
            "in_scope_paths": None,
            "out_of_scope_paths": None,
            "review_changed_file_count": None,
            "review_changed_line_count": None,
        }

    changed = list(delta.get("changed_paths", []))
    in_scope = sorted(path for path in changed if any(_inside(path, item) for item in scope))
    outside = sorted(set(changed).difference(in_scope))
    preexisting_outside = sorted(
        path
        for path in delta.get("preexisting_paths", [])
        if not any(_inside(path, item) for item in scope)
    )
    ambiguous = sorted(delta.get("ambiguous_paths", []))
    overlap = sorted(path for path in ambiguous if path in in_scope)
    material_touches = sorted(
        path for path in outside if any(_inside(path, item) or _inside(item, path) for item in material)
    )
    verification_touches = sorted(
        path
        for path in outside
        if any(_inside(path, item) or _inside(item, path) for item in verification)
    )
    protected_touches = detect_protected_paths(changed, protected_paths=protected_paths)
    halt_paths = sorted(
        set(overlap + material_touches + verification_touches + protected_touches + ambiguous)
    )
    line_counts = delta.get("path_line_counts", {})
    decision = UNKNOWN_CONCURRENT_WRITER if halt_paths else PROCEED
    return {
        "decision": decision,
        "code": decision,
        "concurrent_writer": bool(outside),
        "in_scope_paths": in_scope,
        "out_of_scope_paths": outside,
        "preserved_out_of_scope_paths": sorted(set(outside + preexisting_outside)),
        "overlap_paths": overlap,
        "material_boundary_paths": material_touches,
        "verification_boundary_paths": verification_touches,
        "protected_paths": protected_touches,
        "ambiguous_paths": ambiguous,
        "halt_paths": halt_paths,
        "review_changed_file_count": len(in_scope),
        "review_changed_line_count": sum(line_counts.get(path, 0) for path in in_scope),
    }


def detect_protected_paths(
    delta_or_paths: Mapping[str, Any] | Iterable[str],
    *,
    protected_paths: Iterable[str] | None = None,
) -> list[str]:
    """Return protected repository paths present in a known delta."""

    if isinstance(delta_or_paths, Mapping):
        paths = delta_or_paths.get("changed_paths")
        if not isinstance(paths, list):
            return []
    else:
        paths = list(delta_or_paths)
    patterns = (
        project_profile._ENGINE_PROTECTED
        if protected_paths is None
        else protected_paths
    )
    protected = []
    for raw in paths:
        if not isinstance(raw, str):
            continue
        path = raw.replace("\\", "/")
        while path.startswith("./"):
            path = path[2:]
        if project_profile.is_protected(path, patterns):
            protected.append(path)
    return sorted(set(protected))


def detect_out_of_scope_modifications(
    repo_root: str | os.PathLike[str],
    baseline: Mapping[str, Any],
    scope: Iterable[str | os.PathLike[str]] | None,
    *,
    timeout: float = 10.0,
) -> dict[str, Any]:
    """Detect working-tree or committed modifications occurring outside declared scope."""

    if not scope:
        return {"status": OK, "code": "NO_SCOPE_DECLARED", "paths": []}
    if not isinstance(baseline, Mapping) or not _baseline_valid(baseline):
        return {
            "status": UNKNOWN,
            "code": "BASELINE_INVALID",
            "message": "A valid baseline was not provided for out-of-scope inspection.",
            "paths": [],
        }
    root = Path(repo_root).resolve()
    try:
        normalized_scope = _normalize_optional_scope(scope)
    except _InspectionError as error:
        return {
            "status": UNKNOWN,
            "code": error.code,
            "message": error.message,
            "paths": [],
        }
    except Exception as error:
        return {
            "status": UNKNOWN,
            "code": "SCOPE_NORMALIZATION_FAILED",
            "message": str(error),
            "paths": [],
        }

    if not normalized_scope:
        return {"status": OK, "code": "NO_SCOPE_DECLARED", "paths": []}

    out_of_scope: set[str] = set()
    try:
        # 1. Check whole working tree including untracked and staged/unstaged files
        raw_status = _run_git(
            root,
            ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
            timeout,
            "checking whole-tree status for out-of-scope modifications",
        )
        current_status = _parse_status(raw_status)
        baseline_status_map = {
            (entry["path"], entry.get("state")): entry
            for entry in baseline.get("status_entries", [])
        }

        for entry in current_status:
            path = entry["path"]
            state = entry.get("state")
            if (path, state) in baseline_status_map:
                continue
            if not any(_inside(path, s) for s in normalized_scope):
                out_of_scope.add(path)

        # 2. Check committed changes if HEAD moved
        baseline_head = baseline.get("head")
        if baseline_head:
            current_head = _decode_path(
                _run_git(root, ["rev-parse", "--verify", "HEAD"], timeout, "reading current HEAD")
            ).strip()
            if current_head != baseline_head:
                committed_paths, _ = _diff_stats(
                    root, [baseline_head, current_head], timeout, "committed out-of-scope check"
                )
                for path in committed_paths:
                    if not any(_inside(path, s) for s in normalized_scope):
                        out_of_scope.add(path)
        return {
            "status": OK,
            "code": "OUT_OF_SCOPE_CHECK_COMPLETED",
            "paths": sorted(out_of_scope),
        }
    except _InspectionError as error:
        return {
            "status": UNKNOWN,
            "code": error.code,
            "message": error.message,
            "paths": [],
        }
    except Exception as error:
        return {
            "status": UNKNOWN,
            "code": "OUT_OF_SCOPE_INSPECTION_FAILED",
            "message": str(error),
            "paths": [],
        }



def build_session_record(
    task_id: str,
    thread_id: str | None,
    route: str,
    model: str,
    effort: str,
    *,
    phase: str | None = None,
    scope: Iterable[str | os.PathLike[str]] = (),
    pending_call_baseline: Mapping[str, Any] | None = None,
    last_outcome: Any = None,
    task_objective: str | None = None,
    acceptance_criteria: str | None = None,
    task_contract_grounded: bool = False,
    reason: str | None = None,
    profile_checks: list[dict[str, Any]] | None = None,
    origin_baseline: Mapping[str, Any] | None = None,
    origin_scope: Iterable[str | os.PathLike[str]] = (),
    post_state: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """Build the strict record shape; ``live_thread`` feeds policy directly."""

    live_thread = None
    if thread_id is not None:
        live_thread = {
            "task_id": task_id,
            "thread_id": thread_id,
            "route": route,
            "model": model,
            "effort": effort,
        }
        if reason is not None:
            live_thread["reason"] = reason
        if phase is not None:
            live_thread["phase"] = phase
            live_thread["scope"] = [os.fspath(path) for path in scope]
        if task_objective is not None:
            live_thread["task_objective"] = task_objective
        if acceptance_criteria is not None:
            live_thread["acceptance_criteria"] = acceptance_criteria
        live_thread["task_contract_grounded"] = task_contract_grounded
        if profile_checks is not None:
            live_thread["profile_checks"] = profile_checks
        if origin_baseline is not None and post_state is not None:
            live_thread["origin_baseline"] = dict(origin_baseline)
            live_thread["origin_scope"] = [os.fspath(path) for path in origin_scope]
            live_thread["post_state"] = dict(post_state)
    return {
        "task_id": task_id,
        "live_thread": live_thread,
        "pending_call_baseline": pending_call_baseline,
        "last_outcome": last_outcome,
    }


def _canonical_json(value: Any) -> bytes:
    return json_transport.dumps(value, sort_keys=True, separators=(",", ":")).encode(
        "utf-8"
    )


def _secret_violation(value: Any, *, _location: tuple[str, ...] = ()) -> tuple[str, str] | None:
    if isinstance(value, Mapping):
        path_keyed = _location in _PATH_KEYED_RECORD_LOCATIONS
        for item_key, item in value.items():
            key = str(item_key)
            if any(pattern.search(key) for pattern in _SECRET_VALUE_PATTERNS):
                return (".".join(_location) or "record", "SECRET_PATTERN_IN_FIELD_NAME")
            normalized_key = key.casefold().replace("-", "_")
            if not path_keyed and any(part in normalized_key for part in _SECRET_KEY_PARTS):
                return (".".join((*_location, key)), "SECRET_FIELD_NAME")
            nested = _secret_violation(item, _location=(*_location, key))
            if nested:
                return nested
        return None
    if isinstance(value, (list, tuple)):
        for item in value:
            nested = _secret_violation(item, _location=_location)
            if nested:
                return nested
        return None
    if isinstance(value, str):
        if any(pattern.search(value) for pattern in _SECRET_VALUE_PATTERNS):
            return (".".join(_location) or "record", "SECRET_VALUE_PATTERN")
    return None


def _contains_secret(value: Any, *, _location: tuple[str, ...] = ()) -> bool:
    return _secret_violation(value, _location=_location) is not None


def _valid_session_record(record: Any, expected_task_id: str | None = None) -> bool:
    if not isinstance(record, Mapping) or set(record) != {
        "task_id",
        "live_thread",
        "pending_call_baseline",
        "last_outcome",
    }:
        return False
    task_id = record.get("task_id")
    if not isinstance(task_id, str) or not task_id or (
        expected_task_id is not None and task_id != expected_task_id
    ):
        return False
    thread = record.get("live_thread")
    if thread is None:
        pending = record.get("pending_call_baseline")
        if pending is not None and not isinstance(pending, Mapping):
            return False
        try:
            _canonical_json(record)
        except (TypeError, ValueError):
            return False
        return not _contains_secret(record)
    thread_keys = set(thread)
    required_keys = {"task_id", "thread_id", "route", "model", "effort"}
    allowed_keys = required_keys | {
        "phase",
        "scope",
        "task_objective",
        "acceptance_criteria",
        "task_contract_grounded",
        "reason",
        "profile_checks",
        "origin_baseline",
        "origin_scope",
        "post_state",
    }
    if not required_keys.issubset(thread_keys) or not thread_keys.issubset(allowed_keys):
        return False
    if thread.get("task_id") != task_id or any(
        not isinstance(thread.get(field), str) or not thread.get(field)
        for field in ("task_id", "thread_id", "route", "model", "effort")
    ):
        return False
    if "phase" in thread:
        if thread.get("phase") not in {"implementation", "operation", "plan"}:
            return False
        thread_scope = thread.get("scope")
        if not isinstance(thread_scope, list) or any(
            not isinstance(path, str) or not path for path in thread_scope
        ):
            return False
        if thread["phase"] == "implementation" and not thread_scope:
            return False
    if "task_objective" in thread and not isinstance(thread.get("task_objective"), str):
        return False
    if "acceptance_criteria" in thread and not isinstance(thread.get("acceptance_criteria"), str):
        return False
    if "task_contract_grounded" in thread and not isinstance(thread.get("task_contract_grounded"), bool):
        return False
    if "reason" in thread and thread.get("reason") not in {
        "new_task", "continue", "route_change", "reanchor", "repeated_failure", "user_override"
    }:
        return False
    if "profile_checks" in thread and not isinstance(thread.get("profile_checks"), list):
        return False
    if thread.get("task_contract_grounded") is True:
        if not isinstance(thread.get("task_objective"), str) or not thread.get("task_objective", "").strip():
            return False
        if not isinstance(thread.get("acceptance_criteria"), str) or not thread.get("acceptance_criteria", "").strip():
            return False
    handoff_fields = {"origin_baseline", "origin_scope", "post_state"}
    present_handoff = handoff_fields.intersection(thread)
    if present_handoff and present_handoff != handoff_fields:
        return False
    if present_handoff:
        origin_baseline = thread.get("origin_baseline")
        post_state = thread.get("post_state")
        origin_scope = thread.get("origin_scope")
        if (
            not isinstance(origin_baseline, Mapping)
            or not _baseline_valid(origin_baseline)
            or not isinstance(post_state, Mapping)
            or not _baseline_valid(post_state)
            or origin_baseline.get("status") != OK
            or post_state.get("status") != OK
            or not isinstance(origin_scope, list)
            or any(not isinstance(path, str) or not path for path in origin_scope)
            or origin_scope != origin_baseline.get("scope")
            or origin_scope != post_state.get("scope")
        ):
            return False
    pending = record.get("pending_call_baseline")
    if pending is not None and not isinstance(pending, Mapping):
        return False
    try:
        _canonical_json(record)
    except (TypeError, ValueError):
        return False
    return not _contains_secret(record)


class SessionLedger:
    """Checksummed, atomic per-task session persistence."""

    def __init__(self, root: str | os.PathLike[str] | None = None) -> None:
        if root is None:
            root = purser_state_root.default_state_root()
        self.root = Path(root)
        self.sessions_root = self.root / "sessions"

    def path_for_task(self, task_id: str) -> Path:
        if not isinstance(task_id, str) or not task_id:
            raise ValueError("task_id must be a non-empty string")
        name = hashlib.sha256(task_id.encode("utf-8")).hexdigest() + ".json"
        return self.sessions_root / name

    def save(self, record: Mapping[str, Any]) -> dict[str, Any]:
        task_id = record.get("task_id") if isinstance(record, Mapping) else None
        if not isinstance(task_id, str) or not _valid_session_record(record, task_id):
            violation = _secret_violation(record)
            detail = (f"{violation[1]} at {violation[0]}" if violation
                      else "INVALID_RECORD_STRUCTURE")
            return {
                "status": UNKNOWN,
                "code": "LEDGER_RECORD_INVALID",
                "message": f"The session record was rejected: {detail}.",
            }
        payload = dict(record)
        payload_bytes = _canonical_json(payload)
        envelope = {
            "schema_version": _LEDGER_SCHEMA_VERSION,
            "checksum": hashlib.sha256(payload_bytes).hexdigest(),
            "payload": payload,
        }
        target = self.path_for_task(task_id)
        temporary: Path | None = None
        try:
            self.sessions_root.mkdir(parents=True, exist_ok=True)
            with tempfile.NamedTemporaryFile(
                mode="wb", prefix=target.name + ".", suffix=".tmp", dir=self.sessions_root, delete=False
            ) as handle:
                temporary = Path(handle.name)
                handle.write(_canonical_json(envelope))
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, target)
            temporary = None
            return {"status": OK, "code": "LEDGER_WRITTEN"}
        except OSError:
            return {
                "status": UNKNOWN,
                "code": "LEDGER_WRITE_FAILED",
                "message": "The session record could not be written atomically.",
            }
        finally:
            if temporary is not None:
                try:
                    temporary.unlink()
                except OSError:
                    pass

    write = save

    def load(self, task_id: str) -> dict[str, Any]:
        try:
            target = self.path_for_task(task_id)
            raw = target.read_bytes()
        except FileNotFoundError:
            return {"status": MISSING, "code": "LEDGER_RECORD_MISSING", "record": None}
        except (OSError, ValueError):
            return {"status": CORRUPT, "code": "LEDGER_CORRUPT", "record": None}
        try:
            envelope = json.loads(raw.decode("utf-8"))
            if not isinstance(envelope, Mapping) or set(envelope) != {
                "schema_version",
                "checksum",
                "payload",
            }:
                raise ValueError
            if envelope["schema_version"] != _LEDGER_SCHEMA_VERSION:
                raise ValueError
            payload = envelope["payload"]
            expected = hashlib.sha256(_canonical_json(payload)).hexdigest()
            if not isinstance(envelope["checksum"], str) or not hmac.compare_digest(
                envelope["checksum"], expected
            ):
                raise ValueError
            if not _valid_session_record(payload, task_id):
                raise ValueError
        except (UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError):
            return {"status": CORRUPT, "code": "LEDGER_CORRUPT", "record": None}
        return {"status": OK, "code": "LEDGER_LOADED", "record": payload}

    read = load

    def find_by_thread_id(self, thread_id: str) -> dict[str, Any] | None:
        """Find one valid session record by its live thread identity."""

        if not isinstance(thread_id, str) or not thread_id:
            return None
        matches: list[dict[str, Any]] = []
        try:
            paths = sorted(self.sessions_root.glob("*.json"))
        except OSError:
            return None
        for target in paths:
            try:
                raw = target.read_bytes()
                envelope = json.loads(raw.decode("utf-8"))
                if not isinstance(envelope, Mapping) or set(envelope) != {
                    "schema_version",
                    "checksum",
                    "payload",
                }:
                    continue
                if envelope["schema_version"] != _LEDGER_SCHEMA_VERSION:
                    continue
                payload = envelope["payload"]
                expected = hashlib.sha256(_canonical_json(payload)).hexdigest()
                if not isinstance(envelope["checksum"], str) or not hmac.compare_digest(
                    envelope["checksum"], expected
                ):
                    continue
                task_id = payload.get("task_id") if isinstance(payload, Mapping) else None
                if not isinstance(task_id, str) or not _valid_session_record(payload, task_id):
                    continue
                thread = payload.get("live_thread")
                if isinstance(thread, Mapping) and thread.get("thread_id") == thread_id:
                    matches.append(dict(payload))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError):
                continue
        return matches[0] if len(matches) == 1 else None


__all__ = [
    "CORRUPT",
    "DEFAULT_MANIFEST_BUDGET_SECONDS",
    "MANIFEST_BUDGET_EXCEEDED",
    "MISSING",
    "OK",
    "PROCEED",
    "SessionLedger",
    "UNKNOWN",
    "UNKNOWN_CONCURRENT_WRITER",
    "build_session_record",
    "capture_baseline",
    "classify_concurrent_writers",
    "compute_delta",
    "detect_protected_paths",
    "is_transient_bytecode_cache",
]
