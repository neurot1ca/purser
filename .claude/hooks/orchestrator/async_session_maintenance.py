"""Bounded, fail-open SessionStart hints and retention for Codex async jobs."""

from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
from itertools import islice
import math
from pathlib import Path
import re
import statistics
import time
from typing import Any

import codex_exec_mcp
import codex
import ledger
import telemetry


RETENTION_SECONDS = 7 * 86400
HISTORY_SECONDS = 90 * 86400
MAX_JOB_RECORDS = 256
MAX_JOB_RECORD_BYTES = 64 * 1024
MAX_TELEMETRY_FILES = 32
MAX_TELEMETRY_BYTES = 2 * 1024 * 1024
MAX_TELEMETRY_LINES = 4096
TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS", "EXPIRED"}
_JOB_ID = re.compile(r"[0-9a-f]{64}")


def _number_minutes(value: float) -> int:
    return max(1, math.floor(value / 60000 + 0.5))


def duration_hint(state_root: str | Path, project_key: str, *, now: float | None = None) -> str | None:
    """Build one calibration line from a bounded, newest-first raw-event scan."""
    try:
        current = time.time() if now is None else now
        current_dt = datetime.fromtimestamp(current, timezone.utc)
        cutoff = datetime.fromtimestamp(current - HISTORY_SECONDS, timezone.utc)
        root = Path(state_root) / "telemetry" / "events"
        month = cutoff.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
        final_month = current_dt.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
        files: list[Path] = []
        while month <= final_month:
            directory = root / month.strftime("%Y-%m")
            if directory.is_dir():
                files.extend(directory.glob("session_*.jsonl"))
            next_year = month.year + 1 if month.month == 12 else month.year
            next_month = 1 if month.month == 12 else month.month + 1
            month = month.replace(year=next_year, month=next_month)
        files.sort(key=lambda path: path.stat().st_mtime, reverse=True)
        files = files[:MAX_TELEMETRY_FILES]
        grouped: dict[str, list[float]] = {}
        scanned = lines = 0
        for path in files:
            with path.open("rb") as stream:
                while scanned < MAX_TELEMETRY_BYTES and lines < MAX_TELEMETRY_LINES:
                    raw = stream.readline(MAX_TELEMETRY_BYTES - scanned)
                    if not raw:
                        break
                    scanned += len(raw)
                    lines += 1
                    try:
                        item = json.loads(raw)
                    except (ValueError, UnicodeDecodeError):
                        continue
                    if not isinstance(item, dict):
                        continue
                    if item.get("event_type") != "IMPLEMENTER_FINISHED" or item.get("project_key") != project_key:
                        continue
                    payload = item.get("payload")
                    if not isinstance(payload, dict):
                        continue
                    route = payload.get("route")
                    duration = payload.get("duration_ms")
                    if not isinstance(route, str) or route not in {"mechanical", "subtle", "normal", "consequential", "escalated", "operation"}:
                        continue
                    if isinstance(duration, bool) or not isinstance(duration, (int, float)):
                        continue
                    try:
                        if not math.isfinite(duration) or duration <= 0:
                            continue
                    except (OverflowError, TypeError):
                        continue
                    try:
                        parsed = datetime.fromisoformat(str(item.get("ts_utc")).replace("Z", "+00:00"))
                    except (ValueError, TypeError, OverflowError):
                        continue
                    try:
                        in_window = parsed.tzinfo is not None and cutoff <= parsed.astimezone(timezone.utc) <= current_dt
                    except (OverflowError, ValueError):
                        continue
                    if not in_window:
                        continue
                    grouped.setdefault(route, []).append(float(duration))
            if scanned >= MAX_TELEMETRY_BYTES or lines >= MAX_TELEMETRY_LINES:
                break
        parts = []
        for route in ("mechanical", "subtle", "normal", "consequential", "escalated", "operation"):
            values = grouped.get(route, [])
            if len(values) < 10:
                continue
            ordered = sorted(values)
            p90 = ordered[math.ceil(0.9 * len(ordered)) - 1]
            parts.append(f"{route} {_number_minutes(statistics.median(ordered))}/{_number_minutes(p90)} min (n={len(ordered)})")
        return "Purser: typical Codex durations here (median/p90, n): " + ", ".join(parts) + "." if parts else None
    except BaseException:
        return None


def _read_json(path: Path) -> dict[str, Any] | None:
    try:
        if path.stat().st_size > MAX_JOB_RECORD_BYTES:
            return None
        value = json.loads(path.read_text(encoding="utf-8"))
        return value if isinstance(value, dict) else None
    except (OSError, ValueError):
        return None


def followup_hints(state_root: str | Path, project_key: str, session_id: str) -> str | None:
    """Remind a successor session of up to five verified, unconsumed jobs."""
    if not isinstance(session_id, str) or not session_id or not project_key:
        return None
    try:
        root = Path(state_root)
        lines = []
        paths = sorted(islice((root / "async_jobs").glob("*.json"), MAX_JOB_RECORDS),
                       key=lambda path: (path.stat().st_mtime_ns, path.name))
        for path in paths:
            if len(lines) == 5:
                break
            job_id = path.stem
            if not _JOB_ID.fullmatch(job_id):
                continue
            index = _read_json(path)
            if (not index or index.get("stage") == "CONSUMED"
                    or index.get("jobId") != job_id or index.get("project_key") != project_key
                    or index.get("session_id") == session_id
                    or not isinstance(index.get("session_id"), str)
                    or not isinstance(index.get("call"), str)
                    or hashlib.sha256(index["call"].encode("utf-8")).hexdigest() != job_id
                    or not _JOB_ID.fullmatch(str(index.get("call_key")))):
                continue
            pending = _read_json(root / "pending" / f"{index['call_key']}.json")
            job = _read_json(root / "codex_exec_mcp" / "jobs" / f"{job_id}.json")
            if (not pending or not job or pending.get("job_id") != job_id
                    or pending.get("async_call") != index["call"]
                    or pending.get("session_id") != index["session_id"]
                    or pending.get("project_key") != project_key
                    or job.get("jobId") != job_id or job.get("call") != index["call"]):
                continue
            tool_use_id = pending.get("tool_use_id")
            if (not isinstance(tool_use_id, str)
                    or hashlib.sha256(f"{index['session_id']}\0{tool_use_id}".encode("utf-8")).hexdigest()
                    != index["call_key"]):
                continue
            task = pending.get("task_id")
            if not isinstance(task, str) or not task:
                continue
            task = re.sub(r"\s+", " ", task).strip()[:80]
            state = job.get("state")
            if state in {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"}:
                message = (f"⚓ Uygulama kapanmadan önce başlattığım Codex işi ({task}) "
                           "uygulama kapalıyken çalışmaya devam etti ve bitti. Sonucunu alıp inceleyeyim mi?")
                instruction = "Ask for the user's agreement before calling codex_result with adopt: true."
            elif state in {"QUEUED", "RUNNING"}:
                message = (f"⚓ Uygulama kapanmadan önce başlattığım Codex işi ({task}) "
                           "uygulama kapalıyken çalışmaya devam ediyor, bitince haber vereceğim.")
                instruction = "When it finishes, ask for the user's agreement before calling codex_result with adopt: true."
            else:
                continue
            lines.append(f"Purser (jobId={job_id}, for Claude only): Tell the user in the user's language: "
                         f"\"{message}\" {instruction} Do not show the jobId to the user.")
        return "\n".join(lines) or None
    except BaseException:
        return None


def _write_json(path: Path, value: dict[str, Any]) -> None:
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, sort_keys=True), encoding="utf-8")
    temporary.replace(path)


def cleanup_jobs(state_root: str | Path, *, now: float | None = None) -> None:
    """Reconcile dead hosts and remove expired evidence with its ownership records."""
    try:
        root = Path(state_root)
        jobs = root / "codex_exec_mcp" / "jobs"
        bridge = codex_exec_mcp.Server(job_dir=jobs)
        current = time.time() if now is None else now
        for path in islice(jobs.glob("*.json"), MAX_JOB_RECORDS):
            if not _JOB_ID.fullmatch(path.stem):
                continue
            job = _read_json(path)
            if not job or job.get("jobId") != path.stem:
                continue
            if bridge._host_alive(job):
                continue
            if (job.get("state") == "QUEUED" and isinstance(job.get("acceptedAt"), (int, float))
                    and current - job["acceptedAt"] < 10):
                continue
            if job.get("state") not in TERMINAL:
                job.update(state="UNKNOWN_OR_IN_PROGRESS", finishedAt=current,
                           errorCode="PROCESS_UNCONFIRMED",
                           message="Host and child-tree termination unconfirmed")
                bridge._write_job(job)
                (jobs / f"{path.stem}.launch").unlink(missing_ok=True)
                continue
            (jobs / f"{path.stem}.launch").unlink(missing_ok=True)
            finished = job.get("finishedAt")
            if isinstance(finished, bool) or not isinstance(finished, (int, float)) or current - finished < RETENTION_SECONDS:
                continue
            try:
                with codex._async_claim(root / "async_jobs" / f"{path.stem}.json"):
                    index_path = root / "async_jobs" / f"{path.stem}.json"
                    index = _read_json(index_path)
                    if index_path.exists() and (not index or index.get("jobId") != path.stem
                            or index.get("call") != job.get("call")
                            or not isinstance(index.get("call_key"), str)
                            or not _JOB_ID.fullmatch(index["call_key"])
                            or not re.fullmatch(r"[0-9a-f]{16}", str(index.get("project_key")))
                            or index.get("stage") == "FINALIZING"):
                        continue
                    pending = None
                    pending_exists = False
                    pending_path = None
                    if index:
                        call_key = index.get("call_key")
                        pending_path = root / "pending" / f"{call_key}.json"
                        pending_exists = pending_path.is_file()
                        pending = _read_json(pending_path) if pending_exists else None
                        if pending_exists and (not pending or pending.get("job_id") != path.stem
                                               or pending.get("async_call") != index["call"]):
                            continue
                        owner = pending.get("lease_owner") if pending else call_key
                        if isinstance(owner, str):
                            lease = ledger.ScopeLeases(root / "projects" / index["project_key"])
                            released = lease.release(owner)
                            if released["status"] != ledger.OK and released["code"] != "LEASE_OWNER_MISSING":
                                continue
                    if pending_exists and index is not None and index.get("stage") != "CONSUMED" and not index.get("unfetched_event_emitted"):
                        index["unfetched_event_emitted"] = True
                        _write_json(index_path, index)
                        telemetry.emit("CODEX_RESULT_UNFETCHED",
                                       project_key=index.get("project_key"), route=pending.get("route") if pending else None,
                                       code="RESULT_NOT_FETCHED", state_root=root)
                    output_name = job.get("outputFile")
                    if isinstance(output_name, str) and output_name == path.stem + ".output":
                        output_path = jobs / output_name
                        if output_path.parent == jobs:
                            output_path.unlink(missing_ok=True)
                    path.unlink(missing_ok=True)
                    if pending_path is not None:
                        pending_path.unlink(missing_ok=True)
                    if index:
                        index_path.unlink(missing_ok=True)
            except OSError:
                continue
    except BaseException:
        return None


__all__ = ["cleanup_jobs", "duration_hint", "followup_hints"]
