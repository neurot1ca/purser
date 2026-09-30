"""Manually run read-only maintenance reports outside project repositories."""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import uuid

import codex_exec_mcp
import project_root
import state_root


JOB_TIMEOUT_SECONDS = 2700


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
    return marker


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
                  versions: list[str], limitations: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    content = ("# Cleanup report\n\n"
               f"- Source commit: `{commit}`\n- Run time (UTC): `{timestamp}`\n"
               f"- Tool versions: {', '.join(versions)}\n- Limitations: {limitations}\n\n"
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


def _active_is_stale(active_path: Path, bridge: codex_exec_mcp.Server) -> bool:
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
                server: codex_exec_mcp.Server | None = None, poll_seconds: float = 1.0) -> Path:
    root = _canonical_root(repo)
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
    active_path = directory / "cleanup-active.json"
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    active = {"project_root": str(root), "started_at": timestamp, "state": "RUNNING"}
    bridge = server or codex_exec_mcp.Server(job_dir=state / "codex_exec_mcp" / "jobs", agent="codex")
    try:
        _create_active(active_path, active)
    except FileExistsError as exc:
        if not _active_is_stale(active_path, bridge):
            raise RuntimeError(f"A cleanup report is already running for {root}.") from exc
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
            raise RuntimeError(f"A cleanup report is already running for {root}.") from duplicate

    reports = directory / "reports" / "cleanup"
    report_path = reports / (timestamp + ".md")
    failure_path = reports / "failures" / (timestamp + ".json")
    job_id = None
    try:
        commit = _source_commit(root)
        vulture_version, limitations = _tool_info()
        versions = [f"Python: {sys.version.split()[0]}", "Git: " + subprocess.run(
            ["git", "--version"], cwd=root, capture_output=True, text=True, timeout=10,
            check=True,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0).stdout.strip(),
                    vulture_version]
        prompt = f"""Produce a cleanup report for the repository at {root}.
This is a strictly read-only inspection. Do not edit files, create repository files,
install tools, or run package managers. Report only evidence-backed candidates.
For every item include (1) exact tool output or search evidence, (2) repository
path and symbol, and (3) checks against entry points, hooks, and dynamic/reflection
use before calling it unused. If vulture is available, use its installed version;
otherwise state that vulture is unavailable and use grep-based searches, explaining
that grep can miss indirect or dynamic use. Never install any tool. Include the
commands/searches used and distinguish uncertain candidates from confirmed facts.
"""
        call = str(uuid.uuid4())
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
            result = bridge._result_job({"jobId": job_id}).get("structuredContent", {})
            status = result.get("state")
            if status == "SUCCEEDED":
                _write_report(report_path, result.get("content", ""), commit=commit,
                              timestamp=timestamp, versions=versions, limitations=limitations)
                return report_path
            if status in codex_exec_mcp.TERMINAL:
                raise RuntimeError(f"Codex job {status}: {result.get('errorCode', '')} {result.get('message', '')}".strip())
            time.sleep(poll_seconds)
    except Exception as exc:
        failure_path.parent.mkdir(parents=True, exist_ok=True)
        codex_exec_mcp._atomic_json(failure_path, {
            "project_root": str(root), "started_at": timestamp, "job_id": job_id,
            "state": "FAILED", "error": str(exc),
        })
        raise
    finally:
        active_path.unlink(missing_ok=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    enable = commands.add_parser("opt-in", help="explicitly enable reports for one project")
    enable.add_argument("--repo", required=True)
    run = commands.add_parser("run", help="run a maintenance report")
    run.add_argument("report_type", choices=("cleanup",))
    run.add_argument("--repo", required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "opt-in":
            print(opt_in(args.repo))
        else:
            print(run_cleanup(args.repo))
        return 0
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"maintenance report failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
