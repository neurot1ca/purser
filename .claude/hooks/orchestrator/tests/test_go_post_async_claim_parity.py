"""Python oracle fixture for async result claim locking and adoption."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_async_claim.json"
sys.path.insert(0, str(ORCH))
import codex
import project_root


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _normalize(value, repo, project):
    if isinstance(value, str):
        return value.replace(str(repo), "__REPO__").replace(project, "__PROJECT_KEY__")
    if isinstance(value, list):
        return [_normalize(item, repo, project) for item in value]
    if isinstance(value, dict):
        return {key: ("__PROJECT_KEY__" if key == "project_key" else _normalize(item, repo, project))
                for key, item in value.items()}
    return value


def _setup(root, *, stage="FINISHED", session="old-session", adopt=False, earlier=False):
    repo = root / "repo"
    repo.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    call = "codex async fixture"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256((session + "\0origin-tool-use").encode()).hexdigest()
    event = {"session_id": "new-session" if earlier else session, "tool_name": "mcp__plugin_purser_codex-main__codex_result",
             "tool_input": {"jobId": job_id, "adopt": adopt}, "tool_use_id": "result-tool-use"}
    index = {"jobId": job_id, "call": call, "call_key": call_key, "session_id": session,
             "project_key": project_root.project_key(repo.resolve()), "stage": stage}
    pending = {"tool_use_id": "origin-tool-use", "job_id": job_id, "async_call": call, "session_id": session,
               "project_key": index["project_key"], "task_id": "task-fixture", "baseline": {}}
    job = {"jobId": job_id, "call": call, "state": "SUCCEEDED", "threadId": "thread-fixture"}
    _write(root / "async_jobs" / f"{job_id}.json", index)
    _write(root / "pending" / f"{call_key}.json", pending)
    _write(root / "codex_exec_mcp" / "jobs" / f"{job_id}.json", job)
    return event, index, job, repo.resolve()


def test_python_post_async_claim_oracle(tmp_path, monkeypatch):
    cases = []
    root = tmp_path / "state"
    event, index, job, repo = _setup(root)
    initial_index = dict(index)
    path = root / "async_jobs" / f"{index['jobId']}.json"
    with codex._async_claim(path):
        try:
            with codex._async_claim(path):
                blocked = False
        except OSError:
            blocked = True
    original_duration = codex._async_duration
    monkeypatch.setattr(codex, "_async_duration", lambda _job: (_ for _ in ()).throw(RuntimeError("fixture stop")))
    codex._async_post_followup(event, config=None, state_root=root, repo_root=repo)
    monkeypatch.setattr(codex, "_async_duration", original_duration)
    index = json.loads(path.read_text(encoding="utf-8"))
    cases.append({"name": "claim_lock", "event": event, "index": initial_index, "pending": json.loads((root / "pending" / f"{index['call_key']}.json").read_text()),
                  "python_index": index,
                  "job": job, "expected": {"first_claim": True, "concurrent_blocked": blocked, "stage": "FINALIZING"}})

    consumed_root = tmp_path / "consumed"
    consumed, consumed_index, _, consumed_repo = _setup(consumed_root, stage="CONSUMED")
    cached = {"cached": "review response"}
    consumed_index["response"] = cached
    _write(consumed_root / "async_jobs" / f"{consumed_index['jobId']}.json", consumed_index)
    response = codex._async_post_followup(consumed, config=None, state_root=consumed_root, repo_root=consumed_repo)
    cases.append({"name": "consumed_cached_response", "event": consumed, "index": consumed_index,
                  "expected": {"response": response, "cached": cached}})

    for name, adopt, corrupt in (("adoption_required", False, False), ("adoption_allowed", True, False),
                                 ("revalidation_changed_job", True, True)):
        case_root = tmp_path / name
        case_event, case_index, case_job, case_repo = _setup(case_root, session="old-session", adopt=adopt, earlier=True)
        job_path = case_root / "codex_exec_mcp" / "jobs" / f"{case_index['jobId']}.json"
        if corrupt:
            case_job["call"] = "changed"
            _write(job_path, case_job)
        try:
            owned = codex._async_owned(case_root, case_event, case_repo)
            error = None
            stage = owned[1]["stage"]
        except Exception as exc:
            error = type(exc).__name__
            stage = None
        cases.append({"name": name, "event": case_event, "index": case_index,
                      "pending": json.loads((case_root / "pending" / f"{case_index['call_key']}.json").read_text()),
                      "job": case_job, "expected": {"error": error, "stage": stage}})

    malformed_root = tmp_path / "malformed"
    malformed_root.mkdir()
    malformed_event = {"session_id": "s", "tool_name": "mcp__plugin_purser_codex-main__codex_result",
                       "tool_input": {"jobId": "a" * 64}}
    _write(malformed_root / "async_jobs" / f"{'a' * 64}.json", {"broken": True})
    try:
        codex._async_owned(malformed_root, malformed_event, None)
        malformed_error = None
    except Exception as exc:
        malformed_error = type(exc).__name__
    cases.append({"name": "malformed_index", "event": malformed_event, "index": {"broken": True},
                  "expected": {"error": malformed_error}})

    expected = {"oracle": "codex._async_claim+_async_owned+_async_post_followup",
                "cases": [_normalize(case, tmp_path, project_root.project_key(repo)) for case in cases]}
    if os.environ.get("POST_ASYNC_CLAIM_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set POST_ASYNC_CLAIM_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-build-cache")
    result = subprocess.run(["go", "test", "./internal/enforce/", "-run", "^TestPostAsyncClaimPythonFixture$", "-count=1"],
                            cwd=ROOT / "go", text=True, capture_output=True, timeout=120, check=False, env=env)
    assert result.returncode == 0, result.stdout + result.stderr
