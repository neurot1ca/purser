"""Manually run read-only maintenance reports outside project repositories."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import contextlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import uuid

import project_root
import state_root


JOB_TIMEOUT_SECONDS = 2700
QUIET_PERIOD_SECONDS = 900
MAINTENANCE_CADENCE_SECONDS = 7 * 24 * 60 * 60
QUOTA_MAX_USED_PERCENT = 50
FIRST_SHOW_REMAINING_PCT = 70
SECOND_SHOW_REMAINING_PCT = 30
QUOTA_MAX_AGE_SECONDS = 30 * 60
BACKGROUND_CHECK_DELAY_SECONDS = QUIET_PERIOD_SECONDS
BACKGROUND_CHECK_TIMEOUT_SECONDS = QUIET_PERIOD_SECONDS + JOB_TIMEOUT_SECONDS
_FINDINGS_START = "```purser-findings-json"
_FINDING_LINE = re.compile(r"^(.+?):(\d+)$")
CLEANUP_HANDOFF_COMMIT_RULE = (
    "Any `temizle` cleanup must land as ONE separate commit containing only that cleanup; "
    "a single `git revert` must undo it."
)
_REPORT_TYPES = ("cleanup", "test-gaps")


def _canonical_root(value: str | os.PathLike[str]) -> Path:
    root, error = project_root.resolve_candidate(value)
    if root is None:
        raise ValueError(error)
    return root


def _maintenance_dir(root: Path, state: Path) -> Path:
    return state / "projects" / project_root.project_key(root) / "maintenance"


def opt_in(repo: str | os.PathLike[str], *, state: Path | None = None) -> Path:
    root = _canonical_root(repo)
    directory = _maintenance_dir(root, state or state_root.default_state_root())
    directory.mkdir(parents=True, exist_ok=True)
    marker = directory / "opt-in.json"
    temporary = directory / ("." + uuid.uuid4().hex + ".tmp")
    try:
        with temporary.open("x", encoding="utf-8") as stream:
            json.dump({"project_root": str(root), "enabled": True}, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, marker)
    finally:
        temporary.unlink(missing_ok=True)
    consent = directory / "consent.json"
    try:
        value = json.loads(consent.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        value = {}
    value.update(project_root=str(root), decision="yes", source="manual-opt-in")
    import codex_exec_mcp
    codex_exec_mcp._atomic_json(consent, value)
    return marker


def opt_out(repo: str | os.PathLike[str], *, state: Path | None = None) -> Path:
    root = _canonical_root(repo)
    directory = _maintenance_dir(root, state or state_root.default_state_root())
    marker = directory / "opt-in.json"
    try:
        value = json.loads(marker.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        value = {"project_root": str(root)}
    value.update(project_root=str(root), enabled=False)
    import codex_exec_mcp
    codex_exec_mcp._atomic_json(marker, value)
    return marker


def record_consent(repo: str | os.PathLike[str], decision: str, *, state: Path | None = None) -> Path:
    if decision not in {"yes", "no"}:
        raise ValueError("consent decision must be yes or no")
    root = _canonical_root(repo)
    directory = _maintenance_dir(root, state or state_root.default_state_root())
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / "consent.json"
    import codex_exec_mcp
    codex_exec_mcp._atomic_json(path, {"project_root": str(root), "decision": decision})
    marker = directory / "opt-in.json"
    codex_exec_mcp._atomic_json(marker, {"project_root": str(root), "enabled": decision == "yes"})
    return path


def skip_second_show(repo: str | os.PathLike[str], report: str, *, state: Path | None = None) -> None:
    root = _canonical_root(repo)
    path = Path(report).resolve()
    report_type = path.parent.name
    expected_dir = _maintenance_dir(root, state or state_root.default_state_root()) / "reports" / report_type
    if (report_type not in _REPORT_TYPES or path.parent != expected_dir.resolve()
            or path.suffix != ".json" or not path.name.endswith(".notice.json")):
        raise ValueError("report notice must belong to this project")
    notice = json.loads(path.read_text(encoding="utf-8"))
    cycle = notice.get("pending_cycle") or notice.get("last_shown_cycle")
    if not isinstance(cycle, str) or cycle == "no-window":
        raise ValueError("no quota cycle is pending")
    notice.setdefault("cycles", {}).setdefault(cycle, {})["skip_second"] = True
    notice["pending"] = False
    notice.pop("pending_cycle", None)
    notice.pop("pending_show", None)
    import codex_exec_mcp
    codex_exec_mcp._atomic_json(path, notice)


def _tool_info() -> tuple[str, str]:
    vulture = shutil.which("vulture")
    if not vulture:
        return "vulture: unavailable", "Vulture unavailable; grep-based search only, which can miss indirect and dynamic use."
    try:
        result = subprocess.run([vulture, "--version"], capture_output=True, text=True,
                                timeout=5, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
        version = (result.stdout or result.stderr).strip().splitlines()[0]
    except (OSError, subprocess.SubprocessError, IndexError):
        version = "version unavailable"
    return f"vulture: {version}", "Vulture available; worker must still check entry points, hooks, and dynamic use."


def _source_commit(root: Path) -> str:
    result = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, capture_output=True,
                            text=True, encoding="utf-8", errors="replace", timeout=10, check=False,
                            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0)
    if result.returncode:
        raise RuntimeError("could not read source commit: " + result.stderr.strip())
    return result.stdout.strip()


def _write_report(path: Path, body: str, *, commit: str, timestamp: str,
                  versions: list[str], limitations: str, findings: list[dict] | None = None,
                  cost: dict | None = None, report_type: str = "cleanup") -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    title = "Cleanup" if report_type == "cleanup" else "Test gaps"
    content = (f"# {title} report\n\n"
               f"- Source commit: `{commit}`\n- Run time (UTC): `{timestamp}`\n"
               f"- Tool versions: {', '.join(versions)}\n- Limitations: {limitations}\n\n"
               + f"- Findings: {len(findings) if findings is not None else 'unknown'}\n"
               + f"- Cost: `{json.dumps(cost or {'tokens': 'unknown', 'elapsed_seconds': 'unknown', 'quota_change': 'unknown'}, sort_keys=True)}`\n\n"
               + body.rstrip() + "\n")
    temporary = path.with_name("." + uuid.uuid4().hex + ".tmp")
    try:
        with temporary.open("x", encoding="utf-8", newline="\n") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def parse_findings(body: str) -> list[dict] | None:
    """Parse the worker's final structured list; malformed output stays unknown."""
    try:
        start = body.rindex(_FINDINGS_START) + len(_FINDINGS_START)
        end = body.index("```", start)
        value = json.loads(body[start:end].strip())
        if isinstance(value, dict):
            coverage = value.get("line_coverage_pct")
            if (set(value) != {"findings", "line_coverage_pct"}
                    or (coverage is not None and (isinstance(coverage, bool)
                        or not isinstance(coverage, (int, float)) or not 0 <= coverage <= 100))):
                return None
            value = value["findings"]
        if not isinstance(value, list):
            return None
        result = []
        for item in value:
            if not isinstance(item, dict) or set(item) != {"path_line", "symbol", "evidence", "confidence"}:
                return None
            match = _FINDING_LINE.fullmatch(item["path_line"]) if isinstance(item["path_line"], str) else None
            if (not match or not all(isinstance(item[key], str) and item[key].strip()
                                     for key in ("symbol", "evidence", "confidence"))):
                return None
            if item["confidence"].casefold() not in {"high", "medium", "low"}:
                return None
            result.append({**item, "confidence_rank": _confidence_rank(item["confidence"])})
        return sorted(result, key=lambda item: item["confidence_rank"], reverse=True)
    except (ValueError, TypeError, KeyError):
        return None


def _line_coverage(body: str) -> int | float | None:
    try:
        start = body.rindex(_FINDINGS_START) + len(_FINDINGS_START)
        end = body.index("```", start)
        value = json.loads(body[start:end].strip())
        coverage = value.get("line_coverage_pct") if isinstance(value, dict) else None
        return coverage if (isinstance(coverage, (int, float)) and not isinstance(coverage, bool)
                            and 0 <= coverage <= 100) else None
    except (ValueError, TypeError, AttributeError):
        return None


def _confidence_rank(value: str) -> int:
    return {"high": 3, "medium": 2, "low": 1}.get(value.casefold(), 0)


def _global_slot(state: Path):
    """Exclusive process-lifetime slot shared by all project maintenance jobs."""
    slot = state / "maintenance" / "active.json"
    slot.parent.mkdir(parents=True, exist_ok=True)
    return _exclusive_file(slot, stale_check=_global_slot_is_stale)


@contextlib.contextmanager
def _exclusive_file(path: Path, *, stale_check=None):
    flags = os.O_CREAT | os.O_EXCL | os.O_WRONLY
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError:
        if stale_check is None or not stale_check(path):
            raise
        stale_path = path.with_name("." + uuid.uuid4().hex + ".stale")
        try:
            os.replace(path, stale_path)
        except FileNotFoundError:
            pass
        else:
            stale_path.unlink(missing_ok=True)
        descriptor = os.open(path, flags, 0o600)
    try:
        os.write(descriptor, json.dumps({"pid": os.getpid(), "started_at": time.time()}).encode("utf-8"))
        os.fsync(descriptor)
        yield
    finally:
        os.close(descriptor)
        path.unlink(missing_ok=True)


def _pid_alive(pid: int) -> bool:
    if type(pid) is not int or pid <= 0:
        return False
    if os.name == "nt":
        import codex_exec_mcp
        identity, _created = codex_exec_mcp._windows_process_identity(pid)
        return identity != "DEAD"
    try:
        os.kill(pid, 0)
    except PermissionError:
        return True
    except ProcessLookupError:
        return False
    except OSError as exc:
        return not (getattr(exc, "winerror", None) == 87 or getattr(exc, "errno", None) == 3)
    return True


def _global_slot_is_stale(path: Path) -> bool:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        pid, started_at = value.get("pid"), value.get("started_at")
        if (isinstance(started_at, (int, float))
                and time.time() - started_at > BACKGROUND_CHECK_TIMEOUT_SECONDS):
            return True
        return type(pid) is int and not _pid_alive(pid)
    except (OSError, ValueError, AttributeError):
        return False


def _active_user_jobs(state: Path, *, exclude_job_id: str | None = None) -> bool | None:
    """True means a user job exists; None means job evidence is unreadable."""
    directory = state / "codex_exec_mcp" / "jobs"
    if not directory.exists():
        return False
    try:
        for path in directory.glob("*.json"):
            job = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(job, dict) or not isinstance(job.get("state"), str):
                return None
            if job.get("jobId", path.stem) != exclude_job_id and job["state"] in {"RUNNING", "QUEUED"}:
                return True
    except (OSError, ValueError):
        return None
    return False


def _job_ids(state: Path) -> set[str] | None:
    try:
        return {path.stem for path in (state / "codex_exec_mcp" / "jobs").glob("*.json")}
    except OSError:
        return None


def _fresh_quota(now: float | None = None) -> dict | None:
    import codex_quota
    snapshot = codex_quota.read_latest_quota()
    if not isinstance(snapshot, dict):
        return None
    try:
        stamp = datetime.fromisoformat(snapshot["snapshot_at"].replace("Z", "+00:00")).timestamp()
    except (KeyError, TypeError, ValueError):
        return None
    current = time.time() if now is None else now
    if stamp > current or current - stamp > QUOTA_MAX_AGE_SECONDS:
        return None
    if (_active_quota_window(snapshot) is None
            and (snapshot.get("windows_seen") is True
                 or any(key in snapshot for key in ("5h_used_pct", "7d_used_pct")))):
        return None
    return snapshot


def _active_quota_window(snapshot: dict) -> tuple[str, float, str] | None:
    for label in ("5h", "7d"):
        used, resets_at = snapshot.get(f"{label}_used_pct"), snapshot.get(f"{label}_resets_at")
        if (isinstance(used, (int, float)) and not isinstance(used, bool)
                and isinstance(resets_at, str) and resets_at):
            return label, used, resets_at
    return None


def record_activity(repo: Path, *, state: Path | None = None) -> None:
    state = state or state_root.default_state_root()
    candidate = Path(repo).resolve()
    directory = _maintenance_dir(candidate, state)
    if not _is_opted_in(candidate, directory):
        return
    import codex_exec_mcp
    root = _canonical_root(candidate)
    directory = _maintenance_dir(root, state)
    codex_exec_mcp._atomic_json(directory / "last-activity.json", {"at": time.time()})


def _is_opted_in(root: Path, directory: Path) -> bool:
    try:
        marker = json.loads((directory / "opt-in.json").read_text(encoding="utf-8"))
        return marker.get("enabled") is True and marker.get("project_root") == str(root)
    except (OSError, ValueError, AttributeError):
        return False


def _last_run_path(directory: Path, report_type: str) -> Path:
    return directory / "last-run.json" if report_type == "cleanup" else directory / "reports" / report_type / "last-run.json"


def _last_completed_at(directory: Path, report_type: str) -> float:
    try:
        value = json.loads(_last_run_path(directory, report_type).read_text(encoding="utf-8"))
        at = value.get("completed_at")
        return float(at) if isinstance(at, (int, float)) and not isinstance(at, bool) else 0
    except (OSError, ValueError, TypeError, AttributeError):
        return 0


def _select_due_report_type(directory: Path, candidates: tuple[str, ...] = _REPORT_TYPES) -> str:
    return min(candidates, key=lambda report_type: (_last_completed_at(directory, report_type),
                                                     _REPORT_TYPES.index(report_type)))


def _cadence_due(directory: Path, now: float | None = None, *, report_type: str = "cleanup") -> bool:
    try:
        last = json.loads(_last_run_path(directory, report_type).read_text(encoding="utf-8"))
        at = last.get("completed_at")
        current = time.time() if now is None else now
        return isinstance(at, (int, float)) and current - at >= MAINTENANCE_CADENCE_SECONDS
    except (OSError, ValueError, TypeError):
        return True


def eligible(repo: Path, *, state: Path, now: float | None = None,
             report_type: str = "cleanup") -> bool:
    if report_type not in _REPORT_TYPES:
        raise ValueError(f"unsupported maintenance report type: {report_type}")
    root = _canonical_root(repo)
    directory = _maintenance_dir(root, state)
    try:
        marker = json.loads((directory / "opt-in.json").read_text(encoding="utf-8"))
        activity = json.loads((directory / "last-activity.json").read_text(encoding="utf-8"))
        last_activity = activity.get("at")
        current = time.time() if now is None else now
        if (marker.get("enabled") is not True or marker.get("project_root") != str(root)
                or not isinstance(last_activity, (int, float))
                or current - last_activity < QUIET_PERIOD_SECONDS):
            return False
    except (OSError, ValueError, TypeError, AttributeError):
        return False
    if not _cadence_due(directory, now, report_type=report_type):
        return False
    quota = _fresh_quota(now)
    if quota is None:
        return False
    window = _active_quota_window(quota)
    if window is None:
        consent = {}
        try:
            consent = json.loads((directory / "consent.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            pass
        if consent.get("decision") != "yes" or consent.get("project_root") != str(root):
            return False
    elif window[1] > QUOTA_MAX_USED_PERCENT:
        return False
    return _active_user_jobs(state) is False


def _opted_in_projects(state: Path):
    projects = state / "projects"
    for maintenance in projects.glob("*/maintenance"):
        try:
            value = json.loads((maintenance / "opt-in.json").read_text(encoding="utf-8"))
            root = Path(value["project_root"])
            if value.get("enabled") is True and _maintenance_dir(_canonical_root(root), state) == maintenance:
                yield root
        except (OSError, ValueError, TypeError, KeyError):
            continue


def delayed_check(repo: Path, *, state: Path | None = None, delay: float = BACKGROUND_CHECK_DELAY_SECONDS) -> Path | None:
    """Wait off-hook, then start the oldest-due globally eligible maintenance report."""
    state = state or state_root.default_state_root()
    time.sleep(min(max(delay, 0), BACKGROUND_CHECK_TIMEOUT_SECONDS - JOB_TIMEOUT_SECONDS))
    candidate = _canonical_root(repo)
    if not any(project == candidate for project in _opted_in_projects(state)):
        return None
    due = [report_type for report_type in _REPORT_TYPES
           if eligible(candidate, state=state, report_type=report_type)]
    if not due:
        return None
    report_type = _select_due_report_type(_maintenance_dir(candidate, state), tuple(due))
    try:
        return run_report(candidate, report_type=report_type, state=state, _require_idle=True)
    except FileExistsError:
        return None


def wake_background_check(repo: Path, *, state: Path | None = None) -> bool:
    """Detach bounded work from SessionStart; the hook itself never waits."""
    state = state or state_root.default_state_root()
    candidate = Path(repo).resolve()
    if not _is_opted_in(candidate, _maintenance_dir(candidate, state)):
        return False
    root = _canonical_root(candidate)
    args = [sys.executable, str(Path(__file__).resolve()), "check", "--repo", str(root)]
    try:
        subprocess.Popen(args, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL, close_fds=True,
                         creationflags=(getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000)
                                        | getattr(subprocess, "DETACHED_PROCESS", 0x00000008)) if os.name == "nt" else 0)
        return True
    except OSError:
        return False


def _active_is_stale(active_path: Path, bridge: codex_exec_mcp.Server) -> bool:
    import codex_exec_mcp
    try:
        active = json.loads(active_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    if not isinstance(active, dict):
        return False
    if "job_id" in active:
        job = bridge._read_job(active.get("job_id"))
        return job is None or job.get("state") in codex_exec_mcp.TERMINAL
    try:
        started = datetime.strptime(active["started_at"], "%Y%m%dT%H%M%S.%fZ").replace(
            tzinfo=timezone.utc).timestamp()
    except (KeyError, TypeError, ValueError):
        return False
    return time.time() - started >= JOB_TIMEOUT_SECONDS


def _create_active(active_path: Path, active: dict) -> None:
    with active_path.open("x", encoding="utf-8") as stream:
        json.dump(active, stream)
        stream.flush()
        os.fsync(stream.fileno())


def run_cleanup(repo: str | os.PathLike[str], *, state: Path | None = None,
                server: codex_exec_mcp.Server | None = None, poll_seconds: float = 1.0,
                _require_idle: bool = False) -> Path | None:
    return run_report(repo, report_type="cleanup", state=state, server=server,
                      poll_seconds=poll_seconds, _require_idle=_require_idle)


def run_test_gaps(repo: str | os.PathLike[str], *, state: Path | None = None,
                  server: codex_exec_mcp.Server | None = None, poll_seconds: float = 1.0,
                  _require_idle: bool = False) -> Path | None:
    return run_report(repo, report_type="test-gaps", state=state, server=server,
                      poll_seconds=poll_seconds, _require_idle=_require_idle)


def run_report(repo: str | os.PathLike[str], *, report_type: str, state: Path | None = None,
               server: codex_exec_mcp.Server | None = None, poll_seconds: float = 1.0,
               _require_idle: bool = False) -> Path | None:
    if report_type not in _REPORT_TYPES:
        raise ValueError(f"unsupported maintenance report type: {report_type}")
    root = _canonical_root(repo)
    import codex_exec_mcp
    state = state or state_root.default_state_root()
    directory = _maintenance_dir(root, state)
    marker = directory / "opt-in.json"
    try:
        opted_in = json.loads(marker.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        opted_in = None
    if not isinstance(opted_in, dict) or opted_in.get("enabled") is not True or opted_in.get("project_root") != str(root):
        raise RuntimeError(f"Maintenance is not opted in for {root}; run opt-in explicitly first.")

    directory.mkdir(parents=True, exist_ok=True)
    slot_context = _global_slot(state)
    slot_context.__enter__()
    if _require_idle and not eligible(root, state=state, report_type=report_type):
        slot_context.__exit__(None, None, None)
        return None
    active_path = directory / f"{report_type}-active.json"
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    active = {"project_root": str(root), "started_at": timestamp, "state": "RUNNING"}
    bridge = server or codex_exec_mcp.Server(job_dir=state / "codex_exec_mcp" / "jobs", agent="codex")
    try:
        _create_active(active_path, active)
    except FileExistsError as exc:
        if not _active_is_stale(active_path, bridge):
            slot_context.__exit__(None, None, None)
            raise RuntimeError(f"A {report_type} report is already running for {root}.") from exc
        stale_path = active_path.with_name("." + uuid.uuid4().hex + ".stale")
        try:
            os.replace(active_path, stale_path)
        except FileNotFoundError:
            pass
        else:
            stale_path.unlink(missing_ok=True)
        try:
            _create_active(active_path, active)
        except FileExistsError as duplicate:
            slot_context.__exit__(None, None, None)
            raise RuntimeError(f"A {report_type} report is already running for {root}.") from duplicate
    except Exception:
        slot_context.__exit__(None, None, None)
        raise

    reports = directory / "reports" / report_type
    report_path = reports / (timestamp + ".md")
    failure_path = reports / "failures" / (timestamp + ".json")
    job_id = None
    started_clock = time.monotonic()
    before_quota = _fresh_quota()
    retain_slot = False
    try:
        if _active_user_jobs(state) is not False:
            raise RuntimeError("active or unknown user job state; maintenance skipped")
        commit = _source_commit(root)
        if report_type == "cleanup":
            vulture_version, limitations = _tool_info()
            tool_versions = [vulture_version]
        else:
            limitations = "Coverage availability and result are reported by the worker."
            tool_versions = []
        versions = [f"Python: {sys.version.split()[0]}", "Git: " + subprocess.run(
            ["git", "--version"], cwd=root, capture_output=True, text=True, timeout=10,
            check=True,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0).stdout.strip(),
                    *tool_versions]
        if report_type == "cleanup":
            prompt = f"""Produce a cleanup report for the repository at {root}.
This is a strictly read-only inspection. Do not edit files, create repository files,
install tools, or run package managers. Report only evidence-backed candidates.
For every item include (1) exact tool output or search evidence, (2) repository
path and symbol, and (3) checks against entry points, hooks, and dynamic/reflection
use before calling it unused. If vulture is available, use its installed version;
otherwise state that vulture is unavailable and use grep-based searches, explaining
that grep can miss indirect or dynamic use. Never install any tool. Include the
commands/searches used and distinguish uncertain candidates from confirmed facts.
If user-initiated work starts, it has priority: do not delay it, cancel it, or
continue consuming resources once it needs the machine. End your report with this
machine-readable list, using one object per finding and an empty list for no findings:
```purser-findings-json
[{{"path_line":"relative/path.py:12","symbol":"name","evidence":"short evidence","confidence":"high|medium|low"}}]
```
"""
        else:
            prompt = f"""Produce a test-gaps report for the repository at {root}.
This is a strictly read-only inspection. Do not edit files, create repository files,
install tools, or run package managers. Report only evidence-bound candidate tests.
For each candidate, give the target path:line and symbol, the untested behaviour,
evidence from source, the exact command used to search existing tests and its result
showing no test already covers that behaviour, and a high, medium, or low confidence
level. Include a compact contract evidence section covering documented requirements,
invariants, boundaries, and affected callers, cite each item to a source path:line,
and write "unknown" where evidence is absent.
Check whether a coverage tool is already available in this project's environment.
For Python coverage, if `python -m coverage --version` succeeds, run
`python -m coverage report --format=total`; report its total line coverage as a
number and include that exact command. Use an existing configured command for other
available coverage tools. If no usable coverage tool or coverage data is available,
report coverage as unknown. Never install anything.
If user-initiated work starts, it has priority: do not delay it, cancel it, or
continue consuming resources once it needs the machine. End with one JSON object in
this machine-readable block. Findings use the cleanup shape; line_coverage_pct must
be a number or null:
```purser-findings-json
{{"findings":[{{"path_line":"relative/path.py:12","symbol":"name","evidence":"source evidence; untested behaviour; test search command and no-match result","confidence":"high|medium|low"}}],"line_coverage_pct":null}}
```
"""
        call = str(uuid.uuid4())
        jobs_before = _job_ids(state)
        started = bridge._start_job({"call": call, "mode": "new", "prompt": prompt,
                                     "cwd": str(root), "sandbox": "read-only",
                                     "jobPhase": "operation", "agent": "codex",
                                     "timeoutSeconds": JOB_TIMEOUT_SECONDS})
        value = started.get("structuredContent", {})
        job_id = value.get("jobId")
        if not isinstance(job_id, str) or not job_id:
            raise RuntimeError(value.get("message", "durable Codex job could not be started"))
        active.update(job_id=job_id)
        temporary = active_path.with_name("." + uuid.uuid4().hex + ".tmp")
        with temporary.open("x", encoding="utf-8") as stream:
            json.dump(active, stream)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, active_path)
        while True:
            if _active_user_jobs(state, exclude_job_id=job_id) is True:
                retain_slot = True
                cancellation = bridge._cancel_job({"jobId": job_id}).get("structuredContent", {})
                if cancellation.get("state") != "CANCELLED":
                    deadline = time.monotonic() + JOB_TIMEOUT_SECONDS
                    while time.monotonic() < deadline:
                        current = bridge._result_job({"jobId": job_id}).get("structuredContent", {})
                        if current.get("state") in {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"}:
                            retain_slot = False
                            break
                        time.sleep(min(poll_seconds, max(0, deadline - time.monotonic())))
                    else:
                        raise RuntimeError("maintenance cancellation was unconfirmed; global slot retained by job timeout")
                else:
                    retain_slot = False
                raise RuntimeError("maintenance yielded to a user Codex job")
            result = bridge._result_job({"jobId": job_id}).get("structuredContent", {})
            status = result.get("state")
            if status == "SUCCEEDED":
                body = result.get("content", "")
                findings = parse_findings(body) if isinstance(body, str) else None
                line_coverage = _line_coverage(body) if report_type == "test-gaps" and isinstance(body, str) else None
                after_quota = _fresh_quota()
                concurrent = _active_user_jobs(state)
                before_window = _active_quota_window(before_quota or {})
                after_window = _active_quota_window(after_quota or {})
                jobs_after = _job_ids(state)
                concurrent_job = (jobs_before is None or jobs_after is None
                                  or bool((jobs_after - jobs_before) - {job_id}))
                quota_change = "unknown"
                if (before_window and after_window and before_window[2] == after_window[2]
                        and concurrent is False and not concurrent_job):
                    quota_change = round(after_window[1] - before_window[1], 3)
                usage = result.get("usage") if isinstance(result.get("usage"), dict) else {}
                tokens = usage or result.get("tokens", result.get("totalTokens"))
                cost = {"tokens": tokens if tokens else "unknown",
                        "elapsed_seconds": round(time.monotonic() - started_clock, 3),
                        "quota_before": before_quota or "unknown",
                        "quota_after": after_quota or "unknown",
                        "quota_change_active_window_pct": quota_change}
                _write_report(report_path, body if isinstance(body, str) else "", commit=commit,
                              timestamp=timestamp, versions=versions, limitations=limitations,
                              findings=findings, cost=cost, report_type=report_type)
                codex_exec_mcp._atomic_json(_last_run_path(directory, report_type), {"completed_at": time.time()})
                codex_exec_mcp._atomic_json(report_path.with_suffix(".notice.json"), {
                    "report_type": report_type,
                    "report_path": str(report_path), "findings": findings,
                    "finding_count": len(findings) if findings is not None else None,
                    "line_coverage_pct": line_coverage,
                    "shown": False,
                })
                return report_path
            if status in codex_exec_mcp.TERMINAL:
                raise RuntimeError(f"Codex job {status}: {result.get('errorCode', '')} {result.get('message', '')}".strip())
            time.sleep(poll_seconds)
    except Exception as exc:
        failure_path.parent.mkdir(parents=True, exist_ok=True)
        codex_exec_mcp._atomic_json(failure_path, {
            "project_root": str(root), "started_at": timestamp, "job_id": job_id,
            "state": "FAILED", "error": str(exc),
            "cost": {"tokens": "unknown", "elapsed_seconds": round(time.monotonic() - started_clock, 3),
                     "quota_before": before_quota or "unknown", "quota_after": _fresh_quota() or "unknown",
                     "quota_change_active_window_pct": "unknown"},
        })
        raise
    finally:
        active_path.unlink(missing_ok=True)
        if not retain_slot:
            slot_context.__exit__(None, None, None)


def consent_context(repo: Path, *, state: Path) -> dict | None:
    """Ask once before allowing automatic maintenance with an explicit no-window snapshot."""
    root = _canonical_root(repo)
    maintenance = _maintenance_dir(root, state)
    if not _is_opted_in(root, maintenance):
        return None
    quota = _fresh_quota()
    if quota is None or _active_quota_window(quota) is not None or quota.get("windows_seen") is True:
        return None
    consent_path = maintenance / "consent.json"
    try:
        consent = json.loads(consent_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        consent = {}
    if (consent.get("project_root") != str(root)
            or consent.get("decision") not in {"yes", "no"}) and not consent.get("prompted"):
        consent.update(project_root=str(root), prompted=True)
        import codex_exec_mcp
        codex_exec_mcp._atomic_json(consent_path, consent)
        return {"kind": "consent", "notice_file": str(consent_path),
                "confirmation_evidence": ["API"]}
    return None


def notice_context(repo: Path, *, state: Path, allow_no_window: bool = True) -> dict | None:
    """Return and claim an unfinished report when its quota display threshold is met."""
    root = _canonical_root(repo)
    maintenance = _maintenance_dir(root, state)
    if not _is_opted_in(root, maintenance):
        return None
    candidates = []
    for report_type in _REPORT_TYPES:
        directory = maintenance / "reports" / report_type
        for notice_path in sorted(directory.glob("*.notice.json")):
            try:
                notice = json.loads(notice_path.read_text(encoding="utf-8"))
                if notice.get("done") is not True and notice.get("pending") is not True:
                    notice.setdefault("report_type", report_type)
                    candidates.append((notice_path, notice))
            except (OSError, ValueError, AttributeError):
                continue
    if not candidates:
        return None
    quota = _fresh_quota()
    if quota is None:
        return None
    window = _active_quota_window(quota)
    if window is None and (quota.get("windows_seen") is True or not allow_no_window):
        return None
    for notice_path, notice in candidates:
        try:
            if window is None:
                if notice.get("shown") is True:
                    continue
                show_number = 1
                resets_at = "no-window"
            else:
                label, used, resets_at = window
                remaining = 100 - used
                cycles = notice.setdefault("cycles", {})
                cycle = cycles.get(resets_at, {})
                if cycle.get("skip_second") or cycle.get("shows", 0) >= 2:
                    continue
                if cycle.get("shows", 0) == 0 and remaining > FIRST_SHOW_REMAINING_PCT:
                    continue
                if cycle.get("shows", 0) == 1 and remaining > SECOND_SHOW_REMAINING_PCT:
                    continue
                show_number = cycle.get("shows", 0) + 1
                notice["pending_skip_second"] = show_number == 1 and remaining <= SECOND_SHOW_REMAINING_PCT
                notice["quota_window"] = label
            notice["pending"] = True
            notice["pending_cycle"] = resets_at
            notice["pending_show"] = show_number
            import codex_exec_mcp
            codex_exec_mcp._atomic_json(notice_path, notice)
            evidence = []
            report_path = notice.get("report_path")
            if isinstance(report_path, str) and Path(report_path).name:
                evidence.append(Path(report_path).name)
            findings = notice.get("findings")
            if isinstance(findings, list) and findings and isinstance(findings[0], dict):
                path_line = findings[0].get("path_line")
                if isinstance(path_line, str) and path_line:
                    evidence.append(path_line)
            return {**notice, "notice_file": str(notice_path), "confirmation_evidence": evidence}
        except (OSError, ValueError, AttributeError):
            continue
    return None


def confirm_notice(notice_file: str) -> None:
    try:
        import codex_exec_mcp
        path = Path(notice_file)
        notice = json.loads(path.read_text(encoding="utf-8"))
        cycle, show_number = notice.pop("pending_cycle", "no-window"), notice.pop("pending_show", 1)
        skip_second = notice.pop("pending_skip_second", False)
        notice["pending"] = False
        if cycle == "no-window":
            notice["shown"] = True
        else:
            state = notice.setdefault("cycles", {}).setdefault(cycle, {})
            state["shows"] = max(state.get("shows", 0), show_number)
            notice["last_shown_cycle"] = cycle
            if skip_second:
                state["skip_second"] = True
        codex_exec_mcp._atomic_json(path, notice)
    except (OSError, ValueError, TypeError):
        pass


def format_notice(notice: dict) -> str:
    if notice.get("kind") == "consent":
        return ("> ⚓ **While you work, Purser can scan your project in the background with its proactive cleanup module. "
                "This scan is billed to your API account. Run it?**")
    path = notice.get("report_path", "unknown")
    count = notice.get("finding_count")
    findings = notice.get("findings")
    if notice.get("report_type") == "test-gaps":
        count_text = str(count) if isinstance(count, int) else "an unknown number of"
        intro = (f"⚓ **Purser checked your tests in the background while you worked: the report lists {count_text} missing tests.**")
        lines = []
        coverage = notice.get("line_coverage_pct")
        if (isinstance(coverage, (int, float)) and not isinstance(coverage, bool) and coverage > 90):
            lines.append(f"> ⚓ **Purser: your code is now {int(coverage + 0.5)}% covered by tests! 🚀** You have reached a top-tier quality bar. Purser found the gaps for you in the background while you focused on your work. Type /purser:optimize for the last few steps.")
        lines.append(f"> {intro}")
        if not isinstance(findings, list):
            lines.append(f"> *The findings could not be parsed; report: `{path}`.*")
        else:
            ranked = sorted(findings, key=lambda item: item.get(
                "confidence_rank", _confidence_rank(item.get("confidence", ""))), reverse=True)[:5]
            lines.extend([f"> *The table below shows each gap and its evidence; the full report is at `{path}`.*",
                          "> | Gap | Evidence |", "> | --- | --- |"])
            for item in ranked:
                candidate = f"{item.get('path_line', '?')} {item.get('symbol', '?')}"
                evidence = " ".join(str(item.get("evidence", "")).split())[:48]
                lines.append(f"> | {candidate[:48]} | {evidence} |")
        if (notice.get("pending_cycle") not in {None, "no-window"}
                and notice.get("pending_show") == 1 and not notice.get("pending_skip_second")):
            lines.append("> *Skip showing this again in this cycle?*")
        lines.append("> *You can turn the module off at any time, or run it any time with /purser:optimize.*")
        return "\n".join(lines)
    intro = (f"⚓ **While you worked, Purser scanned your project in the background with its proactive cleanup module: the report has {count} findings.**"
             if isinstance(count, int) else
             "⚓ **While you worked, Purser scanned your project in the background with its proactive cleanup module: the number of findings is unknown.**")
    if not isinstance(findings, list):
        return f"> {intro}\n> *The findings could not be parsed; report: `{path}`.*"
    ranked = sorted(findings, key=lambda item: item.get(
        "confidence_rank", _confidence_rank(item.get("confidence", ""))), reverse=True)
    shown = ranked if len(findings) <= 5 else ranked[:5]
    instruction = ('The table below lists each finding and its evidence. Just say "clean up" to have them cleaned.'
                   if len(findings) <= 5 else
                   f'The table below lists the top 5 findings and their evidence. Just say "clean up" to have them cleaned. The other findings are in this file to save tokens: `{path}`')
    rows = ["> | Finding | Evidence |", "> | --- | --- |"]
    for item in shown:
        candidate = f"{item.get('path_line', '?')} {item.get('symbol', '?')}"
        evidence = " ".join(str(item.get("evidence", "")).split())[:48]
        rows.append(f"> | {candidate[:48]} | {evidence} |")
    lines = [f"> {intro}", f"> *{instruction}*", *rows]
    if (notice.get("pending_cycle") not in {None, "no-window"}
            and notice.get("pending_show") == 1 and not notice.get("pending_skip_second")):
        lines.append("> *Skip showing this again in this cycle?*")
    lines.append("> *You can turn the module off at any time, or run it any time with /purser:optimize.*")
    return "\n".join(lines)


def mark_report_done(repo: str | os.PathLike[str], report: str, *, state: Path | None = None) -> None:
    root = _canonical_root(repo)
    path = Path(report).resolve()
    report_type = path.parent.name
    expected_dir = _maintenance_dir(root, state or state_root.default_state_root()) / "reports" / report_type
    if report_type not in _REPORT_TYPES or path.parent != expected_dir.resolve() or not path.name.endswith(".notice.json"):
        raise ValueError("report notice must belong to this project")
    notice = json.loads(path.read_text(encoding="utf-8"))
    notice.update(done=True, pending=False, shown=True)
    import codex_exec_mcp
    codex_exec_mcp._atomic_json(path, notice)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    enable = commands.add_parser("opt-in", help="explicitly enable reports for one project")
    enable.add_argument("--repo", required=True)
    disable = commands.add_parser("opt-out", help="disable reports for one project")
    disable.add_argument("--repo", required=True)
    consent = commands.add_parser("consent", help="record API maintenance consent or decline")
    consent.add_argument("decision", choices=("yes", "no"))
    consent.add_argument("--repo", required=True)
    skip = commands.add_parser("skip-second-show", help="skip a report's second show this quota cycle")
    skip.add_argument("--repo", required=True)
    skip.add_argument("--report", required=True)
    done = commands.add_parser("mark-done", help="mark a completed cleanup report as done")
    done.add_argument("--repo", required=True)
    done.add_argument("--report", required=True)
    run = commands.add_parser("run", help="run a maintenance report")
    run.add_argument("report_type", choices=_REPORT_TYPES)
    run.add_argument("--repo", required=True)
    check = commands.add_parser("check", help="run a delayed idle maintenance check")
    check.add_argument("--repo", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "opt-in":
            print(opt_in(args.repo))
        elif args.command == "opt-out":
            print(opt_out(args.repo))
        elif args.command == "consent":
            print(record_consent(args.repo, args.decision))
        elif args.command == "skip-second-show":
            skip_second_show(args.repo, args.report)
        elif args.command == "mark-done":
            mark_report_done(args.repo, args.report)
        elif args.command == "run":
            print(run_report(args.repo, report_type=args.report_type))
        else:
            delayed_check(Path(args.repo))
        return 0
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"maintenance report failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
