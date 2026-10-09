"""Python oracle fixture for async terminal failure finalization."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_async_failure.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger
import project_root


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _normalize(value, root):
    if isinstance(value, str):
        if value[:1] in "[{":
            try:
                nested = json.loads(value)
            except ValueError:
                pass
            else:
                if isinstance(nested, (dict, list)):
                    normalized = _normalize(nested, root)
                    if normalized != nested:
                        return json.dumps(normalized)
                    return value
        value = value.replace(str(root).replace("\\", "\\\\"), "__ROOT__").replace(str(root), "__ROOT__")
        return value.replace("\\", "/") if "__ROOT__" in value else value
    if isinstance(value, list):
        return [_normalize(item, root) for item in value]
    if isinstance(value, dict):
        return {key: ("__PROJECT_KEY__" if key == "project_key" else _normalize(item, root))
                for key, item in value.items()}
    return value


def _case(root, name, state):
    repo = root / "repo"
    repo.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    call, session = "async failure " + name, "failure-session"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256((session + "\0origin-tool-use").encode()).hexdigest()
    project = project_root.project_key(repo.resolve())
    event = {"session_id": session, "tool_name": "mcp__plugin_purser_codex-main__codex_result",
             "tool_use_id": "result-tool-use", "tool_input": {"jobId": job_id},
             "tool_response": {"content": [{"type": "text", "text": json.dumps({
                 "jobId": job_id, "state": state, "errorCode": "ASYNC_TERMINAL_FAILURE"})}]}}
    index = {"jobId": job_id, "call": call, "call_key": call_key, "session_id": session,
             "project_key": project, "stage": "FINISHED"}
    pending = {"tool_use_id": "origin-tool-use", "job_id": job_id, "async_call": call,
               "session_id": session, "project_key": project, "task_id": "failure-task",
               "baseline": {}, "route": "normal", "model": "gpt-fixture", "effort": "high",
               "lease_owner": call_key, "prior_thread_id": "prior-thread"}
    job = {"jobId": job_id, "call": call, "state": state, "errorCode": "ASYNC_TERMINAL_FAILURE",
           "threadId": "thread-fixture", "acceptedAt": 10.0, "startedAt": 11.0, "finishedAt": 12.234}
    for directory, filename, value in (("async_jobs", job_id, index), ("pending", call_key, pending),
                                       ("codex_exec_mcp/jobs", job_id, job)):
        _write(root / directory / f"{filename}.json", value)
    ledger.ScopeLeases(root / "projects" / project).acquire(call_key, "failure-task", ["src"])
    return event, index, pending, job, repo.resolve()


def test_python_post_async_failure_oracle(tmp_path, monkeypatch):
    actual = []
    for name, state in (("failed", "FAILED"), ("timed_out", "TIMED_OUT"),
                        ("cancelled", "CANCELLED"), ("expired", "EXPIRED")):
        root = tmp_path / name
        root.mkdir()
        event, index, pending, job, repo = _case(root, name, state)
        response = codex._async_post_followup(event, config=None, state_root=root, repo_root=repo)
        path = root / "async_jobs" / f"{index['jobId']}.json"
        consumed = json.loads(path.read_text(encoding="utf-8"))
        evidence_path = root / "raw_tool_outputs" / f"{index['call_key']}-failure.json"
        evidence = json.loads(evidence_path.read_text(encoding="utf-8"))
        actual.append(_normalize({"name": name, "event": event, "index": index, "pending": pending, "job": job,
                                  "expected": {"response": response, "evidence": evidence,
                                               "stage": consumed["stage"], "pending_exists":
                                               (root / "pending" / f"{index['call_key']}.json").exists()}}, root))

    root = tmp_path / "replay-preparation"
    root.mkdir()
    event, index, pending, job, repo = _case(root, "replay-preparation", "SUCCEEDED")
    job.update(worktreePath=str(repo), repoRoot=str(repo), snapshot={}, snapshotRevision=1)
    _write(root / "codex_exec_mcp" / "jobs" / f"{index['jobId']}.json", job)
    event["tool_response"] = {"content": [{"type": "text", "text": json.dumps({
        "jobId": index["jobId"], "state": "SUCCEEDED", "threadId": "thread-fixture", "content": "done"})}]}
    with patch.object(codex.ledger, "prepare_job_replay", side_effect=OSError("fixture replay failure")):
        response = codex._async_post_followup(event, config=None, state_root=root, repo_root=repo)
    consumed = json.loads((root / "async_jobs" / f"{index['jobId']}.json").read_text())
    evidence = json.loads((root / "raw_tool_outputs" / f"{index['call_key']}-failure.json").read_text())
    pending = evidence["pending_call"]
    replay_event = dict(event, error="Worker replay preparation failed (OSError); FULL review required.",
                        error_code="JOB_REPLAY_PREPARATION_FAILED", replay_thread_id="thread-fixture",
                        duration_ms=1234)
    actual.append(_normalize({"name": "replay_preparation_failure", "event": replay_event, "index": index,
                              "pending": pending, "job": job,
                              "expected": {"response": response, "evidence": evidence, "stage": consumed["stage"],
                                           "pending_exists": False}}, root))

    # Recreate the pre-consume crash point using evidence from the real Python failure path.
    crash_root = tmp_path / "crash-recovery"
    crash_root.mkdir()
    event, index, pending, job, repo = _case(crash_root, "crash-recovery", "FAILED")
    source_evidence = dict(actual[0]["expected"]["evidence"], pending_call=pending)
    index["stage"] = "FINALIZING"
    _write(crash_root / "async_jobs" / f"{index['jobId']}.json", index)
    _write(crash_root / "raw_tool_outputs" / f"{index['call_key']}-failure.json", source_evidence)
    response = codex._async_post_followup(event, config=None, state_root=crash_root, repo_root=repo)
    consumed = json.loads((crash_root / "async_jobs" / f"{index['jobId']}.json").read_text())
    recovered_evidence = json.loads((crash_root / "raw_tool_outputs" / f"{index['call_key']}-failure.json").read_text())
    actual.append(_normalize({"name": "crash_recovery", "event": event, "index": index, "pending": pending,
                              "job": job, "evidence": recovered_evidence,
                              "expected": {"response": response, "evidence": recovered_evidence,
                                           "stage": consumed["stage"], "pending_exists": False}}, crash_root))

    root = tmp_path / "lease-release-failure"
    root.mkdir()
    event, index, pending, job, repo = _case(root, "lease-release-failure", "FAILED")
    with patch.object(codex, "_release_scope_lease", side_effect=codex.WiringError("LEASE_RELEASE_FAILED")):
        response = codex._async_post_followup(event, config=None, state_root=root, repo_root=repo)
    consumed = json.loads((root / "async_jobs" / f"{index['jobId']}.json").read_text())
    evidence = json.loads((root / "raw_tool_outputs" / f"{index['call_key']}-failure.json").read_text())
    actual.append(_normalize({"name": "lease_release_failure", "event": event, "index": index, "pending": pending,
                              "job": job,
                              "expected": {"response": response, "evidence": evidence, "stage": consumed["stage"],
                                           "pending_exists": True}}, root))

    if os.getenv("POST_ASYNC_FAILURE_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps({"oracle": "codex._async_post_followup terminal failure",
                                      "cases": actual},
                                         sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8", newline="\n")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture == {"oracle": "codex._async_post_followup terminal failure",
                       "cases": actual}
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-build-cache")
    result = subprocess.run(["go", "test", "./internal/enforce/", "-run", "^TestPostAsyncFailurePythonFixture$", "-count=1"],
                            cwd=ROOT / "go", text=True, capture_output=True, timeout=120, check=False, env=env)
    assert result.returncode == 0, result.stdout + result.stderr
