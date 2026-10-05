"""Bounded, fail-open SessionStart hints and retention for Codex async jobs."""

from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import json
from itertools import islice
import math
import os
from pathlib import Path
import re
import statistics
import subprocess
import time
from typing import Any

import codex_exec_mcp
import codex
import ledger
import telemetry
import stall_followup
import state_retention


RETENTION_SECONDS = 7 * 86400
HISTORY_SECONDS = 90 * 86400
MAX_JOB_RECORDS = 256
MAX_JOB_RECORD_BYTES = 64 * 1024
MAX_TELEMETRY_FILES = 32
MAX_TELEMETRY_BYTES = 2 * 1024 * 1024
MAX_TELEMETRY_LINES = 4096
ACCURACY_WINDOW = 30
IMPLEMENTER = "codex-main"
IMPLEMENTER_ALIASES = {"codex": IMPLEMENTER}
ROUTES = ("mechanical", "subtle", "normal", "consequential", "escalated", "operation")
AGENT_NAMES = {"codex-main": ("Codex", "in"), "opencode-main": ("OpenCode", "in"), "agy-main": ("Antigravity", "in"), "claude": ("Claude", "un")}
TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS", "EXPIRED"}
_JOB_ID = re.compile(r"[0-9a-f]{64}")


def _number_minutes(value: float) -> int:
    return max(1, math.floor(value / 60000 + 0.5))


def _finished_events(state_root: str | Path, project_key: str, now: float):
    """Yield recent finished calls from the existing bounded telemetry scan."""
    current_dt = datetime.fromtimestamp(now, timezone.utc)
    cutoff = datetime.fromtimestamp(now - HISTORY_SECONDS, timezone.utc)
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
                if not isinstance(route, str) or route not in ROUTES:
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
                yield item, payload, float(duration), parsed.astimezone(timezone.utc)
        if scanned >= MAX_TELEMETRY_BYTES or lines >= MAX_TELEMETRY_LINES:
            break


def duration_hint(state_root: str | Path, project_key: str, *, now: float | None = None,
                  implementer: str = IMPLEMENTER) -> str | None:
    """Prefer an implementer's route sample after five calls; otherwise use the pool."""
    try:
        grouped: dict[str, list[float]] = {}
        own: dict[str, list[float]] = {}
        implementer = IMPLEMENTER_ALIASES.get(implementer, implementer)
        for item, payload, duration, _stamp in _finished_events(
                state_root, project_key, time.time() if now is None else now):
            route = payload["route"]
            grouped.setdefault(route, []).append(duration)
            sample_implementer = payload.get("implementer")
            if IMPLEMENTER_ALIASES.get(sample_implementer, sample_implementer) == implementer:
                own.setdefault(route, []).append(duration)
        parts = []
        for route in ROUTES:
            values = own.get(route, [])
            agent_ready = len(values) >= 5
            if not agent_ready:
                values = grouped.get(route, [])
            if len(values) < (5 if agent_ready else 10):
                continue
            ordered = sorted(values)
            p90 = ordered[math.ceil(0.9 * len(ordered)) - 1]
            parts.append(f"{route} {_number_minutes(statistics.median(ordered))}/{_number_minutes(p90)} min (n={len(ordered)})")
        name, _suffix = AGENT_NAMES.get(implementer, (implementer, "in"))
        return f"Purser: typical {name} durations here (median/p90, n): " + ", ".join(parts) + "." if parts else None
    except BaseException:
        return None


def compact_accuracy_hint(state_root: str | Path, project_key: str, *, now: float | None = None) -> str | None:
    """Count compactions and compare each implementer's last 30 usable estimates."""
    try:
        root = Path(state_root) / "projects" / project_key
        root.mkdir(parents=True, exist_ok=True)
        path = root / "duration_accuracy.json"
        with codex._async_claim(path):
            state = _read_json(path) if path.exists() else {"compacts": 0, "errors": {}}
            if (state is None or type(state.get("compacts")) is not int
                    or state["compacts"] < 0 or not isinstance(state.get("errors"), dict)):
                return None
            count = state["compacts"] + 1
            state["compacts"] = count
            if count % 5:
                _write_json(path, state)
                return None
            jobs: dict[str, list[tuple[datetime, float]]] = {}
            for _item, payload, duration, stamp in _finished_events(
                    state_root, project_key, time.time() if now is None else now):
                agent = payload.get("implementer")
                eta = payload.get("eta_minutes")
                if (not isinstance(agent, str) or not agent or isinstance(eta, bool)
                        or not isinstance(eta, (int, float)) or not math.isfinite(eta)
                        or eta <= 0):
                    continue
                error = abs(eta * 60000 - duration) / duration * 100
                if math.isfinite(error):
                    jobs.setdefault(agent, []).append((stamp, error))
            messages = []
            for agent, samples in jobs.items():
                samples.sort(key=lambda sample: sample[0], reverse=True)
                error = statistics.mean(value for _stamp, value in samples[:ACCURACY_WINDOW])
                previous = state["errors"].get(agent)
                state["errors"][agent] = error
                if (isinstance(previous, (int, float)) and not isinstance(previous, bool)
                        and math.isfinite(previous) and error < previous):
                    name, suffix = AGENT_NAMES.get(agent, (agent, "in"))
                    messages.append(f'Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: "> ⚓ **{name}\'s task duration estimates have improved through the self-learning system! The estimated margin of error is now {round(error)}%.**"')
            _write_json(path, state)
            return "\n".join(messages) or None
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
            liveness = job.get("liveness")
            if not isinstance(liveness, dict):
                liveness = {}
            last_activity = liveness.get("lastActivityAt")
            resumed = job.get("resumedAfterInterruption", 0)
            same_session = index["session_id"] == session_id
            if (same_session and state in TERMINAL - {"SUCCEEDED"}
                    and job.get("waiterObservedTerminalState") == state
                    and job.get("waiterObservedTerminalAt") is not None):
                continue
            if state in {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"} or (same_session and state in TERMINAL):
                message = (None if same_session else
                           f"⚓ The Codex task I started before the app closed ({task}) "
                           "kept running while the app was closed and has finished. Would you like me to retrieve and review its result?")
                instruction = ("Call codex_result with this jobId now, without adopt or asking the user."
                               if same_session else
                               "Ask for the user's agreement before calling codex_result with adopt: true.")
            elif (state == "RUNNING" and job.get("errorCode") == "STALLED"
                  and isinstance(job.get("stallDetectedAt"), (int, float))
                  and not isinstance(job.get("stallDetectedAt"), bool)):
                message = None
                instruction = stall_followup.instruction(last_activity)
            elif state in {"QUEUED", "RUNNING"}:
                message = (None if same_session else
                           f"⚓ The Codex task I started before the app closed ({task}) "
                           "is still running while the app is closed. I'll let you know when it finishes.")
                instruction = ("Start purser_wait.py for this jobId in the background (use the PowerShell tool on Windows); call codex_result when it finishes, without adopt or asking the user."
                               if same_session else
                               "When it finishes, ask for the user's agreement before calling codex_result with adopt: true.")
            else:
                continue
            resumed_hint = (f'Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: "> ⚓ **The Codex task ({task}) was interrupted when the computer shut down; I resumed it from where it left off.**" '
                            if isinstance(resumed, int) and resumed > 0 else "")
            user_hint = f'Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: "> ⚓ **{message.removeprefix("⚓ ")}**" ' if message else ""
            lines.append(f"Purser (jobId={job_id}, for Claude only): {resumed_hint}{user_hint}"
                         f"{instruction} Do not show the jobId to the user.")
        return "\n".join(lines) or None
    except BaseException:
        return None


def _write_json(path: Path, value: dict[str, Any]) -> None:
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, sort_keys=True), encoding="utf-8")
    temporary.replace(path)


def _listed_worktrees(repo_root: Path) -> tuple[Path, list[Path]] | None:
    """Return a repository command root and its registered worktree paths."""
    try:
        completed = subprocess.run(
            ["git", "-C", os.fspath(repo_root), "worktree", "list", "--porcelain"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, encoding="utf-8", errors="replace", timeout=30, check=False,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
        )
        if completed.returncode:
            return None
        paths = [Path(line[9:]).resolve() for line in completed.stdout.splitlines()
                 if line.startswith("worktree ")]
        return (paths[0], paths) if paths else None
    except (OSError, subprocess.SubprocessError, ValueError):
        return None


def _remove_worktree(state_root: Path, repo_root: Path, worktree_path: str | Path,
                     listings: dict[str, tuple[Path, list[Path]] | None],
                     prune_repos: dict[str, Path]) -> bool:
    """Remove only a registered worktree contained by this state's wt directory."""
    try:
        wt_root = (state_root / "wt").resolve()
        target = Path(worktree_path).resolve()
        target.relative_to(wt_root)
    except (OSError, ValueError, TypeError):
        return False
    if target == wt_root:
        return False
    repo = repo_root.resolve()
    repo_key = os.path.normcase(os.fspath(repo))
    if not target.exists():
        prune_repos.setdefault(repo_key, repo)
        return False
    if repo_key not in listings:
        listings[repo_key] = _listed_worktrees(repo)
    listing = listings[repo_key]
    if listing is None:
        return False
    command_root, paths = listing
    if os.path.normcase(os.fspath(target)) not in {os.path.normcase(os.fspath(path)) for path in paths}:
        return False
    try:
        removed = subprocess.run(
            ["git", "-C", os.fspath(command_root), "worktree", "remove", "--force", os.fspath(target)],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=30, check=False,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
        )
        if removed.returncode:
            return False
        paths[:] = [path for path in paths
                    if os.path.normcase(os.fspath(path)) != os.path.normcase(os.fspath(target))]
        return True
    except (OSError, subprocess.SubprocessError):
        return False


def _worktree_key(worktree_path: Any) -> str | None:
    try:
        return os.path.normcase(os.fspath(Path(worktree_path).resolve()))
    except (OSError, TypeError, ValueError):
        return None


def _cleanup_orphan_worktrees(root: Path, current: float, protected_worktrees: set[str],
                              listings: dict[str, tuple[Path, list[Path]] | None],
                              prune_repos: dict[str, Path]) -> None:
    """Prune old registered worktrees only when their job record is absent."""
    wt_root = root / "wt"
    try:
        children = list(wt_root.iterdir())
    except OSError:
        return
    for worktree in children:
        if not worktree.is_dir():
            continue
        try:
            worktree.resolve().relative_to(wt_root.resolve())
        except (OSError, ValueError):
            continue
        if _worktree_key(worktree) in protected_worktrees:
            continue
        try:
            if current - worktree.stat().st_mtime <= RETENTION_SECONDS:
                continue
            common_dir = subprocess.run(
                ["git", "-C", os.fspath(worktree), "rev-parse", "--path-format=absolute", "--git-common-dir"],
                stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, encoding="utf-8", errors="replace", timeout=30, check=False,
                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
            )
            if common_dir.returncode:
                continue
            # Linked worktrees share <main-repo>/.git as their common directory.
            repo_root = Path(common_dir.stdout.strip()).resolve().parent
            _remove_worktree(root, repo_root, worktree, listings, prune_repos)
        except (OSError, subprocess.SubprocessError, ValueError):
            continue


def cleanup_jobs(state_root: str | Path, *, now: float | None = None,
                 check_ceiling: bool = False) -> None:
    """Reconcile dead hosts and remove expired evidence with its ownership records."""
    try:
        root = Path(state_root)
        jobs = root / "codex_exec_mcp" / "jobs"
        bridge = codex_exec_mcp.Server(job_dir=jobs)
        current = time.time() if now is None else now
        all_records = list(jobs.glob("*.json"))
        protected_worktrees = set()
        loaded_records: list[tuple[Path, dict[str, Any]]] = []
        for record in all_records:
            job = _read_json(record)
            if not job:
                continue
            if job.get("jobId") == record.stem:
                loaded_records.append((record, job))
            state = job.get("state")
            discard_without_patch = state in {"FAILED", "CANCELLED", "TIMED_OUT"} and "applyState" not in job
            may_remove_worktree = (state != "UNKNOWN_OR_IN_PROGRESS"
                                   and (job.get("applyState") == "applied" or discard_without_patch))
            if state not in TERMINAL or not may_remove_worktree:
                key = _worktree_key(job.get("worktreePath"))
                if key is not None:
                    protected_worktrees.add(key)
        recoveries: list[tuple[Path, dict[str, Any]]] = []
        removals: list[tuple[Path, dict[str, Any]]] = []
        for path, job in loaded_records:
            if not _JOB_ID.fullmatch(path.stem):
                continue
            state = job.get("state")
            if state not in TERMINAL:
                recoveries.append((path, job))
                continue
            discard_without_patch = state in {"FAILED", "CANCELLED", "TIMED_OUT"} and "applyState" not in job
            may_remove_worktree = (state != "UNKNOWN_OR_IN_PROGRESS"
                                   and (job.get("applyState") == "applied" or discard_without_patch))
            worktree_key = _worktree_key(job.get("worktreePath"))
            removes_worktree = bool(job.get("worktreePath") and may_remove_worktree
                                    and worktree_key not in protected_worktrees)
            launch_exists = state != "UNKNOWN_OR_IN_PROGRESS" and (jobs / f"{path.stem}.launch").exists()
            finished = job.get("finishedAt")
            expired = (not isinstance(finished, bool) and isinstance(finished, (int, float))
                       and not current - finished < RETENTION_SECONDS)
            if removes_worktree or launch_exists or expired:
                removals.append((path, job))

        listings: dict[str, tuple[Path, list[Path]] | None] = {}
        prune_repos: dict[str, Path] = {}
        actions = 0
        for path, job in recoveries:
            if actions >= MAX_JOB_RECORDS:
                break
            bridge.recover_job(path.stem, now=current)
            actions += 1
        for path, job in removals:
            if actions >= MAX_JOB_RECORDS:
                break
            state = job.get("state")
            discard_without_patch = state in {"FAILED", "CANCELLED", "TIMED_OUT"} and "applyState" not in job
            may_remove_worktree = (state != "UNKNOWN_OR_IN_PROGRESS"
                                   and (job.get("applyState") == "applied" or discard_without_patch))
            if job.get("worktreePath") and not may_remove_worktree:
                continue
            acted = False
            if (job.get("worktreePath") and may_remove_worktree
                    and _worktree_key(job["worktreePath"]) not in protected_worktrees):
                repo_root = job.get("repoRoot")
                if isinstance(repo_root, str) and repo_root:
                    acted = _remove_worktree(root, Path(repo_root), job["worktreePath"],
                                             listings, prune_repos) or acted
            if state != "UNKNOWN_OR_IN_PROGRESS":
                launch_path = jobs / f"{path.stem}.launch"
                if launch_path.exists():
                    launch_path.unlink(missing_ok=True)
                    acted = True
            finished = job.get("finishedAt")
            if isinstance(finished, bool) or not isinstance(finished, (int, float)) or current - finished < RETENTION_SECONDS:
                if acted:
                    actions += 1
                continue
            if acted:
                actions += 1
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
                    if not acted:
                        actions += 1
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
                    (jobs / f"{path.stem}.launch").unlink(missing_ok=True)
                    if pending_path is not None:
                        pending_path.unlink(missing_ok=True)
                    if index:
                        index_path.unlink(missing_ok=True)
            except OSError:
                continue
        _cleanup_orphan_worktrees(root, current, protected_worktrees, listings, prune_repos)
        for repo_key, repo_root in prune_repos.items():
            try:
                subprocess.run(
                    ["git", "-C", os.fspath(repo_root), "worktree", "prune"],
                    stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                    timeout=30, check=False,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
                )
            except (OSError, subprocess.SubprocessError):
                continue
        try:
            state_retention.prune_state(root, now=current, check_ceiling=check_ceiling)
        except BaseException:
            pass
    except BaseException:
        return None


__all__ = ["cleanup_jobs", "duration_hint", "followup_hints"]
