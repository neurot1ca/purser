import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import codex
import hook
import ledger
import policy


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
    root = Path(__file__).parent / ".hook-test-tmp" / uuid.uuid4().hex
    repo = root / "repo"
    state = root / "state"
    repo.mkdir(parents=True)
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


def save_live_thread(
    state,
    *,
    route="normal",
    model="gpt-5.6-sol",
    effort="medium",
    phase="implementation",
    scope=("file0.py", "file1.py", "file2.py"),
):
    result = ledger.SessionLedger(state).save(
        ledger.build_session_record(
            "task-1",
            "thread-1",
            route,
            model,
            effort,
            phase=phase,
            scope=scope,
        )
    )
    assert result["status"] == ledger.OK


@pytest.mark.parametrize(
    ("prompt", "code"),
    [
        ("Do the work.", policy.MISSING_ORCH_LINE),
        ("ORCH task=task-1\nDo the work.", policy.MALFORMED_ORCH_LINE),
        (orch(task=None) + "\nDo the work.", policy.MISSING_REQUIRED_KEY),
        (orch(phase=None) + "\nDo the work.", policy.MISSING_REQUIRED_KEY),
        (orch(route=None) + "\nDo the work.", policy.MISSING_REQUIRED_KEY),
        (orch(scope=None) + "\nDo the work.", policy.MISSING_REQUIRED_KEY),
        (orch(scope=".") + "\nDo the work.", policy.UNBOUNDED_SCOPE),
    ],
)
def test_protocol_errors_deny_with_policy_reason(workspace, config, prompt, code):
    repo, state = workspace

    result = codex.pre_tool_use(
        event(prompt=prompt), config=config, state_root=state, repo_root=repo
    )

    decision = result["hookSpecificOutput"]
    assert decision["permissionDecision"] == "deny"
    assert decision["permissionDecisionReason"].startswith(code + ":")


@pytest.mark.parametrize(
    ("route", "model", "effort"),
    [
        ("mechanical", "gpt-5.6-luna", "high"),
        ("subtle", "gpt-5.6-luna", "xhigh"),
        ("normal", "gpt-5.6-sol", "medium"),
        ("consequential", "gpt-5.6-sol", "high"),
        ("escalated", "gpt-5.6-sol", "xhigh"),
    ],
)
def test_each_route_sets_exact_model_and_effort(workspace, config, route, model, effort):
    repo, state = workspace

    result = codex.pre_tool_use(
        event(prompt=orch(route) + "\nImplement."),
        config=config,
        state_root=state,
        repo_root=repo,
    )
    updated = result["hookSpecificOutput"]["updatedInput"]

    assert updated["model"] == model
    assert updated["config"]["model_reasoning_effort"] == effort
    assert updated["prompt"] == "Implement."


def test_declared_scope_reaches_baseline_and_operation_skips_manifest(workspace, config):
    repo, state = workspace
    implementation = event(prompt=orch(scope="file0.py,file2.py") + "\nImplement.")

    codex.pre_tool_use(
        implementation, config=config, state_root=state, repo_root=repo
    )
    record = ledger.SessionLedger(state).load("task-1")["record"]
    assert record["pending_call_baseline"]["scope"] == ["file0.py", "file2.py"]
    assert record["pending_call_baseline"]["manifest_enabled"] is True

    operation = event(
        prompt=orch(task="operation-1", phase="operation", scope=None) + "\nInspect.",
        tool_use_id="operation-call",
    )
    codex.pre_tool_use(operation, config=config, state_root=state, repo_root=repo)
    operation_record = ledger.SessionLedger(state).load("operation-1")["record"]
    assert operation_record["pending_call_baseline"]["scope"] == []
    assert operation_record["pending_call_baseline"]["manifest_enabled"] is False


def test_fresh_rewrite_preserves_safety_and_unrelated_fields(workspace, config):
    repo, state = workspace
    original = event(
        sandbox="read-only",
        **{
            "approval-policy": "never",
            "unrelated": {"bytes": [1, 2, 3]},
            "config": {"unrelated_config": "unchanged"},
            "developer-instructions": "caller supplied",
        },
    )

    result = codex.pre_tool_use(
        original, config=config, state_root=state, repo_root=repo
    )
    updated = result["hookSpecificOutput"]["updatedInput"]

    assert updated["sandbox"] == original["tool_input"]["sandbox"]
    assert updated["approval-policy"] == original["tool_input"]["approval-policy"]
    assert updated["unrelated"] == original["tool_input"]["unrelated"]
    assert updated["config"]["unrelated_config"] == "unchanged"
    assert updated["developer-instructions"] == "caller supplied"


def test_fresh_rewrite_adds_standard_independence_text(workspace, config):
    repo, state = workspace

    result = codex.pre_tool_use(
        event(), config=config, state_root=state, repo_root=repo
    )

    updated = result["hookSpecificOutput"]["updatedInput"]
    assert updated["developer-instructions"] == codex.INDEPENDENCE_INSTRUCTIONS
    assert "sandbox" not in updated
    assert "approval-policy" not in updated


def test_compatible_live_thread_denies_fresh_codex(workspace, config):
    repo, state = workspace
    save_live_thread(state)

    result = codex.pre_tool_use(
        event(), config=config, state_root=state, repo_root=repo
    )

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "codex-reply" in reason


def test_route_change_denies_codex_reply_and_requires_fresh_thread(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    reply = event(
        codex.CODEX_REPLY_TOOL,
        prompt=orch("subtle") + "\nContinue.",
        threadId="thread-1",
    )

    result = codex.pre_tool_use(
        reply, config=config, state_root=state, repo_root=repo
    )

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "fresh thread" in reason


def test_fresh_route_change_does_not_relabel_old_thread(workspace, config):
    repo, state = workspace
    save_live_thread(state)

    result = codex.pre_tool_use(
        event(prompt=orch("subtle") + "\nStart fresh."),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    assert "updatedInput" in result["hookSpecificOutput"]
    loaded = ledger.SessionLedger(state).load("task-1")
    assert loaded["record"]["live_thread"] is None
    assert loaded["record"]["pending_call_baseline"] is not None


def test_compatible_reply_only_strips_orch(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    reply = event(
        codex.CODEX_REPLY_TOOL,
        prompt=orch() + "\nContinue.",
        threadId="thread-1",
    )

    result = codex.pre_tool_use(
        reply, config=config, state_root=state, repo_root=repo
    )
    updated = result["hookSpecificOutput"]["updatedInput"]

    assert updated == {"threadId": "thread-1", "prompt": "Continue."}


def test_reply_without_orch_inherits_recorded_thread_context(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    reply = event(
        codex.CODEX_REPLY_TOOL,
        prompt="Continue without repeating routing.",
        threadId="thread-1",
    )

    result = codex.pre_tool_use(
        reply, config=config, state_root=state, repo_root=repo
    )
    output = result["hookSpecificOutput"]

    assert "permissionDecision" not in output
    assert output["updatedInput"] == {
        "threadId": "thread-1",
        "prompt": "Continue without repeating routing.",
    }
    pending = next((state / "pending").glob("*.json"))
    assert json.loads(pending.read_text(encoding="utf-8"))["scope"] == [
        "file0.py",
        "file1.py",
        "file2.py",
    ]


def test_reply_without_orch_without_recoverable_thread_is_denied(workspace, config):
    repo, state = workspace
    reply = event(
        codex.CODEX_REPLY_TOOL,
        prompt="Continue without routing.",
        threadId="missing-thread",
    )

    result = codex.pre_tool_use(
        reply, config=config, state_root=state, repo_root=repo
    )
    output = result["hookSpecificOutput"]

    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"].startswith("NO_LIVE_THREAD:")


def test_pretool_strips_minimal_orch_and_pins_consequential_route(workspace, config):
    repo, state = workspace
    call = event(
        prompt=(
            "ORCH|task=t|phase=implementation|route=consequential|scope=some/path.rs\n"
            "\nbody text"
        ),
        tool_use_id="minimal-consequential",
    )

    result = codex.pre_tool_use(
        call, config=config, state_root=state, repo_root=repo
    )
    output = result["hookSpecificOutput"]

    assert "permissionDecision" not in output
    assert output["updatedInput"]["prompt"] == "\nbody text"
    assert output["updatedInput"]["model"] == "gpt-5.6-sol"
    assert output["updatedInput"]["config"]["model_reasoning_effort"] == "high"


@pytest.mark.parametrize(
    ("tool_response", "expected"),
    [
        (
            [{"type": "text", "text": json.dumps({"threadId": "thread-array", "content": "array"})}],
            ("thread-array", "array"),
        ),
        (
            {"content": [{"type": "text", "text": json.dumps({"threadId": "thread-mapping"})}]},
            ("thread-mapping", ""),
        ),
        (
            json.dumps({"threadId": "thread-string", "content": "string"}),
            ("thread-string", "string"),
        ),
    ],
)
def test_extract_codex_result_accepts_observed_transport_shapes(tool_response, expected):
    assert codex._extract_codex_result(tool_response) == expected


def test_thread_id_survives_mcp_output_replacement(workspace, config):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    report = "completed " + ("x" * 1300)
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-9", "content": report}),
            }
        ],
    }

    result = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo
    )
    bundle = response_bundle(result, config)

    assert bundle["thread_id"] == "thread-9"
    assert len(bundle["implementer_report"]) == codex.IMPLEMENTER_REPORT_LIMIT
    assert Path(bundle["raw_output_path"]).is_file()
    assert "tool_response" not in bundle
    loaded = ledger.SessionLedger(state).load("task-1")
    assert loaded["record"]["live_thread"]["thread_id"] == "thread-9"


def test_posttool_lone_surrogate_is_preserved_without_crashing(workspace, config):
    repo, state = workspace
    call = event(tool_use_id="lone-surrogate")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    report = "before\udc9dafter"
    tool_response = [
        {
            "type": "text",
            "text": '{"threadId":"thread-surrogate","content":"' + report + '"}',
        }
    ]
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": tool_response,
    }

    result = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo
    )
    output_text = result["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"]
    bundle = json.loads(output_text)

    assert bundle["implementer_report"] == report
    assert "\\udc9d" in output_text
    raw_path = Path(bundle["raw_output_path"])
    assert "\\udc9d" in raw_path.read_text(encoding="utf-8")
    assert json.loads(raw_path.read_text(encoding="utf-8")) == tool_response
    loaded = ledger.SessionLedger(state).load("task-1")
    assert loaded["status"] == ledger.OK
    assert loaded["record"]["last_outcome"]["implementer_report"] == report
    assert not (state / "diagnostics.jsonl").exists()


def test_missing_thread_id_is_explicit_and_forces_full(workspace, config):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [{"type": "text", "text": json.dumps({"content": "done"})}],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["thread_id"] is None
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(item["code"] == "THREAD_ID_UNRECOVERABLE" for item in bundle["findings"])


def test_non_codex_tool_passes_through_untouched(workspace):
    repo, state = workspace
    other = event("mcp__other__tool", prompt="no protocol")

    assert codex.pre_tool_use(other, state_root=state, repo_root=repo) == {}
    assert hook.dispatch(other) == {}


def test_internal_pretool_error_allows_unmodified(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()

    def crash(*args, **kwargs):
        raise OSError("baseline unavailable")

    monkeypatch.setattr(codex.ledger, "capture_baseline", crash)

    result = codex.pre_tool_use(
        call, config=config, state_root=state, repo_root=repo
    )

    pre_output = result["hookSpecificOutput"]
    assert "permissionDecision" not in pre_output
    assert "updatedInput" not in pre_output
    assert "NOT enforced" in pre_output["additionalContext"]
    assert (state / "diagnostics.jsonl").is_file()

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }
    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )
    assert bundle["routing_enforced"] is False
    assert bundle["claude_review_mode"] == policy.FULL
    finding = next(
        item for item in bundle["findings"]
        if item["code"] == "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED"
    )
    assert "baseline unavailable" in finding["message"]


def test_bad_config_is_an_internal_pretool_error_not_a_denial(workspace, config):
    repo, state = workspace
    call = event()
    del config["routes"]["normal"]

    result = codex.pre_tool_use(
        call, config=config, state_root=state, repo_root=repo
    )

    output = result["hookSpecificOutput"]
    assert "permissionDecision" not in output
    assert "updatedInput" not in output
    assert "NOT enforced" in output["additionalContext"]

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }
    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )
    assert bundle["routing_enforced"] is False
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(
        item["code"] == "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED"
        and "route" in item["message"]
        for item in bundle["findings"]
    )


def test_missing_tool_use_id_bypass_surfaces_in_matching_post(workspace, config):
    repo, state = workspace
    call = event(tool_use_id=None)

    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    assert "NOT enforced" in pre["hookSpecificOutput"]["additionalContext"]
    post = {
        **call,
        "tool_use_id": "runtime-post-id",
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }
    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["routing_enforced"] is False
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(
        item["code"] == "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED"
        and "tool_use_id is missing" in item["message"]
        for item in bundle["findings"]
    )


def test_manifest_budget_failure_is_named_and_forces_full(workspace, config):
    repo, state = workspace
    config["manifest_budget_seconds"] = 1e-12
    call = event()

    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in pre["hookSpecificOutput"]
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }
    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["reviewer1_skipped_reason"] is None
    assert any(
        item["code"] == ledger.MANIFEST_BUDGET_EXCEEDED
        for item in bundle["findings"]
    )


def test_internal_posttool_error_forces_full(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    def crash(*args, **kwargs):
        raise RuntimeError("delta exploded")

    monkeypatch.setattr(codex.ledger, "compute_delta", crash)
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["claude_review_mode"] == policy.FULL
    assert any("delta exploded" in item["message"] for item in bundle["findings"])


def test_unknown_delta_forces_full_without_skipping_or_calling_reviewer(
    workspace, config, monkeypatch
):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    monkeypatch.setattr(
        codex.ledger,
        "compute_delta",
        lambda *args, **kwargs: {
            "status": ledger.UNKNOWN,
            "code": "GIT_FAILED",
            "message": "delta unavailable",
            "baseline_head": None,
            "current_head": None,
            "changed_paths": None,
        },
    )
    calls = []

    def reviewer(**kwargs):
        calls.append(kwargs)
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE}

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            repo_root=repo,
            reviewer=reviewer,
        ),
        config,
    )

    assert calls == []
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["reviewer1_skipped_reason"] is None


def test_reviewer_gate_receives_ledger_counts_by_keyword(workspace, config, monkeypatch):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    for index in range(3):
        (repo / f"file{index}.py").write_text("changed = True\n" * 25, encoding="utf-8")
    captured = {}

    def keyword_only_gate(*, phase, delta_files, delta_lines, route, reviewer_available, config):
        captured.update(files=delta_files, lines=delta_lines)
        return policy.SkipReviewerDecision("skip reviewer", policy.BELOW_THRESHOLD, "test")

    monkeypatch.setattr(codex.policy, "reviewer1_gate", keyword_only_gate)
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert captured == {"files": bundle["delta_files"], "lines": bundle["delta_lines"]}


def test_posttool_failure_never_calls_reviewer_and_preserves_ambiguity(workspace, config):
    repo, state = workspace
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    calls = []

    def reviewer(**kwargs):
        calls.append(kwargs)
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE}

    failure = {
        **call,
        "hook_event_name": "PostToolUseFailure",
        "error": "request timed out",
        "is_interrupt": False,
    }

    result = codex.post_tool_use_failure(
        failure,
        config=config,
        state_root=state,
        reviewer=reviewer,
    )

    assert calls == []
    assert "work_performed is UNKNOWN" in result["hookSpecificOutput"]["additionalContext"]
    assert list((state / "pending").glob("*.json")) == []
    loaded = ledger.SessionLedger(state).load("task-1")
    outcome = loaded["record"]["last_outcome"]
    assert outcome["ambiguous_outcome"] is True
    assert outcome["work_performed"] == "UNKNOWN"
    assert Path(outcome["evidence_path"]).is_file()


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


def test_post_tool_use_emits_audit_receipt_on_audit_mode(workspace, config):
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

    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["audit_receipt"] is not None
    assert bundle["audit_receipt"]["verdict"] == "READY_FOR_ONE_TURN_ACCEPT"
    assert "git add" in bundle["audit_receipt"]["commit_command"]
    assert "feat(task-1)" in bundle["audit_receipt"]["commit_command"]


def test_post_tool_use_micro_repair_triggers_on_changes_required(workspace, config):
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
    assert bundle["micro_repair_cleared_review"] is True
    assert bundle["micro_repair"]["status"] == "SUCCEEDED"
    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["audit_receipt"]["verdict"] == "READY_FOR_ONE_TURN_ACCEPT"


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



