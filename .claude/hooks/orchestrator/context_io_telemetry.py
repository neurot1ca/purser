"""Bounded, passive telemetry for structured Claude Read calls in one turn.

The ephemeral journal is deliberately a closed numeric/enum structure.  Paths
are used only while validating a single hook input and are never written to the
journal.  Filesystem identity is retained only long enough to detect repeats
within the current turn and is stripped before canonical telemetry emission.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import time
from typing import Any, Iterator
import uuid

import project_profile


SCHEMA_VERSION = 1
MAX_JOURNAL_AGE_SECONDS = 6 * 60 * 60
MAX_OBSERVATIONS_PER_TURN = 128
MAX_JOURNAL_BYTES = 64 * 1024
MAX_STAT_OPERATIONS_PER_TURN = 160
MAX_BYTES_SCANNED_PER_READ = 256 * 1024
MAX_BYTES_SCANNED_PER_TURN = 2 * 1024 * 1024
MAX_LINES_SCANNED_PER_READ = 501
MAX_LINES_SCANNED_PER_TURN = 8 * MAX_LINES_SCANNED_PER_READ
READ_CHUNK_BYTES = 64 * 1024
LOCK_WAIT_SECONDS = 0.05
ATOMIC_REPLACE_MAX_ATTEMPTS = 4
ATOMIC_REPLACE_MAX_WAIT_SECONDS = 0.03
THRESHOLDS = (200, 350, 500)

_UNKNOWN_REASONS = frozenset(
    {
        "BINARY_SUSPECTED",
        "BOUNDED_SCAN",
        "CORRUPT_STATE",
        "DIRECTORY",
        "INVALID_INPUT",
        "JOURNAL_LIMIT",
        "MISSING_FILE",
        "MISSING_USER_PROMPT",
        "OBSERVATION_LIMIT",
        "OUTSIDE_ROOT",
        "PRIOR_TURN_MISSING_STOP",
        "PROTECTED_PATH",
        "RESOURCE_LIMIT",
        "STALE_STATE",
        "STATE_CONTENTION",
        "STAT_UNAVAILABLE",
    }
)
_UNCLASSIFIABLE_READ_REASONS = frozenset(
    {
        "DIRECTORY",
        "INVALID_INPUT",
        "MISSING_FILE",
        "OUTSIDE_ROOT",
        "PROTECTED_PATH",
        "RESOURCE_LIMIT",
        "STAT_UNAVAILABLE",
    }
)


def _runtime_root(state_root: str | os.PathLike[str] | None) -> Path:
    if state_root is not None:
        return Path(state_root)
    local_app_data = os.environ.get("LOCALAPPDATA")
    if not local_app_data:
        raise OSError("runtime state root unavailable")
    return Path(local_app_data) / "claude-agent-orchestrator"


def _session_key(value: Any) -> str | None:
    if not isinstance(value, str) or not value or len(value) > 512:
        return None
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _paths(root: Path, key: str) -> tuple[Path, Path, Path, Path]:
    base = root / "context_io"
    return (
        base / "turns" / f"{key}.json",
        base / "locks" / f"{key}.lock",
        base / "contention" / key,
        base / "completed" / key,
    )


@contextmanager
def _session_lock(path: Path) -> Iterator[bool]:
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = path.open("a+b")
    acquired = False
    deadline = time.monotonic() + LOCK_WAIT_SECONDS
    try:
        if handle.seek(0, os.SEEK_END) == 0:
            handle.write(b"0")
            handle.flush()
        while time.monotonic() < deadline:
            handle.seek(0)
            try:
                if os.name == "nt":
                    import msvcrt

                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                acquired = True
                break
            except (OSError, BlockingIOError):
                time.sleep(0.005)
        yield acquired
    finally:
        if acquired:
            try:
                handle.seek(0)
                if os.name == "nt":
                    import msvcrt

                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
        handle.close()


def _atomic_json(path: Path, value: Mapping[str, Any]) -> None:
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8")
    if len(encoded) > MAX_JOURNAL_BYTES:
        raise ValueError("journal bound exceeded")
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", prefix=path.name + ".", suffix=".tmp", dir=path.parent, delete=False
        ) as handle:
            temporary = Path(handle.name)
            handle.write(encoded)
            handle.flush()
            os.fsync(handle.fileno())
        _replace_with_retry(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass


def _replace_with_retry(source: Path, destination: Path) -> None:
    deadline = time.monotonic() + ATOMIC_REPLACE_MAX_WAIT_SECONDS
    for attempt in range(ATOMIC_REPLACE_MAX_ATTEMPTS):
        try:
            os.replace(source, destination)
            return
        except PermissionError:
            if attempt + 1 >= ATOMIC_REPLACE_MAX_ATTEMPTS:
                raise
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise
            time.sleep(min(0.005 * (2**attempt), remaining))


def _write_turn(
    journal: Path,
    turn: Mapping[str, Any],
    contention: Path,
) -> bool:
    try:
        _atomic_json(journal, turn)
    except OSError:
        _mark_contention(contention)
        return False
    return True


def _new_turn(reason: str | None = None) -> dict[str, Any]:
    return {
        "schema_version": SCHEMA_VERSION,
        "turn_nonce": uuid.uuid4().hex,
        "opened_at": time.time(),
        "observations": [],
        "read_count": 0,
        "full_read_count": 0,
        "targeted_read_count": 0,
        "unknown_read_count": 0,
        "stat_operations": 0,
        "bytes_scanned": 0,
        "lines_scanned": 0,
        "task_association_known": False,
        "task_id": None,
        "codex_boundary_index": None,
        "unknown_reason": reason,
        "latency": [],
    }


def _valid_turn(value: Any) -> bool:
    if not isinstance(value, Mapping) or value.get("schema_version") != SCHEMA_VERSION:
        return False
    required = {
        "turn_nonce": str,
        "opened_at": (int, float),
        "observations": list,
        "read_count": int,
        "full_read_count": int,
        "targeted_read_count": int,
        "unknown_read_count": int,
        "stat_operations": int,
        "bytes_scanned": int,
        "lines_scanned": int,
        "task_association_known": bool,
        "latency": list,
    }
    for name, expected in required.items():
        item = value.get(name)
        if not isinstance(item, expected) or isinstance(item, bool) and expected is int:
            return False
    if len(value["turn_nonce"]) != 32 or len(value["observations"]) > MAX_OBSERVATIONS_PER_TURN:
        return False
    if any(value[name] < 0 for name in (
        "read_count", "full_read_count", "targeted_read_count", "unknown_read_count",
        "stat_operations", "bytes_scanned", "lines_scanned",
    )):
        return False
    if value.get("final_event") is not None and not isinstance(
        value.get("final_event"), Mapping
    ):
        return False
    return True


def _load_turn(path: Path) -> tuple[dict[str, Any] | None, str | None]:
    try:
        if path.stat().st_size > MAX_JOURNAL_BYTES:
            return None, "CORRUPT_STATE"
        value = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None, None
    except (OSError, UnicodeError, json.JSONDecodeError):
        return None, "CORRUPT_STATE"
    if not _valid_turn(value):
        return None, "CORRUPT_STATE"
    turn = dict(value)
    if time.time() - float(turn["opened_at"]) > MAX_JOURNAL_AGE_SECONDS:
        return None, "STALE_STATE"
    return turn, None


def _set_reason(turn: dict[str, Any], reason: str | None) -> None:
    if reason in _UNKNOWN_REASONS and turn.get("unknown_reason") is None:
        turn["unknown_reason"] = reason


def _mark_contention(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.close(descriptor)
    except FileExistsError:
        pass
    except OSError:
        pass


def _consume_contention(turn: dict[str, Any], path: Path) -> None:
    if path.exists():
        _set_reason(turn, "STATE_CONTENTION")
        try:
            path.unlink()
        except OSError:
            pass


def _record_latency(turn: dict[str, Any], internal_us: int, wall_us: int) -> None:
    latency = turn.get("latency")
    if not isinstance(latency, list):
        _set_reason(turn, "CORRUPT_STATE")
        return
    if len(latency) < MAX_OBSERVATIONS_PER_TURN + 2:
        latency.append([max(0, internal_us), max(0, wall_us)])


def _mode(tool_input: Any) -> str:
    if not isinstance(tool_input, Mapping):
        return "UNKNOWN"
    return "TARGETED" if "offset" in tool_input or "limit" in tool_input else "FULL"


def _is_protected(relative: Path, protected_paths: Iterable[str] | None = None) -> bool:
    patterns = (
        project_profile._ENGINE_PROTECTED
        if protected_paths is None
        else protected_paths
    )
    return project_profile.is_protected(relative.as_posix(), patterns)


def _identity(file_stat: os.stat_result) -> list[int] | None:
    device = getattr(file_stat, "st_dev", None)
    inode = getattr(file_stat, "st_ino", None)
    if not isinstance(device, int) or not isinstance(inode, int) or inode <= 0:
        return None
    return [device, inode]


def _scan_lines(path: Path, *, byte_budget: int, line_budget: int) -> dict[str, Any]:
    scanned = 0
    newline_count = 0
    last_byte: int | None = None
    eof = False
    binary = False
    with path.open("rb") as handle:
        while scanned < byte_budget and newline_count < line_budget:
            request = min(READ_CHUNK_BYTES, byte_budget - scanned)
            chunk = handle.read(request)
            if not chunk:
                eof = True
                break
            scanned += len(chunk)
            if b"\0" in chunk:
                binary = True
                break
            allowed_newlines = line_budget - newline_count
            chunk_newlines = chunk.count(b"\n")
            if chunk_newlines > allowed_newlines:
                position = -1
                for _ in range(allowed_newlines):
                    position = chunk.find(b"\n", position + 1)
                newline_count = line_budget
                last_byte = 10
                break
            newline_count += chunk_newlines
            last_byte = chunk[-1]
            if len(chunk) < request:
                eof = True
                break
        if not binary and not eof and scanned < byte_budget and newline_count < line_budget:
            eof = handle.read(1) == b""

    if binary:
        return {"kind": "UNKNOWN", "value": None, "binary": True, "bytes": scanned, "lines": newline_count}
    value = newline_count + int(eof and scanned > 0 and last_byte != 10)
    if eof:
        return {"kind": "EXACT", "value": value, "binary": False, "bytes": scanned, "lines": value}
    return {
        "kind": "LOWER_BOUND",
        "value": max(value, newline_count),
        "binary": False,
        "bytes": scanned,
        "lines": max(value, newline_count),
    }


def _target_range(tool_input: Mapping[str, Any]) -> tuple[int, int | None] | None:
    offset = tool_input.get("offset", 1)
    limit = tool_input.get("limit")
    if (
        not isinstance(offset, int)
        or isinstance(offset, bool)
        or offset < 1
        or limit is not None
        and (not isinstance(limit, int) or isinstance(limit, bool) or limit < 1)
    ):
        return None
    return offset, limit


def _scan_targeted_lines(
    path: Path,
    *,
    offset: int,
    limit: int | None,
    byte_budget: int,
    line_budget: int,
) -> dict[str, Any]:
    scanned_bytes = 0
    scanned_lines = 0
    current_line = 1
    selected_bytes = 0
    selected_lines = 0
    last_byte: int | None = None
    eof = False
    complete = False
    binary = False
    with path.open("rb") as handle:
        while scanned_bytes < byte_budget and scanned_lines < line_budget:
            request = min(READ_CHUNK_BYTES, byte_budget - scanned_bytes)
            chunk = handle.read(request)
            if not chunk:
                eof = True
                break
            scanned_bytes += len(chunk)
            if b"\0" in chunk:
                binary = True
                break
            last_byte = chunk[-1]
            for value in chunk:
                selected = current_line >= offset and (
                    limit is None or selected_lines < limit
                )
                if selected:
                    selected_bytes += 1
                if value != 10:
                    continue
                scanned_lines += 1
                if selected:
                    selected_lines += 1
                current_line += 1
                if limit is not None and selected_lines >= limit:
                    complete = True
                    break
                if scanned_lines >= line_budget:
                    break
            if complete or scanned_lines >= line_budget:
                break
            if len(chunk) < request:
                eof = True
                break

    if eof and last_byte is not None and last_byte != 10:
        scanned_lines += 1
        if current_line >= offset:
            selected_lines += 1
    if binary:
        return {
            "kind": "UNKNOWN", "value": None, "binary": True,
            "bytes": scanned_bytes, "lines": scanned_lines, "observed_bytes": None,
        }
    if complete or eof:
        return {
            "kind": "EXACT", "value": selected_lines, "binary": False,
            "bytes": scanned_bytes, "lines": scanned_lines,
            "observed_bytes": selected_bytes,
        }
    return {
        "kind": "UNKNOWN", "value": None, "binary": False,
        "bytes": scanned_bytes, "lines": scanned_lines, "observed_bytes": None,
    }


def _unknown_observation(mode: str, reason: str) -> dict[str, Any]:
    return {
        "mode": mode,
        "bytes": None,
        "line_kind": "UNKNOWN",
        "line_value": None,
        "oversized": False,
        "binary_suspected": False,
        "thresholds": {str(threshold): None for threshold in THRESHOLDS},
        "identity": None,
        "repeat": None,
        "reason": reason,
    }


def _observe_read(
    turn: dict[str, Any],
    event: Mapping[str, Any],
    *,
    repo_root: str | os.PathLike[str] | None,
    protected_paths: Iterable[str] | None,
) -> dict[str, Any]:
    tool_input = event.get("tool_input")
    mode = _mode(tool_input)
    if not isinstance(tool_input, Mapping):
        return _unknown_observation(mode, "INVALID_INPUT")
    raw_path = tool_input.get("file_path")
    if not isinstance(raw_path, str) or not raw_path or len(raw_path) > 4096 or "\0" in raw_path:
        return _unknown_observation(mode, "INVALID_INPUT")
    if not isinstance(repo_root, (str, os.PathLike)):
        return _unknown_observation(mode, "INVALID_INPUT")
    try:
        root = Path(repo_root).resolve(strict=True)
        requested = Path(raw_path)
        candidate = (requested if requested.is_absolute() else root / requested).resolve(strict=False)
        relative = candidate.relative_to(root)
    except (OSError, RuntimeError, ValueError):
        return _unknown_observation(mode, "OUTSIDE_ROOT")
    if _is_protected(relative, protected_paths):
        return _unknown_observation(mode, "PROTECTED_PATH")
    if turn["stat_operations"] >= MAX_STAT_OPERATIONS_PER_TURN:
        return _unknown_observation(mode, "RESOURCE_LIMIT")
    turn["stat_operations"] += 1
    try:
        file_stat = candidate.stat()
    except FileNotFoundError:
        return _unknown_observation(mode, "MISSING_FILE")
    except OSError:
        return _unknown_observation(mode, "STAT_UNAVAILABLE")
    if not stat.S_ISREG(file_stat.st_mode):
        return _unknown_observation(mode, "DIRECTORY")

    observation = _unknown_observation(mode, "BOUNDED_SCAN")
    observation["identity"] = _identity(file_stat)
    if mode == "FULL":
        observation["bytes"] = max(0, int(file_stat.st_size))
        observation["oversized"] = file_stat.st_size > MAX_BYTES_SCANNED_PER_READ
    byte_budget = min(
        MAX_BYTES_SCANNED_PER_READ,
        max(0, MAX_BYTES_SCANNED_PER_TURN - turn["bytes_scanned"]),
    )
    line_budget = min(
        MAX_LINES_SCANNED_PER_READ,
        max(0, MAX_LINES_SCANNED_PER_TURN - turn["lines_scanned"]),
    )
    if byte_budget <= 0 or line_budget <= 0:
        observation["reason"] = "RESOURCE_LIMIT"
        return observation
    try:
        if mode == "TARGETED":
            target_range = _target_range(tool_input)
            if target_range is None:
                observation["reason"] = "INVALID_INPUT"
                return observation
            result = _scan_targeted_lines(
                candidate,
                offset=target_range[0],
                limit=target_range[1],
                byte_budget=byte_budget,
                line_budget=line_budget,
            )
            observation["bytes"] = result["observed_bytes"]
        else:
            result = _scan_lines(candidate, byte_budget=byte_budget, line_budget=line_budget)
    except OSError:
        observation["reason"] = "STAT_UNAVAILABLE"
        return observation
    turn["bytes_scanned"] += result["bytes"]
    turn["lines_scanned"] += result["lines"]
    observation["line_kind"] = result["kind"]
    observation["line_value"] = result["value"]
    observation["binary_suspected"] = result["binary"]
    if result["binary"]:
        observation["reason"] = "BINARY_SUSPECTED"
    elif result["kind"] == "EXACT":
        observation["reason"] = None
    for threshold in THRESHOLDS:
        key = str(threshold)
        if result["kind"] == "EXACT":
            observation["thresholds"][key] = result["value"] > threshold
        elif result["kind"] == "LOWER_BOUND" and result["value"] > threshold:
            observation["thresholds"][key] = True
    return observation


def _add_observation(turn: dict[str, Any], observation: dict[str, Any]) -> None:
    mode = observation["mode"]
    turn["read_count"] += 1
    if observation.get("reason") in _UNCLASSIFIABLE_READ_REASONS:
        bucket = "unknown_read_count"
    else:
        bucket = {"FULL": "full_read_count", "TARGETED": "targeted_read_count"}.get(
            mode, "unknown_read_count"
        )
    turn[bucket] += 1
    observations = turn["observations"]
    if len(observations) >= MAX_OBSERVATIONS_PER_TURN:
        _set_reason(turn, "OBSERVATION_LIMIT")
        return
    identity = observation.get("identity")
    if identity is not None:
        prior = {tuple(item["identity"]) for item in observations if item.get("identity") is not None}
        observation["repeat"] = tuple(identity) in prior
    observations.append(observation)
    _set_reason(turn, observation.get("reason"))


def user_prompt_submit(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
    hook_started_ns: int | None = None,
) -> None:
    started = time.perf_counter_ns()
    key = _session_key(event.get("session_id"))
    if key is None:
        return
    root = _runtime_root(state_root)
    journal, lock, contention, completed = _paths(root, key)
    with _session_lock(lock) as acquired:
        if not acquired:
            _mark_contention(contention)
            return
        prior, prior_error = _load_turn(journal)
        reason = prior_error
        if prior is not None:
            reason = "PRIOR_TURN_MISSING_STOP"
        turn = _new_turn(reason)
        _consume_contention(turn, contention)
        try:
            completed.unlink()
        except FileNotFoundError:
            pass
        internal_us = (time.perf_counter_ns() - started) // 1000
        wall_us = (time.perf_counter_ns() - (hook_started_ns or started)) // 1000
        _record_latency(turn, internal_us, wall_us)
        _write_turn(journal, turn, contention)


def read_pre_tool_use(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    protected_paths: Iterable[str] | None = None,
    hook_started_ns: int | None = None,
) -> None:
    started = time.perf_counter_ns()
    key = _session_key(event.get("session_id"))
    if key is None:
        return
    root = _runtime_root(state_root)
    journal, lock, contention, _completed = _paths(root, key)
    with _session_lock(lock) as acquired:
        if not acquired:
            _mark_contention(contention)
            return
        turn, error = _load_turn(journal)
        if turn is None:
            turn = _new_turn(error or "MISSING_USER_PROMPT")
        _consume_contention(turn, contention)
        observation = _observe_read(
            turn, event, repo_root=repo_root, protected_paths=protected_paths
        )
        _add_observation(turn, observation)
        internal_us = (time.perf_counter_ns() - started) // 1000
        wall_us = (time.perf_counter_ns() - (hook_started_ns or started)) // 1000
        _record_latency(turn, internal_us, wall_us)
        try:
            _write_turn(journal, turn, contention)
        except ValueError:
            turn["observations"] = turn["observations"][: MAX_OBSERVATIONS_PER_TURN // 2]
            _set_reason(turn, "JOURNAL_LIMIT")
            _write_turn(journal, turn, contention)


def associate_turn(
    state_root: str | os.PathLike[str],
    *,
    session_id: Any,
    task_id: Any,
) -> bool:
    key = _session_key(session_id)
    if key is None or not isinstance(task_id, str) or not task_id or len(task_id) > 128:
        return False
    root = _runtime_root(state_root)
    journal, lock, contention, _completed = _paths(root, key)
    with _session_lock(lock) as acquired:
        if not acquired:
            _mark_contention(contention)
            return False
        turn, error = _load_turn(journal)
        if turn is None:
            return False
        _consume_contention(turn, contention)
        if error is not None or turn.get("task_association_known") is True:
            return turn.get("task_id") == task_id
        turn["task_association_known"] = True
        turn["task_id"] = task_id
        turn["codex_boundary_index"] = turn["read_count"]
        return _write_turn(journal, turn, contention)


def _summary(turn: Mapping[str, Any], *, stop_internal_us: int, stop_wall_us: int) -> dict[str, Any]:
    observations = turn.get("observations") if isinstance(turn.get("observations"), list) else []
    payload: dict[str, Any] = {
        "read_count": turn.get("read_count", 0),
        "full_read_count": turn.get("full_read_count", 0),
        "targeted_read_count": turn.get("targeted_read_count", 0),
        "unknown_read_count": turn.get("unknown_read_count", 0),
        "observed_bytes_total": sum(item["bytes"] for item in observations if isinstance(item.get("bytes"), int)),
        "exact_line_read_count": 0,
        "exact_line_count_total": 0,
        "lower_bound_line_read_count": 0,
        "lower_bound_line_count_total": 0,
        "oversized_read_count": 0,
        "binary_suspected_read_count": 0,
        "repeat_count": sum(item.get("repeat") is True for item in observations),
        "repeat_known": all(item.get("repeat") is not None for item in observations),
        "task_association_known": turn.get("task_association_known") is True,
    }
    for threshold in THRESHOLDS:
        values = [item.get("thresholds", {}).get(str(threshold)) for item in observations]
        payload[f"threshold_{threshold}_known_count"] = sum(isinstance(value, bool) for value in values)
        payload[f"threshold_{threshold}_candidate_count"] = sum(value is True for value in values)
    for item in observations:
        value = item.get("line_value")
        if item.get("line_kind") == "EXACT" and isinstance(value, int):
            payload["exact_line_read_count"] += 1
            payload["exact_line_count_total"] += value
        elif item.get("line_kind") == "LOWER_BOUND" and isinstance(value, int):
            payload["lower_bound_line_read_count"] += 1
            payload["lower_bound_line_count_total"] += value
        payload["oversized_read_count"] += int(item.get("oversized") is True)
        payload["binary_suspected_read_count"] += int(item.get("binary_suspected") is True)

    latency = [item for item in turn.get("latency", []) if isinstance(item, list) and len(item) == 2]
    latency.append([max(0, stop_internal_us), max(0, stop_wall_us)])
    payload["hook_sample_count"] = len(latency)
    payload["internal_processing_us_total"] = sum(int(item[0]) for item in latency)
    payload["internal_processing_us_max"] = max((int(item[0]) for item in latency), default=0)
    payload["hook_wall_time_us_total"] = sum(int(item[1]) for item in latency)
    payload["hook_wall_time_us_max"] = max((int(item[1]) for item in latency), default=0)

    boundary = turn.get("codex_boundary_index")
    if turn.get("task_association_known") is True and isinstance(boundary, int):
        payload["pre_codex_read_count"] = min(boundary, payload["read_count"])
        payload["post_codex_read_count"] = max(0, payload["read_count"] - boundary)
    reason = turn.get("unknown_reason")
    if reason in _UNKNOWN_REASONS:
        payload["unknown_reason"] = reason
    return payload


def stop(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
    hook_started_ns: int | None = None,
) -> None:
    started = time.perf_counter_ns()
    if event.get("stop_hook_active") is True:
        return
    key = _session_key(event.get("session_id"))
    if key is None:
        return
    root = _runtime_root(state_root)
    journal, lock, contention, completed = _paths(root, key)
    with _session_lock(lock) as acquired:
        if not acquired:
            _mark_contention(contention)
            return
        if completed.exists() and not journal.exists():
            return
        turn, error = _load_turn(journal)
        if turn is None:
            turn = _new_turn(error or "MISSING_USER_PROMPT")
        _consume_contention(turn, contention)
        try:
            import telemetry

            frozen_event = turn.get("final_event")
            if not isinstance(frozen_event, Mapping):
                internal_us = (time.perf_counter_ns() - started) // 1000
                wall_us = (time.perf_counter_ns() - (hook_started_ns or started)) // 1000
                payload = _summary(
                    turn, stop_internal_us=internal_us, stop_wall_us=wall_us
                )
                event_id = str(
                    uuid.uuid5(uuid.NAMESPACE_URL, "context-io:" + turn["turn_nonce"])
                )
                frozen_event = telemetry.build_context_io_turn_event(
                    payload,
                    task_id=(
                        turn.get("task_id")
                        if turn.get("task_association_known") is True
                        else None
                    ),
                    event_id=event_id,
                )
                turn["final_event"] = frozen_event
                if not _write_turn(journal, turn, contention):
                    return
            durable = telemetry.emit_context_io_turn(frozen_event, state_root=root)
        except BaseException:
            durable = False
        if not durable:
            return
        completed.parent.mkdir(parents=True, exist_ok=True)
        try:
            descriptor = os.open(completed, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(descriptor)
        except FileExistsError:
            pass
        journal.unlink(missing_ok=True)


def handle(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    protected_paths: Iterable[str] | None = None,
    hook_started_ns: int | None = None,
) -> None:
    hook_name = event.get("hook_event_name")
    if hook_name == "UserPromptSubmit":
        user_prompt_submit(event, state_root=state_root, hook_started_ns=hook_started_ns)
    elif hook_name == "PreToolUse" and event.get("tool_name") == "Read":
        read_pre_tool_use(
            event,
            state_root=state_root,
            repo_root=repo_root,
            protected_paths=protected_paths,
            hook_started_ns=hook_started_ns,
        )
    elif hook_name == "Stop":
        stop(event, state_root=state_root, hook_started_ns=hook_started_ns)


__all__ = [
    "MAX_BYTES_SCANNED_PER_READ",
    "MAX_JOURNAL_AGE_SECONDS",
    "MAX_LINES_SCANNED_PER_READ",
    "MAX_OBSERVATIONS_PER_TURN",
    "THRESHOLDS",
    "associate_turn",
    "handle",
    "read_pre_tool_use",
    "stop",
    "user_prompt_submit",
]
