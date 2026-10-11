import json
import os
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "pre_success.json"
sys.path.insert(0, str(ORCH))
import codex
import code_map
import ledger
import project_root


_PLAN_SHAPE_BLOCK = (
    "Required plan shape (Purser):\n"
    "Write Markdown with a leading Summary: section of at most 4000 characters, then sections Goal and boundaries, "
    "Current evidence (observed facts vs labelled assumptions, with paths/symbols), Decision (one recommendation and "
    "brief alternatives), Milestones (at most 5; each has bounded scope, observable acceptance, validation, "
    "dependency, touched files/dirs, and parallel group; same-group paths are disjoint), Risks/open decisions (at "
    "most 5, each with an owner or blocking question), and Implementation handoff (first task's paths and acceptance). "
    "Read-only: no code edits."
)
_GO_DEFAULT_REVIEWER_CANDIDATES = [
    {"agent": "agy", "model": "gemini-3.8-flash-high"},
    {"agent": "claude", "model": "claude-haiku-5-5"},
    {"agent": "codex", "model": "gpt-6-luna", "effort": "low"},
]


def _add_go_plan_shape_prompt(case):
    """Account for Go-only prompt and reviewer-default changes in the Python oracle."""
    if case["name"] == "p64_plan_start_read_only":
        for container in (case["pending"]["async_updated_input"], case["response"]["updatedInput"]):
            container["prompt"] = container["prompt"].rstrip() + "\n\n" + _PLAN_SHAPE_BLOCK
    if case.get("pending") is not None:
        case["pending"]["reviewer_candidates"] = [dict(item) for item in _GO_DEFAULT_REVIEWER_CANDIDATES]
    return case


CASES = [
    {"name": "p64_implementation_start_admitted", "task_id": "p64-impl", "tool": "mcp__codex-main__codex_start",
     "input": {"mode": "new", "label": "P64 implementation", "timeoutSeconds": 90,
               "prompt": "ORCH|task=p64-impl|phase=implementation|route=subtle|scope=src/a.go\nImplement the change."}},
    {"name": "p64_implementation_start_queued", "task_id": "p64-queued", "tool": "mcp__codex-main__codex_start",
     "lease_setup": "hot_conflict", "input": {"mode": "new", "label": "P64 queued", "timeoutSeconds": 90,
               "prompt": "ORCH|task=p64-queued|phase=implementation|route=subtle|scope=src/a.go\nImplement after the conflict clears."}},
    {"name": "p64_scope_conflict_denial", "task_id": "p64-conflict", "tool": "mcp__codex-main__codex",
     "lease_setup": "conflict", "input": {"label": "P64 conflict", "prompt": "ORCH|task=p64-conflict|phase=implementation|route=subtle|scope=src/a.go\nImplement now."}},
    {"name": "p64_plan_start_read_only", "task_id": "p64-plan", "tool": "mcp__codex-main__codex_start",
     "input": {"mode": "new", "label": "P64 plan", "prompt": "ORCH|task=p64-plan|phase=plan|route=plan_standard\nPlan the change."}},
    {"name": "p64_operation_no_lease", "task_id": "p64-operation", "tool": "mcp__codex-main__codex",
     "input": {"label": "P64 operation", "prompt": "ORCH|task=p64-operation|phase=operation|route=operation\nInspect the status."}},
    {"name": "p64_sync_reply_send_back", "task_id": "p64-reply", "tool": "mcp__codex-main__codex-reply",
     "thread_id": "p64-thread", "input": {"threadId": "p64-thread", "label": "P64 reply",
               "prompt": "ORCH|task=p64-reply|phase=operation|route=operation\nContinue the investigation."}},
    {"name": "p64_failure_after_pending_write", "task_id": "p64-failure", "tool": "mcp__codex-main__codex_start",
     "failure": "session_save", "input": {"mode": "new", "label": "P64 failure", "prompt": "ORCH|task=p64-failure|phase=implementation|route=subtle|scope=src/a.go\nImplement then fail saving."}},
]


def _replace(value, repo, state, project_key):
    if isinstance(value, str):
        value = (value.replace(str(repo), "__REPO__").replace(str(state), "__STATE__")
                .replace(project_key, "__PROJECT_KEY__"))
        if any(token in value for token in ("__REPO__", "__STATE__", "__PROJECT_KEY__")):
            value = value.replace("\\", "/")
        return (value
                # Python's retired fallback emits its script path; the golden records the native CLI contract.
                .replace("python .claude/hooks/orchestrator/purser_leases.py --release",
                         '"__EXECUTABLE__" leases --release'))
    if isinstance(value, list):
        return [_replace(item, repo, state, project_key) for item in value]
    if isinstance(value, dict):
        result = {}
        for key, item in value.items():
            if key.endswith(("_at", "_time", "_after")) and isinstance(item, (int, float)):
                result[key] = "__TIME__"
            else:
                result[key] = _replace(item, repo, state, project_key)
        return result
    return value


def _seed_repo(repo):
    repo.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10)
    (repo / "README.md").write_text("p64 fixture baseline\n", encoding="utf-8")
    subprocess.run(["git", "add", "README.md"], cwd=repo, check=True, timeout=10)
    env = {**os.environ, "GIT_AUTHOR_NAME": "Purser Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
           "GIT_COMMITTER_NAME": "Purser Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
           "GIT_AUTHOR_DATE": "1700000000 +0000", "GIT_COMMITTER_DATE": "1700000000 +0000"}
    subprocess.run(["git", "commit", "-qm", "fixture baseline"], cwd=repo, env=env, check=True, timeout=10)


def _seed_lease(runtime, repo, setup):
    if not setup:
        return
    key = project_root.project_key(repo)
    project_state = codex._project_state_root(runtime, key)
    path = project_state / "scope_leases.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    now = time.time()
    owner = {"task_id": "p64-existing", "scope": ["src/a.go"], "state": "ACTIVE",
             "job_id": "existing-job", "queued_at": now - 10, "acquired_at": now - 10,
             "retention_after": now + 3600}
    value = {"version": 1, "owners": {"existing-owner": owner}}
    if setup == "hot_conflict":
        value["conflicts"] = [{"path": "src/a.go", "recorded_at": now - 5}]
    path.write_text(json.dumps(value), encoding="utf-8")


def _capture(case, tmp_path, monkeypatch):
    root = tmp_path / case["name"]
    runtime, repo = root / "state", root / "repo"
    runtime.mkdir(parents=True)
    _seed_repo(repo)
    home = root / "home"
    home.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    monkeypatch.setenv("CODEX_HOME", str(root / "codex"))
    monkeypatch.setenv("PURSER_STATE_DIR", str(runtime))
    _seed_lease(runtime, repo, case.get("lease_setup"))
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    if case.get("thread_id"):
        key = project_root.project_key(repo)
        state = codex._project_state_root(runtime, key)
        record = ledger.build_session_record(case["task_id"], case["thread_id"], "operation", "gpt-6-luna", "high",
                                             phase="operation", scope=[], label="P64 prior", task_contract_grounded=False)
        assert ledger.SessionLedger(state).save(record)["status"] == ledger.OK
        codex._record_thread_root(runtime, case["thread_id"], repo)
    event = {"hook_event_name": "PreToolUse", "tool_name": case["tool"], "tool_use_id": case["name"],
             "session_id": "p64-session", "tool_input": dict(case["input"])}
    if case.get("failure") == "session_save":
        original = ledger.SessionLedger.save
        def fail_save(self, record):
            if record.get("task_id") == case["task_id"]:
                return {"status": ledger.UNKNOWN, "code": "LEDGER_WRITE_FAILED", "message": "fixture failure"}
            return original(self, record)
        monkeypatch.setattr(ledger.SessionLedger, "save", fail_save)
    code_map.refresh(repo, state=runtime, budget_seconds=10.0)
    result = codex.pre_tool_use(event, config=config, state_root=runtime, repo_root=repo)
    output = result.get("hookSpecificOutput", {})
    call_key = codex._call_key(event)
    pending_path = codex._pending_path(runtime, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8")) if pending_path.exists() else None
    namespace = project_root.project_key(repo)
    session = ledger.SessionLedger(codex._project_state_root(runtime, namespace)).load(case["task_id"])
    session_record = session.get("record") if session.get("status") == ledger.OK else None
    events_path = runtime / "telemetry" / "events" / "by_task" / f"{case['task_id']}.jsonl"
    event_types = []
    if events_path.exists():
        event_types = [json.loads(line)["event_type"] for line in events_path.read_text(encoding="utf-8").splitlines()]
    lease_path = codex._project_state_root(runtime, namespace) / "scope_leases.json"
    lease_state = json.loads(lease_path.read_text(encoding="utf-8")) if lease_path.exists() else None
    response = {key: output.get(key) for key in ("updatedInput", "permissionDecision", "permissionDecisionReason") if key in output}
    if "systemMessage" in result:
        response["systemMessage"] = result["systemMessage"]
    expected = {"name": case["name"], "tool": case["tool"], "task_id": case["task_id"],
                "input": case["input"], "lease_setup": case.get("lease_setup"),
                "thread_id": case.get("thread_id"), "failure": case.get("failure")}
    return _replace({**expected, "response": response, "pending": pending,
                     "session": session_record, "event_types": event_types, "lease": lease_state},
                    repo, runtime, project_root.project_key(repo))


def test_p64_python_pre_tool_use_golden_fixture(tmp_path, monkeypatch):
    actual = [_add_go_plan_shape_prompt(_capture(case, tmp_path, monkeypatch)) for case in CASES]
    if os.environ.get("P64_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps({"oracle": "codex.pre_tool_use durable success path", "cases": actual},
                                      ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "codex.pre_tool_use durable success path"
    assert [case["name"] for case in fixture["cases"]] == [case["name"] for case in CASES]
    assert actual == fixture["cases"]
