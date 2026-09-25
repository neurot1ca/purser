"""Claude Desktop per-task JSONL token telemetry parser and lifecycle manager.

Reads allow-listed usage metadata from local Claude Desktop session transcripts,
deduplicates via (message.id, requestId) with safe fallback, anchors to PreToolUse,
and finalizes at Stop.
Preserves strict privacy: never logs, emits, or persists message content, prompt text,
thinking tokens, tool arguments, or arbitrary JSON fields.
"""

from __future__ import annotations

from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import time
from typing import Any

import telemetry
import state_root as purser_state_root


MAX_LINE_LENGTH = 1_000_000
MAX_INCREMENTAL_BYTES = 50_000_000
MAX_RECORDS = 50_000
MAX_ACTIVE_TASK_AGE_SECONDS = 86_400
MAX_TOKEN_VALUE = 100_000_000

SESSION_LOCK_TIMEOUT_SECONDS = 0.5
SESSION_LOCK_POLL_INTERVAL_SECONDS = 0.01

TASK_USAGE_SOURCE = "desktop_jsonl_observed"
TASK_USAGE_BOUNDARY = "anchored_pre_tool_to_stop"
TASK_USAGE_DEDUP = "message_id_request_id"

_SAFE_ID_RE = re.compile(r"^[A-Za-z0-9._-]{1,128}\Z")


def _default_projects_root() -> Path:
    userprofile = os.environ.get("USERPROFILE", os.path.expanduser("~"))
    return Path(userprofile) / ".claude" / "projects"


def _atomic_json_write(path: Path, data: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp_path = path.with_suffix(f".tmp.{os.getpid()}.{hashlib.sha256(os.urandom(8)).hexdigest()[:8]}")
    try:
        encoded = json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True)
        temp_path.write_text(encoded, encoding="utf-8")
        temp_path.replace(path)
    except BaseException:
        try:
            if temp_path.exists():
                temp_path.unlink()
        except OSError:
            pass
        raise


def _lock_path(state_root: Path, session_id: str) -> Path:
    return state_root / "active_tasks" / ".locks" / f"{session_id}.lock"


def _contention_marker_path(state_root: Path, session_id: str) -> Path:
    return state_root / "active_tasks" / f"{session_id}.contention"


def _write_all(fd: int, payload: bytes) -> bool:
    """Write all bytes to fd in a bounded loop, handling short writes.

    Returns True if all bytes are successfully written and fsynced.
    Returns False on OSError, zero/negative write, invalid count, or fsync failure.
    """
    total = len(payload)
    offset = 0
    max_attempts = total + 1
    attempts = 0

    while offset < total:
        attempts += 1
        if attempts > max_attempts:
            return False
        remaining = payload[offset:]
        try:
            n = os.write(fd, remaining)
        except OSError:
            return False

        if not isinstance(n, int) or isinstance(n, bool) or n <= 0 or n > len(remaining):
            return False
        offset += n

    try:
        os.fsync(fd)
    except OSError:
        return False

    return True


def _mark_contention(state_root: Path | str, session_id: str, task_id: str | None = None) -> str:
    """Atomically claim the contention marker for session_id (first-writer wins).

    Uses os.O_WRONLY | os.O_CREAT | os.O_EXCL to ensure only one process creates the marker.
    If the marker already exists, FileExistsError is caught and the existing marker
    is preserved untouched.
    Returns: 'CREATED', 'EXISTS', or 'FAILED'.
    """
    root = Path(state_root)
    marker = _contention_marker_path(root, session_id)
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
    except OSError:
        return "FAILED"

    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_BINARY", 0)
    try:
        fd = os.open(str(marker), flags, 0o600)
    except FileExistsError:
        return "EXISTS"
    except OSError:
        return "FAILED"

    try:
        if task_id and _SAFE_ID_RE.match(task_id):
            payload = task_id.encode("utf-8")
            if not _write_all(fd, payload):
                return "FAILED"
        return "CREATED"
    finally:
        try:
            os.close(fd)
        except OSError:
            pass


def _read_contention_marker(state_root: Path | str, session_id: str) -> tuple[bool, str | None]:
    marker = _contention_marker_path(Path(state_root), session_id)
    if not marker.exists():
        return False, None
    competing_tid = None
    try:
        content = marker.read_text(encoding="utf-8").strip()
        if content and _SAFE_ID_RE.match(content):
            competing_tid = content
    except OSError:
        pass
    return True, competing_tid


def _remove_contention_marker(state_root: Path | str, session_id: str) -> bool:
    marker = _contention_marker_path(Path(state_root), session_id)
    if not marker.exists():
        return True
    try:
        marker.unlink()
    except OSError:
        pass
    return not marker.exists()


@contextmanager
def session_lock(
    state_root: Path,
    session_id: str,
    *,
    timeout_seconds: float = SESSION_LOCK_TIMEOUT_SECONDS,
) -> Iterator[bool]:
    """Acquire an OS-level inter-process file lock for the given session.

    Uses msvcrt.locking on Windows and fcntl.flock on Unix.
    Bounded acquisition loop with timeout_seconds.
    Lock is automatically released upon exit or process termination by the OS kernel.
    """
    lock_file = _lock_path(state_root, session_id)
    try:
        lock_file.parent.mkdir(parents=True, exist_ok=True)
        handle = lock_file.open("a+b")
    except OSError:
        yield False
        return

    acquired = False
    try:
        if handle.tell() == 0:
            try:
                handle.write(b"0")
                handle.flush()
            except OSError:
                pass
        handle.seek(0)

        deadline = time.monotonic() + timeout_seconds
        while True:
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
                if time.monotonic() >= deadline:
                    break
                time.sleep(SESSION_LOCK_POLL_INTERVAL_SECONDS)
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
        try:
            handle.close()
        except OSError:
            pass


def validate_transcript_path(
    transcript_path: str | os.PathLike[str] | None,
    session_id: str | None,
    allowed_projects_root: str | os.PathLike[str] | None = None,
) -> tuple[Path | None, str | None]:
    """Validate transcript path against safety, session, and project root invariants."""
    if not transcript_path or not session_id:
        return None, "TRANSCRIPT_UNAVAILABLE"

    if not isinstance(session_id, str) or not _SAFE_ID_RE.fullmatch(session_id):
        return None, "SESSION_MISMATCH"

    raw_path_str = str(transcript_path)
    if "\x00" in raw_path_str:
        return None, "TRANSCRIPT_PATH_ESCAPE"

    raw_path = Path(raw_path_str)
    if raw_path.suffix.lower() != ".jsonl":
        return None, "WRONG_EXTENSION"

    # Require filename stem to match session_id
    if raw_path.stem != session_id:
        return None, "SESSION_MISMATCH"

    projects_root = Path(allowed_projects_root) if allowed_projects_root is not None else _default_projects_root()
    try:
        resolved_root = projects_root.resolve()
        resolved_path = raw_path.resolve()
    except (OSError, RuntimeError):
        return None, "TRANSCRIPT_PATH_ESCAPE"

    try:
        resolved_path.relative_to(resolved_root)
    except ValueError:
        return None, "TRANSCRIPT_PATH_ESCAPE"

    if not resolved_path.is_file():
        return None, "TRANSCRIPT_UNAVAILABLE"

    try:
        resolved_path.stat()
    except OSError:
        return None, "TRANSCRIPT_UNAVAILABLE"

    return resolved_path, None


def get_transcript_identity(resolved_path: Path) -> dict[str, Any] | None:
    """Extract filesystem metadata and canonical path identity for transcript comparison."""
    try:
        st = resolved_path.stat()
        canonical = os.path.normcase(os.path.realpath(str(resolved_path)))
        ctime_ns = getattr(st, "st_ctime_ns", int(st.st_ctime * 1e9))
        return {
            "canonical_path": canonical,
            "st_dev": st.st_dev,
            "st_ino": st.st_ino,
            "st_ctime_ns": ctime_ns,
            "st_size": st.st_size,
        }
    except OSError:
        return None


def check_transcript_identity(
    stop_path: Path,
    expected_identity: Mapping[str, Any],
    anchor_offset: int | None,
    cursor_offset: int | None,
) -> str | None:
    """Verify stop transcript identity against baseline. Fails closed on mismatch or rotation/truncation."""
    current_identity = get_transcript_identity(stop_path)
    if not current_identity:
        return "TRANSCRIPT_UNAVAILABLE"

    expected_canonical = expected_identity.get("canonical_path")
    if expected_canonical and current_identity["canonical_path"] != expected_canonical:
        return "TRANSCRIPT_IDENTITY_MISMATCH"

    expected_dev = expected_identity.get("st_dev")
    if expected_dev is not None and current_identity["st_dev"] != expected_dev:
        return "TRANSCRIPT_IDENTITY_MISMATCH"

    expected_ino = expected_identity.get("st_ino")
    if expected_ino is not None and expected_ino != 0:
        if current_identity["st_ino"] != expected_ino:
            return "TRANSCRIPT_IDENTITY_MISMATCH"

    expected_ctime = expected_identity.get("st_ctime_ns")
    if expected_ctime is not None and current_identity["st_ctime_ns"] != expected_ctime:
        return "TRANSCRIPT_IDENTITY_MISMATCH"

    initial_size = expected_identity.get("st_size", 0)
    current_size = current_identity["st_size"]
    if current_size < initial_size:
        return "TRANSCRIPT_ROTATED_OR_TRUNCATED"

    if anchor_offset is not None and anchor_offset > current_size:
        return "TRANSCRIPT_ROTATED_OR_TRUNCATED"
    if cursor_offset is not None and cursor_offset > current_size:
        return "TRANSCRIPT_ROTATED_OR_TRUNCATED"

    return None


def _validate_token_int(value: Any) -> int | None:
    if not isinstance(value, int) or isinstance(value, bool):
        return None
    if value < 0 or value > MAX_TOKEN_VALUE:
        return None
    return value


def scan_for_anchor_at_pre_tool_use(
    transcript_path: Path,
    tool_use_id: str,
) -> tuple[int | None, int, bool, str | None]:
    """Scan transcript near EOF (within MAX_INCREMENTAL_BYTES) to locate tool_use_id anchor.

    Counts every complete JSONL record examined after aligned seek position against MAX_RECORDS.
    Returns: (anchor_byte_offset, last_complete_line_cursor, anchor_deferred, error_reason)
    """
    try:
        file_size = transcript_path.stat().st_size
    except OSError:
        return None, 0, True, "TRANSCRIPT_UNAVAILABLE"

    seek_offset = max(0, file_size - MAX_INCREMENTAL_BYTES)

    try:
        with transcript_path.open("rb") as handle:
            if seek_offset > 0:
                handle.seek(seek_offset)
                handle.readline()

            last_complete_cursor = handle.tell()
            anchor_byte_offset = None
            anchor_found = False
            record_count = 0

            while True:
                line_start = handle.tell()
                line_bytes = handle.readline(MAX_LINE_LENGTH + 1)
                if not line_bytes:
                    break
                if len(line_bytes) > MAX_LINE_LENGTH:
                    return None, last_complete_cursor, True, "BOUNDS_EXCEEDED"
                if not line_bytes.endswith(b"\n"):
                    # Incomplete trailing line at PreToolUse is deferred without failure
                    break

                last_complete_cursor = handle.tell()
                record_count += 1
                if record_count > MAX_RECORDS:
                    return None, last_complete_cursor, True, "BOUNDS_EXCEEDED"

                if not anchor_found:
                    try:
                        data = json.loads(line_bytes.decode("utf-8"))
                        if isinstance(data, Mapping) and data.get("type") == "assistant":
                            msg = data.get("message")
                            if isinstance(msg, Mapping):
                                content = msg.get("content")
                                if isinstance(content, list):
                                    for block in content:
                                        if (
                                            isinstance(block, Mapping)
                                            and block.get("type") == "tool_use"
                                            and block.get("id") == tool_use_id
                                        ):
                                            anchor_byte_offset = line_start
                                            anchor_found = True
                                            break
                    except (UnicodeDecodeError, json.JSONDecodeError):
                        continue

            return anchor_byte_offset, last_complete_cursor, not anchor_found, None
    except OSError:
        return None, 0, True, "TRANSCRIPT_UNAVAILABLE"


def read_incremental_records_from_offset(
    transcript_path: Path,
    from_byte_offset: int,
    *,
    allow_incomplete_tail: bool = False,
) -> tuple[list[dict[str, Any]], str | None]:
    """Read JSONL assistant records from from_byte_offset to EOF.

    Enforces MAX_INCREMENTAL_BYTES on bytes consumed from from_byte_offset.
    At Stop boundary (allow_incomplete_tail=False), any trailing bytes not ending
    in b"\\n" fail closed with TRANSCRIPT_UNRESOLVED_TAIL.
    """
    try:
        file_size = transcript_path.stat().st_size
    except OSError:
        return [], "TRANSCRIPT_UNAVAILABLE"

    if from_byte_offset > file_size:
        return [], "TRANSCRIPT_ROTATED_OR_TRUNCATED"

    bytes_to_read = file_size - from_byte_offset
    if bytes_to_read > MAX_INCREMENTAL_BYTES:
        return [], "BOUNDS_EXCEEDED"

    records: list[dict[str, Any]] = []
    line_count = 0
    has_unresolved_tail = False

    try:
        with transcript_path.open("rb") as handle:
            handle.seek(from_byte_offset)
            while True:
                line_bytes = handle.readline(MAX_LINE_LENGTH + 1)
                if not line_bytes:
                    break
                if len(line_bytes) > MAX_LINE_LENGTH:
                    return [], "BOUNDS_EXCEEDED"
                if not line_bytes.endswith(b"\n"):
                    has_unresolved_tail = True
                    break

                line_count += 1
                if line_count > MAX_RECORDS:
                    return [], "BOUNDS_EXCEEDED"

                try:
                    data = json.loads(line_bytes.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError):
                    return [], "INVALID_USAGE_RECORD"

                if isinstance(data, Mapping) and data.get("type") == "assistant":
                    records.append(data)

        if has_unresolved_tail and not allow_incomplete_tail:
            return [], "TRANSCRIPT_UNRESOLVED_TAIL"

        return records, None
    except OSError:
        return [], "TRANSCRIPT_UNAVAILABLE"


def parse_transcript_assistant_records(
    transcript_path: Path,
    *,
    allow_incomplete_tail: bool = False,
) -> tuple[list[dict[str, Any]], str | None]:
    """Convenience full-scan parser delegating to read_incremental_records_from_offset(0)."""
    return read_incremental_records_from_offset(
        transcript_path,
        0,
        allow_incomplete_tail=allow_incomplete_tail,
    )


def aggregate_usage_from_records(
    assistant_records: list[Mapping[str, Any]],
    anchor_tool_use_id: str,
) -> tuple[dict[str, Any] | None, str | None]:
    """Aggregate deduplicated tokens starting from the anchor assistant record through end."""
    anchor_index = -1
    for idx, record in enumerate(assistant_records):
        msg = record.get("message")
        if not isinstance(msg, Mapping):
            continue
        content = msg.get("content")
        if isinstance(content, list):
            for block in content:
                if isinstance(block, Mapping) and block.get("type") == "tool_use":
                    if block.get("id") == anchor_tool_use_id:
                        anchor_index = idx
                        break
        if anchor_index != -1:
            break

    if anchor_index == -1:
        return None, "ANCHOR_NOT_FOUND"

    seen_keys: dict[tuple[str, str], tuple[int, int, int, int]] = {}
    target_records = assistant_records[anchor_index:]

    input_tokens_total = 0
    output_tokens_total = 0
    cache_creation_total = 0
    cache_read_total = 0

    for record in target_records:
        msg = record.get("message")
        if not isinstance(msg, Mapping):
            return None, "INVALID_USAGE_RECORD"

        msg_id = msg.get("id")
        if not isinstance(msg_id, str) or not msg_id:
            return None, "INVALID_USAGE_RECORD"

        request_id = record.get("requestId")
        if request_id is not None:
            if not isinstance(request_id, str) or not request_id:
                return None, "INVALID_USAGE_RECORD"
            dedup_key = (msg_id, request_id)
        else:
            # Documented fallback key for missing requestId
            dedup_key = (msg_id, f"_msg_fallback_{msg_id}")

        raw_usage = msg.get("usage")
        if not isinstance(raw_usage, Mapping):
            return None, "INVALID_USAGE_RECORD"

        inp = _validate_token_int(raw_usage.get("input_tokens"))
        out = _validate_token_int(raw_usage.get("output_tokens"))
        cache_create = _validate_token_int(raw_usage.get("cache_creation_input_tokens", 0))
        cache_read = _validate_token_int(raw_usage.get("cache_read_input_tokens", 0))

        if inp is None or out is None or cache_create is None or cache_read is None:
            return None, "INVALID_USAGE_RECORD"

        usage_tuple = (inp, out, cache_create, cache_read)

        if dedup_key in seen_keys:
            prev_tuple = seen_keys[dedup_key]
            if prev_tuple != usage_tuple:
                return None, "CONTRADICTORY_USAGE_RECORD"
            # Identical duplicate: count once
            continue

        seen_keys[dedup_key] = usage_tuple
        input_tokens_total += inp
        output_tokens_total += out
        cache_creation_total += cache_create
        cache_read_total += cache_read

    request_count = len(seen_keys)
    if request_count == 0:
        return None, "NO_USAGE_RECORDS"

    total_observed = input_tokens_total + output_tokens_total + cache_creation_total + cache_read_total

    result = {
        "claude_task_usage_source": TASK_USAGE_SOURCE,
        "claude_task_usage_known": True,
        "claude_task_usage_complete": True,
        "claude_task_input_tokens": input_tokens_total,
        "claude_task_output_tokens": output_tokens_total,
        "claude_task_cache_creation_input_tokens": cache_creation_total,
        "claude_task_cache_read_input_tokens": cache_read_total,
        "claude_task_total_observed_tokens": total_observed,
        "claude_task_request_count": request_count,
        "claude_task_usage_boundary": TASK_USAGE_BOUNDARY,
        "claude_task_usage_dedup": TASK_USAGE_DEDUP,
        "claude_task_usage_unknown_reason": None,
        "claude_task_subagent_usage_included": False,
    }
    return result, None


def _active_task_path(state_root: Path, session_id: str) -> Path:
    return state_root / "active_tasks" / f"{session_id}.json"


def start_active_task(
    state_root: str | os.PathLike[str] | None,
    *,
    session_id: str | None,
    task_id: str | None,
    project_key: str | None = None,
    tool_use_id: str | None,
    transcript_path: str | os.PathLike[str] | None,
    allowed_projects_root: str | os.PathLike[str] | None = None,
    lock_timeout_seconds: float = SESSION_LOCK_TIMEOUT_SECONDS,
) -> dict[str, Any]:
    """Establish active task baseline at PreToolUse boundary under per-session lock."""
    if not session_id or not task_id or not tool_use_id:
        return {"status": "SKIPPED", "reason": "MISSING_IDENTIFIER"}

    root = Path(state_root) if state_root is not None else purser_state_root.default_state_root()
    active_path = _active_task_path(root, session_id)

    with session_lock(root, session_id, timeout_seconds=lock_timeout_seconds) as acquired:
        if not acquired:
            _mark_contention(root, session_id, task_id=task_id)
            telemetry.emit(
                "CLAUDE_TASK_USAGE_RECORDED",
                task_id=task_id,
                project_key=project_key,
                session_id=session_id,
                claude_task_usage_source=TASK_USAGE_SOURCE,
                claude_task_usage_known=False,
                claude_task_usage_complete=False,
                claude_task_usage_unknown_reason="ACTIVE_TASK_STATE_CONTENTION",
                claude_task_subagent_usage_included=False,
                state_root=root,
            )
            telemetry.update_task(task_id, project_key=project_key, state_root=root)
            return {"status": "DENIED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}

        now_utc = datetime.now(timezone.utc)
        now_iso = now_utc.isoformat().replace("+00:00", "Z")

        # Check contention marker
        marker_present, competing_tid = _read_contention_marker(root, session_id)

        existing = None
        if active_path.exists():
            try:
                raw_existing = json.loads(active_path.read_text(encoding="utf-8"))
                if isinstance(raw_existing, Mapping):
                    existing = raw_existing
            except (OSError, UnicodeDecodeError, json.JSONDecodeError):
                try:
                    active_path.unlink()
                except OSError:
                    pass

        # If contention was recorded while locked, invalidate existing active boundary
        if marker_present and existing and existing.get("completed") is not True:
            cur_tid = existing.get("task_id")
            affected = list(existing.get("affected_task_ids", []))
            if cur_tid and cur_tid not in affected:
                affected.append(cur_tid)
            if competing_tid and competing_tid not in affected:
                affected.append(competing_tid)

            existing_copy = dict(existing)
            existing_copy["invalidated"] = True
            existing_copy["invalidated_reason"] = "ACTIVE_TASK_STATE_CONTENTION"
            existing_copy["completed"] = True
            existing_copy["affected_task_ids"] = affected[-16:]
            _atomic_json_write(active_path, existing_copy)
            if cur_tid:
                telemetry.emit(
                    "CLAUDE_TASK_USAGE_RECORDED",
                    task_id=cur_tid,
                    project_key=existing.get("project_key", project_key),
                    session_id=session_id,
                    claude_task_usage_source=TASK_USAGE_SOURCE,
                    claude_task_usage_known=False,
                    claude_task_usage_complete=False,
                    claude_task_usage_unknown_reason="ACTIVE_TASK_STATE_CONTENTION",
                    claude_task_subagent_usage_included=False,
                    state_root=root,
                )
                telemetry.update_task(
                    cur_tid, project_key=existing.get("project_key", project_key), state_root=root
                )
            existing = existing_copy

        # Remove marker after durable invalidation and terminal telemetry
        marker_cleared = True
        if marker_present:
            marker_cleared = _remove_contention_marker(root, session_id)

        if not marker_cleared:
            telemetry.emit(
                "CLAUDE_TASK_USAGE_RECORDED",
                task_id=task_id,
                project_key=project_key,
                session_id=session_id,
                claude_task_usage_source=TASK_USAGE_SOURCE,
                claude_task_usage_known=False,
                claude_task_usage_complete=False,
                claude_task_usage_unknown_reason="ACTIVE_TASK_STATE_CONTENTION",
                claude_task_subagent_usage_included=False,
                state_root=root,
            )
            telemetry.update_task(task_id, project_key=project_key, state_root=root)
            return {"status": "DENIED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}

        terminal_task_ids: list[str] = []
        if competing_tid:
            terminal_task_ids.append(competing_tid)

        if existing is not None:
            # 1. Stale check
            created_str = existing.get("created_at")
            is_stale = False
            if created_str:
                try:
                    created_dt = datetime.fromisoformat(created_str.replace("Z", "+00:00"))
                    age = (now_utc - created_dt).total_seconds()
                    if age > MAX_ACTIVE_TASK_AGE_SECONDS:
                        is_stale = True
                except ValueError:
                    is_stale = True

            if is_stale:
                stale_task_id = existing.get("task_id")
                if stale_task_id and existing.get("completed") is not True:
                    telemetry.emit(
                        "CLAUDE_TASK_USAGE_RECORDED",
                        task_id=stale_task_id,
                        project_key=existing.get("project_key", project_key),
                        session_id=session_id,
                        claude_task_usage_source=TASK_USAGE_SOURCE,
                        claude_task_usage_known=False,
                        claude_task_usage_complete=False,
                        claude_task_usage_unknown_reason="STALE_ACTIVE_TASK",
                        claude_task_subagent_usage_included=False,
                        state_root=root,
                    )
                    telemetry.update_task(
                        stale_task_id,
                        project_key=existing.get("project_key", project_key),
                        state_root=root,
                    )
                try:
                    active_path.unlink()
                except OSError:
                    pass
                existing = None

            # 2. Completed / retired boundary check
            elif existing.get("completed") is True:
                existing_affected = existing.get("affected_task_ids", [existing.get("task_id")])
                existing_terminal = list(existing.get("terminal_task_ids", []))

                # Check if this task was already part of this completed/terminal boundary
                if task_id == existing.get("task_id") or task_id in existing_affected or task_id in existing_terminal:
                    reason = existing.get("invalidated_reason") or "ALREADY_COMPLETED"
                    return {"status": "DENIED", "reason": reason}

                # Genuinely new task in same session: retire completed boundary and collect terminal history
                for tid in existing_affected:
                    if tid and tid not in existing_terminal:
                        existing_terminal.append(tid)
                terminal_task_ids = existing_terminal[-16:]
                try:
                    active_path.unlink()
                except OSError:
                    pass
                existing = None

            # 3. Active in-progress task check
            else:
                if existing.get("task_id") == task_id:
                    # Same task (e.g. codex-reply concurrent or sequential): retain original baseline
                    return {"status": "RETAINED", "active_task": existing}
                else:
                    # Different task active at the same time: OVERLAPPING_TASK_BOUNDARY!
                    other_task_id = existing.get("task_id")
                    existing_copy = dict(existing)
                    existing_copy["invalidated"] = True
                    existing_copy["invalidated_reason"] = "OVERLAPPING_TASK_BOUNDARY"
                    existing_copy["completed"] = True
                    existing_copy["affected_task_ids"] = [other_task_id, task_id]
                    _atomic_json_write(active_path, existing_copy)

                    for tid in (other_task_id, task_id):
                        if tid:
                            telemetry.emit(
                                "CLAUDE_TASK_USAGE_RECORDED",
                                task_id=tid,
                                project_key=existing.get("project_key", project_key),
                                session_id=session_id,
                                claude_task_usage_source=TASK_USAGE_SOURCE,
                                claude_task_usage_known=False,
                                claude_task_usage_complete=False,
                                claude_task_usage_unknown_reason="OVERLAPPING_TASK_BOUNDARY",
                                claude_task_subagent_usage_included=False,
                                state_root=root,
                            )
                            telemetry.update_task(
                                tid,
                                project_key=existing.get("project_key", project_key),
                                state_root=root,
                            )
                    return {"status": "DENIED", "reason": "OVERLAPPING_TASK_BOUNDARY"}

        if task_id in terminal_task_ids:
            return {"status": "DENIED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}

        # Fresh new task boundary setup
        resolved_transcript, error = validate_transcript_path(
            transcript_path, session_id, allowed_projects_root=allowed_projects_root
        )

        identity = get_transcript_identity(resolved_transcript) if resolved_transcript and not error else None

        anchor_byte_offset = None
        anchor_search_start_offset = 0
        anchor_deferred = True

        if resolved_transcript and not error:
            anchor_offset, cursor_offset, deferred, scan_err = scan_for_anchor_at_pre_tool_use(
                resolved_transcript, tool_use_id
            )
            if scan_err:
                telemetry.emit(
                    "CLAUDE_TASK_USAGE_RECORDED",
                    task_id=task_id,
                    project_key=project_key,
                    session_id=session_id,
                    claude_task_usage_source=TASK_USAGE_SOURCE,
                    claude_task_usage_known=False,
                    claude_task_usage_complete=False,
                    claude_task_usage_unknown_reason=scan_err,
                    claude_task_subagent_usage_included=False,
                    state_root=root,
                )
                telemetry.update_task(task_id, project_key=project_key, state_root=root)
                return {"status": "DENIED", "reason": scan_err}

            anchor_byte_offset = anchor_offset
            anchor_search_start_offset = cursor_offset
            anchor_deferred = deferred

        active_record = {
            "session_id": session_id,
            "task_id": task_id,
            "project_key": project_key,
            "tool_use_id": tool_use_id,
            "canonical_transcript_path": identity["canonical_path"] if identity else (str(transcript_path) if transcript_path else None),
            "transcript_dev": identity["st_dev"] if identity else None,
            "transcript_ino": identity["st_ino"] if identity else None,
            "transcript_ctime_ns": identity["st_ctime_ns"] if identity else None,
            "initial_size": identity["st_size"] if identity else 0,
            "anchor_byte_offset": anchor_byte_offset,
            "anchor_search_start_offset": anchor_search_start_offset,
            "anchor_deferred": anchor_deferred,
            "created_at": now_iso,
            "completed": False,
            "invalidated": False,
            "invalidated_reason": None,
            "affected_task_ids": [task_id],
            "terminal_task_ids": terminal_task_ids,
        }
        _atomic_json_write(active_path, active_record)
        return {"status": "STARTED", "active_task": active_record}


def finalize_stop_task(
    state_root: str | os.PathLike[str] | None,
    *,
    session_id: str | None,
    transcript_path: str | os.PathLike[str] | None = None,
    allowed_projects_root: str | os.PathLike[str] | None = None,
    lock_timeout_seconds: float = SESSION_LOCK_TIMEOUT_SECONDS,
) -> dict[str, Any] | None:
    """Finalize active task at Stop boundary under per-session lock."""
    if not session_id:
        return None

    root = Path(state_root) if state_root is not None else purser_state_root.default_state_root()
    active_path = _active_task_path(root, session_id)
    marker = _contention_marker_path(root, session_id)
    if not active_path.exists() and not marker.exists():
        return None

    with session_lock(root, session_id, timeout_seconds=lock_timeout_seconds) as acquired:
        if not acquired:
            _mark_contention(root, session_id)
            return {"status": "FAILED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}

        marker_present, competing_tid = _read_contention_marker(root, session_id)

        if not active_path.exists():
            if marker_present:
                _remove_contention_marker(root, session_id)
            return None

        try:
            active_task = json.loads(active_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            if marker_present:
                _remove_contention_marker(root, session_id)
            return None

        if not isinstance(active_task, Mapping):
            if marker_present:
                _remove_contention_marker(root, session_id)
            return None

        project_key = active_task.get("project_key")

        # 1. Idempotency check: already finalized
        if active_task.get("completed") is True:
            if marker_present:
                _remove_contention_marker(root, session_id)
            if active_task.get("invalidated") is True:
                return {"status": "FAILED", "reason": active_task.get("invalidated_reason", "OVERLAPPING_TASK_BOUNDARY")}
            return None

        # 2. Overlap / prior invalidation check
        if active_task.get("invalidated") is True:
            if marker_present:
                _remove_contention_marker(root, session_id)
            return {"status": "FAILED", "reason": active_task.get("invalidated_reason", "OVERLAPPING_TASK_BOUNDARY")}

        # 3. Contention marker observed while in-progress active boundary exists!
        if marker_present:
            task_id = active_task.get("task_id")
            affected = list(active_task.get("affected_task_ids", []))
            if task_id and task_id not in affected:
                affected.append(task_id)
            if competing_tid and competing_tid not in affected:
                affected.append(competing_tid)

            active_task_copy = dict(active_task)
            active_task_copy["invalidated"] = True
            active_task_copy["invalidated_reason"] = "ACTIVE_TASK_STATE_CONTENTION"
            active_task_copy["completed"] = True
            active_task_copy["affected_task_ids"] = affected[-16:]
            _atomic_json_write(active_path, active_task_copy)

            if task_id:
                telemetry.emit(
                    "CLAUDE_TASK_USAGE_RECORDED",
                    task_id=task_id,
                    project_key=project_key,
                    session_id=session_id,
                    claude_task_usage_source=TASK_USAGE_SOURCE,
                    claude_task_usage_known=False,
                    claude_task_usage_complete=False,
                    claude_task_usage_unknown_reason="ACTIVE_TASK_STATE_CONTENTION",
                    claude_task_subagent_usage_included=False,
                    state_root=root,
                )
                telemetry.update_task(task_id, project_key=project_key, state_root=root)

            _remove_contention_marker(root, session_id)
            return {"status": "FAILED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}

        task_id = active_task.get("task_id")
        tool_use_id = active_task.get("tool_use_id")
        if not task_id or not tool_use_id:
            try:
                active_path.unlink()
            except OSError:
                pass
            return None

        # Stale task check
        now_utc = datetime.now(timezone.utc)
        created_str = active_task.get("created_at")
        path_error = None
        if created_str:
            try:
                created_dt = datetime.fromisoformat(created_str.replace("Z", "+00:00"))
                if (now_utc - created_dt).total_seconds() > MAX_ACTIVE_TASK_AGE_SECONDS:
                    path_error = "STALE_ACTIVE_TASK"
            except ValueError:
                path_error = "STALE_ACTIVE_TASK"

        target_transcript_path = transcript_path or active_task.get("canonical_transcript_path")
        resolved_transcript = None
        if not path_error:
            resolved_transcript, path_error = validate_transcript_path(
                target_transcript_path, session_id, allowed_projects_root=allowed_projects_root
            )

        # Transcript identity and monotonicity check
        identity_error = None
        if not path_error and resolved_transcript:
            expected_identity = {
                "canonical_path": active_task.get("canonical_transcript_path"),
                "st_dev": active_task.get("transcript_dev"),
                "st_ino": active_task.get("transcript_ino"),
                "st_ctime_ns": active_task.get("transcript_ctime_ns"),
                "st_size": active_task.get("initial_size", 0),
            }
            identity_error = check_transcript_identity(
                resolved_transcript,
                expected_identity,
                active_task.get("anchor_byte_offset"),
                active_task.get("anchor_search_start_offset"),
            )

        fatal_error = path_error or identity_error
        if fatal_error or not resolved_transcript:
            reason = fatal_error or "TRANSCRIPT_UNAVAILABLE"
            active_task_copy = dict(active_task)
            active_task_copy["invalidated"] = True
            active_task_copy["invalidated_reason"] = reason
            active_task_copy["completed"] = True
            _atomic_json_write(active_path, active_task_copy)

            telemetry.emit(
                "CLAUDE_TASK_USAGE_RECORDED",
                task_id=task_id,
                project_key=project_key,
                session_id=session_id,
                claude_task_usage_source=TASK_USAGE_SOURCE,
                claude_task_usage_known=False,
                claude_task_usage_complete=False,
                claude_task_usage_unknown_reason=reason,
                claude_task_subagent_usage_included=False,
                state_root=root,
            )
            telemetry.update_task(task_id, project_key=project_key, state_root=root)
            return {"status": "FAILED", "reason": reason}

        # Determine incremental seek offset
        anchor_deferred = active_task.get("anchor_deferred", True)
        if not anchor_deferred and active_task.get("anchor_byte_offset") is not None:
            seek_offset = active_task["anchor_byte_offset"]
        else:
            seek_offset = active_task.get("anchor_search_start_offset", 0)

        records, read_err = read_incremental_records_from_offset(
            resolved_transcript, seek_offset, allow_incomplete_tail=False
        )
        if read_err:
            active_task_copy = dict(active_task)
            active_task_copy["invalidated"] = True
            active_task_copy["invalidated_reason"] = read_err
            active_task_copy["completed"] = True
            _atomic_json_write(active_path, active_task_copy)

            telemetry.emit(
                "CLAUDE_TASK_USAGE_RECORDED",
                task_id=task_id,
                project_key=project_key,
                session_id=session_id,
                claude_task_usage_source=TASK_USAGE_SOURCE,
                claude_task_usage_known=False,
                claude_task_usage_complete=False,
                claude_task_usage_unknown_reason=read_err,
                claude_task_subagent_usage_included=False,
                state_root=root,
            )
            telemetry.update_task(task_id, project_key=project_key, state_root=root)
            return {"status": "FAILED", "reason": read_err}

        usage_data, agg_err = aggregate_usage_from_records(records, tool_use_id)
        if agg_err or not usage_data:
            reason = agg_err or "NO_USAGE_RECORDS"
            active_task_copy = dict(active_task)
            active_task_copy["invalidated"] = True
            active_task_copy["invalidated_reason"] = reason
            active_task_copy["completed"] = True
            _atomic_json_write(active_path, active_task_copy)

            telemetry.emit(
                "CLAUDE_TASK_USAGE_RECORDED",
                task_id=task_id,
                project_key=project_key,
                session_id=session_id,
                claude_task_usage_source=TASK_USAGE_SOURCE,
                claude_task_usage_known=False,
                claude_task_usage_complete=False,
                claude_task_usage_unknown_reason=reason,
                claude_task_subagent_usage_included=False,
                state_root=root,
            )
            telemetry.update_task(task_id, project_key=project_key, state_root=root)
            return {"status": "FAILED", "reason": reason}

        # Success: emit event with full totals
        telemetry.emit(
            "CLAUDE_TASK_USAGE_RECORDED",
            task_id=task_id,
            project_key=project_key,
            session_id=session_id,
            state_root=root,
            **usage_data,
        )
        telemetry.update_task(task_id, project_key=project_key, state_root=root)
        active_task_copy = dict(active_task)
        active_task_copy["completed"] = True
        _atomic_json_write(active_path, active_task_copy)
        return {"status": "COMPLETED", "usage": usage_data}
