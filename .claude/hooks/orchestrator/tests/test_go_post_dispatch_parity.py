"""Process-level parity for the complete Go PostToolUse dispatcher."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
GO = shutil.which("go")
sys.path.insert(0, str(ORCH))
import codex
import ledger
import project_root


CASES = (
    "sync_codex_success",
    "sync_reply_success",
    "codex_start_success",
    "codex_result_finished",
    "codex_result_consumed_cached",
    "codex_result_consumed_empty",
    "codex_cancel_running",
    "codex_apply_guidance",
    "missing_pending",
)


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _fixture_repo(root):
    repo = root / "repo"
    (repo / "go").mkdir(parents=True)
    (repo / "go" / "fixture.go").write_text("package fixture\n", encoding="utf-8")
    env = {**os.environ, "GIT_AUTHOR_NAME": "Parity Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
           "GIT_COMMITTER_NAME": "Parity Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
           "GIT_AUTHOR_DATE": "2020-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2020-01-01T00:00:00Z"}
    for args in (("init", "-q"), ("add", "--all"), ("commit", "-q", "-m", "fixture")):
        command = ["git", *args]
        if args[0] == "commit":
            command[1:1] = ["-c", "user.name=Parity Fixture", "-c", "user.email=fixture@example.invalid"]
        subprocess.run(command, cwd=repo, env=env, check=True, capture_output=True)
    return repo.resolve()


def _update_tool_result(event, **values):
    item = event["tool_response"]["content"][0]
    result = json.loads(item["text"])
    result.update(values)
    item["text"] = json.dumps(result, separators=(",", ":"))


def _fake_reviewer_source():
    return '''package main
import ("os")
func main() { if os.Getenv("PURSER_FAKE_MODE") == "success" { _, _ = os.Stdout.WriteString(os.Getenv("PURSER_FAKE_OUTPUT")) } }
'''


@pytest.fixture(scope="session")
def go_binaries(request):
    if GO is None:
        pytest.skip("Go toolchain unavailable")
    scratch_root = ROOT / ".tmp"
    scratch_root.mkdir(exist_ok=True)
    worker = getattr(request.config, "workerinput", {}).get("workerid", "master")
    root = scratch_root / ("post-dispatch-binaries-" + worker + "-" + uuid.uuid4().hex)
    root.mkdir()
    env = os.environ.copy()
    env["GOCACHE"] = str(root / "go-build-cache")
    root.mkdir(parents=True, exist_ok=True)
    hook = root / ("purser-hook.exe" if os.name == "nt" else "purser-hook")
    reviewer_src = root / "fake_reviewer.go"
    reviewer_src.write_text(_fake_reviewer_source(), encoding="utf-8")
    reviewer = root / ("agy.exe" if os.name == "nt" else "agy")
    try:
        for args, output in ((["build", "-o", str(hook), "./cmd/purser-hook"], hook),
                             (["build", "-o", str(reviewer), str(reviewer_src)], reviewer)):
            result = subprocess.run([GO, *args], cwd=ROOT / "go", env=env,
                                    capture_output=True, text=True, timeout=120)
            assert result.returncode == 0, result.stdout + result.stderr
            assert output.is_file()
        yield hook, reviewer
    finally:
        shutil.rmtree(root, ignore_errors=True)


@pytest.fixture
def case_tmp():
    scratch_root = ROOT / ".tmp"
    scratch_root.mkdir(exist_ok=True)
    root = scratch_root / ("post-dispatch-case-" + uuid.uuid4().hex)
    root.mkdir()
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


def _seed(name, state, repo):
    session = "post-dispatch-session"
    origin = name if name in {"sync_codex_success", "sync_reply_success", "codex_start_success",
                              "codex_apply_guidance"} else "origin-" + name
    tool = "mcp__codex-main__codex-reply" if name == "sync_reply_success" else "mcp__codex-main__codex"
    if name == "codex_start_success":
        tool = "mcp__codex-main__codex_start"
    if name.startswith("codex_result"):
        tool = "mcp__codex-main__codex_result"
    if name == "codex_cancel_running":
        tool = "mcp__codex-main__codex_cancel"
    if name == "codex_apply_guidance":
        tool = "mcp__codex-main__codex_apply"
    tool_result = {"threadId": "fixture-thread",
                   "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"fixture","commands":["fixture"],"exit_codes":[0]}'}
    event = {"hook_event_name": "PostToolUse", "tool_name": tool, "tool_use_id": name,
             "session_id": session, "cwd": str(repo), "duration_ms": 321,
             "tool_response": {"content": [{"type": "text", "text": json.dumps(tool_result, separators=(",", ":"))}]},
             "tool_input": {}}
    if name == "missing_pending":
        return event, []

    project = project_root.project_key(repo)
    call_key = hashlib.sha256((session + "\0" + origin).encode()).hexdigest()
    task = "task-" + name
    job_call = "async call " + name
    job_id = hashlib.sha256(job_call.encode()).hexdigest()
    scope = ["go"]
    baseline = ledger.capture_baseline(repo, scope, manifest_budget_seconds=2)
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, check=True,
                          capture_output=True, text=True).stdout.strip()
    assert baseline["status"] == ledger.OK and baseline["head"] == head
    pending = {"tool_use_id": origin, "session_id": session, "project_key": project,
               "task_id": task, "phase": "implementation", "route": "normal",
               "model": "gpt-6-sol", "effort": "medium", "agent": "codex", "scope": scope,
               "baseline": baseline, "repo_root": str(repo), "lease_owner": call_key,
               "task_contract_grounded": True, "task_objective": "fixture parity",
               "acceptance_criteria": "compare both dispatchers", "protected_paths": [],
               "force_full_review": False, "skip_max_lines": 0 if name == "codex_result_finished" else 50,
               "skip_max_files": 0 if name == "codex_result_finished" else 2,
               "configured": True, "user_status": "missing", "project_status": "missing",
               "reviewer_candidates": [{"agent": "agy", "model": "gemini-3.8-flash-high"}],
               "reviewer": "agy"}
    if name == "codex_start_success":
        pending.update(async_call=job_call, async_ready=True)
        _update_tool_result(event, jobId=job_id, state="RUNNING")
    if name.startswith("codex_result") or name == "codex_cancel_running":
        pending.update(async_call=job_call, async_ready=True, job_id=job_id)
        index = {"jobId": job_id, "call": job_call, "call_key": call_key,
                 "session_id": session, "project_key": project, "stage": "FINISHED"}
        if name == "codex_result_finished":
            pending["force_full_review"] = True
            index["stage"] = "FINISHED"
            _update_tool_result(event, jobId=job_id, state="SUCCEEDED")
            _write(state / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
                   {"jobId": job_id, "call": job_call, "state": "SUCCEEDED", "threadId": "fixture-thread", "finishedAt": 1700000000})
        elif name == "codex_cancel_running":
            index["stage"] = "RUNNING"
            _update_tool_result(event, jobId=job_id, state="RUNNING")
            _write(state / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
                   {"jobId": job_id, "call": job_call, "state": "RUNNING", "threadId": "fixture-thread"})
        else:
            index["stage"] = "CONSUMED"
            if name == "codex_result_consumed_cached":
                index["response"] = {"hookSpecificOutput": {"hookEventName": "PostToolUse", "additionalContext": "cached fixture"}}
            else:
                index["response"] = {}
        _write(state / "async_jobs" / f"{job_id}.json", index)
        event["tool_input"] = {"jobId": job_id}
    elif name == "codex_apply_guidance":
        _write(state / "async_jobs" / f"{job_id}.json", {"jobId": job_id, "call": job_call,
               "call_key": call_key, "session_id": session, "project_key": project, "stage": "CONSUMED"})
        _write(state / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
               {"jobId": job_id, "call": job_call, "state": "SUCCEEDED", "applyState": "applied", "repoRoot": str(repo)})
        event["tool_input"] = {"jobId": job_id}

    _write(state / "pending" / f"{call_key}.json", pending)
    ledger.ScopeLeases(state / "projects" / project).acquire(call_key, task, scope, job_id=job_id if pending.get("async_call") else None)
    return event, [call_key]


def _normalize(value, state, repo, job_ids):
    if isinstance(value, dict):
        return {key: ("__TIME__" if key == "at" or key.endswith(("_at", "_time", "timestamp")) else
                      "__DURATION__" if "duration" in key.lower() or key.endswith("_seconds") else
                      "__JOB__" if key in {"jobId", "job_id"} else
                      _normalize(item, state, repo, job_ids)) for key, item in value.items()}
    if isinstance(value, list):
        return [_normalize(item, state, repo, job_ids) for item in value]
    if isinstance(value, str):
        for path, token in ((state, "__STATE__"), (repo, "__REPO__")):
            for form in {str(path), path.as_posix(), str(path).replace("\\", "\\\\")}:
                value = value.replace(form, token)
        for job_id in job_ids:
            value = value.replace(job_id, "__JOB__")
        return value
    return value


def _state_view(state, pending_keys, job_ids, repo):
    files = {}
    for path in sorted(state.rglob("*")):
        if not path.is_file():
            continue
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            continue
        files[path.relative_to(state).as_posix()] = _normalize(value, state, repo, job_ids)
    session_keys = sorted(path.stem for path in state.glob("projects/*/sessions/*.json"))
    return {"pending": {key: (state / "pending" / f"{key}.json").exists() for key in pending_keys},
            "async_jobs": {path: value for path, value in files.items() if path.startswith("async_jobs/")},
            "session_ledger_keys": session_keys,
            "receipts_and_artifacts": {path: value for path, value in files.items()
                                       if path.startswith(("bundle_artifacts/", "receipts/", "receipt_queue/",
                                                           "compaction_advisor/", "raw_tool_outputs/"))}}


def _differences(left, right, path=""):
    if isinstance(left, dict) and isinstance(right, dict):
        result = {}
        for key in left.keys() | right.keys():
            result.update(_differences(left.get(key), right.get(key), f"{path}/{key}"))
        return result
    if left != right:
        return {path or "/": {"python": left, "go": right}}
    return {}


@pytest.mark.parametrize("case", CASES, ids=CASES)
def test_go_post_dispatch_process_parity(case, case_tmp, go_binaries):
    hook_binary, reviewer = go_binaries
    repo = _fixture_repo(case_tmp)
    source_state = case_tmp / "seed"
    source_state.mkdir()
    event, pending_keys = _seed(case, source_state, repo)
    python_state, go_state = case_tmp / "python-state", case_tmp / "go-state"
    shutil.copytree(source_state, python_state)
    shutil.copytree(source_state, go_state)
    job_ids = [path.stem for path in (source_state / "async_jobs").glob("*.json")]
    pending = next(iter((source_state / "pending").glob("*.json")), None)
    if pending is not None:
        value = json.loads(pending.read_text(encoding="utf-8"))
        if value.get("job_id"):
            job_ids.append(value["job_id"])
    payload = json.dumps(event).encode("utf-8")
    env = {**os.environ, "CLAUDE_PLUGIN_ROOT": str(ROOT), "PURSER_FAKE_MODE": "success",
           "PATH": str(reviewer.parent) + os.pathsep + os.environ.get("PATH", ""),
           "PURSER_FAKE_OUTPUT": json.dumps({"conversation_id": "fake-review", "status": "SUCCESS",
               "response": "", "duration_seconds": 0.01, "num_turns": 1, "usage": {},
               "structured_output": {"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
                                     "validation": "PASS", "findings": []}})}
    python = subprocess.run([sys.executable, str(ORCH / "hook.py"), "enforce"], cwd=ROOT,
                            env={**env, "PURSER_STATE_DIR": str(python_state)}, input=payload,
                            capture_output=True, timeout=90, check=False)
    go = subprocess.run([str(hook_binary), "enforce"], cwd=ROOT,
                        env={**env, "PURSER_STATE_DIR": str(go_state)}, input=payload,
                        capture_output=True, timeout=90, check=False)
    assert python.returncode == go.returncode == 0, (case, python.stdout, python.stderr, go.stdout, go.stderr)
    python_output = _normalize(json.loads(python.stdout), python_state, repo, job_ids)
    go_output = _normalize(json.loads(go.stdout), go_state, repo, job_ids)
    if case == "codex_start_success":
        # Go owns the user-facing waiter command; Python's optional oracle still names purser_wait.py.
        for output in (python_output, go_output):
            tool_output = output["hookSpecificOutput"]["updatedToolOutput"][0]
            message = json.loads(tool_output["text"])
            if output is go_output:
                assert "purser-hook" in message["next"] and " wait " in message["next"]
                assert message["jobId"] in message["next"]
            message["next"] = "__WAITER_COMMAND__"
            tool_output["text"] = json.dumps(message, separators=(",", ":"))
    python_state_view = _state_view(python_state, pending_keys, job_ids, repo)
    go_state_view = _state_view(go_state, pending_keys, job_ids, repo)
    differences = {}
    differences.update({"stdout" + key: value for key, value in _differences(python_output, go_output).items()})
    differences.update({"durable_side_effects" + key: value
                        for key, value in _differences(python_state_view, go_state_view).items()})
    assert not differences, f"{case} parity divergences: {json.dumps(differences, sort_keys=True, ensure_ascii=False)}"
