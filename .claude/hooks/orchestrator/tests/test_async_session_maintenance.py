import json
import os
import hashlib
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import sys
from datetime import datetime, timezone

sys.path.insert(0, str(Path(__file__).parents[1]))
import async_session_maintenance as maintenance
import codex_exec_mcp
import ledger
import telemetry_schema


PROJECT_KEY = "0123456789abcdef"


def _single_followup_job(tmp_path, state, observed=False):
    call = "failed-call"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256(b"session-1\0tool-1").hexdigest()
    records = (
        ("async_jobs", job_id, {"jobId": job_id, "call": call, "call_key": call_key,
                                "session_id": "session-1", "project_key": PROJECT_KEY}),
        ("pending", call_key, {"job_id": job_id, "async_call": call,
                               "session_id": "session-1", "project_key": PROJECT_KEY,
                               "tool_use_id": "tool-1", "task_id": "failed-task"}),
        ("codex_exec_mcp/jobs", job_id, {"jobId": job_id, "call": call, "state": state,
                                           **({"waiterObservedTerminalAt": 123,
                                               "waiterObservedTerminalState": state} if observed else {})}),
    )
    for folder, name, value in records:
        path = tmp_path / folder / f"{name}.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value), encoding="utf-8")


def test_same_session_observed_failed_job_has_no_followup_hint(tmp_path):
    _single_followup_job(tmp_path, "FAILED", observed=True)
    assert maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-1") is None
    assert "adopt: true" in maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-2")


def test_same_session_unobserved_failed_job_is_reminded(tmp_path):
    _single_followup_job(tmp_path, "FAILED")
    assert "Call codex_result" in maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-1")


def test_stalled_followup_keeps_waiting_and_instructs_user_receipt(tmp_path):
    _single_followup_job(tmp_path, "RUNNING")
    job_path = next((tmp_path / "codex_exec_mcp" / "jobs").glob("*.json"))
    job = json.loads(job_path.read_text(encoding="utf-8"))
    job.update(errorCode="STALLED", stallDetectedAt=time.time(),
               liveness={"lastActivityAt": time.time() - 125})
    job_path.write_text(json.dumps(job), encoding="utf-8")

    hint = maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-1")

    assert "do not call codex_result" in hint.lower()
    assert "Restart purser_wait.py for this same jobId" in hint
    assert 'Tell the user in the user\'s language exactly one line, translated from: "> ⚓ **Purser has seen no worker activity for 2 minutes.** The job is still running; I\'m checking its status."' in hint


def test_same_session_observed_success_is_still_reminded(tmp_path):
    _single_followup_job(tmp_path, "SUCCEEDED", observed=True)
    assert "Call codex_result" in maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-1")


def test_same_session_unobserved_unknown_job_is_reminded(tmp_path):
    _single_followup_job(tmp_path, "UNKNOWN_OR_IN_PROGRESS")
    assert "Call codex_result" in maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-1")
    assert maintenance.followup_hints(tmp_path, PROJECT_KEY, "session-2") is None


def event(kind, route=None, duration=None, project=PROJECT_KEY, stamp="2026-09-01T00:00:00Z"):
    payload = {}
    if route is not None:
        payload["route"] = route
    if duration is not None:
        payload["duration_ms"] = duration
    return {"event_type": kind, "project_key": project, "payload": payload, "ts_utc": stamp}


def test_duration_hint_reads_real_layout_and_skips_irrelevant_records(tmp_path):
    events_root = tmp_path / "telemetry" / "events"
    september = events_root / "2026-09"
    september.mkdir(parents=True)
    sample_events = [event("IMPLEMENTER_FINISHED", "mechanical", 120_000) for _ in range(10)]
    sample_events += [event("IMPLEMENTER_FINISHED", duration=600_000)]
    sample_events += [event("IMPLEMENTER_FINISHED", "subtle", 600_000, project="fedcba9876543210")]
    sample_events += [event("IMPLEMENTER_FAILED", "subtle", 900_000)]
    contents = "{malformed json\n" + "\n".join(json.dumps(item) for item in sample_events)
    for index in range(36):
        path = september / f"session_{index:03}.jsonl"
        path.write_text(contents if index == 35 else "{}\n", encoding="utf-8")
        os.utime(path, (index + 1, index + 1))

    july = events_root / "2026-07"
    july.mkdir()
    (july / "session_boundary.jsonl").write_text(
        json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 9_000_000, stamp="2026-07-02T23:59:00Z")) + "\n"
        + json.dumps(event("IMPLEMENTER_FINISHED", "subtle", 9_000_000, stamp="2026-07-04T00:00:00Z")),
        encoding="utf-8",
    )
    by_task = events_root / "by_task"
    by_task.mkdir()
    (by_task / "duplicate.jsonl").write_text(
        "\n".join(json.dumps(item) for item in sample_events[:10]), encoding="utf-8"
    )

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 10, 1, tzinfo=timezone.utc).timestamp()
    )

    assert hint == "Purser: typical Codex durations here (median/p90, n): mechanical 2/2 min (n=10)."


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


def test_duration_hint_prefers_agent_after_five_and_keeps_other_samples(tmp_path):
    emitted = telemetry_schema.build_event("IMPLEMENTER_FINISHED", task_id="task", project_key=PROJECT_KEY,
                                           payload={"implementer": "codex-main", "route": "normal",
                                                    "duration_ms": 120000})
    assert telemetry_schema.valid_event(emitted)
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    records = []
    for agent, count, minutes in (("codex-main", 4, 2), ("claude", 10, 10)):
        for _ in range(count):
            item = event("IMPLEMENTER_FINISHED", "mechanical", minutes * 60000)
            item["payload"]["implementer"] = agent
            records.append(item)
    (month / "session_agents.jsonl").write_text(
        "{bad\n" + "\n".join(json.dumps(item) for item in records), encoding="utf-8")
    now = datetime(2026, 9, 2, tzinfo=timezone.utc).timestamp()
    pooled = maintenance.duration_hint(tmp_path, PROJECT_KEY, now=now)
    assert "mechanical 10/10 min (n=14)" in pooled
    records.append(records[0])
    (month / "session_agents.jsonl").write_text("\n".join(map(json.dumps, records)), encoding="utf-8")
    own = maintenance.duration_hint(tmp_path, PROJECT_KEY, now=now)
    other = maintenance.duration_hint(tmp_path, PROJECT_KEY, now=now, implementer="claude")
    assert "mechanical 2/2 min (n=5)" in own
    assert "typical Codex durations" in own
    assert "mechanical 10/10 min (n=10)" in other
    assert "typical Claude durations" in other


def test_duration_hint_pools_codex_aliases_at_five_samples(tmp_path):
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    records = []
    for agent, count, minutes in (("codex", 3, 2), ("codex-main", 2, 8)):
        for _ in range(count):
            item = event("IMPLEMENTER_FINISHED", "mechanical", minutes * 60000)
            item["payload"]["implementer"] = agent
            records.append(item)
    (month / "session_aliases.jsonl").write_text("\n".join(map(json.dumps, records)), encoding="utf-8")

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 9, 2, tzinfo=timezone.utc).timestamp()
    )

    assert "mechanical 2/8 min (n=5)" in hint


def test_duration_hint_missing_implementer_stays_route_only(tmp_path):
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    records = []
    for agent, count, minutes in (("codex-main", 4, 2), (None, 1, 8), ("claude", 5, 10)):
        for _ in range(count):
            item = event("IMPLEMENTER_FINISHED", "mechanical", minutes * 60000)
            if agent is not None:
                item["payload"]["implementer"] = agent
            records.append(item)
    (month / "session_missing.jsonl").write_text("\n".join(map(json.dumps, records)), encoding="utf-8")

    hint = maintenance.duration_hint(
        tmp_path, PROJECT_KEY, now=datetime(2026, 9, 2, tzinfo=timezone.utc).timestamp()
    )

    assert "mechanical 9/10 min (n=10)" in hint


def test_compact_accuracy_baseline_improvement_regression_and_bad_data(tmp_path):
    month = tmp_path / "telemetry" / "events" / "2026-09"
    month.mkdir(parents=True)
    path = month / "session_accuracy.jsonl"
    item = event("IMPLEMENTER_FINISHED", "normal", 120000)
    item["payload"].update(implementer="codex-main", eta_minutes=4)
    now = datetime(2026, 9, 2, tzinfo=timezone.utc).timestamp()
    malformed = event("IMPLEMENTER_FINISHED", "normal", 120000)
    malformed["payload"].update(implementer="codex-main", eta_minutes="oops")
    path.write_text("{bad\n" + json.dumps(malformed) + "\n" + json.dumps(item), encoding="utf-8")
    for _ in range(4):
        assert maintenance.compact_accuracy_hint(tmp_path, PROJECT_KEY, now=now) is None
    state_path = tmp_path / "projects" / PROJECT_KEY / "duration_accuracy.json"
    assert json.loads(state_path.read_text())["compacts"] == 4
    assert maintenance.compact_accuracy_hint(tmp_path, PROJECT_KEY, now=now) is None
    assert json.loads(state_path.read_text())["errors"]["codex-main"] == 100
    item["payload"]["eta_minutes"] = 3
    path.write_text("{bad\n" + json.dumps(item), encoding="utf-8")
    for _ in range(4):
        assert maintenance.compact_accuracy_hint(tmp_path, PROJECT_KEY, now=now) is None
    message = maintenance.compact_accuracy_hint(tmp_path, PROJECT_KEY, now=now)
    assert message == ('Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: '
                       '"> ⚓ **Codex\'s task duration estimates have improved through the self-learning system! '
                       'The estimated margin of error is now 50%.**"')
    item["payload"]["eta_minutes"] = 5
    path.write_text(json.dumps(item), encoding="utf-8")
    for _ in range(5):
        assert maintenance.compact_accuracy_hint(tmp_path, PROJECT_KEY, now=now) is None
    assert json.loads(state_path.read_text())["errors"]["codex-main"] == 150


def test_compact_accuracy_claude_and_unknown_possessives(tmp_path):
    now = datetime(2026, 9, 2, tzinfo=timezone.utc).timestamp()
    for agent, name in (("claude", "Claude"), ("agent-x", "agent-x")):
        root = tmp_path / agent
        month = root / "telemetry" / "events" / "2026-09"
        month.mkdir(parents=True)
        item = event("IMPLEMENTER_FINISHED", "normal", 120000)
        item["payload"].update(implementer=agent, eta_minutes=3)
        (month / "session_accuracy.jsonl").write_text(json.dumps(item), encoding="utf-8")
        state_path = root / "projects" / PROJECT_KEY / "duration_accuracy.json"
        state_path.parent.mkdir(parents=True)
        state_path.write_text(json.dumps({"compacts": 4, "errors": {agent: 100}}), encoding="utf-8")
        assert maintenance.compact_accuracy_hint(root, PROJECT_KEY, now=now) == (
            f'Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: '
            f'"> ⚓ **{name}\'s task duration estimates have improved through the self-learning system! '
            'The estimated margin of error is now 50%.**"'
        )


def test_cleanup_live_job_preserves_pending_and_lease(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "RUNNING", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "ALIVE")
    maintenance.cleanup_jobs(tmp_path, now=1000 + 9 * 86400)
    assert (tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json").exists()
    assert pending.exists()
    assert lease._read()["owners"][job_id]["state"] == "ACTIVE"


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


def test_cleanup_dead_host_reconciles_once_to_ambiguous(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "RUNNING", 1)
    record = tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    job = json.loads(record.read_text())
    job.update(hostPid=123, hostContained=True)
    record.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    maintenance.cleanup_jobs(tmp_path, now=1000)
    first = json.loads(record.read_text())
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert json.loads(record.read_text()) == first
    assert first["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert first["errorCode"] == "RECOVERY_PROOF_MISSING"
    assert pending.exists() and lease._read()["owners"][job_id]["state"] == "ACTIVE"
    assert record.with_suffix(".launch").exists()


def test_cleanup_consumed_job_does_not_refinalize(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000, stage="CONSUMED")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    emitted = []
    monkeypatch.setattr(maintenance.telemetry, "emit", lambda *a, **kw: emitted.append(a))
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert (tmp_path / "async_jobs" / f"{job_id}.json").exists()
    assert pending.exists() and not emitted


def test_cleanup_removes_expired_terminal_evidence_pending_and_lease_once(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    emitted = []
    monkeypatch.setattr(maintenance.telemetry, "emit", lambda *a, **kw: emitted.append(a))
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    assert not (jobs / f"{job_id}.json").exists()
    assert not (jobs / f"{job_id}.output").exists()
    assert not (tmp_path / "async_jobs" / f"{job_id}.json").exists()
    assert not pending.exists()
    assert lease._read()["owners"][job_id]["state"] == "RELEASED"
    assert len(emitted) == 1 and emitted[0][0] == "CODEX_RESULT_UNFETCHED"


def test_cleanup_removes_launch_payload_for_recent_terminal_job(tmp_path, monkeypatch):
    job_id, pending, _ = _job_fixture(tmp_path, "FAILED", 1000)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    assert (jobs / f"{job_id}.json").exists() and pending.exists()
    assert not (jobs / f"{job_id}.launch").exists()


def _registered_job_worktree(root, state, *, apply_state=None, outside=False):
    job_id, _, _ = _job_fixture(root, state, 1000)
    repo = root / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.email", "test@example.invalid"], check=True)
    subprocess.run(["git", "-C", str(repo), "config", "user.name", "Test"], check=True)
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    subprocess.run(["git", "-C", str(repo), "add", "tracked.txt"], check=True)
    subprocess.run(["git", "-C", str(repo), "commit", "-qm", "base"], check=True)
    worktree = (root / "outside" / job_id[:12]) if outside else (root / "wt" / job_id[:12])
    worktree.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "-C", str(repo), "worktree", "add", "--detach", str(worktree), "HEAD"], check=True)
    record = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(worktreePath=str(worktree), repoRoot=str(repo))
    if apply_state is not None:
        job["applyState"] = apply_state
    record.write_text(json.dumps(job), encoding="utf-8")
    return job_id, repo, worktree, record


def test_cleanup_removes_applied_job_worktree(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert not worktree.exists()


def test_cleanup_keeps_review_pending_job_worktree(tmp_path, monkeypatch):
    _, _, worktree, record = _registered_job_worktree(tmp_path, "SUCCEEDED")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    assert worktree.is_dir() and record.exists()


def test_cleanup_keeps_conflicted_job_worktree(tmp_path, monkeypatch):
    _, _, worktree, record = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="conflict")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    assert worktree.is_dir() and record.exists()


def test_cleanup_keeps_ambiguous_job_worktree(tmp_path, monkeypatch):
    _, _, worktree, record = _registered_job_worktree(tmp_path, "UNKNOWN_OR_IN_PROGRESS")
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    assert worktree.is_dir() and record.exists()


def test_cleanup_removes_orphan_worktree_older_than_seven_days(tmp_path):
    _, _, worktree, record = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="conflict")
    record.unlink()
    old = 1000
    os.utime(worktree, (old, old))
    maintenance.cleanup_jobs(tmp_path, now=old + 8 * 86400)
    assert not worktree.exists()


def test_cleanup_never_removes_worktree_outside_state_wt(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied", outside=True)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert worktree.is_dir()


def test_cleanup_preserves_finalizing_evidence_for_retry(tmp_path, monkeypatch):
    job_id, pending, lease = _job_fixture(tmp_path, "SUCCEEDED", 1000, stage="FINALIZING")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)
    assert (tmp_path / "codex_exec_mcp" / "jobs" / f"{job_id}.json").exists()
    assert pending.exists() and lease._read()["owners"][job_id]["state"] == "ACTIVE"


def _recoverable(root, *, thread_id=None, stage="PENDING", attempts=0):
    job_id, pending, lease = _job_fixture(root, "RUNNING", 1, stage=stage)
    jobs = root / "codex_exec_mcp" / "jobs"
    record = jobs / f"{job_id}.json"
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, pid=789,
               hostCreatedAt=456, hostContained=True, deadlineAt=100,
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


def test_recovery_with_thread_resumes_and_without_thread_inspects(tmp_path, monkeypatch):
    for thread_id in ("thread-1", None):
        root = tmp_path / (thread_id or "fresh")
        job_id, record, _, _ = _recoverable(root, thread_id=thread_id)
        monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
        monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
        monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: None)
        maintenance.cleanup_jobs(root, now=1000)
        launch = json.loads(record.with_suffix(".launch").read_text())
        assert launch["name"] == ("codex-reply" if thread_id else "codex")
        assert launch["args"].get("threadId") == thread_id
        assert "previous execution was interrupted" in launch["args"]["prompt"].lower()
        assert json.loads(record.read_text())["state"] == "QUEUED"


def test_recovery_attempt_cap_and_unconfirmed_and_consumed(tmp_path, monkeypatch):
    job_id, record, _, _ = _recoverable(tmp_path / "cap", attempts=2)
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    launches = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launches.append(args))
    maintenance.cleanup_jobs(tmp_path / "cap", now=1000)
    assert json.loads(record.read_text())["errorCode"] == "RECOVERY_ATTEMPTS_EXHAUSTED"
    for label, host, stage in (("uncertain", "UNCONFIRMED", "PENDING"),
                               ("consumed", "DEAD", "CONSUMED")):
        root = tmp_path / label
        _, path, _, _ = _recoverable(root, stage=stage)
        before = path.read_text()
        monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job, host=host: host)
        maintenance.cleanup_jobs(root, now=1000)
        assert path.read_text() == before
    assert not launches


def test_recovery_missing_launch_stays_ambiguous(tmp_path, monkeypatch):
    _, record, _, _ = _recoverable(tmp_path)
    record.with_suffix(".launch").unlink()
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    maintenance.cleanup_jobs(tmp_path, now=1000)
    job = json.loads(record.read_text())
    assert job["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert job["errorCode"] == "RECOVERY_LAUNCH_MISSING"
    assert job["resumedAfterInterruption"] == 0


def test_failed_thread_resume_never_starts_a_fresh_thread(tmp_path, monkeypatch):
    job_id, record, _, _ = _recoverable(tmp_path, thread_id="thread-1")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: None)
    maintenance.cleanup_jobs(tmp_path, now=1000)
    server = codex_exec_mcp.Server(job_dir=record.parent)
    job = json.loads(record.read_text())
    server.claim_generation = job["generation"]
    server.claim_token = job["ownershipToken"]
    calls = []
    monkeypatch.setattr(server, "_run_tool", lambda name, args, *_a, **_kw: (calls.append(name), {"isError": True})[1])
    server._job_worker(job_id, "codex-reply", json.loads(record.with_suffix(".launch").read_text())["args"])
    result = json.loads(record.read_text())
    assert calls == ["codex-reply"]
    assert result["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert result["errorCode"] == "RECOVERY_RESUME_FAILED"
