import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
sys.path.insert(0, str(ORCH))
import codex
import ledger
import project_root
import task_retry


def _repo(path):
    path.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=path, check=True, timeout=10)
    return path.resolve()


def _seed(runtime, repo, disposition=None, failure=None, finished=False):
    key = project_root.project_key(repo)
    if disposition or failure:
        record = {"project_key": key, "task_id": "retry-task",
                  "terminal_disposition": disposition or "started"}
        if failure:
            record["failure"] = {"job_id": "prior-job", "category": failure}
        task_retry.save(runtime, record)
    if finished:
        session = ledger.build_session_record(
            "retry-task", "old-thread", "subtle", "gpt-6-luna", "high",
            phase="implementation", scope=["src/a.go"], label="Retry task",
            last_outcome={"status": "finished"},
        )
        assert ledger.SessionLedger(codex._project_state_root(runtime, key)).save(session)["status"] == ledger.OK
        codex._record_thread_root(runtime, "old-thread", repo)


def _case(name, repo, state):
    prompt = "ORCH|task=retry-task|phase=implementation|route=subtle|reason=new_task|basis=design choice|scope=src/a.go\nImplement."
    tool = "mcp__codex-main__codex_start"
    args = {"mode": "new", "label": "Retry task", "prompt": prompt}
    if name in {"override", "consequential"}:
        args.update(model="gpt-6-luna", config={"model_reasoning_effort": "low"})
        if name == "consequential":
            args["prompt"] = prompt.replace("route=subtle", "route=consequential")
    if name == "sync_reply":
        tool = "mcp__codex-main__codex-reply"
        args = {"threadId": "old-thread", "label": "Retry task", "prompt": prompt}
    event = {"hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": name,
             "session_id": "retry-parity", "cwd": str(repo), "tool_input": args}
    if name == "duplicate":
        call = str(uuid.uuid5(uuid.NAMESPACE_OID, codex._call_key(event)))
        job_id = hashlib.sha256(call.encode()).hexdigest()
        path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps({"jobId": job_id, "state": "QUEUED"}), encoding="utf-8")
    return event


def _semantic(response):
    hook = response.get("hookSpecificOutput", {})
    result = {key: hook.get(key) for key in ("permissionDecision", "permissionDecisionReason") if key in hook}
    updated = hook.get("updatedInput")
    if isinstance(updated, dict):
        result["dispatch"] = {key: updated.get(key) for key in ("route", "jobModel", "jobEffort", "mode")}
        result["dispatch"]["configEffort"] = updated.get("config", {}).get("model_reasoning_effort")
        result["dispatch"]["header"] = str(updated.get("prompt", "")).splitlines()[0]
    if response.get("systemMessage"):
        result["systemMessage"] = response["systemMessage"]
    return result

@pytest.fixture(scope="module")
def scratch():
    base = ROOT / ".tmp"
    base.mkdir(exist_ok=True)
    path = base / ("task-retry-" + uuid.uuid4().hex)
    path.mkdir()
    return path


@pytest.fixture(scope="module")
def go_hook(scratch):
    binary = scratch / ("purser-hook.exe" if os.name == "nt" else "purser-hook")
    cache = scratch / "go-cache"
    cache.mkdir()
    env = {**os.environ, "GOCACHE": str(cache), "GOTELEMETRY": "off"}
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/purser-hook"],
                   cwd=ROOT / "go", env=env, check=True, timeout=120)
    return binary

@pytest.mark.parametrize("name", ["first", "override", "consequential", "sync_reply",
                                   "cancelled_reuse", "corrupt", "duplicate"])
def test_python_matches_go_task_retry_dispatch(scratch, go_hook, name):
    responses = []
    for engine in ("python", "go"):
        root = scratch / (engine + "-" + name)
        repo = _repo(root / "repo")
        state = root / "state"
        state.mkdir(parents=True)
        if name == "override":
            _seed(state, repo, "cancelled", "rejected_after_sendback")
        elif name == "consequential":
            _seed(state, repo, "failed", "failed_abandonment")
        elif name == "sync_reply":
            _seed(state, repo, "discarded", "rejected_after_sendback")
            key = project_root.project_key(repo)
            session = ledger.build_session_record(
                "retry-task", "old-thread", "subtle", "gpt-6-luna", "high",
                phase="implementation", scope=["src/a.go"], label="Retry task",
            )
            assert ledger.SessionLedger(codex._project_state_root(state, key)).save(session)["status"] == ledger.OK
            codex._record_thread_root(state, "old-thread", repo)
        elif name == "cancelled_reuse":
            _seed(state, repo, "cancelled", finished=True)
        elif name == "corrupt":
            _seed(state, repo, "cancelled", finished=True)
            path = state / "task_retries" / (hashlib.sha256(
                (project_root.project_key(repo) + "\0retry-task").encode()).hexdigest() + ".json")
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('{"failure":', encoding="utf-8")
        event = _case("override" if name == "override" else name, repo, state)
        if engine == "python":
            response = codex.pre_tool_use(event, state_root=state, repo_root=repo)
        else:
            env = {**os.environ, "PURSER_STATE_DIR": str(state), "CLAUDE_PLUGIN_ROOT": str(ROOT),
                   "CODEX_HOME": str(root / "codex"), "HOME": str(root), "USERPROFILE": str(root)}
            completed = subprocess.run([str(go_hook)], input=json.dumps(event), text=True,
                                       capture_output=True, cwd=ROOT, env=env, check=True, timeout=30)
            response = json.loads(completed.stdout)
        responses.append(_semantic(response))
    assert responses[0] == responses[1]
    if name in {"override", "consequential"}:
        dispatch = responses[0]["dispatch"]
        if name == "consequential":
            assert dispatch["route"] == "consequential"
            assert dispatch["jobModel"] != "gpt-6-luna" and dispatch["jobEffort"] == "high"
        else:
            assert dispatch["route"] == "normal"
            assert dispatch["jobModel"] != "gpt-6-luna" and dispatch["jobEffort"] == "medium"
    elif name == "sync_reply":
        assert responses[0]["permissionDecision"] == "deny"
        assert responses[0]["permissionDecisionReason"] == (
            "Start a fresh job for this task with route=normal and basis=prior luna attempt failed after send-back.")
    elif name == "cancelled_reuse":
        assert "dispatch" in responses[0]
    elif name == "duplicate":
        assert responses[0]["permissionDecision"] == "deny"
        assert responses[0]["permissionDecisionReason"].startswith("ASYNC_CALL_ALREADY_ACCEPTED:")
    elif name == "corrupt":
        assert responses[0]["permissionDecision"] == "deny"
        assert responses[0]["permissionDecisionReason"] == (
            "TASK_ALREADY_FINISHED: This task already finished; use a fresh task id.")
