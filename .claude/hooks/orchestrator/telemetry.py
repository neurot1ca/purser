"""Fail-open raw telemetry persistence for the orchestrator hooks."""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from contextlib import contextmanager
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
from typing import Any, Iterator

import telemetry_schema as schema


LOCK_TIMEOUT_SECONDS = 0.05
SPOOL_MERGE_LIMIT = 16
_PACKAGE_ROOT = Path(__file__).resolve().parent
_SKILL_PATH = _PACKAGE_ROOT.parents[1] / "skills" / "codex-orchestrator" / "SKILL.md"
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
        "MALFORMED_SPOOL_EVENT",
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
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass


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


def _append_event(root: Path, event: Mapping[str, Any]) -> None:
    path = _event_path(root, event)
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(event, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    with path.open("a", encoding="utf-8", newline="\n") as handle:
        handle.write(encoded + "\n")
        handle.flush()
        os.fsync(handle.fileno())


def _task_index_path(root: Path, task_id: str) -> Path:
    return root / "events" / "by_task" / f"{task_id}.jsonl"


def _append_task_index(root: Path, event: Mapping[str, Any]) -> None:
    """Persist a bounded-read feed without changing the raw event schema."""

    task_id = event.get("task_id")
    if not isinstance(task_id, str):
        return
    path = _task_index_path(root, task_id)
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(event, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    with path.open("a", encoding="utf-8", newline="\n") as handle:
        handle.write(encoded + "\n")
        handle.flush()
        os.fsync(handle.fileno())


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


def _usage_defaults() -> dict[str, Any]:
    values: dict[str, Any] = {
        "codex_call_count": 0,
        "codex_duration_ms": None,
        "codex_usage_known": False,
        "gemini_duration_ms": None,
        "gemini_usage_known": False,
        "deepseek_usage_known": False,
    }
    for provider in ("codex", "gemini", "deepseek"):
        for field in schema.TOKEN_FIELDS:
            values[f"{provider}_{field}"] = None
    return values


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
        "status": "UNKNOWN_OR_IN_PROGRESS",
        "started_at": None,
        "ended_at": None,
        "duration_ms": None,
        "phase": None,
        "route": None,
        "model": None,
        "effort": None,
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
    }


def _add_duration(current: Any, value: Any) -> int | float | None:
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        return current
    return value if current is None else current + value


def _fold_event(aggregate: dict[str, Any], event: Mapping[str, Any]) -> None:
    event_type = event["event_type"]
    payload = event["payload"]
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
    elif event_type in {"IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED"}:
        aggregate["ended_at"] = event["ts_utc"]
        aggregate["duration_ms"] = _add_duration(aggregate["duration_ms"], payload.get("duration_ms"))
        aggregate["usage"]["codex_duration_ms"] = _add_duration(
            aggregate["usage"]["codex_duration_ms"], payload.get("duration_ms")
        )
        for field in ("changed_paths_count", "thread_id", "claude_review_mode", "routing_enforced"):
            if field in payload:
                aggregate[field] = payload[field]
        if payload.get("codex_usage_known") is True:
            aggregate["usage"]["codex_usage_known"] = True
        for field in schema.TOKEN_FIELDS:
            key = f"codex_{field}"
            if key in payload:
                aggregate["usage"][key] = _add_duration(
                    aggregate["usage"][key], payload[key]
                )
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
    for event in sorted(events, key=lambda item: (str(item["ts_utc"]), str(item["event_id"]))):
        _fold_event(aggregate, event)
    verdict = _verdict(root, task_id)
    if verdict is not None:
        aggregate["status"], aggregate["ended_at"] = verdict
    return aggregate


def update_task(task_id: Any, *, state_root: str | os.PathLike[str] | None = None) -> None:
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
        expected = {f"{task_id}.json" for task_id in grouped}
        for path in tasks_root.glob("*.json"):
            if path.name not in expected:
                path.unlink()
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
