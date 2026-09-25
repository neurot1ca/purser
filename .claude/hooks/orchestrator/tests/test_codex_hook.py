import io
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


@pytest.mark.parametrize("name", ("codex_start", "codex_result", "codex_cancel"))
def test_async_tools_fail_closed_until_lifecycle_is_wired(workspace, config, name):
    repo, state = workspace
    call = event(tool_name=f"mcp__plugin_purser_codex-main__{name}")
    call["tool_input"] = {"jobId": "0" * 64} if name != "codex_start" else {
        "prompt": orch() + "\nDo the work.", "call": "claude-must-not-control-this"
    }
    result = codex.pre_tool_use(call, config=config, state_root=state, repo_root=repo)
    assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
    assert not list((state / "pending").glob("*.json"))


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
