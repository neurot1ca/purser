import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys

import pytest

from contract_hook_runner import run_contract_hook


PACKAGE = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).parents[4]
TOOL = "mcp__codex-main__codex"
START = "mcp__codex-main__codex_start"
ORCH = "ORCH|task=contract-task|phase=implementation|route=normal|reason=new_task|basis=bounded process contract|scope=changed.py"
THREAD = {"structuredContent": {"threadId": "contract-thread", "content": "Implemented the change."}}


@pytest.fixture
def repo(tmp_path):
    root = tmp_path / "repo"
    root.mkdir()
    subprocess.run(["git", "init", "-q", str(root)], check=True, timeout=10)
    for args in (("config", "user.email", "tests@example.invalid"),
                 ("config", "user.name", "Contract Test")):
        subprocess.run(["git", *args], cwd=root, check=True, timeout=10)
    (root / "changed.py").write_text("value = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=root, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "baseline"], cwd=root, check=True, timeout=10)
    return root


@pytest.fixture
def process(tmp_path, monkeypatch):
    state = tmp_path / "state"
    event_overrides = {
        event: os.environ.get(f"PURSER_HOOK_CMD_{event}")
        for event in ("PRETOOLUSE", "POSTTOOLUSE", "POSTTOOLUSEFAILURE")
    }
    global_override = os.environ.get("PURSER_HOOK_CMD")
    wrapper = tmp_path / "hook_with_test_seam.py"
    wrapper.write_text(
        "import os, sys\n"
        f"sys.path.insert(0, {str(PACKAGE)!r})\n"
        "import codex, hook\n"
        "mode = os.environ.get('PURSER_CONTRACT_SEAM')\n"
        "if mode == 'config-error':\n"
        "    def fail_config(*args, **kwargs): raise RuntimeError('injected config failure')\n"
        "    codex._load_config = fail_config\n"
        "elif mode == 'post-error':\n"
        "    def fail_post(*args, **kwargs): raise RuntimeError('injected post failure')\n"
        "    codex.post_tool_use = fail_post\n"
        "elif mode in ('reviewer-ok', 'reviewer-unavailable'):\n"
        "    def reviewer(**kwargs): return {'result': 'REVIEWER1_UNAVAILABLE' if mode == 'reviewer-unavailable' else 'NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE', 'validation': 'UNKNOWN' if mode == 'reviewer-unavailable' else 'PASS', 'findings': []}\n"
        "    reviewer.available = True\n"
        "    codex.REVIEWER = reviewer\n"
        "raise SystemExit(hook.main())\n",
        encoding="utf-8",
    )

    def run(event, payload, seam=None):
        event_key = f"PURSER_HOOK_CMD_{event.upper()}"
        event_override = event_overrides.get(event.upper())
        configured_override = event_override if event_override is not None else global_override
        if event_overrides.get(event.upper()) is None:
            monkeypatch.delenv(event_key, raising=False)
        else:
            monkeypatch.setenv(event_key, event_overrides[event.upper()])
        if seam:
            monkeypatch.setenv("PURSER_CONTRACT_SEAM", seam)
            if configured_override is None:
                argv = [sys.executable, str(wrapper)]
                command = subprocess.list2cmdline(argv) if os.name == "nt" else shlex.join(argv)
                monkeypatch.setenv(event_key, command)
        else:
            monkeypatch.delenv("PURSER_CONTRACT_SEAM", raising=False)
        return run_contract_hook(event, payload, state, entrypoint="enforcement")

    return state, run


def event(event_name, repo, tool_use_id="contract-call", tool=TOOL, **fields):
    return {"session_id": "process-contract", "hook_event_name": event_name,
            "tool_name": tool, "tool_use_id": tool_use_id, "cwd": str(repo), **fields}


def pre(repo, *, tool_use_id="contract-call", tool=TOOL, prompt=None):
    tool_input = {"label": "contract task", "prompt": prompt or ORCH + "\nChange the value."}
    return event("PreToolUse", repo, tool_use_id, tool, tool_input=tool_input)


def decision(result):
    assert result.returncode == 0, result.stderr.decode("utf-8", errors="replace")
    return json.loads(result.stdout)


def pending_record(state):
    records = list((state / "pending").glob("*.json")) + list((state / "pre_bypass").glob("*.json"))
    assert len(records) == 1
    return json.loads(records[0].read_text(encoding="utf-8"))


def bundle(result):
    output = decision(result)["hookSpecificOutput"]
    assert output["hookEventName"] == "PostToolUse"
    return json.loads(output["updatedToolOutput"][0]["text"])


def test_pretool_allow_process_contract(repo, process):
    state, run = process
    result = run("PreToolUse", pre(repo))
    output = decision(result)
    assert output.keys() == {"hookSpecificOutput", "systemMessage"}
    specific = output["hookSpecificOutput"]
    assert specific["hookEventName"] == "PreToolUse"
    assert "permissionDecision" not in specific
    updated = specific["updatedInput"]
    assert updated["model"] == "gpt-6.1-sol"
    assert updated["config"] == {
        "model_reasoning_effort": "medium", "tool_output_token_limit": 8000,
    }
    assert updated["prompt"].startswith("Change the value.\n")
    assert ORCH not in updated["prompt"]
    saved = pending_record(state)
    assert {key: saved[key] for key in ("kind", "task_id", "route", "model", "effort", "scope")} == {
        "kind": "call", "task_id": "contract-task", "route": "normal",
        "model": "gpt-6.1-sol", "effort": "medium", "scope": ["changed.py"],
    }
    assert saved["baseline"]["status"] == "OK"


def test_pretool_policy_deny_process_contract(repo, process):
    state, run = process
    output = decision(run("PreToolUse", pre(repo, prompt="Do this without the required routing contract.")))
    assert output == {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "deny",
        "permissionDecisionReason": output["hookSpecificOutput"]["permissionDecisionReason"],
    }}
    assert output["hookSpecificOutput"]["permissionDecisionReason"].startswith("MISSING_ORCH_LINE:")
    assert not list(state.glob("pending/*.json"))
    records = list((state / "telemetry").rglob("*.jsonl"))
    assert any("ROUTING_PROTOCOL_VIOLATION" in path.read_text(encoding="utf-8") for path in records)


def test_sync_pretool_internal_error_is_visible_and_durable(repo, process):
    state, run = process
    result = run("PreToolUse", pre(repo), "config-error")
    output = decision(result)["hookSpecificOutput"]
    assert set(output) == {"hookEventName", "additionalContext"}
    assert output["hookEventName"] == "PreToolUse"
    assert "internal error: RuntimeError: injected config failure" in output["additionalContext"]
    assert "allowed unmodified" in output["additionalContext"]
    record = pending_record(state)
    assert {key: record[key] for key in ("kind", "routing_enforced", "baseline_captured", "reason")} == {
        "kind": "pretool_bypass", "routing_enforced": False,
        "baseline_captured": False, "reason": "RuntimeError: injected config failure",
    }
    diagnostic = (state / "diagnostics.jsonl").read_text(encoding="utf-8")
    assert '"phase": "PreToolUse"' in diagnostic and "injected config failure" in diagnostic


def test_async_start_internal_error_denies_without_durable_route(repo, process):
    state, run = process
    result = run("PreToolUse", pre(repo, tool_use_id="async-call", tool=START), "config-error")
    output = decision(result)
    assert output == {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "deny",
        "permissionDecisionReason": "ASYNC_ENFORCEMENT_FAILED: Async start lacked a durable route/baseline; FULL review is required.",
    }}
    assert not list((state / "pending").glob("*.json"))
    assert not list((state / "pre_bypass").glob("*.json"))
    diagnostic = result.stderr.decode("utf-8")
    assert "injected config failure" in diagnostic
    assert '"phase": "PreToolUse"' in diagnostic
    assert '"tool_use_id": "async-call"' in diagnostic


def test_go_pretooluse_tools_cover_hooks_json(repo):
    sys.path.insert(0, str(PACKAGE))
    import codex

    hooks = json.loads((REPO_ROOT / "hooks" / "hooks.json").read_text(encoding="utf-8"))
    go_source = (REPO_ROOT / "go" / "internal" / "enforce" / "pre.go").read_text(encoding="utf-8")
    go_map = go_source.split("var codexTools = map[string]bool{", 1)[1].split("}", 1)[0]
    go_names = set(re.findall(r'"(mcp__[^" ]+)"\s*:', go_map))
    hooked_names = set()
    for entry in hooks["hooks"]["PreToolUse"]:
        matcher = re.fullmatch(r"\^([^()]+)\(([^()]+)\)\$", entry["matcher"])
        assert matcher, entry["matcher"]
        prefix, alternatives = matcher.groups()
        names = {prefix + suffix for suffix in alternatives.split("|")}
        assert names <= codex.CODEX_TOOLS
        hooked_names.update(names)
    assert hooked_names <= go_names


def test_malformed_json_uses_outer_recovery(repo, process):
    _, run = process
    result = run("PreToolUse", b'{"hook_event_name":')
    assert result.returncode == 0
    assert result.stdout == b"{}\n"
    assert b"JSONDecodeError" in result.stderr


def test_posttool_reviewed_output_keeps_route_attribution(repo, process):
    state, run = process
    call = pre(repo)
    assert "updatedInput" in decision(run("PreToolUse", call))["hookSpecificOutput"]
    (repo / "changed.py").write_text("".join(f"value_{i} = {i}\n" for i in range(80)), encoding="utf-8")
    result = event("PostToolUse", repo, tool_response=THREAD)
    reviewed = bundle(run("PostToolUse", result, "reviewer-ok"))
    assert {key: reviewed[key] for key in ("task_id", "route", "model", "effort", "thread_id", "claude_review_mode")} == {
        "task_id": "contract-task", "route": "normal", "model": "gpt-6.1-sol",
        "effort": "medium", "thread_id": "contract-thread", "claude_review_mode": "FULL",
    }
    artifact = json.loads(Path(reviewed["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["route"] == "normal" and artifact["reviewer1_result"] == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
    assert any("IMPLEMENTER_FINISHED" in p.read_text(encoding="utf-8")
               for p in (state / "telemetry").rglob("*.jsonl"))


def test_tool_failure_is_unknown_full_and_skips_reviewer(repo, process):
    state, run = process
    call = pre(repo)
    decision(run("PreToolUse", call))
    failed = event("PostToolUseFailure", repo, error="worker exited", is_interrupt=False,
                   duration_ms=23)
    output = decision(run("PostToolUseFailure", failed))["hookSpecificOutput"]
    assert set(output) == {"hookEventName", "additionalContext"}
    assert output["hookEventName"] == "PostToolUseFailure"
    assert "work_performed is UNKNOWN" in output["additionalContext"]
    assert "FULL Claude review is required" in output["additionalContext"]
    files = list((state / "raw_tool_outputs").glob("*-failure.json"))
    assert len(files) == 1
    evidence = json.loads(files[0].read_text(encoding="utf-8"))
    assert {key: evidence[key] for key in (
        "ambiguous_outcome", "work_performed", "error", "is_interrupt", "duration_ms"
    )} == {"ambiguous_outcome": True, "work_performed": "UNKNOWN", "error": "worker exited",
          "is_interrupt": False, "duration_ms": 23}
    pending = evidence["pending_call"]
    assert pending["route"] == "normal" and pending["task_id"] == "contract-task"
    assert not list((state / "pending").glob("*.json"))
    project_state = state / "projects" / pending["project_key"]
    ledger_path = project_state / "sessions" / (hashlib.sha256(b"contract-task").hexdigest() + ".json")
    ledger_record = json.loads(ledger_path.read_text(encoding="utf-8"))["payload"]
    assert ledger_record["last_outcome"] == {
        "status": "AMBIGUOUS_TOOL_FAILURE", "ambiguous_outcome": True,
        "work_performed": "UNKNOWN", "routing_enforced": True,
        "evidence_path": str(files[0]), "claude_review_mode": "FULL",
    }
    assert ledger_record["live_thread"] is None
    leases = json.loads((project_state / "scope_leases.json").read_text(encoding="utf-8"))
    assert leases["owners"][pending["lease_owner"]]["state"] == "RELEASED"
    events = [path.read_text(encoding="utf-8") for path in state.rglob("*.jsonl")]
    assert any('"event_type":"IMPLEMENTER_FAILED"' in contents for contents in events)
    sys.path.insert(0, str(PACKAGE))
    import telemetry_schema
    failed_events = [json.loads(line) for path in state.rglob("*.jsonl")
                     for line in path.read_text(encoding="utf-8").splitlines()
                     if '"event_type":"IMPLEMENTER_FAILED"' in line]
    assert len(failed_events) >= 1 and len({item["event_id"] for item in failed_events}) == 1
    assert all(telemetry_schema.valid_event(item) for item in failed_events)
    assert not any("REVIEWER1_STARTED" in contents for contents in events)


def test_tool_failure_ledger_error_is_visible_and_preserves_pending(repo, process):
    state, run = process
    decision(run("PreToolUse", pre(repo)))
    pending = pending_record(state)
    sessions = state / "projects" / pending["project_key"] / "sessions"
    shutil.rmtree(sessions)
    sessions.write_text("blocked", encoding="utf-8")
    result = run("PostToolUseFailure", event("PostToolUseFailure", repo,
                 error="worker exited", is_interrupt=False, duration_ms=23))
    output = decision(result)["hookSpecificOutput"]["additionalContext"]
    assert "work_performed is UNKNOWN" in output and "FULL Claude review is required" in output
    assert "Evidence cleanup error" in output
    assert len(list((state / "raw_tool_outputs").glob("*-failure.json"))) == 1
    assert len(list((state / "pending").glob("*.json"))) == 1
    events = [path.read_text(encoding="utf-8") for path in state.rglob("*.jsonl")]
    assert any('"event_type":"IMPLEMENTER_FAILED"' in contents for contents in events)


def test_reviewer_unavailable_is_not_a_pass(repo, process):
    state, run = process
    decision(run("PreToolUse", pre(repo)))
    (repo / "changed.py").write_text("".join(f"value_{i} = {i}\n" for i in range(80)), encoding="utf-8")
    reviewed = bundle(run("PostToolUse", event("PostToolUse", repo, tool_response=THREAD),
                          "reviewer-unavailable"))
    assert reviewed["reviewer1_result"] == "REVIEWER1_UNAVAILABLE"
    assert reviewed["reviewer1_result"] != "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
    assert reviewed["claude_review_mode"] == "FULL"
    artifact = json.loads(Path(reviewed["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["reviewer1_result"] == "REVIEWER1_UNAVAILABLE"
    assert artifact["claude_review_mode"] == "FULL"
    assert any("REVIEWER1_UNAVAILABLE" in p.read_text(encoding="utf-8")
               for p in (state / "telemetry").rglob("*.jsonl"))


def test_posttool_internal_error_replaces_output_with_full_bundle(repo, process):
    state, run = process
    decision(run("PreToolUse", pre(repo)))
    pending = pending_record(state)
    result = run("PostToolUse", event("PostToolUse", repo, tool_response=THREAD), "post-error")
    assert result.stderr.replace(b"\r\n", b"\n") == b"orchestrator hook internal error: RuntimeError: injected post failure\n"
    assert result.stdout.endswith(b"\n") and result.stdout.count(b"\n") == 1
    output = bundle(result)
    assert any(finding["code"] == "INTERNAL_POST_ERROR" for finding in output["findings"])
    assert output["claude_review_mode"] == "FULL"
    artifact = json.loads(Path(output["full_bundle_path"]).read_text(encoding="utf-8"))
    assert artifact["claude_review_mode"] == "FULL"
    assert any(finding["code"] == "INTERNAL_POST_ERROR" for finding in artifact["findings"])
    assert "injected post failure" in (state / "diagnostics.jsonl").read_text(encoding="utf-8")
    raw_path = Path(output["raw_output_path"])
    assert json.loads(raw_path.read_text(encoding="utf-8")) == {"structuredContent": THREAD["structuredContent"]}
    assert not list((state / "pending").glob("*.json"))
    leases = json.loads((state / "projects" / pending["project_key"] / "scope_leases.json").read_text(encoding="utf-8"))
    assert leases["owners"][pending["lease_owner"]]["state"] == "RELEASED"
