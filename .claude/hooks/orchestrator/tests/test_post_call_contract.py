"""PostToolUse migration map and process oracle for Go assembly passes."""
import ast
import hashlib
import json
import os
from pathlib import Path
import sys

from test_enforcement_process_contract import event, process, repo  # noqa: F401 - shared fixtures

PACKAGE = Path(__file__).resolve().parents[1]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "post_process.json"
TOOL = "mcp__codex-main__codex"

# Go symbols carry their implementation pass. PENDING entries identify the
# first assembly pass that still owns the Python behavior.
POST_CALL_CONTRACTS = {
    "_append_dispatch_receipt": "PENDING:116", "_append_pending_guidance": "PENDING:116",
    "_append_project_job_status": "PENDING:116", "_append_worktree_apply_context": "PENDING:116",
    "_post_start": "PENDING:110", "_async_post_followup": "PENDING:111",
    "_apply_out_of_scope_result": "go/internal/enforce.applyOutOfScopeResult (81)", "_async_claim": "PENDING:111",
    "_async_continuation_turn_count": "PENDING:111", "_async_duration": "PENDING:111",
    "_async_job": "PENDING:111", "_async_ledger_rejection": "PENDING:112",
    "_async_owned": "PENDING:111", "_async_rejected_response": "PENDING:112",
    "_atomic_json_write": "PENDING:113", "_base_bundle": "go/internal/enforce.baseBundle (pre-tranche-5)",
    "_build_audit_receipt": "PENDING:115", "_bypass_path": "go/internal/enforce.postIntake (93)",
    "_call_key": "go/internal/enforce.callKey (pre-tranche-5)", "_cancel_start_job": "PENDING:111",
    "_canonical_unknown_validation": "go/internal/enforce.canonicalUnknownValidation (86)",
    "_changed_paths_summary": "PENDING:114", "_cleanup_stale_pending": "PENDING:113",
    "_codex_quota_system_message": "PENDING:116", "_codex_response_is_error": "PENDING:111",
    "_codex_usage": "PENDING:116", "_codex_version_dispatch_context": "PENDING:116",
    "_compact_bundle": "PENDING:116", "_concurrent_writer_message": "PENDING:114",
    "_copy_findings": "PENDING:114", "_decoded_codex_objects": "go/internal/enforce.decodedCodexObjects (pre-tranche-5)",
    "_extract_codex_result": "go/internal/enforce.decodeCodexResult (pre-tranche-5)", "_finalize_replacement": "PENDING:116",
    "_finding": "PENDING:108", "_implementer_provider": "PENDING:113",
    "_internal_post_response": "go/internal/enforce.PostInternalError (pre-tranche-5)",
    "_load_config": "PENDING:113", "_non_report_bytes": "PENDING:114",
    "_numeric_duration_ms": "PENDING:116", "_observe": "PENDING:116",
    "_observe_undelivered": "go/internal/enforce.postIntake (93)", "_parse_worker_validation": "go/internal/enforce.parseWorkerValidation (86)",
    "_pending_candidates": "go/internal/enforce.postIntake (93)", "_pending_path": "PENDING:113",
    "_pending_project_key": "PENDING:113", "_persist_raw_output": "go/internal/enforce.persistRawOutput (pre-tranche-5)",
    "_plan_summary": "PENDING:116", "_post_observations": "PENDING:116",
    "_project_job_board_details": "PENDING:116", "_project_state_root": "PENDING:113",
    "_read_pending": "go/internal/enforce.postIntake (93)", "_record_diagnostic": "PENDING:116",
    "_record_thread_root": "PENDING:116", "_release_async_lease": "PENDING:110",
    "_release_scope_lease": "PENDING:110", "_remove_pending": "PENDING:113",
    "_replacement": "go/internal/enforce.replacement (pre-tranche-5)", "_repo_root": "PENDING:113",
    "_report_limit": "PENDING:114", "_resolve_job_conflicts": "PENDING:112",
    "_restore_checks": "PENDING:105", "_reviewer1_scope": "PENDING:115",
    "_reviewer_available": "PENDING:115", "_reviewer_findings_with_evidence": "PENDING:115",
    "_route_duration_percentiles": "PENDING:116", "_route_eta_minutes": "PENDING:116",
    "_safe_error": "PENDING:108", "_start_job_terminal_or_absent": "PENDING:111",
    "_start_value": "PENDING:111", "_state_root": "PENDING:113",
    "_store_plan": "PENDING:116", "_telemetry_envelope": "PENDING:116",
    "_thread_index_path": "PENDING:113", "_undelivered_marker": "go/internal/enforce.postIntake (93)",
    "post_tool_use": "PENDING:116", "post_tool_use_failure": "PENDING:118",
}

POST_IMPORTED_CALLS = {
    "ledger.compute_delta": "go/internal/ledger.ComputeDelta (77)",
    "ledger.detect_out_of_scope_modifications": "go/internal/ledger.DetectOutOfScopeModifications (78)",
    "ledger.attribute_known_concurrent_scopes": "go/internal/ledger.AttributeKnownConcurrentScopes (79)",
    "ledger.detect_protected_paths": "go/internal/ledger.DetectProtectedPaths (79)",
    "ledger.classify_concurrent_writers": "go/internal/ledger.ClassifyConcurrentWriters (80)",
    "ledger.compute_job_delta": "go/internal/ledger.ComputeJobDelta (82)",
    "ledger.filter_job_delta": "go/internal/ledger.FilterJobDelta (82)",
    "ledger.prepare_job_replay": "go/internal/ledger.PrepareJobReplay (83b, 84)",
    "checks.select_checks": "go/internal/review.SelectChecks (90)",
    "checks.run_checks": "go/internal/review.RunChecks (90, 91)",
}
POST_IMPORTED_CALLS.update({name: "PENDING:116" for name in "coder_config.load codex_quota.read_quota compaction_advisor.pending_guidance compaction_advisor.queue_dispatch compaction_advisor.queue_receipt compaction_advisor.refresh_dispatch_board datetime.datetime.fromisoformat datetime.datetime.now hashlib.sha256 inspect.signature json.loads json_transport.dumps ledger.ScopeLeases ledger.SessionLedger ledger._scopes_overlap ledger.build_session_record ledger.capture_baseline math.ceil math.isfinite model_version_notice.newer_version model_version_notice.read_state model_version_notice.write_state os.fsync os.replace pathlib.Path.home policy.ReviewerReport policy.SkipReviewerDecision policy.claude_review_mode policy.model_family policy.reviewer1_gate policy.validate_config policy.vendor_family project_profile.Check project_root.project_key project_root.resolve_project_root re.compile re.escape re.fullmatch re.search re.sub shutil.which socket.create_connection state_root.default_state_root subprocess.run telemetry._raw_events telemetry._root telemetry.record_health telemetry_schema.normalize_identifier tempfile.NamedTemporaryFile threading.Thread time.monotonic time.time uuid.uuid4".split()})
POST_METHOD_CALLS = {name: "PENDING:113" for name in "append astimezone date decode difference duration_hint encode endswith expanduser extend fileno find flock flush fromkeys fullmatch get glob group hexdigest is_alive is_file isoformat items join keys load locking lower lstrip mkdir open overlapping_owners pop read read_text record_conflicts record_sample recv release replace resolve rstrip save seek sendall set_retention_after setdefault split splitlines start start_shadow startswith stat strftime strip timestamp unlink update utcoffset values with_name with_suffix write write_text __enter__ __exit__".split()}
POST_METHOD_CALLS.update({"overlapping_owners": "go/internal/lease.Store.OverlappingOwners (85, 85c)",
                          "set_retention_after": "go/internal/lease.Store.SetRetentionAfter (85)"})
POST_BUILTIN_CALLS = {name: "PENDING:114" for name in "all any bool dict enumerate float getattr int isinstance len list map max min round set sorted str tuple type ValueError WiringError AsyncAdoptionRequired interrupted preview reject repair_callable resume reviewer_callable".split()}
POST_EXCLUSIONS = {"reviewer_bridge": "reviewer seam remains Python until bridge adapter assembly (115)"}


def _reachable_post(tree):
    functions = {node.name: node for node in tree.body
                 if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    reached, todo = set(), ["post_tool_use", "_post_start", "_async_post_followup"]
    while todo:
        name = todo.pop()
        if name in reached or name not in functions:
            continue
        reached.add(name)
        todo.extend(node.func.id for node in ast.walk(functions[name])
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id in functions)
    return functions, reached


def test_post_call_diff_is_mapped():
    tree = ast.parse((PACKAGE / "codex.py").read_text(encoding="utf-8"))
    functions, reached = _reachable_post(tree)
    calls = [node for name in reached for node in ast.walk(functions[name]) if isinstance(node, ast.Call)]
    internal = {node.func.id for node in calls if isinstance(node.func, ast.Name) and node.func.id in functions}
    assert internal <= set(POST_CALL_CONTRACTS) | set(POST_EXCLUSIONS), f"unmapped Python callees: {sorted(internal - set(POST_CALL_CONTRACTS) - set(POST_EXCLUSIONS))}"
    aliases = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            aliases.update({item.asname or item.name.split(".")[0]: item.name for item in node.names})
        elif isinstance(node, ast.ImportFrom):
            aliases.update({item.asname or item.name: f"{node.module}.{item.name}" for item in node.names})
    imported = {f"{aliases[node.func.value.id]}.{node.func.attr}" for node in calls
                if isinstance(node.func, ast.Attribute) and isinstance(node.func.value, ast.Name)
                and node.func.value.id in aliases}
    assert imported <= set(POST_IMPORTED_CALLS), f"unmapped imported calls: {sorted(imported - set(POST_IMPORTED_CALLS))}"
    methods = {node.func.attr for node in calls if isinstance(node.func, ast.Attribute)
               and not (isinstance(node.func.value, ast.Name) and node.func.value.id in aliases)}
    assert methods <= set(POST_METHOD_CALLS), f"unmapped method calls: {sorted(methods - set(POST_METHOD_CALLS))}"
    bare = {node.func.id for node in calls if isinstance(node.func, ast.Name)
            and node.func.id not in functions and node.func.id not in aliases}
    assert bare <= set(POST_BUILTIN_CALLS), f"unmapped builtin/nested calls: {sorted(bare - set(POST_BUILTIN_CALLS))}"


def test_post_contract_targets_are_ported_or_pending():
    for target in (*POST_CALL_CONTRACTS.values(), *POST_IMPORTED_CALLS.values(),
                   *POST_METHOD_CALLS.values(), *POST_BUILTIN_CALLS.values()):
        if target.startswith("PENDING:"):
            assert 93 <= int(target.split(":", 1)[1]) <= 123, target
        else:
            assert target.startswith("go/") and target.endswith(")") and " (" in target, target


def _post_bundle(result):
    response = json.loads(result.stdout)
    specific = response["hookSpecificOutput"]
    return response, json.loads(specific["updatedToolOutput"][0]["text"])


def _oracle(name, response, bundle, state, pending=None, repo_path=None):
    project = pending.get("project_key") if pending else None
    leases = state / "projects" / project / "scope_leases.json" if project else None
    owners = json.loads(leases.read_text(encoding="utf-8"))["owners"] if leases and leases.exists() else {}
    return {"name": name, "bundle": {key: bundle.get(key) for key in
            ("task_id", "phase", "routing_enforced", "apply", "changed_paths", "delta_lines", "delta_files", "claude_review_mode")},
            "findings": sorted(item["code"] for item in bundle.get("findings", [])),
            "guidance_has_codex_apply": "codex_apply" in response.get("systemMessage", ""),
            "pending_remaining": bool(list((state / "pending").glob("*.json"))),
            "lease_states": sorted(owner["state"] for owner in owners.values())}


def test_python_post_process_oracle(repo, process):
    import codex
    import ledger
    state, run = process
    scenarios = []
    def call(tool_id, *, tool=TOOL, scope="changed.py", prompt=None, response=None):
        request = event("PreToolUse", repo, tool_id, tool,
                        tool_input={"label": tool_id, "prompt": prompt or
                            f"ORCH|task={tool_id}|phase=implementation|route=normal|reason=new_task|basis=post oracle|scope={scope}\nUpdate the file."})
        if response is None:
            assert "updatedInput" in json.loads(run("PreToolUse", request).stdout)["hookSpecificOutput"]
        post = dict(request, hook_event_name="PostToolUse", tool_response=response or
                    {"structuredContent": {"threadId": "post-thread", "content": "Done."}})
        return request, post

    # No matching routed pre-call models the fail-open/bypass Post path.
    bypass_pre = event("PreToolUse", repo, "post-bypass", tool_input={"label": "post-bypass",
        "prompt": "ORCH|task=post-bypass|phase=implementation|route=normal|reason=new_task|basis=post oracle|scope=changed.py\nUpdate the file."})
    run("PreToolUse", bypass_pre, "config-error")
    bypass = dict(bypass_pre, hook_event_name="PostToolUse", tool_response={"structuredContent": {"threadId": "post-thread", "content": "Done."}})
    response, bundle = _post_bundle(run("PostToolUse", bypass))
    scenarios.append(_oracle("bypass_unrouted", response, bundle, state))

    request, post = call("post-marker")
    pending = json.loads(codex._pending_path(state, request).read_text(encoding="utf-8"))
    marker = state / "pending" / f"undelivered-{codex._call_key(request)}.json"
    marker.write_text(json.dumps({"schema": "purser.undelivered.v1", "call_key": codex._call_key(request),
                                  "tool_name": request["tool_name"], "reason": "rewrite undelivered"}), encoding="utf-8")
    response, bundle = _post_bundle(run("PostToolUse", post))
    scenarios.append(_oracle("undelivered_marker", response, bundle, state, pending))

    request, post = call("post-no-delta")
    pending = json.loads(codex._pending_path(state, request).read_text(encoding="utf-8"))
    response, bundle = _post_bundle(run("PostToolUse", post, "reviewer-ok"))
    scenarios.append(_oracle("sync_no_delta", response, bundle, state, pending))

    request, post = call("post-in-scope")
    pending = json.loads(codex._pending_path(state, request).read_text(encoding="utf-8"))
    (repo / "changed.py").write_text("value = 2\n", encoding="utf-8")
    response, bundle = _post_bundle(run("PostToolUse", post, "reviewer-ok"))
    scenarios.append(_oracle("sync_in_scope_delta", response, bundle, state, pending))
    (repo / "changed.py").write_text("value = 1\n", encoding="utf-8")

    request, post = call("post-outside")
    pending = json.loads(codex._pending_path(state, request).read_text(encoding="utf-8"))
    (repo / "outside.py").write_text("value = 9\n", encoding="utf-8")
    response, bundle = _post_bundle(run("PostToolUse", post, "reviewer-ok"))
    scenarios.append(_oracle("out_of_scope", response, bundle, state, pending))
    (repo / "outside.py").unlink()

    # Start a real async worktree job; the finished Post result must retain apply guidance.
    start_tool, result_tool = sorted(codex.CODEX_START_TOOL_NAMES)[0], sorted(codex.CODEX_RESULT_TOOL_NAMES)[0]
    start, _ = call("post-apply", tool=start_tool)
    pending = json.loads(codex._pending_path(state, start).read_text(encoding="utf-8"))
    job_id = hashlib.sha256(pending["async_call"].encode("utf-8")).hexdigest()
    snapshot = ledger.create_job_worktree(repo, job_id, state / "worktrees")
    (Path(snapshot["path"]) / "changed.py").write_text("value = 3\n", encoding="utf-8")
    run("PostToolUse", dict(start, hook_event_name="PostToolUse", tool_response={"structuredContent": {"jobId": job_id, "state": "SUCCEEDED"}}))
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True, exist_ok=True)
    (jobs / f"{job_id}.json").write_text(json.dumps({"jobId": job_id, "call": pending["async_call"],
        "state": "SUCCEEDED", "threadId": "post-thread", "worktreePath": snapshot["path"],
        "snapshot": snapshot["snapshot"], "repoRoot": str(repo), "finishedAt": 1.0}), encoding="utf-8")
    followup = event("PostToolUse", repo, "post-apply-result", result_tool,
                     tool_input={"jobId": job_id}, tool_response={"structuredContent": {
                         "jobId": job_id, "state": "SUCCEEDED", "threadId": "post-thread",
                         "content": "Done.", "worktreePath": snapshot["path"]}})
    response, bundle = _post_bundle(run("PostToolUse", followup, "reviewer-ok"))
    scenarios.append(_oracle("codex_apply_guidance", response, bundle, state, pending))

    expected = {"oracle": "Python PostToolUse process behavior", "cases": scenarios}
    if os.environ.get("PURSER_WRITE_GO_POST_PROCESS_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set PURSER_WRITE_GO_POST_PROCESS_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected
