"""Capture the async successful codex_result path from the Python oracle."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_async_success.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger
import project_root


def _git(repo, *args):
    subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True)


def _success(root):
    state, repo = root / "state", root / "repo"
    state.mkdir()
    (repo / "src").mkdir(parents=True)
    _git(repo, "init", "-q")
    (repo / "src" / "file.go").write_text("package src\n", encoding="utf-8")
    _git(repo, "add", "--all")
    _git(repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
    project = project_root.project_key(repo.resolve())
    session, origin = "async-success-session", "origin-call"
    call = "async-success-call"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256((session + "\0" + origin).encode()).hexdigest()
    task, owner = "async-success-task", call_key
    scope = ["src"]
    baseline = ledger.capture_baseline(repo, scope, manifest_budget_seconds=1)
    assert baseline["status"] == ledger.OK
    pending = {"tool_use_id": origin, "job_id": job_id, "async_call": call, "session_id": session,
               "project_key": project, "task_id": task, "phase": "implementation", "route": "normal",
               "model": "gpt-6-sol", "effort": "medium", "scope": scope, "baseline": baseline,
               "repo_root": str(repo.resolve()), "lease_owner": owner, "agent": "codex",
               "task_contract_grounded": True, "task_objective": "review async result",
               "acceptance_criteria": "record reviewed outcome", "protected_paths": [],
               "force_full_review": False, "skip_max_lines": 50, "skip_max_files": 2,
               "configured": True, "user_status": "missing", "project_status": "missing",
               "reviewer_candidates": [{"agent": "agy", "model": "gemini-3.8-flash-high"}], "reviewer": "agy"}
    assert ledger.ScopeLeases(state / "projects" / project).acquire(owner, task, scope, job_id=job_id)["status"] == ledger.OK
    (state / "pending").mkdir()
    (state / "pending" / f"{call_key}.json").write_text(json.dumps(pending), encoding="utf-8")
    index = {"jobId": job_id, "call": call, "call_key": call_key, "session_id": session,
             "project_key": project, "stage": "FINISHED"}
    (state / "async_jobs").mkdir()
    (state / "async_jobs" / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
    job = {"jobId": job_id, "call": call, "state": "SUCCEEDED", "threadId": "async-thread",
           "repoRoot": str(repo.resolve()), "finishedAt": 1700000000}
    (state / "codex_exec_mcp" / "jobs").mkdir(parents=True)
    (state / "codex_exec_mcp" / "jobs" / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
    event = {"tool_name": "mcp__codex-main__codex_result", "tool_use_id": "result-call", "session_id": session,
             "tool_input": {"jobId": job_id}, "tool_response": {"structuredContent": {
                 "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
                 "content": 'Done.\nVALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"passed", "commands":["go test ./..."],"exit_codes":[0]}'}}}
    return state, repo.resolve(), event, index, pending


def test_post_async_success_python_fixture(tmp_path, monkeypatch):
    state, repo, event, initial_index, pending = _success(tmp_path)
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    response = codex._async_post_followup(event, config=config, state_root=state, repo_root=repo,
                                          reviewer=lambda *_a, **_k: {"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"})
    returned = json.loads(response["hookSpecificOutput"]["updatedToolOutput"][0]["text"])
    key = initial_index["call_key"]
    artifact = json.loads((state / "bundle_artifacts" / f"{key}.json").read_text(encoding="utf-8"))
    repeat = codex._async_post_followup(event, config=config, state_root=state, repo_root=repo)
    cases = {"success_no_delta": {"stage": json.loads((state / "async_jobs" / f"{initial_index['jobId']}.json").read_text())["stage"],
                                  "mode": artifact.get("claude_review_mode"),
                                  "skipped_reason": artifact.get("reviewer1_skipped_reason"),
                                  "cached_repeat_equal": repeat == response,
                                  "pending_retained": (state / "pending" / f"{key}.json").exists(),
                                  "response_state": returned.get("claude_review_mode")}}

    reviewed_root = tmp_path / "reviewed"
    reviewed_root.mkdir()
    review_state, review_repo, review_event, review_index, _ = _success(reviewed_root)
    (review_repo / "src" / "file.go").write_text("package src\n// worker delta\n", encoding="utf-8")
    review_config = dict(config, reviewer_gate={"small_delta_max_files": 0, "small_delta_max_lines": 0})
    calls = []
    def reviewer(**_kwargs):
        calls.append("called")
        return {"review_status": "COMPLETED", "provider": "agy", "reviewer1_model_requested": "gemini-3.8-flash-high",
                "result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []}
    review_response = codex._async_post_followup(review_event, config=review_config, state_root=review_state,
                                                  repo_root=review_repo, reviewer=reviewer)
    review_bundle = json.loads(review_response["hookSpecificOutput"]["updatedToolOutput"][0]["text"])
    cases["success_delta_audit"] = {"mode": review_bundle.get("claude_review_mode"),
                                    "reviewer_result": review_bundle.get("reviewer1_result"),
                                    "reviewer_calls": len(calls),
                                    "stage": json.loads((review_state / "async_jobs" / f"{review_index['jobId']}.json").read_text())["stage"]}

    full_root = tmp_path / "full"
    full_root.mkdir()
    full_state, full_repo, full_event, full_index, _ = _success(full_root)
    (full_repo / "src" / "file.go").write_text("package src\n// worker delta\n", encoding="utf-8")
    full_calls = []
    def uncertain_reviewer(**_kwargs):
        full_calls.append("called")
        return {"review_status": "COMPLETED", "provider": "agy", "reviewer1_model_requested": "gemini-3.8-flash-high",
                "result": "UNCERTAIN", "validation": "UNKNOWN", "findings": []}
    full_response = codex._async_post_followup(full_event, config=review_config, state_root=full_state,
                                                repo_root=full_repo, reviewer=uncertain_reviewer)
    full_bundle = json.loads(full_response["hookSpecificOutput"]["updatedToolOutput"][0]["text"])
    cases["success_reviewer_full"] = {"mode": full_bundle.get("claude_review_mode"),
                                      "reviewer_result": full_bundle.get("reviewer1_result"),
                                      "reviewer_calls": len(full_calls),
                                      "stage": json.loads((full_state / "async_jobs" / f"{full_index['jobId']}.json").read_text())["stage"]}

    recovery_root = tmp_path / "recovery"
    recovery_root.mkdir()
    recovery_state, recovery_repo, recovery_event, recovery_index, recovery_pending = _success(recovery_root)
    recovery_config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    first = codex._async_post_followup(recovery_event, config=recovery_config, state_root=recovery_state,
                                       repo_root=recovery_repo)
    recovery_index["stage"] = "FINALIZING"
    (recovery_state / "async_jobs" / f"{recovery_index['jobId']}.json").write_text(json.dumps(recovery_index), encoding="utf-8")
    (recovery_state / "pending" / f"{recovery_index['call_key']}.json").write_text(json.dumps(recovery_pending), encoding="utf-8")
    recovered = codex._async_post_followup(recovery_event, config=recovery_config, state_root=recovery_state,
                                            repo_root=recovery_repo)
    cases["finalizing_recovery"] = {"response_reused": recovered == first,
                                    "stage": json.loads((recovery_state / "async_jobs" / f"{recovery_index['jobId']}.json").read_text())["stage"]}

    retained_root = tmp_path / "retained"
    retained_root.mkdir()
    retained_state, retained_repo, retained_event, retained_index, _ = _success(retained_root)
    retained_job_path = retained_state / "codex_exec_mcp" / "jobs" / f"{retained_index['jobId']}.json"
    retained_job = json.loads(retained_job_path.read_text(encoding="utf-8"))
    retained_job["worktreePath"] = str(retained_repo)
    retained_job["snapshot"] = subprocess.run(["git", "-C", str(retained_repo), "rev-parse", "HEAD"],
                                                check=True, capture_output=True, text=True).stdout.strip()
    retained_job_path.write_text(json.dumps(retained_job), encoding="utf-8")
    monkeypatch.setattr(ledger, "prepare_job_replay", lambda *_a, **_k: {"status": "unchanged", "snapshot": retained_job["snapshot"]})
    retained_config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    codex._async_post_followup(retained_event, config=retained_config, state_root=retained_state,
                               repo_root=retained_repo)
    retained_pending = retained_state / "pending" / f"{retained_index['call_key']}.json"
    lease_state = json.loads((retained_state / "projects" / retained_index["project_key"] / "scope_leases.json").read_text())
    lease_owner = lease_state["owners"][retained_index["call_key"]]
    cases["unapplied_worktree_lease_retained"] = {"pending_retained": retained_pending.exists(),
                                                  "lease_retention_after": lease_owner.get("retention_after"),
                                                  "stage": json.loads((retained_state / "async_jobs" / f"{retained_index['jobId']}.json").read_text())["stage"]}
    expected = {"oracle": "codex._async_post_followup successful codex_result branch", "cases": cases}
    if os.environ.get("POST_ASYNC_SUCCESS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set POST_ASYNC_SUCCESS_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected
    cache = ROOT / ".tmp" / "go-build-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp")}
    result = subprocess.run(["go", "test", "./internal/enforce/", "-run",
                             "^(TestFinishAsyncSuccess.*|TestFinalizeNoDeltaReleasesScopeLease)$", "-count=1"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
