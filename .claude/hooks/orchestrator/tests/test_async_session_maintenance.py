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


def _shared_worktree_job(root, state, worktree):
    call = f"shared-worktree-{state}"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    record = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    record.write_text(json.dumps({"jobId": job_id, "call": call, "state": state,
        "finishedAt": 1000, "worktreePath": str(worktree),
        "repoRoot": str(root / "repo")}), encoding="utf-8")
    return record


def test_cleanup_removes_applied_job_worktree(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied")
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert not worktree.exists()


def test_cleanup_keeps_applied_job_worktree_shared_with_running_job(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied")
    _shared_worktree_job(tmp_path, "RUNNING", worktree)
    monkeypatch.setattr(codex_exec_mcp.Server, "recover_job", lambda *args, **kwargs: None)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)

    maintenance.cleanup_jobs(tmp_path, now=2000)

    assert worktree.is_dir()


def test_cleanup_keeps_applied_job_worktree_shared_with_unapplied_success(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied")
    _shared_worktree_job(tmp_path, "SUCCEEDED", worktree)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)

    maintenance.cleanup_jobs(tmp_path, now=2000)

    assert worktree.is_dir()


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


def test_cleanup_retires_applied_consumed_preparation(tmp_path):
    generation = _preparation(tmp_path, "a" * 64, revision=False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert not generation.exists()


def test_cleanup_preserves_unapplied_conflicted_interrupted_and_finalizing_preparations(tmp_path):
    generations = [
        _preparation(tmp_path, "1" * 64, apply_state=None),
        _preparation(tmp_path, "2" * 64, apply_state="conflict"),
        _preparation(tmp_path, "3" * 64, state="PREPARING"),
        _preparation(tmp_path, "4" * 64, state="BLOCKED"),
        _preparation(tmp_path, "5" * 64, index_stage="FINALIZING"),
    ]
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert all(generation.exists() for generation in generations)


def test_cleanup_retires_old_orphan_preparation_but_keeps_young_one(tmp_path):
    old = _preparation(tmp_path, "6" * 64, record=False, index_stage=None, stamp=1000)
    young = _preparation(tmp_path, "7" * 64, record=False, index_stage=None,
                         stamp=1000 + maintenance.RETENTION_SECONDS)
    maintenance.cleanup_jobs(tmp_path, now=1000 + maintenance.RETENTION_SECONDS + 1)
    assert not old.exists()
    assert young.exists()


def test_cleanup_retires_completed_preparations_oldest_first_over_count_bound(tmp_path, monkeypatch):
    generations = [_preparation(tmp_path, f"{index:064x}", stamp=1000 + index)
                   for index in range(maintenance.MAX_PREPARATION_GENERATIONS + 2)]
    removed = []
    remove = maintenance._remove_state_tree

    def recorded_remove(path, root):
        removed.append(path)
        return remove(path, root)

    monkeypatch.setattr(maintenance, "_remove_state_tree", recorded_remove)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert removed[:2] == generations[:2]
    budget = maintenance.MAX_PREPARATION_REMOVALS_PER_SCAN
    assert removed == generations[:budget]
    assert all(not generation.exists() for generation in generations[:budget])
    assert all(generation.exists() for generation in generations[budget:])


def test_daily_byte_check_warns_when_protected_preparation_exceeds_bound(tmp_path, monkeypatch):
    generation = _preparation(tmp_path, "9" * 64, state="PREPARING", stamp=1000)
    warnings = []
    monkeypatch.setattr(maintenance, "MAX_PREPARATION_BYTES", 0)
    monkeypatch.setattr(maintenance.state_retention, "_warning", lambda root: warnings.append(root))
    maintenance.cleanup_jobs(tmp_path, now=2000, check_ceiling=True)
    assert generation.exists()
    assert warnings == [tmp_path]


def test_cleanup_warns_when_protected_preparations_exceed_count_bound(tmp_path, monkeypatch):
    generations = [_preparation(tmp_path, f"{index:064x}", state="PREPARING", stamp=1000 + index)
                   for index in range(maintenance.MAX_PREPARATION_GENERATIONS + 1)]
    warnings = []
    monkeypatch.setattr(maintenance.state_retention, "_warning", lambda root: warnings.append(root))
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert all(generation.exists() for generation in generations)
    assert warnings == [tmp_path]


def test_cleanup_removes_readonly_preparation_without_following_symlink(tmp_path):
    generation = _preparation(tmp_path, "8" * 64)
    external = tmp_path.parent / (tmp_path.name + "-outside")
    external.mkdir()
    (external / "keep.txt").write_text("outside", encoding="utf-8")
    try:
        readonly = generation / "worker-tree" / "readonly.txt"
        readonly.parent.mkdir()
        readonly.write_text("copy", encoding="utf-8")
        readonly.chmod(0o444)
        link = generation / "worker-tree" / "external"
        try:
            link.symlink_to(external, target_is_directory=True)
        except (OSError, NotImplementedError):
            link = None
        maintenance.cleanup_jobs(tmp_path, now=2000)
        assert not generation.exists()
        assert (external / "keep.txt").read_text(encoding="utf-8") == "outside"
    finally:
        import shutil
        shutil.rmtree(external, ignore_errors=True)


def test_cleanup_worktree_count_evicts_oldest_eligible_first(tmp_path, monkeypatch):
    wt_root = tmp_path / "wt"
    protected = set()
    paths = []
    for index in range(maintenance.MAX_WORKTREES + 1):
        path = wt_root / f"{index:03}"
        path.mkdir(parents=True)
        os.utime(path, (1000 + index, 1000 + index))
        paths.append(path)
    removed = []
    monkeypatch.setattr(maintenance, "_remove_worktree",
                        lambda _root, _repo, path, *_args: (removed.append(path), path.rmdir(), True)[-1])
    maintenance._enforce_worktree_count(tmp_path, 1000 + maintenance.RETENTION_SECONDS + 10,
                                        protected, {}, {})
    assert removed == [paths[0]]


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


def test_cleanup_orphan_keeps_worktree_referenced_by_differently_named_job(tmp_path):
    _, repo, worktree, record = _registered_job_worktree(
        tmp_path, "SUCCEEDED", apply_state="conflict")
    renamed = worktree.with_name("independent-worktree-name")
    subprocess.run(["git", "-C", str(repo), "worktree", "move", str(worktree), str(renamed)], check=True)
    worktree = renamed
    job = json.loads(record.read_text(encoding="utf-8"))
    job["worktreePath"] = str(worktree)
    record.write_text(json.dumps(job), encoding="utf-8")
    os.utime(worktree, (1000, 1000))

    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)

    assert worktree.is_dir()


def test_cleanup_never_removes_worktree_outside_state_wt(tmp_path, monkeypatch):
    _, _, worktree, _ = _registered_job_worktree(tmp_path, "SUCCEEDED", apply_state="applied", outside=True)
    monkeypatch.setattr(codex_exec_mcp.Server, "_host_alive", lambda self, job: False)
    maintenance.cleanup_jobs(tmp_path, now=2000)
    assert worktree.is_dir()


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


def test_cleanup_reaches_recovery_and_expired_jobs_after_inert_prefix(tmp_path, monkeypatch):
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    for index in range(maintenance.MAX_JOB_RECORDS + 2):
        job_id = f"{index + 1:064x}"
        (jobs / f"{job_id}.json").write_text(json.dumps({
            "jobId": job_id, "state": "SUCCEEDED", "finishedAt": 1000 + 8 * 86400 - 1,
        }), encoding="utf-8")
    recovery_id = f"{maintenance.MAX_JOB_RECORDS + 3:064x}"
    expired_id = f"{maintenance.MAX_JOB_RECORDS + 4:064x}"
    (jobs / f"{recovery_id}.json").write_text(json.dumps({
        "jobId": recovery_id, "state": "RUNNING", "finishedAt": 1000,
    }), encoding="utf-8")
    (jobs / f"{expired_id}.json").write_text(json.dumps({
        "jobId": expired_id, "state": "SUCCEEDED", "finishedAt": 1000,
    }), encoding="utf-8")
    recovered = []
    monkeypatch.setattr(codex_exec_mcp.Server, "recover_job", lambda self, job_id, **kwargs: recovered.append(job_id))

    maintenance.cleanup_jobs(tmp_path, now=1000 + 8 * 86400)

    assert recovered == [recovery_id]
    assert not (jobs / f"{expired_id}.json").exists()


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
