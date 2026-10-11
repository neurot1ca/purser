import io
import hashlib
import json
import os
import re
from dataclasses import replace
from datetime import datetime, timedelta, timezone
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import threading
import time
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
sys.path.insert(0, str(Path(__file__).parents[4] / "scripts"))
import active_coder_state
import codex
import async_session_maintenance
import codex_exec_mcp
import checks
import project_profile
import context_io_telemetry
import hook
import json_transport
import ledger
import policy
import project_root
import reviewer as reviewer_module
import telemetry
import compaction_advisor
import telemetry_schema


CONFIG_PATH = Path(__file__).parents[1] / "config.json"


def remove_test_tree(path):
    def make_writable(function, target, _error):
        os.chmod(target, stat.S_IWRITE)
        function(target)

    shutil.rmtree(path, onerror=make_writable)


@pytest.fixture()
def config():
    return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))


@pytest.fixture()
def workspace():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "codex" / uuid.uuid4().hex
    repo = root / "repo"
    state = root / "state"
    repo.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10)
    subprocess.run(
        ["git", "config", "user.email", "tests@example.invalid"],
        cwd=repo,
        check=True,
        timeout=10,
    )
    subprocess.run(
        ["git", "config", "user.name", "Hook Tests"],
        cwd=repo,
        check=True,
        timeout=10,
    )
    for index in range(3):
        (repo / f"file{index}.py").write_text("value = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(
        ["git", "commit", "-qm", "initial"], cwd=repo, check=True, timeout=10
    )
    try:
        yield repo, state
    finally:
        remove_test_tree(root)
        try:
            root.parent.rmdir()
        except OSError:
            pass


def orch(
    route="normal",
    *,
    task="task-1",
    reason="new_task",
    basis=None,
    phase="implementation",
    scope="file0.py,file1.py,file2.py",
    include_reason=True,
    include_basis=True,
):
    if basis is None and include_basis:
        basis = "specific repository boundary" if route in {
            "normal",
            "consequential",
            "escalated",
        } else "na"
    fields = []
    for key, value, included in (
        ("task", task, task is not None),
        ("phase", phase, phase is not None),
        ("route", route, route is not None),
        ("reason", reason, include_reason),
        ("basis", basis, include_basis),
        ("scope", scope, scope is not None),
    ):
        if included:
            fields.append(f"{key}={value}")
    return "ORCH|" + "|".join(fields)


def event(tool_name=codex.CODEX_TOOL, *, prompt=None, tool_use_id="call-1", **fields):
    tool_input = {"prompt": prompt if prompt is not None else orch() + "\nDo the work."}
    if tool_name in codex.CODEX_TOOLS:
        tool_input["label"] = "test task"
    tool_input.update(fields)
    return {
        "session_id": "session-1",
        "hook_event_name": "PreToolUse",
        "tool_name": tool_name,
        "tool_use_id": tool_use_id,
        "tool_input": tool_input,
    }


def response_bundle(response, config):
    output = response["hookSpecificOutput"][config["post_tool_output_field"]]
    assert output[0]["type"] == "text"
    return json.loads(output[0]["text"])


START_TOOL = "mcp__plugin_purser_codex-main__codex_start"
RESULT_TOOL = "mcp__plugin_purser_codex-main__codex_result"
CANCEL_TOOL = "mcp__plugin_purser_codex-main__codex_cancel"


def async_started(workspace, config, *, prompt=None, state_name="RUNNING", tool_use_id="async-original"):
    repo, state = workspace
    start = event(START_TOOL, prompt=prompt, tool_use_id=tool_use_id)
    updated = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    codex.post_tool_use({**start, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"jobId": job_id, "state": "RUNNING"}}},
                        config=config, state_root=state, repo_root=repo)
    record = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    record.parent.mkdir(parents=True, exist_ok=True)
    record.write_text(json.dumps({"jobId": job_id, "call": updated["call"], "state": state_name,
                                  "acceptedAt": 100.0, "finishedAt": 112.5,
                                  "threadId": "async-thread" if state_name == "SUCCEEDED" else None}), encoding="utf-8")
    return start, job_id, record


def async_followup(name, job_id):
    call = event(name, tool_use_id=f"followup-{name}")
    call["tool_input"] = {"jobId": job_id}
    return call


def _board_job(state_root, name, status="RUNNING", **values):
    job_id = hashlib.sha256(name.encode()).hexdigest()
    call_key = hashlib.sha256((name + "-call").encode()).hexdigest()
    index = {"jobId": job_id, "call": name, "call_key": call_key,
             "project_key": "project", "session_id": "session"}
    job = {"jobId": job_id, "call": name, "state": status, "acceptedAt": time.time(),
           "label": name, "model": "gpt-6-sol", "effort": "medium", "route": "normal",
           "repoRoot": str(state_root.parent / "repo"),
           **values}
    if status in ("SUCCEEDED", "FAILED", "CANCELLED"):
        job.setdefault("boardDisplayCount", 0)
    (state_root / "async_jobs").mkdir(parents=True, exist_ok=True)
    (state_root / "codex_exec_mcp" / "jobs").mkdir(parents=True, exist_ok=True)
    (state_root / "async_jobs" / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
    (state_root / "codex_exec_mcp" / "jobs" / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
    return job_id, call_key


def test_async_result_waits_for_terminal_and_internal_error_fails_open(workspace, config):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config)
    result = async_followup(RESULT_TOOL, job_id)
    denied = codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo)
    assert "still running; wait for the completion signal" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert not list((state / "async_jobs").glob("*.claim*"))
    bypass = codex.internal_pre_error_response(result, RuntimeError("internal"), state_root=state)
    assert "additionalContext" in bypass["hookSpecificOutput"]


def test_async_duration_excludes_queue_wait_and_keeps_legacy_fallback():
    assert codex._async_duration({"acceptedAt": 100, "startedAt": 838, "finishedAt": 868}) == 30_000
    assert codex._async_duration({"acceptedAt": 100, "finishedAt": 105}) == 5_000


def _consumed_worktree_job(workspace, config, *, name="discard", applied=False):
    repo, state = workspace
    _, job_id, job_path = async_started(
        workspace, config, prompt=orch(task=name, scope="file0.py") + "\nWork.",
        state_name="SUCCEEDED", tool_use_id=f"{name}-start",
    )
    job = json.loads(job_path.read_text(encoding="utf-8"))
    job.update(state="SUCCEEDED", threadId="async-thread", worktreePath=str(repo), repoRoot=str(repo),
               applyState="applied" if applied else None, finishedAt=time.time())
    job_path.write_text(json.dumps(job), encoding="utf-8")
    index_path = state / "async_jobs" / f"{job_id}.json"
    index = json.loads(index_path.read_text(encoding="utf-8"))
    index.update(stage="CONSUMED", response={"status": "reviewed"})
    index_path.write_text(json.dumps(index), encoding="utf-8")
    leases = ledger.ScopeLeases(codex._project_state_root(state, index["project_key"]))
    assert leases.acquire(index["call_key"], name, ["file0.py"], job_id=job_id)["status"] == ledger.OK
    return repo, state, job_id, job_path, index, leases


def test_discard_consumed_job_releases_lease_and_promotes_overlapping_queue(workspace, config, monkeypatch):
    repo, state, job_id, job_path, index, leases = _consumed_worktree_job(workspace, config)
    queued_id = hashlib.sha256(b"queued-job").hexdigest()
    assert leases.acquire("queued-owner", "queued-task", ["file0.py"], job_id=queued_id,
                          queue_conflict=True)["code"] == "LEASE_QUEUED"
    queued_path = job_path.parent / f"{queued_id}.json"
    queued_path.write_text(json.dumps({"jobId": queued_id, "call": "queued-job", "state": "QUEUED",
                                       "leaseQueued": True, "acceptedAt": time.time(),
                                       "deadlineAt": time.time() + 30}), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: state)
    server = codex_exec_mcp.Server(host=True, job_dir=job_path.parent)
    launched = threading.Event()
    monkeypatch.setattr(codex, "bind_queued_baseline",
                        lambda *_args: leases.owner_state("queued-owner") == "ACTIVE")
    server._run_tool = lambda *_args, **_kwargs: (launched.set() or {"structuredContent": {
        "threadId": "queued-thread", "content": "started"}})
    worker = threading.Thread(target=server._job_worker, args=(queued_id, "codex", {}), daemon=True)
    worker.start()

    result = codex.pre_tool_use(async_followup(CANCEL_TOOL, job_id), config=config,
                                state_root=state, repo_root=repo)
    assert result == {}
    discarded = server._cancel_job({"jobId": job_id})["structuredContent"]

    assert discarded["discarded"] is True
    assert leases.owner_state(index["call_key"]) == "RELEASED"
    assert leases.owner_state("queued-owner") == "ACTIVE"
    assert launched.wait(timeout=5)
    worker.join(timeout=5)
    assert server._read_job(queued_id)["state"] == "SUCCEEDED"


def test_apply_after_discard_returns_typed_discarded_error(workspace, config, monkeypatch):
    repo, state, job_id, job_path, _, leases = _consumed_worktree_job(workspace, config)
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: state)
    server = codex_exec_mcp.Server(host=True, job_dir=job_path.parent)
    server._cancel_job({"jobId": job_id})

    result = server._apply_job({"jobId": job_id})["structuredContent"]

    assert result["errorCode"] == "DISCARDED"
    assert leases.owner_state(json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["call_key"]) == "RELEASED"


def test_cancel_on_applied_job_remains_consumed_denial(workspace, config):
    repo, state, job_id, _, _, _ = _consumed_worktree_job(workspace, config, applied=True)

    result = codex.pre_tool_use(async_followup(CANCEL_TOOL, job_id), config=config,
                                state_root=state, repo_root=repo)

    assert result["hookSpecificOutput"]["permissionDecisionReason"].startswith("ASYNC_ALREADY_CONSUMED:")


def test_discard_cancel_is_idempotent_and_leaves_foreign_owner_lease(workspace, config, monkeypatch):
    repo, state, job_id, job_path, index, leases = _consumed_worktree_job(workspace, config)
    assert leases.acquire("foreign-owner", "foreign-task", ["other.py"], job_id="f" * 64)["status"] == ledger.OK
    index["stage"] = "ACTIVE"
    (state / "async_jobs" / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.purser_state_root, "default_state_root", lambda: state)
    server = codex_exec_mcp.Server(host=True, job_dir=job_path.parent)
    first = server._cancel_job({"jobId": job_id})["structuredContent"]
    second = server._cancel_job({"jobId": job_id})["structuredContent"]
    followup = async_followup(CANCEL_TOOL, job_id)
    assert codex.pre_tool_use(followup, config=config, state_root=state, repo_root=repo) == {}
    response = codex.post_tool_use({**followup, "hook_event_name": "PostToolUse",
                                    "tool_response": {"structuredContent": second}},
                                   config=config, state_root=state, repo_root=repo)

    assert first["discarded"] is True and second["discarded"] is True
    assert response_bundle(response, config) == {"jobId": job_id, "state": "SUCCEEDED", "discarded": True}
    assert "systemMessage" not in response
    assert "codex_apply" not in json.dumps(response)
    replay = codex.post_tool_use({**followup, "hook_event_name": "PostToolUse",
                                  "tool_response": {"structuredContent": second}},
                                 config=config, state_root=state, repo_root=repo)
    assert response_bundle(replay, config) == {"jobId": job_id, "state": "SUCCEEDED", "discarded": True}
    assert leases.owner_state(index["call_key"]) == "RELEASED"
    assert leases.owner_state("foreign-owner") == "ACTIVE"


@pytest.mark.parametrize("response_shape", ("string", "blocks", "mapping"))
def test_async_result_finalizes_once_from_start_baseline(workspace, config, monkeypatch, response_shape):
    repo, state = workspace
    prompt = orch(scope="file0.py") + "|eta=4m\nTASK_CONTRACT: {\"objective\": \"Update file0\", \"criteria\": \"file0 is updated\"}"
    start, job_id, record = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "file1.py").write_text("concurrent = True\n", encoding="utf-8")
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    reviews = []
    def reviewer(**kwargs):
        reviews.append(kwargs)
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "provider": "agy"}
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    value = {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
             "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":[],"exit_codes":[]}'}
    response = {
        "string": json.dumps(value),
        "blocks": [{"type": "text", "text": json.dumps(value)}],
        "mapping": {"structuredContent": value},
    }[response_shape]
    post = {**result, "hook_event_name": "PostToolUse", "duration_ms": 1, "tool_response": response}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    bundle = response_bundle(first, config)
    assert bundle["thread_id"] == "async-thread" and bundle["concurrent_writer"] is True
    assert "file0.py" in bundle["changed_paths"] and "file1.py" in bundle["changed_paths"]
    assert len(reviews) == 1
    finished = [e["payload"] for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_FINISHED"]
    assert len(finished) == 1 and finished[0]["duration_ms"] == 12500 and finished[0]["eta_minutes"] == 4
    index = json.loads((state / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
    assert index["stage"] == "CONSUMED" and Path(index["bundle_path"]).is_file()
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer) == first
    assert len(reviews) == 1
    assert not codex._pending_path(state, start).exists()


def test_result_hook_does_not_call_readiness_checker():
    assert "_run_readiness_check" not in codex.post_tool_use.__code__.co_names
    assert "run_checker" not in codex.post_tool_use.__code__.co_names


def test_recovered_job_reviews_original_baseline_and_releases_lease_after_consumed(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, prompt=orch(scope="file0.py,file1.py") + "\nUpdate both files.")
    original_call = json.loads(record.read_text(encoding="utf-8"))["call"]
    (repo / "file0.py").write_text("before = True\n", encoding="utf-8")
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, pid=789, hostCreatedAt=456,
               hostContained=True, deadlineAt=time.time() + 3600, signalToken="secret")
    record.write_text(json.dumps(job), encoding="utf-8")
    record.with_suffix(".launch").write_text(json.dumps({"name": "codex", "args": {
        "prompt": "Update both files.", "timeoutSeconds": 30}, "codexBin": "codex"}), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    launches = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launches.append(args))
    async_session_maintenance.cleanup_jobs(state)
    recovered = json.loads(record.read_text(encoding="utf-8"))
    assert len(launches) == 1 and recovered["state"] == "QUEUED"
    assert recovered["call"] == original_call and recovered["generation"] == 2
    owner = codex._call_key(start)
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
    assert codex._pending_path(state, start).is_file()

    server = codex_exec_mcp.Server(job_dir=record.parent)
    server.claim_generation = recovered["generation"]
    server.claim_token = recovered["ownershipToken"]
    def fake_codex(*_args, **_kwargs):
        (repo / "file1.py").write_text("after = True\n", encoding="utf-8")
        return {"structuredContent": {"threadId": "resumed-thread", "content": "done"}}
    monkeypatch.setattr(server, "_run_tool", fake_codex)
    server._job_worker(job_id, "codex", json.loads(record.with_suffix(".launch").read_text())["args"])
    result = server._result_job({"jobId": job_id})
    assert result["structuredContent"]["state"] == "SUCCEEDED"
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
    followup = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(followup, config=config, state_root=state, repo_root=repo) == {}
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": result}
    reviews = []
    def reviewer(**kwargs):
        reviews.append(kwargs)
        assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "provider": "agy"}
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    bundle = response_bundle(first, config)
    assert set(bundle["changed_paths"]) == {"file0.py", "file1.py"}
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "RELEASED"
    assert len(reviews) == 1 and not codex._pending_path(state, start).exists()
    async_session_maintenance.cleanup_jobs(state)
    assert len(launches) == 1
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                               reviewer=lambda **kwargs: pytest.fail("second review")) == first


def test_failed_recovery_finalizes_through_ambiguous_path(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config)
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, pid=789, hostCreatedAt=456,
               hostContained=True, deadlineAt=time.time() + 3600)
    record.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    monkeypatch.setattr(codex_exec_mcp, "_posix_process_group_stopped", lambda _pid: True)
    async_session_maintenance.cleanup_jobs(state)
    assert json.loads(record.read_text())["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "ACTIVE"
    result = codex_exec_mcp.Server(job_dir=record.parent)._result_job({"jobId": job_id})
    followup = async_followup(RESULT_TOOL, job_id)
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": result}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(first, config)
    assert bundle["ambiguous_outcome"] is True and bundle["claude_review_mode"] == policy.FULL
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "RELEASED"


def test_finalizing_crash_before_bundle_survives_cleanup_and_recovers(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, state_name="SUCCEEDED")
    job = json.loads(record.read_text(encoding="utf-8"))
    job["finishedAt"] = time.time() - 8 * 86400
    record.write_text(json.dumps(job), encoding="utf-8")
    result = async_followup(RESULT_TOOL, job_id)
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    original_job = codex._async_job
    failed = []
    def crash_once(*args):
        if not failed:
            failed.append(True)
            raise OSError("crash after FINALIZING")
        return original_job(*args)
    monkeypatch.setattr(codex, "_async_job", crash_once)
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert response_bundle(first, config)["status"] == "ALREADY_CONSUMED_OR_INTERRUPTED"
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "FINALIZING"
    async_session_maintenance.cleanup_jobs(state)
    assert record.exists() and codex._pending_path(state, start).exists()
    second = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert response_bundle(second, config)["task_id"] == "task-1"
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"


@pytest.mark.parametrize("state_name", ("FAILED", "TIMED_OUT", "CANCELLED", "UNKNOWN_OR_IN_PROGRESS", "EXPIRED", "UNREADABLE"))
def test_async_terminal_failure_uses_ambiguous_path(workspace, config, state_name):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, state_name=state_name)
    if state_name == "UNREADABLE":
        record.write_text("{bad", encoding="utf-8")
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    post = {**result, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"jobId": job_id, "state": state_name, "errorCode": "CODEX_FAILED"}}}
    output = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert "FULL" in output["hookSpecificOutput"]["additionalContext"]
    assert not codex._pending_path(state, start).exists()
    assert any(e["event_type"] == "IMPLEMENTER_FAILED" for e in _all_telemetry_events(state))


def test_async_real_shaped_token_report_is_recorded(workspace, config, monkeypatch):
    repo, state = workspace
    path = "scripts/codex_token_report.py"
    prompt = orch(scope=f"file0.py,{path}") + "\nUpdate the token report."
    _, job_id, _ = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    capture = ledger.capture_baseline
    def dirty_token_report(*args, **kwargs):
        snapshot = capture(*args, **kwargs)
        snapshot["tracked_snapshots"][path] = {
            "status": [" M"], "index": [], "worktree": {"kind": "file"}}
        return snapshot
    monkeypatch.setattr(ledger, "capture_baseline", dirty_token_report)
    result = async_followup(RESULT_TOOL, job_id)
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
        "content": "Set tool_output_token_limit to 8000."}}}
    response = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(response, config)
    assert not any(f["code"].startswith("LEDGER_") for f in bundle["findings"])
    stored = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")
    assert stored["status"] == ledger.OK
    assert stored["record"]["last_outcome"]["raw_output_path"] == bundle["raw_output_path"]


def test_async_ledger_rejection_returns_typed_full_bundle(workspace, config, monkeypatch):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    monkeypatch.setattr(ledger.SessionLedger, "save", lambda self, record: {
        "status": "ERROR", "code": "LEDGER_RECORD_INVALID", "message": "rejected"})
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(first, config)
    assert bundle["claude_review_mode"] == "FULL"
    assert any(f["code"] == "LEDGER_RECORD_REJECTED" and "LEDGER_RECORD_INVALID" in f["message"]
               for f in bundle["findings"])
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo) == first


def _lease_state(workspace):
    repo, state = workspace
    return ledger.ScopeLeases(state / "projects" / project_root.project_key(repo.resolve()))


def _attribution_call(task, scope, suffix):
    prompt = (orch(task=task, scope=scope) + "\n"
              'TASK_CONTRACT: {"objective":"Edit the owned file","criteria":"Review only that file"}')
    return event(prompt=prompt, tool_use_id=f"attribution-{suffix}")


def _finish_attribution_call(call, workspace, config, observed):
    repo, state = workspace
    def review(**kwargs):
        observed.append(reviewer_module._delegated_diff(
            repo, kwargs["delta"], kwargs["classification"]))
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS", "findings": []}
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"threadId": call["tool_use_id"], "content":
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["focused"],"exit_codes":[0]}'}}}
    return response_bundle(codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=review), config)


def test_concurrent_leased_jobs_get_only_their_own_diff(workspace, config):
    repo, state = workspace
    calls = [_attribution_call(f"owned-{n}", f"file{n}.py", n) for n in (0, 1)]
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0,
                                                 "small_delta_max_files": 0})
    for call in calls:
        codex.pre_tool_use(call, config=review_config, state_root=state, repo_root=repo)
    for n in (0, 1):
        (repo / f"file{n}.py").write_text(f"value = {n + 2}\n", encoding="utf-8")
    for n, call in enumerate(calls):
        observed = []
        bundle = _finish_attribution_call(call, workspace, review_config, observed)
        assert observed and observed[0][0] == [f"file{n}.py"]
        assert bundle["changed_paths"] == [f"file{n}.py"]
        assert bundle["delta_files"] == 1
        assert "whole_tree_delta" not in bundle and "whole_tree_anomaly" not in bundle
        assert bundle["known_concurrent_scopes"] == {
            "count": 1, "task_ids": [f"owned-{1-n}"]}
        artifact = json.loads(Path(bundle["full_bundle_path"]).read_text(encoding="utf-8"))
        assert artifact["whole_tree_delta"]["changed_file_count"] == 2
        assert f"file{1-n}.py" in artifact["whole_tree_anomaly"]["paths"]
        assert artifact["known_concurrent_scopes"] == [
            {"path": f"file{1-n}.py", "task_id": f"owned-{1-n}"}]
        finished = [item for item in _all_telemetry_events(state)
                    if item["event_type"] == "IMPLEMENTER_FINISHED"
                    and item["task_id"] == f"owned-{n}"]
        assert finished[-1]["payload"]["known_concurrent_scopes"] == artifact["known_concurrent_scopes"]
        assert not {"OUT_OF_SCOPE_MODIFICATION", "UNKNOWN_CONCURRENT_WRITER"} & {
            finding["code"] for finding in bundle["findings"]}


def test_ambiguous_foreign_path_still_forces_full(workspace, config):
    repo, state = workspace
    (repo / "file1.py").write_text("value = 3\n", encoding="utf-8")
    calls = [_attribution_call("owned", "file0.py", "ambiguous-a"),
             _attribution_call("foreign", "file1.py", "ambiguous-b")]
    for call in calls:
        codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file1.py").write_text("value = 4\n", encoding="utf-8")
    bundle = _finish_attribution_call(calls[0], workspace, config, [])
    assert bundle["claude_review_mode"] == policy.FULL
    assert "known_concurrent_scopes" not in bundle


def test_async_lease_waits_for_finalize_or_expired_job_record(workspace, monkeypatch):
    _, state = workspace
    store = _lease_state(workspace)
    call = "async-call"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    assert store.acquire("async-owner", "async-task", ["file0.py"],
                         job_id=job_id, timeout_seconds=100)["status"] == ledger.OK
    path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    path.parent.mkdir(parents=True)
    job = {"jobId": job_id, "call": call, "state": "RUNNING"}
    path.write_text(json.dumps(job), encoding="utf-8")
    retention_after = 1000 + 100 + ledger.JOB_RETENTION_SECONDS
    monkeypatch.setattr(ledger.time, "time", lambda: retention_after + 1)
    assert store.acquire("next", "next-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    job.update(state="SUCCEEDED")
    path.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(ledger.time, "time", lambda: retention_after - 1)
    assert store.acquire("next", "next-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    monkeypatch.setattr(ledger.time, "time", lambda: retention_after + 1)
    assert store.acquire("next", "next-task", ["file0.py"])["status"] == ledger.OK
    assert store.release("next")["status"] == ledger.OK
    assert store.acquire("expired-owner", "expired-task", ["file0.py"],
                         job_id=job_id, timeout_seconds=100)["status"] == ledger.OK
    job.update(state="EXPIRED", errorCode="JOB_EXPIRED")
    path.write_text(json.dumps(job), encoding="utf-8")
    assert store.acquire("after-expired", "after-expired-task", ["file0.py"])["status"] == ledger.OK
    assert not [item for item in _all_telemetry_events(state)
                if item["event_type"] == "SCOPE_CONFLICT_OUTCOME"
                and item["payload"].get("action") == "discarded"]


def test_owner_state_promotes_waiter_after_blocker_retention_expires(workspace, monkeypatch):
    store = _lease_state(workspace)
    blocker_call = "completed blocker"
    blocker_job_id = hashlib.sha256(blocker_call.encode()).hexdigest()
    store.jobs_root.mkdir(parents=True)
    (store.jobs_root / f"{blocker_job_id}.json").write_text(json.dumps({
        "jobId": blocker_job_id, "call": blocker_call, "state": "SUCCEEDED",
    }), encoding="utf-8")
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    assert store.acquire("blocker", "blocking-task", ["file0.py"],
                         job_id=blocker_job_id, timeout_seconds=100)["status"] == ledger.OK
    cutoff = store._read()["owners"]["blocker"]["retention_after"]
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff - 10)
    assert store.acquire("waiter", "waiting-task", ["file0.py"],
                         job_id="b" * 64, timeout_seconds=100,
                         queue_conflict=True)["code"] == "LEASE_QUEUED"
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff)
    assert store.owner_state("waiter") == "ACTIVE"
    owners = store._read()["owners"]
    assert owners["blocker"]["state"] == "RELEASED"
    assert owners["waiter"]["acquired_at"] == cutoff
    expired = [event for event in _all_telemetry_events(workspace[1])
               if event["event_type"] == "SCOPE_CONFLICT_OUTCOME"
               and event["payload"].get("action") == "expired"]
    assert [event["payload"]["job_id"] for event in expired] == [blocker_job_id]


def test_async_start_rewrites_and_retries_same_call(workspace, config):
    repo, state = workspace
    prompt = orch(scope="file0.py", reason="repeated_failure") + "|eta=8m\nDo the work."
    call = event(START_TOOL, prompt=prompt, call="claude-value", model="wrong model",
                 timeoutSeconds=90, config={"model_reasoning_effort": "low"})
    first = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    updated = first["hookSpecificOutput"]["updatedInput"]
    assert updated["call"] != "claude-value"
    assert uuid.UUID(updated["call"])
    assert updated["mode"] == "new"
    assert updated["model"] == config["routes"]["normal"]["model"]
    assert updated["config"]["model_reasoning_effort"] == config["routes"]["normal"]["effort"]
    assert updated["config"]["tool_output_token_limit"] == 8000
    pending = codex._read_pending(state, call)
    assert pending["async_call"] == updated["call"] and pending["job_id"] is None
    assert pending["baseline"]["status"] == ledger.OK
    lease = _lease_state(workspace)._read()["owners"][codex._call_key(call)]
    assert lease["retention_after"] == lease["acquired_at"] + 90 + ledger.JOB_RETENTION_SECONDS
    assert (pending["reason"], pending["eta_minutes"]) == ("repeated_failure", 8)
    assert codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"] == updated
    assert len([e for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_STARTED"]) == 1


@pytest.mark.parametrize("fail_store", (False, True))
@pytest.mark.parametrize("error_shape", ("string", "blocks", "mapping"))
def test_async_start_failure_uses_ambiguous_path_and_cancels_job(workspace, config, monkeypatch, fail_store, error_shape):
    repo, state = workspace
    call = event(START_TOOL)
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    cancelled = []
    bridge_job = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    bridge_job.parent.mkdir(parents=True)
    bridge_job.write_text(json.dumps({"jobId": job_id, "call": updated["call"]}), encoding="utf-8")
    def confirm_cancel(root, call_uuid, job):
        cancelled.append(job)
        bridge_job.write_text(json.dumps({"jobId": job_id, "call": updated["call"],
                                          "state": "CANCELLED"}), encoding="utf-8")
        return True
    monkeypatch.setattr(codex, "_cancel_start_job", confirm_cancel)
    if fail_store:
        write = codex._atomic_json_write
        def fail_job_write(path, value):
            if path == codex._pending_path(state, call) and value.get("job_id"):
                raise OSError("job association could not be stored")
            return write(path, value)
        monkeypatch.setattr(codex, "_atomic_json_write", fail_job_write)
    value = {"jobId": job_id, "state": "RUNNING",
             **({} if fail_store else {"errorCode": "CODEX_FAILED", "isError": True})}
    response = {
        "string": json.dumps(value),
        "blocks": [{"type": "text", "text": json.dumps(value)}],
        "mapping": {"structuredContent": value, "isError": not fail_store},
    }[error_shape]
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": response}
    result = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert "FULL" in result["hookSpecificOutput"]["additionalContext"]
    assert cancelled == [job_id]
    assert not list((state / "pending").glob("*.json"))
    assert "ASYNC_CALL_ALREADY_ACCEPTED" in codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]
    assert any(e["event_type"] == "IMPLEMENTER_FAILED" for e in _all_telemetry_events(state))


def test_post_tool_quota_message_failure_preserves_replacement(workspace, config, monkeypatch):
    _, state = workspace
    bundle = codex._base_bundle()
    bundle["codex_quota"] = {"5h_used_pct": 1, "5h_resets_at": "2026-09-25T08:57:11Z"}
    call = event(tool_use_id="quota-message-failure")
    monkeypatch.setattr(codex, "_codex_quota_system_message", lambda _quota: (_ for _ in ()).throw(ValueError()))
    result = codex._finalize_replacement(bundle, config, state, call)
    assert "systemMessage" not in result
    compact, _ = codex._compact_bundle(bundle, config["bundle_non_report_max_bytes"])
    compact["codex_quota"] = bundle["codex_quota"]
    compact["implementer_report"] = ""[: codex._report_limit(config, bundle.get("phase"))]
    assert result["hookSpecificOutput"] == codex._replacement(compact, config)["hookSpecificOutput"]


def _quota_snapshot(used_5h=None, used_7d=None):
    value = {}
    if used_5h is not None:
        value.update({"5h_used_pct": used_5h, "5h_resets_at": "2026-09-24T15:00:00Z"})
    if used_7d is not None:
        value.update({"7d_used_pct": used_7d, "7d_resets_at": "2026-09-30T12:00:00Z"})
    return value or None


def test_low_codex_quota_does_not_check_codex_reply(workspace, config, monkeypatch):
    _, state = workspace
    calls = []
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: calls.append(True) or _quota_snapshot(92), raising=False)
    call = event(tool_name=codex.CODEX_REPLY_TOOL, prompt=orch(scope="file0.py") + "\nContinue.",
                 tool_use_id="low-quota-reply", threadId="existing-thread")
    codex.pre_tool_use(call, config=config, state_root=state)
    assert calls == []


def hook_check_call(workspace, config, monkeypatch, *, worker_status="UNKNOWN", check_result=None,
                    declared=True, reviewer=None, before_post=None):
    repo, state = workspace
    original_load = project_profile.load_profile
    monkeypatch.setattr(project_profile, "load_profile", lambda root: replace(
        original_load(root), force_full_review=False,
        checks=(project_profile.Check("focused", ("python", "-V")),) if declared else (),
    ))
    monkeypatch.setattr(checks, "run_checks", check_result if callable(check_result)
                        else lambda *args, **kwargs: check_result)
    prompt = (orch(scope="file0.py") +
              "\n## Objective\nUpdate file0.\n## Acceptance criteria\n- file0 changes.")
    call = event(prompt=prompt, tool_use_id="hook-check-" + uuid.uuid4().hex)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    if before_post:
        before_post(repo, state, call)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    content = ("No worker validation" if worker_status == "UNKNOWN" else
               'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}')
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": [{"type": "text", "text": json.dumps({"threadId": "thread-check", "content": content})}]}
    return response_bundle(codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo,
        reviewer=reviewer or (lambda **kwargs: {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                "validation": "PASS", "findings": []}),
    ), config)


def check_result(status, *, tail="", side_effect_paths=None):
    return {"status": status, "results": [{"name": "focused", "status": status,
            "exit_code": 1 if status == "FAIL" else 0 if status == "PASS" else None,
            "duration_ms": 1, "tail": tail}],
            "side_effect_paths": [] if side_effect_paths is None else side_effect_paths}


def test_hook_check_exception_still_returns_bundle(workspace, config, monkeypatch):
    def raise_check(*args, **kwargs):
        raise RuntimeError("runner broke")
    bundle = hook_check_call(workspace, config, monkeypatch, worker_status="PASS",
                             check_result=raise_check)
    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["hook_checks"] == {"status": "UNKNOWN", "results": []}
    assert [f["code"] for f in bundle["findings"]].count("HOOK_CHECK_UNKNOWN") == 1
    assert any("RuntimeError" in f["message"] for f in bundle["findings"])


def test_no_declared_hook_checks_preserves_worker_decision(workspace, config, monkeypatch):
    bundle = hook_check_call(workspace, config, monkeypatch, declared=False, worker_status="UNKNOWN",
                             check_result=lambda *args, **kwargs: pytest.fail("checks must not run"))
    assert bundle["hook_checks"] is None
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "WORKER_VALIDATION_UNVERIFIED" for f in bundle["findings"])


def test_hook_checks_overlap_reviewer(workspace, config, monkeypatch):
    intervals = {}
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    def slow_check(*args, **kwargs):
        intervals["check_start"] = time.monotonic()
        time.sleep(0.5)
        intervals["check_end"] = time.monotonic()
        return check_result("PASS")
    def slow_reviewer(**kwargs):
        intervals["reviewer_start"] = time.monotonic()
        time.sleep(0.5)
        intervals["reviewer_end"] = time.monotonic()
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS", "findings": []}
    bundle = hook_check_call(workspace, review_config, monkeypatch, check_result=slow_check,
                             reviewer=slow_reviewer)
    assert bundle["hook_checks"]["status"] == "PASS"
    assert intervals["check_start"] < intervals["reviewer_end"]
    assert intervals["reviewer_start"] < intervals["check_end"]


def test_pending_cleanup_failure_does_not_change_pretool_output(
    workspace, config, monkeypatch
):
    repo, state = workspace
    call = event(tool_use_id="cleanup-failure")
    expected = codex.pre_tool_use(
        call, config=config, state_root=state / "control", repo_root=repo
    )

    def fail_cleanup(*_args, **_kwargs):
        raise OSError("cleanup unavailable")

    monkeypatch.setattr(codex, "_cleanup_stale_pending", fail_cleanup)
    actual = codex.pre_tool_use(
        call, config=config, state_root=state / "failure", repo_root=repo
    )
    assert actual == expected


def test_context_association_failure_does_not_change_phase_1a_pretool_output(
    workspace, config, monkeypatch
):
    repo, state = workspace
    call = event(tool_use_id="context-association-failure")
    expected = codex.pre_tool_use(
        call, config=config, state_root=state / "control", repo_root=repo
    )

    monkeypatch.setattr(
        context_io_telemetry,
        "associate_turn",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("telemetry unavailable")),
    )
    actual = codex.pre_tool_use(
        call, config=config, state_root=state / "failure", repo_root=repo
    )
    assert actual == expected


def _all_telemetry_events(state):
    events = []
    for path in (state / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return events


def test_entry_point_writes_exactly_one_json_object(monkeypatch):
    stdin = io.StringIO(
        json.dumps(
            {
                "hook_event_name": "PreToolUse",
                "tool_name": "unrelated",
                "tool_input": {},
            }
        )
    )
    stdout = io.StringIO()
    monkeypatch.setattr(hook.sys, "stdin", stdin)
    monkeypatch.setattr(hook.sys, "stdout", stdout)

    assert hook.main() == 0
    assert stdout.getvalue() == "{}\n"


def test_entry_point_pretool_crash_uses_visible_internal_bypass(monkeypatch):
    payload = event()
    stdin = io.StringIO(json.dumps(payload))
    stdout = io.StringIO()

    def crash(_event):
        raise RuntimeError("dispatch failed")

    def visible_bypass(_event, error):
        return {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "additionalContext": f"routing NOT enforced: {error}",
            }
        }

    monkeypatch.setattr(hook.codex, "pre_tool_use", crash)
    monkeypatch.setattr(hook.codex, "internal_pre_error_response", visible_bypass)
    monkeypatch.setattr(hook.sys, "stdin", stdin)
    monkeypatch.setattr(hook.sys, "stdout", stdout)

    assert hook.main() == 0
    result = json.loads(stdout.getvalue())
    assert "NOT enforced" in result["hookSpecificOutput"]["additionalContext"]


_HIDDEN_CAP_TEST_EVIDENCE, _HIDDEN_CAP_TEST_EVIDENCE_FAILED = codex_exec_mcp._codex_test_evidence([
    {"type": "item.completed", "item": {
        "type": "command_execution", "command": f"pytest test-{index}",
        "exit_code": 1 if index == 0 else 0, "aggregated_output": "failed" if index == 0 else "passed",
    }}
    for index in range(7)
])


