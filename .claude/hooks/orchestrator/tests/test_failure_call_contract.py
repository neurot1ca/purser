"""PostToolUseFailure migration map and real-hook process oracle."""
import ast
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from contract_hook_runner import run_contract_hook
from test_enforcement_process_contract import event, repo  # noqa: F401

PACKAGE = Path(__file__).resolve().parents[1]
ROOT = Path(__file__).parents[4]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "failure_process.json"
SHARED_FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "failure.json"


def test_failure_registration_is_owned_by_go_mod():
    hooks = json.loads((ROOT / "hooks" / "hooks.json").read_text(encoding="utf-8"))
    assert hooks == {"modules": ["./register.ts"]}  # Failure registration is owned by the Go module.
    adapter = (ROOT / "hooks" / "mod" / "enforcement.ts").read_text(encoding="utf-8")
    assert 'on("classic.PostToolUseFailure", ($, e, next) => handlePostToolUse(\'PostToolUseFailure\', $, e, next))' in adapter
    assert "event === 'PostToolUseFailure' && !postToolUseFailureTools.has(e.tool_name)" in adapter

# Entries name the current Go owner, or the first still-pending assembly pass.
FAILURE_CALL_CONTRACTS = {
    "post_tool_use_failure": "go/internal/enforce.PostToolUseFailure (122)",
    "_async_post_followup": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_cancel_start_job": "go/internal/enforce.cancelAsyncStart (111)",
    "_start_job_terminal_or_absent": "go/internal/enforce.asyncStartTerminalOrAbsent (111)",
    "_async_claim": "go/internal/enforce.claimAsyncResult (112)",
    "_async_owned": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_async_job": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_async_duration": "go/internal/enforce.asyncFailureDuration (115)",
    "_async_ledger_rejection": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_async_rejected_response": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_base_bundle": "go/internal/enforce.baseBundle (121)",
    "_atomic_json_write": "go/internal/enforce.atomicObservationJSON (119)",
    "_bypass_path": "go/internal/enforce.bypassPath (119)",
    "_call_key": "go/internal/enforce.PostToolUseFailure (122)",
    "_cleanup_stale_pending": "go/internal/enforce.cleanupStaleFailurePending (94)",
    "_observe": "go/internal/enforce.failureObservation (95)",
    "_observe_undelivered": "go/internal/enforce.failureUndeliveredTail (121)",
    "_pending_path": "go/internal/enforce.pendingPath (119)",
    "_pending_project_key": "go/internal/enforce.pendingProjectKey (121)",
    "_persist_raw_output": "go/internal/enforce.persistFailureEvidence (95)",
    "_project_state_root": "go/internal/enforce.asyncProjectStateRoot (120)",
    "_read_pending": "go/internal/enforce.readFailurePending (94)",
    "_record_diagnostic": "go/internal/enforce.writeFailureDiagnostic (121)",
    "_release_async_lease": "go/internal/enforce.asyncFailureCleanupConsumed (120)",
    "_release_scope_lease": "go/internal/enforce.leaseRelease (95)",
    "_remove_pending": "go/internal/enforce.cleanupFailureTail (95)",
    "_safe_error": "go/internal/enforce.failureErrorText (95)",
    "_state_root": "go/internal/enforce.PostToolUseFailure (122)",
    "_telemetry_envelope": "go/internal/enforce.PostToolUseFailure (122)",
    "_undelivered_marker": "go/internal/enforce.failureUndelivered (121)",
    "_numeric_duration_ms": "go/internal/enforce.telemetryDuration (95)",
    "post_tool_use": "go/internal/enforce.PostToolUseFailure (122)",
    "_append_dispatch_receipt": "go/internal/enforce.PostToolUseFailure (122)",
    "_append_pending_guidance": "go/internal/enforce.PostToolUseFailure (122)",
    "_append_project_job_status": "go/internal/enforce.PostToolUseFailure (122)",
    "_append_worktree_apply_context": "go/internal/enforce.PostToolUseFailure (122)",
    "_apply_out_of_scope_result": "go/internal/enforce.PostToolUseFailure (122)",
    "_async_continuation_turn_count": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_build_audit_receipt": "go/internal/enforce.PostToolUseFailure (122)",
    "_canonical_unknown_validation": "go/internal/enforce.PostToolUseFailure (122)",
    "_changed_paths_summary": "go/internal/enforce.PostToolUseFailure (122)",
    "_codex_quota_system_message": "go/internal/enforce.PostToolUseFailure (122)",
    "_codex_response_is_error": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_codex_usage": "go/internal/enforce.PostToolUseFailure (122)",
    "_codex_version_dispatch_context": "go/internal/enforce.PostToolUseFailure (122)",
    "_compact_bundle": "go/internal/enforce.PostToolUseFailure (122)",
    "_concurrent_writer_message": "go/internal/enforce.PostToolUseFailure (122)",
    "_copy_findings": "go/internal/enforce.PostToolUseFailure (122)",
    "_decoded_codex_objects": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_extract_codex_result": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_finalize_replacement": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_finding": "go/internal/enforce.PostToolUseFailure (122)",
    "_implementer_provider": "go/internal/enforce.PostToolUseFailure (122)",
    "_internal_post_response": "go/internal/enforce.PostToolUseFailure (122)",
    "_load_config": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_non_report_bytes": "go/internal/enforce.PostToolUseFailure (122)",
    "_parse_worker_validation": "go/internal/enforce.PostToolUseFailure (122)",
    "_pending_candidates": "go/internal/enforce.PostToolUseFailure (122)",
    "_plan_summary": "go/internal/enforce.PostToolUseFailure (122)",
    "_post_observations": "go/internal/enforce.PostToolUseFailure (122)",
    "_post_start": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_project_job_board_details": "go/internal/enforce.PostToolUseFailure (122)",
    "_record_thread_root": "go/internal/enforce.PostToolUseFailure (122)",
    "_replacement": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_repo_root": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_report_limit": "go/internal/enforce.PostToolUseFailure (122)",
    "_resolve_job_conflicts": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_restore_checks": "go/internal/enforce.PostToolUseFailure (122)",
    "_reviewer1_scope": "go/internal/enforce.PostToolUseFailure (122)",
    "_reviewer_available": "go/internal/enforce.PostToolUseFailure (122)",
    "_reviewer_findings_with_evidence": "go/internal/enforce.PostToolUseFailure (122)",
    "_route_duration_percentiles": "go/internal/enforce.PostToolUseFailure (122)",
    "_route_eta_minutes": "go/internal/enforce.PostToolUseFailure (122)",
    "_start_value": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_store_plan": "go/internal/enforce.PostToolUseFailure (122)",
    "_thread_index_path": "go/internal/enforce.PostToolUseFailure (122)",
    "overlapping_owners": "go/internal/lease.OverlappingOwners (85, 85c)",
    "Path": "go/internal/enforce.PostToolUseFailure (122)",
    "_async_ledger_rejection": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "_async_rejected_response": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "checks.run_checks": "go/internal/enforce.PostToolUseFailure (122)",
    "checks.select_checks": "go/internal/enforce.PostToolUseFailure (122)",
    "coder_config.load": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "codex_quota.read_quota": "go/internal/enforce.PostToolUseFailure (122)",
    "compaction_advisor.pending_guidance": "go/internal/enforce.PostToolUseFailure (122)",
    "compaction_advisor.queue_dispatch": "go/internal/enforce.PostToolUseFailure (122)",
    "compaction_advisor.queue_receipt": "go/internal/enforce.PostToolUseFailure (122)",
    "compaction_advisor.refresh_dispatch_board": "go/internal/enforce.PostToolUseFailure (122)",
    "datetime.datetime.fromisoformat": "go/internal/enforce.PostToolUseFailure (122)",
    "datetime.datetime.now": "go/internal/enforce.PostToolUseFailure (122)",
    "hashlib.sha256": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "inspect.signature": "go/internal/enforce.PostToolUseFailure (122)",
    "json.loads": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "json_transport.dumps": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "ledger.ScopeLeases": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.SessionLedger": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger._scopes_overlap": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.attribute_known_concurrent_scopes": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.build_session_record": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.capture_baseline": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.classify_concurrent_writers": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.compute_delta": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.compute_job_delta": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.detect_out_of_scope_modifications": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.detect_protected_paths": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.filter_job_delta": "go/internal/enforce.PostToolUseFailure (122)",
    "ledger.prepare_job_replay": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "math.ceil": "go/internal/enforce.PostToolUseFailure (122)",
    "math.isfinite": "go/internal/enforce.PostToolUseFailure (122)",
    "model_version_notice.newer_version": "go/internal/enforce.PostToolUseFailure (122)",
    "model_version_notice.read_state": "go/internal/enforce.PostToolUseFailure (122)",
    "model_version_notice.write_state": "go/internal/enforce.PostToolUseFailure (122)",
    "os.fsync": "go/internal/enforce.PostToolUseFailure (122)",
    "os.replace": "go/internal/enforce.PostToolUseFailure (122)",
    "pathlib.Path.home": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.ReviewerReport": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.SkipReviewerDecision": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.claude_review_mode": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.model_family": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.reviewer1_gate": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.validate_config": "go/internal/enforce.PostToolUseFailure (122)",
    "policy.vendor_family": "go/internal/enforce.PostToolUseFailure (122)",
    "project_profile.Check": "go/internal/enforce.PostToolUseFailure (122)",
    "project_root.project_key": "go/internal/enforce.PostToolUseFailure (122)",
    "project_root.resolve_project_root": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "re.compile": "go/internal/enforce.PostToolUseFailure (122)",
    "re.escape": "go/internal/enforce.PostToolUseFailure (122)",
    "re.fullmatch": "go/internal/enforce.PostToolUseFailure (122)",
    "re.finditer": "go/internal/enforce.postPlanSummary (122)",
    "re.search": "go/internal/enforce.PostToolUseFailure (122)",
    "re.sub": "go/internal/enforce.PostToolUseFailure (122)",
    "shutil.which": "go/internal/enforce.PostToolUseFailure (122)",
    "socket.create_connection": "go/internal/enforce.PostToolUseFailure (122)",
    "state_root.default_state_root": "go/internal/enforce.PostToolUseFailure (122)",
    "subprocess.run": "go/internal/enforce.PostToolUseFailure (122)",
    "telemetry._raw_events": "go/internal/enforce.PostToolUseFailure (122)",
    "telemetry._root": "go/internal/enforce.PostToolUseFailure (122)",
    "telemetry.record_health": "go/internal/enforce.PostToolUseFailure (122)",
    "telemetry_schema.normalize_identifier": "go/internal/enforce.PostToolUseFailure (122)",
    "tempfile.NamedTemporaryFile": "go/internal/enforce.PostToolUseFailure (122)",
    "threading.Thread": "go/internal/enforce.PostToolUseFailure (122)",
    "time.monotonic": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "time.time": "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)",
    "uuid.uuid4": "go/internal/enforce.PostToolUseFailure (122)",
}

for _name in "append astimezone date decode difference duration_hint encode endswith expanduser extend fileno find flock flush fromkeys fullmatch get glob group hexdigest is_alive is_file isoformat items join keys load locking lower lstrip mkdir open pop read read_text record_conflicts record_sample recv release replace resolve rstrip save seek sendall set_retention_after setdefault split splitlines start start_shadow startswith stat strftime strip timestamp unlink update utcoffset values with_name with_suffix write write_text __enter__ __exit__".split():
    FAILURE_CALL_CONTRACTS[_name] = "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)"
for _name in "AsyncAdoptionRequired ValueError WiringError all any bool dict enumerate float getattr int interrupted isinstance len list map max min preview reject repair_callable resume reviewer_callable round set sorted str tuple type".split():
    FAILURE_CALL_CONTRACTS[_name] = "go/internal/enforce.PostToolUseFailureAsyncFollowup (120)"


def _reachable(tree):
    functions = {node.name: node for node in tree.body
                 if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    reached, todo = set(), ["post_tool_use_failure"]
    while todo:
        name = todo.pop()
        if name in reached or name not in functions:
            continue
        reached.add(name)
        todo.extend(node.func.id for node in ast.walk(functions[name])
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id in functions)
    return functions, reached


def test_failure_call_diff_is_mapped_and_go_targets_exist():
    tree = ast.parse((PACKAGE / "codex.py").read_text(encoding="utf-8"))
    functions, reached = _reachable(tree)
    calls = [node for name in reached for node in ast.walk(functions[name]) if isinstance(node, ast.Call)]
    callees = {node.func.id for node in calls if isinstance(node.func, ast.Name)}
    aliases = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            aliases.update({item.asname or item.name.split(".")[0]: item.name for item in node.names})
        elif isinstance(node, ast.ImportFrom):
            aliases.update({item.asname or item.name: f"{node.module}.{item.name}" for item in node.names})
    callees.update(f"{aliases[node.func.value.id]}.{node.func.attr}" for node in calls
                   if isinstance(node.func, ast.Attribute) and isinstance(node.func.value, ast.Name)
                   and node.func.value.id in aliases)
    callees.update(node.func.attr for node in calls if isinstance(node.func, ast.Attribute)
                   and not (isinstance(node.func.value, ast.Name) and node.func.value.id in aliases))
    assert callees <= set(FAILURE_CALL_CONTRACTS), f"unmapped Python callees: {sorted(callees - set(FAILURE_CALL_CONTRACTS))}"
    for target in FAILURE_CALL_CONTRACTS.values():
        assert not target.startswith("PENDING:"), target
        path, symbol, _pass = re.fullmatch(
            r"(go/[^.]+(?:/[^.]+)*)\.([A-Za-z_][A-Za-z_0-9]*) \(([^)]+)\)", target
        ).groups()
        files = (ROOT / path).glob("*.go")
        assert any(re.search(rf"\bfunc\s+(?:\([^)]*\)\s+)?{re.escape(symbol)}\s*\(",
                              candidate.read_text(encoding="utf-8")) for candidate in files), target


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _async_failure_state(state, repo, name, tool, *, stage="FINISHED", job_state="FAILED", response=None):
    import ledger
    import project_root
    session, origin = "failure-session", "origin-tool-use"
    call = "async failure " + name
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256((session + "\0" + origin).encode()).hexdigest()
    project = project_root.project_key(repo.resolve())
    pending = {"tool_use_id": origin, "job_id": job_id, "async_call": call,
               "session_id": session, "project_key": project, "task_id": name,
               "baseline": {}, "route": "normal", "model": "gpt-6.1-sol", "effort": "medium",
               "lease_owner": call_key, "prior_thread_id": "prior-thread", "repo_root": str(repo)}
    index = {"jobId": job_id, "call": call, "call_key": call_key, "session_id": session,
             "project_key": project, "stage": stage}
    if response is not None:
        index["response"] = response
    job = {"jobId": job_id, "call": call, "state": job_state, "errorCode": "ASYNC_TERMINAL_FAILURE",
           "threadId": "thread-fixture", "acceptedAt": 10.0, "startedAt": 11.0}
    if job_state != "RUNNING":
        job["finishedAt"] = 12.0
    _write(state / "async_jobs" / f"{job_id}.json", index)
    _write(state / "pending" / f"{call_key}.json", pending)
    _write(state / "codex_exec_mcp" / "jobs" / f"{job_id}.json", job)
    ledger.ScopeLeases(state / "projects" / project).acquire(call_key, name, ["src"])
    return {"hook_event_name": "PostToolUseFailure", "session_id": session, "cwd": str(repo),
            "tool_name": tool, "tool_use_id": name + "-tool", "tool_input": {"jobId": job_id},
            "error": "worker failed", "is_interrupt": False, "duration_ms": 1200}, job_id


def _normalize(value, state, repo):
    if isinstance(value, str):
        if re.fullmatch(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", value):
            return "__UUID__"
        value = value.replace(str(state), "__STATE__").replace(str(repo), "__REPO__")
        return re.sub(r"(projects[\\/])[0-9a-f]{16}([\\/]code-map)", r"\1__PROJECT_KEY__\2", value)
    if isinstance(value, list):
        return [_normalize(item, state, repo) for item in value]
    if isinstance(value, dict):
        return {key: ("__PROJECT_KEY__" if key == "project_key" else
                      "__HEAD__" if key == "head" else
                      "__ASYNC_INPUT_HASH__" if key == "async_input_hash" else _normalize(item, state, repo))
                for key, item in value.items()}
    return value


def _case_view(name, result, state, repo, job_id=None):
    response = json.loads(result.stdout)
    output = response.get("hookSpecificOutput", {})
    evidence_files = list((state / "raw_tool_outputs").glob("*-failure.json"))
    evidence = json.loads(evidence_files[0].read_text(encoding="utf-8")) if evidence_files else None
    indexes = list((state / "async_jobs").glob("*.json"))
    index = json.loads(indexes[0].read_text(encoding="utf-8")) if indexes else None
    return _normalize({"name": name, "hook_event_name": output.get("hookEventName"),
            "response": response,
            "additional_context": output.get("additionalContext", "").replace(str(state), "__STATE__"),
            "evidence": evidence, "pending_remaining": bool(list((state / "pending").glob("*.json"))),
            "marker_remaining": bool(list((state / "pending").glob("undelivered-*.json"))),
            "async_stage": index.get("stage") if index else None,
            "async_job_exists": (state / "codex_exec_mcp" / "jobs" / f"{job_id}.json").exists() if job_id else None}, state, repo)


def test_python_failure_process_oracles(repo, tmp_path):
    import codex
    shared = json.loads(SHARED_FIXTURE.read_text(encoding="utf-8"))
    shared_names = {case["name"] for case in shared["cases"]}
    cases = []

    def run(event_name, payload, state):
        return run_contract_hook(event_name, payload, state, entrypoint="enforcement")

    def sync_case(name, *, interrupt=False, bypass=False, marker=False):
        state = tmp_path / name
        call_id = "failure-" + name
        start = event("PreToolUse", repo, call_id, codex.CODEX_TOOL,
                      tool_input={"label": call_id,
                                  "prompt": "ORCH|task=" + call_id + "|phase=implementation|route=normal|reason=new_task|basis=failure oracle|scope=changed.py\nChange the value."})
        state.mkdir()
        if bypass:
            key = codex._call_key(start)
            bypass_path = codex._bypass_path(state, start)
            _write(bypass_path, {"tool_use_id": call_id, "kind": "pretool_bypass",
                "project_key": codex.project_root.project_key(repo.resolve()),
                "task_id": call_id, "route": "normal", "model": "gpt-6.1-sol",
                "effort": "medium", "phase": "implementation", "scope": ["changed.py"],
                "reason": "new_task", "reason_source": "explicit", "label": call_id})
        else:
            pre_result = run("PreToolUse", start, state)
            assert pre_result.returncode == 0
        if marker:
            key = codex._call_key(start)
            marker_path = state / "pending" / f"undelivered-{key}.json"
            _write(marker_path, {"schema": "purser.undelivered.v1", "call_key": key,
                                 "tool_name": codex.CODEX_TOOL, "reason": "adapter delivery failed"})
        failed = dict(start, hook_event_name="PostToolUseFailure", error="worker failed",
                      is_interrupt=interrupt, duration_ms=1234)
        result = run("PostToolUseFailure", failed, state)
        cases.append(_case_view(name, result, state, repo))

    sync_case("sync_failure")
    sync_case("sync_interrupt", interrupt=True)
    sync_case("bypass_failure", bypass=True)
    sync_case("undelivered_marker_cleanup", marker=True)

    for name, live in (("failed_start_no_job", False), ("failed_start_live_job_rejected", True)):
        state = tmp_path / name
        state.mkdir()
        call_id = "failure-" + name
        start = event("PreToolUse", repo, call_id, sorted(codex.CODEX_START_TOOL_NAMES)[0],
                      tool_input={"label": call_id,
                                  "prompt": "ORCH|task=" + call_id + "|phase=implementation|route=normal|reason=new_task|basis=failure oracle|scope=changed.py\nChange the value."})
        pre_result = run("PreToolUse", start, state)
        assert pre_result.returncode == 0
        pending_path = codex._pending_path(state, start)
        pending = json.loads(pending_path.read_text(encoding="utf-8"))
        pending["async_call"] = "async-call-" + name
        pending["job_id"] = hashlib.sha256(pending["async_call"].encode()).hexdigest()
        _write(pending_path, pending)
        job_path = state / "codex_exec_mcp" / "jobs" / f"{pending['job_id']}.json"
        if live:
            _write(job_path, {"jobId": pending["job_id"], "call": pending["async_call"], "state": "RUNNING"})
        failed = dict(start, hook_event_name="PostToolUseFailure", error="start failed",
                      is_interrupt=False, duration_ms=1234)
        result = run("PostToolUseFailure", failed, state)
        cases.append(_case_view(name, result, state, repo, pending["job_id"]))

    for name, tool in (("codex_result_failure_claimed", sorted(codex.CODEX_RESULT_TOOL_NAMES)[0]),
                       ("codex_cancel_failure", sorted(codex.CODEX_CANCEL_TOOL_NAMES)[0])):
        state = tmp_path / name
        state.mkdir()
        failed, job_id = _async_failure_state(state, repo, name, tool)
        result = run("PostToolUseFailure", failed, state)
        cases.append(_case_view(name, result, state, repo, job_id))

    followup_cases = []
    scenarios = (
            ("result_finished", sorted(codex.CODEX_RESULT_TOOL_NAMES)[0], "FINISHED", "FAILED", None, True),
            ("result_consumed", sorted(codex.CODEX_RESULT_TOOL_NAMES)[0], "CONSUMED", "FAILED",
             {"hookSpecificOutput": {"hookEventName": "PostToolUse", "updatedToolOutput": [
                 {"type": "text", "text": "{\"status\":\"cached\"}"}]}}, True),
            ("result_finalizing_recovery", sorted(codex.CODEX_RESULT_TOOL_NAMES)[0], "FINALIZING", "FAILED", None, True),
            ("cancel_running", sorted(codex.CODEX_CANCEL_TOOL_NAMES)[0], "FINISHED", "RUNNING", None, True),
            ("cancel_absent_job", sorted(codex.CODEX_CANCEL_TOOL_NAMES)[0], "FINISHED", "RUNNING", None, False),
            ("claim_lock_contention", sorted(codex.CODEX_RESULT_TOOL_NAMES)[0], "FINISHED", "FAILED", None, True),
    )
    for name, tool, stage, job_state, cached, job_exists in scenarios:
        state = tmp_path / ("followup-" + name)
        state.mkdir()
        failed, job_id = _async_failure_state(state, repo, name, tool, stage=stage,
                                               job_state=job_state, response=cached)
        if not job_exists:
            (state / "codex_exec_mcp" / "jobs" / f"{job_id}.json").unlink()
        index_path = state / "async_jobs" / f"{job_id}.json"
        index = json.loads(index_path.read_text(encoding="utf-8"))
        pending_path = state / "pending" / f"{index['call_key']}.json"
        pending = json.loads(pending_path.read_text(encoding="utf-8"))
        initial = {"event": failed, "index": index, "pending": pending,
                   "job": (json.loads((state / "codex_exec_mcp" / "jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
                           if job_exists else None)}
        if stage == "FINALIZING":
            evidence = {"ambiguous_outcome": True, "work_performed": "UNKNOWN",
                        "error": failed["error"], "is_interrupt": failed["is_interrupt"],
                        "duration_ms": failed["duration_ms"], "pending_call": pending}
            codex._persist_raw_output(state, dict(failed, tool_name="mcp__codex-main__codex",
                                                   tool_use_id=pending["tool_use_id"]), evidence, "-failure")
        if name == "claim_lock_contention":
            with codex._async_claim(index_path):
                result = run("PostToolUseFailure", failed, state)
        else:
            result = run("PostToolUseFailure", failed, state)
        captured = _case_view(name, result, state, repo, job_id)
        captured["async_initial"] = _normalize(initial, state, repo)
        followup_cases.append(captured)
    cases.extend(followup_cases)

    expected = {"oracle": "Python PostToolUseFailure real hook process behavior", "cases": cases,
                "shared_finalizer_reuse": sorted(shared_names & {
                    "sync_failure", "sync_interrupt", "lease_release_failure",
                    "pending_removal_failure", "async_job_record_removal", "not_enforced_note"})}
    if (os.environ.get("FAILURE_PROCESS_WRITE_FIXTURE") == "1"
            or os.environ.get("FAILURE_FOLLOWUP_WRITE_FIXTURE") == "1"):
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set FAILURE_PROCESS_WRITE_FIXTURE=1 to capture)"
    saved = json.loads(FIXTURE.read_text(encoding="utf-8"))
    if saved != expected:
        differences = []
        def changed_paths(left, right, prefix=""):
            if isinstance(left, dict) and isinstance(right, dict):
                return [item for key in left.keys() | right.keys()
                        for item in changed_paths(left.get(key), right.get(key), f"{prefix}.{key}")]
            if isinstance(left, list) and isinstance(right, list):
                return [item for i, (a, b) in enumerate(zip(left, right))
                        for item in changed_paths(a, b, f"{prefix}[{i}]")]
            return [] if left == right else [f"{prefix}: {left!r} != {right!r}"]
        for old, new in zip(saved.get("cases", []), expected["cases"]):
            keys = [key for key in old if old.get(key) != new.get(key)]
            if keys:
                differences.append({"case": new.get("name"), "details": changed_paths(old, new)[:12]})
        assert saved == expected, differences
