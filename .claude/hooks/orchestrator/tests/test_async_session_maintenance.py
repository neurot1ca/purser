import json
import os
import hashlib
import subprocess
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import sys
from datetime import datetime, timezone

sys.path.insert(0, str(Path(__file__).parents[1]))
import async_session_maintenance as maintenance
import codex_exec_mcp
import ledger
import telemetry_schema
import pytest
from mixed_owner_support import go_test_tools, launch_go_host

PROJECT_KEY = "0123456789abcdef"

def test_startup_entrypoint_runs_cleanup_with_ceiling_check(tmp_path, monkeypatch):
    calls = []
    monkeypatch.setattr(maintenance, "cleanup_jobs",
                        lambda root, **kwargs: calls.append((root, kwargs)))

    assert maintenance.main(["--startup", str(tmp_path)]) == 0
    assert calls == [(str(tmp_path), {"check_ceiling": True})]

def event(kind, route=None, duration=None, project=PROJECT_KEY, stamp="2026-09-01T00:00:00Z"):
    payload = {}
    if route is not None:
        payload["route"] = route
    if duration is not None:
        payload["duration_ms"] = duration
    return {"event_type": kind, "project_key": project, "payload": payload, "ts_utc": stamp}

def test_duration_hint_uses_newest_files_when_file_byte_and_line_caps_hit(tmp_path, monkeypatch):
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    older = month / "session_older.jsonl"
    older.write_text("\n".join(json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 600_000)) for _ in range(10)),
                     encoding="utf-8")
    newest = month / "session_newest.jsonl"
    latest_lines = [json.dumps(event("IMPLEMENTER_FINISHED", "mechanical", 120_000)).encode() + b"\n"
                    for _ in range(11)]
    newest.write_bytes(b"".join(latest_lines))
    os.utime(older, (1, 1))
    os.utime(newest, (2, 2))
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_FILES", 1)
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_LINES", 10)
    monkeypatch.setattr(maintenance, "MAX_TELEMETRY_BYTES", sum(map(len, latest_lines[:10])))

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 10, 1, tzinfo=timezone.utc).timestamp()
    )

    assert hint == "Purser: typical Codex durations here (median/p90, n): mechanical 2/2 min (n=10)."

def _job_fixture(root, state, finished, *, stage="PENDING"):
    call = "job-" + state + "-" + stage
    job_id = hashlib.sha256(call.encode()).hexdigest()
    jobs = root / "codex_exec_mcp" / "jobs"
    indices = root / "async_jobs"
    pending_dir = root / "pending"
    for directory in (jobs, indices, pending_dir):
        directory.mkdir(parents=True, exist_ok=True)
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "call": call, "state": state, "acceptedAt": 1,
        "finishedAt": finished, "outputFile": f"{job_id}.output"}), encoding="utf-8")
    (jobs / f"{job_id}.output").write_text("result", encoding="utf-8")
    (jobs / f"{job_id}.launch").write_text("secret prompt", encoding="utf-8")
    (indices / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "call": call, "call_key": job_id,
        "project_key": PROJECT_KEY, "stage": stage}), encoding="utf-8")
    pending = pending_dir / f"{job_id}.json"
    pending.write_text(json.dumps({"job_id": job_id, "async_call": call,
        "lease_owner": job_id, "route": "subtle", "project_key": PROJECT_KEY}), encoding="utf-8")
    lease = ledger.ScopeLeases(root / "projects" / PROJECT_KEY)
    assert lease.acquire(job_id, "task-a", ["file.py"])["status"] == ledger.OK
    return job_id, pending, lease

def _preparation(root, job_id, state="PREPARED", *, index_stage="CONSUMED", apply_state="applied",
                 stamp=1000, record=True, revision=True):
    generation = root / "preparations" / job_id / ("revision-1" if revision else "")
    generation.mkdir(parents=True)
    (generation / "state.json").write_text(json.dumps({"state": state}), encoding="utf-8")
    os.utime(generation, (stamp, stamp))
    os.utime(generation / "state.json", (stamp, stamp))
    if record:
        job_path = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
        job_path.parent.mkdir(parents=True, exist_ok=True)
        job = {"jobId": job_id, "call": job_id, "state": "SUCCEEDED", "finishedAt": stamp}
        if apply_state is not None:
            job["applyState"] = apply_state
        job_path.write_text(json.dumps(job), encoding="utf-8")
    if index_stage is not None:
        index_path = root / "async_jobs" / f"{job_id}.json"
        index_path.parent.mkdir(parents=True, exist_ok=True)
        index_path.write_text(json.dumps({"jobId": job_id, "stage": index_stage}), encoding="utf-8")
    return generation

def test_worktree_owner_map_read_cost_scales_with_records_plus_worktrees(tmp_path, monkeypatch):
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    wt_root = tmp_path / "wt"
    record_count, worktree_count = 9, 7
    for index in range(record_count):
        worktree = wt_root / f"{index:03}"
        if index < worktree_count:
            worktree.mkdir(parents=True)
        record = {"jobId": f"{index + 1:064x}", "worktreePath": str(worktree),
                  "repoRoot": str(tmp_path / "repo"), "applyState": "applied"}
        (jobs / f"{record['jobId']}.json").write_text(json.dumps(record), encoding="utf-8")
    reads = 0
    read_json = maintenance._read_json

    def counted_read(path):
        nonlocal reads
        if Path(path).parent == jobs:
            reads += 1
        return read_json(path)

    monkeypatch.setattr(maintenance, "_read_json", counted_read)
    maintenance._enforce_worktree_count(tmp_path, 10_000_000, set(), {}, {})
    assert reads <= record_count + worktree_count

def test_preparation_generation_listing_runs_once_per_scan(tmp_path, monkeypatch):
    for index in range(4):
        _preparation(tmp_path, f"{index + 1:064x}")
    calls = 0
    list_generations = maintenance._preparation_generations

    def counted_listing(root):
        nonlocal calls
        calls += 1
        return list_generations(root)

    monkeypatch.setattr(maintenance, "_preparation_generations", counted_listing)
    maintenance._retire_preparations(tmp_path, 2000, check_bytes=False)
    assert calls == 1

def test_preparation_removal_budget_limits_one_scan(tmp_path):
    generations = [_preparation(tmp_path, f"{index + 1:064x}") for index in range(12)]
    maintenance._retire_preparations(tmp_path, 2000, check_bytes=False)
    removed = sum(not generation.exists() for generation in generations)
    assert 0 < removed <= getattr(maintenance, "MAX_PREPARATION_REMOVALS_PER_SCAN", 8)

def test_cleanup_lists_each_repository_once_for_multiple_removals(tmp_path, monkeypatch):
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "Test"], check=True)
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "base"], check=True)
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    worktrees = []
    for index in range(2):
        job_id = f"{index + 1:064x}"
        worktree = tmp_path / "wt" / str(index)
        worktree.parent.mkdir(exist_ok=True)
        subprocess.run(["git", "-C", str(repo), "worktree", "add", "--detach", str(worktree), "HEAD"], check=True)
        worktrees.append(worktree)
        (jobs / f"{job_id}.json").write_text(json.dumps({
            "jobId": job_id, "state": "SUCCEEDED", "finishedAt": 1000,
            "applyState": "applied", "worktreePath": str(worktree), "repoRoot": str(repo),
        }), encoding="utf-8")

    calls = []
    run = maintenance.subprocess.run

    def counted_run(command, *args, **kwargs):
        if "worktree" in command and "list" in command:
            calls.append(command)
        return run(command, *args, **kwargs)

    monkeypatch.setattr(maintenance.subprocess, "run", counted_run)
    maintenance.cleanup_jobs(tmp_path, now=2000)

    assert len(calls) == 1
    assert all(not worktree.exists() for worktree in worktrees)

def test_cleanup_does_not_retry_failed_repository_listing(tmp_path, monkeypatch):
    repo = tmp_path / "repo"
    repo.mkdir()
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    for index in range(2):
        job_id = f"{index + 1:064x}"
        worktree = tmp_path / "wt" / str(index)
        worktree.mkdir(parents=True)
        (jobs / f"{job_id}.json").write_text(json.dumps({
            "jobId": job_id, "state": "SUCCEEDED", "finishedAt": 1000,
            "applyState": "applied", "worktreePath": str(worktree), "repoRoot": str(repo),
        }), encoding="utf-8")

    calls = []
    run = maintenance.subprocess.run

    def failed_listing(command, *args, **kwargs):
        if "worktree" in command and "list" in command:
            calls.append(command)
            return subprocess.CompletedProcess(command, 1, stdout="", stderr="failed")
        return run(command, *args, **kwargs)

    monkeypatch.setattr(maintenance.subprocess, "run", failed_listing)
    maintenance.cleanup_jobs(tmp_path, now=2000)

    assert len(calls) == 1

def test_cleanup_skips_listing_for_absent_worktrees_and_prunes_once(tmp_path, monkeypatch):
    repo = tmp_path / "repo"
    repo.mkdir()
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    for index in range(2):
        job_id = f"{index + 1:064x}"
        (jobs / f"{job_id}.json").write_text(json.dumps({
            "jobId": job_id, "state": "SUCCEEDED", "finishedAt": 1000,
            "applyState": "applied", "worktreePath": str(tmp_path / "wt" / str(index)),
            "repoRoot": str(repo),
        }), encoding="utf-8")

    calls = []

    def run(command, *args, **kwargs):
        if "worktree" in command:
            calls.append(command)
            return subprocess.CompletedProcess(command, 0, stdout="", stderr="")
        raise AssertionError(command)

    monkeypatch.setattr(maintenance.subprocess, "run", run)
    maintenance.cleanup_jobs(tmp_path, now=2000)

    assert not any("list" in call for call in calls)
    assert sum("prune" in call for call in calls) == 1

def test_cleanup_parses_summary_at_most_once_per_scan(tmp_path, monkeypatch):
    records = []
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    for index in range(3):
        job_id = f"{index + 1:064x}"
        record = jobs / f"{job_id}.json"
        job = {"jobId": job_id, "state": "SUCCEEDED", "finishedAt": 1000}
        record.write_text(json.dumps(job), encoding="utf-8")
        records.append((record, job))
    maintenance.learning_summary.distill_jobs(tmp_path, records, now=1000)
    reads = []
    original = maintenance.learning_summary.read_summary
    monkeypatch.setattr(maintenance.learning_summary, "read_summary",
                        lambda root: (reads.append(root), original(root))[1])
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400, check_ceiling=False)
    assert len(reads) == 1

def _recoverable(root, *, thread_id=None, stage="PENDING", attempts=0):
    job_id, pending, lease = _job_fixture(root, "RUNNING", 1, stage=stage)
    jobs = root / "codex_exec_mcp" / "jobs"
    record = jobs / f"{job_id}.json"
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, pid=789,
               hostCreatedAt=456, hostContained=True, deadlineAt=2000,
               resumedAfterInterruption=attempts)
    if thread_id:
        job["threadId"] = thread_id
        job["threadConfig"] = {"cwd": str(root), "model": None,
                               "config_args": [], "sandbox": "workspace-write"}
    record.write_text(json.dumps(job), encoding="utf-8")
    (jobs / f"{job_id}.launch").write_text(json.dumps({
        "name": "codex", "args": {"prompt": "original", "timeoutSeconds": 90},
        "codexBin": "codex"}), encoding="utf-8")
    return job_id, record, pending, lease

def test_concurrent_recoverers_launch_one_host(tmp_path, monkeypatch):
    job_id, record, pending, lease = _recoverable(tmp_path)
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    launches = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launches.append(args))
    bridge = codex_exec_mcp.Server(job_dir=record.parent)
    with ThreadPoolExecutor(max_workers=2) as pool:
        list(pool.map(lambda action: action(),
                      (lambda: maintenance.cleanup_jobs(tmp_path, now=1000),
                       lambda: bridge.recover_job(job_id, now=1000))))
    job = json.loads(record.read_text())
    assert len(launches) == 1
    assert job["generation"] == 2 and job["resumedAfterInterruption"] == 1
    assert pending.exists() and lease._read()["owners"][job_id]["state"] == "ACTIVE"
    assert lease._read()["owners"][job_id]["retention_after"] >= job["deadlineAt"] + codex_exec_mcp.RETENTION_SECONDS

