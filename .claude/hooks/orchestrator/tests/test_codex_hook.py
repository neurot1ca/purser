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


def test_post_tool_use_ungrounded_contract_forces_full_review_even_on_audit(workspace, config):
    repo, state = workspace
    # Default event() prompt has no Task Objective / Acceptance Criteria
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({
                    "threadId": "thread-1",
                    "content": "updated\nVALIDATION: {\"source\": \"WORKER_REPORTED\", \"status\": \"PASS\", \"commands\": [\"pytest\"], \"exit_codes\": [0]}",
                }),
            }
        ],
    }

    def reviewer(**_kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=reviewer
    )
    bundle = response_bundle(raw, config)

    # Must be forced to FULL because task contract is not grounded!
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "TASK_CONTRACT_NOT_GROUNDED" for f in bundle["findings"])
    assert not any(f["code"] == "WORKER_VALIDATION_UNVERIFIED" for f in bundle["findings"])


def test_post_tool_use_micro_repair_triggers_on_changes_required(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py,file1.py,file2.py\n"
        "Task Objective: Apply changes to files 0 to 2\n"
        "Acceptance Criteria: All files updated and clean\n"
    )
    call = event(prompt=prompt)
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({
                    "threadId": "thread-1",
                    "content": "updated\nVALIDATION: {\"source\": \"WORKER_REPORTED\", \"status\": \"PASS\", \"commands\": [\"pytest -q\"], \"exit_codes\": [0]}",
                }),
            }
        ],
    }

    reviewer_invocations = []
    repair_invocations = []

    def reviewer(**kwargs):
        reviewer_invocations.append(kwargs)
        if len(reviewer_invocations) == 1:
            return {
                "result": policy.CHANGES_REQUIRED,
                "validation": "FAIL",
                "findings": [{"code": "TYPO_FOUND", "message": "fix typo"}],
            }
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    def mock_micro_repair(thread_id, findings, repo_root, scope=None, timeout_seconds=60, **_kwargs):
        repair_invocations.append({"thread_id": thread_id, "findings": findings, "scope": scope, "timeout": timeout_seconds})
        return {"repaired": True, "output": "repaired in place"}

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair,
    )
    bundle = response_bundle(raw, repair_config)

    assert len(repair_invocations) == 1
    assert repair_invocations[0]["thread_id"] == "thread-1"
    assert repair_invocations[0]["scope"] == ["file0.py", "file1.py", "file2.py"]
    assert len(reviewer_invocations) == 2
    assert reviewer_invocations[1]["task_objective"] == reviewer_invocations[0]["task_objective"]
    assert reviewer_invocations[1]["acceptance_criteria"] == reviewer_invocations[0]["acceptance_criteria"]
    assert reviewer_invocations[1]["validation_summary"] == reviewer_invocations[0]["validation_summary"]
    assert bundle["micro_repair_cleared_review"] is True
    assert bundle["micro_repair"]["status"] == "SUCCEEDED"
    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["audit_receipt"]["verdict"] == "READY_FOR_ONE_TURN_ACCEPT"


def test_micro_repair_hides_windows_console(monkeypatch):
    calls = []
    monkeypatch.setattr(codex, "os", type("OS", (), {"name": "nt"})())
    monkeypatch.setattr(codex.subprocess, "run", lambda *args, **kwargs: calls.append(kwargs)
                        or subprocess.CompletedProcess(args[0], 0, "ok", ""))

    codex._attempt_in_hook_micro_repair("thread", [{"code": "F", "message": "finding"}], Path.cwd())

    assert calls[0]["creationflags"] & 0x08000000


def test_post_tool_use_micro_repair_stale_delta_forces_full_review(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    reviewer_invocations = []

    def reviewer(**kwargs):
        reviewer_invocations.append(kwargs)
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "TYPO", "message": "fix"}],
        }

    def mock_micro_repair(thread_id, findings, repo_root, **kwargs):
        return {"repaired": True, "output": "repaired"}

    # Mock compute_delta on the second call (post-repair recompute) to fail
    call_count = [0]
    original_compute_delta = ledger.compute_delta

    def mock_compute_delta(*args, **kwargs):
        call_count[0] += 1
        if call_count[0] == 2:
            return {"status": ledger.CORRUPT, "code": "CORRUPT_DELTA", "message": "failed"}
        return original_compute_delta(*args, **kwargs)

    monkeypatch.setattr(ledger, "compute_delta", mock_compute_delta)

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair,
    )
    bundle = response_bundle(raw, repair_config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "CORRUPT_DELTA" for f in bundle["findings"])


def test_post_tool_use_micro_repair_failure_reports_failed_status(workspace, config):
    repo, state = workspace
    call = event()
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    def mock_micro_repair_fail(thread_id, findings, repo_root, **kwargs):
        return {"repaired": False, "error": "TimeoutExpired: 60s"}

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair_fail,
    )
    bundle = response_bundle(raw, repair_config)

    assert bundle["micro_repair_cleared_review"] is False
    assert bundle["micro_repair"]["status"] == "TIMED_OUT"
    assert bundle["claude_review_mode"] == policy.FULL


def test_post_tool_use_micro_repair_timeout_recomputes_partial_delta(workspace, config):
    repo, state = workspace
    call = event()
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    def mock_micro_repair_timeout(thread_id, findings, repo_root, **kwargs):
        # Simulate partial edit before timeout
        (repo / "file0.py").write_text("value = 999\n" * 25, encoding="utf-8")
        return {"repaired": False, "error": "TimeoutExpired: 60s"}

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair_timeout,
    )
    bundle = response_bundle(raw, repair_config)

    assert bundle["micro_repair_cleared_review"] is False
    assert bundle["micro_repair"]["status"] == "TIMED_OUT"
    assert bundle["claude_review_mode"] == policy.FULL
    assert "file0.py" in bundle["changed_paths"]
    assert bundle["delta_lines"] is not None


def test_post_tool_use_micro_repair_out_of_scope_escapes_forces_full_review(workspace, config):
    repo, state = workspace
    call = event()
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    def mock_micro_repair_out_of_scope(thread_id, findings, repo_root, **kwargs):
        # Micro repair modifies a file outside declared scope ["file0.py", "file1.py", "file2.py"]
        (repo / "unscoped_escape.py").write_text("evil = 1\n", encoding="utf-8")
        return {"repaired": True, "output": "repaired"}

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair_out_of_scope,
    )
    bundle = response_bundle(raw, repair_config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "OUT_OF_SCOPE_MODIFICATION" for f in bundle["findings"])


def test_post_tool_use_micro_repair_out_of_scope_inspection_failure_forces_full_review(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()
    repair_config = dict(config, micro_repair_enabled=True)
    codex.pre_tool_use(call, config=repair_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    def mock_micro_repair(thread_id, findings, repo_root, **kwargs):
        return {"repaired": True, "output": "repaired"}

    # Mock detect_out_of_scope_modifications to fail (e.g. git error)
    def mock_detect_failure(*args, **kwargs):
        return {"status": ledger.UNKNOWN, "code": "OUT_OF_SCOPE_INSPECTION_FAILED", "message": "inspection failed"}

    monkeypatch.setattr(ledger, "detect_out_of_scope_modifications", mock_detect_failure)

    raw = codex.post_tool_use(
        post,
        config=repair_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair,
    )
    bundle = response_bundle(raw, repair_config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "OUT_OF_SCOPE_INSPECTION_FAILED" for f in bundle["findings"])


def test_post_tool_use_micro_repair_honors_configured_total_budget(workspace, config):
    repo, state = workspace
    call = event()
    tight_config = dict(config, micro_repair_enabled=True, max_total_hook_budget_seconds=10.0)
    codex.pre_tool_use(call, config=tight_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    raw = codex.post_tool_use(
        post,
        config=tight_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
    )
    bundle = response_bundle(raw, tight_config)

    assert bundle["micro_repair"] is not None
    assert bundle["micro_repair"]["attempted"] is False
    assert bundle["micro_repair"]["status"] == "SKIPPED_BUDGET"


def test_post_tool_use_ledger_write_failure_clears_audit_receipt(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    def mock_save_failure(self, record):
        return {"status": ledger.CORRUPT, "code": "LEDGER_WRITE_FAILED", "message": "disk full"}

    monkeypatch.setattr(ledger.SessionLedger, "save", mock_save_failure)

    raw = codex.post_tool_use(
        post,
        config=config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
    )
    bundle = response_bundle(raw, config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "LEDGER_WRITE_FAILED" for f in bundle["findings"])


def test_post_tool_use_micro_repair_disabled_in_config_skips_repair(workspace, config):
    repo, state = workspace
    call = event()
    disabled_config = dict(config, micro_repair_enabled=False)
    codex.pre_tool_use(call, config=disabled_config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "updated"}),
            }
        ],
    }

    repair_called = []

    def reviewer(**kwargs):
        return {
            "result": policy.CHANGES_REQUIRED,
            "validation": "FAIL",
            "findings": [{"code": "FAIL", "message": "bug"}],
        }

    def mock_micro_repair(thread_id, findings, repo_root, **kwargs):
        repair_called.append(True)
        return {"repaired": True, "output": "repaired"}

    raw = codex.post_tool_use(
        post,
        config=disabled_config,
        state_root=state,
        repo_root=repo,
        reviewer=reviewer,
        micro_repair=mock_micro_repair,
    )
    bundle = response_bundle(raw, disabled_config)

    assert len(repair_called) == 0
    assert bundle["micro_repair"] is None
    assert bundle["claude_review_mode"] == policy.FULL


def test_task_contract_parsing_and_grounding_flag(workspace, config):
    repo, state = workspace

    # Case A: Explicit JSON TASK_CONTRACT
    prompt_json = (
        "ORCH|task=task-a|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\n"
        'TASK_CONTRACT: {"objective": "Fix deadline logic", "criteria": "Zero latency drift"}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json, "task-a", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Fix deadline logic"
    assert crit == "Zero latency drift"

    # Case B: Multiline headers
    prompt_multiline = (
        "ORCH|task=task-b|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\n"
        "Task Objective:\nRefactor connection pool\n"
        "Acceptance Criteria:\nPreserve retry backoff"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_multiline, "task-b", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Refactor connection pool"
    assert crit == "Preserve retry backoff"

    # Case C: Generic prompt without explicit contract
    prompt_generic = "ORCH|task=task-c|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\nJust do the work"
    obj, crit, grounded = codex._parse_task_contract(prompt_generic, "task-c", "implementation", ["file.py"])
    assert grounded is False
    assert "Task task-c" in obj
    assert "file.py" in crit


def test_worker_validation_failure_mechanically_forces_full_claude_review(workspace, config):
    repo, state = workspace
    for i in range(3):
        (repo / f"f{i}.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    prompt = (
        "ORCH|task=task-val|phase=implementation|route=normal|basis=test fixture design boundary|scope=f0.py,f1.py,f2.py\n"
        "Task Objective: Implement f0-f2 updates\n"
        "Acceptance Criteria: All f0-f2 files changed\n"
    )
    call = event(prompt=prompt)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    for i in range(3):
        (repo / f"f{i}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    report = (
        "Execution completed with failure.\n"
        'VALIDATION: {"source": "WORKER_REPORTED", "status": "FAIL", "summary": "Integration test failed", "commands": ["pytest -q tests/test_integration.py"], "exit_codes": [1], "artifacts": []}'
    )
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": report}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "UNKNOWN",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=reviewer
    )
    bundle = response_bundle(raw, config)

    # Mechanical invariant: worker validation failure MUST force FULL mode even if reviewer accepted!
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "WORKER_VALIDATION_FAILED" for f in bundle["findings"])
    assert bundle["worker_validation"]["status"] == "FAIL"


def test_reviewer_internal_typeerror_never_retried(workspace, config):
    repo, state = workspace
    for i in range(3):
        (repo / f"g{i}.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    call = event(
        prompt="ORCH|task=task-err|phase=implementation|route=normal|basis=test fixture design boundary|scope=g0.py,g1.py,g2.py"
    )
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    for i in range(3):
        (repo / f"g{i}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "Validation: PASS"}),
            }
        ],
    }
    calls = []

    def failing_reviewer(**kwargs):
        calls.append(kwargs)
        raise TypeError("simulated internal adapter error")

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=failing_reviewer
    )
    bundle = response_bundle(raw, config)

    assert len(calls) == 1
    assert bundle["claude_review_mode"] == policy.FULL


def test_worker_validation_parser_avoids_substring_false_positives():
    expected_keys = {"source", "status", "summary", "commands", "exit_codes", "artifacts"}

    # 1. Serbest metin "tests never passed" => UNKNOWN
    res1 = codex._parse_worker_validation("Validation: tests never passed")
    assert res1["status"] == "UNKNOWN"
    assert res1["summary"] == "Validation: tests never passed"
    assert set(res1.keys()) == expected_keys

    # 2. Geçerli structured PASS => PASS
    good_pass = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    res2 = codex._parse_worker_validation(good_pass)
    assert res2["status"] == "PASS"
    assert res2["commands"] == ["pytest"]
    assert res2["exit_codes"] == [0]
    assert set(res2.keys()) == expected_keys

    # 3. Geçerli structured FAIL => FAIL
    good_fail = 'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[1]}'
    res3 = codex._parse_worker_validation(good_fail)
    assert res3["status"] == "FAIL"
    assert res3["commands"] == ["pytest"]
    assert res3["exit_codes"] == [1]
    assert set(res3.keys()) == expected_keys

    # 4. source="INDEPENDENT_VERIFIED" => UNKNOWN (forged provenance rejected)
    bad_source = 'VALIDATION: {"source":"INDEPENDENT_VERIFIED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    res4 = codex._parse_worker_validation(bad_source)
    assert res4["status"] == "UNKNOWN"
    assert set(res4.keys()) == expected_keys

    # 5. commands=["   "] => UNKNOWN (empty string after trim)
    blank_cmd = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["   "],"exit_codes":[0]}'
    res5 = codex._parse_worker_validation(blank_cmd)
    assert res5["status"] == "UNKNOWN"
    assert set(res5.keys()) == expected_keys

    # 6. commands string => UNKNOWN (wrong type)
    str_cmd = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":"pytest","exit_codes":[0]}'
    res6 = codex._parse_worker_validation(str_cmd)
    assert res6["status"] == "UNKNOWN"
    assert set(res6.keys()) == expected_keys

    # 7. exit_codes içinde bool => UNKNOWN (bool is not accepted as int)
    bool_code = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[true]}'
    res7 = codex._parse_worker_validation(bool_code)
    assert res7["status"] == "UNKNOWN"
    assert set(res7.keys()) == expected_keys

    # 8. PASS ve nonzero exit code => UNKNOWN
    pass_nonzero = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[1]}'
    res8 = codex._parse_worker_validation(pass_nonzero)
    assert res8["status"] == "UNKNOWN"
    assert set(res8.keys()) == expected_keys

    # 9. FAIL ve bütün exit code değerleri 0 => UNKNOWN
    fail_zero = 'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[0]}'
    res9 = codex._parse_worker_validation(fail_zero)
    assert res9["status"] == "UNKNOWN"
    assert set(res9.keys()) == expected_keys

    # 10. Valid PASS + ikinci malformed FAIL kaydı => UNKNOWN (multiple records)
    multi_malformed = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}\n'
        'VALIDATION: malformed json'
    )
    res10 = codex._parse_worker_validation(multi_malformed)
    assert res10["status"] == "UNKNOWN"
    assert res10["summary"] == "Multiple conflicting VALIDATION records were reported."
    assert set(res10.keys()) == expected_keys

    # 11. İki valid PASS kaydı => UNKNOWN (multiple records)
    multi_valid = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}\n'
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    )
    res11 = codex._parse_worker_validation(multi_valid)
    assert res11["status"] == "UNKNOWN"
    assert res11["summary"] == "Multiple conflicting VALIDATION records were reported."
    assert set(res11.keys()) == expected_keys

    # 12. Boyut sınırını aşan summary => UNKNOWN
    big_summary = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"' + ('x' * 1300) + '","commands":["pytest"],"exit_codes":[0]}'
    res12 = codex._parse_worker_validation(big_summary)
    assert res12["status"] == "UNKNOWN"
    assert set(res12.keys()) == expected_keys
    assert len(json_transport.dumps(res12).encode("utf-8")) <= codex.IMPLEMENTER_REPORT_LIMIT


def test_worker_validation_ignores_prose_heading_before_structured_record():
    report = (
        "Validation:\n"
        "- Ran the focused checks.\n"
        "- No issues found.\n"
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest"],"exit_codes":[0]}\n'
    )

    result = codex._parse_worker_validation(report)

    assert result["status"] == "PASS"
    assert result["commands"] == ["pytest"]
    assert result["exit_codes"] == [0]


def test_worker_validation_rejects_two_genuine_records_as_multiple():
    record = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest"],"exit_codes":[0]}'
    )
    result = codex._parse_worker_validation(f"{record}\n{record}")

    assert result["status"] == "UNKNOWN"
    assert result["summary"] == "Multiple conflicting VALIDATION records were reported."


def test_worker_validation_ignores_mixed_case_prose_line():
    result = codex._parse_worker_validation("Validation: npm test passed")

    assert result["status"] == "UNKNOWN"
    assert result["summary"] == "Validation: npm test passed"


@pytest.mark.parametrize(
    ("line", "reason"),
    [
        ("VALIDATION: {not json", "invalid_json"),
        ("VALIDATION: []", "not_object"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0],"zeta":2,"alpha":1}',
            "unknown_keys=alpha,zeta",
        ),
        (
            'VALIDATION: {"source":"OTHER","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "bad_source",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"MAYBE",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "bad_status",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"summary":"   ","commands":["pytest"],"exit_codes":[0]}',
            "bad_summary",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":"pytest","exit_codes":[0]}',
            "bad_commands",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0],"artifacts":"report.txt"}',
            "bad_artifacts",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[true]}',
            "bad_exit_codes",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[]}',
            "commands_exit_codes_mismatch",
        ),
        ('VALIDATION: {"source":"WORKER_REPORTED","status":"PASS"}', "pass_without_commands"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[1]}',
            "pass_with_nonzero_exit",
        ),
        ('VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL"}', "fail_without_commands"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "fail_without_nonzero_exit",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"'
            + ("x" * 1300)
            + '","commands":["pytest"],"exit_codes":[0]}',
            "record_too_large",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "serialization_error",
        ),
    ],
)
def test_worker_validation_rejection_reason_is_first_and_preserves_line(line, reason, monkeypatch):
    if reason == "serialization_error":
        original_dumps = codex.json_transport.dumps

        def fail_pass_serialization(value, *args, **kwargs):
            if isinstance(value, dict) and value.get("status") == "PASS":
                raise ValueError("simulated serialization failure")
            return original_dumps(value, *args, **kwargs)

        monkeypatch.setattr(codex.json_transport, "dumps", fail_pass_serialization)

    result = codex._parse_worker_validation(line)

    assert result["status"] == "UNKNOWN"
    assert result["summary"].startswith(f"{reason}: ")
    if reason == "record_too_large":
        assert result["summary"].startswith(f"{reason}: {line[:40]}")
    else:
        assert result["summary"] == f"{reason}: {line}"


def test_worker_validation_accepted_record_is_unchanged():
    line = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"summary":"focused validation passed","commands":["pytest -q"],'
        '"exit_codes":[0],"artifacts":["report.txt"]}'
    )

    assert codex._parse_worker_validation(line) == {
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "summary": "focused validation passed",
        "commands": ["pytest -q"],
        "exit_codes": [0],
        "artifacts": ["report.txt"],
    }


def test_worker_validation_accepts_backticked_record():
    line = (
        '`VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"summary":"focused validation passed","commands":["pytest -q"],'
        '"exit_codes":[0],"artifacts":["report.txt"]}`'
    )

    assert codex._parse_worker_validation(line)["status"] == "PASS"


def test_worker_validation_backticked_and_plain_records_count_as_multiple():
    record = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest -q"],"exit_codes":[0]}'
    )

    result = codex._parse_worker_validation(f'`{record}`\n{record}')

    assert result["status"] == "UNKNOWN"
    assert result["summary"].startswith("Multiple conflicting VALIDATION records")


def test_worker_validation_does_not_unwrap_only_leading_backtick():
    record = (
        '`VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest -q"],"exit_codes":[0]}'
    )

    assert codex._parse_worker_validation(record)["status"] == "UNKNOWN"


def test_worker_validation_normalized_byte_limit_invariants():
    expected_keys = {"source", "status", "summary", "commands", "exit_codes", "artifacts"}

    # 1. 1200 emoji içeren free-text validation
    emojis = "🚀" * 1200
    res1 = codex._parse_worker_validation(f"Validation: {emojis}")
    assert res1["status"] == "UNKNOWN"
    assert set(res1.keys()) == expected_keys
    enc1 = json_transport.dumps(res1).encode("utf-8")
    assert len(enc1) <= codex.IMPLEMENTER_REPORT_LIMIT
    res1["summary"].encode("utf-8")

    # 2. Summary içermeyen, limite yakın geçerli PASS:
    # Summary tüm JSON satırının kopyası olmamalı, kısa ve sabit özet olmalı.
    cmds = [f"pytest tests/test_{i:02d}.py" for i in range(25)]
    pass_payload = json.dumps({
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "commands": cmds,
        "exit_codes": [0] * len(cmds),
    })
    res2 = codex._parse_worker_validation(f"VALIDATION: {pass_payload}")
    assert res2["status"] == "PASS"
    assert set(res2.keys()) == expected_keys
    assert res2["summary"] == "Structured worker validation reported PASS."
    assert "VALIDATION:" not in res2["summary"]
    enc2 = json_transport.dumps(res2).encode("utf-8")
    assert len(enc2) <= codex.IMPLEMENTER_REPORT_LIMIT

    # 3. Normalize edilmiş PASS/FAIL nesnesi sınırı aşıyorsa:
    huge_cmds = [f"pytest tests/test_long_path_module_{i:04d}_spec.py" for i in range(50)]
    huge_payload = json.dumps({
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "commands": huge_cmds,
        "exit_codes": [0] * len(huge_cmds),
    })
    res3 = codex._parse_worker_validation(f"VALIDATION: {huge_payload}")
    assert res3["status"] == "UNKNOWN"
    assert set(res3.keys()) == expected_keys
    enc3 = json_transport.dumps(res3).encode("utf-8")
    assert len(enc3) <= codex.IMPLEMENTER_REPORT_LIMIT

    # 4. ASCII ve çok baytlı Unicode summary için byte invariantı:
    res4_ascii = codex._parse_worker_validation("Validation: " + ("a" * 2000))
    assert res4_ascii["status"] == "UNKNOWN"
    enc4_ascii = json_transport.dumps(res4_ascii).encode("utf-8")
    assert len(enc4_ascii) <= codex.IMPLEMENTER_REPORT_LIMIT

    cjk_and_emoji = "测试验证🌟" * 400
    res4_uni = codex._parse_worker_validation(f"Validation: {cjk_and_emoji}")
    assert res4_uni["status"] == "UNKNOWN"
    enc4_uni = json_transport.dumps(res4_uni).encode("utf-8")
    assert len(enc4_uni) <= codex.IMPLEMENTER_REPORT_LIMIT
    res4_uni["summary"].encode("utf-8")


def test_task_contract_partial_is_not_grounded_and_handles_lists():
    # Case 1: Only objective provided -> grounded MUST be False
    prompt_obj_only = "ORCH|task=t1|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nTask Objective: Only objective"
    obj, crit, grounded = codex._parse_task_contract(prompt_obj_only, "t1", "implementation", ["f.py"])
    assert grounded is False
    assert obj == "Only objective"

    # Case 2: Only criteria provided -> grounded MUST be False
    prompt_crit_only = "ORCH|task=t2|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nAcceptance Criteria: Only criteria"
    obj, crit, grounded = codex._parse_task_contract(prompt_crit_only, "t2", "implementation", ["f.py"])
    assert grounded is False
    assert crit == "Only criteria"

    # Case 3: Multiline bullet list criteria
    prompt_bullets = (
        "ORCH|task=t3|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "Task Objective: Full feature\n"
        "Acceptance Criteria:\n"
        "- First criterion\n"
        "- Second criterion\n"
        "- Third criterion"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_bullets, "t3", "implementation", ["f.py"])
    assert grounded is True
    assert obj == "Full feature"
    assert crit == "First criterion; Second criterion; Third criterion"

    # Case 4: JSON list criteria
    prompt_json_list = (
        "ORCH|task=t4|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        'TASK_CONTRACT: {"objective": "Feature", "criteria": ["Crit 1", "Crit 2"]}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json_list, "t4", "implementation", ["f.py"])
    assert grounded is True
    assert obj == "Feature"
    assert crit == "Crit 1; Crit 2"


def test_goal_and_acceptance_key_value_headers_ground_task_contract():
    prompt = "gOaL: Update the parser\naCcEpTaNcE: Preserve contract validation"

    obj, crit, grounded = codex._parse_task_contract(prompt, "goal", "implementation", [])

    assert grounded is True
    assert obj == "Update the parser"
    assert crit == "Preserve contract validation"


def test_requirements_key_value_header_and_missing_criteria_label():
    prompt = "Goal: Update parser\nRequirements:\n- a\n- b\nValidation: run focused tests"
    _, crit, grounded = codex._parse_task_contract(prompt, "requirements", "implementation", [])
    assert grounded is True
    assert crit == "a; b"

    _, _, grounded = codex._parse_task_contract(
        "Goal: Update parser\n- a\n- b\nValidation: run focused tests",
        "no-criteria",
        "implementation",
        [],
    )
    assert grounded is False


def test_goal_only_key_value_header_is_not_grounded():
    obj, _, grounded = codex._parse_task_contract(
        "Goal: Update the parser", "goal-only", "implementation", []
    )

    assert grounded is False
    assert obj == "Update the parser"


def test_acceptance_key_value_header_collects_bullets_until_next_section():
    prompt = (
        "Goal: Update the parser\n"
        "Acceptance:\n"
        "- Preserve JSON contracts\n"
        "- Collect multiline criteria\n"
        "Verified context:\n"
        "This must not become a criterion.\n"
        "Report:\n"
        "Summarize the change."
    )

    _, crit, grounded = codex._parse_task_contract(prompt, "goal-list", "implementation", [])

    assert grounded is True
    assert crit == "Preserve JSON contracts; Collect multiline criteria"


def test_plain_objective_label_grounds_with_next_line_and_inline_criteria():
    prompt = (
        "ORCH|task=plain|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "oBjEcTiVe:\n"
        "Recognize plain objective labels. Acceptance criteria: preserve grounding"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt, "plain", "implementation", ["f.py"])

    assert grounded is True
    assert obj == "Recognize plain objective labels"
    assert crit == "preserve grounding"


def test_mid_sentence_objective_is_not_a_plain_objective_label():
    prompt = (
        "ORCH|task=mid|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "This sentence mentions objective: but is not a label.\n"
        "Acceptance Criteria: preserve grounding"
    )
    obj, _, grounded = codex._parse_task_contract(prompt, "mid", "implementation", ["f.py"])

    assert grounded is False
    assert "Task mid" in obj


def test_first_task_or_plain_objective_label_wins():
    prompt = (
        "ORCH|task=precedence|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "Task objective: first objective\n"
        "Objective: second objective\n"
        "Acceptance Criteria: preserve grounding"
    )
    obj, _, grounded = codex._parse_task_contract(
        prompt, "precedence", "implementation", ["f.py"]
    )

    # If both labels appear, the first one in the prompt wins.
    assert grounded is True
    assert obj == "first objective"


def test_task_contract_markdown_sections_are_grounded_and_fence_aware():
    prompt = (
        "# Objective\n"
        "Refactor the task contract parser.\n\n"
        "# Acceptance criteria\n"
        "1. Collect the first criterion.\n\n"
        "2. Collect the second criterion.\n"
        "\n"
        "- Collect the third criterion.\n"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt, "markdown", "implementation", [])
    assert grounded is True
    assert obj == "Refactor the task contract parser."
    assert crit == "Collect the first criterion.; Collect the second criterion.; Collect the third criterion."

    inline_prompt = "## Task: Fix the parser\n\n### Requirements\n- Preserve JSON parsing"
    obj, crit, grounded = codex._parse_task_contract(inline_prompt, "inline", "implementation", [])
    assert grounded is True
    assert obj == "Fix the parser"
    assert crit == "Preserve JSON parsing"

    objective_only = "# Goal\nDo the work\n\n# Acceptance criteria:\n"
    _, _, grounded = codex._parse_task_contract(objective_only, "objective-only", "implementation", [])
    assert grounded is False

    empty_criteria = "# Objective\nDo the work\n\n# Criteria:\n"
    _, _, grounded = codex._parse_task_contract(empty_criteria, "empty-criteria", "implementation", [])
    assert grounded is False

    fenced_headings = "```markdown\n# Objective\nDo the work\n# Criteria\n- Looks valid\n```"
    _, _, grounded = codex._parse_task_contract(fenced_headings, "fenced", "implementation", [])
    assert grounded is False

    exact_task_prompt = """Task objective: make `_parse_task_contract` in `.claude/hooks/orchestrator/codex.py` recognise task contracts written as Markdown sections, so correctly specified delegations stop being flagged `TASK_CONTRACT_NOT_GROUNDED`.

Context (verified at HEAD 3fab168): every delegation today (5 of 5) was flagged `TASK_CONTRACT_NOT_GROUNDED` and forced to FULL review, although each prompt had an objective section and an acceptance-criteria section. The parser only accepts a one-line `TASK_CONTRACT: {json}` or key-value lines starting with `task objective:` / `acceptance criteria:`. Prompts written as Markdown headings (`# Objective` with the text on following lines) are never matched. Separately, criteria collection stops at the first blank line, so a Markdown list with blank lines between items would be cut short even in the key-value form. Consequence: the one-turn AUDIT path never fires, costing Claude an extra review turn on every delegation.

Acceptance criteria:

1. Recognise the CLASS "a Markdown heading (any level, `#` to `###`) naming the objective or the criteria, followed by its section body". Objective heading names: `Objective`, `Task objective`, `Task`, `Goal`. Criteria heading names: `Acceptance criteria`, `Criteria`, `Requirements`. Case-insensitive; an optional trailing colon is allowed; text on the heading line after a colon (e.g. `# Task: fix the parser`) counts as body.
2. A section body runs until the next heading of the same or a higher level, NOT until the first blank line. Numbered and bulleted items separated by blank lines are all collected.
3. Headings inside fenced code blocks (```) are ignored.
4. Existing forms keep working unchanged: `TASK_CONTRACT:` JSON, and the `task objective:` / `acceptance criteria:` key-value lines.
5. Unchanged rule: grounded only when BOTH a non-empty objective and non-empty criteria are found. A present-but-empty section does not count.

Tests — examples, non-exhaustive: `# Objective` + `# Acceptance criteria` with a numbered list separated by blank lines -> grounded, every item collected; `## Task: X` + `### Requirements` -> grounded; objective only -> not grounded; empty criteria section -> not grounded; headings only inside a code fence -> not grounded; both existing forms -> still grounded. Also run the exact prompt of this task through the parser: it must be grounded.

Validation (explicit user instruction): do NOT run the full suite. Run only `tests/test_codex_hook.py` plus any new test file, once green at the end. Claude runs the full suite once before acceptance.

Report: the change, the tests added, the exact test command and counts."""
    _, _, grounded = codex._parse_task_contract(exact_task_prompt, "exact", "implementation", [])
    assert grounded is True


def test_base_bundle_contains_contract_and_validation_fields():
    bundle = codex._base_bundle()
    assert "task_contract_grounded" in bundle
    assert bundle["task_contract_grounded"] is False
    assert "worker_validation" in bundle
    assert bundle["worker_validation"] is None


def test_codex_reply_continuation_inherits_prior_grounded_contract(workspace, config):
    repo, state = workspace
    (repo / "f.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    # Initial call with explicit grounded contract
    initial_prompt = (
        "ORCH|task=task-cont|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        'TASK_CONTRACT: {"objective": "Persistent objective", "criteria": "Persistent criteria"}'
    )
    initial_call = event(prompt=initial_prompt, tool_use_id="call-init")
    codex.pre_tool_use(initial_call, config=config, state_root=state, repo_root=repo)

    post = {
        **initial_call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-persistent", "content": "Initial work done"}),
            }
        ],
    }

    def stub_reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=stub_reviewer)

    # Continuation reply with generic prompt without explicit contract
    reply_prompt = "ORCH|task=task-cont|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nContinue work"
    reply_call = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt=reply_prompt,
        tool_use_id="call-reply",
        threadId="thread-persistent",
    )
    codex.pre_tool_use(reply_call, config=config, state_root=state, repo_root=repo)

    # Read pending and verify inherited contract
    pending = codex._read_pending(state, reply_call)
    assert pending["task_contract_grounded"] is True
    assert pending["task_objective"] == "Persistent objective"
    assert pending["acceptance_criteria"] == "Persistent criteria"


def test_codex_reply_inherits_handoff_and_reviews_cumulative_same_path_diff(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=reply-repair|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Both turns are reviewed"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-repair-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\n" + "".join(f"turn_one_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-repair",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    diffs = []

    def stub_reviewer(**kwargs):
        _, diff = reviewer_module._delegated_diff(
            Path(kwargs["repo_root"]), kwargs["delta"], kwargs["classification"]
        )
        diffs.append(diff)
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )

    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-repair-second",
        threadId="thread-reply-repair",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        (repo / "file0.py").read_text(encoding="utf-8")
        + "".join(f"turn_two_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-repair",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert len(diffs) == 2
    assert "+turn_one_0 = True" in diffs[1]
    assert "+turn_two_0 = True" in diffs[1]
    assert bundle["concurrent_writer"] is False
    assert "UNKNOWN_CONCURRENT_WRITER" not in [finding["code"] for finding in bundle["findings"]]
    assert "REVIEW_PACKET_INCOMPLETE" not in [finding["code"] for finding in bundle["findings"]]


def test_codex_reply_does_not_inherit_handoff_after_intervening_edit(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=reply-writer|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Reject concurrent edits"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-writer-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 1\nturn_one = True\n", encoding="utf-8")
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-writer",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }

    seen_deltas = []

    def stub_reviewer(**kwargs):
        seen_deltas.append(kwargs["delta"])
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    (repo / "file0.py").write_text(
        "value = 1\nturn_one = True\nother_writer = True\n", encoding="utf-8"
    )
    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-writer-second",
        threadId="thread-reply-writer",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\nturn_one = True\nother_writer = True\nturn_two = True\n",
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-writer",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert "UNKNOWN_CONCURRENT_WRITER" in [finding["code"] for finding in bundle["findings"]]


@pytest.mark.parametrize("halt_paths", [["file0.py"], [f"path-{i:02d}.py" for i in range(21)], None])
def test_codex_reply_keeps_pre_turn_dirty_path_ambiguous(workspace, config, monkeypatch, halt_paths):
    repo, state = workspace
    original_classify = ledger.classify_concurrent_writers
    def classify_with_halt_paths(*args, **kwargs):
        result = original_classify(*args, **kwargs)
        if "file0.py" in result.get("ambiguous_paths", []):
            result["halt_paths"] = halt_paths
            if halt_paths is None:
                result["code"] = "UNKNOWN_DELTA"
        return result
    monkeypatch.setattr(ledger, "classify_concurrent_writers", classify_with_halt_paths)
    observed = []
    monkeypatch.setattr(codex, "_observe", lambda entrypoint, *args, **kwargs: observed.append((entrypoint, args, kwargs)))
    (repo / "file0.py").write_text("value = 1\nuser_before = True\n", encoding="utf-8")
    prompt = (
        "ORCH|task=reply-dirty|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Keep user edits attributable"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-dirty-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\nuser_before = True\n" + "".join(
            f"turn_one_{index} = True\n" for index in range(30)
        ),
        encoding="utf-8",
    )
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-dirty",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }

    seen_deltas = []

    def stub_reviewer(**kwargs):
        seen_deltas.append(kwargs["delta"])
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-dirty-second",
        threadId="thread-reply-dirty",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        (repo / "file0.py").read_text(encoding="utf-8")
        + "".join(f"turn_two_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-dirty",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert bundle["concurrent_writer"] is False
    assert "file0.py" in seen_deltas[-1]["ambiguous_paths"]
    code = "UNKNOWN_DELTA" if halt_paths is None else "UNKNOWN_CONCURRENT_WRITER"
    halt = next(finding for finding in bundle["findings"] if finding["code"] == code)
    if halt_paths is None:
        assert halt["message"] == "Concurrent-writer classification requires FULL Claude review; halt paths unavailable (UNKNOWN_DELTA)"
    else:
        preview = ", ".join(halt_paths[:10]) + (" …" if len(halt_paths) > 10 else "")
        assert halt["message"] == f"Concurrent-writer classification requires FULL Claude review; halt paths ({len(halt_paths)}): {preview}"
    payload = next(kwargs for entrypoint, args, kwargs in observed if args and args[0] == "UNKNOWN_CONCURRENT_WRITER")
    assert payload["code"] == code
    assert payload["halt_paths"] == (halt_paths[:20] if halt_paths is not None else None)
    assert payload["halt_path_count"] == (len(halt_paths) if halt_paths is not None else None)


@pytest.mark.parametrize("case", ["invalid", "save_error", "valid"])
def test_reviewer_raw_evidence_only_for_invalid_response(workspace, config, monkeypatch, case):
    repo, state = workspace
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    call = event(tool_use_id=f"reviewer-raw-{case}")
    codex.pre_tool_use(call, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [{"type": "text", "text": json.dumps({
            "threadId": f"thread-reviewer-raw-{case}",
            "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
        })}],
    }
    if case == "valid":
        report = reviewer_module.CompletedReview(
            policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "PASS", (), "conversation-1", "gemini-3.8-flash-high"
        )
    else:
        report = reviewer_module.UnavailableReview(
            "REVIEWER_RESPONSE_INVALID", "Reviewer-1 response or configuration was invalid (JSONDecodeError).",
            "gemini-3.8-flash-high", raw_stdout="{invalid json", decoder_detail="Expecting property name: line 1 column 2 (char 1)",
        )
    if case == "save_error":
        original_persist = codex._persist_raw_output
        def fail_reviewer_save(root, event, value, suffix=""):
            if suffix == "-reviewer1":
                raise OSError("disk full")
            return original_persist(root, event, value, suffix)
        monkeypatch.setattr(codex, "_persist_raw_output", fail_reviewer_save)
    raw = codex.post_tool_use(
        post, config=review_config, state_root=state, repo_root=repo, reviewer=lambda **kwargs: report
    )
    bundle = response_bundle(raw, review_config)
    evidence = list((state / "raw_tool_outputs").glob("*-reviewer1.json"))
    if case == "valid":
        assert evidence == []
        assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    else:
        finding = next(item for item in bundle["findings"] if item["code"] == "REVIEWER_RESPONSE_INVALID")
        assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
        if case == "invalid":
            assert len(evidence) == 1
            assert json.loads(evidence[0].read_text(encoding="utf-8")) == "{invalid json"
            assert str(evidence[0]) in finding["message"]
            assert "Expecting property name: line 1 column 2 (char 1)" in finding["message"]
        else:
            assert evidence == []
            assert "raw output not saved (OSError)" in finding["message"]


@pytest.mark.parametrize(
    "is_grounded,report_content,expected_mode,expected_findings,expected_receipt",
    [
        # grounded + valid worker PASS + reviewer accept => AUDIT ve audit_receipt
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}',
            policy.AUDIT,
            [],
            True,
        ),
        # ungrounded + valid PASS + reviewer accept => FULL + TASK_CONTRACT_NOT_GROUNDED
        (
            False,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}',
            policy.FULL,
            ["TASK_CONTRACT_NOT_GROUNDED"],
            False,
        ),
        # grounded + missing validation + reviewer accept => FULL + WORKER_VALIDATION_UNVERIFIED
        (
            True,
            "Tests ran and everything looked fine in stdout.",
            policy.FULL,
            ["WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
        # grounded + malformed validation + reviewer accept => FULL + WORKER_VALIDATION_UNVERIFIED
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":"pytest","exit_codes":[0]}',
            policy.FULL,
            ["WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
        # grounded + valid FAIL + reviewer accept => FULL + WORKER_VALIDATION_FAILED
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[1]}',
            policy.FULL,
            ["WORKER_VALIDATION_FAILED"],
            False,
        ),
        # Grounding ve validation ayni anda hataliysa iki finding de korunmali
        (
            False,
            "Just done without contract and without validation block.",
            policy.FULL,
            ["TASK_CONTRACT_NOT_GROUNDED", "WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
    ],
)
def test_end_to_end_review_mode_matrix(
    workspace, config, is_grounded, report_content, expected_mode, expected_findings, expected_receipt
):
    repo, state = workspace
    (repo / "test_matrix.py").write_text("v = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "matrix-init"], cwd=repo, check=True)

    if is_grounded:
        prompt = (
            "ORCH|task=t-mat|phase=implementation|route=normal|basis=test fixture design boundary|scope=test_matrix.py\n"
            "Task Objective: Update test matrix file\n"
            "Acceptance Criteria: test_matrix.py has value 2"
        )
    else:
        prompt = "ORCH|task=t-mat|phase=implementation|route=normal|basis=test fixture design boundary|scope=test_matrix.py\nGeneric prompt"

    call = event(prompt=prompt)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "test_matrix.py").write_text("v = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-matrix", "content": report_content}),
            }
        ],
    }

    def stub_reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=stub_reviewer
    )
    bundle = response_bundle(raw, config)

    assert bundle["claude_review_mode"] == expected_mode
    if expected_receipt:
        assert bundle["audit_receipt"] is not None
        assert bundle["audit_receipt"]["verdict"] == "READY_FOR_ONE_TURN_ACCEPT"
    else:
        assert bundle["audit_receipt"] is None

    for finding_code in expected_findings:
        assert any(f["code"] == finding_code for f in bundle["findings"]), f"Expected finding {finding_code}"


def test_post_accepts_pending_written_before_project_namespacing(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(task="legacy-pending") + "\nImplement.", tool_use_id="call-legacy")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(Path(state), call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending.pop("project_key")
    pending_path.write_text(json.dumps(pending), encoding="utf-8")
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {"type": "text", "text": json.dumps({"threadId": "thread-legacy", "content": "done"})}
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo), config
    )

    assert not any(item["code"] == "INTERNAL_POST_ERROR" for item in bundle["findings"])
    assert bundle["thread_id"] == "thread-legacy"


def test_hook_main_reads_utf8_and_emits_utf8_with_cp1254_wrappers(monkeypatch):
    payload = {"hook_event_name": "unknown", "prompt": "❯ ı"}
    input_raw = io.BytesIO(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(hook, "dispatch", lambda event_value: event_value)

    assert hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8")) == payload


def test_hook_main_writes_non_ascii_response_as_utf8_with_cp1254(monkeypatch):
    output_raw = io.BytesIO()
    monkeypatch.setattr(hook.sys, "stdin", io.TextIOWrapper(io.BytesIO(b"{}"), encoding="cp1254"))
    monkeypatch.setattr(hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(hook, "dispatch", lambda _event_value: {"marker": "❯"})

    assert hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8")) == {"marker": "❯"}


def _fallback_config(config):
    config["fallback"] = {"agent": "opencode", "model": "anthropic/claude-sonnet",
                          "family": "anthropic"}
    return config


def _fallback_state(workspace):
    repo, root = workspace
    return active_coder_state.read(repo, root)


def _record_launch_failure(workspace, kind, *, retry_after=None):
    repo, state = workspace
    project = project_root.project_key(repo.resolve())
    job_id = uuid.uuid4().hex + uuid.uuid4().hex
    indexes = state / "async_jobs"
    jobs = state / "codex_exec_mcp" / "jobs"
    indexes.mkdir(parents=True, exist_ok=True)
    jobs.mkdir(parents=True, exist_ok=True)
    (indexes / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "project_key": project,
    }), encoding="utf-8")
    evidence = {"type": kind}
    if retry_after is not None:
        evidence["retryAfterSeconds"] = retry_after
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "state": "FAILED", "phase": "implementation",
        "finishedAt": time.time(), "launchFailureEvidence": evidence,
    }), encoding="utf-8")
    return job_id


def test_p63b_fallback_gaps_python_oracle(workspace, config, monkeypatch):
    fixture_path = Path(__file__).parent / "fixtures" / "go_policy" / "pre_fallback_gaps.json"
    fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python codex.pre_tool_use"
    repo, state_root = workspace
    project = project_root.project_key(repo.resolve())
    config["fallback"] = fixture["fallback"]
    now = datetime.now(timezone.utc)
    for case in fixture["cases"]:
        shutil.rmtree(state_root / "async_jobs", ignore_errors=True)
        shutil.rmtree(state_root / "codex_exec_mcp" / "jobs", ignore_errors=True)
        active_path = active_coder_state.state_path(repo, state_root)
        active_path.parent.mkdir(parents=True, exist_ok=True)
        if case["active"] == "malformed":
            active_path.write_text("{", encoding="utf-8")
        elif case["active"] == "primary_asked":
            active_coder_state.write(repo, {
                "active_coder": "primary", "triggering_evidence": {
                    "type": "CREDIT_EXHAUSTED", "trigger_id": "asked",
                    "question": "already asked", "question_session": "session-1",
                    "question_shown": False, "answerable": False,
                }, "reset_cycle_marker": None, "last_probe_time": None,
            }, state_root)
        elif case["active"] == "fallback_quota":
            marker = now - timedelta(days=1)
            reset = now + timedelta(days=1)
            marker_text = marker.isoformat().replace("+00:00", "Z")
            reset_text = reset.isoformat().replace("+00:00", "Z")
            evidence = {"type": "QUOTA", "trigger_id": "old-cycle", "reset_cycle_marker": marker_text}
            if case.get("quota") == "same_cycle":
                evidence["declined_reset_cycle_marker"] = reset_text
            active_coder_state.write(repo, {
                "active_coder": "fallback", "triggering_evidence": evidence,
                "reset_cycle_marker": marker_text, "last_probe_time": None,
            }, state_root)
            monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda value=reset_text: {
                "5h_used_pct": 96, "5h_resets_at": value,
            })
        else:
            active_path.unlink(missing_ok=True)
            monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)

        indexes = state_root / "async_jobs"
        jobs = state_root / "codex_exec_mcp" / "jobs"
        for launch in case["launch"]:
            indexes.mkdir(parents=True, exist_ok=True)
            jobs.mkdir(parents=True, exist_ok=True)
            job_id = launch["id"]
            (indexes / f"{job_id}.json").write_text(json.dumps({"jobId": job_id, "project_key": project}), encoding="utf-8")
            value = {"jobId": job_id, "phase": "implementation", "state": launch["state"], "finishedAt": launch["finishedAt"]}
            if launch.get("type"):
                value["launchFailureEvidence"] = {"type": launch["type"]}
            (jobs / f"{job_id}.json").write_text(json.dumps(value), encoding="utf-8")

        observed = []
        monkeypatch.setattr(codex.telemetry, "emit", lambda event_type, *args, **kwargs: observed.append(event_type))
        if case["outcome"] == "internal_failure":
            assert active_path.read_text(encoding="utf-8") == "{"
            failed = codex.pre_tool_use(event(), config=config, state_root=state_root, repo_root=repo)
            assert "additionalContext" in failed["hookSpecificOutput"]
            assert observed == case["observations"]
            continue
        result = codex.pre_tool_use(event(), config=config, state_root=state_root, repo_root=repo)
        output = result["hookSpecificOutput"]
        if case["outcome"] in {"fallback_question", "deny_asked"}:
            assert output["permissionDecision"] == "deny"
            assert "FALLBACK_CONSENT_REQUIRED" in output["permissionDecisionReason"]
            active = active_coder_state.read(repo, state_root)
            if case["outcome"] == "fallback_question":
                assert active["triggering_evidence"] == case["record"]
                pending = compaction_advisor._read_state(
                    compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1"
                )["fallback_pending"]
                assert pending == {
                    "line": case["question"], "project": str(repo), "model": fixture["fallback"]["model"],
                    "blocked": False, "notice": False, "delivered": False,
                }
            else:
                assert active["triggering_evidence"]["question"] == "already asked"
        elif case["outcome"] == "return_offer":
            assert output["permissionDecision"] == "deny"
            assert "PRIMARY_RETURN_CONSENT_REQUIRED" in output["permissionDecisionReason"]
            active = active_coder_state.read(repo, state_root)
            assert active["triggering_evidence"]["question"] == case["question"]
            pending = compaction_advisor._read_state(
                compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1"
            )["fallback_pending"]
            assert pending == {
                "line": case["question"], "project": str(repo), "model": config["routes"]["normal"]["model"],
                "blocked": False, "notice": False, "delivered": False,
            }
        else:
            assert "updatedInput" in output, (case["name"], output)
        assert observed == case["observations"], (case["name"], observed)


def _show_fallback_question(repo, state_root, session_id):
    path = compaction_advisor._state_directory(repo) / "compaction_advisor.json"
    pending = compaction_advisor._read_state(path, session_id)["fallback_pending"]
    assert pending is not None
    blocked = compaction_advisor.stop({"session_id": session_id, "last_assistant_message": ""}, repo)
    assert blocked and "exactly one bold quoted question line" in blocked["reason"]
    rendered = compaction_advisor.stop(
        {"session_id": session_id, "last_assistant_message": pending["line"]}, repo)
    assert rendered is None
    compaction_advisor.advise({"session_id": session_id}, repo)
    evidence = active_coder_state.read(repo, state_root)["triggering_evidence"]
    assert evidence["question_shown"] is True and evidence["answerable"] is True
    return pending


def test_fallback_receipt_detector_accepts_quoted_form(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    _record_launch_failure(workspace, "CREDIT_EXHAUSTED")
    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    pending = compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json",
        "session-1")["fallback_pending"]
    assert pending["line"].startswith("> ⚓ **")
    assert compaction_advisor.stop(
        {"session_id": "session-1", "last_assistant_message": pending["line"]}, repo) is None


def _approve_fallback(workspace, config, state, *, session_id="session-1", tool_use_id="approve"):
    repo, _ = workspace
    prompt = event(prompt=orch(reason="user_override", scope="file0.py") + "\nSwitch to fallback.",
                   tool_use_id=tool_use_id)
    prompt["session_id"] = session_id
    return codex.pre_tool_use(prompt, config=config, state_root=state, repo_root=repo)


def test_approved_fallback_routes_same_and_later_session(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: {
        "5h_used_pct": 96, "5h_resets_at": "2099-01-01T00:00:00Z"})
    call = event(prompt=orch(scope="file0.py") + "\nDo the work.")
    denied = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert denied["hookSpecificOutput"]["permissionDecision"] == "deny"
    pending_path = compaction_advisor._state_directory(repo) / "compaction_advisor.json"
    pending = compaction_advisor._read_state(pending_path, "session-1")["fallback_pending"]
    assert "anthropic/claude-sonnet" in pending["line"]
    shown = compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": ""}, repo)
    assert shown["decision"] == "block" and "exactly one bold quoted question line" in shown["reason"]
    line = pending["line"]
    assert compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": line}, repo) is None
    compaction_advisor.advise({"session_id": "session-1"}, repo)
    evidence = _fallback_state(workspace)["triggering_evidence"]
    assert evidence["question_shown"] is True and evidence["answerable"] is True
    approved = codex.pre_tool_use(
        event(prompt=orch(reason="user_override", scope="file0.py") + "\nSwitch to fallback."),
        config=config, state_root=state, repo_root=repo)
    routed = approved["hookSpecificOutput"]["updatedInput"]
    assert routed["agent"] == "opencode" and routed["model"] == "anthropic/claude-sonnet"
    switched = _fallback_state(workspace)
    assert switched["reset_cycle_marker"] == "2099-01-01T00:00:00Z"
    assert isinstance(switched["triggering_evidence"]["switched_at"], float)
    next_session = event(prompt=orch(task="task-2") + "\nDo another task.", tool_use_id="session-2")
    next_session["tool_input"]["prompt"] = orch(task="task-2", scope="file1.py") + "\nDo another task."
    next_session["session_id"] = "session-2"
    later = codex.pre_tool_use(next_session, config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in later["hookSpecificOutput"], later
    assert later["hookSpecificOutput"]["updatedInput"]["model"] == "anthropic/claude-sonnet"
    receipts = []
    monkeypatch.setattr(compaction_advisor, "queue_dispatch",
                        lambda _event, _repo, **values: receipts.append(values["line"]))
    codex._append_dispatch_receipt({}, {"label": "test task", "model": routed["model"],
                                        "effort": "none", "route": "normal"}, state, event())
    assert "anthropic/claude-sonnet" in receipts[0]


def test_ambiguous_launch_evidence_does_not_trigger_fallback(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    project = project_root.project_key(repo.resolve())
    job_id = "a" * 64
    (state / "async_jobs").mkdir(parents=True)
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    (state / "async_jobs" / f"{job_id}.json").write_text(
        json.dumps({"jobId": job_id, "project_key": project}), encoding="utf-8")
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "state": "FAILED", "phase": "implementation", "finishedAt": time.time(),
        "launchFailureEvidence": {"type": "AMBIGUOUS"}}), encoding="utf-8")
    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in result["hookSpecificOutput"]
    assert _fallback_state(workspace)["triggering_evidence"] is None
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "state": "FAILED", "phase": "implementation", "finishedAt": time.time(),
        "launchFailureEvidence": {"type": "CREDIT_EXHAUSTED"}}), encoding="utf-8")
    triggered = codex.pre_tool_use(event(tool_use_id="typed-credit"), config=config,
                                   state_root=state, repo_root=repo)
    assert triggered["hookSpecificOutput"]["permissionDecision"] == "deny"


def test_unconfigured_fallback_does_not_add_transition_prompt(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in result["hookSpecificOutput"]
    assert not (compaction_advisor._state_directory(repo) / "compaction_advisor.json").exists()
    active_path = active_coder_state.state_path(repo, state)
    assert not active_path.exists() and not active_path.with_name(active_path.name + ".lock").exists()
    actual_bundle = response_bundle(
        codex._finalize_replacement(codex._base_bundle(), config, state,
                                    event(tool_use_id="normal-no-fallback")), config)
    configured = _fallback_config(dict(config))
    control = codex._finalize_replacement(codex._base_bundle(), configured, state,
                                          event(tool_use_id="normal-no-fallback"))
    assert actual_bundle == response_bundle(control, configured)
    assert not any("fallback" in key for key in actual_bundle)
    active_path = active_coder_state.state_path(repo, state)
    assert not active_path.exists() and not active_path.with_name(active_path.name + ".lock").exists()
    actual_bundle = response_bundle(
        codex._finalize_replacement(codex._base_bundle(), config, state,
                                    event(tool_use_id="normal-no-fallback")), config)
    control = codex._finalize_replacement(codex._base_bundle(), config, state,
                                          event(tool_use_id="normal-no-fallback"))
    assert actual_bundle == response_bundle(control, config)
    assert not any("fallback" in key for key in actual_bundle)


def test_user_override_without_later_prompt_is_refused(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    result = codex.pre_tool_use(event(prompt=orch(reason="user_override") + "\nProceed."),
                                config=config, state_root=state, repo_root=repo)
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "FALLBACK_CONSENT_REQUIRED" in result["hookSpecificOutput"]["permissionDecisionReason"]


def test_user_override_with_stale_fallback_consent_is_refused(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    active = _fallback_state(workspace)
    active["triggering_evidence"] = {
        "type": "CREDIT_EXHAUSTED", "trigger_id": "failure-1",
        "question_session": "earlier-session", "question_shown": True, "answerable": True,
    }
    active_coder_state.write(repo, active, state)

    result = codex.pre_tool_use(
        event(prompt=orch(reason="user_override") + "\nSwitch to fallback."),
        config=config, state_root=state, repo_root=repo)

    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "FALLBACK_CONSENT_REQUIRED" in result["hookSpecificOutput"]["permissionDecisionReason"]
    assert _fallback_state(workspace)["active_coder"] == "primary"


def test_later_success_clears_prior_typed_failure(workspace, config, monkeypatch):
    repo, state = workspace
    _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    project = project_root.project_key(repo.resolve())
    indexes = state / "async_jobs"
    jobs = state / "codex_exec_mcp" / "jobs"
    indexes.mkdir(parents=True)
    jobs.mkdir(parents=True)
    for char, status, finished, evidence in (
            ("a", "FAILED", 10, {"type": "CREDIT_EXHAUSTED"}),
            ("b", "SUCCEEDED", 20, None)):
        job_id = char * 64
        (indexes / f"{job_id}.json").write_text(json.dumps(
            {"jobId": job_id, "project_key": project}), encoding="utf-8")
        job = {"jobId": job_id, "state": status, "phase": "implementation", "finishedAt": finished}
        if evidence is not None:
            job["launchFailureEvidence"] = evidence
        (jobs / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
    assert codex._last_launch_failure(state, project) is None
    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in result["hookSpecificOutput"]
    assert _fallback_state(workspace)["triggering_evidence"] is None


@pytest.mark.parametrize("reason", ["continue", "fix"])
def test_reply_stays_pinned_to_recorded_thread_after_fallback_switch(
    workspace, config, monkeypatch, reason
):
    repo, state = workspace
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    active = _fallback_state(workspace)
    active["active_coder"] = "fallback"
    active_coder_state.write(repo, active, state)
    route = policy.resolve_route("normal", config)
    record = ledger.build_session_record("task-1", "thread-1", "normal", route.model, route.effort,
                                         phase="implementation", scope=["file0.py"],
                                         reason="new_task", label="test task", agent="codex")
    ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo.resolve()))).save(record)
    reply = event(codex.CODEX_REPLY_TOOL, prompt=orch(reason=reason) + "\nContinue.", threadId="thread-1")
    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    updated = result["hookSpecificOutput"]["updatedInput"]
    assert "permissionDecision" not in result["hookSpecificOutput"]
    assert updated.get("agent", "codex") == "codex"
    assert "model" not in updated
    assert codex._read_pending(state, reply)["reason"] == reason


def test_reply_to_fallback_thread_stays_pinned_to_fallback_agent_and_model(workspace, config):
    repo, state = workspace
    config = _fallback_config(config)
    fallback_model = config["fallback"]["model"]
    record = ledger.build_session_record(
        "task-1", "fallback-thread", "normal", fallback_model, "none",
        phase="implementation", scope=["file0.py"], label="test task", agent="opencode",
    )
    ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo.resolve()))).save(record)

    reply = event(codex.CODEX_REPLY_TOOL, prompt=orch() + "\nContinue.", threadId="fallback-thread")
    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)

    assert "permissionDecision" not in result["hookSpecificOutput"]
    updated = result["hookSpecificOutput"]["updatedInput"]
    assert updated["agent"] == "opencode"
    assert "model" not in updated
    pending = codex._read_pending(state, reply)
    assert pending["agent"] == "opencode" and pending["model"] == fallback_model


def test_reviewer_uses_configured_fallback_family(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    config["fallback"]["family"] = "google"
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    active = _fallback_state(workspace)
    active["active_coder"] = "fallback"
    active_coder_state.write(repo, active, state)
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(state, call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["reviewer_candidates"] = [{"agent": "agy", "model": "gemini-3.8-flash"}]
    codex._atomic_json_write(pending_path, pending)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    result = codex.post_tool_use(
        {**call, "hook_event_name": "PostToolUse", "tool_response":
         {"structuredContent": {"threadId": "fallback-family-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo,
        reviewer=lambda **values: invoked.append(values),
    )
    bundle = response_bundle(result, config)
    assert not invoked
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_VENDOR_CONFLICT"


def test_task_type_mismatch_is_telemetry_only(workspace, config, monkeypatch):
    repo, state = workspace
    contract = (
        "Objective: Count the accepted entries in the supplied report.\n"
        "Acceptance criteria: Return the exact number of accepted entries."
    )
    objective, criteria, grounded = codex._parse_task_contract(
        contract, "task-1", "implementation", ["file0.py"]
    )
    assert grounded
    assert codex._derive_task_type(objective, criteria, "implementation") == "unknown"
    assert codex._derive_task_type(objective, criteria, "operation") == "counting_status"

    ambiguous = (
        "Objective: Check the deployment status.\n"
        "Acceptance criteria: Report whether the service is healthy."
    )
    objective, criteria, grounded = codex._parse_task_contract(
        ambiguous, "task-1", "implementation", ["file0.py"]
    )
    assert grounded
    assert codex._derive_task_type(objective, criteria, "implementation") == "unknown"
    assert codex._derive_task_type(
        "Follow the established runbook to fetch the package.",
        "Use the documented steps and return the result.",
        "implementation",
    ) == "operation"

    prompt = orch(phase="operation", route="subtle", scope=None) + "\n" + contract
    call = event(prompt=prompt)
    observations = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observations.append((args, kwargs)))
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    updated = result["hookSpecificOutput"]["updatedInput"]
    pending = codex._read_pending(state, call)
    route_event = next(item[1] for item in observations if item[0][1] == "TASK_ROUTE_SELECTED")

    assert updated["model"] == config["routes"]["subtle"]["model"]
    assert pending["route"] == "subtle"
    assert pending["task_type"] == "counting_status"
    assert route_event["task_type"] == "counting_status"
    assert route_event["suspected_route_mismatch"] == {
        "declared_route": "subtle", "derived_type": "counting_status"
    }
    codex._post_observations(call, state, pending, {"delta_files": 0})
    finished_event = next(item[1] for item in observations if item[0][1] == "IMPLEMENTER_FINISHED")
    assert finished_event["task_type"] == "counting_status"
    assert finished_event["suspected_route_mismatch"] == route_event["suspected_route_mismatch"]

    ambiguous_call = event(
        prompt=orch(task="ambiguous-task", phase="operation", route="subtle", scope=None) + "\n" + ambiguous,
        tool_use_id="call-ambiguous",
    )
    codex.pre_tool_use(ambiguous_call, config=config, state_root=state, repo_root=repo)
    ambiguous_pending = codex._read_pending(state, ambiguous_call)
    assert ambiguous_pending["task_type"] == "unknown"
    assert ambiguous_pending["suspected_route_mismatch"] is None

    before = updated.copy()
    monkeypatch.setattr(codex, "_derive_task_type", lambda *_args: "unknown")
    other_call = event(
        prompt=orch(task="no-flag", phase="operation", route="subtle", scope=None) + "\n" + contract,
        tool_use_id="call-no-flag",
    )
    other = codex.pre_tool_use(other_call, config=config, state_root=state, repo_root=repo)
    assert other["hookSpecificOutput"]["updatedInput"] == before


def test_implementation_count_wording_stays_unknown_and_unflagged(workspace, config, monkeypatch):
    repo, state = workspace
    prompt = (
        orch(task="implementation-count", route="subtle", scope="file0.py")
        + "\nObjective: Record the number of retries in the implementation.\n"
        + "Acceptance criteria: Preserve the retry counter behavior."
    )
    call = event(prompt=prompt, tool_use_id="implementation-count")
    observations = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observations.append((args, kwargs)))

    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending = codex._read_pending(state, call)
    route_event = next(item[1] for item in observations if item[0][1] == "TASK_ROUTE_SELECTED")

    assert "updatedInput" in result["hookSpecificOutput"]
    assert pending["phase"] == "implementation"
    assert pending["task_type"] == "unknown"
    assert pending["suspected_route_mismatch"] is None
    assert route_event["task_type"] == "implementation"
    assert route_event["suspected_route_mismatch"] is None


def test_finished_task_persists_code_map_present_and_codex_usage(workspace):
    _repo, state = workspace
    task_id = "code-map-present"
    codex._post_observations(
        {"session_id": "session-1", "duration_ms": 1234,
         "tool_response": {"structuredContent": {"usage": {
             "input_tokens": 23, "cached_input_tokens": 11, "output_tokens": 7,
             "reasoning_output_tokens": 3,
         }}}},
        state,
        {"task_id": task_id, "project_key": "unresolved", "agent": "codex",
         "model": "gpt-6-sol", "effort": "high", "phase": "implementation", "route": "normal",
         "has_code_map": True},
        {"delta_files": 0, "model": "served-model", "effort": "xhigh"},
    )

    aggregate = json.loads((state / "projects" / "unresolved" / "telemetry" /
                            "tasks" / f"{task_id}.json").read_text(encoding="utf-8"))
    assert aggregate["has_code_map"] is True
    assert aggregate["duration_ms"] == 1234
    assert aggregate["usage"]["codex_input_tokens"] == 23
    assert aggregate["usage"]["codex_cached_input_tokens"] == 11
    assert aggregate["usage"]["codex_output_tokens"] == 7
    assert aggregate["usage"]["codex_reasoning_output_tokens"] == 3
    assert (aggregate["served_route"], aggregate["served_model"], aggregate["served_effort"]) == (
        "normal", "served-model", "xhigh")


def test_partial_codex_usage_keeps_missing_fields_unknown(workspace, monkeypatch):
    _repo, state = workspace
    events = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: events.append((args, kwargs)))
    codex._post_observations(
        {"session_id": "session-1", "duration_ms": 1234,
         "tool_response": {"structuredContent": {"usage": {
             "input_tokens": 23, "output_tokens": 7, "thinking_tokens": 0,
         }}}},
        state,
        {"task_id": "partial-usage", "project_key": "unresolved", "agent": "codex",
         "model": "gpt-6-sol", "effort": "high", "phase": "implementation", "route": "normal"},
        {"delta_files": 0},
    )

    finished = next(kwargs for args, kwargs in events if args[1] == "IMPLEMENTER_FINISHED")
    assert finished["codex_usage_known"] is True
    assert finished["codex_input_tokens"] == 23
    assert finished["codex_output_tokens"] == 7
    assert "codex_cached_input_tokens" not in finished
    assert "codex_reasoning_output_tokens" not in finished
    assert "codex_thinking_tokens" not in finished


def test_async_continuation_count_uses_session_and_job_window(monkeypatch, tmp_path):
    events = [
        {"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": "session-1",
         "ts_utc": "2026-10-03T10:00:00Z"},
        {"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": "session-1",
         "ts_utc": "2026-10-03T10:01:00Z"},
        {"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": "session-1",
         "ts_utc": "2026-10-03T10:02:00Z"},
        {"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": "other-session",
         "ts_utc": "2026-10-03T10:01:00Z"},
    ]
    monkeypatch.setattr(codex.telemetry, "_raw_events", lambda _root: events)

    assert codex._async_continuation_turn_count(
        tmp_path, "session-1", 1791021630, 1791021690,
    ) == 1


def test_async_continuation_count_is_unknown_without_session_id(monkeypatch, tmp_path):
    assert codex._async_continuation_turn_count(tmp_path, None, 1, 2) is None
    turn_events = [{"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": None,
                    "ts_utc": "1970-01-01T00:00:01Z"}]
    monkeypatch.setattr(codex.telemetry, "_raw_events", lambda _root: turn_events)
    assert codex._async_continuation_turn_count(tmp_path, "session-1", 1, 2) is None
    turn_events.append({"event_type": "CONTEXT_IO_TURN_RECORDED", "session_id": "session-1",
                        "ts_utc": "1970-01-01T00:00:03Z"})
    assert codex._async_continuation_turn_count(tmp_path, "session-1", 1, 2) == 0
    events = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: events.append((args, kwargs)))
    codex._post_observations(
        {"duration_ms": 1, "tool_response": {"usage": {}}}, tmp_path,
        {"task_id": "sessionless-job", "project_key": "unresolved", "agent": "codex",
         "async_call": "call", "claude_continuation_turn_count": None},
        {},
    )
    finished = next(kwargs for args, kwargs in events if args[1] == "IMPLEMENTER_FINISHED")
    assert finished["claude_continuation_turn_count"] is None


def test_async_continuation_event_includes_session_id(tmp_path):
    state = tmp_path / "state"
    session = "session-emitted-turn"
    context_io_telemetry.stop({"session_id": session, "hook_event_name": "Stop"}, state_root=state)
    emitted = codex.telemetry._raw_events(codex.telemetry._root(state))
    [event] = [item for item in emitted if item.get("event_type") == "CONTEXT_IO_TURN_RECORDED"]
    assert event["session_id"] == session


@pytest.mark.parametrize(
    ("worker_validation", "expected_status"),
    [
        ({"status": "FAIL"}, "FAIL"),
        ({"status": "PASS"}, "PASS"),
        (None, "UNKNOWN"),
    ],
)
def test_finished_event_records_worker_validation_and_fix_reason(
    workspace, worker_validation, expected_status
):
    _repo, state = workspace
    codex._post_observations(
        {"session_id": "session-1"},
        state,
        {
            "task_id": f"worker-validation-{expected_status}",
            "project_key": "unresolved",
            "phase": "implementation",
            "route": "normal",
            "reason": "fix",
            "reason_source": "explicit",
        },
        {"worker_validation": worker_validation},
    )

    events = [
        json.loads(line)
        for path in (state / "telemetry" / "events").rglob("*.jsonl")
        for line in path.read_text(encoding="utf-8").splitlines()
    ]
    finished = next(event for event in events if event["event_type"] == "IMPLEMENTER_FINISHED")
    assert finished["payload"]["worker_validation_status"] == expected_status
    assert finished["payload"]["reason"] == "fix"


def test_finished_task_persists_code_map_absent(workspace):
    _repo, state = workspace
    task_id = "code-map-absent"
    codex._post_observations(
        {"session_id": "session-1", "duration_ms": 1234},
        state,
        {"task_id": task_id, "project_key": "unresolved", "agent": "codex",
         "model": "gpt-6-sol", "phase": "implementation", "route": "normal",
         "has_code_map": False},
        {"delta_files": 0},
    )

    aggregate = json.loads((state / "projects" / "unresolved" / "telemetry" /
                            "tasks" / f"{task_id}.json").read_text(encoding="utf-8"))
    assert aggregate["has_code_map"] is False


def test_reviewer_incomplete_telemetry_has_error_elapsed_and_retry_outcome(workspace, monkeypatch):
    _repo, state = workspace
    observed = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observed.append((args, kwargs)))
    event_value = event(tool_use_id="review-incomplete")
    codex._post_observations(
        event_value, state,
        {"task_id": "review-incomplete", "agent": "codex", "model": "gpt-6-sol",
         "phase": "implementation", "route": "normal"},
        {"reviewer1_result": policy.UNCERTAIN, "reviewer1_provider": "agy",
         "reviewer1_model_requested": "gemini-test", "reviewer1_attempts": []},
        reviewer_report={"review_status": "NOT_RUN_INCOMPLETE_PACKET",
                         "error_code": "REVIEW_PACKET_INCOMPLETE",
                         "elapsed_seconds": 2.5, "retry_count": 1,
                         "retry_recovered": False},
        reviewer_started=True,
    )
    failure = next(kwargs for args, kwargs in observed if args[1] == "REVIEWER1_FAILED")
    assert failure["reviewer1_error_code"] == "REVIEW_PACKET_INCOMPLETE"
    assert failure["duration_ms"] == 2500
    assert failure["reviewer1_retry_count"] == 1
    assert failure["reviewer1_retry_recovered"] is False


def test_reviewer_finding_count_stays_separate_from_task_decisions_and_schema_is_bounded(
    workspace, monkeypatch,
):
    _repo, state = workspace
    captured = []

    def capture(*args, **kwargs):
        if args[:2] and args[0] == "emit" and args[1].startswith("REVIEWER1_"):
            captured.append((args[1], kwargs))

    monkeypatch.setattr(codex, "_observe", capture)

    def observe_review(task_id, result, *, findings=None, skipped=None):
        captured.clear()
        telemetry.emit(
            "TASK_ROUTE_SELECTED", task_id=task_id, state_root=state,
            phase="implementation", route="normal", model="gpt-6-sol", effort="medium",
            task_type="implementation",
        )
        telemetry.emit(
            "IMPLEMENTER_STARTED", task_id=task_id, state_root=state,
            phase="implementation", route="normal", model="gpt-6-sol", effort="medium",
            implementer="codex-main",
        )
        bundle = {"reviewer1_result": result, "reviewer1_provider": "agy",
                  "reviewer1_model_requested": "gemini-test", "reviewer1_attempts": [], "findings": []}
        if skipped is not None:
            bundle["reviewer1_skipped_reason"] = skipped
        report = {"review_status": ("NOT_RUN_INCOMPLETE_PACKET"
                                     if result == policy.REVIEWER1_UNAVAILABLE else "COMPLETED"),
                  "findings": findings or []}
        codex._post_observations(
            event(tool_use_id=f"review-{task_id}"), state,
            {"task_id": task_id, "agent": "codex", "model": "gpt-6-sol",
             "phase": "implementation", "route": "normal"},
            bundle, reviewer_report=report, reviewer_started=skipped is None,
        )
        event_type, payload = next(
            (name, kwargs) for name, kwargs in captured if name.startswith("REVIEWER1_")
        )
        payload = dict(payload)
        payload.pop("task_id", None)
        telemetry.emit(event_type, task_id=task_id, **payload)
        telemetry.update_task(task_id, state_root=state)

    # Simulate post-repair bundle state: the initial reviewer report still has both findings.
    observe_review("findings-then-send-back", policy.CHANGES_REQUIRED,
                   findings=[{"code": "F1"}, {"code": "F2"}])
    telemetry.emit(
        "CLAUDE_DECISION_RECORDED", task_id="findings-then-send-back", state_root=state,
        decision="send_back", task_type="implementation",
        source={"kind": "reply", "job_id": "reply-1"},
    )
    telemetry.update_task("findings-then-send-back", state_root=state)

    observe_review("accepted", policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE)
    telemetry.emit(
        "CLAUDE_DECISION_RECORDED", task_id="accepted", state_root=state,
        decision="accepted", task_type="implementation",
        source={"kind": "apply", "job_id": "apply-1"},
    )
    telemetry.update_task("accepted", state_root=state)

    observe_review("unavailable", policy.REVIEWER1_UNAVAILABLE)
    observe_review("skipped", None, skipped="NO_DELTA")

    def task(task_id):
        path = state / "projects" / "unresolved" / "telemetry" / "tasks" / f"{task_id}.json"
        return json.loads(path.read_text(encoding="utf-8"))

    finding_task = task("findings-then-send-back")
    assert finding_task["reviewer1_initial_finding_count"] == 2
    assert finding_task["reviewer1_result"] == policy.CHANGES_REQUIRED
    assert finding_task["claude_acceptance"]["send_backs"] == 1
    assert finding_task["claude_acceptance"]["outcome"] == "unknown"
    assert task("accepted")["claude_acceptance"]["outcome"] == "accepted"
    assert task("unavailable")["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
    assert task("unavailable")["reviewer1_initial_finding_count"] is None
    assert task("skipped")["reviewer1_result"] is None
    assert task("skipped")["reviewer1_skipped_reason"] == "NO_DELTA"
    assert "REVIEWER1_FINDING_DISPOSITION" not in telemetry_schema.EVENT_TYPES

    valid_payloads = {
        "COMPACTION_ADVICE_OUTCOME": {"notice_id": "notice-1", "advice_written": True,
                                      "compaction_observed": True, "pre_context_tokens": 100,
                                      "post_context_tokens": 25, "elapsed_turns": 2},
        "SCOPE_CONFLICT_OUTCOME": {"action": "queued", "job_id": "job-1", "queue_wait_ms": 1},
    }
    for event_type, payload in valid_payloads.items():
        assert telemetry_schema.valid_event(telemetry_schema.build_event(event_type, payload=payload))
    malformed = (
        ("REVIEWER1_FINISHED", {"reviewer1_initial_finding_count": True}),
        ("COMPACTION_ADVICE_OUTCOME", {"notice_id": "bad id"}),
        ("SCOPE_CONFLICT_OUTCOME", {"action": "bogus"}),
    )
    for event_type, payload in malformed:
        with pytest.raises(ValueError):
            telemetry_schema.build_event(event_type, payload=payload)


def test_reviewer_incomplete_telemetry_uses_measured_elapsed_when_report_is_none(workspace, monkeypatch):
    _repo, state = workspace
    observed = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observed.append((args, kwargs)))
    event_value = event(tool_use_id="review-incomplete-none")
    codex._post_observations(
        event_value, state,
        {"task_id": "review-incomplete-none", "agent": "codex", "model": "gpt-6-sol",
         "phase": "implementation", "route": "normal"},
        {"reviewer1_result": policy.UNCERTAIN, "reviewer1_provider": "agy",
         "reviewer1_model_requested": "gemini-test", "reviewer1_attempts": []},
        reviewer_report={"review_status": "NOT_RUN_INCOMPLETE_PACKET",
                         "error_code": "REVIEW_PACKET_INCOMPLETE",
                         "elapsed_seconds": None},
        reviewer_started=True,
        reviewer_elapsed_seconds=4.25,
    )
    failure = next(kwargs for args, kwargs in observed if args[1] == "REVIEWER1_FAILED")
    assert failure["duration_ms"] == 4250


def _activate_fallback(workspace, kind, **evidence):
    repo, root = workspace
    state = active_coder_state.read(repo, root)
    state.update(active_coder="fallback", triggering_evidence={"type": kind, **evidence})
    active_coder_state.write(repo, state, root)
    return repo, root


def test_credit_return_probes_once_and_notifies_on_success(workspace, config, monkeypatch):
    repo, state = _activate_fallback(
        workspace, "CREDIT_EXHAUSTED", trigger_id="job", switched_at=1000)
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    monkeypatch.setattr(codex.time, "time", lambda: 2000)
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    probes = []
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda agent, model, **kwargs:
                        probes.append((agent, model, kwargs.get("timeout"))) or {"ok": True})

    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)

    assert probes == [("codex", config["sol_model"], 25)]
    assert _fallback_state(workspace)["active_coder"] == "primary"
    assert "permissionDecision" not in result["hookSpecificOutput"]
    queued = compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1")
    pending = queued["fallback_pending"]
    assert pending is not None
    assert pending["notice"] is True
    stop = compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": ""}, repo)
    assert "exactly one line in the user's language as" in stop["reason"]
    retry = compaction_advisor.stop(
        {"session_id": "session-1", "last_assistant_message": "No notice yet."}, repo)
    assert "exactly one line in the user's language as" in retry["reason"]
    localized = f"> ⚓ **{config['sol_model']} credit is available; primary coder restored.**"
    assert compaction_advisor.stop(
        {"session_id": "session-1", "last_assistant_message": localized}, repo) is None
    assert compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json",
        "session-1")["fallback_pending"] is None
    assert compaction_advisor.stop(
        {"session_id": "session-1", "last_assistant_message": localized}, repo) is None


def test_credit_probe_failure_throttle_keeps_fallback(workspace, config, monkeypatch):
    repo, state = _activate_fallback(
        workspace, "CREDIT_EXHAUSTED", trigger_id="job", switched_at=1000)
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    monkeypatch.setattr(codex.time, "time", lambda: 2000)
    probes = []
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda agent, model, **kwargs:
                        probes.append((agent, model, kwargs.get("timeout"))) or {"ok": False})

    first = event()
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    _lease_state(workspace).release_task("task-1")
    second = event(prompt=orch(task="throttled") + "\nContinue.", tool_use_id="throttled")
    codex.pre_tool_use(
        second,
        config=config, state_root=state, repo_root=repo)

    assert len(probes) == 1
    assert _fallback_state(workspace)["active_coder"] == "fallback"


def test_no_response_probe_success_is_silent_and_throttled(workspace, config, monkeypatch):
    repo, state = _activate_fallback(
        workspace, "NO_RESPONSE", trigger_id="job", switched_at=1000)
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    monkeypatch.setattr(codex.time, "time", lambda: 2000)
    probes = []
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda agent, model, **kwargs:
                        probes.append((agent, model, kwargs.get("timeout"))) or {"ok": False})

    result = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    _lease_state(workspace).release_task("task-1")
    throttled = event(prompt=orch(task="silent-throttled") + "\nContinue.",
                      tool_use_id="silent-throttled")
    codex.pre_tool_use(throttled, config=config,
                       state_root=state, repo_root=repo)
    _lease_state(workspace).release_task("silent-throttled")
    now = [2300]
    monkeypatch.setattr(codex.time, "time", lambda: now[0])
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda agent, model, **kwargs:
                        probes.append((agent, model, kwargs.get("timeout"))) or {"ok": True})
    returned = codex.pre_tool_use(event(prompt=orch(task="silent-return") + "\nContinue.",
                                        tool_use_id="silent-return"), config=config,
                                  state_root=state, repo_root=repo)

    assert len(probes) == 2
    assert _fallback_state(workspace)["active_coder"] == "primary"
    assert "permissionDecision" not in returned["hookSpecificOutput"]
    assert result["hookSpecificOutput"]["updatedInput"]["agent"] == "opencode"
    advisor_state = compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1")
    assert advisor_state["dispatch_pending"] is None


def test_rate_limit_wait_suppresses_then_returns_without_probe(workspace, config, monkeypatch):
    repo, state = _activate_fallback(
        workspace, "RATE_LIMITED", trigger_id="job", retryAfterSeconds=30, switched_at=1000)
    config = _fallback_config(config)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    now = [1029]
    monkeypatch.setattr(codex.time, "time", lambda: now[0])
    probes = []
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda *args: probes.append(args) or {"ok": True})

    early = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert _fallback_state(workspace)["active_coder"] == "fallback"
    assert early["hookSpecificOutput"]["updatedInput"]["agent"] == "opencode"
    _lease_state(workspace).release_task("task-1")
    now[0] = 1030
    returned = codex.pre_tool_use(
        event(prompt=orch(task="rate-ready") + "\nContinue.", tool_use_id="rate-ready"),
        config=config, state_root=state, repo_root=repo)
    assert _fallback_state(workspace)["active_coder"] == "primary"
    assert "permissionDecision" not in returned["hookSpecificOutput"], returned
    assert probes == []


def test_quota_return_question_decline_is_remembered_per_reset(workspace, config, monkeypatch):
    repo, state = _activate_fallback(
        workspace, "QUOTA", trigger_id="2099-01-01T00:00:00Z")
    active_coder_state.update(
        repo, lambda value: value.update(reset_cycle_marker="2099-01-01T00:00:00Z"), state)
    config = _fallback_config(config)
    quota = {"5h_resets_at": "2099-01-01T05:00:00Z"}
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: quota)

    asked = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert asked["hookSpecificOutput"]["permissionDecision"] == "deny"
    pending_path = compaction_advisor._state_directory(repo) / "compaction_advisor.json"
    pending = compaction_advisor._read_state(pending_path, "session-1")["fallback_pending"]
    assert pending["line"] == f"> ⚓ **{config['sol_model']}'s quota has renewed. Return to the primary coder?**"
    compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": ""}, repo)
    compaction_advisor.stop(
        {"session_id": "session-1", "last_assistant_message": pending["line"]}, repo)
    compaction_advisor.advise({"session_id": "session-1"}, repo)
    evidence = _fallback_state(workspace)["triggering_evidence"]
    assert evidence["question_shown"] is True and evidence["answerable"] is True, evidence
    declined = codex.pre_tool_use(
        event(prompt=orch(scope="file0.py") + "\nStay with fallback.", tool_use_id="decline"),
        config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in declined["hookSpecificOutput"]
    assert _fallback_state(workspace)["triggering_evidence"]["declined_reset_cycle_marker"] == quota["5h_resets_at"]
    _lease_state(workspace).release_task("task-1")
    later = codex.pre_tool_use(
        event(prompt=orch(task="later", scope="file0.py") + "\nContinue.", tool_use_id="later"),
        config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in later["hookSpecificOutput"]
    assert _fallback_state(workspace)["active_coder"] == "fallback"
    _lease_state(workspace).release_task("later")
    quota["5h_resets_at"] = "2099-01-01T10:00:00Z"
    new_cycle_call = event(prompt=orch(task="new-cycle", scope="file0.py") + "\nContinue.",
                           tool_use_id="new-cycle")
    new_cycle_call["session_id"] = "session-2"
    new_cycle = codex.pre_tool_use(
        new_cycle_call, config=config, state_root=state, repo_root=repo)
    assert new_cycle["hookSpecificOutput"]["permissionDecision"] == "deny"
    pending = compaction_advisor._read_state(pending_path, "session-2")["fallback_pending"]
    compaction_advisor.stop({"session_id": "session-2", "last_assistant_message": ""}, repo)
    compaction_advisor.stop(
        {"session_id": "session-2", "last_assistant_message": f"**{pending['line']}**"}, repo)
    compaction_advisor.advise({"session_id": "session-2"}, repo)
    return_call = event(prompt=orch(reason="user_override") + "\nReturn to primary.",
                        tool_use_id="accept-return")
    return_call["session_id"] = "session-2"
    returned = codex.pre_tool_use(
        return_call, config=config, state_root=state, repo_root=repo)
    assert _fallback_state(workspace)["active_coder"] == "primary"
    assert "permissionDecision" not in returned["hookSpecificOutput"]


def test_integrated_credit_exhaustion_switch_receipt_persistence_and_return(workspace, config, monkeypatch):
    repo, state = workspace
    _record_launch_failure(workspace, "CREDIT_EXHAUSTED")
    config = _fallback_config(config)
    now = [1000]
    probes = []
    monkeypatch.setattr(codex.time, "time", lambda: now[0])
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary", lambda agent, model, **kwargs:
                        probes.append((agent, model, kwargs["timeout"])) or {"ok": True})

    denied = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert denied["hookSpecificOutput"]["permissionDecision"] == "deny"
    question = _show_fallback_question(repo, state, "session-1")

    start = event(START_TOOL, prompt=orch(reason="user_override", scope="file0.py") + "\nSwitch.",
                  tool_use_id="fallback-start")
    approved = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)
    routed = approved["hookSpecificOutput"]["updatedInput"]
    assert (routed["agent"], routed["model"]) == ("opencode", "anthropic/claude-sonnet")
    assert active_coder_state.read(repo, state)["active_coder"] == "fallback"

    started = codex.post_tool_use({**start, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"jobId": hashlib.sha256(routed["call"].encode()).hexdigest(),
                              "state": "QUEUED"}}}, config=config, state_root=state, repo_root=repo)
    assert started["hookSpecificOutput"]["hookEventName"] == "PostToolUse"
    receipt = compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1")
    assert any("anthropic/claude-sonnet" in line for line in receipt["dispatch_pending"]["lines"])

    later = event(prompt=orch(task="later-session", scope="file1.py") + "\nContinue.",
                  tool_use_id="later-session")
    later["session_id"] = "session-2"
    resumed = codex.pre_tool_use(later, config=config, state_root=state, repo_root=repo)
    assert resumed["hookSpecificOutput"]["updatedInput"]["model"] == "anthropic/claude-sonnet"
    assert probes == []

    now[0] = 1900
    returned = codex.pre_tool_use(event(prompt=orch(task="probe-return", scope="file2.py") + "\nContinue.",
                                        tool_use_id="probe-return"),
                                  config=config, state_root=state, repo_root=repo)
    assert probes == [("codex", config["sol_model"], 25)]
    assert active_coder_state.read(repo, state)["active_coder"] == "primary"
    assert returned["hookSpecificOutput"]["updatedInput"].get("agent", "codex") == "codex"
    path = compaction_advisor._state_directory(repo) / "compaction_advisor.json"
    notice = compaction_advisor._read_state(path, "session-1")["fallback_pending"]
    assert notice and notice["notice"] is True
    expected = f"> ⚓ **{config['sol_model']} credit is available; primary coder restored.**"
    assert compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": ""}, repo)
    assert compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": expected}, repo) is None
    assert compaction_advisor._read_state(path, "session-1")["fallback_pending"] is None
    assert compaction_advisor.stop({"session_id": "session-1", "last_assistant_message": expected}, repo) is None
    assert question["model"] == "anthropic/claude-sonnet"


def test_integrated_high_quota_decline_dispatches_primary_once_per_reset(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    quota = {"5h_used_pct": 96, "5h_resets_at": "2099-01-01T05:00:00Z"}
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: quota)

    blocked = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert blocked["hookSpecificOutput"]["permissionDecision"] == "deny"
    _show_fallback_question(repo, state, "session-1")
    declined = codex.pre_tool_use(
        event(prompt=orch(scope="file0.py") + "\nKeep primary.", tool_use_id="decline-primary"),
        config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in declined["hookSpecificOutput"]
    assert declined["hookSpecificOutput"]["updatedInput"].get("agent", "codex") == "codex"
    assert active_coder_state.read(repo, state)["active_coder"] == "primary"

    again = codex.pre_tool_use(
        event(prompt=orch(task="same-reset", scope="file1.py") + "\nContinue.", tool_use_id="same-reset"),
        config=config, state_root=state, repo_root=repo)
    reason = again["hookSpecificOutput"].get("permissionDecisionReason", "")
    assert "FALLBACK_CONSENT_REQUIRED" not in reason
    assert compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1")["fallback_pending"] is None


def test_integrated_rate_limit_switch_returns_silently_after_wait(workspace, config, monkeypatch):
    repo, state = workspace
    _record_launch_failure(workspace, "RATE_LIMITED", retry_after=30)
    config = _fallback_config(config)
    now = [1000]
    monkeypatch.setattr(codex.time, "time", lambda: now[0])
    monkeypatch.setattr(active_coder_state.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    denied = codex.pre_tool_use(event(), config=config, state_root=state, repo_root=repo)
    assert denied["hookSpecificOutput"]["permissionDecision"] == "deny"
    _show_fallback_question(repo, state, "session-1")
    approved = _approve_fallback(workspace, config, state)
    assert active_coder_state.read(repo, state)["active_coder"] == "fallback"
    assert approved["hookSpecificOutput"]["updatedInput"].get("model") == config["fallback"]["model"]

    now[0] += 29
    waiting = codex.pre_tool_use(event(prompt=orch(task="rate-wait", scope="file1.py") + "\nContinue.",
                                       tool_use_id="rate-wait"),
                                 config=config, state_root=state, repo_root=repo)
    assert waiting["hookSpecificOutput"]["updatedInput"]["model"] == "anthropic/claude-sonnet"
    assert active_coder_state.read(repo, state)["active_coder"] == "fallback"
    now[0] += 1
    returned = codex.pre_tool_use(event(prompt=orch(task="rate-return", scope="file2.py") + "\nContinue.",
                                         tool_use_id="rate-return"),
                                  config=config, state_root=state, repo_root=repo)
    assert "permissionDecision" not in returned["hookSpecificOutput"]
    assert active_coder_state.read(repo, state)["active_coder"] == "primary"
    pending = compaction_advisor._read_state(
        compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-1")
    assert pending["fallback_pending"] is None


def test_fallback_active_sync_async_reply_and_reviewer_attribution(workspace, config, monkeypatch):
    repo, state = workspace
    config = _fallback_config(config)
    config["fallback"]["family"] = "google"
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: None)
    probes = []
    monkeypatch.setattr(codex.time, "time", lambda: 1000)
    monkeypatch.setattr(codex.codex_exec_mcp, "probe_api_primary",
                        lambda *args, **kwargs: probes.append(args) or {"ok": False})
    active = active_coder_state.read(repo, state)
    active.update(active_coder="fallback", triggering_evidence={"type": "CREDIT_EXHAUSTED"})
    active_coder_state.write(repo, active, state)

    sync_call = event(prompt=orch(scope="file0.py") + "\nWork.", tool_use_id="fallback-sync")
    sync = codex.pre_tool_use(sync_call, config=config, state_root=state, repo_root=repo)
    sync_input = sync["hookSpecificOutput"]["updatedInput"]
    assert (sync_input["agent"], sync_input["model"]) == ("opencode", "anthropic/claude-sonnet")
    _lease_state(workspace).release_task("task-1")
    async_call = event(START_TOOL, prompt=orch(task="fallback-async", scope="file1.py") + "\nWork.",
                       tool_use_id="fallback-async")
    async_input = codex.pre_tool_use(async_call, config=config, state_root=state, repo_root=repo)[
        "hookSpecificOutput"]["updatedInput"]
    assert (async_input["agent"], async_input["model"]) == ("opencode", "anthropic/claude-sonnet")
    assert probes == [("codex", config["sol_model"])]
    _lease_state(workspace).release_task("fallback-async")

    primary_thread = ledger.build_session_record(
        "pinned-primary", "primary-thread", "normal", config["sol_model"], "high",
        phase="implementation", scope=["file2.py"], label="primary thread", agent="codex")
    ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo.resolve()))).save(primary_thread)
    fallback_thread = ledger.build_session_record(
        "pinned-fallback", "fallback-thread", "normal", config["fallback"]["model"], "none",
        phase="implementation", scope=["file2.py"], label="fallback thread", agent="opencode")
    ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo.resolve()))).save(fallback_thread)
    for thread, expected_agent, expected_model, task_id in (
            ("primary-thread", "codex", config["sol_model"], "pinned-primary"),
            ("fallback-thread", "opencode", config["fallback"]["model"], "pinned-fallback")):
        for tool_name, mode in ((codex.CODEX_REPLY_TOOL, None), (START_TOOL, "reply")):
            reply = event(tool_name, prompt=orch(task=task_id, scope="file2.py") + "\nContinue.",
                          tool_use_id=f"reply-{expected_agent}-{mode or 'sync'}", threadId=thread,
                          **({"mode": mode} if mode else {}))
            if mode:
                reply["tool_input"].pop("label", None)
            reply_result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
            assert "updatedInput" in reply_result["hookSpecificOutput"], reply_result
            pending = codex._read_pending(state, reply)
            assert reply_result["hookSpecificOutput"]["updatedInput"].get("agent", "codex") == expected_agent
            assert pending["agent"] == expected_agent and pending["model"] == expected_model
            _lease_state(workspace).release_task(task_id)

    call = event(tool_use_id="reviewer-family")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(state, call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["reviewer_candidates"] = [{"agent": "agy", "model": "gemini-3.8-flash"}]
    codex._atomic_json_write(pending_path, pending)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    post = codex.post_tool_use(
        {**call, "hook_event_name": "PostToolUse", "tool_response":
         {"structuredContent": {"threadId": "reviewer-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo,
        reviewer=lambda **values: invoked.append(values))
    bundle = response_bundle(post, config)
    assert not invoked
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_VENDOR_CONFLICT"


def test_agy_start_passes_route_effort_to_bridge(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "agy"
    for entry in config["routes"].values():
        entry.update(model="gemini-3.8-flash", effort="medium")
    start = event(START_TOOL, tool_use_id="agy-start")
    updated = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == "gemini-3.8-flash"
    assert updated["config"] == {"model_reasoning_effort": "medium"}


class _RegisteredReviewerFixture:
    def validate_model(self, model):
        return None

    def build_command(self, *args, **kwargs):
        return []

    def invoke(self, *args, **kwargs):
        return None

    def parse(self, *args, **kwargs):
        return None


def _post_with_reviewer_candidates(workspace, config, candidates, reviewer, monkeypatch=None,
                                  coder_family="unknown"):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    config["coder_family"] = coder_family
    for entry in config["routes"].values():
        entry.update(model="opencode/big-pickle", effort="none")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(state, call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["reviewer_candidates"] = candidates
    codex._atomic_json_write(pending_path, pending)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    if monkeypatch is not None:
        monkeypatch.setitem(reviewer_module.REVIEWER_ADAPTERS, "codex", _RegisteredReviewerFixture())
    result = codex.post_tool_use(
        {**call, "hook_event_name": "PostToolUse", "tool_response":
         {"structuredContent": {"threadId": "reviewer-fallback-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo, reviewer=reviewer,
    )
    return response_bundle(result, config)


def test_primary_reviewer_success_reports_selected_provider_and_no_attempt_list(workspace, config):
    report = {
        "review_status": "COMPLETED", "provider": "agy",
        "reviewer1_model_requested": "gemini-3.8-flash-high",
        "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        "validation": "PASS", "findings": [],
    }
    bundle = _post_with_reviewer_candidates(
        workspace, config, [{"agent": "agy", "model": "gemini-3.8-flash-high"}],
        lambda **kwargs: report,
    )
    assert bundle["reviewer1_provider"] == "agy"
    assert bundle["reviewer1_model_requested"] == "gemini-3.8-flash-high"
    assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    assert "reviewer1_attempts" not in bundle


def test_plan_sync_read_only_artifact_and_mutation(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(phase="plan", route="plan_standard", scope="file0.py") + "\nPlan this.",
                 sandbox="danger-full-access", **{"approval-policy": "on-request"})
    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    updated = pre["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == config["routes"]["plan_standard"]["model"]
    assert updated["config"]["model_reasoning_effort"] == "high"
    assert updated["sandbox"] == "read-only" and updated["approval-policy"] == "never"
    (repo / "file1.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "new.py").write_text("new = True\n", encoding="utf-8")
    content = "# Plan\n\nSummary:\n" + "A" * 4300 + "\n## Milestones\nFinish."
    post = {**call, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"threadId": "plan-thread", "content": content}}}
    bundle = response_bundle(codex.post_tool_use(post, config=config, state_root=state, repo_root=repo), config)
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["reviewer1_skipped_reason"] == "PHASE_PLAN"
    assert bundle["reviewer1_result"] is None
    mutation = next(f for f in bundle["findings"] if f["code"] == "PLAN_MUTATED_REPO")
    assert "concurrent writer may be the cause" in mutation["message"]
    assert "file1.py" in mutation["message"]
    assert not {"TASK_CONTRACT_NOT_GROUNDED", "WORKER_VALIDATION_UNVERIFIED"} & {
        f["code"] for f in bundle["findings"]
    }
    assert bundle["worker_validation"] is None
    stored = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
    assert stored["status"] == ledger.OK
    assert stored["record"]["live_thread"]["phase"] == "plan"
    assert Path(bundle["plan_path"]).read_text(encoding="utf-8") == content
    assert state in Path(bundle["plan_path"]).parents
    assert bundle["plan_summary"] == "A" * 4300


def test_codex_start_dispatch_caches_installed_cli_version_for_the_day(workspace, monkeypatch):
    repo, state = workspace
    cache_home = state / "codex-home"
    cache_home.mkdir(parents=True)
    monkeypatch.setenv("CODEX_HOME", str(cache_home))
    (cache_home / "models_cache.json").write_text(json.dumps({
        "client_version": "0.159.0", "models": [],
    }), encoding="utf-8")
    monkeypatch.setattr(codex.purser_state_root, "default_state_root", lambda: state)
    calls = []
    monkeypatch.setattr(codex.shutil, "which", lambda _name: "codex")
    monkeypatch.setattr(codex.subprocess, "run", lambda *_a, **_k:
                        calls.append(True) or subprocess.CompletedProcess([], 0, "codex-cli 0.156.1", ""))

    for _ in range(2):
        codex._append_dispatch_receipt({}, {
            "task_id": "task-1", "label": "First run", "model": "gpt-6-sol",
            "effort": "high", "route": "normal", "project_key": "project",
            "repo_root": str(repo), "agent": "codex",
        }, state, {"tool_name": START_TOOL, "session_id": "notice-session"})
    assert calls == [True]


def test_async_worktree_reply_is_admitted_during_overlapping_job(workspace, config, monkeypatch):
    repo, state = workspace
    async_started(
        workspace, config, prompt=orch(task="active-worktree", scope="tests/") + "\nWork."
    )
    worktree = state / "wt" / "reply-thread"
    worktree.mkdir(parents=True)
    bridge_threads = state / "bridge-threads"
    monkeypatch.setattr(codex_exec_mcp, "_state_dir", lambda: bridge_threads)
    codex_exec_mcp._save_record(bridge_threads, {"thread_id": "worktree-thread", "cwd": str(worktree),
        "config_args": [], "worktreePath": str(worktree), "snapshot": "snapshot", "head": "head",
        "repoRoot": str(repo)})
    route = policy.resolve_route("normal", config)
    saved = ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo))).save(
        ledger.build_session_record("worktree-reply", "worktree-thread", "normal", route.model,
                                    route.effort, phase="implementation", scope=["tests/"],
                                    reason="new_task"))
    assert saved["status"] == ledger.OK
    _lease_state(workspace).record_conflicts(["tests/hot.py"])
    reply = event(START_TOOL, prompt=orch(task="worktree-reply", scope="tests/") + "\nContinue.",
                  tool_use_id="worktree-reply-start", mode="reply", threadId="worktree-thread")
    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    updated = result["hookSpecificOutput"]["updatedInput"]
    assert updated["mode"] == "reply"
    assert updated["leaseQueued"] is True
    disjoint = event(START_TOOL, prompt=orch(task="worktree-reply", scope="docs/") + "\nContinue.",
                     tool_use_id="worktree-reply-disjoint", mode="reply", threadId="worktree-thread")
    disjoint_input = codex.pre_tool_use(disjoint, config=config, state_root=state, repo_root=repo)[
        "hookSpecificOutput"]["updatedInput"]
    assert "leaseQueued" not in disjoint_input


def test_queued_and_refreshed_job_board_does_not_count_until_shown(workspace, monkeypatch):
    repo, state = workspace
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    finished_id, _ = _board_job(state, "finished", "SUCCEEDED", finishedAt=time.time(),
                                applyState="conflict", applyResult={"applied": False})
    job_path = state / "codex_exec_mcp" / "jobs" / f"{finished_id}.json"
    _board_job(state, "running-one", "RUNNING")
    _board_job(state, "running-two", "RUNNING")
    payload = {"session_id": "board-refresh-session"}
    board, job_ids = codex._project_job_board_details(state, "project")
    compaction_advisor.queue_dispatch(payload, repo, board=board, board_job_ids=job_ids)
    compaction_advisor.refresh_dispatch_board(payload, repo, board, board_job_ids=job_ids)

    assert json.loads(job_path.read_text(encoding="utf-8"))["boardDisplayCount"] == 0
    shown = {**payload, "last_assistant_message": board}
    assert compaction_advisor.stop(shown, repo) is None
    updated = json.loads(job_path.read_text(encoding="utf-8"))
    assert updated["boardDisplayCount"] == 1
    assert updated["applyState"] == "conflict" and updated["applyResult"] == {"applied": False}
