"""Fail-open raw telemetry persistence for the orchestrator hooks."""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import sys
import tempfile
import time
from typing import Any, Iterator
import uuid

import telemetry_schema as schema
import obsidian_projection


LOCK_TIMEOUT_SECONDS = 0.05
SPOOL_MERGE_LIMIT = 16
ATOMIC_REPLACE_MAX_ATTEMPTS = 4
ATOMIC_REPLACE_MAX_WAIT_SECONDS = 0.03
CONTEXT_IO_EVENT_MAX_BYTES = 64 * 1024
CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES = 256 * 1024
CONTEXT_IO_METADATA_MAX_BYTES = 4 * 1024
CLAUDE_USAGE_MAX_STALENESS_SECONDS = 5 * 60
CLAUDE_USAGE_MAX_BYTES = 16 * 1024
CLAUDE_RATE_LIMIT_DELTA_SCOPE = "account_window_observation"
_CLAUDE_USAGE_EVENTS = frozenset({"IMPLEMENTER_STARTED", "IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED"})
_PACKAGE_ROOT = Path(__file__).resolve().parent
_SKILL_PATH = _PACKAGE_ROOT.parents[1] / "skills" / "codex-orchestrator" / "SKILL.md"
_CONFIG_PATH = _PACKAGE_ROOT / "config.json"
_stderr_reported = False
_CURSOR_SCHEMA_VERSION = 1
_HEALTH_ERROR_CODES = frozenset(
    {
        "TELEMETRY_ENTRYPOINT_FAILURE",
        "TELEMETRY_EMIT_FAILED",
        "TELEMETRY_UPDATE_TASK_FAILED",
        "TELEMETRY_REBUILD_FAILED",
        "TELEMETRY_HEALTH_FAILED",
        "TELEMETRY_SPOOL_MERGE_FAILED",
        "TELEMETRY_PROJECTION_FAILED",
        "MALFORMED_SPOOL_EVENT",
        "CLAUDE_TRANSCRIPT_PARSER_UNAVAILABLE",
        "CLAUDE_TRANSCRIPT_PARSER_FAILED",
        "CLAUDE_TRANSCRIPT_FINALIZATION_FAILED",
        "CONTEXT_IO_EMIT_FAILED",
    }
)
TERMINAL_TASK_USAGE_FAILURES = frozenset(
    {
        "OVERLAPPING_TASK_BOUNDARY",
        "CONTRADICTORY_USAGE_RECORD",
        "TRANSCRIPT_IDENTITY_MISMATCH",
        "TRANSCRIPT_ROTATED_OR_TRUNCATED",
        "TRANSCRIPT_UNRESOLVED_TAIL",
        "BOUNDS_EXCEEDED",
        "INVALID_USAGE_RECORD",
        "STALE_ACTIVE_TASK",
        "ANCHOR_NOT_FOUND",
        "TRANSCRIPT_UNAVAILABLE",
        "SESSION_MISMATCH",
        "WRONG_EXTENSION",
        "TRANSCRIPT_PATH_ESCAPE",
        "NO_USAGE_RECORDS",
        "ACTIVE_TASK_STATE_CONTENTION",
    }
)


def _state_root(root: str | os.PathLike[str] | None) -> Path:
    if root is not None:
        return Path(root)
    local_app_data = os.environ.get("LOCALAPPDATA")
    if not local_app_data:
        raise OSError("telemetry state root unavailable")
    return Path(local_app_data) / "claude-agent-orchestrator"


def _root(state_root: str | os.PathLike[str] | None) -> Path:
    return _state_root(state_root) / "telemetry"


def _atomic_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", prefix=path.name + ".", suffix=".tmp", dir=path.parent, delete=False
        ) as handle:
            temporary = Path(handle.name)
            json.dump(value, handle, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
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


@contextmanager
def _try_lock(root: Path) -> Iterator[bool]:
    """Try once for the process-wide append lock; never block or poll."""

    path = root / "events" / ".append.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    handle = path.open("a+b")
    acquired = False
    try:
        if handle.tell() == 0:
            handle.write(b"0")
            handle.flush()
        handle.seek(0)
        try:
            if os.name == "nt":
                import msvcrt

                msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            acquired = True
        except (OSError, BlockingIOError):
            acquired = False
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


def _event_path(root: Path, event: Mapping[str, Any]) -> Path:
    month = str(event["ts_utc"])[:7]
    session_id = event.get("session_id") or "unknown"
    return root / "events" / month / f"session_{session_id}.jsonl"


def _encoded_event(event: Mapping[str, Any]) -> bytes:
    return (
        json.dumps(event, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        + "\n"
    ).encode("utf-8")


def _append_record(path: Path, event: Mapping[str, Any]) -> tuple[int, int]:
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = _encoded_event(event)
    with path.open("ab") as handle:
        handle.seek(0, os.SEEK_END)
        offset = handle.tell()
        handle.write(encoded)
        handle.flush()
        os.fsync(handle.fileno())
    return offset, len(encoded)


def _append_event(root: Path, event: Mapping[str, Any]) -> tuple[int, int]:
    return _append_record(_event_path(root, event), event)


def _task_index_path(root: Path, task_id: str) -> Path:
    return root / "events" / "by_task" / f"{task_id}.jsonl"


def _append_task_index(root: Path, event: Mapping[str, Any]) -> tuple[int, int] | None:
    """Persist a bounded-read feed without changing the raw event schema."""

    task_id = event.get("task_id")
    if not isinstance(task_id, str):
        return
    return _append_record(_task_index_path(root, task_id), event)


def _context_io_metadata_paths(root: Path, event_id: str) -> tuple[Path, Path]:
    spool = root / "events" / "spool"
    return (
        spool / ".context_io_intents" / f"{event_id}.json",
        spool / ".context_io_receipts" / f"{event_id}.json",
    )


def _read_context_io_metadata(path: Path) -> tuple[dict[str, Any] | None, str]:
    try:
        with path.open("rb") as handle:
            encoded = handle.read(CONTEXT_IO_METADATA_MAX_BYTES + 1)
        if len(encoded) > CONTEXT_IO_METADATA_MAX_BYTES:
            return None, "UNREADABLE"
        value = json.loads(encoded.decode("utf-8"))
    except FileNotFoundError:
        return None, "MISSING"
    except (OSError, UnicodeError, json.JSONDecodeError):
        return None, "UNREADABLE"
    return (dict(value), "VALID") if isinstance(value, Mapping) else (None, "UNREADABLE")


def _context_io_metadata(event: Mapping[str, Any], encoded: bytes, root: Path) -> dict[str, Any]:
    task_id = event.get("task_id")
    event_path = _event_path(root, event)
    index_path = _task_index_path(root, task_id) if isinstance(task_id, str) else None
    return {
        "schema_version": 1,
        "event_id": event["event_id"],
        "event_sha256": hashlib.sha256(encoded).hexdigest(),
        "canonical_month": str(event["ts_utc"])[:7],
        "canonical_offset": event_path.stat().st_size if event_path.exists() else 0,
        "canonical_length": len(encoded),
        "task_indexed": index_path is not None,
        "task_index_offset": (
            index_path.stat().st_size if index_path is not None and index_path.exists()
            else 0 if index_path is not None
            else None
        ),
        "task_index_length": len(encoded) if index_path is not None else None,
    }


def _valid_context_io_metadata(
    value: Mapping[str, Any], event: Mapping[str, Any], encoded: bytes
) -> bool:
    task_indexed = isinstance(event.get("task_id"), str)
    expected_keys = {
        "schema_version", "event_id", "event_sha256", "canonical_month",
        "canonical_offset", "canonical_length", "task_indexed",
        "task_index_offset", "task_index_length",
    }
    integer_fields = ("canonical_offset", "canonical_length")
    if set(value) != expected_keys or any(
        not isinstance(value.get(name), int)
        or isinstance(value.get(name), bool)
        or value[name] < 0
        for name in integer_fields
    ):
        return False
    if task_indexed and (
        not isinstance(value.get("task_index_offset"), int)
        or isinstance(value.get("task_index_offset"), bool)
        or value["task_index_offset"] < 0
        or value.get("task_index_length") != len(encoded)
    ):
        return False
    return (
        value.get("schema_version") == 1
        and value.get("event_id") == event.get("event_id")
        and value.get("event_sha256") == hashlib.sha256(encoded).hexdigest()
        and value.get("canonical_month") == str(event.get("ts_utc"))[:7]
        and value.get("canonical_length") == len(encoded)
        and value.get("task_indexed") is task_indexed
        and (task_indexed or value.get("task_index_offset") is None)
        and (task_indexed or value.get("task_index_length") is None)
    )


def _record_matches(path: Path, *, offset: int, encoded: bytes) -> bool:
    if len(encoded) > CONTEXT_IO_EVENT_MAX_BYTES:
        raise ValueError("context-I/O event exceeds verification bound")
    try:
        with path.open("rb") as handle:
            handle.seek(offset)
            return handle.read(len(encoded)) == encoded
    except FileNotFoundError:
        return False


def _file_size(path: Path) -> int:
    try:
        return path.stat().st_size
    except FileNotFoundError:
        return 0


def _complete_valid_record_at(path: Path, offset: int, event_id: str) -> bool:
    try:
        with path.open("rb") as handle:
            handle.seek(offset)
            candidate = handle.read(CONTEXT_IO_EVENT_MAX_BYTES + 1)
    except FileNotFoundError:
        return False
    newline = candidate.find(b"\n")
    if newline < 0 or newline >= CONTEXT_IO_EVENT_MAX_BYTES:
        return False
    record = candidate[: newline + 1]
    try:
        value = json.loads(record)
    except (UnicodeError, json.JSONDecodeError):
        return False
    return (
        isinstance(value, Mapping)
        and schema.valid_event(value)
        and value.get("event_id") != event_id
        and _encoded_event(value) == record
    )


def _eof_is_record_boundary(path: Path, size: int) -> bool:
    if size == 0:
        return True
    try:
        with path.open("rb") as handle:
            handle.seek(size - 1)
            return handle.read(1) == b"\n"
    except FileNotFoundError:
        return False


def _bounded_event_by_id(
    path: Path, event_id: str, encoded: bytes
) -> tuple[tuple[int, int] | None, bool]:
    try:
        size = path.stat().st_size
        start = max(0, size - CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES)
        with path.open("rb") as handle:
            handle.seek(start)
            data = handle.read(CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES)
    except FileNotFoundError:
        return None, True
    if start:
        boundary = data.find(b"\n")
        if boundary < 0:
            return None, False
        start += boundary + 1
        data = data[boundary + 1 :]
    offset = start
    for line in data.splitlines(keepends=True):
        try:
            value = json.loads(line)
        except (UnicodeError, json.JSONDecodeError):
            offset += len(line)
            continue
        if isinstance(value, Mapping) and value.get("event_id") == event_id:
            if line != encoded:
                raise ValueError("context-I/O event identity collision")
            return (offset, len(line)), start == 0
        offset += len(line)
    return None, start == 0


def _fallback_context_io_metadata(
    root: Path, event: Mapping[str, Any], encoded: bytes
) -> dict[str, Any]:
    task_id = event.get("task_id")
    event_path = _event_path(root, event)
    index_path = _task_index_path(root, task_id) if isinstance(task_id, str) else None
    canonical, canonical_complete = _bounded_event_by_id(
        event_path, str(event["event_id"]), encoded
    )
    indexed, index_complete = (
        _bounded_event_by_id(index_path, str(event["event_id"]), encoded)
        if index_path is not None
        else (None, True)
    )
    if (canonical is None and not canonical_complete) or (
        indexed is None and not index_complete
    ):
        raise OSError("context-I/O fallback verification bound exceeded")
    value = _context_io_metadata(event, encoded, root)
    if canonical is not None:
        value["canonical_offset"] = canonical[0]
    if indexed is not None:
        value["task_index_offset"] = indexed[0]
    return value


def _verify_context_io_metadata(
    root: Path, event: Mapping[str, Any], encoded: bytes, value: Mapping[str, Any]
) -> bool:
    if not _record_matches(
        _event_path(root, event), offset=value["canonical_offset"], encoded=encoded
    ):
        return False
    task_id = event.get("task_id")
    return not isinstance(task_id, str) or _record_matches(
        _task_index_path(root, task_id), offset=value["task_index_offset"], encoded=encoded
    )


def _relocate_occupied_context_io_offsets(
    root: Path,
    event: Mapping[str, Any],
    encoded: bytes,
    intent_path: Path,
    intent: Mapping[str, Any],
) -> dict[str, Any]:
    task_id = event.get("task_id")
    locations: list[tuple[str, Path]] = [
        ("canonical_offset", _event_path(root, event))
    ]
    if isinstance(task_id, str):
        locations.append(("task_index_offset", _task_index_path(root, task_id)))
    relocated = dict(intent)
    changed = False
    for offset_field, path in locations:
        offset = intent[offset_field]
        if _record_matches(path, offset=offset, encoded=encoded):
            continue
        size = _file_size(path)
        if size == offset:
            continue
        if (
            size < offset
            or not _complete_valid_record_at(path, offset, str(event["event_id"]))
            or not _eof_is_record_boundary(path, size)
        ):
            raise OSError("context-I/O intent offset is inconclusive")
        relocated[offset_field] = size
        changed = True
    if not changed:
        return relocated
    _atomic_json(intent_path, relocated)
    persisted, state = _read_context_io_metadata(intent_path)
    if (
        state != "VALID"
        or persisted != relocated
        or not _valid_context_io_metadata(persisted, event, encoded)
    ):
        raise OSError("context-I/O relocated intent was not persisted")
    return persisted


def _persist_context_io_event(root: Path, event: Mapping[str, Any]) -> None:
    encoded = _encoded_event(event)
    if len(encoded) > CONTEXT_IO_EVENT_MAX_BYTES:
        raise ValueError("context-I/O event exceeds verification bound")
    intent_path, receipt_path = _context_io_metadata_paths(root, str(event["event_id"]))
    receipt, receipt_state = _read_context_io_metadata(receipt_path)
    if receipt_state == "VALID":
        if not _valid_context_io_metadata(receipt, event, encoded):
            raise ValueError("context-I/O receipt disagrees with event")
        if not _verify_context_io_metadata(root, event, encoded, receipt):
            raise OSError("context-I/O receipt disagrees with durable log")
        intent_path.unlink(missing_ok=True)
        return

    intent, intent_state = _read_context_io_metadata(intent_path)
    if intent_state == "VALID":
        if not _valid_context_io_metadata(intent, event, encoded):
            raise ValueError("context-I/O intent disagrees with event")
    elif intent_state == "MISSING":
        intent = _context_io_metadata(event, encoded, root)
        _atomic_json(intent_path, intent)
    else:
        intent = _fallback_context_io_metadata(root, event, encoded)
        _atomic_json(intent_path, intent)

    intent = _relocate_occupied_context_io_offsets(
        root, event, encoded, intent_path, intent
    )

    event_path = _event_path(root, event)
    if not _record_matches(
        event_path, offset=intent["canonical_offset"], encoded=encoded
    ):
        if _file_size(event_path) != intent["canonical_offset"]:
            raise OSError("context-I/O canonical append position is occupied")
        offset, length = _append_event(root, event)
        if (offset, length) != (intent["canonical_offset"], intent["canonical_length"]):
            raise OSError("context-I/O canonical append offset changed")
    task_id = event.get("task_id")
    index_path = _task_index_path(root, task_id) if isinstance(task_id, str) else None
    if index_path is not None and not _record_matches(
        index_path, offset=intent["task_index_offset"], encoded=encoded
    ):
        if _file_size(index_path) != intent["task_index_offset"]:
            raise OSError("context-I/O task-index append position is occupied")
        appended = _append_task_index(root, event)
        if appended != (intent["task_index_offset"], intent["task_index_length"]):
            raise OSError("context-I/O task-index append offset changed")
    if not _verify_context_io_metadata(root, event, encoded, intent):
        raise OSError("context-I/O durable verification failed")
    _atomic_json(receipt_path, intent)
    committed, committed_state = _read_context_io_metadata(receipt_path)
    if (
        committed_state != "VALID"
        or not _valid_context_io_metadata(committed, event, encoded)
        or not _verify_context_io_metadata(root, event, encoded, committed)
    ):
        raise OSError("context-I/O receipt verification failed")
    intent_path.unlink(missing_ok=True)


def _spool_event(root: Path, event: Mapping[str, Any]) -> None:
    path = root / "events" / "spool" / f"{event['event_id']}.json"
    _atomic_json(path, event)


def _merge_spool(root: Path) -> None:
    spool = root / "events" / "spool"
    if not spool.exists():
        return
    merged = spool / ".merged"
    merged.mkdir(parents=True, exist_ok=True)
    for path in sorted(spool.glob("*.json"))[:SPOOL_MERGE_LIMIT]:
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
            if not schema.valid_event(value):
                raise ValueError("invalid spooled event")
        except (OSError, UnicodeDecodeError, json.JSONDecodeError, ValueError):
            _record_failure(root, "MALFORMED_SPOOL_EVENT")
            try:
                path.unlink()
            except OSError:
                pass
            continue
        try:
            marker = merged / str(value["event_id"])
            if not marker.exists():
                # Claim the identity before append. A crash may drop this
                # best-effort event, but can never duplicate it on recovery.
                marker.touch(exist_ok=False)
                _append_event(root, value)
                _append_task_index(root, value)
            path.unlink()
        except OSError:
            _record_failure(root, "TELEMETRY_SPOOL_MERGE_FAILED")


def _read_health(root: Path) -> dict[str, Any]:
    try:
        value = json.loads((root / "telemetry_health.json").read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return {"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": None}
    return dict(value) if isinstance(value, Mapping) else {
        "schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": None
    }


def _short_stderr(code: str) -> None:
    global _stderr_reported
    if _stderr_reported:
        return
    _stderr_reported = True
    try:
        sys.stderr.write(f"orchestrator telemetry degraded: {code}\n")
    except Exception:
        pass


def _record_failure(root: Path, code: str) -> None:
    code = code if code in _HEALTH_ERROR_CODES else "TELEMETRY_HEALTH_FAILED"
    try:
        health = _read_health(root)
        count = health.get("failure_count", 0)
        health.update(
            schema_version=1,
            status="DEGRADED",
            failure_count=(count if isinstance(count, int) and count >= 0 else 0) + 1,
            last_error_code=code,
            updated_at=schema.utc_now(),
        )
        _atomic_json(root / "telemetry_health.json", health)
    except Exception:
        pass
    _short_stderr(code)


def _record_projection_success(root: Path, written_at: str) -> None:
    health = _read_health(root)
    health.update(schema_version=1, last_projection_write_at=written_at)
    _atomic_json(root / "telemetry_health.json", health)


def record_health(
    status: str = "OK",
    *,
    error_code: str | None = None,
    state_root: str | os.PathLike[str] | None = None,
) -> None:
    try:
        root = _root(state_root)
        if status not in {"OK", "DEGRADED"}:
            raise ValueError("invalid telemetry health status")
        if status == "DEGRADED":
            _record_failure(
                root,
                error_code if error_code in _HEALTH_ERROR_CODES else "TELEMETRY_HEALTH_FAILED",
            )
            return None
        health = _read_health(root)
        health.update(schema_version=1, status="OK", last_error_code=None, updated_at=schema.utc_now())
        _atomic_json(root / "telemetry_health.json", health)
    except BaseException:
        _short_stderr("TELEMETRY_HEALTH_FAILED")
        return None
    return None


def emit(event_type: str, *, task_id: Any = None, **payload: Any) -> None:
    """Append one schema-checked event; arbitrary caller text is never serialised."""

    state_root = payload.pop("state_root", None)
    envelope = {name: payload.pop(name, None) for name in ("session_id", "repo", "branch", "agent_id", "agent_type")}
    try:
        root = _root(state_root)
        if event_type in _CLAUDE_USAGE_EVENTS:
            for field in schema.CLAUDE_USAGE_PAYLOAD_FIELDS:
                payload.pop(field, None)
            payload.update(_claude_usage_payload(root, event_type))
        event = schema.build_event(event_type, task_id=task_id, payload=payload, **envelope)
        with _try_lock(root) as acquired:
            if acquired:
                _merge_spool(root)
                _append_event(root, event)
                _append_task_index(root, event)
            else:
                _spool_event(root, event)
    except BaseException:
        try:
            _record_failure(_root(state_root), "TELEMETRY_EMIT_FAILED")
        except BaseException:
            _short_stderr("TELEMETRY_EMIT_FAILED")
    return None


def build_context_io_turn_event(
    payload: Mapping[str, Any],
    *,
    task_id: Any,
    event_id: str,
) -> dict[str, Any]:
    uuid.UUID(event_id)
    event = schema.build_event(
        "CONTEXT_IO_TURN_RECORDED", task_id=task_id, payload=payload
    )
    event["event_id"] = event_id
    if not schema.valid_event(event):
        raise ValueError("context-I/O event is invalid")
    return event


def emit_context_io_turn(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
) -> bool:
    """Durably accept one frozen context-I/O event with at-most-once identity."""

    try:
        root = _root(state_root)
        if (
            not schema.valid_event(event)
            or event.get("event_type") != "CONTEXT_IO_TURN_RECORDED"
        ):
            raise ValueError("context-I/O event is invalid")
        with _try_lock(root) as acquired:
            if acquired:
                _merge_spool(root)
                _persist_context_io_event(root, event)
                durable = True
            else:
                durable = False
        task_id = event.get("task_id")
        if task_id is not None:
            update_task(task_id, state_root=state_root)
        return durable
    except BaseException:
        try:
            _record_failure(_root(state_root), "CONTEXT_IO_EMIT_FAILED")
        except BaseException:
            _short_stderr("CONTEXT_IO_EMIT_FAILED")
        return False


def _usage_defaults() -> dict[str, Any]:
    values: dict[str, Any] = {
        "codex_call_count": 0,
        "codex_duration_ms": None,
        "codex_usage_known": False,
        "gemini_duration_ms": None,
        "gemini_usage_known": False,
        "deepseek_usage_known": False,
        "claude_5h_start_pct": None,
        "claude_5h_end_pct": None,
        "claude_5h_delta_pct": None,
        "claude_7d_start_pct": None,
        "claude_7d_end_pct": None,
        "claude_context_used_percentage": None,
        "claude_session_estimated_cost_usd": None,
        "claude_usage_known": False,
        "claude_rate_limit_delta_scope": CLAUDE_RATE_LIMIT_DELTA_SCOPE,
        "claude_task_usage_source": None,
        "claude_task_usage_known": False,
        "claude_task_usage_complete": False,
        "claude_task_input_tokens": None,
        "claude_task_output_tokens": None,
        "claude_task_cache_creation_input_tokens": None,
        "claude_task_cache_read_input_tokens": None,
        "claude_task_total_observed_tokens": None,
        "claude_task_request_count": None,
        "claude_task_usage_boundary": None,
        "claude_task_usage_dedup": None,
        "claude_task_usage_unknown_reason": None,
        "claude_task_subagent_usage_included": False,
        "_claude_5h_start_session_id": None,
        "_claude_5h_start_captured_at": None,
        "_claude_5h_start_resets_at": None,
        "_claude_5h_end_session_id": None,
        "_claude_5h_end_captured_at": None,
        "_claude_5h_end_resets_at": None,
    }
    for provider in ("codex", "gemini", "deepseek"):
        for field in schema.TOKEN_FIELDS:
            values[f"{provider}_{field}"] = None
    return values


def _claude_number(value: Any) -> int | float | None:
    if (
        not isinstance(value, (int, float))
        or isinstance(value, bool)
        or not math.isfinite(value)
        or value < 0
    ):
        return None
    return value


def _claude_timestamp(value: Any) -> datetime | None:
    if not isinstance(value, str) or len(value) > 40:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.utcoffset() != timezone.utc.utcoffset(parsed):
        return None
    return parsed


def _claude_session_id(value: Any) -> str | None:
    if not isinstance(value, str) or not value or len(value) > 128 or not value.isascii():
        return None
    if not all(character.isalnum() or character in ".-_" for character in value):
        return None
    return value


def _claude_usage_payload(root: Path, event_type: str) -> dict[str, Any]:
    """Read one bounded, point-in-time account observation for a task endpoint."""

    payload: dict[str, Any] = {
        "claude_usage_known": False,
        "claude_rate_limit_delta_scope": CLAUDE_RATE_LIMIT_DELTA_SCOPE,
    }
    endpoint = "start" if event_type == "IMPLEMENTER_STARTED" else "end"
    try:
        path = root / "latest" / "claude_usage.json"
        with path.open("rb") as handle:
            raw = handle.read(CLAUDE_USAGE_MAX_BYTES + 1)
        if len(raw) > CLAUDE_USAGE_MAX_BYTES:
            return payload
        snapshot = json.loads(raw)
        if not isinstance(snapshot, Mapping) or snapshot.get("schema_version") != 1:
            return payload
        captured_at = _claude_timestamp(snapshot.get("captured_at"))
        observed_at = datetime.now(timezone.utc)
        if (
            captured_at is None
            or captured_at > observed_at
            or (observed_at - captured_at).total_seconds() > CLAUDE_USAGE_MAX_STALENESS_SECONDS
        ):
            return payload

        payload["claude_usage_captured_at"] = snapshot["captured_at"]
        session_id = _claude_session_id(snapshot.get("session_id"))
        if session_id is not None:
            payload["claude_usage_session_id"] = session_id

        for source, target in (
            ("context_used_percentage", "claude_context_used_percentage"),
            ("context_window_size", "claude_context_window_size"),
            ("context_total_tokens", "claude_context_total_tokens"),
            ("context_input_tokens", "claude_context_input_tokens"),
            ("context_output_tokens", "claude_context_output_tokens"),
            ("cache_creation_input_tokens", "claude_cache_creation_input_tokens"),
            ("cache_read_input_tokens", "claude_cache_read_input_tokens"),
            ("session_estimated_cost_usd", "claude_session_estimated_cost_usd"),
            ("session_total_duration_ms", "claude_session_total_duration_ms"),
        ):
            number = _claude_number(snapshot.get(source))
            if number is not None:
                payload[target] = number
                payload["claude_usage_known"] = True

        for window_name, prefix in (("five_hour", "claude_5h"), ("seven_day", "claude_7d")):
            window = snapshot.get(window_name)
            if not isinstance(window, Mapping):
                continue
            number = _claude_number(window.get("used_percentage"))
            if number is not None:
                payload[f"{prefix}_{endpoint}_pct"] = number
                payload["claude_usage_known"] = True
            resets_at = window.get("resets_at")
            if _claude_timestamp(resets_at) is not None:
                payload[f"{prefix}_resets_at"] = resets_at
    except BaseException:
        return {
            "claude_usage_known": False,
            "claude_rate_limit_delta_scope": CLAUDE_RATE_LIMIT_DELTA_SCOPE,
        }
    return payload


def _projection_settings() -> tuple[bool, Path]:
    """Read the optional local projection setting without making it authoritative."""

    try:
        config = json.loads(_CONFIG_PATH.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return False, obsidian_projection.DEFAULT_VAULT_ROOT
    if not isinstance(config, Mapping):
        return False, obsidian_projection.DEFAULT_VAULT_ROOT

    setting = config.get("obsidian_projection")
    if isinstance(setting, Mapping):
        enabled = setting.get("enabled") is True
        configured_root = setting.get("vault_root")
    elif isinstance(setting, bool):
        enabled = setting
        configured_root = config.get("obsidian_vault_root")
    else:
        enabled = config.get("obsidian_projection_enabled") is True
        configured_root = config.get("obsidian_vault_root")
    if not enabled:
        return False, obsidian_projection.DEFAULT_VAULT_ROOT
    if configured_root is None:
        return True, obsidian_projection.DEFAULT_VAULT_ROOT
    if not isinstance(configured_root, str) or not configured_root.strip():
        raise ValueError("obsidian projection vault_root is invalid")
    return True, Path(configured_root)


def _project_task(
    task_id: str,
    *,
    state_root: Path,
    projection_enabled: bool | None,
    vault_root: str | os.PathLike[str] | None,
) -> None:
    target_vault: str | os.PathLike[str] | None = None
    try:
        if projection_enabled is None and vault_root is not None:
            enabled, configured_root = True, Path(vault_root)
        elif projection_enabled is None:
            enabled, configured_root = _projection_settings()
        else:
            enabled, configured_root = projection_enabled, obsidian_projection.DEFAULT_VAULT_ROOT
        if not enabled:
            return
        target_vault = vault_root if vault_root is not None else configured_root
        obsidian_projection.project_task(
            task_id,
            state_root=state_root.parent,
            vault_root=target_vault,
        )
        projection_written_at = schema.utc_now()
        _record_projection_success(state_root, projection_written_at)
        obsidian_projection.project_health(
            state_root=state_root.parent,
            vault_root=target_vault,
            last_projection_write_at=projection_written_at,
        )
    except BaseException:
        _record_failure(state_root, "TELEMETRY_PROJECTION_FAILED")
        if target_vault is not None:
            try:
                obsidian_projection.project_health(
                    state_root=state_root.parent,
                    vault_root=target_vault,
                )
            except BaseException:
                pass


def _hash_file(path: Path) -> str | None:
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError:
        return None


def _policy_hashes() -> tuple[dict[str, str | None], str | None]:
    skill_hash = _hash_file(_SKILL_PATH)
    config_hash = _hash_file(_PACKAGE_ROOT / "config.json")
    sources = sorted(path for path in _PACKAGE_ROOT.glob("*.py") if path.is_file())
    if not sources:
        hook_hash = None
    else:
        digest = hashlib.sha256()
        try:
            for path in sources:
                digest.update(path.name.encode("utf-8") + b"\0" + path.read_bytes() + b"\0")
            if config_hash is not None:
                digest.update(b"config\0" + bytes.fromhex(config_hash))
            hook_hash = digest.hexdigest()
        except OSError:
            hook_hash = None
    fingerprint = f"{skill_hash[:8]}-{hook_hash[:8]}" if skill_hash and hook_hash else None
    return {"skill_sha256": skill_hash, "config_sha256": config_hash, "hook_sha256": hook_hash}, fingerprint


def _new_aggregate(
    task_id: str,
    existing: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    if (
        isinstance(existing, Mapping)
        and "policy_hashes" in existing
        and "policy_fingerprint" in existing
    ):
        hashes = existing["policy_hashes"]
        fingerprint = existing["policy_fingerprint"]
    else:
        hashes, fingerprint = _policy_hashes()
    return {
        "task_id": task_id,
        "repo": None,
        "branch": None,
        "status": "UNKNOWN_OR_IN_PROGRESS",
        "started_at": None,
        "ended_at": None,
        "duration_ms": None,
        "phase": None,
        "route": None,
        "model": None,
        "effort": None,
        "validation": None,
        "thread_id": None,
        "fresh_threads": 0,
        "reply_calls": 0,
        "routing_enforced": None,
        "reviewer1_provider": None,
        "reviewer1_model_requested": None,
        "served_model_attestation": "UNAVAILABLE",
        "reviewer1_result": None,
        "reviewer1_skipped_reason": None,
        "claude_review_mode": None,
        "changed_paths_count": None,
        "routing_protocol_violations": 0,
        "thread_continuity_violations": 0,
        "concurrent_writer": False,
        "policy_fingerprint": fingerprint,
        "policy_hashes": hashes,
        "usage": _usage_defaults(),
        "context_io": None,
    }


def _add_duration(current: Any, value: Any) -> int | float | None:
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        return current
    return value if current is None else current + value


def _claude_reset_crossed(usage: Mapping[str, Any]) -> bool:
    start_captured_at = _claude_timestamp(usage.get("_claude_5h_start_captured_at"))
    end_captured_at = _claude_timestamp(usage.get("_claude_5h_end_captured_at"))
    start_reset_at = _claude_timestamp(usage.get("_claude_5h_start_resets_at"))
    end_reset_at = _claude_timestamp(usage.get("_claude_5h_end_resets_at"))
    if start_reset_at is not None and end_reset_at is not None and start_reset_at != end_reset_at:
        return True
    return (
        start_captured_at is not None
        and end_captured_at is not None
        and start_captured_at <= end_captured_at
        and start_reset_at is not None
        and start_captured_at <= start_reset_at <= end_captured_at
    )


def _recompute_claude_delta(usage: dict[str, Any]) -> None:
    start = _claude_number(usage.get("claude_5h_start_pct"))
    end = _claude_number(usage.get("claude_5h_end_pct"))
    start_session = usage.get("_claude_5h_start_session_id")
    end_session = usage.get("_claude_5h_end_session_id")
    if (
        start is None
        or end is None
        or not isinstance(start_session, str)
        or not isinstance(end_session, str)
        or start_session != end_session
        or _claude_reset_crossed(usage)
    ):
        usage["claude_5h_delta_pct"] = None
        return
    start_captured_at = _claude_timestamp(usage.get("_claude_5h_start_captured_at"))
    end_captured_at = _claude_timestamp(usage.get("_claude_5h_end_captured_at"))
    if start_captured_at is not None and end_captured_at is not None and end_captured_at < start_captured_at:
        usage["claude_5h_delta_pct"] = None
        return
    usage["claude_5h_delta_pct"] = end - start


def _fold_claude_usage(aggregate: dict[str, Any], payload: Mapping[str, Any], endpoint: str) -> None:
    usage = aggregate["usage"]
    if payload.get("claude_usage_known") is True:
        usage["claude_usage_known"] = True
    if payload.get("claude_rate_limit_delta_scope") == CLAUDE_RATE_LIMIT_DELTA_SCOPE:
        usage["claude_rate_limit_delta_scope"] = CLAUDE_RATE_LIMIT_DELTA_SCOPE
    for field in ("claude_context_used_percentage", "claude_session_estimated_cost_usd"):
        number = _claude_number(payload.get(field))
        if number is not None:
            usage[field] = number

    endpoint_field = f"claude_5h_{endpoint}_pct"
    endpoint_value = _claude_number(payload.get(endpoint_field))
    if endpoint == "start":
        if endpoint_value is not None and usage.get("claude_5h_start_pct") is None:
            usage["claude_5h_start_pct"] = endpoint_value
            for metadata in ("session_id", "captured_at", "resets_at"):
                usage[f"_claude_5h_start_{metadata}"] = payload.get(
                    "claude_usage_session_id"
                    if metadata == "session_id"
                    else "claude_usage_captured_at"
                    if metadata == "captured_at"
                    else "claude_5h_resets_at"
                )
    elif endpoint_value is not None:
        usage["claude_5h_end_pct"] = endpoint_value
        for metadata in ("session_id", "captured_at", "resets_at"):
            usage[f"_claude_5h_end_{metadata}"] = payload.get(
                "claude_usage_session_id"
                if metadata == "session_id"
                else "claude_usage_captured_at"
                if metadata == "captured_at"
                else "claude_5h_resets_at"
            )
    seven_day_value = _claude_number(payload.get(f"claude_7d_{endpoint}_pct"))
    if seven_day_value is not None:
        usage[f"claude_7d_{endpoint}_pct"] = seven_day_value
    _recompute_claude_delta(usage)


def _fold_event(aggregate: dict[str, Any], event: Mapping[str, Any]) -> None:
    event_type = event["event_type"]
    payload = event["payload"]
    for field in ("repo", "branch"):
        if aggregate[field] is None and event.get(field) is not None:
            aggregate[field] = event[field]
    if event_type == "TASK_ROUTE_SELECTED":
        for field in ("phase", "route", "model", "effort", "routing_enforced"):
            if field in payload:
                aggregate[field] = payload[field]
    elif event_type == "IMPLEMENTER_STARTED":
        aggregate["status"] = "IN_PROGRESS"
        aggregate["started_at"] = aggregate["started_at"] or event["ts_utc"]
        aggregate["fresh_threads"] += int(payload.get("fresh_thread") is True)
        aggregate["reply_calls"] += int(payload.get("reply_call") is True)
        aggregate["usage"]["codex_call_count"] += 1
        _fold_claude_usage(aggregate, payload, "start")
    elif event_type in {"IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED"}:
        aggregate["ended_at"] = event["ts_utc"]
        aggregate["duration_ms"] = _add_duration(aggregate["duration_ms"], payload.get("duration_ms"))
        aggregate["usage"]["codex_duration_ms"] = _add_duration(
            aggregate["usage"]["codex_duration_ms"], payload.get("duration_ms")
        )
        for field in ("changed_paths_count", "thread_id", "claude_review_mode", "routing_enforced"):
            if field in payload:
                aggregate[field] = payload[field]
        if aggregate["validation"] is None and payload.get("validation") is not None:
            aggregate["validation"] = payload["validation"]
        if payload.get("codex_usage_known") is True:
            aggregate["usage"]["codex_usage_known"] = True
        for field in schema.TOKEN_FIELDS:
            key = f"codex_{field}"
            if key in payload:
                aggregate["usage"][key] = _add_duration(
                    aggregate["usage"][key], payload[key]
                )
        _fold_claude_usage(aggregate, payload, "end")
    elif event_type in {"REVIEWER1_STARTED", "REVIEWER1_FINISHED", "REVIEWER1_FAILED"}:
        if event_type == "REVIEWER1_STARTED":
            aggregate["reviewer1_result"] = None
            aggregate["reviewer1_skipped_reason"] = None
        else:
            aggregate["reviewer1_skipped_reason"] = None
        for field in ("reviewer1_provider", "reviewer1_model_requested", "served_model_attestation", "reviewer1_result"):
            if field in payload:
                aggregate[field] = payload[field]
        if event_type != "REVIEWER1_STARTED":
            aggregate["usage"]["gemini_duration_ms"] = _add_duration(
                aggregate["usage"]["gemini_duration_ms"], payload.get("duration_ms")
            )
            if payload.get("gemini_usage_known") is True:
                aggregate["usage"]["gemini_usage_known"] = True
            for field in schema.TOKEN_FIELDS:
                key = f"gemini_{field}"
                if key in payload:
                    aggregate["usage"][key] = _add_duration(
                        aggregate["usage"][key], payload[key]
                    )
    elif event_type == "REVIEWER1_SKIPPED":
        aggregate["reviewer1_provider"] = None
        aggregate["reviewer1_result"] = None
        aggregate["reviewer1_skipped_reason"] = payload.get("reviewer1_skipped_reason")
        aggregate["reviewer1_model_requested"] = payload.get("reviewer1_model_requested")
    elif event_type == "ROUTING_PROTOCOL_VIOLATION":
        aggregate["routing_protocol_violations"] += 1
    elif event_type == "THREAD_CONTINUITY_VIOLATION":
        aggregate["thread_continuity_violations"] += 1
    elif event_type == "UNKNOWN_CONCURRENT_WRITER":
        aggregate["concurrent_writer"] = True
    elif event_type == "CLAUDE_TASK_USAGE_RECORDED":
        usage = aggregate["usage"]
        current_reason = usage.get("claude_task_usage_unknown_reason")

        # If already in a terminal failure state, NEVER overwrite with known=True or complete=True
        if current_reason in TERMINAL_TASK_USAGE_FAILURES:
            return

        new_reason = payload.get("claude_task_usage_unknown_reason")
        known = payload.get("claude_task_usage_known") is True and new_reason not in TERMINAL_TASK_USAGE_FAILURES

        if new_reason in TERMINAL_TASK_USAGE_FAILURES or not known:
            usage["claude_task_usage_known"] = False
            usage["claude_task_usage_complete"] = False
            usage["claude_task_usage_unknown_reason"] = new_reason
            usage["claude_task_usage_source"] = None
            usage["claude_task_usage_boundary"] = None
            usage["claude_task_usage_dedup"] = None
            for token_field in (
                "claude_task_input_tokens",
                "claude_task_output_tokens",
                "claude_task_cache_creation_input_tokens",
                "claude_task_cache_read_input_tokens",
                "claude_task_total_observed_tokens",
                "claude_task_request_count",
            ):
                usage[token_field] = None
            usage["claude_task_subagent_usage_included"] = False
        else:
            usage["claude_task_usage_known"] = True
            usage["claude_task_usage_complete"] = payload.get("claude_task_usage_complete") is True
            usage["claude_task_usage_unknown_reason"] = None
            usage["claude_task_usage_source"] = payload.get("claude_task_usage_source")
            usage["claude_task_usage_boundary"] = payload.get("claude_task_usage_boundary")
            usage["claude_task_usage_dedup"] = payload.get("claude_task_usage_dedup")
            usage["claude_task_subagent_usage_included"] = (
                payload.get("claude_task_subagent_usage_included") is True
            )
            for token_field in (
                "claude_task_input_tokens",
                "claude_task_output_tokens",
                "claude_task_cache_creation_input_tokens",
                "claude_task_cache_read_input_tokens",
                "claude_task_total_observed_tokens",
                "claude_task_request_count",
            ):
                val = payload.get(token_field)
                if isinstance(val, int) and not isinstance(val, bool) and val >= 0:
                    usage[token_field] = val
    elif event_type == "CONTEXT_IO_TURN_RECORDED":
        aggregate["context_io"] = dict(payload)


def _raw_events(root: Path) -> Iterable[dict[str, Any]]:
    events_root = root / "events"
    if not events_root.exists():
        return
    for path in sorted(events_root.glob("????-??/session_*.jsonl")):
        try:
            with path.open("r", encoding="utf-8") as handle:
                for line in handle:
                    try:
                        value = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if schema.valid_event(value):
                        yield value
        except (OSError, UnicodeDecodeError):
            continue
    spool = events_root / "spool"
    for path in sorted(spool.glob("*.json")) if spool.exists() else ():
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
            marker = spool / ".merged" / str(value.get("event_id"))
            if schema.valid_event(value) and not marker.exists():
                yield value
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            continue


def _cursor_path(root: Path, task_id: str) -> Path:
    return root / "tasks" / ".cursors" / f"{task_id}.json"


def _read_cursor(root: Path, task_id: str) -> tuple[int, set[str]]:
    try:
        value = json.loads(_cursor_path(root, task_id).read_text(encoding="utf-8"))
    except FileNotFoundError:
        return 0, set()
    if (
        not isinstance(value, Mapping)
        or set(value) != {"schema_version", "index_offset", "spool_event_ids"}
        or value.get("schema_version") != _CURSOR_SCHEMA_VERSION
        or not isinstance(value.get("index_offset"), int)
        or isinstance(value.get("index_offset"), bool)
        or value["index_offset"] < 0
        or not isinstance(value.get("spool_event_ids"), list)
        or not all(isinstance(event_id, str) for event_id in value["spool_event_ids"])
    ):
        raise ValueError("malformed task telemetry cursor")
    return value["index_offset"], set(value["spool_event_ids"])


def _write_cursor(root: Path, task_id: str, index_offset: int, spool_event_ids: set[str]) -> None:
    _atomic_json(
        _cursor_path(root, task_id),
        {
            "schema_version": _CURSOR_SCHEMA_VERSION,
            "index_offset": index_offset,
            "spool_event_ids": sorted(spool_event_ids),
        },
    )


def _task_index_events(
    root: Path,
    task_id: str,
    index_offset: int,
) -> tuple[list[dict[str, Any]], int]:
    path = _task_index_path(root, task_id)
    if not path.exists():
        if index_offset:
            raise ValueError("task telemetry index disappeared")
        return [], 0
    size = path.stat().st_size
    if index_offset > size:
        raise ValueError("task telemetry index was truncated")
    events: list[dict[str, Any]] = []
    consumed = index_offset
    with path.open("rb") as handle:
        handle.seek(index_offset)
        for raw_line in handle.read().splitlines(keepends=True):
            if not raw_line.endswith(b"\n"):
                break
            consumed += len(raw_line)
            try:
                value = json.loads(raw_line.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                continue
            if schema.valid_event(value) and value.get("task_id") == task_id:
                events.append(dict(value))
    return events, consumed


def _unmerged_spool_events(root: Path, task_id: str) -> list[dict[str, Any]]:
    spool = root / "events" / "spool"
    if not spool.exists():
        return []
    merged = spool / ".merged"
    events: list[dict[str, Any]] = []
    for path in sorted(spool.glob("*.json")):
        if (merged / path.stem).exists():
            continue
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            continue
        if schema.valid_event(value) and value.get("task_id") == task_id:
            events.append(dict(value))
    return events


def _verdict(root: Path, task_id: str) -> tuple[str, str] | None:
    path = root / "verdicts" / f"{task_id}.json"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        if (
            not isinstance(value, Mapping)
            or set(value) != {"schema_version", "task_id", "verdict", "recorded_at"}
            or value.get("schema_version") != 1
            or schema.normalize_identifier(value.get("task_id")) != task_id
            or value.get("verdict") not in schema.TERMINAL_VERDICTS
            or not isinstance(value.get("recorded_at"), str)
            or len(value["recorded_at"]) > 40
        ):
            raise ValueError("malformed verdict")
        recorded_at = datetime.fromisoformat(value["recorded_at"].replace("Z", "+00:00"))
        if recorded_at.utcoffset() is None or recorded_at.utcoffset().total_seconds() != 0:
            raise ValueError("verdict timestamp is not UTC")
        return value["verdict"], value["recorded_at"]
    except FileNotFoundError:
        return None
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, TypeError, ValueError):
        emit("TELEMETRY_WARNING", task_id=task_id, state_root=root.parent, warning_code="MALFORMED_VERDICT")
        return None


def _build_task(
    root: Path,
    task_id: str,
    events: Iterable[Mapping[str, Any]],
    *,
    incremental: bool = True,
) -> dict[str, Any]:
    path = root / "tasks" / f"{task_id}.json"
    try:
        existing = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        existing = None
    aggregate = _new_aggregate(task_id, existing if isinstance(existing, Mapping) else None)
    if incremental and isinstance(existing, Mapping):
        for key in aggregate:
            if key in existing:
                aggregate[key] = existing[key]
        existing_usage = aggregate.get("usage")
        if isinstance(existing_usage, Mapping):
            usage = _usage_defaults()
            usage.update(existing_usage)
            aggregate["usage"] = usage
        else:
            aggregate["usage"] = _usage_defaults()
    for event in sorted(events, key=lambda item: (str(item["ts_utc"]), str(item["event_id"]))):
        _fold_event(aggregate, event)
    verdict = _verdict(root, task_id)
    if verdict is not None:
        aggregate["status"], aggregate["ended_at"] = verdict
    return aggregate


def update_task(
    task_id: Any,
    *,
    state_root: str | os.PathLike[str] | None = None,
    projection_enabled: bool | None = None,
    vault_root: str | os.PathLike[str] | None = None,
) -> None:
    try:
        root = _root(state_root)
        normalized = schema.normalize_identifier(task_id)
        index_offset, consumed_spool_ids = _read_cursor(root, normalized)
        indexed_events, index_offset = _task_index_events(root, normalized, index_offset)
        consumed_from_index: set[str] = set()
        events: list[dict[str, Any]] = []
        for event in indexed_events:
            event_id = str(event["event_id"])
            if event_id in consumed_spool_ids:
                # The event may have been folded while it was still in the
                # spool. Its later index entry advances the cursor without a
                # second fold.
                consumed_spool_ids.remove(event_id)
                consumed_from_index.add(event_id)
            else:
                events.append(event)
        for event in _unmerged_spool_events(root, normalized):
            event_id = str(event["event_id"])
            if event_id in consumed_from_index or event_id in consumed_spool_ids:
                continue
            events.append(event)
            consumed_spool_ids.add(event_id)
        aggregate = _build_task(root, normalized, events)
        _atomic_json(root / "tasks" / f"{normalized}.json", aggregate)
        _write_cursor(root, normalized, index_offset, consumed_spool_ids)
        _project_task(
            normalized,
            state_root=root,
            projection_enabled=projection_enabled,
            vault_root=vault_root,
        )
    except BaseException:
        try:
            _record_failure(_root(state_root), "TELEMETRY_UPDATE_TASK_FAILED")
        except BaseException:
            _short_stderr("TELEMETRY_UPDATE_TASK_FAILED")
    return None


def rebuild(*, state_root: str | os.PathLike[str] | None = None) -> None:
    try:
        root = _root(state_root)
        grouped: dict[str, list[dict[str, Any]]] = {}
        for event in _raw_events(root):
            task_id = event.get("task_id")
            if isinstance(task_id, str):
                grouped.setdefault(task_id, []).append(event)
        verdict_root = root / "verdicts"
        if verdict_root.exists():
            for path in verdict_root.glob("*.json"):
                grouped.setdefault(schema.normalize_identifier(path.stem), [])
        tasks_root = root / "tasks"
        tasks_root.mkdir(parents=True, exist_ok=True)
        for task_id, events in grouped.items():
            _atomic_json(
                tasks_root / f"{task_id}.json",
                _build_task(root, task_id, events, incremental=False),
            )
            _write_cursor(
                root,
                task_id,
                _task_index_path(root, task_id).stat().st_size
                if _task_index_path(root, task_id).exists()
                else 0,
                {
                    str(event["event_id"])
                    for event in _unmerged_spool_events(root, task_id)
                },
            )
        cursor_root = tasks_root / ".cursors"
        if cursor_root.exists():
            expected_cursors = {f"{task_id}.json" for task_id in grouped}
            for path in cursor_root.glob("*.json"):
                if path.name not in expected_cursors:
                    path.unlink()
    except BaseException:
        try:
            _record_failure(_root(state_root), "TELEMETRY_REBUILD_FAILED")
        except BaseException:
            _short_stderr("TELEMETRY_REBUILD_FAILED")
    return None


__all__ = ["emit", "rebuild", "record_health", "update_task"]
