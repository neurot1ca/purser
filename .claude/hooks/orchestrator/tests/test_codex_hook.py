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
import context_io_telemetry
import hook
import json_transport
import ledger
import policy
import telemetry


CONFIG_PATH = Path(__file__).parents[1] / "config.json"


def remove_test_tree(path):
    def make_writable(function, target, _error):
        os.chmod(target, stat.S_IWRITE)
        function(target)

    shutil.rmtree(path, onerror=make_writable)


@pytest.fixture()
def config():
    return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))


@pytest.fixture(autouse=True)
def disable_configured_projection_for_codex_tests(monkeypatch):
    monkeypatch.setattr(
        telemetry,
        "_projection_settings",
        lambda: (False, telemetry.obsidian_projection.DEFAULT_VAULT_ROOT),
    )


@pytest.fixture()
def workspace():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "codex" / uuid.uuid4().hex
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


def non_report_size(bundle):
    return len(
        json_transport.dumps(
            {key: value for key, value in bundle.items() if key != "implementer_report"},
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    )


def test_bundle_cap_summarizes_large_deltas_and_preserves_safety_fields(workspace, config):
    _, state = workspace
    protected = "state/current.yaml"
    changed_paths = [f"generated/path-{index:04d}.txt" for index in range(4999)] + [protected]
    bundle = codex._base_bundle()
    bundle.update(
        task_id="stress-task",
        thread_id="thread-stress",
        phase="implementation",
        routing_enforced=True,
        claude_review_mode=policy.FULL,
        reviewer1_result=policy.CHANGES_REQUIRED,
        protected_paths_touched=[protected],
        concurrent_writer=True,
        micro_repair={"attempted": False, "status": "SKIPPED_BUDGET"},
        validation="FAIL",
        changed_paths=changed_paths,
        findings=[
            {"code": f"FINDING_{index:02d}", "message": "x" * 2000}
            for index in range(20)
        ],
        implementer_report="r" * 5000,
    )

    raw = codex._finalize_replacement(bundle, config, state, event(tool_use_id="stress"))
    compact = response_bundle(raw, config)

    assert non_report_size(compact) <= config["bundle_non_report_max_bytes"]
    assert compact["changed_path_count"] == 5000
    assert len(compact["changed_paths"]) <= codex.CHANGED_PATH_PREVIEW_LIMIT
    assert len(compact["changed_paths_sha256"]) == 64
    assert compact["protected_paths_touched"] == [protected]
    assert compact["claude_review_mode"] == policy.FULL
    assert [finding["code"] for finding in compact["findings"]] == [
        f"FINDING_{index:02d}" for index in range(20)
    ]
    assert all(finding["message_char_count"] == 2000 for finding in compact["findings"])
    assert all(len(finding["message_sha256"]) == 64 for finding in compact["findings"])
    assert len(compact["implementer_report"]) == config["implementation_report_max_chars"]

    artifact = Path(compact["full_bundle_path"])
    stored = json.loads(artifact.read_text(encoding="utf-8"))
    assert stored["changed_paths"] == changed_paths
    assert stored["findings"][19]["message"] == "x" * 2000
    assert stored["implementer_report"] == "r" * 5000


def test_runtime_caps_change_bundle_and_phase_report_behavior(workspace, config):
    _, state = workspace
    base = codex._base_bundle()
    base.update(
        task_id="runtime-caps",
        thread_id="thread-caps",
        phase="operation",
        routing_enforced=True,
        claude_review_mode=policy.FULL,
        reviewer1_skipped_reason="PHASE_OPERATION",
        protected_paths_touched=[],
        validation="UNKNOWN",
        changed_paths=[f"path-{index}.txt" for index in range(100)],
        findings=[{"code": "LONG_MESSAGE", "message": "m" * 6000}],
        implementer_report="operation deliverable " * 100,
    )
    roomy_config = dict(
        config,
        bundle_non_report_max_bytes=8192,
        operation_report_max_chars=24000,
    )
    tight_config = dict(
        config,
        bundle_non_report_max_bytes=4096,
        operation_report_max_chars=37,
    )

    roomy = response_bundle(
        codex._finalize_replacement(dict(base), roomy_config, state, event(tool_use_id="roomy")),
        roomy_config,
    )
    tight = response_bundle(
        codex._finalize_replacement(dict(base), tight_config, state, event(tool_use_id="tight")),
        tight_config,
    )

    assert non_report_size(roomy) <= 8192
    assert non_report_size(tight) <= 4096
    assert len(tight["findings"][0]["message"]) < len(roomy["findings"][0]["message"])
    assert len(roomy["implementer_report"]) == len(base["implementer_report"])
    assert len(tight["implementer_report"]) == 37


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


def test_context_association_failure_does_not_change_phase_1a_pretool_output(
    workspace, config, monkeypatch
):
    repo, state = workspace
    call = event(tool_use_id="context-association-failure")
    expected = codex.pre_tool_use(
        call, config=config, state_root=state / "control", repo_root=repo
    )

    monkeypatch.setattr(
        context_io_telemetry,
        "associate_turn",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("telemetry unavailable")),
    )
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
        ("subtle", "gpt-5.6-luna", "high"),
        ("normal", "gpt-5.6-sol", "medium"),
        ("consequential", "gpt-5.6-sol", "high"),
        ("escalated", "gpt-5.6-sol", "xhigh"),
        ("operation", "gpt-5.6-terra", "high"),
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
    assert updated["prompt"] == (
        "Implement.\n" + codex._WORKER_VALIDATION_INSTRUCTION
    )


@pytest.mark.parametrize("tool_name", [codex.CODEX_TOOL, codex.CODEX_REPLY_TOOL])
def test_implementation_prompt_reports_worker_validation_once(workspace, config, tool_name):
    repo, state = workspace
    if tool_name == codex.CODEX_REPLY_TOOL:
        save_live_thread(state)
        fields = {"threadId": "thread-1"}
        tool_use_id = "implementation-reply-validation"
    else:
        fields = {}
        tool_use_id = "implementation-validation"

    result = codex.pre_tool_use(
        event(
            tool_name,
            prompt=orch() + "\nImplement.\n" + codex._WORKER_VALIDATION_INSTRUCTION,
            tool_use_id=tool_use_id,
            **fields,
        ),
        config=config,
        state_root=state,
        repo_root=repo,
    )
    prompt = result["hookSpecificOutput"]["updatedInput"]["prompt"]

    assert prompt.count(codex._WORKER_VALIDATION_INSTRUCTION) == 1
    listed_keys = {
        token.strip("`")
        for token in codex._WORKER_VALIDATION_INSTRUCTION.split(
            "Use only these keys: ", 1
        )[1].split(". ", 1)[0].split(", ")
    }
    assert listed_keys <= codex._ALLOWED_VALIDATION_KEYS


@pytest.mark.parametrize("tool_name", [codex.CODEX_TOOL, codex.CODEX_REPLY_TOOL])
def test_operation_prompt_does_not_include_worker_validation_instruction(
    workspace, config, tool_name
):
    repo, state = workspace
    if tool_name == codex.CODEX_REPLY_TOOL:
        save_live_thread(state, phase="operation", scope=())
        fields = {"threadId": "thread-1"}
        tool_use_id = "operation-reply-validation"
    else:
        fields = {}
        tool_use_id = "operation-validation"

    result = codex.pre_tool_use(
        event(
            tool_name,
            prompt=orch(phase="operation", scope=None) + "\nInspect.",
            tool_use_id=tool_use_id,
            **fields,
        ),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    assert codex._WORKER_VALIDATION_INSTRUCTION not in result["hookSpecificOutput"][
        "updatedInput"
    ]["prompt"]


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


def test_pretool_succeeds_with_dirty_secret_named_repository_path(workspace, config):
    repo, state = workspace
    target = repo / "scripts" / "orchestration_token_audit_0.py"
    target.parent.mkdir()
    target.write_text("value = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(
        ["git", "commit", "-qm", "add token-named script"],
        cwd=repo,
        check=True,
        timeout=10,
    )
    target.write_text("value = 2\n", encoding="utf-8")
    call = event(prompt=orch(scope="scripts") + "\nImplement.")

    result = codex.pre_tool_use(
        call, config=config, state_root=state, repo_root=repo
    )

    output = result["hookSpecificOutput"]
    assert "updatedInput" in output
    assert "additionalContext" not in output
    loaded = ledger.SessionLedger(state).load("task-1")
    assert loaded["status"] == ledger.OK
    assert (
        "scripts/orchestration_token_audit_0.py"
        in loaded["record"]["pending_call_baseline"]["tracked_snapshots"]
    )


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

    assert updated == {
        "threadId": "thread-1",
        "prompt": "Continue.\n" + codex._WORKER_VALIDATION_INSTRUCTION,
    }


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
        "prompt": (
            "Continue without repeating routing.\n"
            + codex._WORKER_VALIDATION_INSTRUCTION
        ),
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
    assert output["updatedInput"]["prompt"] == (
        "\nbody text\n" + codex._WORKER_VALIDATION_INSTRUCTION
    )
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


def test_hook_cwd_resolves_external_repository_top_level(workspace, config):
    repo, state = workspace
    nested = repo / "nested" / "work"
    nested.mkdir(parents=True)
    call = event(cwd=str(nested))

    pre = codex.pre_tool_use(call, config=config, state_root=state)
    assert "updatedInput" in pre["hookSpecificOutput"]
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-external", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            reviewer=lambda **_kwargs: {
                "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "provider": "stub",
                "validation": "UNKNOWN",
                "findings": [],
            },
        ),
        config,
    )

    assert bundle["changed_paths"] == ["file0.py"]
    assert bundle["project_root_reason"] is None


@pytest.mark.parametrize("cwd_kind", ["missing", "non_git"])
def test_unresolved_hook_root_returns_full_bundle(
    workspace, config, cwd_kind, monkeypatch
):
    repo, state = workspace
    call = event(tool_use_id=f"unresolved-{cwd_kind}")
    if cwd_kind == "non_git":
        non_git = repo.parent / "not-a-repository"
        non_git.mkdir()
        monkeypatch.setenv("GIT_CEILING_DIRECTORIES", str(non_git.parent))
        call["tool_input"]["cwd"] = str(non_git)

    pre = codex.pre_tool_use(call, config=config, state_root=state)
    assert "updatedInput" in pre["hookSpecificOutput"]
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-unresolved", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state), config
    )

    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["project_root_reason"].startswith("project root unresolved:")
    assert any(item["code"] == "PROJECT_ROOT_UNRESOLVED" for item in bundle["findings"])


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
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|scope=file0.py\n"
        "Task Objective: Update file0 with new value\n"
        "Acceptance Criteria: file0 contains value = 2\n"
    )
    call = event(prompt=prompt)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({
                    "threadId": "thread-1",
                    "content": "updated\nVALIDATION: {\"source\": \"WORKER_REPORTED\", \"status\": \"PASS\", \"commands\": [\"pytest -q\"], \"exit_codes\": [0]}",
                }),
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


def test_worker_validation_parses_full_report_before_phase_cap(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|scope=file0.py\n"
        "Task Objective: Update file0 with new value\n"
        "Acceptance Criteria: file0 contains value = 2\n"
    )
    call = event(prompt=prompt, tool_use_id="full-report-validation")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")

    validation_line = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest -q"],"exit_codes":[0]}'
    )
    report = ("worker output\n" * 150) + validation_line
    assert len(report) > codex.IMPLEMENTER_REPORT_LIMIT
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-full-report", "content": report}),
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

    assert bundle["worker_validation"]["status"] == "PASS"
    assert len(bundle["implementer_report"]) == codex.IMPLEMENTER_REPORT_LIMIT
    assert validation_line not in bundle["implementer_report"]


def test_post_tool_use_ungrounded_contract_forces_full_review_even_on_audit(workspace, config):
    repo, state = workspace
    # Default event() prompt has no Task Objective / Acceptance Criteria
    call = event()
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({
                    "threadId": "thread-1",
                    "content": "updated\nVALIDATION: {\"source\": \"WORKER_REPORTED\", \"status\": \"PASS\", \"commands\": [\"pytest\"], \"exit_codes\": [0]}",
                }),
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

    # Must be forced to FULL because task contract is not grounded!
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None
    assert any(f["code"] == "TASK_CONTRACT_NOT_GROUNDED" for f in bundle["findings"])
    assert not any(f["code"] == "WORKER_VALIDATION_UNVERIFIED" for f in bundle["findings"])


def test_post_tool_use_micro_repair_triggers_on_changes_required(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|scope=file0.py,file1.py,file2.py\n"
        "Task Objective: Apply changes to files 0 to 2\n"
        "Acceptance Criteria: All files updated and clean\n"
    )
    call = event(prompt=prompt)
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
                "text": json.dumps({
                    "threadId": "thread-1",
                    "content": "updated\nVALIDATION: {\"source\": \"WORKER_REPORTED\", \"status\": \"PASS\", \"commands\": [\"pytest -q\"], \"exit_codes\": [0]}",
                }),
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
    assert reviewer_invocations[1]["task_objective"] == reviewer_invocations[0]["task_objective"]
    assert reviewer_invocations[1]["acceptance_criteria"] == reviewer_invocations[0]["acceptance_criteria"]
    assert reviewer_invocations[1]["validation_summary"] == reviewer_invocations[0]["validation_summary"]
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


def test_task_contract_parsing_and_grounding_flag(workspace, config):
    repo, state = workspace

    # Case A: Explicit JSON TASK_CONTRACT
    prompt_json = (
        "ORCH|task=task-a|phase=implementation|route=normal|scope=file.py\n"
        'TASK_CONTRACT: {"objective": "Fix deadline logic", "criteria": "Zero latency drift"}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json, "task-a", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Fix deadline logic"
    assert crit == "Zero latency drift"

    # Case B: Multiline headers
    prompt_multiline = (
        "ORCH|task=task-b|phase=implementation|route=normal|scope=file.py\n"
        "Task Objective:\nRefactor connection pool\n"
        "Acceptance Criteria:\nPreserve retry backoff"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_multiline, "task-b", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Refactor connection pool"
    assert crit == "Preserve retry backoff"

    # Case C: Generic prompt without explicit contract
    prompt_generic = "ORCH|task=task-c|phase=implementation|route=normal|scope=file.py\nJust do the work"
    obj, crit, grounded = codex._parse_task_contract(prompt_generic, "task-c", "implementation", ["file.py"])
    assert grounded is False
    assert "Task task-c" in obj
    assert "file.py" in crit


def test_worker_validation_failure_mechanically_forces_full_claude_review(workspace, config):
    repo, state = workspace
    for i in range(3):
        (repo / f"f{i}.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    prompt = (
        "ORCH|task=task-val|phase=implementation|route=normal|scope=f0.py,f1.py,f2.py\n"
        "Task Objective: Implement f0-f2 updates\n"
        "Acceptance Criteria: All f0-f2 files changed\n"
    )
    call = event(prompt=prompt)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    for i in range(3):
        (repo / f"f{i}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    report = (
        "Execution completed with failure.\n"
        'VALIDATION: {"source": "WORKER_REPORTED", "status": "FAIL", "summary": "Integration test failed", "commands": ["pytest -q tests/test_integration.py"], "exit_codes": [1], "artifacts": []}'
    )
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": report}),
            }
        ],
    }

    def reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "UNKNOWN",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=reviewer
    )
    bundle = response_bundle(raw, config)

    # Mechanical invariant: worker validation failure MUST force FULL mode even if reviewer accepted!
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "WORKER_VALIDATION_FAILED" for f in bundle["findings"])
    assert bundle["worker_validation"]["status"] == "FAIL"


def test_reviewer_internal_typeerror_never_retried(workspace, config):
    repo, state = workspace
    for i in range(3):
        (repo / f"g{i}.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    call = event(
        prompt="ORCH|task=task-err|phase=implementation|route=normal|scope=g0.py,g1.py,g2.py"
    )
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    for i in range(3):
        (repo / f"g{i}.py").write_text("changed = True\n" * 25, encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-1", "content": "Validation: PASS"}),
            }
        ],
    }
    calls = []

    def failing_reviewer(**kwargs):
        calls.append(kwargs)
        raise TypeError("simulated internal adapter error")

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=failing_reviewer
    )
    bundle = response_bundle(raw, config)

    assert len(calls) == 1
    assert bundle["claude_review_mode"] == policy.FULL


def test_worker_validation_parser_avoids_substring_false_positives():
    expected_keys = {"source", "status", "summary", "commands", "exit_codes", "artifacts"}

    # 1. Serbest metin "tests never passed" => UNKNOWN
    res1 = codex._parse_worker_validation("Validation: tests never passed")
    assert res1["status"] == "UNKNOWN"
    assert set(res1.keys()) == expected_keys

    # 2. Geçerli structured PASS => PASS
    good_pass = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    res2 = codex._parse_worker_validation(good_pass)
    assert res2["status"] == "PASS"
    assert res2["commands"] == ["pytest"]
    assert res2["exit_codes"] == [0]
    assert set(res2.keys()) == expected_keys

    # 3. Geçerli structured FAIL => FAIL
    good_fail = 'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[1]}'
    res3 = codex._parse_worker_validation(good_fail)
    assert res3["status"] == "FAIL"
    assert res3["commands"] == ["pytest"]
    assert res3["exit_codes"] == [1]
    assert set(res3.keys()) == expected_keys

    # 4. source="INDEPENDENT_VERIFIED" => UNKNOWN (forged provenance rejected)
    bad_source = 'VALIDATION: {"source":"INDEPENDENT_VERIFIED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    res4 = codex._parse_worker_validation(bad_source)
    assert res4["status"] == "UNKNOWN"
    assert set(res4.keys()) == expected_keys

    # 5. commands=["   "] => UNKNOWN (empty string after trim)
    blank_cmd = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["   "],"exit_codes":[0]}'
    res5 = codex._parse_worker_validation(blank_cmd)
    assert res5["status"] == "UNKNOWN"
    assert set(res5.keys()) == expected_keys

    # 6. commands string => UNKNOWN (wrong type)
    str_cmd = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":"pytest","exit_codes":[0]}'
    res6 = codex._parse_worker_validation(str_cmd)
    assert res6["status"] == "UNKNOWN"
    assert set(res6.keys()) == expected_keys

    # 7. exit_codes içinde bool => UNKNOWN (bool is not accepted as int)
    bool_code = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[true]}'
    res7 = codex._parse_worker_validation(bool_code)
    assert res7["status"] == "UNKNOWN"
    assert set(res7.keys()) == expected_keys

    # 8. PASS ve nonzero exit code => UNKNOWN
    pass_nonzero = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[1]}'
    res8 = codex._parse_worker_validation(pass_nonzero)
    assert res8["status"] == "UNKNOWN"
    assert set(res8.keys()) == expected_keys

    # 9. FAIL ve bütün exit code değerleri 0 => UNKNOWN
    fail_zero = 'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[0]}'
    res9 = codex._parse_worker_validation(fail_zero)
    assert res9["status"] == "UNKNOWN"
    assert set(res9.keys()) == expected_keys

    # 10. Valid PASS + ikinci malformed FAIL kaydı => UNKNOWN (multiple records)
    multi_malformed = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}\n'
        'VALIDATION: malformed json'
    )
    res10 = codex._parse_worker_validation(multi_malformed)
    assert res10["status"] == "UNKNOWN"
    assert set(res10.keys()) == expected_keys

    # 11. İki valid PASS kaydı => UNKNOWN (multiple records)
    multi_valid = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}\n'
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    )
    res11 = codex._parse_worker_validation(multi_valid)
    assert res11["status"] == "UNKNOWN"
    assert set(res11.keys()) == expected_keys

    # 12. Boyut sınırını aşan summary => UNKNOWN
    big_summary = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"' + ('x' * 1300) + '","commands":["pytest"],"exit_codes":[0]}'
    res12 = codex._parse_worker_validation(big_summary)
    assert res12["status"] == "UNKNOWN"
    assert set(res12.keys()) == expected_keys
    assert len(json_transport.dumps(res12).encode("utf-8")) <= codex.IMPLEMENTER_REPORT_LIMIT


def test_worker_validation_normalized_byte_limit_invariants():
    expected_keys = {"source", "status", "summary", "commands", "exit_codes", "artifacts"}

    # 1. 1200 emoji içeren free-text validation
    emojis = "🚀" * 1200
    res1 = codex._parse_worker_validation(f"Validation: {emojis}")
    assert res1["status"] == "UNKNOWN"
    assert set(res1.keys()) == expected_keys
    enc1 = json_transport.dumps(res1).encode("utf-8")
    assert len(enc1) <= codex.IMPLEMENTER_REPORT_LIMIT
    res1["summary"].encode("utf-8")

    # 2. Summary içermeyen, limite yakın geçerli PASS:
    # Summary tüm JSON satırının kopyası olmamalı, kısa ve sabit özet olmalı.
    cmds = [f"pytest tests/test_{i:02d}.py" for i in range(25)]
    pass_payload = json.dumps({
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "commands": cmds,
        "exit_codes": [0] * len(cmds),
    })
    res2 = codex._parse_worker_validation(f"VALIDATION: {pass_payload}")
    assert res2["status"] == "PASS"
    assert set(res2.keys()) == expected_keys
    assert res2["summary"] == "Structured worker validation reported PASS."
    assert "VALIDATION:" not in res2["summary"]
    enc2 = json_transport.dumps(res2).encode("utf-8")
    assert len(enc2) <= codex.IMPLEMENTER_REPORT_LIMIT

    # 3. Normalize edilmiş PASS/FAIL nesnesi sınırı aşıyorsa:
    huge_cmds = [f"pytest tests/test_long_path_module_{i:04d}_spec.py" for i in range(50)]
    huge_payload = json.dumps({
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "commands": huge_cmds,
        "exit_codes": [0] * len(huge_cmds),
    })
    res3 = codex._parse_worker_validation(f"VALIDATION: {huge_payload}")
    assert res3["status"] == "UNKNOWN"
    assert set(res3.keys()) == expected_keys
    enc3 = json_transport.dumps(res3).encode("utf-8")
    assert len(enc3) <= codex.IMPLEMENTER_REPORT_LIMIT

    # 4. ASCII ve çok baytlı Unicode summary için byte invariantı:
    res4_ascii = codex._parse_worker_validation("Validation: " + ("a" * 2000))
    assert res4_ascii["status"] == "UNKNOWN"
    enc4_ascii = json_transport.dumps(res4_ascii).encode("utf-8")
    assert len(enc4_ascii) <= codex.IMPLEMENTER_REPORT_LIMIT

    cjk_and_emoji = "测试验证🌟" * 400
    res4_uni = codex._parse_worker_validation(f"Validation: {cjk_and_emoji}")
    assert res4_uni["status"] == "UNKNOWN"
    enc4_uni = json_transport.dumps(res4_uni).encode("utf-8")
    assert len(enc4_uni) <= codex.IMPLEMENTER_REPORT_LIMIT
    res4_uni["summary"].encode("utf-8")


def test_task_contract_partial_is_not_grounded_and_handles_lists():
    # Case 1: Only objective provided -> grounded MUST be False
    prompt_obj_only = "ORCH|task=t1|phase=implementation|route=normal|scope=f.py\nTask Objective: Only objective"
    obj, crit, grounded = codex._parse_task_contract(prompt_obj_only, "t1", "implementation", ["f.py"])
    assert grounded is False
    assert obj == "Only objective"

    # Case 2: Only criteria provided -> grounded MUST be False
    prompt_crit_only = "ORCH|task=t2|phase=implementation|route=normal|scope=f.py\nAcceptance Criteria: Only criteria"
    obj, crit, grounded = codex._parse_task_contract(prompt_crit_only, "t2", "implementation", ["f.py"])
    assert grounded is False
    assert crit == "Only criteria"

    # Case 3: Multiline bullet list criteria
    prompt_bullets = (
        "ORCH|task=t3|phase=implementation|route=normal|scope=f.py\n"
        "Task Objective: Full feature\n"
        "Acceptance Criteria:\n"
        "- First criterion\n"
        "- Second criterion\n"
        "- Third criterion"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_bullets, "t3", "implementation", ["f.py"])
    assert grounded is True
    assert obj == "Full feature"
    assert crit == "First criterion; Second criterion; Third criterion"

    # Case 4: JSON list criteria
    prompt_json_list = (
        "ORCH|task=t4|phase=implementation|route=normal|scope=f.py\n"
        'TASK_CONTRACT: {"objective": "Feature", "criteria": ["Crit 1", "Crit 2"]}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json_list, "t4", "implementation", ["f.py"])
    assert grounded is True
    assert obj == "Feature"
    assert crit == "Crit 1; Crit 2"


def test_task_contract_markdown_sections_are_grounded_and_fence_aware():
    prompt = (
        "# Objective\n"
        "Refactor the task contract parser.\n\n"
        "# Acceptance criteria\n"
        "1. Collect the first criterion.\n\n"
        "2. Collect the second criterion.\n"
        "\n"
        "- Collect the third criterion.\n"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt, "markdown", "implementation", [])
    assert grounded is True
    assert obj == "Refactor the task contract parser."
    assert crit == "Collect the first criterion.; Collect the second criterion.; Collect the third criterion."

    inline_prompt = "## Task: Fix the parser\n\n### Requirements\n- Preserve JSON parsing"
    obj, crit, grounded = codex._parse_task_contract(inline_prompt, "inline", "implementation", [])
    assert grounded is True
    assert obj == "Fix the parser"
    assert crit == "Preserve JSON parsing"

    objective_only = "# Goal\nDo the work\n\n# Acceptance criteria:\n"
    _, _, grounded = codex._parse_task_contract(objective_only, "objective-only", "implementation", [])
    assert grounded is False

    empty_criteria = "# Objective\nDo the work\n\n# Criteria:\n"
    _, _, grounded = codex._parse_task_contract(empty_criteria, "empty-criteria", "implementation", [])
    assert grounded is False

    fenced_headings = "```markdown\n# Objective\nDo the work\n# Criteria\n- Looks valid\n```"
    _, _, grounded = codex._parse_task_contract(fenced_headings, "fenced", "implementation", [])
    assert grounded is False

    exact_task_prompt = """Task objective: make `_parse_task_contract` in `.claude/hooks/orchestrator/codex.py` recognise task contracts written as Markdown sections, so correctly specified delegations stop being flagged `TASK_CONTRACT_NOT_GROUNDED`.

Context (verified at HEAD 3fab168): every delegation today (5 of 5) was flagged `TASK_CONTRACT_NOT_GROUNDED` and forced to FULL review, although each prompt had an objective section and an acceptance-criteria section. The parser only accepts a one-line `TASK_CONTRACT: {json}` or key-value lines starting with `task objective:` / `acceptance criteria:`. Prompts written as Markdown headings (`# Objective` with the text on following lines) are never matched. Separately, criteria collection stops at the first blank line, so a Markdown list with blank lines between items would be cut short even in the key-value form. Consequence: the one-turn AUDIT path never fires, costing Claude an extra review turn on every delegation.

Acceptance criteria:

1. Recognise the CLASS "a Markdown heading (any level, `#` to `###`) naming the objective or the criteria, followed by its section body". Objective heading names: `Objective`, `Task objective`, `Task`, `Goal`. Criteria heading names: `Acceptance criteria`, `Criteria`, `Requirements`. Case-insensitive; an optional trailing colon is allowed; text on the heading line after a colon (e.g. `# Task: fix the parser`) counts as body.
2. A section body runs until the next heading of the same or a higher level, NOT until the first blank line. Numbered and bulleted items separated by blank lines are all collected.
3. Headings inside fenced code blocks (```) are ignored.
4. Existing forms keep working unchanged: `TASK_CONTRACT:` JSON, and the `task objective:` / `acceptance criteria:` key-value lines.
5. Unchanged rule: grounded only when BOTH a non-empty objective and non-empty criteria are found. A present-but-empty section does not count.

Tests — examples, non-exhaustive: `# Objective` + `# Acceptance criteria` with a numbered list separated by blank lines -> grounded, every item collected; `## Task: X` + `### Requirements` -> grounded; objective only -> not grounded; empty criteria section -> not grounded; headings only inside a code fence -> not grounded; both existing forms -> still grounded. Also run the exact prompt of this task through the parser: it must be grounded.

Validation (explicit user instruction): do NOT run the full suite. Run only `tests/test_codex_hook.py` plus any new test file, once green at the end. Claude runs the full suite once before acceptance.

Report: the change, the tests added, the exact test command and counts."""
    _, _, grounded = codex._parse_task_contract(exact_task_prompt, "exact", "implementation", [])
    assert grounded is True


def test_base_bundle_contains_contract_and_validation_fields():
    bundle = codex._base_bundle()
    assert "task_contract_grounded" in bundle
    assert bundle["task_contract_grounded"] is False
    assert "worker_validation" in bundle
    assert bundle["worker_validation"] is None


def test_codex_reply_continuation_inherits_prior_grounded_contract(workspace, config):
    repo, state = workspace
    (repo / "f.py").write_text("v = 0\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "init"], cwd=repo, check=True)

    # Initial call with explicit grounded contract
    initial_prompt = (
        "ORCH|task=task-cont|phase=implementation|route=normal|scope=f.py\n"
        'TASK_CONTRACT: {"objective": "Persistent objective", "criteria": "Persistent criteria"}'
    )
    initial_call = event(prompt=initial_prompt, tool_use_id="call-init")
    codex.pre_tool_use(initial_call, config=config, state_root=state, repo_root=repo)

    post = {
        **initial_call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-persistent", "content": "Initial work done"}),
            }
        ],
    }

    def stub_reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=stub_reviewer)

    # Continuation reply with generic prompt without explicit contract
    reply_prompt = "ORCH|task=task-cont|phase=implementation|route=normal|scope=f.py\nContinue work"
    reply_call = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt=reply_prompt,
        tool_use_id="call-reply",
        threadId="thread-persistent",
    )
    codex.pre_tool_use(reply_call, config=config, state_root=state, repo_root=repo)

    # Read pending and verify inherited contract
    pending = codex._read_pending(state, reply_call)
    assert pending["task_contract_grounded"] is True
    assert pending["task_objective"] == "Persistent objective"
    assert pending["acceptance_criteria"] == "Persistent criteria"


@pytest.mark.parametrize(
    "is_grounded,report_content,expected_mode,expected_findings,expected_receipt",
    [
        # grounded + valid worker PASS + reviewer accept => AUDIT ve audit_receipt
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}',
            policy.AUDIT,
            [],
            True,
        ),
        # ungrounded + valid PASS + reviewer accept => FULL + TASK_CONTRACT_NOT_GROUNDED
        (
            False,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}',
            policy.FULL,
            ["TASK_CONTRACT_NOT_GROUNDED"],
            False,
        ),
        # grounded + missing validation + reviewer accept => FULL + WORKER_VALIDATION_UNVERIFIED
        (
            True,
            "Tests ran and everything looked fine in stdout.",
            policy.FULL,
            ["WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
        # grounded + malformed validation + reviewer accept => FULL + WORKER_VALIDATION_UNVERIFIED
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":"pytest","exit_codes":[0]}',
            policy.FULL,
            ["WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
        # grounded + valid FAIL + reviewer accept => FULL + WORKER_VALIDATION_FAILED
        (
            True,
            'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL","commands":["pytest"],"exit_codes":[1]}',
            policy.FULL,
            ["WORKER_VALIDATION_FAILED"],
            False,
        ),
        # Grounding ve validation ayni anda hataliysa iki finding de korunmali
        (
            False,
            "Just done without contract and without validation block.",
            policy.FULL,
            ["TASK_CONTRACT_NOT_GROUNDED", "WORKER_VALIDATION_UNVERIFIED"],
            False,
        ),
    ],
)
def test_end_to_end_review_mode_matrix(
    workspace, config, is_grounded, report_content, expected_mode, expected_findings, expected_receipt
):
    repo, state = workspace
    (repo / "test_matrix.py").write_text("v = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "matrix-init"], cwd=repo, check=True)

    if is_grounded:
        prompt = (
            "ORCH|task=t-mat|phase=implementation|route=normal|scope=test_matrix.py\n"
            "Task Objective: Update test matrix file\n"
            "Acceptance Criteria: test_matrix.py has value 2"
        )
    else:
        prompt = "ORCH|task=t-mat|phase=implementation|route=normal|scope=test_matrix.py\nGeneric prompt"

    call = event(prompt=prompt)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "test_matrix.py").write_text("v = 2\n", encoding="utf-8")

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-matrix", "content": report_content}),
            }
        ],
    }

    def stub_reviewer(**kwargs):
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=stub_reviewer
    )
    bundle = response_bundle(raw, config)

    assert bundle["claude_review_mode"] == expected_mode
    if expected_receipt:
        assert bundle["audit_receipt"] is not None
        assert bundle["audit_receipt"]["verdict"] == "READY_FOR_ONE_TURN_ACCEPT"
    else:
        assert bundle["audit_receipt"] is None

    for finding_code in expected_findings:
        assert any(f["code"] == finding_code for f in bundle["findings"]), f"Expected finding {finding_code}"
