import hashlib
import json
import shutil
import time

from test_enforcement_process_contract import (
    START, REPO_ROOT, decision, event, go_pre_binary, pending_record, pre,
    process, repo, run_go_pre,  # noqa: F401 - pytest fixtures
)
from contract_hook_runner import run_contract_hook


def async_start(repo, *, tool_use_id="async-call", task="async-task", mode="new",
                prompt=None, thread_id=None, scope="changed.py"):
    fields = {"mode": mode, "prompt": prompt or
              f"ORCH|task={task}|phase=implementation|route=normal|reason=new_task|basis=bounded async contract|scope={scope}\nImplement the change."}
    if mode == "new":
        fields["label"] = "async contract"
    if thread_id:
        fields["threadId"] = thread_id
    return event("PreToolUse", repo, tool_use_id, START, tool_input=fields)


def start_value(state, call, status="RUNNING"):
    pending = next(json.loads(path.read_text(encoding="utf-8"))
                   for path in (state / "pending").glob("*.json")
                   if json.loads(path.read_text(encoding="utf-8")).get("tool_use_id") == call["tool_use_id"])
    job_id = hashlib.sha256(pending["async_call"].encode()).hexdigest()
    return pending, {"jobId": job_id, "state": status}


def result_call(repo, job_id):
    return event("PreToolUse", repo, "result-call", "mcp__codex-main__codex_result",
                 tool_input={"jobId": job_id})


def test_go_start_python_post_then_go_result_running_denied(repo, process, go_pre_binary):
    state, run = process
    call = async_start(repo)
    admitted = decision(run_go_pre(go_pre_binary, state, repo, call))["hookSpecificOutput"]
    assert "updatedInput" in admitted and "permissionDecision" not in admitted
    pending, value = start_value(state, call)
    post = dict(call, hook_event_name="PostToolUse", tool_response={"structuredContent": value})
    decision(run("PostToolUse", post))
    index_path = state / "async_jobs" / f"{value['jobId']}.json"
    index = json.loads(index_path.read_text(encoding="utf-8"))
    assert index["jobId"] == value["jobId"] and index["call"] == pending["async_call"]
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    (jobs / f"{value['jobId']}.json").write_text(json.dumps({
        "jobId": value["jobId"], "call": pending["async_call"], "state": "RUNNING",
    }), encoding="utf-8")
    denied = decision(run_go_pre(go_pre_binary, state, repo, result_call(repo, value["jobId"])))
    assert denied["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert denied["hookSpecificOutput"]["permissionDecisionReason"].startswith("ASYNC_STILL_RUNNING:")


def test_go_reply_mode_start_is_consumed_by_python_post(repo, process, go_pre_binary):
    state, run = process
    original = pre(repo, tool_use_id="seed-sync")
    assert "updatedInput" in decision(run_go_pre(go_pre_binary, state, repo, original))["hookSpecificOutput"]
    assert "hookSpecificOutput" in decision(run("PostToolUse", event(
        "PostToolUse", repo, "seed-sync", tool_response={"structuredContent": {
            "threadId": "contract-thread", "content": "Seeded thread."}})))
    reply = async_start(repo, tool_use_id="reply-call", task="contract-task", mode="reply",
                        prompt="ORCH|task=contract-task|phase=implementation|route=normal|reason=continue|basis=bounded reply contract|scope=changed.py\nContinue the investigation.",
                        thread_id="contract-thread")
    admitted = decision(run_go_pre(go_pre_binary, state, repo, reply))["hookSpecificOutput"]
    assert "updatedInput" in admitted and "permissionDecision" not in admitted
    pending, value = start_value(state, reply)
    assert pending["async_mode"] == "reply" and pending["prior_thread_id"] == "contract-thread"
    result = run("PostToolUse", dict(reply, hook_event_name="PostToolUse",
                                     tool_response={"structuredContent": value}))
    decision(result)
    index = json.loads((state / "async_jobs" / f"{value['jobId']}.json").read_text(encoding="utf-8"))
    assert index["call"] == pending["async_call"]


def test_duplicate_identical_start_matches_python_pre(repo, process, go_pre_binary):
    state, _ = process
    call = async_start(repo, tool_use_id="duplicate-call")
    first = decision(run_go_pre(go_pre_binary, state, repo, call))["hookSpecificOutput"]
    assert "updatedInput" in first
    python_state = state.parent / "python-reference"
    shutil.copytree(state, python_state)
    py = run_contract_hook("PreToolUse", call, python_state, entrypoint="enforcement")
    go = run_go_pre(go_pre_binary, state, repo, call)
    go_out, py_out = decision(go)["hookSpecificOutput"], decision(py)["hookSpecificOutput"]
    assert go_out.get("permissionDecision") == py_out.get("permissionDecision")
    assert ("updatedInput" in go_out) == ("updatedInput" in py_out)
    if "permissionDecision" in py_out:
        assert go_out["permissionDecisionReason"].split(":", 1)[0] == py_out["permissionDecisionReason"].split(":", 1)[0]
    else:
        assert go_out["updatedInput"].get("call") == py_out["updatedInput"].get("call")


def test_scope_lease_conflict_queues_like_python_pre(repo, process, go_pre_binary):
    state, _ = process
    blocker = pre(repo, tool_use_id="lease-owner", prompt=(
        "ORCH|task=lease-owner-task|phase=implementation|route=normal|reason=new_task|"
        "basis=lease conflict fixture|scope=changed.py\nHold the declared scope."))
    assert "updatedInput" in decision(run_go_pre(go_pre_binary, state, repo, blocker))["hookSpecificOutput"]
    blocker_pending = pending_record(state)
    lease_path = state / "projects" / blocker_pending["project_key"] / "scope_leases.json"
    lease_record = json.loads(lease_path.read_text(encoding="utf-8"))
    lease_record.setdefault("conflicts", []).append({"path": "changed.py", "recorded_at": time.time()})
    lease_path.write_text(json.dumps(lease_record), encoding="utf-8")
    py_state = state.parent / "python-reference"
    shutil.copytree(state, py_state)
    queued_call = async_start(repo, tool_use_id="queued-call", task="queued-task")
    go = decision(run_go_pre(go_pre_binary, state, repo, queued_call))["hookSpecificOutput"]
    py = decision(run_contract_hook("PreToolUse", queued_call, py_state, entrypoint="enforcement"))["hookSpecificOutput"]
    assert "permissionDecision" not in go and "permissionDecision" not in py
    assert go["updatedInput"].get("leaseQueued") is True
    assert py["updatedInput"].get("leaseQueued") is True
    go_pending = next(json.loads(path.read_text(encoding="utf-8")) for path in (state / "pending").glob("*.json")
                      if json.loads(path.read_text(encoding="utf-8")).get("tool_use_id") == "queued-call")
    py_pending = next(json.loads(path.read_text(encoding="utf-8")) for path in (py_state / "pending").glob("*.json")
                      if json.loads(path.read_text(encoding="utf-8")).get("tool_use_id") == "queued-call")
    assert go_pending["lease_queued"] is True and py_pending["lease_queued"] is True
    assert go_pending["baseline"]["status"] == py_pending["baseline"]["status"] == "UNKNOWN"


def test_rejected_start_failure_releases_lease_and_removes_pending(repo, process, go_pre_binary):
    state, run = process
    call = async_start(repo, tool_use_id="failed-start")
    assert "updatedInput" in decision(run_go_pre(go_pre_binary, state, repo, call))["hookSpecificOutput"]
    pending = next(json.loads(path.read_text(encoding="utf-8")) for path in (state / "pending").glob("*.json"))
    failure = dict(call, hook_event_name="PostToolUseFailure", error="bridge rejected start",
                   is_interrupt=False, duration_ms=3)
    result = decision(run("PostToolUseFailure", failure))["hookSpecificOutput"]
    assert "work_performed is UNKNOWN" in result["additionalContext"]
    assert not list((state / "pending").glob("*.json"))
    assert not list((state / "async_jobs").glob("*.json"))
    leases = json.loads((state / "projects" / pending["project_key"] / "scope_leases.json").read_text())
    assert leases["owners"][pending["lease_owner"]]["state"] == "RELEASED"
