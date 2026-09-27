import io
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from dataclasses import replace
from datetime import timedelta, timezone
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import time
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import codex
import async_session_maintenance
import codex_exec_mcp
import coder_config
import checks
import project_profile
import context_io_telemetry
import hook
import json_transport
import ledger
import policy
import project_root
import reviewer as reviewer_module
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
    isolated_home = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "codex-profile-home"
        / uuid.uuid4().hex
    )
    monkeypatch.setenv("USERPROFILE", str(isolated_home))
    monkeypatch.setenv("HOME", str(isolated_home))
    monkeypatch.setattr(Path, "home", lambda: isolated_home)
    monkeypatch.setattr(
        telemetry,
        "_projection_settings",
        lambda: (False, telemetry.obsidian_projection.DEFAULT_VAULT_ROOT),
    )
    yield
    if isolated_home.exists():
        remove_test_tree(isolated_home)


@pytest.fixture()
def workspace():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "codex" / uuid.uuid4().hex
    repo = root / "repo"
    state = root / "state"
    repo.mkdir(parents=True, exist_ok=True)
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
    if tool_name in codex.CODEX_TOOLS:
        tool_input["label"] = "test task"
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


def test_opencode_start_and_cross_agent_reply_denied(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="opencode/big-pickle", effort="none")
    start = event(START_TOOL, tool_use_id="opencode-start")
    updated = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == "opencode/big-pickle"
    assert updated["config"] == {}
    assert "developer-instructions" not in updated
    record = ledger.build_session_record("task-2", "foreign-thread", "normal", "gpt-6-sol", "medium",
                                         phase="implementation", scope=["file0.py"], agent="codex")
    assert ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).save(record)["status"] == ledger.OK
    reply = event(codex.CODEX_REPLY_TOOL, prompt=orch(task="task-2", scope="file0.py") + "\nContinue.",
                  threadId="foreign-thread", tool_use_id="foreign-reply")
    denied = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    assert denied["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "different coding agent" in denied["hookSpecificOutput"]["permissionDecisionReason"]


def test_mechanical_empty_candidates_keep_configured_opencode_binding(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="provider/global", effort="none")
    call = event(prompt=orch(route="mechanical", scope="file0.py") + "\nMake the small change.")
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert "agent" not in updated
    assert updated["model"] == "provider/global"
    assert "model_reasoning_effort" not in updated["config"]
    pending = codex._read_pending(state, call)
    assert (pending["agent"], pending["model"], pending["effort"]) == ("opencode", "provider/global", "none")
    codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"threadId": "mechanical-cold", "content": "Done"}}},
                        config=config, state_root=state, repo_root=repo)
    stored = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")["record"]["live_thread"]
    assert (stored["agent"], stored["model"], stored["effort"]) == ("opencode", "provider/global", "none")


def test_nonmechanical_route_binding_is_unchanged(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="provider/global", effort="none")
    call = event(route="normal", scope="file0.py")
    updated = codex.pre_tool_use(call, config=config, state_root=state,
                                 repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == "provider/global"
    pending = codex._read_pending(state, call)
    assert pending["agent"] == "opencode"


def test_mechanical_candidate_binds_opencode_per_job(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(coder_config, "opencode_candidates", lambda: ["provider/model-candidate"])
    call = event(prompt=orch(route="mechanical", scope="file0.py") + "\nMake the change.")
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["agent"] == "opencode"
    assert updated["model"] == "provider/model-candidate"
    assert updated["config"] == {}
    pending = codex._read_pending(state, call)
    assert (pending["agent"], pending["model"]) == ("opencode", "provider/model-candidate")
    codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"threadId": "mechanical-opencode", "content": "Done"}}},
                        config=config, state_root=state, repo_root=repo)
    stored = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")["record"]["live_thread"]
    assert (stored["agent"], stored["model"]) == ("opencode", "provider/model-candidate")


def test_served_model_updates_ledger_and_finished_telemetry(workspace, config, monkeypatch):
    repo, state = workspace
    candidate = "vendor/mechanical-free"
    monkeypatch.setattr(coder_config, "opencode_candidates", lambda: [candidate])
    config["coder_family"] = "gemini"
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(route="mechanical", scope="file0.py") + "\nMake the change.")
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["fallbackAgent"] == config["coder_agent"]
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    observed_pending = []
    original_observations = codex._post_observations
    def capture(*args, **kwargs):
        observed_pending.append(dict(args[2]))
        return original_observations(*args, **kwargs)
    monkeypatch.setattr(codex, "_post_observations", capture)
    reviewed = []
    response = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"threadId": "served-thread", "content": "Done", "servedAgent": "codex",
                               "servedModel": "gpt-6-sol", "attemptedModels": [candidate]}}},
        config=config, state_root=state, repo_root=repo,
        reviewer=lambda **kw: reviewed.append(kw) or {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                      "validation": "PASS", "findings": []})
    bundle = response_bundle(response, config)
    record = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")["record"]
    assert observed_pending[0]["model"] == "gpt-6-sol"
    assert record["live_thread"]["model"] == "gpt-6-sol"
    assert bundle["model"] == "gpt-6-sol"
    assert reviewed and reviewed[0]["model"] == "gpt-6-sol"
    finished = [item["payload"] for item in _all_telemetry_events(state)
                if item["event_type"] == "IMPLEMENTER_FINISHED"]
    assert finished[-1]["served_model"] == "gpt-6-sol"
    assert finished[-1]["implementer"] == "codex"


def test_agy_start_passes_route_effort_to_bridge(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "agy"
    for entry in config["routes"].values():
        entry.update(model="gemini-3.8-flash", effort="medium")
    start = event(START_TOOL, tool_use_id="agy-start")
    updated = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == "gemini-3.8-flash"
    assert updated["config"] == {"model_reasoning_effort": "medium"}


def test_unknown_model_family_still_reviews(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="unknown/mystery", effort="none")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    result = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "mystery-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo, reviewer=lambda **kw: invoked.append(kw))
    bundle = response_bundle(result, config)
    assert invoked
    assert bundle["reviewer1_skipped_reason"] is None
    assert bundle["reviewer1_result"] == policy.UNCERTAIN
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["implementer"] == "opencode-main"


def test_same_vendor_stored_family_skips_reviewer_and_forces_full(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    config["coder_family"] = "google"
    for entry in config["routes"].values():
        entry.update(model="opencode/big-pickle", effort="none")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    result = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "stored-family-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo, reviewer=lambda **kw: invoked.append(kw))
    bundle = response_bundle(result, config)
    assert not invoked
    assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_VENDOR_CONFLICT"
    assert bundle["claude_review_mode"] == policy.FULL


def test_stored_gemini_subfamily_matches_gemini_reviewer(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "agy"
    config["coder_family"] = "gemini-flash"
    for entry in config["routes"].values():
        entry.update(model="gemini-3.8-flash", effort="medium")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    result = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "gemini-subfamily-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo, reviewer=lambda **kw: invoked.append(kw))
    bundle = response_bundle(result, config)
    assert not invoked
    assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_VENDOR_CONFLICT"
    assert bundle["claude_review_mode"] == policy.FULL


def test_different_stored_family_runs_reviewer(workspace, config):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    config["coder_family"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="opencode/big-pickle", effort="none")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    invoked = []
    result = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "different-family-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo,
        reviewer=lambda **kw: invoked.append(kw) or {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                    "validation": "PASS", "findings": []})
    bundle = response_bundle(result, config)
    assert invoked
    assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE


class _RegisteredReviewerFixture:
    def validate_model(self, model):
        return None

    def build_command(self, *args, **kwargs):
        return []

    def invoke(self, *args, **kwargs):
        return None

    def parse(self, *args, **kwargs):
        return None


def _post_with_reviewer_candidates(workspace, config, candidates, reviewer, monkeypatch=None,
                                  coder_family="unknown"):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    config["coder_family"] = coder_family
    for entry in config["routes"].values():
        entry.update(model="opencode/big-pickle", effort="none")
    config["reviewer_gate"].update(small_delta_max_lines=0, small_delta_max_files=0)
    call = event(prompt=orch(scope="file0.py") + "\nChange it.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(state, call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["reviewer_candidates"] = candidates
    codex._atomic_json_write(pending_path, pending)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    if monkeypatch is not None:
        monkeypatch.setitem(reviewer_module.REVIEWER_ADAPTERS, "codex", _RegisteredReviewerFixture())
    result = codex.post_tool_use(
        {**call, "hook_event_name": "PostToolUse", "tool_response":
         {"structuredContent": {"threadId": "reviewer-fallback-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo, reviewer=reviewer,
    )
    return response_bundle(result, config)


def test_primary_reviewer_success_reports_selected_provider_and_no_attempt_list(workspace, config):
    report = {
        "review_status": "COMPLETED", "provider": "agy",
        "reviewer1_model_requested": "gemini-3.8-flash-high",
        "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        "validation": "PASS", "findings": [],
    }
    bundle = _post_with_reviewer_candidates(
        workspace, config, [{"agent": "agy", "model": "gemini-3.8-flash-high"}],
        lambda **kwargs: report,
    )
    assert bundle["reviewer1_provider"] == "agy"
    assert bundle["reviewer1_model_requested"] == "gemini-3.8-flash-high"
    assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    assert "reviewer1_attempts" not in bundle


def test_unavailable_primary_falls_back_and_records_attempts(workspace, config, monkeypatch):
    calls = []
    def reviewer(**kwargs):
        calls.append(kwargs)
        if kwargs["reviewer_agent"] == "agy":
            return {"review_status": "UNAVAILABLE", "provider": "agy",
                    "reviewer1_model_requested": kwargs["reviewer_model"],
                    "result": policy.REVIEWER1_UNAVAILABLE, "validation": "UNKNOWN", "findings": []}
        return {"review_status": "COMPLETED", "provider": "codex",
                "reviewer1_model_requested": kwargs["reviewer_model"],
                "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS", "findings": []}
    bundle = _post_with_reviewer_candidates(
        workspace, config,
        [{"agent": "agy", "model": "gemini-3.8-flash-high"},
         {"agent": "codex", "model": "gpt-6-sol"}], reviewer, monkeypatch,
    )
    assert [call["reviewer_agent"] for call in calls] == ["agy", "codex"]
    assert bundle["reviewer1_provider"] == "codex"
    assert bundle["reviewer1_model_requested"] == "gpt-6-sol"
    assert bundle["reviewer1_attempts"] == [
        {"agent": "agy", "model": "gemini-3.8-flash-high", "outcome": "UNAVAILABLE"},
        {"agent": "codex", "model": "gpt-6-sol", "outcome": "COMPLETED"},
    ]


def test_opencode_selection_reaches_reviewer_adapter_with_fake_runner(workspace, config, tmp_path):
    packet_response = {
        "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        "validation": "PASS",
        "findings": [],
    }
    stream = "\n".join(json.dumps(event) for event in (
        {"type": "step_start", "providerID": "vendor", "modelID": "review-model"},
        {"type": "text", "part": {"text": json.dumps(packet_response)}},
        {"type": "step_finish", "part": {"reason": "stop"}},
    ))

    class FakeRunner:
        def __init__(self):
            self.calls = []

        def __call__(self, command, **kwargs):
            self.calls.append((command, kwargs))
            return subprocess.CompletedProcess(command, 0, stdout=stream, stderr="")

    runner = FakeRunner()
    caller_workdir = tmp_path / "opencode-caller"
    caller_workdir.mkdir()

    def selected_reviewer(**kwargs):
        selected_config = {
            **reviewer_module._load_config(config),
            "reviewer1_executable": r"C:\tools\opencode.exe",
        }
        return reviewer_module.review_materials(
            {
                "objective": "Review the delegated change.",
                "acceptance_criteria": "The adapter returns a valid review.",
                "authority_contract": "Use only the supplied packet.",
                "delegated_diff": "diff --git a/a.py b/a.py\n-old\n+new\n",
                "validation_summary": "Focused tests requested.",
            },
            config=selected_config,
            runner=runner,
            cwd=caller_workdir,
            repo_root=Path("C:/reviewed-repo"),
            reviewer_agent=kwargs["reviewer_agent"],
            reviewer_model=kwargs["reviewer_model"],
            reviewer_timeout_seconds=kwargs["reviewer_timeout_seconds"],
        )

    candidate_model = "vendor/review-model"
    bundle = _post_with_reviewer_candidates(
        workspace,
        config,
        [{"agent": "opencode", "model": candidate_model}],
        selected_reviewer,
        coder_family="unknown",
    )

    assert len(runner.calls) == 1
    command, run_kwargs = runner.calls[0]
    assert command[:2] == [r"C:\tools\opencode.exe", "run"]
    assert command[command.index("--model") + 1] == candidate_model
    assert run_kwargs["env"]["OPENCODE_CONFIG_CONTENT"]
    assert bundle["reviewer1_provider"] == "opencode"
    assert bundle["reviewer1_model_requested"] == candidate_model
    assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE


def test_all_reviewer_candidates_exhausted_forces_full(workspace, config, monkeypatch):
    def unavailable(**kwargs):
        return {"review_status": "UNAVAILABLE", "provider": kwargs["reviewer_agent"],
                "reviewer1_model_requested": kwargs["reviewer_model"],
                "result": policy.REVIEWER1_UNAVAILABLE, "validation": "UNKNOWN", "findings": []}
    bundle = _post_with_reviewer_candidates(
        workspace, config,
        [{"agent": "agy", "model": "gemini-3.8-flash-high"},
         {"agent": "codex", "model": "gpt-6-sol"}], unavailable, monkeypatch,
    )
    assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
    assert bundle["claude_review_mode"] == policy.FULL
    assert [item["outcome"] for item in bundle["reviewer1_attempts"]] == ["UNAVAILABLE", "UNAVAILABLE"]


def test_same_unaliased_vendor_reviewer_is_refused(workspace, config):
    invoked = []
    bundle = _post_with_reviewer_candidates(
        workspace, config, [{"agent": "agy", "model": "qwen3-32b"}],
        lambda **kwargs: invoked.append(kwargs), coder_family="qwen",
    )
    assert not invoked
    assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_VENDOR_CONFLICT"
    assert bundle["claude_review_mode"] == policy.FULL


@pytest.mark.parametrize(("phase", "route", "reason"), [
    ("operation", "operation", policy.PHASE_OPERATION),
    ("implementation", "normal", policy.NO_DELTA),
])
def test_unknown_family_keeps_original_reviewer_skip(workspace, config, phase, route, reason):
    repo, state = workspace
    config["coder_agent"] = "opencode"
    for entry in config["routes"].values():
        entry.update(model="unknown/mystery", effort="none")
    call = event(prompt=orch(phase=phase, route=route, scope="file0.py") + "\nInspect.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    result = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "unchanged-thread", "content": "Done"}}},
        config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(result, config)
    assert bundle["reviewer1_skipped_reason"] == reason
    assert bundle["claude_review_mode"] == policy.FULL


def test_plan_sync_read_only_artifact_and_mutation(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(phase="plan", route="plan_standard", scope="file0.py") + "\nPlan this.",
                 sandbox="danger-full-access", **{"approval-policy": "on-request"})
    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    updated = pre["hookSpecificOutput"]["updatedInput"]
    assert updated["model"] == "gpt-6-sol"
    assert updated["config"]["model_reasoning_effort"] == "high"
    assert updated["sandbox"] == "read-only" and updated["approval-policy"] == "never"
    (repo / "file1.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "new.py").write_text("new = True\n", encoding="utf-8")
    content = "# Plan\n\nSummary:\n" + "A" * 4300 + "\n## Milestones\nFinish."
    post = {**call, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"threadId": "plan-thread", "content": content}}}
    bundle = response_bundle(codex.post_tool_use(post, config=config, state_root=state, repo_root=repo), config)
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["reviewer1_skipped_reason"] == "PHASE_PLAN"
    assert bundle["reviewer1_result"] is None
    mutation = next(f for f in bundle["findings"] if f["code"] == "PLAN_MUTATED_REPO")
    assert "concurrent writer may be the cause" in mutation["message"]
    assert "file1.py" in mutation["message"]
    assert not {"TASK_CONTRACT_NOT_GROUNDED", "WORKER_VALIDATION_UNVERIFIED"} & {
        f["code"] for f in bundle["findings"]
    }
    assert bundle["worker_validation"] is None
    stored = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
    assert stored["status"] == ledger.OK
    assert stored["record"]["live_thread"]["phase"] == "plan"
    assert Path(bundle["plan_path"]).read_text(encoding="utf-8") == content
    assert state in Path(bundle["plan_path"]).parents
    assert len(bundle["plan_summary"]) <= 4000


def test_plan_async_result(workspace, config):
    repo, state = workspace
    prompt = orch(phase="plan", route="plan_standard", scope=None) + "\nPlan."
    start, job_id, _ = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    pending = codex._read_pending(state, start)
    assert pending["async_updated_input"]["sandbox"] == "read-only"
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    post = {**result, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
                                   "content": "Summary:\nAsync plan."}}}
    bundle = response_bundle(codex.post_tool_use(post, config=config, state_root=state, repo_root=repo), config)
    assert bundle["phase"] == "plan" and bundle["plan_summary"] == "Async plan."
    assert bundle["reviewer1_skipped_reason"] == "PHASE_PLAN"
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["worker_validation"] is None
    assert not {"TASK_CONTRACT_NOT_GROUNDED", "WORKER_VALIDATION_UNVERIFIED"} & {
        f["code"] for f in bundle["findings"]
    }
    finished = [e["payload"] for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_FINISHED"]
    assert len(finished) == 1 and finished[0]["phase"] == "plan"


@pytest.mark.parametrize("unleased", (False, True))
def test_plan_attributes_only_foreign_leased_changes(workspace, config, unleased):
    repo, state = workspace
    plan = event(prompt=orch(phase="plan", route="plan_standard", scope=None) + "\nPlan.",
                 tool_use_id="plan-with-foreign")
    codex.pre_tool_use(plan, config=config, state_root=state, repo_root=repo)
    foreign = _attribution_call("foreign-plan-writer", "file1.py", "plan-foreign")
    codex.pre_tool_use(foreign, config=config, state_root=state, repo_root=repo)
    (repo / "file1.py").write_text("value = 2\n", encoding="utf-8")
    if unleased:
        (repo / "file2.py").write_text("value = 3\n", encoding="utf-8")
    post = {**plan, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"threadId": "plan-foreign-thread", "content": "Summary:\nPlan."}}}
    bundle = response_bundle(codex.post_tool_use(post, config=config, state_root=state,
                                                   repo_root=repo), config)
    assert bundle["claude_review_mode"] == policy.FULL
    artifact = json.loads(Path(bundle["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["known_concurrent_scopes"] == [
        {"path": "file1.py", "task_id": "foreign-plan-writer"}]
    mutations = [finding for finding in bundle["findings"]
                 if finding["code"] == "PLAN_MUTATED_REPO"]
    assert bool(mutations) == unleased
    if unleased:
        assert "file2.py" in mutations[0]["message"]
        assert "file1.py" not in mutations[0]["message"]


def test_plan_file_and_summary_caps(workspace):
    _, state = workspace
    report = "Summary:\n" + "x" * 16000
    bundle = {"findings": []}
    codex._store_plan(state, event(), "long-plan", report, bundle)
    assert len(Path(bundle["plan_path"]).read_text(encoding="utf-8")) == 15000
    assert len(bundle["plan_summary"]) == 4000
    assert bundle["findings"][0]["code"] == "PLAN_OUTPUT_TOO_LONG"


@pytest.mark.parametrize("name", ("codex_result", "codex_cancel"))
def test_async_unknown_and_foreign_jobs_are_denied(workspace, config, name):
    repo, state = workspace
    call = event(tool_name=f"mcp__plugin_purser_codex-main__{name}")
    call["tool_input"] = {"jobId": "0" * 64}
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert not list((state / "pending").glob("*.json"))
    start, job_id, record = async_started(workspace, config)
    call["tool_input"]["jobId"] = job_id
    call["session_id"] = "other-session"
    assert codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecision"] == "deny"


START_TOOL = "mcp__plugin_purser_codex-main__codex_start"
RESULT_TOOL = "mcp__plugin_purser_codex-main__codex_result"
CANCEL_TOOL = "mcp__plugin_purser_codex-main__codex_cancel"


def async_started(workspace, config, *, prompt=None, state_name="RUNNING", tool_use_id="async-original"):
    repo, state = workspace
    start = event(START_TOOL, prompt=prompt, tool_use_id=tool_use_id)
    updated = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    codex.post_tool_use({**start, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"jobId": job_id, "state": "RUNNING"}}},
                        config=config, state_root=state, repo_root=repo)
    record = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    record.parent.mkdir(parents=True, exist_ok=True)
    record.write_text(json.dumps({"jobId": job_id, "call": updated["call"], "state": state_name,
                                  "acceptedAt": 100.0, "finishedAt": 112.5,
                                  "threadId": "async-thread" if state_name == "SUCCEEDED" else None}), encoding="utf-8")
    return start, job_id, record


def async_followup(name, job_id):
    call = event(name, tool_use_id=f"followup-{name}")
    call["tool_input"] = {"jobId": job_id}
    return call


def _admitted_start_response(workspace, config, *, task_id, tool_use_id, label=None):
    repo, state = workspace
    start = event(START_TOOL, prompt=orch(task=task_id, scope="file0.py") + "\nWork.",
                  tool_use_id=tool_use_id, label=label or task_id)
    updated = codex.pre_tool_use(start, config=config, state_root=state,
                                 repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert "label" not in updated
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    response = codex.post_tool_use({**start, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"jobId": job_id, "state": "RUNNING"}}},
        config=config, state_root=state, repo_root=repo)
    return start, response


def _seed_live_thread(workspace, task_id):
    repo, state = workspace
    route = policy.resolve_route("normal", json.loads(CONFIG_PATH.read_text(encoding="utf-8")))
    record = ledger.build_session_record(
        task_id, f"thread-{task_id}", "normal", route.model, route.effort,
        phase="implementation", scope=["file0.py"], reason="new_task", label="existing task",
    )
    session_ledger = ledger.SessionLedger(
        codex._project_state_root(state, project_root.project_key(repo.resolve()))
    )
    assert session_ledger.save(record)["status"] == ledger.OK
    return session_ledger


def test_denied_fresh_start_does_not_poison_new_task_id(workspace, config):
    repo, state = workspace
    session_ledger = _seed_live_thread(workspace, "denied-task")
    before = session_ledger.load("denied-task")["record"]
    denied = event(
        START_TOOL, prompt=orch(task="denied-task", scope="file0.py") + "\nWork.",
        tool_use_id="denied-fresh-start", mode="new",
    )

    response = codex.pre_tool_use(denied, config=config, state_root=state, repo_root=repo)

    assert response["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert session_ledger.load("denied-task")["record"] == before
    retry = event(
        START_TOOL, prompt=orch(task="brand-new-task", scope="file0.py") + "\nWork.",
        tool_use_id="brand-new-after-denial", mode="new",
    )
    allowed = codex.pre_tool_use(retry, config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in allowed["hookSpecificOutput"]


@pytest.mark.parametrize("reason", ["user_override", "reanchor"])
def test_fresh_start_override_reason_allows_compatible_live_thread(workspace, config, reason):
    repo, state = workspace
    _seed_live_thread(workspace, f"override-{reason}")
    call = event(
        START_TOOL,
        prompt=orch(task=f"override-{reason}", reason=reason, scope="file0.py") + "\nWork.",
        tool_use_id=f"override-fresh-{reason}", mode="new",
    )

    response = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    assert "updatedInput" in response["hookSpecificOutput"]


def test_plain_new_task_fresh_start_still_denies_compatible_live_thread(workspace, config):
    repo, state = workspace
    _seed_live_thread(workspace, "plain-new-task")
    call = event(
        START_TOOL,
        prompt=orch(task="plain-new-task", reason="new_task", scope="file0.py") + "\nWork.",
        tool_use_id="plain-new-task-fresh", mode="new",
    )

    response = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    assert response["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "USE_CODEX_REPLY" in response["hookSpecificOutput"]["permissionDecisionReason"]


def test_dispatch_receipt_renders_english_in_approved_format(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *args, **kwargs:
        "Purser: typical Codex durations here (median/p90, n): normal 5/8 min (n=10).")
    receipt = codex._append_dispatch_receipt({}, {
        "task_id": "task-1", "label": "OpenCode and Codex reviewer adapters",
        "model": "gpt-6-luna", "effort": "high", "route": "normal",
        "project_key": "project", "repo_root": str(repo), "agent": "codex",
    }, state)
    assert receipt["systemMessage"] == (
        "**New session · model: gpt-6-luna (high) · task: OpenCode and Codex reviewer adapters · "
        "estimated completion: 5 min**"
    )


def test_dispatch_receipt_uses_continuing_session_wording(workspace, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *args, **kwargs: None)
    receipt = codex._append_dispatch_receipt({}, {
        "task_id": "task-1", "label": "Keep working", "model": "gpt-6-luna",
        "effort": "high", "route": "normal", "project_key": "project",
        "repo_root": str(repo), "agent": "codex", "prior_thread_id": "thread-1",
    }, state)
    assert receipt["systemMessage"] == (
        "**Continuing session · model: gpt-6-luna (high) · task: Keep working**"
    )


def test_dispatch_receipt_omits_estimate_without_duration_history(workspace, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *args, **kwargs: None)
    receipt = codex._append_dispatch_receipt({}, {
        "task_id": "task-1", "label": "First run", "model": "gpt-6-luna",
        "effort": "high", "route": "normal", "project_key": "project",
        "repo_root": str(repo), "agent": "codex",
    }, state)
    assert receipt["systemMessage"] == "**New session · model: gpt-6-luna (high) · task: First run**"


def test_two_admitted_codex_starts_emit_two_dispatch_receipts(workspace, config, monkeypatch):
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *args, **kwargs:
        "Purser: typical Codex durations here (median/p90, n): normal 2/6 min (n=10).")
    first = _admitted_start_response(
        workspace, config, task_id="receipt-first", tool_use_id="receipt-first-start")[1]
    second = _admitted_start_response(
        workspace, config, task_id="receipt-second", tool_use_id="receipt-second-start")[1]
    messages = [first.get("systemMessage", ""), second.get("systemMessage", "")]
    assert sum(message.count("**New session · model: gpt-6-sol (medium) · task:") for message in messages) == 2
    assert "task: receipt-first · estimated completion: 2 min**" in messages[0]
    assert "task: receipt-second · estimated completion: 2 min**" in messages[1]


def test_denied_codex_start_emits_no_dispatch_receipt(workspace, config):
    repo, state = workspace
    start = event(START_TOOL, prompt=orch(scope="file0.py") + "\nWork.",
                  tool_use_id="receipt-denied")
    start["tool_input"].pop("label")
    response = codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)
    assert response["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "label" in response["hookSpecificOutput"]["permissionDecisionReason"]
    assert "fix" in response["hookSpecificOutput"]["permissionDecisionReason"].casefold()


def test_async_implementation_starts_with_overlapping_scope_are_admitted(workspace, config):
    repo, state = workspace
    nested = repo / "tests"
    nested.mkdir()
    starts = [event(START_TOOL, prompt=orch(task=f"overlap-{n}", scope="tests/") + "\nWork.",
                    tool_use_id=f"overlap-start-{n}", cwd=str(nested)) for n in range(2)]
    updated = [codex.pre_tool_use(start, config=config, state_root=state, repo_root=repo)
               ["hookSpecificOutput"]["updatedInput"] for start in starts]
    assert all(item["worktree"] is True and "leaseQueued" not in item for item in updated)
    assert all(Path(item["cwd"]) == nested for item in updated)
    leases = ledger.ScopeLeases(codex._project_state_root(state, project_root.project_key(repo)))
    assert len(leases.overlapping_owners(codex._call_key(starts[0]))) == 1
    assert all(owner["state"] == "ACTIVE" for owner in leases._read()["owners"].values())


def test_async_worktree_delta_and_apply_use_only_job_changes(workspace, config):
    repo, state = workspace
    start, job_id, job_path = async_started(
        workspace, config, prompt=orch(task="worktree-delta", scope="file1.py") + "\nWork."
    )
    snapshot = ledger.create_job_worktree(repo, job_id, state / "wt")
    (Path(snapshot["path"]) / "file1.py").write_text("job = True\n", encoding="utf-8")
    (repo / "file0.py").write_text("shared = True\n", encoding="utf-8")
    job = json.loads(job_path.read_text(encoding="utf-8"))
    job.update(state="SUCCEEDED", threadId="async-thread", worktreePath=snapshot["path"], snapshot=snapshot["snapshot"],
               head=snapshot["head"], repoRoot=str(repo))
    job_path.write_text(json.dumps(job), encoding="utf-8")
    followup = async_followup(RESULT_TOOL, job_id)
    response = codex.post_tool_use({**followup, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
                              "content": "done", "worktreePath": snapshot["path"]}}},
        config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(response, config)
    assert bundle["changed_paths"] == ["file1.py"]
    assert bundle["apply"] == "codex_apply"
    assert "codex_apply" in response["systemMessage"]


def test_dirty_worktree_snapshot_reviews_only_worker_diff(workspace, config, monkeypatch):
    repo, state = workspace
    (repo / "file0.py").write_text("preexisting = True\n", encoding="utf-8")
    start, job_id, job_path = async_started(
        workspace, config, prompt=orch(task="dirty-snapshot", scope="file1.py") + "\nWork."
    )
    snapshot = ledger.create_job_worktree(repo, job_id, state / "wt")
    worktree = Path(snapshot["path"])
    (worktree / "file1.py").write_text("worker = True\n", encoding="utf-8")
    job = json.loads(job_path.read_text(encoding="utf-8"))
    job.update(state="SUCCEEDED", threadId="async-thread", worktreePath=snapshot["path"],
               snapshot=snapshot["snapshot"], head=snapshot["head"], repoRoot=str(repo))
    job_path.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    packets = []
    def reviewer(**kwargs):
        packets.append(reviewer_module._delegated_diff(kwargs["repo_root"], kwargs["delta"]))
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "provider": "agy"}
    followup = async_followup(RESULT_TOOL, job_id)
    response = codex.post_tool_use({**followup, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
                              "content": "done", "worktreePath": snapshot["path"]}}},
        config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    bundle = response_bundle(response, config)
    assert bundle["changed_paths"] == ["file1.py"]
    assert bundle["delta_files"] == 1 and bundle["delta_lines"] == 2
    assert packets and packets[0][0] == ["file1.py"]
    assert "preexisting" not in packets[0][1]
    assert not {"OUT_OF_SCOPE_MODIFICATION", "UNKNOWN_CONCURRENT_WRITER", "REVIEW_PACKET_INCOMPLETE"} & {
        finding["code"] for finding in bundle["findings"]}


def test_operation_concurrent_commit_is_not_worker_delta(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(phase="operation", route="operation", scope=None) + "\nInspect.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("external = True\n", encoding="utf-8")
    subprocess.run(["git", "add", "file0.py"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "external"], cwd=repo, check=True, timeout=10)
    response = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "operation-thread", "content": "done"}}},
        config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(response, config)
    assert bundle["changed_paths"] is None and bundle["delta_files"] is None
    assert any(f["code"] == "EXTERNAL_REPOSITORY_CHANGE" for f in bundle["findings"])


def test_clean_sync_implementation_delta_unchanged(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(scope="file0.py") + "\nUpdate file0.")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("worker = True\n", encoding="utf-8")
    response = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
        {"structuredContent": {"threadId": "sync-thread", "content": "done"}}},
        config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(response, config)
    assert bundle["changed_paths"] == ["file0.py"]
    assert bundle["delta_files"] == 1 and bundle["delta_lines"] == 2
    assert not any(f["code"] == "EXTERNAL_REPOSITORY_CHANGE" for f in bundle["findings"])


def test_async_worktree_reply_is_admitted_during_overlapping_job(workspace, config, monkeypatch):
    repo, state = workspace
    async_started(
        workspace, config, prompt=orch(task="active-worktree", scope="tests/") + "\nWork."
    )
    worktree = state / "wt" / "reply-thread"
    worktree.mkdir(parents=True)
    bridge_threads = state / "bridge-threads"
    monkeypatch.setattr(codex_exec_mcp, "_state_dir", lambda: bridge_threads)
    codex_exec_mcp._save_record(bridge_threads, {"thread_id": "worktree-thread", "cwd": str(worktree),
        "config_args": [], "worktreePath": str(worktree), "snapshot": "snapshot", "head": "head",
        "repoRoot": str(repo)})
    route = policy.resolve_route("normal", config)
    saved = ledger.SessionLedger(codex._project_state_root(state, project_root.project_key(repo))).save(
        ledger.build_session_record("worktree-reply", "worktree-thread", "normal", route.model,
                                    route.effort, phase="implementation", scope=["tests/"],
                                    reason="new_task"))
    assert saved["status"] == ledger.OK
    reply = event(START_TOOL, prompt=orch(task="worktree-reply", scope="tests/") + "\nContinue.",
                  tool_use_id="worktree-reply-start", mode="reply", threadId="worktree-thread")
    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    updated = result["hookSpecificOutput"]["updatedInput"]
    assert updated["mode"] == "reply"
    assert "leaseQueued" not in updated


def test_worktree_flag_does_not_change_sync_plan_or_operation_calls(workspace, config):
    repo, state = workspace
    sync = codex.pre_tool_use(event(prompt=orch(task="sync-worktree", scope="file0.py") + "\nWork."),
                              config=config, state_root=state, repo_root=repo)
    plan = codex.pre_tool_use(event(START_TOOL,
        prompt=orch(task="plan-worktree", phase="plan", route="plan_standard", scope=None) + "\nPlan.",
        tool_use_id="plan-worktree"), config=config, state_root=state, repo_root=repo)
    operation = codex.pre_tool_use(event(START_TOOL,
        prompt=orch(task="operation-worktree", phase="operation", route="operation", scope=None) + "\nInspect.",
        tool_use_id="operation-worktree"), config=config, state_root=state, repo_root=repo)
    assert "worktree" not in sync["hookSpecificOutput"]["updatedInput"]
    assert "worktree" not in plan["hookSpecificOutput"]["updatedInput"]
    assert "worktree" not in operation["hookSpecificOutput"]["updatedInput"]


def test_async_result_uses_indexed_project_when_event_cwd_is_nested_repo(workspace, config, monkeypatch):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    nested = repo / ".tmp" / "nested-repo"
    nested.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=nested, check=True, timeout=10)
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    followup = async_followup(RESULT_TOOL, job_id)
    followup["cwd"] = str(nested)
    assert codex.pre_tool_use(followup, config=config, state_root=state) == {}
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    result = codex.post_tool_use(post, config=config, state_root=state,
                                 reviewer=lambda **kwargs: {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                            "provider": "agy"})
    assert response_bundle(result, config)["changed_paths"] == ["file0.py"]
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))["stage"] == "CONSUMED"


def _board_job(state_root, name, status="RUNNING", **values):
    job_id = hashlib.sha256(name.encode()).hexdigest()
    call_key = hashlib.sha256((name + "-call").encode()).hexdigest()
    index = {"jobId": job_id, "call": name, "call_key": call_key,
             "project_key": "project", "session_id": "session"}
    job = {"jobId": job_id, "call": name, "state": status, "acceptedAt": time.time(),
           "label": name, "model": "gpt-6-sol", "effort": "medium", "route": "normal",
           **values}
    (state_root / "async_jobs").mkdir(parents=True, exist_ok=True)
    (state_root / "codex_exec_mcp" / "jobs").mkdir(parents=True, exist_ok=True)
    (state_root / "async_jobs" / f"{job_id}.json").write_text(json.dumps(index), encoding="utf-8")
    (state_root / "codex_exec_mcp" / "jobs" / f"{job_id}.json").write_text(json.dumps(job), encoding="utf-8")
    return job_id, call_key


def test_project_job_board_is_omitted_with_one_job(workspace):
    _, state = workspace
    _board_job(state, "only")
    response = codex._append_project_job_status({}, state, "project")
    assert "systemMessage" not in response


def test_project_job_board_orders_attention_running_queued_and_applied(workspace):
    _, state = workspace
    rows = [
        ("applied", "SUCCEEDED", {"applyState": "applied", "appliedAt": time.time()}),
        ("queued", "QUEUED", {}),
        ("running", "RUNNING", {}),
        ("done", "SUCCEEDED", {}),
        ("conflict", "SUCCEEDED", {"applyState": "conflict"}),
        ("failed", "FAILED", {"finishedAt": time.time()}),
    ]
    for label, state_name, values in rows:
        _, call_key = _board_job(state, label, state_name, **values)
        (state / "bundle_artifacts").mkdir(exist_ok=True)
        (state / "bundle_artifacts" / f"{call_key}.json").write_text(json.dumps({
            "phase": "implementation", "apply": "codex_apply",
            "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
        }), encoding="utf-8")
    message = codex._append_project_job_status({}, state, "project")["systemMessage"]
    tasks = [line.strip("|").split("|")[0].strip() for line in message.splitlines()
             if line.startswith("| ") and not line.startswith("| Task |") and "---" not in line]
    assert tasks == ["failed", "conflict", "done", "running", "queued", "applied"]
    assert "| Task | Model | State | ETA | Review | Apply |" in message


def test_project_job_board_renders_overdue_and_unknown_eta(workspace, monkeypatch):
    _, state = workspace
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *args, **kwargs: None)
    _board_job(state, "overdue", etaMinutes=2, acceptedAt=time.time() - 180)
    _board_job(state, "unknown", etaMinutes=None, acceptedAt=time.time() - 10)
    message = codex._append_project_job_status({}, state, "project")["systemMessage"]
    assert "| overdue | gpt-6-sol (medium) | running | overdue |" in message
    assert "| unknown | gpt-6-sol (medium) | running | — |" in message


def test_project_job_board_drops_applied_jobs_after_an_hour(workspace):
    _, state = workspace
    _board_job(state, "old-applied", "SUCCEEDED", applyState="applied",
               appliedAt=time.time() - 3601)
    _board_job(state, "old-failed", "FAILED", finishedAt=time.time() - 3601)
    _board_job(state, "active-one")
    _board_job(state, "active-two")
    message = codex._append_project_job_status({}, state, "project")["systemMessage"]
    assert "old-applied" not in message
    assert "old-failed" not in message
    assert "active-one" in message and "active-two" in message


def test_async_dispatch_receipt_is_followed_by_project_job_board(workspace):
    _, state = workspace
    _board_job(state, "dispatch-one")
    _board_job(state, "dispatch-two")
    response = codex._append_dispatch_receipt({"systemMessage": "accepted"}, {
        "label": "new work", "model": "gpt-6-sol", "effort": "medium",
        "route": "normal", "async_call": "call", "project_key": "project",
    }, state)
    message = response["systemMessage"]
    assert message.index("task: new work") < message.index("Purser jobs:")


def test_async_result_response_appends_project_job_board(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs:
                        policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    jobs = []
    for number, job_state in ((1, "SUCCEEDED"), (2, "RUNNING")):
        _, job_id, path = async_started(workspace, config, prompt=orch(task=f"board-result-{number}") + "\nWork.",
                                         state_name=job_state, tool_use_id=f"board-result-{number}")
        record = json.loads(path.read_text(encoding="utf-8"))
        record.update(label=f"result-{number}", model="gpt-6-sol", effort="medium",
                      route="normal", etaMinutes=5)
        path.write_text(json.dumps(record), encoding="utf-8")
        jobs.append(job_id)
    followup = async_followup(RESULT_TOOL, jobs[0])
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"jobId": jobs[0], "state": "SUCCEEDED",
                              "threadId": "async-thread", "content": "done"}}}
    response = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                                   reviewer=lambda **kwargs: {
                                       "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                       "provider": "agy"})
    assert "| Task | Model | State | ETA | Review | Apply |" in response["systemMessage"]
    assert "| result-2 | gpt-6-sol (medium) | running |" in response["systemMessage"]


def test_codex_apply_response_appends_project_job_board(workspace, config):
    _, state = workspace
    job_id, _ = _board_job(state, "applied-job", "SUCCEEDED", applyState="applied",
                           appliedAt=time.time())
    _board_job(state, "still-running")
    event_value = {"tool_name": "mcp__plugin_purser_codex-main__codex_apply",
                   "tool_input": {"jobId": job_id}}
    assert codex.pre_tool_use(event_value, config=config, state_root=state) == {}
    response = codex.post_tool_use(event_value, config=config, state_root=state)
    assert "| Task | Model | State | ETA | Review | Apply |" in response["systemMessage"]
    assert "| applied-job | gpt-6-sol (medium) | done | — | — | applied |" in response["systemMessage"]


def test_async_result_waits_for_terminal_and_internal_error_fails_open(workspace, config):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config)
    result = async_followup(RESULT_TOOL, job_id)
    denied = codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo)
    assert "still running; wait for the completion signal" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert not list((state / "async_jobs").glob("*.claim*"))
    bypass = codex.internal_pre_error_response(result, RuntimeError("internal"), state_root=state)
    assert "additionalContext" in bypass["hookSpecificOutput"]


@pytest.mark.parametrize("response_shape", ("string", "blocks", "mapping"))
def test_async_result_finalizes_once_from_start_baseline(workspace, config, monkeypatch, response_shape):
    repo, state = workspace
    prompt = orch(scope="file0.py") + "|eta=4m\nTASK_CONTRACT: {\"objective\": \"Update file0\", \"criteria\": \"file0 is updated\"}"
    start, job_id, record = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    (repo / "file1.py").write_text("concurrent = True\n", encoding="utf-8")
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    reviews = []
    def reviewer(**kwargs):
        reviews.append(kwargs)
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "provider": "agy"}
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    value = {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
             "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":[],"exit_codes":[]}'}
    response = {
        "string": json.dumps(value),
        "blocks": [{"type": "text", "text": json.dumps(value)}],
        "mapping": {"structuredContent": value},
    }[response_shape]
    post = {**result, "hook_event_name": "PostToolUse", "duration_ms": 1, "tool_response": response}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    bundle = response_bundle(first, config)
    assert bundle["thread_id"] == "async-thread" and bundle["concurrent_writer"] is True
    assert "file0.py" in bundle["changed_paths"] and "file1.py" in bundle["changed_paths"]
    assert len(reviews) == 1
    finished = [e["payload"] for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_FINISHED"]
    assert len(finished) == 1 and finished[0]["duration_ms"] == 12500 and finished[0]["eta_minutes"] == 4
    index = json.loads((state / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
    assert index["stage"] == "CONSUMED" and Path(index["bundle_path"]).is_file()
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer) == first
    assert len(reviews) == 1
    assert not codex._pending_path(state, start).exists()


def test_recovered_job_reviews_original_baseline_and_releases_lease_after_consumed(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, prompt=orch(scope="file0.py,file1.py") + "\nUpdate both files.")
    original_call = json.loads(record.read_text(encoding="utf-8"))["call"]
    (repo / "file0.py").write_text("before = True\n", encoding="utf-8")
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, hostCreatedAt=456,
               hostContained=True, deadlineAt=time.time() - 1, signalToken="secret")
    record.write_text(json.dumps(job), encoding="utf-8")
    record.with_suffix(".launch").write_text(json.dumps({"name": "codex", "args": {
        "prompt": "Update both files.", "timeoutSeconds": 30}, "codexBin": "codex"}), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    launches = []
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *args: launches.append(args))
    async_session_maintenance.cleanup_jobs(state)
    recovered = json.loads(record.read_text(encoding="utf-8"))
    assert len(launches) == 1 and recovered["state"] == "QUEUED"
    assert recovered["call"] == original_call and recovered["generation"] == 2
    owner = codex._call_key(start)
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
    assert codex._pending_path(state, start).is_file()

    server = codex_exec_mcp.Server(job_dir=record.parent)
    server.claim_generation = recovered["generation"]
    server.claim_token = recovered["ownershipToken"]
    def fake_codex(*_args, **_kwargs):
        (repo / "file1.py").write_text("after = True\n", encoding="utf-8")
        return {"structuredContent": {"threadId": "resumed-thread", "content": "done"}}
    monkeypatch.setattr(server, "_run_tool", fake_codex)
    server._job_worker(job_id, "codex", json.loads(record.with_suffix(".launch").read_text())["args"])
    result = server._result_job({"jobId": job_id})
    assert result["structuredContent"]["state"] == "SUCCEEDED"
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
    followup = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(followup, config=config, state_root=state, repo_root=repo) == {}
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": result}
    reviews = []
    def reviewer(**kwargs):
        reviews.append(kwargs)
        assert _lease_state(workspace)._read()["owners"][owner]["state"] == "ACTIVE"
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "provider": "agy"}
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    bundle = response_bundle(first, config)
    assert set(bundle["changed_paths"]) == {"file0.py", "file1.py"}
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert _lease_state(workspace)._read()["owners"][owner]["state"] == "RELEASED"
    assert len(reviews) == 1 and not codex._pending_path(state, start).exists()
    async_session_maintenance.cleanup_jobs(state)
    assert len(launches) == 1
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                               reviewer=lambda **kwargs: pytest.fail("second review")) == first


def test_failed_recovery_finalizes_through_ambiguous_path(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config)
    job = json.loads(record.read_text(encoding="utf-8"))
    job.update(generation=1, ownershipToken="old", hostPid=123, hostCreatedAt=456,
               hostContained=True, deadlineAt=time.time() - 1)
    record.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(codex_exec_mcp.Server, "_classify_host", lambda self, job: "DEAD")
    async_session_maintenance.cleanup_jobs(state)
    assert json.loads(record.read_text())["state"] == "UNKNOWN_OR_IN_PROGRESS"
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "ACTIVE"
    result = codex_exec_mcp.Server(job_dir=record.parent)._result_job({"jobId": job_id})
    followup = async_followup(RESULT_TOOL, job_id)
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": result}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(first, config)
    assert bundle["ambiguous_outcome"] is True and bundle["claude_review_mode"] == policy.FULL
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "RELEASED"


def test_new_session_result_requires_adoption_and_original_baseline(workspace, config):
    repo, state = workspace
    prompt = orch(task="original-task", scope="file0.py") + "\nDo the work."
    start, job_id, _ = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 9\n", encoding="utf-8")
    followup = async_followup(RESULT_TOOL, job_id)
    followup["session_id"] = "new-session"
    denied = codex.pre_tool_use(followup, config=config, state_root=state, repo_root=repo)
    assert "ASYNC_ADOPTION_REQUIRES_USER_CONSENT" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    followup["tool_input"]["adopt"] = True
    assert codex.pre_tool_use(followup, config=config, state_root=state, repo_root=repo) == {}
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(first, config)
    assert bundle["task_id"] == "original-task"
    assert bundle["changed_paths"] == ["file0.py"]
    assert Path(bundle["raw_output_path"]).stem == codex._call_key(start)
    assert not codex._pending_path(state, start).exists()
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "RELEASED"
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                               reviewer=lambda **kwargs: pytest.fail("second review")) == first


def test_new_session_adoption_rejects_wrong_project_and_mismatched_chain(workspace, config, tmp_path):
    repo, state = workspace
    start, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    followup = async_followup(RESULT_TOOL, job_id)
    followup.update(session_id="new-session", tool_input={"jobId": job_id, "adopt": True})
    wrong = tmp_path / "other-repo"
    wrong.mkdir()
    assert "UNKNOWN_ASYNC_JOB" in codex.pre_tool_use(
        followup, config=config, state_root=state, repo_root=wrong)["hookSpecificOutput"]["permissionDecisionReason"]
    pending_path = codex._pending_path(state, start)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["job_id"] = "0" * 64
    pending_path.write_text(json.dumps(pending), encoding="utf-8")
    assert "UNKNOWN_ASYNC_JOB" in codex.pre_tool_use(
        followup, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]


def test_interrupted_first_finalize_then_new_session_adoption_recovers_bundle(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 5\n", encoding="utf-8")
    monkeypatch.setattr(policy, "reviewer1_gate", lambda **kwargs: policy.RunReviewerDecision("run reviewer", "TEST", "test"))
    original = async_followup(RESULT_TOOL, job_id)
    post = {**original, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    write = codex._atomic_json_write
    reviews = []
    failures = []
    def interrupt_once(path, value):
        if path == state / "async_jobs" / f"{job_id}.json" and value.get("stage") == "CONSUMED" and not failures:
            failures.append(True)
            raise OSError("interrupted before consumed record")
        return write(path, value)
    monkeypatch.setattr(codex, "_atomic_json_write", interrupt_once)
    reviewer = lambda **kwargs: reviews.append(kwargs) or {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo, reviewer=reviewer)
    assert response_bundle(first, config)["status"] == "ALREADY_CONSUMED_OR_INTERRUPTED"
    assert len(reviews) == 1
    adopted = {**post, "session_id": "new-session", "tool_input": {"jobId": job_id, "adopt": True}}
    assert codex.pre_tool_use({**adopted, "hook_event_name": "PreToolUse"},
                              config=config, state_root=state, repo_root=repo) == {}
    second = codex.post_tool_use(adopted, config=config, state_root=state, repo_root=repo,
                                 reviewer=lambda **kwargs: pytest.fail("second review"))
    assert response_bundle(second, config)["task_id"] == "task-1"
    assert len(reviews) == 1
    assert codex.post_tool_use(adopted, config=config, state_root=state, repo_root=repo) == second
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "RELEASED"


def test_finalizing_crash_before_bundle_survives_cleanup_and_recovers(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, state_name="SUCCEEDED")
    job = json.loads(record.read_text(encoding="utf-8"))
    job["finishedAt"] = time.time() - 8 * 86400
    record.write_text(json.dumps(job), encoding="utf-8")
    result = async_followup(RESULT_TOOL, job_id)
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    original_job = codex._async_job
    failed = []
    def crash_once(*args):
        if not failed:
            failed.append(True)
            raise OSError("crash after FINALIZING")
        return original_job(*args)
    monkeypatch.setattr(codex, "_async_job", crash_once)
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert response_bundle(first, config)["status"] == "ALREADY_CONSUMED_OR_INTERRUPTED"
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "FINALIZING"
    async_session_maintenance.cleanup_jobs(state)
    assert record.exists() and codex._pending_path(state, start).exists()
    second = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert response_bundle(second, config)["task_id"] == "task-1"
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"


def test_stale_pending_cleanup_keeps_async_baseline_without_lease_field(workspace, config):
    repo, state = workspace
    start, job_id, _ = async_started(workspace, config)
    path = codex._pending_path(state, start)
    pending = json.loads(path.read_text(encoding="utf-8"))
    pending.pop("lease_owner", None)
    path.write_text(json.dumps(pending), encoding="utf-8")
    old = time.time() - codex.PENDING_MAX_AGE_SECONDS - 60
    os.utime(path, (old, old))
    codex._cleanup_stale_pending(state, event(tool_use_id="another-call"))
    assert path.exists() and pending["job_id"] == job_id


@pytest.mark.parametrize("state_name", ("FAILED", "TIMED_OUT", "CANCELLED", "UNKNOWN_OR_IN_PROGRESS", "EXPIRED", "UNREADABLE"))
def test_async_terminal_failure_uses_ambiguous_path(workspace, config, state_name):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, state_name=state_name)
    if state_name == "UNREADABLE":
        record.write_text("{bad", encoding="utf-8")
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    post = {**result, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"jobId": job_id, "state": state_name, "errorCode": "CODEX_FAILED"}}}
    output = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert "FULL" in output["hookSpecificOutput"]["additionalContext"]
    assert not codex._pending_path(state, start).exists()
    assert any(e["event_type"] == "IMPLEMENTER_FAILED" for e in _all_telemetry_events(state))


@pytest.mark.parametrize("tool_failed", (False, True))
def test_async_cancel_uses_failure_path_and_cleanup(workspace, config, tool_failed):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config)
    call = async_followup(CANCEL_TOOL, job_id)
    assert codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo) == {}
    if tool_failed:
        response = codex.post_tool_use_failure({**call, "hook_event_name": "PostToolUseFailure", "error": "cancel failed"},
                                               config=config, state_root=state, repo_root=repo)
    else:
        response = codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response":
                                        {"structuredContent": {"jobId": job_id, "state": "CANCELLED"}}},
                                       config=config, state_root=state, repo_root=repo)
    assert "FULL" in response["hookSpecificOutput"]["additionalContext"]
    assert not codex._pending_path(state, start).exists()


def test_async_cleanup_crash_does_not_repeat_finalization(workspace, config, monkeypatch):
    repo, state = workspace
    start, job_id, record = async_started(workspace, config, state_name="SUCCEEDED")
    (repo / "file0.py").write_text("value = 3\n", encoding="utf-8")
    followup = async_followup(RESULT_TOOL, job_id)
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    remove = codex._remove_pending
    calls = []
    def crash_once(root, event):
        if not calls:
            calls.append(True)
            raise OSError("simulated cleanup crash")
        return remove(root, event)
    monkeypatch.setattr(codex, "_remove_pending", crash_once)
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                                reviewer=lambda **kwargs: calls.append("review") or {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE})
    second = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                                 reviewer=lambda **kwargs: pytest.fail("second review"))
    assert second == first and not codex._pending_path(state, start).exists()


def test_async_real_shaped_token_report_is_recorded(workspace, config, monkeypatch):
    repo, state = workspace
    path = "scripts/codex_token_report.py"
    prompt = orch(scope=f"file0.py,{path}") + "\nUpdate the token report."
    _, job_id, _ = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    capture = ledger.capture_baseline
    def dirty_token_report(*args, **kwargs):
        snapshot = capture(*args, **kwargs)
        snapshot["tracked_snapshots"][path] = {
            "status": [" M"], "index": [], "worktree": {"kind": "file"}}
        return snapshot
    monkeypatch.setattr(ledger, "capture_baseline", dirty_token_report)
    result = async_followup(RESULT_TOOL, job_id)
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread",
        "content": "Set tool_output_token_limit to 8000."}}}
    response = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(response, config)
    assert not any(f["code"].startswith("LEDGER_") for f in bundle["findings"])
    stored = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")
    assert stored["status"] == ledger.OK
    assert stored["record"]["last_outcome"]["raw_output_path"] == bundle["raw_output_path"]


def test_async_ledger_rejection_returns_typed_full_bundle(workspace, config, monkeypatch):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    result = async_followup(RESULT_TOOL, job_id)
    assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
    monkeypatch.setattr(ledger.SessionLedger, "save", lambda self, record: {
        "status": "ERROR", "code": "LEDGER_RECORD_INVALID", "message": "rejected"})
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(first, config)
    assert bundle["claude_review_mode"] == "FULL"
    assert any(f["code"] == "LEDGER_RECORD_REJECTED" and "LEDGER_RECORD_INVALID" in f["message"]
               for f in bundle["findings"])
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"
    assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo) == first


@pytest.mark.parametrize("reject_ledger", (False, True))
def test_async_crash_after_bundle_retries_without_second_review(workspace, config, monkeypatch, reject_ledger):
    repo, state = workspace
    _, job_id, _ = async_started(workspace, config, state_name="SUCCEEDED")
    result = async_followup(RESULT_TOOL, job_id)
    post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
    if reject_ledger:
        monkeypatch.setattr(ledger.SessionLedger, "save", lambda self, record: {
            "status": "ERROR", "code": "LEDGER_RECORD_INVALID", "message": "SECRET_VALUE_PATTERN at last_outcome"})
    write = codex._atomic_json_write
    failures = []
    def crash_before_consumed(path, value):
        if path == state / "async_jobs" / f"{job_id}.json" and value.get("stage") == "CONSUMED" and not failures:
            failures.append(True)
            raise OSError("simulated finalizer crash")
        return write(path, value)
    monkeypatch.setattr(codex, "_atomic_json_write", crash_before_consumed)
    first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert response_bundle(first, config)["status"] == "ALREADY_CONSUMED_OR_INTERRUPTED"
    second = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(second, config)
    assert bundle["thread_id"] == "async-thread"
    assert any(f["code"] == "LEDGER_RECORD_REJECTED" for f in bundle["findings"]) == reject_ledger
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text())["stage"] == "CONSUMED"


def test_two_parallel_async_jobs_finalize_independently(workspace, config):
    repo, state = workspace
    jobs = []
    for number in (1, 2):
        prompt = orch(task=f"parallel-{number}", scope=f"file{number - 1}.py") + "\nDo the work."
        _, job_id, _ = async_started(workspace, config, prompt=prompt,
                                     state_name="SUCCEEDED", tool_use_id=f"start-{number}")
        jobs.append(job_id)
    for number, job_id in enumerate(jobs, 1):
        result = async_followup(RESULT_TOOL, job_id)
        result["tool_use_id"] = f"result-{number}"
        assert codex.pre_tool_use(result, config=config, state_root=state, repo_root=repo) == {}
        post = {**result, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
            "jobId": job_id, "state": "SUCCEEDED", "threadId": "async-thread", "content": "done"}}}
        first = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
        assert response_bundle(first, config)["task_id"] == f"parallel-{number}"
        assert codex.post_tool_use(post, config=config, state_root=state, repo_root=repo) == first


def _lease_call(task, scope, tool_use_id, *, tool_name=codex.CODEX_TOOL):
    return event(tool_name, prompt=orch(task=task, scope=scope) + "\nDo the work.",
                 tool_use_id=tool_use_id)


def _lease_state(workspace):
    repo, state = workspace
    return ledger.ScopeLeases(state / "projects" / project_root.project_key(repo.resolve()))


def _attribution_call(task, scope, suffix):
    prompt = (orch(task=task, scope=scope) + "\n"
              'TASK_CONTRACT: {"objective":"Edit the owned file","criteria":"Review only that file"}')
    return event(prompt=prompt, tool_use_id=f"attribution-{suffix}")


def _finish_attribution_call(call, workspace, config, observed):
    repo, state = workspace
    def review(**kwargs):
        observed.append(reviewer_module._delegated_diff(
            repo, kwargs["delta"], kwargs["classification"]))
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS", "findings": []}
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"threadId": call["tool_use_id"], "content":
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["focused"],"exit_codes":[0]}'}}}
    return response_bundle(codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=review), config)


def test_single_job_compact_bundle_omits_whole_tree_fields(workspace, config):
    repo, state = workspace
    call = _attribution_call("single", "file0.py", "single")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    bundle = _finish_attribution_call(call, workspace, config, [])
    assert not {"whole_tree_delta", "whole_tree_anomaly", "known_concurrent_scopes"} & bundle.keys()
    artifact = json.loads(Path(bundle["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["whole_tree_delta"]["changed_paths"] == ["file0.py"]
    assert artifact["known_concurrent_scopes"] == []


def test_code_map_line_survives_worker_prompt(workspace, config):
    repo, state = workspace
    line = r"CODE_MAP=C:\Users\Example User\code-map\map.txt STATUS=FRESH"
    call = event(prompt=orch() + "\nDo the work.\n" + line, tool_use_id="map-prompt")
    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert line in pre["hookSpecificOutput"]["updatedInput"]["prompt"].splitlines()


def test_code_map_stale_after_mutating_job(workspace, config):
    repo, state = workspace
    call = _attribution_call("map-mutation", "file0.py", "map-mutation")
    call["tool_input"]["prompt"] += "\nCODE_MAP=C:\\state\\code-map\\map.txt STATUS=FRESH"
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    bundle = _finish_attribution_call(call, workspace, config, [])
    assert bundle["code_map"] == "STALE"


def test_code_map_field_absent_without_map_line(workspace, config):
    repo, state = workspace
    call = _attribution_call("no-map", "file0.py", "no-map")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    bundle = _finish_attribution_call(call, workspace, config, [])
    assert "code_map" not in bundle


def test_known_concurrent_scope_summary_bounds_task_ids(config):
    bundle = codex._base_bundle()
    bundle["known_concurrent_scopes"] = [
        {"path": f"file{n}.py", "task_id": f"task-{n}"} for n in range(8)]
    bundle["whole_tree_delta"] = {"changed_paths": [f"file{n}.py" for n in range(10000)]}
    bundle["whole_tree_anomaly"] = {"paths": [f"file{n}.py" for n in range(10000)]}
    compact, overflow = codex._compact_bundle(bundle, config["bundle_non_report_max_bytes"])
    assert not overflow
    assert compact["known_concurrent_scopes"] == {
        "count": 8, "task_ids": [f"task-{n}" for n in range(5)]}
    assert "whole_tree_delta" not in compact and "whole_tree_anomaly" not in compact


def test_concurrent_leased_jobs_get_only_their_own_diff(workspace, config):
    repo, state = workspace
    calls = [_attribution_call(f"owned-{n}", f"file{n}.py", n) for n in (0, 1)]
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0,
                                                 "small_delta_max_files": 0})
    for call in calls:
        codex.pre_tool_use(call, config=review_config, state_root=state, repo_root=repo)
    for n in (0, 1):
        (repo / f"file{n}.py").write_text(f"value = {n + 2}\n", encoding="utf-8")
    for n, call in enumerate(calls):
        observed = []
        bundle = _finish_attribution_call(call, workspace, review_config, observed)
        assert observed and observed[0][0] == [f"file{n}.py"]
        assert bundle["changed_paths"] == [f"file{n}.py"]
        assert bundle["delta_files"] == 1
        assert "whole_tree_delta" not in bundle and "whole_tree_anomaly" not in bundle
        assert bundle["known_concurrent_scopes"] == {
            "count": 1, "task_ids": [f"owned-{1-n}"]}
        artifact = json.loads(Path(bundle["full_bundle_path"]).read_text(encoding="utf-8"))
        assert artifact["whole_tree_delta"]["changed_file_count"] == 2
        assert f"file{1-n}.py" in artifact["whole_tree_anomaly"]["paths"]
        assert artifact["known_concurrent_scopes"] == [
            {"path": f"file{1-n}.py", "task_id": f"owned-{1-n}"}]
        finished = [item for item in _all_telemetry_events(state)
                    if item["event_type"] == "IMPLEMENTER_FINISHED"
                    and item["task_id"] == f"owned-{n}"]
        assert finished[-1]["payload"]["known_concurrent_scopes"] == artifact["known_concurrent_scopes"]
        assert not {"OUT_OF_SCOPE_MODIFICATION", "UNKNOWN_CONCURRENT_WRITER"} & {
            finding["code"] for finding in bundle["findings"]}


def test_reply_lease_attributes_same_task_repair_during_foreign_result(workspace, config):
    repo, state = workspace
    foreign = _attribution_call("foreign", "file1.py", "reply-foreign")
    first = _attribution_call("repair", "file0.py", "reply-first")
    codex.pre_tool_use(foreign, config=config, state_root=state, repo_root=repo)
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    _finish_attribution_call(first, workspace, config, [])
    assert _lease_state(workspace)._read()["owners"][codex._call_key(first)]["state"] == "RELEASED"

    reply = event(START_TOOL, prompt="Continue the repair.", tool_use_id="reply-lease",
                  mode="reply", threadId=first["tool_use_id"])
    pre = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in pre["hookSpecificOutput"]
    owner = _lease_state(workspace)._read()["owners"][codex._call_key(reply)]
    assert owner["state"] == "ACTIVE" and owner["task_id"] == "repair"
    assert owner["scope"] == ["file0.py"]
    call_uuid = pre["hookSpecificOutput"]["updatedInput"]["call"]
    job_id = hashlib.sha256(call_uuid.encode()).hexdigest()
    codex.post_tool_use({**reply, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"jobId": job_id, "state": "RUNNING"}}},
                        config=config, state_root=state, repo_root=repo)
    assert _lease_state(workspace)._read()["owners"][codex._call_key(reply)]["state"] == "ACTIVE"
    (repo / "file0.py").write_text("value = 3\n", encoding="utf-8")

    bundle = _finish_attribution_call(foreign, workspace, config, [])
    artifact = json.loads(Path(bundle["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["known_concurrent_scopes"] == [{"path": "file0.py", "task_id": "repair"}]
    assert bundle["changed_paths"] == []
    assert not any(item["code"] == "OUT_OF_SCOPE_MODIFICATION" for item in bundle["findings"])
    assert _lease_state(workspace)._read()["owners"][codex._call_key(reply)]["state"] == "ACTIVE"
    bridge_job = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    bridge_job.parent.mkdir(parents=True, exist_ok=True)
    bridge_job.write_text(json.dumps({"jobId": job_id, "call": call_uuid,
                                      "state": "SUCCEEDED", "threadId": first["tool_use_id"]}),
                          encoding="utf-8")
    result = async_followup(RESULT_TOOL, job_id)
    codex.post_tool_use({**result, "hook_event_name": "PostToolUse", "tool_response":
                         {"structuredContent": {"jobId": job_id, "state": "SUCCEEDED",
                                                "threadId": first["tool_use_id"], "content": "done"}}},
                        config=config, state_root=state, repo_root=repo)
    assert _lease_state(workspace)._read()["owners"][codex._call_key(reply)]["state"] == "RELEASED"


def test_reply_lease_denies_conflicting_active_task(workspace, config):
    repo, state = workspace
    first = _attribution_call("repair", "file0.py", "reply-conflict-first")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    _finish_attribution_call(first, workspace, config, [])
    conflicting = _lease_call("other", "file0.py", "reply-conflict-other")
    codex.pre_tool_use(conflicting, config=config, state_root=state, repo_root=repo)

    reply = event(START_TOOL, prompt="Continue the repair.", tool_use_id="reply-conflict",
                  mode="reply", threadId=first["tool_use_id"])
    denied = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    assert "SCOPE_LEASE_CONFLICT" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert codex._call_key(reply) not in _lease_state(workspace)._read()["owners"]


def test_unleased_foreign_edit_still_forces_full(workspace, config):
    repo, state = workspace
    call = _attribution_call("owned", "file0.py", "unleased")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file1.py").write_text("value = 9\n", encoding="utf-8")
    bundle = _finish_attribution_call(call, workspace, config, [])
    assert bundle["claude_review_mode"] == policy.FULL
    assert "known_concurrent_scopes" not in bundle
    assert any(f["code"] == "OUT_OF_SCOPE_MODIFICATION" for f in bundle["findings"])


def test_ambiguous_foreign_path_still_forces_full(workspace, config):
    repo, state = workspace
    (repo / "file1.py").write_text("value = 3\n", encoding="utf-8")
    calls = [_attribution_call("owned", "file0.py", "ambiguous-a"),
             _attribution_call("foreign", "file1.py", "ambiguous-b")]
    for call in calls:
        codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file1.py").write_text("value = 4\n", encoding="utf-8")
    bundle = _finish_attribution_call(calls[0], workspace, config, [])
    assert bundle["claude_review_mode"] == policy.FULL
    assert "known_concurrent_scopes" not in bundle


def test_lease_released_before_job_start_does_not_excuse_path(workspace, config):
    repo, state = workspace
    store = _lease_state(workspace)
    assert store.acquire("former", "former-task", ["file1.py"])["status"] == ledger.OK
    assert store.release("former")["status"] == ledger.OK
    call = _attribution_call("owned", "file0.py", "after-release")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file1.py").write_text("value = 9\n", encoding="utf-8")
    bundle = _finish_attribution_call(call, workspace, config, [])
    assert "known_concurrent_scopes" not in bundle
    assert bundle["claude_review_mode"] == policy.FULL


def test_scope_lease_disjoint_and_conflicting_starts(workspace, config):
    repo, state = workspace
    first = _lease_call("owner-a", "file0.py", "lease-a")
    second = _lease_call("owner-b", "file1.py", "lease-b", tool_name=START_TOOL)
    conflict = _lease_call("owner-c", "file0.py", "lease-c")
    assert "updatedInput" in codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]
    assert "updatedInput" in codex.pre_tool_use(second, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]
    denied = codex.pre_tool_use(conflict, config=config, state_root=state, repo_root=repo)
    assert "SCOPE_LEASE_CONFLICT" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert "owner-a" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert "purser_leases.py --release <task-id>" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    violations = [item for item in _all_telemetry_events(state)
                  if item["event_type"] == "ROUTING_PROTOCOL_VIOLATION"]
    assert any(item["payload"]["violation_code"] == "SCOPE_LEASE_CONFLICT"
               for item in violations)
    owners = _lease_state(workspace)._read()["owners"]
    assert len([owner for owner in owners.values() if owner["state"] == "ACTIVE"]) == 2


def test_scope_lease_path_prefix_and_stale_owner_block(workspace, config):
    repo, state = workspace
    store = _lease_state(workspace)
    assert store.acquire("lost-owner", "old-task", ["src"])["status"] == ledger.OK
    # No pending call or host evidence exists after this simulated restart.
    contender = _lease_call("new-task", "src/module.py", "lease-stale")
    denied = codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)
    assert "SCOPE_LEASE_CONFLICT" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert "old-task" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    assert store.release("lost-owner")["status"] == ledger.OK
    assert "updatedInput" in codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]
    broad = _lease_call("broad-task", "src", "lease-broad")
    assert "SCOPE_LEASE_CONFLICT" in codex.pre_tool_use(
        broad, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]


def test_corrupt_lease_record_denies_admission(workspace, config):
    repo, state = workspace
    store = _lease_state(workspace)
    store.path.parent.mkdir(parents=True)
    store.path.write_text("{broken", encoding="utf-8")
    call = _lease_call("new-task", "file0.py", "lease-corrupt")
    denied = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "LEASE_ADMISSION_UNCERTAIN" in denied["hookSpecificOutput"]["permissionDecisionReason"]


def test_sync_lease_deadline_releases_only_after_timeout_and_margin(workspace, monkeypatch):
    store = _lease_state(workspace)
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    assert store.acquire("old", "old-task", ["file0.py"], timeout_seconds=10)["status"] == ledger.OK
    assert store._read()["owners"]["old"]["deadline_at"] == 1070.0
    monkeypatch.setattr(ledger.time, "time", lambda: 1069.0)
    assert store.acquire("new", "new-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    monkeypatch.setattr(ledger.time, "time", lambda: 1070.0)
    assert store.acquire("new", "new-task", ["file0.py"])["status"] == ledger.OK
    assert store._read()["owners"]["old"]["state"] == "RELEASED"
    assert store._read()["owners"]["new"]["deadline_at"] == 1070 + 7200


def test_async_lease_waits_for_finalize_or_expired_job_record(workspace, monkeypatch):
    _, state = workspace
    store = _lease_state(workspace)
    call = "async-call"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    assert store.acquire("async-owner", "async-task", ["file0.py"],
                         job_id=job_id, timeout_seconds=100)["status"] == ledger.OK
    path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    path.parent.mkdir(parents=True)
    job = {"jobId": job_id, "call": call, "state": "RUNNING"}
    path.write_text(json.dumps(job), encoding="utf-8")
    monkeypatch.setattr(ledger.time, "time", lambda: 1000 + 100 + ledger.JOB_RETENTION_SECONDS + 1)
    assert store.acquire("next", "next-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    job.update(state="SUCCEEDED")
    path.write_text(json.dumps(job), encoding="utf-8")
    assert store.acquire("next", "next-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    job.update(state="EXPIRED", errorCode="JOB_EXPIRED")
    path.write_text(json.dumps(job), encoding="utf-8")
    assert store.acquire("next", "next-task", ["file0.py"])["status"] == ledger.OK


def test_async_missing_job_record_requires_retention_cutoff(workspace, monkeypatch):
    store = _lease_state(workspace)
    job_id = hashlib.sha256(b"missing-call").hexdigest()
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    store.acquire("missing-owner", "missing-task", ["file0.py"], job_id=job_id,
                  timeout_seconds=100)
    cutoff = store._read()["owners"]["missing-owner"]["retention_after"]
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff - 1)
    assert store.acquire("next", "next-task", ["file0.py"])["code"] == "SCOPE_LEASE_CONFLICT"
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff)
    assert store.acquire("next", "next-task", ["file0.py"])["status"] == ledger.OK


def test_owner_state_promotes_waiter_after_blocker_retention_expires(workspace, monkeypatch):
    store = _lease_state(workspace)
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    assert store.acquire("blocker", "blocking-task", ["file0.py"],
                         job_id="a" * 64, timeout_seconds=100)["status"] == ledger.OK
    cutoff = store._read()["owners"]["blocker"]["retention_after"]
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff - 10)
    assert store.acquire("waiter", "waiting-task", ["file0.py"],
                         job_id="b" * 64, timeout_seconds=100,
                         queue_conflict=True)["code"] == "LEASE_QUEUED"
    monkeypatch.setattr(ledger.time, "time", lambda: cutoff)
    assert store.owner_state("waiter") == "ACTIVE"
    owners = store._read()["owners"]
    assert owners["blocker"]["state"] == "RELEASED"
    assert owners["waiter"]["acquired_at"] == cutoff


def test_released_lease_entries_are_pruned_on_write(workspace, monkeypatch):
    store = _lease_state(workspace)
    monkeypatch.setattr(ledger.time, "time", lambda: 1_000_000.0)
    value = {"version": 1, "owners": {
        "old": {"task_id": "old", "scope": ["file0.py"], "state": "RELEASED",
                "job_id": None, "acquired_at": 1.0,
                "released_at": 1_000_000 - ledger.RELEASED_LEASE_RETENTION_SECONDS - 1},
        "recent": {"task_id": "recent", "scope": ["file1.py"], "state": "RELEASED",
                   "job_id": None, "acquired_at": 1.0,
                   "released_at": 1_000_000 - ledger.RELEASED_LEASE_RETENTION_SECONDS + 1},
    }}
    store._write(value)
    assert set(store._read()["owners"]) == {"recent"}


def test_lease_cli_lists_and_releases_only_named_task(workspace, monkeypatch):
    _, state = workspace
    store = _lease_state(workspace)
    store.acquire("one", "task-one", ["file0.py"])
    call = "cli-async"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    store.acquire("two", "task-two", ["file1.py"], job_id=job_id)
    path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({"jobId": job_id, "call": call, "state": "RUNNING"}), encoding="utf-8")
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    cli = Path(codex.__file__).with_name("purser_leases.py")
    listed = subprocess.run([sys.executable, str(cli), "--list"], capture_output=True,
                            text=True, check=True)
    active = json.loads(listed.stdout)["active"]
    assert {row["task_id"] for row in active} == {"task-one", "task-two"}
    assert next(row for row in active if row["task_id"] == "task-two")["job_state"] == "RUNNING"
    assert all(row["age_seconds"] >= 0 and row["scope"] for row in active)
    released = subprocess.run([sys.executable, str(cli), "--release", "task-one"],
                              capture_output=True, text=True, check=True)
    assert [row["task_id"] for row in json.loads(released.stdout)["released"]] == ["task-one"]
    assert [row["task_id"] for row in store.active()] == ["task-two"]


def test_simultaneous_overlapping_worktree_starts_run_and_disjoint_runs(workspace, config):
    repo, state = workspace
    calls = [_lease_call(f"race-{n}", "file0.py", f"lease-race-{n}", tool_name=START_TOOL)
             for n in (1, 2)]
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(lambda call: codex.pre_tool_use(
            call, config=config, state_root=state, repo_root=repo), calls))
    assert all("updatedInput" in result["hookSpecificOutput"] for result in results)
    assert all(result["hookSpecificOutput"]["updatedInput"].get("worktree") is True
               and "leaseQueued" not in result["hookSpecificOutput"]["updatedInput"]
               for result in results)
    disjoint = _lease_call("disjoint", "file1.py", "lease-disjoint", tool_name=START_TOOL)
    assert "leaseQueued" not in codex.pre_tool_use(
        disjoint, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    owners = _lease_state(workspace)._read()["owners"]
    assert sorted(owner["state"] for owner in owners.values()) == ["ACTIVE", "ACTIVE", "ACTIVE"]


def test_overlapping_worktree_admission_keeps_baseline_without_waiting(workspace, config, monkeypatch):
    repo, state = workspace
    first = _lease_call("first", "file0.py", "queue-first", tool_name=START_TOOL)
    second = _lease_call("second", "file0.py", "queue-second", tool_name=START_TOOL)
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    updated = codex.pre_tool_use(second, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["worktree"] is True
    assert "leaseQueued" not in updated
    assert codex._read_pending(state, second)["baseline"]["status"] == ledger.OK
    assert len(_lease_state(workspace).overlapping_owners(codex._call_key(first))) == 1
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    jobs = state / "codex_exec_mcp" / "jobs"
    server = codex_exec_mcp.Server(job_dir=jobs)
    monkeypatch.setattr(codex_exec_mcp, "_launch_host", lambda *_args: None)
    monkeypatch.setattr(server, "_await_host", lambda *_args: {"structuredContent": {"jobId": job_id, "state": "QUEUED"}})
    server._start_job(updated)
    job = json.loads((jobs / f"{job_id}.json").read_text(encoding="utf-8"))
    assert "leaseQueued" not in job
    pending = codex._read_pending(state, second)
    assert isinstance(pending["baseline"]["status_entries"], list)


def test_sync_finalize_releases_scope_lease(workspace, config):
    repo, state = workspace
    first = _lease_call("owner-a", "file0.py", "lease-finalize")
    contender = _lease_call("owner-b", "file0.py", "lease-after")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    assert "SCOPE_LEASE_CONFLICT" in codex.pre_tool_use(
        contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]
    post = {**first, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"threadId": "lease-thread", "content": "done"}}}
    codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert _lease_state(workspace)._read()["owners"][codex._call_key(first)]["state"] == "RELEASED"
    assert "updatedInput" in codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]


def test_failure_finalize_releases_scope_lease(workspace, config):
    repo, state = workspace
    first = _lease_call("failed-owner", "file0.py", "lease-failed")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    failure = {**first, "hook_event_name": "PostToolUseFailure", "error": "worker failed"}
    codex.post_tool_use_failure(failure, config=config, state_root=state, repo_root=repo)
    assert _lease_state(workspace)._read()["owners"][codex._call_key(first)]["state"] == "RELEASED"
    contender = _lease_call("after-failure", "file0.py", "lease-after-failure")
    assert "updatedInput" in codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]


def test_rejected_start_waits_for_durable_terminal_state(workspace, config):
    repo, state = workspace
    first = _lease_call("rejected-owner", "file0.py", "lease-rejected", tool_name=START_TOOL)
    updated = codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    path.parent.mkdir(parents=True)
    job = {"jobId": job_id, "call": updated["call"], "state": "RUNNING"}
    path.write_text(json.dumps(job), encoding="utf-8")
    codex.post_tool_use_failure({**first, "hook_event_name": "PostToolUseFailure",
                                 "error": "start rejected"}, config=config, state_root=state, repo_root=repo)
    contender = _lease_call("after-rejection", "file0.py", "lease-after-rejection")
    denied = codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)
    assert "SCOPE_LEASE_CONFLICT" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    job["state"] = "CANCELLED"
    path.write_text(json.dumps(job), encoding="utf-8")
    assert "updatedInput" in codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]
    assert _lease_state(workspace)._read()["owners"][codex._call_key(first)]["state"] == "RELEASED"


def test_async_lease_survives_start_and_releases_after_finalize(workspace, config):
    repo, state = workspace
    prompt = orch(task="async-owner", scope="file0.py") + "\nDo the work."
    start, job_id, _ = async_started(workspace, config, prompt=prompt, state_name="SUCCEEDED")
    contender = _lease_call("other", "file0.py", "lease-async-conflict")
    assert "SCOPE_LEASE_CONFLICT" in codex.pre_tool_use(
        contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]
    followup = async_followup(RESULT_TOOL, job_id)
    post = {**followup, "hook_event_name": "PostToolUse", "tool_response":
            {"structuredContent": {"jobId": job_id, "state": "SUCCEEDED",
                                   "threadId": "async-thread", "content": "done"}}}
    codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert _lease_state(workspace)._read()["owners"][codex._call_key(start)]["state"] == "RELEASED"
    assert "updatedInput" in codex.pre_tool_use(contender, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]


@pytest.mark.parametrize("phase,route", (("plan", "plan_standard"), ("operation", "normal")))
def test_nonimplementation_calls_take_no_write_lease(workspace, config, phase, route):
    repo, state = workspace
    first = _lease_call("owner", "file0.py", "lease-active")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    call = event(prompt=orch(task="read-only", phase=phase, route=route,
                             scope="file0.py") + "\nInspect.", tool_use_id=f"lease-{phase}")
    assert "updatedInput" in codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]
    owners = _lease_state(workspace)._read()["owners"]
    assert len(owners) == (2 if phase == "plan" else 1)
    if phase == "plan":
        assert owners[codex._call_key(call)]["scope"] == []
        assert owners[codex._call_key(call)]["read_only"] is True


def test_async_start_requires_orch_and_denies_internal_pre_error(workspace, config):
    repo, state = workspace
    call = event(START_TOOL, prompt="Do the work.")
    denied = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "MISSING_ORCH_LINE" in denied["hookSpecificOutput"]["permissionDecisionReason"]
    failed = codex.internal_pre_error_response(call, RuntimeError("broken"), state_root=state)
    assert failed["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert not list((state / "pending").glob("*.json"))


def test_async_start_rewrites_and_retries_same_call(workspace, config):
    repo, state = workspace
    prompt = orch(scope="file0.py", reason="repeated_failure") + "|eta=8m\nDo the work."
    call = event(START_TOOL, prompt=prompt, call="claude-value", model="wrong",
                 timeoutSeconds=90, config={"model_reasoning_effort": "low"})
    first = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    updated = first["hookSpecificOutput"]["updatedInput"]
    assert updated["call"] != "claude-value"
    assert uuid.UUID(updated["call"])
    assert updated["mode"] == "new"
    assert updated["model"] == config["routes"]["normal"]["model"]
    assert updated["config"]["model_reasoning_effort"] == config["routes"]["normal"]["effort"]
    assert updated["config"]["tool_output_token_limit"] == 8000
    pending = codex._read_pending(state, call)
    assert pending["async_call"] == updated["call"] and pending["job_id"] is None
    assert pending["baseline"]["status"] == ledger.OK
    lease = _lease_state(workspace)._read()["owners"][codex._call_key(call)]
    assert lease["retention_after"] == lease["acquired_at"] + 90 + ledger.JOB_RETENTION_SECONDS
    assert (pending["reason"], pending["eta_minutes"]) == ("repeated_failure", 8)
    assert codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"] == updated
    assert len([e for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_STARTED"]) == 1


@pytest.mark.parametrize("response_shape", ("string", "blocks", "mapping"))
def test_async_start_post_stores_only_job_id_without_finalizing(workspace, config, response_shape):
    repo, state = workspace
    call = event(START_TOOL)
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    value = {"jobId": job_id, "state": "RUNNING"}
    response = {
        "string": json.dumps(value),
        "blocks": [{"type": "text", "text": json.dumps(value)}],
        "mapping": {"structuredContent": value},
    }[response_shape]
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": response}
    result = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo,
                                 reviewer=lambda **kwargs: pytest.fail("reviewer ran at start"))
    next_text = response_bundle(result, config)["next"]
    assert next_text == (f'Run in the background: "{sys.executable}" '
                         f'"{Path(codex.__file__).resolve().parent / "purser_wait.py"}" '
                         f'{job_id} ; when it finishes, call codex_result with this jobId.')
    assert response_bundle(result, config)["jobId"] == job_id
    assert response_bundle(result, config)["state"] == "RUNNING"
    assert codex._read_pending(state, call)["job_id"] == job_id
    assert json.loads((state / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))["call_key"] == codex._call_key(call)
    record = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")["record"]
    assert record["last_outcome"] is None and record["pending_call_baseline"] is not None
    assert not [e for e in _all_telemetry_events(state) if e["event_type"] == "IMPLEMENTER_FINISHED"]


@pytest.mark.parametrize("fail_store", (False, True))
@pytest.mark.parametrize("error_shape", ("string", "blocks", "mapping"))
def test_async_start_failure_uses_ambiguous_path_and_cancels_job(workspace, config, monkeypatch, fail_store, error_shape):
    repo, state = workspace
    call = event(START_TOOL)
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    job_id = hashlib.sha256(updated["call"].encode()).hexdigest()
    cancelled = []
    bridge_job = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    bridge_job.parent.mkdir(parents=True)
    bridge_job.write_text(json.dumps({"jobId": job_id, "call": updated["call"]}), encoding="utf-8")
    def confirm_cancel(root, call_uuid, job):
        cancelled.append(job)
        bridge_job.write_text(json.dumps({"jobId": job_id, "call": updated["call"],
                                          "state": "CANCELLED"}), encoding="utf-8")
        return True
    monkeypatch.setattr(codex, "_cancel_start_job", confirm_cancel)
    if fail_store:
        write = codex._atomic_json_write
        def fail_job_write(path, value):
            if path == codex._pending_path(state, call) and value.get("job_id"):
                raise OSError("job association could not be stored")
            return write(path, value)
        monkeypatch.setattr(codex, "_atomic_json_write", fail_job_write)
    value = {"jobId": job_id, "state": "RUNNING",
             **({} if fail_store else {"errorCode": "CODEX_FAILED", "isError": True})}
    response = {
        "string": json.dumps(value),
        "blocks": [{"type": "text", "text": json.dumps(value)}],
        "mapping": {"structuredContent": value, "isError": not fail_store},
    }[error_shape]
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": response}
    result = codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    assert "FULL" in result["hookSpecificOutput"]["additionalContext"]
    assert cancelled == [job_id]
    assert not list((state / "pending").glob("*.json"))
    assert "ASYNC_CALL_ALREADY_ACCEPTED" in codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["permissionDecisionReason"]
    assert any(e["event_type"] == "IMPLEMENTER_FAILED" for e in _all_telemetry_events(state))


def test_async_reply_start_keeps_recorded_thread(workspace, config):
    repo, state = workspace
    save_live_thread(state, reason="continue")
    call = event(START_TOOL, prompt="Continue.", mode="reply", threadId="thread-1")
    updated = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)["hookSpecificOutput"]["updatedInput"]
    assert updated["mode"] == "reply" and updated["threadId"] == "thread-1"
    assert "model" not in updated and "config" not in updated
    pending = codex._read_pending(state, call)
    assert pending["prior_thread_id"] == "thread-1" and pending["reason"] == "continue"


def test_plugin_tool_names_route_new_and_reply(workspace, config):
    repo, state = workspace
    prompt = orch(scope="file0.py") + "\n## Objective\nInspect file0.\n## Acceptance criteria\n- Report result."
    first = event(tool_name="mcp__plugin_purser_codex-main__codex", prompt=prompt,
                  tool_use_id="plugin-first")
    routed = codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    assert routed["hookSpecificOutput"]["updatedInput"]["model"] == config["routes"]["normal"]["model"]
    assert codex._read_pending(state, first)["prior_thread_id"] is None
    response = [{"type": "text", "text": json.dumps({"threadId": "plugin-thread", "content": "Done"})}]
    result = codex.post_tool_use({**first, "hook_event_name": "PostToolUse", "tool_response": response},
                                 config=config, state_root=state, repo_root=repo)
    assert response_bundle(result, config)["thread_id"] == "plugin-thread"

    reply = event(tool_name="mcp__plugin_purser_codex-main__codex-reply", prompt="Continue.",
                  tool_use_id="plugin-reply", threadId="plugin-thread")
    reply_routed = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    assert reply_routed["hookSpecificOutput"]["updatedInput"]["threadId"] == "plugin-thread"
    assert codex._read_pending(state, reply)["prior_thread_id"] == "plugin-thread"
    reply_result = codex.post_tool_use({**reply, "hook_event_name": "PostToolUse", "tool_response": response},
                                       config=config, state_root=state, repo_root=repo)
    assert response_bundle(reply_result, config)["thread_id"] == "plugin-thread"


@pytest.mark.parametrize("quota", [
    {"5h_used_pct": 24, "5h_resets_at": "2026-09-23T16:49:02Z"}, None,
])
def test_quota_reply_bundle_and_review_mode(workspace, config, monkeypatch, quota):
    repo, state = workspace
    receipts = []
    monkeypatch.setattr(codex.compaction_advisor, "queue_receipt", lambda event, repository, bundle: receipts.append((repository, bundle)))
    monkeypatch.setattr(codex.codex_quota, "read_quota", lambda thread_id: quota)
    prompt = orch(scope="file0.py") + "\n## Objective\nInspect file0.\n## Acceptance criteria\n- Report result."
    first = event(prompt=prompt, tool_use_id="quota-first")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    tool_response = [{"type": "text", "text": json.dumps({"threadId": "quota-thread", "content": "Done"})}]
    first_result = codex.post_tool_use(
        {**first, "hook_event_name": "PostToolUse", "tool_response": tool_response},
        config=config, state_root=state, repo_root=repo)
    assert response_bundle(first_result, config)["codex_quota"] == quota
    reply = event(tool_name=codex.CODEX_REPLY_TOOL, prompt="Continue.",
                  tool_use_id="quota-reply", threadId="quota-thread")
    codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    result = codex.post_tool_use({**reply, "hook_event_name": "PostToolUse", "tool_response": tool_response},
                                 config=config, state_root=state, repo_root=repo)
    bundle = response_bundle(result, config)
    assert bundle["codex_quota"] == quota
    assert bundle["claude_review_mode"] == policy.FULL
    assert receipts[-1][0] == repo.resolve()
    assert receipts[-1][1]["model"] == bundle["model"]
    assert receipts[-1][1]["codex_quota"] == quota


def test_quota_does_not_change_review_mode_under_cap(workspace, config):
    _, state = workspace
    event_data = event(tool_use_id="quota-cap")
    bundle = codex._base_bundle()
    bundle["thread_id"] = "quota-thread"
    bundle["claude_review_mode"] = policy.AUDIT
    without = response_bundle(codex._finalize_replacement(bundle.copy(), config, state, event_data), config)
    with_quota = bundle.copy()
    with_quota["codex_quota"] = {"5h_used_pct": 24, "5h_resets_at": "2026-09-23T16:49:02Z"}
    included = response_bundle(codex._finalize_replacement(with_quota, config, state, event_data), config)
    assert included["codex_quota"] == with_quota["codex_quota"]
    assert included["claude_review_mode"] == without["claude_review_mode"] == policy.AUDIT


def test_post_tool_quota_system_message_formats_local_windows(workspace, config, monkeypatch):
    local_tz = timezone(timedelta(hours=-7))
    message = codex._codex_quota_system_message(
        {
            "5h_used_pct": 1.0,
            "5h_resets_at": "2026-09-25T08:57:11Z",
            "7d_used_pct": 46.0,
            "7d_resets_at": "2026-09-28T16:07:43Z",
        },
        local_tz=local_tz,
    )
    assert message == "⚓ **Purser:** Codex quota left: 5h 99% (resets 01:57) · 7d 54% (resets Sep 28)"

    _, state = workspace
    format_quota = codex._codex_quota_system_message
    monkeypatch.setattr(
        codex,
        "_codex_quota_system_message",
        lambda quota: format_quota(quota, local_tz=local_tz),
    )
    bundle = codex._base_bundle()
    bundle["codex_quota"] = {
        "5h_used_pct": 1.0,
        "5h_resets_at": "2026-09-25T08:57:11Z",
        "7d_used_pct": 46.0,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }
    call = event(tool_use_id="quota-system-message")
    result = codex._finalize_replacement(bundle, config, state, call)
    assert result["systemMessage"] == message
    assert set(result) == {"hookSpecificOutput", "systemMessage"}
    compact, _ = codex._compact_bundle(bundle, config["bundle_non_report_max_bytes"])
    compact["codex_quota"] = bundle["codex_quota"]
    compact["implementer_report"] = ""[: codex._report_limit(config, bundle.get("phase"))]
    assert result["hookSpecificOutput"] == codex._replacement(compact, config)["hookSpecificOutput"]


def test_post_tool_quota_system_message_omits_missing_or_malformed_windows():
    local_tz = timezone.utc
    quota = {
        "7d_used_pct": 46.0,
        "7d_resets_at": "2026-09-28T16:07:43Z",
    }
    assert codex._codex_quota_system_message(quota, local_tz=local_tz) == (
        "⚓ **Purser:** Codex quota left: 7d 54% (resets Sep 28)"
    )
    assert codex._codex_quota_system_message(None, local_tz=local_tz) is None
    assert codex._codex_quota_system_message({}, local_tz=local_tz) is None
    assert codex._codex_quota_system_message(
        {
            "5h_used_pct": 1,
            "5h_resets_at": "not-a-timestamp",
            "7d_used_pct": 46,
            "7d_resets_at": "2026-09-28T16:07:43Z",
        },
        local_tz=local_tz,
    ) == "⚓ **Purser:** Codex quota left: 7d 54% (resets Sep 28)"


@pytest.mark.parametrize(("used", "expected"), [(100.4, 0), (-3, 100)])
def test_post_tool_quota_system_message_clamps_remaining_percent(used, expected):
    message = codex._codex_quota_system_message(
        {"5h_used_pct": used, "5h_resets_at": "2026-09-25T08:57:11Z"},
        local_tz=timezone.utc,
    )
    assert message == f"⚓ **Purser:** Codex quota left: 5h {expected}% (resets 08:57)"


def test_post_tool_quota_message_failure_preserves_replacement(workspace, config, monkeypatch):
    _, state = workspace
    bundle = codex._base_bundle()
    bundle["codex_quota"] = {"5h_used_pct": 1, "5h_resets_at": "2026-09-25T08:57:11Z"}
    call = event(tool_use_id="quota-message-failure")
    monkeypatch.setattr(codex, "_codex_quota_system_message", lambda _quota: (_ for _ in ()).throw(ValueError()))
    result = codex._finalize_replacement(bundle, config, state, call)
    assert "systemMessage" not in result
    compact, _ = codex._compact_bundle(bundle, config["bundle_non_report_max_bytes"])
    compact["codex_quota"] = bundle["codex_quota"]
    compact["implementer_report"] = ""[: codex._report_limit(config, bundle.get("phase"))]
    assert result["hookSpecificOutput"] == codex._replacement(compact, config)["hookSpecificOutput"]


def test_post_tool_quota_message_joins_existing_system_message(workspace, config, monkeypatch):
    _, state = workspace
    bundle = codex._base_bundle()
    bundle["codex_quota"] = {"7d_used_pct": 46, "7d_resets_at": "2026-09-28T16:07:43Z"}
    original_replacement = codex._replacement
    monkeypatch.setattr(
        codex,
        "_replacement",
        lambda value, loaded_config: {
            **original_replacement(value, loaded_config),
            "systemMessage": "Existing message",
        },
    )
    result = codex._finalize_replacement(bundle, config, state, event(tool_use_id="quota-join"))
    assert result["systemMessage"] == (
        "Existing message\n⚓ **Purser:** Codex quota left: 7d 54% (resets Sep 28)"
    )


def test_post_tool_quota_message_survives_utf8_json_round_trip(monkeypatch):
    response = {
        "hookSpecificOutput": {"hookEventName": "PostToolUse", "updatedToolOutput": []},
        "systemMessage": "⚓ **Purser:** Codex quota left: 7d 54% (resets Sep 28)",
    }
    stdin_raw = io.BytesIO(b"{}")
    stdout_raw = io.BytesIO()
    monkeypatch.setattr(hook.sys, "stdin", type("Input", (), {"buffer": stdin_raw})())
    monkeypatch.setattr(hook.sys, "stdout", type("Output", (), {"buffer": stdout_raw})())
    monkeypatch.setattr(hook, "dispatch", lambda _event: response)

    assert hook.main() == 0
    rendered = stdout_raw.getvalue()
    assert "⚓".encode("utf-8") in rendered
    assert json.loads(rendered.decode("utf-8")) == response


def _quota_snapshot(used_5h=None, used_7d=None):
    value = {}
    if used_5h is not None:
        value.update({"5h_used_pct": used_5h, "5h_resets_at": "2026-09-24T15:00:00Z"})
    if used_7d is not None:
        value.update({"7d_used_pct": used_7d, "7d_resets_at": "2026-09-30T12:00:00Z"})
    return value or None


def _permission_reason(result):
    return result.get("hookSpecificOutput", {}).get("permissionDecisionReason", "")


def test_low_codex_quota_denies_before_pending_record(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: _quota_snapshot(92), raising=False)
    call = event(prompt=orch(scope="file0.py") + "\nDo the work.", tool_use_id="low-quota")
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    reason = _permission_reason(result)
    assert "LOW_CODEX_QUOTA" in reason
    assert "5h" in reason and "92%" in reason and "2026-09-24T15:00:00Z" in reason
    assert "before this task starts" in reason
    assert "reason=user_override" in reason
    assert "background sleep command" in reason
    assert "until the 5h reset at 2026-09-24T15:00:00Z UTC" in reason
    assert "waiting costs nothing" in reason
    assert not (state / "pending").exists() or list((state / "pending").glob("*.json")) == []


def test_low_codex_quota_user_override_proceeds(workspace, config, monkeypatch):
    repo, state = workspace
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: _quota_snapshot(92), raising=False)
    call = event(prompt=orch(scope="file0.py", reason="user_override") + "\nDo the work.",
                 tool_use_id="low-quota-override")
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "LOW_CODEX_QUOTA" not in _permission_reason(result)
    assert codex._read_pending(state, call) is not None


def test_low_codex_quota_does_not_check_codex_reply(workspace, config, monkeypatch):
    _, state = workspace
    calls = []
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: calls.append(True) or _quota_snapshot(92), raising=False)
    call = event(tool_name=codex.CODEX_REPLY_TOOL, prompt=orch(scope="file0.py") + "\nContinue.",
                 tool_use_id="low-quota-reply", threadId="existing-thread")
    codex.pre_tool_use(call, config=config, state_root=state)
    assert calls == []


@pytest.mark.parametrize("quota", [_quota_snapshot(used_7d=95)])
def test_low_weekly_quota_denies(workspace, config, monkeypatch, quota):
    repo, state = workspace
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: quota, raising=False)
    call = event(prompt=orch(scope="file0.py") + "\nDo the work.", tool_use_id="low-weekly-quota")
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "LOW_CODEX_QUOTA" in _permission_reason(result)
    assert "7d" in _permission_reason(result) and "95%" in _permission_reason(result)


@pytest.mark.parametrize("quota", [_quota_snapshot(used_5h=89), None])
def test_low_codex_quota_allows_under_threshold_or_missing(workspace, config, monkeypatch, quota):
    repo, state = workspace
    monkeypatch.setattr(codex.codex_quota, "read_latest_quota", lambda: quota, raising=False)
    call = event(prompt=orch(scope="file0.py") + "\nDo the work.", tool_use_id="normal-quota")
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "LOW_CODEX_QUOTA" not in _permission_reason(result)
    assert codex._read_pending(state, call) is not None


def hook_check_call(workspace, config, monkeypatch, *, worker_status="UNKNOWN", check_result=None,
                    declared=True, reviewer=None, before_post=None):
    repo, state = workspace
    original_load = project_profile.load_profile
    monkeypatch.setattr(project_profile, "load_profile", lambda root: replace(
        original_load(root), force_full_review=False,
        checks=(project_profile.Check("focused", ("python", "-V")),) if declared else (),
    ))
    monkeypatch.setattr(checks, "run_checks", check_result if callable(check_result)
                        else lambda *args, **kwargs: check_result)
    prompt = (orch(scope="file0.py") +
              "\n## Objective\nUpdate file0.\n## Acceptance criteria\n- file0 changes.")
    call = event(prompt=prompt, tool_use_id="hook-check-" + uuid.uuid4().hex)
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    if before_post:
        before_post(repo, state, call)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    content = ("No worker validation" if worker_status == "UNKNOWN" else
               'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}')
    post = {**call, "hook_event_name": "PostToolUse", "tool_response": [{"type": "text", "text": json.dumps({"threadId": "thread-check", "content": content})}]}
    return response_bundle(codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo,
        reviewer=reviewer or (lambda **kwargs: {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                "validation": "PASS", "findings": []}),
    ), config)


def test_hook_pass_supplies_audit_evidence_for_unknown_worker(workspace, config, monkeypatch):
    result = {"status": "PASS", "results": [{"name": "focused", "status": "PASS", "exit_code": 0,
                                               "duration_ms": 1, "tail": ""}], "side_effect_paths": []}
    bundle = hook_check_call(workspace, config, monkeypatch, check_result=result)
    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["hook_checks"]["status"] == "PASS"
    assert not any(f["code"] == "WORKER_VALIDATION_UNVERIFIED" for f in bundle["findings"])


def check_result(status, *, tail="", side_effect_paths=None):
    return {"status": status, "results": [{"name": "focused", "status": status,
            "exit_code": 1 if status == "FAIL" else 0 if status == "PASS" else None,
            "duration_ms": 1, "tail": tail}],
            "side_effect_paths": [] if side_effect_paths is None else side_effect_paths}


def test_hook_fail_overrides_worker_pass(workspace, config, monkeypatch):
    bundle = hook_check_call(workspace, config, monkeypatch, worker_status="PASS",
                             check_result=check_result("FAIL", tail="failed output"))
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "HOOK_CHECK_FAILED" and "failed output" in f["message"]
               for f in bundle["findings"])
    assert "tail" not in bundle["hook_checks"]["results"][0]


@pytest.mark.parametrize("worker_status,expected", [("PASS", policy.AUDIT),
                                                   ("UNKNOWN", policy.FULL)])
def test_hook_unknown_falls_back_to_worker(workspace, config, monkeypatch, worker_status, expected):
    bundle = hook_check_call(workspace, config, monkeypatch, worker_status=worker_status,
                             check_result=check_result("UNKNOWN", tail="unavailable"))
    assert bundle["claude_review_mode"] == expected
    assert any(f["code"] == "HOOK_CHECK_UNKNOWN" for f in bundle["findings"])


def test_hook_side_effects_force_full(workspace, config, monkeypatch):
    bundle = hook_check_call(workspace, config, monkeypatch, worker_status="PASS",
                             check_result=check_result("PASS", side_effect_paths=["file1.py"]))
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "HOOK_CHECK_SIDE_EFFECTS" and "file1.py" in f["message"]
               for f in bundle["findings"])


def test_hook_check_exception_still_returns_bundle(workspace, config, monkeypatch):
    def raise_check(*args, **kwargs):
        raise RuntimeError("runner broke")
    bundle = hook_check_call(workspace, config, monkeypatch, worker_status="PASS",
                             check_result=raise_check)
    assert bundle["claude_review_mode"] == policy.AUDIT
    assert bundle["hook_checks"] == {"status": "UNKNOWN", "results": []}
    assert [f["code"] for f in bundle["findings"]].count("HOOK_CHECK_UNKNOWN") == 1
    assert any("RuntimeError" in f["message"] for f in bundle["findings"])


def test_hook_uses_pending_check_snapshot(workspace, config, monkeypatch):
    def change_profile(repo, state, call):
        pending = json.loads(codex._pending_path(state, call).read_text(encoding="utf-8"))
        assert pending["profile_checks"][0]["name"] == "focused"
        profile_path = repo / ".orchestrator" / "profile.toml"
        profile_path.parent.mkdir()
        profile_path.write_text('schema_version = 1\n[[checks]]\nname = "changed"\nargv = ["python"]\n', encoding="utf-8")
        monkeypatch.setattr(project_profile, "load_profile", lambda root: pytest.fail("post reloaded profile"))
    bundle = hook_check_call(workspace, config, monkeypatch,
                             check_result=check_result("PASS"), before_post=change_profile)
    assert bundle["hook_checks"]["status"] == "PASS"


def test_reply_inherits_thread_check_snapshot(workspace, config, monkeypatch):
    repo, state = workspace
    original_load = project_profile.load_profile
    monkeypatch.setattr(project_profile, "load_profile", lambda root: replace(
        original_load(root), force_full_review=False,
        checks=(project_profile.Check("original", ("python", "-V")),),
    ))
    monkeypatch.setattr(checks, "run_checks", lambda *args, **kwargs: {
        "status": "PASS", "results": [], "side_effect_paths": []})
    prompt = (orch(scope="file0.py") +
              "\n## Objective\nUpdate file0.\n## Acceptance criteria\n- file0 changes.")
    call = event(prompt=prompt, tool_use_id="first-check-turn")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    response = [{"type": "text", "text": json.dumps({"threadId": "thread-reply-checks", "content": "No validation"})}]
    codex.post_tool_use({**call, "hook_event_name": "PostToolUse", "tool_response": response},
                        config=config, state_root=state, repo_root=repo,
                        reviewer=lambda **kwargs: {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                                   "validation": "PASS", "findings": []})
    monkeypatch.setattr(project_profile, "load_profile", lambda root: replace(
        original_load(root), force_full_review=False,
        checks=(project_profile.Check("changed", ("python", "-V")),),
    ))
    reply = event(tool_name=codex.CODEX_REPLY_TOOL, prompt="Continue.",
                  tool_use_id="reply-check-turn", threadId="thread-reply-checks")
    codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    pending = json.loads(codex._pending_path(state, reply).read_text(encoding="utf-8"))
    assert [item["name"] for item in pending["profile_checks"]] == ["original"]


def test_no_declared_hook_checks_preserves_worker_decision(workspace, config, monkeypatch):
    bundle = hook_check_call(workspace, config, monkeypatch, declared=False, worker_status="UNKNOWN",
                             check_result=lambda *args, **kwargs: pytest.fail("checks must not run"))
    assert bundle["hook_checks"] is None
    assert bundle["claude_review_mode"] == policy.FULL
    assert any(f["code"] == "WORKER_VALIDATION_UNVERIFIED" for f in bundle["findings"])


def test_hook_checks_overlap_reviewer(workspace, config, monkeypatch):
    intervals = {}
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    def slow_check(*args, **kwargs):
        intervals["check_start"] = time.monotonic()
        time.sleep(0.5)
        intervals["check_end"] = time.monotonic()
        return check_result("PASS")
    def slow_reviewer(**kwargs):
        intervals["reviewer_start"] = time.monotonic()
        time.sleep(0.5)
        intervals["reviewer_end"] = time.monotonic()
        return {"result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS", "findings": []}
    bundle = hook_check_call(workspace, review_config, monkeypatch, check_result=slow_check,
                             reviewer=slow_reviewer)
    assert bundle["hook_checks"]["status"] == "PASS"
    assert intervals["check_start"] < intervals["reviewer_end"]
    assert intervals["reviewer_start"] < intervals["check_end"]


@pytest.fixture()
def isolated_profile_home(monkeypatch):
    isolated_home = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "codex-profile-home"
        / uuid.uuid4().hex
    )
    monkeypatch.setenv("USERPROFILE", str(isolated_home))
    monkeypatch.setenv("HOME", str(isolated_home))
    monkeypatch.setattr(Path, "home", lambda: isolated_home)
    try:
        yield isolated_home
    finally:
        if isolated_home.exists():
            remove_test_tree(isolated_home)


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
    model="gpt-6-sol",
    effort="medium",
    phase="implementation",
    scope=("file0.py", "file1.py", "file2.py"),
    reason=None,
):
    result = ledger.SessionLedger(
        state / "projects" / project_root.project_key((state.parent / "repo").resolve())
    ).save(
        ledger.build_session_record(
            "task-1",
            "thread-1",
            route,
            model,
            effort,
            phase=phase,
            scope=scope,
            reason=reason,
        )
    )
    assert result["status"] == ledger.OK


def _call_events(state):
    events = []
    for path in (state / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return [event for event in events if event["event_type"] == "IMPLEMENTER_STARTED"]


def _all_telemetry_events(state):
    events = []
    for path in (state / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return events


def test_eta_is_recorded_from_orch_through_finished_duration(workspace, config):
    repo, state = workspace
    prompt = orch(scope="file0.py") + "|eta=23m\nImplement the scoped change."
    call = event(prompt=prompt)

    routed = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    forwarded_prompt = routed["hookSpecificOutput"]["updatedInput"]["prompt"]
    assert forwarded_prompt.startswith("Implement the scoped change.")
    assert "ORCH|" not in forwarded_prompt
    assert codex._read_pending(state, call)["eta_minutes"] == 23
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "duration_ms": 1250,
        "tool_response": [{"type": "text", "text": json.dumps({"threadId": "thread-1", "content": "done"})}],
    }
    codex.post_tool_use(post, config=config, state_root=state, repo_root=repo)
    events = _all_telemetry_events(state)
    for event_type in ("TASK_ROUTE_SELECTED", "IMPLEMENTER_STARTED", "IMPLEMENTER_FINISHED"):
        payload = next(item["payload"] for item in events if item["event_type"] == event_type)
        assert payload["eta_minutes"] == 23
    finished = next(item["payload"] for item in events if item["event_type"] == "IMPLEMENTER_FINISHED")
    assert finished["duration_ms"] == 1250
    assert finished["route"] == "normal"


@pytest.mark.parametrize("eta", ["eta", "eta=5", "eta=", "eta=0m", "eta=601m", "eta=1.5m", "eta=2h", "eta= 3m", "eta=+3m"])
def test_invalid_orch_eta_is_denied_with_accepted_form(workspace, config, eta):
    repo, state = workspace
    call = event(prompt=orch(scope="file0.py") + f"|{eta}\nDo work.")

    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    denied = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert "eta=<N>m" in denied
    assert "1..600" in denied


def test_missing_eta_is_recorded_as_null_and_bare_reply_does_not_inherit(workspace, config):
    repo, state = workspace
    first = event(prompt=orch(scope="file0.py") + "\nStart.", tool_use_id="eta-missing")
    codex.pre_tool_use(first, config=config, state_root=state, repo_root=repo)
    assert codex._read_pending(state, first)["eta_minutes"] is None
    first_payload = next(
        item["payload"] for item in _all_telemetry_events(state)
        if item["event_type"] == "IMPLEMENTER_STARTED"
    )
    assert "eta_minutes" in first_payload and first_payload["eta_minutes"] is None

    codex.post_tool_use_failure({**first, "hook_event_name": "PostToolUseFailure",
                                 "error": "fixture completed"}, state_root=state)
    save_live_thread(state)
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1", tool_use_id="bare-reply")
    codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)
    assert codex._read_pending(state, reply)["eta_minutes"] is None


def test_explicit_orch_reason_is_recorded_in_call_event(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(reason="repeated_failure") + "\nFix the failure.")

    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    payload = _call_events(state)[0]["payload"]
    assert payload["reason"] == "repeated_failure"
    assert payload["reason_source"] == "explicit"


def test_bare_reply_inherits_recorded_reason_in_call_event(workspace, config):
    repo, state = workspace
    save_live_thread(state, reason="route_change")
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1")

    codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)

    payload = _call_events(state)[0]["payload"]
    assert payload["reason"] == "route_change"
    assert payload["reason_source"] == "inherited"


@pytest.mark.parametrize(
    ("tool_name", "extra_field", "expected_tool"),
    [
        (codex.CODEX_REPLY_TOOL, "model", "codex-reply"),
        (START_TOOL, "cwd", "codex_start mode=reply"),
        (START_TOOL, "route", "codex_start mode=reply"),
        (START_TOOL, "taskId", "codex_start mode=reply"),
    ],
)
def test_reply_input_invalid_names_tool_and_extra_field(
    workspace, config, tool_name, extra_field, expected_tool
):
    repo, state = workspace
    save_live_thread(state)
    reply = event(tool_name, prompt="Continue.", threadId="thread-1", mode="reply")
    reply["tool_input"][extra_field] = str(repo) if extra_field == "cwd" else "gpt-6-astra"

    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert reason.startswith("CODEX_REPLY_INPUT_INVALID:")
    assert expected_tool in reason
    assert extra_field in reason
    assert f"field {extra_field} is not allowed" in reason


@pytest.mark.parametrize("tool_name", [codex.CODEX_REPLY_TOOL, START_TOOL])
def test_recorded_thread_task_mismatch_names_both_tasks(workspace, config, tool_name):
    repo, state = workspace
    save_live_thread(state)
    reply = event(
        tool_name,
        prompt=orch(task="task-2", reason="continue") + "\nContinue.",
        threadId="thread-1",
        mode="reply",
    )

    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert reason.startswith("THREAD_TASK_MISMATCH:")
    assert "thread-1" in reason
    assert "task-1" in reason
    assert "task-2" in reason
    assert "task id differs" in reason
    assert ("codex_start mode=reply" if tool_name == START_TOOL else "codex-reply") in reason


@pytest.mark.parametrize("tool_name", [codex.CODEX_REPLY_TOOL, START_TOOL])
def test_unknown_recorded_thread_with_orch_still_reports_no_live_thread(workspace, config, tool_name):
    repo, state = workspace
    reply = event(
        tool_name,
        prompt=orch(task="task-2", reason="continue") + "\nContinue.",
        threadId="unknown-thread",
        mode="reply",
    )

    result = codex.pre_tool_use(reply, config=config, state_root=state, repo_root=repo)

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert reason.startswith("NO_LIVE_THREAD:")
    assert "ORCH|task=" in reason
    assert ("codex_start mode=reply" if tool_name == START_TOOL else "codex-reply") in reason


def test_fresh_route_change_records_previous_and_new_route(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    call = event(prompt=orch("subtle", reason="route_change") + "\nStart fresh.")

    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    payload = _call_events(state)[0]["payload"]
    assert payload["previous_route"] == "normal"
    assert payload["new_route"] == "subtle"


@pytest.mark.parametrize("nested", [False, True])
def test_reply_follows_recorded_project_across_session_cwd(config, nested):
    tmp_path = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "project-isolation" / uuid.uuid4().hex
    tmp_path.mkdir(parents=True, exist_ok=True)

    def make_repo(name):
        repo = tmp_path / name
        repo.mkdir(parents=True)
        git_env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10, env=git_env)
        (repo / "file0.py").write_text("value = 1\n", encoding="utf-8")
        (repo / "file1.py").write_text("value = 1\n", encoding="utf-8")
        (repo / "file2.py").write_text("value = 1\n", encoding="utf-8")
        subprocess.run(
            ["git", "add", "."],
            cwd=repo,
            check=True,
            capture_output=True,
            text=True,
            timeout=10,
            env=git_env,
        )
        subprocess.run(
            ["git", "-c", "user.email=tests@example.invalid", "-c", "user.name=Hook Tests", "commit", "-qm", "initial"],
            cwd=repo,
            check=True,
            timeout=10,
            env=git_env,
        )
        return repo

    try:
        state = tmp_path / "state"
        repo_a = make_repo("project-a")
        repo_b = make_repo("project-b") if not nested else make_repo("project-a/.tmp/project-b")
        call_a = event(prompt=orch(task="shared-task") + "\nImplement.", tool_use_id="call-a")
        call_a["cwd"] = str(repo_a)

        codex.pre_tool_use(call_a, config=config, state_root=state)
        post_a = {
            **call_a,
            "hook_event_name": "PostToolUse",
            "tool_response": [
                {"type": "text", "text": json.dumps({"threadId": "thread-a", "content": "done"})}
            ],
        }
        codex.post_tool_use(post_a, config=config, state_root=state)

        reply_b = event(
            codex.CODEX_REPLY_TOOL,
            prompt="Continue without routing.",
            tool_use_id="call-b",
            threadId="thread-a",
        )
        reply_b["cwd"] = str(repo_b)
        result = codex.pre_tool_use(reply_b, config=config, state_root=state)

        assert "updatedInput" in result["hookSpecificOutput"]
        assert codex._read_pending(state, reply_b)["repo_root"] == str(repo_a.resolve())
        reply_post = {
            **reply_b,
            "hook_event_name": "PostToolUse",
            "tool_response": [
                {"type": "text", "text": json.dumps({"threadId": "thread-a", "content": "continued"})}
            ],
        }
        bundle = response_bundle(codex.post_tool_use(reply_post, config=config, state_root=state), config)
        assert bundle["project_root_reason"] is None
        key_a = project_root.project_key(repo_a.resolve())
        key_b = project_root.project_key(repo_b.resolve())
        record_a = ledger.SessionLedger(state / "projects" / key_a).load("shared-task")
        record_b = ledger.SessionLedger(state / "projects" / key_b).load("shared-task")
        assert record_a["record"]["live_thread"]["thread_id"] == "thread-a"
        assert record_b["status"] == ledger.MISSING
    finally:
        remove_test_tree(tmp_path)


def test_legacy_thread_without_index_still_uses_session_project(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1")
    reply["cwd"] = str(repo)

    result = codex.pre_tool_use(reply, config=config, state_root=state)

    assert "updatedInput" in result["hookSpecificOutput"]
    assert codex._read_pending(state, reply)["repo_root"] == str(repo.resolve())


def test_legacy_thread_without_index_in_other_project_is_denied(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    other = repo / ".tmp" / "other"
    other.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=other, check=True, timeout=10)
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1")
    reply["cwd"] = str(other)

    result = codex.pre_tool_use(reply, config=config, state_root=state)

    assert result["hookSpecificOutput"]["permissionDecisionReason"].startswith("NO_LIVE_THREAD:")


def test_indexed_thread_with_lost_git_root_denies_without_fallback(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    codex._record_thread_root(state, "thread-1", repo.resolve())
    (repo / ".git").rename(repo / ".git-gone")
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1")
    reply["cwd"] = str(repo)

    result = codex.pre_tool_use(reply, config=config, state_root=state)

    reason = result["hookSpecificOutput"]["permissionDecisionReason"]
    assert reason.startswith("RECORDED_PROJECT_UNRESOLVED:")
    assert "no longer a git work tree" in reason


def test_indexed_reply_failure_records_in_thread_project(workspace, config):
    repo, state = workspace
    save_live_thread(state)
    codex._record_thread_root(state, "thread-1", repo.resolve())
    other = repo / ".tmp" / "other"
    other.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=other, check=True, timeout=10)
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId="thread-1")
    reply["cwd"] = str(other)

    pre = codex.pre_tool_use(reply, config=config, state_root=state)
    assert "updatedInput" in pre["hookSpecificOutput"]
    failure = {**reply, "hook_event_name": "PostToolUseFailure", "error": "timeout"}
    codex.post_tool_use_failure(failure, config=config, state_root=state)

    record_a = ledger.SessionLedger(state / "projects" / project_root.project_key(repo.resolve())).load("task-1")
    record_b = ledger.SessionLedger(state / "projects" / project_root.project_key(other.resolve())).load("task-1")
    assert record_a["record"]["last_outcome"]["ambiguous_outcome"] is True
    assert record_b["status"] == ledger.MISSING


@pytest.mark.parametrize("thread_id", ["../x", "a/b", "a\\b"])
def test_path_like_thread_id_is_rejected_without_index_path(workspace, config, thread_id):
    repo, state = workspace
    reply = event(codex.CODEX_REPLY_TOOL, prompt="Continue.", threadId=thread_id)
    reply["cwd"] = str(repo)

    result = codex.pre_tool_use(reply, config=config, state_root=state)

    assert codex._thread_index_path(state, thread_id) is None
    assert result["hookSpecificOutput"]["permissionDecisionReason"].startswith("INVALID_THREAD_ID:")
    assert not (state / "threads").exists()


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


def test_sol_route_without_basis_denies_with_protocol_help(workspace, config):
    repo, state = workspace

    result = codex.pre_tool_use(
        event(prompt=orch(route="normal", include_basis=False) + "\nDo the work."),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    decision = result["hookSpecificOutput"]
    assert decision["permissionDecision"] == "deny"
    assert decision["permissionDecisionReason"].startswith("SOL_ROUTE_NEEDS_BASIS:")
    assert "route=normal runs gpt-6-sol" in decision["permissionDecisionReason"]
    assert "basis is required for normal, consequential, and escalated" in decision[
        "permissionDecisionReason"
    ]


@pytest.mark.parametrize(
    ("route", "model", "effort"),
    [
        ("mechanical", "gpt-6-luna", "high"),
        ("subtle", "gpt-6-luna", "high"),
        ("normal", "gpt-6-sol", "medium"),
        ("consequential", "gpt-6-sol", "high"),
        ("escalated", "gpt-6-sol", "xhigh"),
        ("operation", "gpt-6-luna", "high"),
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


def test_routed_call_shows_selected_model_and_effort(workspace, config):
    repo, state = workspace
    result = codex.pre_tool_use(
        event(prompt=orch(route="mechanical") + "\nImplement."),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    assert result["systemMessage"] == "Purser: route=mechanical -> gpt-6-luna / high"


def test_codex_reply_shows_recorded_model_and_effort(workspace, config):
    repo, state = workspace
    save_live_thread(
        state,
        route="mechanical",
        model=config["routes"]["mechanical"]["model"],
        effort=config["routes"]["mechanical"]["effort"],
    )
    result = codex.pre_tool_use(
        event(
            codex.CODEX_REPLY_TOOL,
            prompt="Continue.",
            tool_use_id="reply-route-message",
            threadId="thread-1",
        ),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    assert result["systemMessage"] == "Purser: route=mechanical -> gpt-6-luna / high"


def test_denied_call_has_no_system_message(workspace, config):
    repo, state = workspace
    result = codex.pre_tool_use(
        event(prompt=orch(route="normal", include_basis=False) + "\nDo the work."),
        config=config,
        state_root=state,
        repo_root=repo,
    )

    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert "systemMessage" not in result


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


def test_worker_validation_instruction_separates_final_runs_from_deliberate_red_checks():
    assert (
        "List only final validation runs in `commands`; put any deliberate red check in the report prose, never in the `VALIDATION:` line."
        in codex._WORKER_VALIDATION_INSTRUCTION
    )


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
    record = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")["record"]
    assert record["pending_call_baseline"]["scope"] == ["file0.py", "file2.py"]
    assert record["pending_call_baseline"]["manifest_enabled"] is True

    operation = event(
        prompt=orch(task="operation-1", phase="operation", scope=None) + "\nInspect.",
        tool_use_id="operation-call",
    )
    codex.pre_tool_use(operation, config=config, state_root=state, repo_root=repo)
    operation_record = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("operation-1")["record"]
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
    loaded = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
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
    loaded = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
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
            "ORCH|task=t|phase=implementation|route=consequential|basis=repository design boundary|scope=some/path.rs\n"
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
    assert output["updatedInput"]["model"] == "gpt-6-sol"
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
    loaded = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
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
    loaded = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
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


def test_project_profile_protected_path_reaches_real_hook_bundle(
    workspace, config, monkeypatch
):
    repo, state = workspace
    isolated_home = repo.parent / "isolated-home"
    monkeypatch.setenv("USERPROFILE", str(isolated_home))
    monkeypatch.setenv("HOME", str(isolated_home))
    monkeypatch.setattr(Path, "home", lambda: isolated_home)

    (repo / ".orchestrator").mkdir()
    (repo / ".orchestrator" / "profile.toml").write_text(
        '''schema_version = 1

[project]
id = "external-test"

[paths]
protected = ["project-only-secret.txt"]
''',
        encoding="utf-8",
    )
    protected = repo / "project-only-secret.txt"
    protected.write_text("before\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(
        ["git", "commit", "-qm", "add project profile"], cwd=repo, check=True, timeout=10
    )

    call = event(
        tool_use_id="project-profile-protected",
        scope="file0.py,project-only-secret.txt",
    )
    pre = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert "updatedInput" in pre["hookSpecificOutput"]
    protected.write_text("after\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps({"threadId": "thread-project-profile", "content": "done"}),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            repo_root=repo,
            reviewer=lambda **_kwargs: {
                "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "provider": "stub",
                "validation": "UNKNOWN",
                "findings": [],
            },
        ),
        config,
    )

    assert bundle["protected_paths_touched"] == ["project-only-secret.txt"]


def test_project_profile_force_full_reaches_real_hook_bundle(
    workspace, config, isolated_profile_home
):
    repo, state = workspace
    del isolated_profile_home
    (repo / ".orchestrator").mkdir()
    profile_path = repo / ".orchestrator" / "profile.toml"
    profile_path.write_text(
        """schema_version = 1

[review]
force_full = false
""",
        encoding="utf-8",
    )
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "baseline profile"], cwd=repo, check=True, timeout=10)

    prompt = (
        orch(scope="file0.py")
        + "\n## Objective\nChange file0 within the declared scope."
        + "\n## Acceptance criteria\n- Keep the change bounded to file0."
    )
    call = event(
        tool_use_id="project-profile-force-audit-baseline",
        prompt=prompt,
        scope="file0.py",
    )
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n" * 100, encoding="utf-8")
    reviewer_calls = []

    def reviewer(**_kwargs):
        reviewer_calls.append(_kwargs)
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps(
                    {
                        "threadId": "thread-force-audit-baseline",
                        "content": (
                            'VALIDATION: {"artifacts":[],"commands":["focused-test"],'
                            '"exit_codes":[0],"source":"WORKER_REPORTED","status":"PASS",'
                            '"summary":"focused validation passed"}'
                        ),
                    }
                ),
            }
        ],
    }

    audit_bundle = response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            repo_root=repo,
            reviewer=reviewer,
        ),
        config,
    )

    assert reviewer_calls
    assert audit_bundle["claude_review_mode"] == policy.AUDIT

    subprocess.run(["git", "add", "file0.py"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "audit baseline"], cwd=repo, check=True, timeout=10)
    profile_path.write_text(
        """schema_version = 1

[review]
force_full = true
""",
        encoding="utf-8",
    )
    subprocess.run(["git", "add", ".orchestrator/profile.toml"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "force full profile"], cwd=repo, check=True, timeout=10)

    force_call = event(
        tool_use_id="project-profile-force-full",
        prompt=prompt,
        scope="file0.py",
    )
    codex.pre_tool_use(
        force_call,
        config=config,
        state_root=state / "force-full",
        repo_root=repo,
    )
    force_post = {
        **force_call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps(
                    {
                        "threadId": "thread-force-full",
                        "content": (
                            'VALIDATION: {"artifacts":[],"commands":["focused-test"],'
                            '"exit_codes":[0],"source":"WORKER_REPORTED","status":"PASS",'
                            '"summary":"focused validation passed"}'
                        ),
                    }
                ),
            }
        ],
    }
    bundle = response_bundle(
        codex.post_tool_use(
            force_post,
            config=config,
            state_root=state / "force-full",
            repo_root=repo,
            reviewer=reviewer,
        ),
        config,
    )

    assert bundle["claude_review_mode"] == policy.FULL
    assert any(item["code"] == "PROFILE_FORCES_FULL" for item in bundle["findings"])


def test_project_profile_snapshot_wins_if_profile_changes_between_hooks(
    workspace, config, isolated_profile_home, monkeypatch
):
    repo, state = workspace
    del isolated_profile_home
    (repo / ".orchestrator").mkdir()
    profile_path = repo / ".orchestrator" / "profile.toml"
    profile_path.write_text(
        """schema_version = 1

[paths]
protected = ["captured-secret.txt"]

[review]
force_full = true
""",
        encoding="utf-8",
    )
    (repo / "captured-secret.txt").write_text("before\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "capture profile"], cwd=repo, check=True, timeout=10)

    call = event(
        tool_use_id="project-profile-snapshot",
        scope="captured-secret.txt",
    )
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    profile_path.write_text(
        """schema_version = 1

[paths]
protected = ["different-secret.txt"]

[review]
force_full = false
""",
        encoding="utf-8",
    )
    (repo / "captured-secret.txt").write_text("after\n", encoding="utf-8")
    monkeypatch.setattr(
        codex.project_profile,
        "load_profile",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(AssertionError("profile reloaded")),
    )
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {"type": "text", "text": json.dumps({"threadId": "thread-snapshot", "content": "done"})}
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["claude_review_mode"] == policy.FULL
    assert any(item["code"] == "PROFILE_FORCES_FULL" for item in bundle["findings"])
    assert "captured-secret.txt" in bundle["protected_paths_touched"]


def test_project_profile_skip_thresholds_are_capped_by_config(
    workspace, config, isolated_profile_home, monkeypatch
):
    repo, state = workspace
    user_config = isolated_profile_home / ".orchestrator" / "config.toml"
    user_config.parent.mkdir(parents=True)
    reviewer_executable = isolated_profile_home / "reviewer.exe"
    user_config.write_text(
        f'''schema_version = 1

[reviewer]
executable = "{reviewer_executable.as_posix()}"

[review]
skip_max_lines = 500
skip_max_files = 50
''',
        encoding="utf-8",
    )
    (repo / ".orchestrator").mkdir()
    (repo / ".orchestrator" / "profile.toml").write_text(
        "schema_version = 1\n",
        encoding="utf-8",
    )
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "threshold profile"], cwd=repo, check=True, timeout=10)

    call = event(tool_use_id="project-profile-thresholds", scope="file0.py")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("changed = True\n" * 100, encoding="utf-8")
    captured = {}

    original_gate = codex.policy.reviewer1_gate

    def capture_gate(*args, **kwargs):
        gate_config = kwargs["config"]
        captured["gate"] = dict(gate_config["reviewer_gate"])
        return original_gate(*args, **kwargs)

    monkeypatch.setattr(codex.policy, "reviewer1_gate", capture_gate)

    reviewer_calls = []

    def capture_reviewer(**kwargs):
        reviewer_calls.append(kwargs)
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {"type": "text", "text": json.dumps({"threadId": "thread-thresholds", "content": "done"})}
        ],
    }
    response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            repo_root=repo,
            reviewer=capture_reviewer,
        ),
        config,
    )

    assert captured["gate"]["small_delta_max_lines"] == config["reviewer_gate"]["small_delta_max_lines"]
    assert captured["gate"]["small_delta_max_files"] == config["reviewer_gate"]["small_delta_max_files"]
    assert captured["gate"]["small_delta_max_lines"] <= config["reviewer_gate"]["small_delta_max_lines"]
    assert captured["gate"]["small_delta_max_files"] <= config["reviewer_gate"]["small_delta_max_files"]
    assert reviewer_calls
    assert reviewer_calls[0]["reviewer_executable"] == reviewer_executable.as_posix()
    assert reviewer_calls[0]["authority_sources"] == [
        {
            "path": "AGENTS.md",
            "headings": [
                "Authority",
                "Working scope",
                "Proportional validation",
                "Test and comment quality",
            ],
            "when_changed": None,
        }
    ]


def test_unconfigured_project_is_marked_without_forcing_full(
    workspace, config, isolated_profile_home
):
    repo, state = workspace
    del isolated_profile_home
    prompt = (
        orch(scope="file0.py")
        + '\nTASK_CONTRACT: {"objective":"Change file0","criteria":["Keep the change bounded."]}'
        + "\nDo the work."
    )
    call = event(tool_use_id="project-profile-unconfigured", prompt=prompt, scope="file0.py")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    validation = (
        'VALIDATION: {"artifacts":[],"commands":["focused-test"],"exit_codes":[0],'
        '"source":"WORKER_REPORTED","status":"PASS","summary":"focused validation passed"}'
    )
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps(
                    {"threadId": "thread-unconfigured", "content": validation}
                ),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(
            post,
            config=config,
            state_root=state,
            repo_root=repo,
            reviewer=lambda **_kwargs: {
                "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                "validation": "PASS",
                "findings": [],
            },
        ),
        config,
    )

    assert bundle["profile"] == "not_configured"
    assert bundle["claude_review_mode"] != policy.FULL


def test_post_tool_use_detects_initial_out_of_scope_and_engine_protected_files(workspace, config):
    repo, state = workspace
    call = event(tool_use_id="initial-out-of-scope", scope="file0.py")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)

    (repo / "outside.txt").write_text("outside\n", encoding="utf-8")
    (repo / ".orchestrator").mkdir()
    (repo / ".orchestrator" / "profile.toml").write_text(
        "schema_version = 1\n", encoding="utf-8"
    )
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps(
                    {"threadId": "thread-initial-out-of-scope", "content": "done"}
                ),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert bundle["claude_review_mode"] == policy.FULL
    findings = [
        item for item in bundle["findings"] if item["code"] == "OUT_OF_SCOPE_MODIFICATION"
    ]
    assert len(findings) == 1
    assert "2 file(s)" in findings[0]["message"]
    assert ".orchestrator/profile.toml" in findings[0]["message"]
    assert bundle["protected_paths_touched"] == [".orchestrator/profile.toml"]


def test_post_tool_use_ignores_out_of_scope_file_present_in_baseline(workspace, config):
    repo, state = workspace
    (repo / "preexisting-outside.txt").write_text("already here\n", encoding="utf-8")
    call = event(tool_use_id="baseline-out-of-scope", scope="file0.py")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {
                "type": "text",
                "text": json.dumps(
                    {"threadId": "thread-baseline-out-of-scope", "content": "done"}
                ),
            }
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo),
        config,
    )

    assert not any(
        item["code"] == "OUT_OF_SCOPE_MODIFICATION" for item in bundle["findings"]
    )


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
    loaded = ledger.SessionLedger(
        state / "projects" / project_root.project_key(repo.resolve())
    ).load("task-1")
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


def test_reviewer_agy_keeps_audit_receipt_behavior(workspace, config):
    repo, state = workspace
    user_profile = Path.home() / ".orchestrator" / "config.toml"
    user_profile.parent.mkdir(parents=True, exist_ok=True)
    user_profile.write_text(
        'schema_version = 1\n[roles]\nreviewer = "agy"\n', encoding="utf-8"
    )
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
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


def test_reviewer_disabled_skips_reviewer_and_forces_full(workspace, config, monkeypatch):
    repo, state = workspace
    user_profile = Path.home() / ".orchestrator" / "config.toml"
    user_profile.parent.mkdir(parents=True, exist_ok=True)
    user_profile.write_text(
        'schema_version = 1\n[roles]\nreviewer = "none"\n', encoding="utf-8"
    )
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        "Task Objective: Update file0 with new value\n"
        "Acceptance Criteria: file0 contains value = 2\n"
    )
    call = event(prompt=prompt, tool_use_id="reviewer-disabled")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending = codex._read_pending(state, call)
    assert pending["reviewer"] == "none"
    hook_check_calls = []
    monkeypatch.setattr(codex.checks, "select_checks", lambda *_args: ("check",))
    monkeypatch.setattr(
        codex.checks,
        "run_checks",
        lambda *_args, **_kwargs: hook_check_calls.append("ran")
        or {"status": "PASS", "results": []},
    )
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [{"type": "text", "text": json.dumps({
            "threadId": "thread-disabled",
            "content": 'updated\nVALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}',
        })}],
    }
    reviewer_calls = []

    def reviewer(**kwargs):
        reviewer_calls.append(kwargs)
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    raw = codex.post_tool_use(
        post, config=config, state_root=state, repo_root=repo, reviewer=reviewer
    )
    bundle = response_bundle(raw, config)
    assert hook_check_calls == ["ran"]
    assert bundle["hook_checks"]["status"] == "PASS"
    assert reviewer_calls == []
    assert bundle["reviewer1_skipped_reason"] == "REVIEWER_DISABLED"
    assert any(
        finding["code"] == "REVIEWER_DISABLED"
        and finding["message"] == "no independent reviewer configured; FULL review required"
        for finding in bundle["findings"]
    )
    assert bundle["claude_review_mode"] == policy.FULL
    assert bundle["audit_receipt"] is None


def test_worker_validation_parses_full_report_before_phase_cap(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=task-1|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
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
        "ORCH|task=task-1|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py,file1.py,file2.py\n"
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


def test_micro_repair_hides_windows_console(monkeypatch):
    calls = []
    monkeypatch.setattr(codex, "os", type("OS", (), {"name": "nt"})())
    monkeypatch.setattr(codex.subprocess, "run", lambda *args, **kwargs: calls.append(kwargs)
                        or subprocess.CompletedProcess(args[0], 0, "ok", ""))

    codex._attempt_in_hook_micro_repair("thread", [{"code": "F", "message": "finding"}], Path.cwd())

    assert calls[0]["creationflags"] & 0x08000000


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
        "ORCH|task=task-a|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\n"
        'TASK_CONTRACT: {"objective": "Fix deadline logic", "criteria": "Zero latency drift"}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json, "task-a", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Fix deadline logic"
    assert crit == "Zero latency drift"

    # Case B: Multiline headers
    prompt_multiline = (
        "ORCH|task=task-b|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\n"
        "Task Objective:\nRefactor connection pool\n"
        "Acceptance Criteria:\nPreserve retry backoff"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_multiline, "task-b", "implementation", ["file.py"])
    assert grounded is True
    assert obj == "Refactor connection pool"
    assert crit == "Preserve retry backoff"

    # Case C: Generic prompt without explicit contract
    prompt_generic = "ORCH|task=task-c|phase=implementation|route=normal|basis=test fixture design boundary|scope=file.py\nJust do the work"
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
        "ORCH|task=task-val|phase=implementation|route=normal|basis=test fixture design boundary|scope=f0.py,f1.py,f2.py\n"
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
        prompt="ORCH|task=task-err|phase=implementation|route=normal|basis=test fixture design boundary|scope=g0.py,g1.py,g2.py"
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
    assert res1["summary"] == "Validation: tests never passed"
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
    assert res10["summary"] == "Multiple conflicting VALIDATION records were reported."
    assert set(res10.keys()) == expected_keys

    # 11. İki valid PASS kaydı => UNKNOWN (multiple records)
    multi_valid = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}\n'
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}'
    )
    res11 = codex._parse_worker_validation(multi_valid)
    assert res11["status"] == "UNKNOWN"
    assert res11["summary"] == "Multiple conflicting VALIDATION records were reported."
    assert set(res11.keys()) == expected_keys

    # 12. Boyut sınırını aşan summary => UNKNOWN
    big_summary = 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"' + ('x' * 1300) + '","commands":["pytest"],"exit_codes":[0]}'
    res12 = codex._parse_worker_validation(big_summary)
    assert res12["status"] == "UNKNOWN"
    assert set(res12.keys()) == expected_keys
    assert len(json_transport.dumps(res12).encode("utf-8")) <= codex.IMPLEMENTER_REPORT_LIMIT


def test_worker_validation_ignores_prose_heading_before_structured_record():
    report = (
        "Validation:\n"
        "- Ran the focused checks.\n"
        "- No issues found.\n"
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest"],"exit_codes":[0]}\n'
    )

    result = codex._parse_worker_validation(report)

    assert result["status"] == "PASS"
    assert result["commands"] == ["pytest"]
    assert result["exit_codes"] == [0]


def test_worker_validation_rejects_two_genuine_records_as_multiple():
    record = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest"],"exit_codes":[0]}'
    )
    result = codex._parse_worker_validation(f"{record}\n{record}")

    assert result["status"] == "UNKNOWN"
    assert result["summary"] == "Multiple conflicting VALIDATION records were reported."


def test_worker_validation_ignores_mixed_case_prose_line():
    result = codex._parse_worker_validation("Validation: npm test passed")

    assert result["status"] == "UNKNOWN"
    assert result["summary"] == "Validation: npm test passed"


@pytest.mark.parametrize(
    ("line", "reason"),
    [
        ("VALIDATION: {not json", "invalid_json"),
        ("VALIDATION: []", "not_object"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0],"zeta":2,"alpha":1}',
            "unknown_keys=alpha,zeta",
        ),
        (
            'VALIDATION: {"source":"OTHER","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "bad_source",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"MAYBE",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "bad_status",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"summary":"   ","commands":["pytest"],"exit_codes":[0]}',
            "bad_summary",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":"pytest","exit_codes":[0]}',
            "bad_commands",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0],"artifacts":"report.txt"}',
            "bad_artifacts",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[true]}',
            "bad_exit_codes",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[]}',
            "commands_exit_codes_mismatch",
        ),
        ('VALIDATION: {"source":"WORKER_REPORTED","status":"PASS"}', "pass_without_commands"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[1]}',
            "pass_with_nonzero_exit",
        ),
        ('VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL"}', "fail_without_commands"),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"FAIL",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "fail_without_nonzero_exit",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"'
            + ("x" * 1300)
            + '","commands":["pytest"],"exit_codes":[0]}',
            "record_too_large",
        ),
        (
            'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
            '"commands":["pytest"],"exit_codes":[0]}',
            "serialization_error",
        ),
    ],
)
def test_worker_validation_rejection_reason_is_first_and_preserves_line(line, reason, monkeypatch):
    if reason == "serialization_error":
        original_dumps = codex.json_transport.dumps

        def fail_pass_serialization(value, *args, **kwargs):
            if isinstance(value, dict) and value.get("status") == "PASS":
                raise ValueError("simulated serialization failure")
            return original_dumps(value, *args, **kwargs)

        monkeypatch.setattr(codex.json_transport, "dumps", fail_pass_serialization)

    result = codex._parse_worker_validation(line)

    assert result["status"] == "UNKNOWN"
    assert result["summary"].startswith(f"{reason}: ")
    if reason == "record_too_large":
        assert result["summary"].startswith(f"{reason}: {line[:40]}")
    else:
        assert result["summary"] == f"{reason}: {line}"


def test_worker_validation_accepted_record_is_unchanged():
    line = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"summary":"focused validation passed","commands":["pytest -q"],'
        '"exit_codes":[0],"artifacts":["report.txt"]}'
    )

    assert codex._parse_worker_validation(line) == {
        "source": "WORKER_REPORTED",
        "status": "PASS",
        "summary": "focused validation passed",
        "commands": ["pytest -q"],
        "exit_codes": [0],
        "artifacts": ["report.txt"],
    }


def test_worker_validation_accepts_backticked_record():
    line = (
        '`VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"summary":"focused validation passed","commands":["pytest -q"],'
        '"exit_codes":[0],"artifacts":["report.txt"]}`'
    )

    assert codex._parse_worker_validation(line)["status"] == "PASS"


def test_worker_validation_backticked_and_plain_records_count_as_multiple():
    record = (
        'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest -q"],"exit_codes":[0]}'
    )

    result = codex._parse_worker_validation(f'`{record}`\n{record}')

    assert result["status"] == "UNKNOWN"
    assert result["summary"].startswith("Multiple conflicting VALIDATION records")


def test_worker_validation_does_not_unwrap_only_leading_backtick():
    record = (
        '`VALIDATION: {"source":"WORKER_REPORTED","status":"PASS",'
        '"commands":["pytest -q"],"exit_codes":[0]}'
    )

    assert codex._parse_worker_validation(record)["status"] == "UNKNOWN"


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
    prompt_obj_only = "ORCH|task=t1|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nTask Objective: Only objective"
    obj, crit, grounded = codex._parse_task_contract(prompt_obj_only, "t1", "implementation", ["f.py"])
    assert grounded is False
    assert obj == "Only objective"

    # Case 2: Only criteria provided -> grounded MUST be False
    prompt_crit_only = "ORCH|task=t2|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nAcceptance Criteria: Only criteria"
    obj, crit, grounded = codex._parse_task_contract(prompt_crit_only, "t2", "implementation", ["f.py"])
    assert grounded is False
    assert crit == "Only criteria"

    # Case 3: Multiline bullet list criteria
    prompt_bullets = (
        "ORCH|task=t3|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
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
        "ORCH|task=t4|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        'TASK_CONTRACT: {"objective": "Feature", "criteria": ["Crit 1", "Crit 2"]}'
    )
    obj, crit, grounded = codex._parse_task_contract(prompt_json_list, "t4", "implementation", ["f.py"])
    assert grounded is True
    assert obj == "Feature"
    assert crit == "Crit 1; Crit 2"


def test_plain_objective_label_grounds_with_next_line_and_inline_criteria():
    prompt = (
        "ORCH|task=plain|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "oBjEcTiVe:\n"
        "Recognize plain objective labels. Acceptance criteria: preserve grounding"
    )
    obj, crit, grounded = codex._parse_task_contract(prompt, "plain", "implementation", ["f.py"])

    assert grounded is True
    assert obj == "Recognize plain objective labels"
    assert crit == "preserve grounding"


def test_mid_sentence_objective_is_not_a_plain_objective_label():
    prompt = (
        "ORCH|task=mid|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "This sentence mentions objective: but is not a label.\n"
        "Acceptance Criteria: preserve grounding"
    )
    obj, _, grounded = codex._parse_task_contract(prompt, "mid", "implementation", ["f.py"])

    assert grounded is False
    assert "Task mid" in obj


def test_first_task_or_plain_objective_label_wins():
    prompt = (
        "ORCH|task=precedence|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
        "Task objective: first objective\n"
        "Objective: second objective\n"
        "Acceptance Criteria: preserve grounding"
    )
    obj, _, grounded = codex._parse_task_contract(
        prompt, "precedence", "implementation", ["f.py"]
    )

    # If both labels appear, the first one in the prompt wins.
    assert grounded is True
    assert obj == "first objective"


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
        "ORCH|task=task-cont|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\n"
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
    reply_prompt = "ORCH|task=task-cont|phase=implementation|route=normal|basis=test fixture design boundary|scope=f.py\nContinue work"
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


def test_codex_reply_inherits_handoff_and_reviews_cumulative_same_path_diff(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=reply-repair|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Both turns are reviewed"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-repair-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\n" + "".join(f"turn_one_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-repair",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    diffs = []

    def stub_reviewer(**kwargs):
        _, diff = reviewer_module._delegated_diff(
            Path(kwargs["repo_root"]), kwargs["delta"], kwargs["classification"]
        )
        diffs.append(diff)
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )

    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-repair-second",
        threadId="thread-reply-repair",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        (repo / "file0.py").read_text(encoding="utf-8")
        + "".join(f"turn_two_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-repair",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert len(diffs) == 2
    assert "+turn_one_0 = True" in diffs[1]
    assert "+turn_two_0 = True" in diffs[1]
    assert bundle["concurrent_writer"] is False
    assert "UNKNOWN_CONCURRENT_WRITER" not in [finding["code"] for finding in bundle["findings"]]
    assert "REVIEW_PACKET_INCOMPLETE" not in [finding["code"] for finding in bundle["findings"]]


def test_codex_reply_does_not_inherit_handoff_after_intervening_edit(workspace, config):
    repo, state = workspace
    prompt = (
        "ORCH|task=reply-writer|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Reject concurrent edits"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-writer-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 1\nturn_one = True\n", encoding="utf-8")
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-writer",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }

    seen_deltas = []

    def stub_reviewer(**kwargs):
        seen_deltas.append(kwargs["delta"])
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    (repo / "file0.py").write_text(
        "value = 1\nturn_one = True\nother_writer = True\n", encoding="utf-8"
    )
    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-writer-second",
        threadId="thread-reply-writer",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\nturn_one = True\nother_writer = True\nturn_two = True\n",
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-writer",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert bundle["claude_review_mode"] == policy.FULL
    assert "UNKNOWN_CONCURRENT_WRITER" in [finding["code"] for finding in bundle["findings"]]


@pytest.mark.parametrize("halt_paths", [["file0.py"], [f"path-{i:02d}.py" for i in range(21)], None])
def test_codex_reply_keeps_pre_turn_dirty_path_ambiguous(workspace, config, monkeypatch, halt_paths):
    repo, state = workspace
    original_classify = ledger.classify_concurrent_writers
    def classify_with_halt_paths(*args, **kwargs):
        result = original_classify(*args, **kwargs)
        if "file0.py" in result.get("ambiguous_paths", []):
            result["halt_paths"] = halt_paths
            if halt_paths is None:
                result["code"] = "UNKNOWN_DELTA"
        return result
    monkeypatch.setattr(ledger, "classify_concurrent_writers", classify_with_halt_paths)
    observed = []
    monkeypatch.setattr(codex, "_observe", lambda entrypoint, *args, **kwargs: observed.append((entrypoint, args, kwargs)))
    (repo / "file0.py").write_text("value = 1\nuser_before = True\n", encoding="utf-8")
    prompt = (
        "ORCH|task=reply-dirty|phase=implementation|route=normal|basis=test fixture design boundary|scope=file0.py\n"
        'TASK_CONTRACT: {"objective": "Complete the repair", "criteria": "Keep user edits attributable"}'
    )
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    first = event(prompt=prompt, tool_use_id="reply-dirty-first")
    codex.pre_tool_use(first, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        "value = 1\nuser_before = True\n" + "".join(
            f"turn_one_{index} = True\n" for index in range(30)
        ),
        encoding="utf-8",
    )
    response = {
        **first,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-dirty",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }

    seen_deltas = []

    def stub_reviewer(**kwargs):
        seen_deltas.append(kwargs["delta"])
        return {
            "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
            "validation": "PASS",
            "findings": [],
        }

    codex.post_tool_use(
        response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    reply = event(
        tool_name=codex.CODEX_REPLY_TOOL,
        prompt="Continue the repair.",
        tool_use_id="reply-dirty-second",
        threadId="thread-reply-dirty",
    )
    codex.pre_tool_use(reply, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text(
        (repo / "file0.py").read_text(encoding="utf-8")
        + "".join(f"turn_two_{index} = True\n" for index in range(30)),
        encoding="utf-8",
    )
    second_response = {
        **reply,
        "hook_event_name": "PostToolUse",
        "tool_response": [{
            "type": "text",
            "text": json.dumps({
                "threadId": "thread-reply-dirty",
                "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
            }),
        }],
    }
    raw = codex.post_tool_use(
        second_response,
        config=review_config,
        state_root=state,
        repo_root=repo,
        reviewer=stub_reviewer,
    )
    bundle = response_bundle(raw, review_config)

    assert bundle["concurrent_writer"] is False
    assert "file0.py" in seen_deltas[-1]["ambiguous_paths"]
    code = "UNKNOWN_DELTA" if halt_paths is None else "UNKNOWN_CONCURRENT_WRITER"
    halt = next(finding for finding in bundle["findings"] if finding["code"] == code)
    if halt_paths is None:
        assert halt["message"] == "Concurrent-writer classification requires FULL Claude review; halt paths unavailable (UNKNOWN_DELTA)"
    else:
        preview = ", ".join(halt_paths[:10]) + (" …" if len(halt_paths) > 10 else "")
        assert halt["message"] == f"Concurrent-writer classification requires FULL Claude review; halt paths ({len(halt_paths)}): {preview}"
    payload = next(kwargs for entrypoint, args, kwargs in observed if args and args[0] == "UNKNOWN_CONCURRENT_WRITER")
    assert payload["code"] == code
    assert payload["halt_paths"] == (halt_paths[:20] if halt_paths is not None else None)
    assert payload["halt_path_count"] == (len(halt_paths) if halt_paths is not None else None)


@pytest.mark.parametrize("case", ["invalid", "save_error", "valid"])
def test_reviewer_raw_evidence_only_for_invalid_response(workspace, config, monkeypatch, case):
    repo, state = workspace
    review_config = dict(config, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    call = event(tool_use_id=f"reviewer-raw-{case}")
    codex.pre_tool_use(call, config=review_config, state_root=state, repo_root=repo)
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [{"type": "text", "text": json.dumps({
            "threadId": f"thread-reviewer-raw-{case}",
            "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["focused"],"exit_codes":[0]}',
        })}],
    }
    if case == "valid":
        report = reviewer_module.CompletedReview(
            policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE, "PASS", (), "conversation-1", "gemini-3.8-flash-high"
        )
    else:
        report = reviewer_module.UnavailableReview(
            "REVIEWER_RESPONSE_INVALID", "Reviewer-1 response or configuration was invalid (JSONDecodeError).",
            "gemini-3.8-flash-high", raw_stdout="{invalid json", decoder_detail="Expecting property name: line 1 column 2 (char 1)",
        )
    if case == "save_error":
        original_persist = codex._persist_raw_output
        def fail_reviewer_save(root, event, value, suffix=""):
            if suffix == "-reviewer1":
                raise OSError("disk full")
            return original_persist(root, event, value, suffix)
        monkeypatch.setattr(codex, "_persist_raw_output", fail_reviewer_save)
    raw = codex.post_tool_use(
        post, config=review_config, state_root=state, repo_root=repo, reviewer=lambda **kwargs: report
    )
    bundle = response_bundle(raw, review_config)
    evidence = list((state / "raw_tool_outputs").glob("*-reviewer1.json"))
    if case == "valid":
        assert evidence == []
        assert bundle["reviewer1_result"] == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
    else:
        finding = next(item for item in bundle["findings"] if item["code"] == "REVIEWER_RESPONSE_INVALID")
        assert bundle["reviewer1_result"] == policy.REVIEWER1_UNAVAILABLE
        if case == "invalid":
            assert len(evidence) == 1
            assert json.loads(evidence[0].read_text(encoding="utf-8")) == "{invalid json"
            assert str(evidence[0]) in finding["message"]
            assert "Expecting property name: line 1 column 2 (char 1)" in finding["message"]
        else:
            assert evidence == []
            assert "raw output not saved (OSError)" in finding["message"]


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
            "ORCH|task=t-mat|phase=implementation|route=normal|basis=test fixture design boundary|scope=test_matrix.py\n"
            "Task Objective: Update test matrix file\n"
            "Acceptance Criteria: test_matrix.py has value 2"
        )
    else:
        prompt = "ORCH|task=t-mat|phase=implementation|route=normal|basis=test fixture design boundary|scope=test_matrix.py\nGeneric prompt"

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


def test_post_accepts_pending_written_before_project_namespacing(workspace, config):
    repo, state = workspace
    call = event(prompt=orch(task="legacy-pending") + "\nImplement.", tool_use_id="call-legacy")
    codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(Path(state), call)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending.pop("project_key")
    pending_path.write_text(json.dumps(pending), encoding="utf-8")
    (repo / "file0.py").write_text("value = 2\n", encoding="utf-8")
    post = {
        **call,
        "hook_event_name": "PostToolUse",
        "tool_response": [
            {"type": "text", "text": json.dumps({"threadId": "thread-legacy", "content": "done"})}
        ],
    }

    bundle = response_bundle(
        codex.post_tool_use(post, config=config, state_root=state, repo_root=repo), config
    )

    assert not any(item["code"] == "INTERNAL_POST_ERROR" for item in bundle["findings"])
    assert bundle["thread_id"] == "thread-legacy"


def test_hook_main_reads_utf8_and_emits_utf8_with_cp1254_wrappers(monkeypatch):
    payload = {"hook_event_name": "unknown", "prompt": "❯ ı"}
    input_raw = io.BytesIO(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(hook, "dispatch", lambda event_value: event_value)

    assert hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8")) == payload


def test_hook_main_writes_non_ascii_response_as_utf8_with_cp1254(monkeypatch):
    output_raw = io.BytesIO()
    monkeypatch.setattr(hook.sys, "stdin", io.TextIOWrapper(io.BytesIO(b"{}"), encoding="cp1254"))
    monkeypatch.setattr(hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(hook, "dispatch", lambda _event_value: {"marker": "❯"})

    assert hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8")) == {"marker": "❯"}
