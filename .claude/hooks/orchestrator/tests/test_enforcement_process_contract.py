import hashlib
import ast
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

# Each direct/indirect Python call in synchronous pre_tool_use maps to an
# existing process contract or a source-branch exclusion. New callees fail closed.
PRE_CALL_CONTRACTS = {
    name: "C1" for name in "_append_code_map _append_worker_validation_instruction _asks_worker_to_run_full_suite _atomic_json_write _full_suite_request_phrase _baseline_state_fields _bypass_path _call_key _check_snapshot _claude_runs_full_suite_at_acceptance _cleanup_stale_pending _derive_task_type _fallback_question _handoff_matches _implementer_provider _indexed_reply_root _inherited_reply_parse _last_launch_failure _load_config _observe _observed_denial _parse_task_contract _pending_candidates _pending_path _pending_project_key _project_state_root _read_pending _reconcile_rejected_starts _record_thread_root _repo_root _requested_thread _route_duration_percentiles _route_eta_minutes _safe_error _start_job_terminal_or_absent _state_root _telemetry_envelope _thread_index_path _thread_worktree_path".split()
}
PRE_CALL_CONTRACTS["_plan_milestone_label"] = "C1: plan milestone label normalization"
PRE_CALL_CONTRACTS.update({"_deny": "C2", "_internal_pre_allow": "C3", "_record_diagnostic": "C3",
                           "_release_scope_lease": "C3", "_remove_pending": "C3", "_write_pre_bypass": "C3"})
PRE_CALL_EXCLUSIONS = {
    "codex_quota.read_latest_quota": "C1: quota snapshot is optional and failures are explicitly ignored",
    "codex_exec_mcp.probe_api_primary": "EXCLUDED: fallback-only 25-second branch",
}
ASYNC_PRE_CALL_CONTRACTS = {
    "_async_pre_followup": "A1: codex_result/codex_cancel PreToolUse decisions",
    "_async_job": "A1: durable async job identity and result state",
    "_async_owned": "A1: async index, pending call, and session/project ownership",
    "_call_key": "A1: pending call identity binding",
    "_deny": "A1: async PreToolUse denial response",
    "_repo_root": "A1: project ownership resolution for adoption",
    "_state_root": "A1: async state directory resolution",
}
ASYNC_PRE_IMPORTED_CALLS = {
    "hashlib.sha256": "A1: job and pending identity validation",
    "json.loads": "A1: durable index, pending, and job decoding",
    "project_root.project_key": "A1: same-project adoption check",
    "project_root.resolve_project_root": "A1: adoption repository resolution",
    "re.fullmatch": "A1: job id and call hash validation",
    "state_root.default_state_root": "A1: default async state directory",
}
ASYNC_PRE_METHOD_CALLS = {
    "encode": "A1: hash input encoding",
    "get": "A1: event and durable record fields",
    "hexdigest": "A1: stable async identity hash",
    "read_text": "A1: durable index, pending, and job reads",
}
ASYNC_PRE_BUILTIN_CALLS = {
    "AsyncAdoptionRequired": "A1: explicit consent boundary for cross-session adoption",
    "WiringError": "A1: malformed or mismatched ownership state",
    "dict": "A1: reconstructed originating call",
    "isinstance": "A1: input and durable state validation",
    "str": "A1: defensive call hash validation",
}
PRE_IMPORTED_CALLS = {
    name: "C1" for name in "active_coder_state.approve_fallback active_coder_state.read active_coder_state.record_fallback_question active_coder_state.update coder_config.load coder_config.opencode_candidates codex_exec_mcp._load_record codex_exec_mcp._state_dir compaction_advisor.queue_fallback_question datetime.datetime.fromisoformat datetime.datetime.now hashlib.sha256 json.loads json_transport.dumps ledger.ScopeLeases ledger.SessionLedger ledger.build_session_record ledger.capture_baseline math.ceil math.isfinite os.fsync os.replace policy.OrchParseResult policy.fresh_codex_decision policy.parse_orch_prompt policy.resolve_route policy.thread_compatibility policy.validate_config project_profile.load_profile project_root.project_key project_root.resolve_candidate project_root.resolve_project_root re.escape re.fullmatch re.match re.search re.sub state_root.default_state_root telemetry.record_health tempfile.NamedTemporaryFile time.time uuid.uuid5".split()
}
PRE_IMPORTED_CALLS.update({"codex_quota.read_latest_quota": "C1: optional quota read; errors are ignored",
                          "codex_exec_mcp.probe_api_primary": "EXCLUDED: fallback recovery branch can wait 25 seconds"})
PRE_METHOD_CALLS = {name: "C1" for name in "acquire add append associate_turn casefold decode duration_hint encode end endswith exists fileno find find_by_thread_id finditer flush fullmatch get glob group hexdigest intersection is_absolute isoformat isprintable items join load lower mkdir open partition pop read read_text refresh relative_to replace resolve rstrip save search split splitlines start start_active_task startswith stat strip sub timestamp total_seconds unlink update upper write".split()}
PRE_METHOD_CALLS.update({"release": "C3: lease cleanup after failure or undelivered adapter result"})
PRE_BUILTIN_CALLS = {name: "C1" for name in "all any bool dict enumerate float getattr int isinstance len list max next set sorted str tuple type".split()}
PRE_BUILTIN_CALLS.update({"finish_section": "C1: nested task-contract parser helper",
                          "return_to_primary": "C1: nested fallback policy helper",
                          "WiringError": "C1: validation and durable-state failure path"})


def test_sync_pre_call_diff_is_mapped():
    source = (PACKAGE / "codex.py").read_text(encoding="utf-8")
    tree = ast.parse(source)
    functions = {node.name: node for node in tree.body if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    reached, todo = set(), ["pre_tool_use"]
    while todo:
        name = todo.pop()
        if name in reached or name not in functions:
            continue
        reached.add(name)
        for node in ast.walk(functions[name]):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id in functions):
                todo.append(node.func.id)
    calls = {node.func.id for name in reached for node in ast.walk(functions[name])
             if isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id in functions}
    assert calls <= set(PRE_CALL_CONTRACTS) | set(PRE_CALL_EXCLUSIONS) | set(ASYNC_PRE_CALL_CONTRACTS), f"unmapped Python callees: {sorted(calls - set(PRE_CALL_CONTRACTS) - set(PRE_CALL_EXCLUSIONS) - set(ASYNC_PRE_CALL_CONTRACTS))}"
    aliases = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            aliases.update({item.asname or item.name.split(".")[0]: item.name for item in node.names})
        elif isinstance(node, ast.ImportFrom):
            aliases.update({item.asname or item.name: f"{node.module}.{item.name}" for item in node.names})
    imported = {f"{aliases[node.func.value.id]}.{node.func.attr}"
                for name in reached for node in ast.walk(functions[name])
                if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and isinstance(node.func.value, ast.Name) and node.func.value.id in aliases}
    assert imported <= set(PRE_IMPORTED_CALLS) | set(ASYNC_PRE_IMPORTED_CALLS), f"unmapped imported calls: {sorted(imported - set(PRE_IMPORTED_CALLS) - set(ASYNC_PRE_IMPORTED_CALLS))}"
    methods = {node.func.attr for name in reached for node in ast.walk(functions[name])
               if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
               and not (isinstance(node.func.value, ast.Name) and node.func.value.id in aliases)}
    assert methods <= set(PRE_METHOD_CALLS) | set(ASYNC_PRE_METHOD_CALLS), f"unmapped method calls: {sorted(methods - set(PRE_METHOD_CALLS) - set(ASYNC_PRE_METHOD_CALLS))}"
    bare = {node.func.id for name in reached for node in ast.walk(functions[name])
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
            and node.func.id not in functions and node.func.id not in aliases}
    assert bare <= set(PRE_BUILTIN_CALLS) | set(ASYNC_PRE_BUILTIN_CALLS), f"unmapped builtin/nested calls: {sorted(bare - set(PRE_BUILTIN_CALLS) - set(ASYNC_PRE_BUILTIN_CALLS))}"


def test_async_pre_call_diff_is_mapped():
    source = (PACKAGE / "codex.py").read_text(encoding="utf-8")
    tree = ast.parse(source)
    functions = {node.name: node for node in tree.body if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    reached, todo = set(), ["_async_owned", "_async_job", "_async_pre_followup"]
    while todo:
        name = todo.pop()
        if name in reached or name not in functions:
            continue
        reached.add(name)
        todo.extend(node.func.id for node in ast.walk(functions[name])
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id in functions)
    calls = [node for name in reached for node in ast.walk(functions[name]) if isinstance(node, ast.Call)]
    internal = {node.func.id for node in calls if isinstance(node.func, ast.Name) and node.func.id in functions}
    assert internal <= set(ASYNC_PRE_CALL_CONTRACTS), f"unmapped async Python callees: {sorted(internal - set(ASYNC_PRE_CALL_CONTRACTS))}"
    aliases = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            aliases.update({item.asname or item.name.split(".")[0]: item.name for item in node.names})
        elif isinstance(node, ast.ImportFrom):
            aliases.update({item.asname or item.name: f"{node.module}.{item.name}" for item in node.names})
    imported = {f"{aliases[node.func.value.id]}.{node.func.attr}" for node in calls
                if isinstance(node.func, ast.Attribute) and isinstance(node.func.value, ast.Name)
                and node.func.value.id in aliases}
    assert imported <= set(ASYNC_PRE_IMPORTED_CALLS), f"unmapped async imported calls: {sorted(imported - set(ASYNC_PRE_IMPORTED_CALLS))}"
    methods = {node.func.attr for node in calls if isinstance(node.func, ast.Attribute)
               and not (isinstance(node.func.value, ast.Name) and node.func.value.id in aliases)}
    assert methods <= set(ASYNC_PRE_METHOD_CALLS), f"unmapped async method calls: {sorted(methods - set(ASYNC_PRE_METHOD_CALLS))}"
    bare = {node.func.id for node in calls if isinstance(node.func, ast.Name)
            and node.func.id not in functions and node.func.id not in aliases}
    assert bare <= set(ASYNC_PRE_BUILTIN_CALLS), f"unmapped async builtin/nested calls: {sorted(bare - set(ASYNC_PRE_BUILTIN_CALLS))}"


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


@pytest.fixture
def go_pre_binary(tmp_path):
    binary = tmp_path / ("purser-hook.exe" if os.name == "nt" else "purser-hook")
    cache = REPO_ROOT / ".tmp" / "m3-p68-go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache)}
    result = subprocess.run(["go", "build", "-tags", "contracttest", "-o", str(binary), "./cmd/purser-hook"],
                            cwd=REPO_ROOT / "go", env=env, capture_output=True, timeout=120)
    assert result.returncode == 0, result.stderr.decode("utf-8", errors="replace")
    return binary


def run_go_pre(binary, state, repo, payload, seam=None):
    env = os.environ.copy()
    env.update(PURSER_STATE_DIR=str(state), CLAUDE_PLUGIN_ROOT=str(REPO_ROOT),
               HOME=str(state.parent / "home"), USERPROFILE=str(state.parent / "home"),
               CODEX_HOME=str(state.parent / "codex"))
    if seam:
        env["PURSER_CONTRACT_SEAM"] = seam
    else:
        env.pop("PURSER_CONTRACT_SEAM", None)
    (state.parent / "home").mkdir(exist_ok=True)
    return subprocess.run([str(binary), "enforce"], input=json.dumps(payload).encode(),
                          capture_output=True, cwd=repo, env=env, timeout=45)


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


@pytest.mark.parametrize("contract,consumer", [("C1", "PostToolUse"), ("C3", "PostToolUse"),
                                                ("C4", "PostToolUseFailure")])
def test_mixed_go_pre_python_consumers(repo, process, go_pre_binary, contract, consumer, monkeypatch):
    state, run = process
    call = pre(repo)
    started = run_go_pre(go_pre_binary, state, repo, call)
    output = decision(started)["hookSpecificOutput"]
    assert "updatedInput" in output
    record = pending_record(state)
    assert record["baseline"]["status"] == "OK"
    if contract == "C1":
        python_state = state.parent / "python-reference"
        isolated_home = state.parent / "home"
        monkeypatch.setenv("HOME", str(isolated_home))
        monkeypatch.setenv("USERPROFILE", str(isolated_home))
        python_result = run_contract_hook("PreToolUse", call, python_state, entrypoint="enforcement")
        python_record = pending_record(python_state)
        assert "updatedInput" in decision(python_result)["hookSpecificOutput"]
        assert record["baseline"] == python_record["baseline"]
        for key in ("kind", "task_id", "route", "model", "effort", "scope", "repo_root", "project_key"):
            assert record[key] == python_record[key], key
        go_leases = json.loads((state / "projects" / record["project_key"] / "scope_leases.json").read_text())
        py_leases = json.loads((python_state / "projects" / record["project_key"] / "scope_leases.json").read_text())
        assert {k: {x: v[x] for x in ("task_id", "scope", "state")} for k, v in go_leases["owners"].items()} == {
            k: {x: v[x] for x in ("task_id", "scope", "state")} for k, v in py_leases["owners"].items()}
    if contract != "C1":
        marker_event = dict(call, hook_event_name="WriteUndeliveredMarker", reason="adapter could not deliver rewritten input")
        marked = subprocess.run([str(go_pre_binary)], input=json.dumps(marker_event).encode(), capture_output=True,
                                cwd=repo, env={**os.environ, "PURSER_STATE_DIR": str(state)}, timeout=45)
        assert marked.returncode == 0 and json.loads(marked.stdout)["written"] is True
    if consumer == "PostToolUse":
        (repo / "changed.py").write_text("".join(f"value_{i} = {i}\n" for i in range(80)), encoding="utf-8")
        result = run(consumer, event(consumer, repo, tool_response=THREAD), "reviewer-ok")
        reviewed = bundle(result)
        assert reviewed["claude_review_mode"] == "FULL"
        if contract == "C1":
            assert isinstance(reviewed["changed_paths_sha256"], str) and re.fullmatch(r"[0-9a-f]{64}", reviewed["changed_paths_sha256"])
        if contract == "C3":
            assert reviewed["routing_enforced"] is False
            assert any(item["code"] == "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED" for item in reviewed["findings"])
    else:
        result = run(consumer, event(consumer, repo, error="adapter delivery failed", is_interrupt=False,
                                     duration_ms=1))
        message = decision(result)["hookSpecificOutput"]["additionalContext"]
        assert "work_performed is UNKNOWN" in message and "FULL Claude review is required" in message
        evidence = json.loads(next((state / "raw_tool_outputs").glob("*-failure.json")).read_text(encoding="utf-8"))
        assert evidence["pending_call"]["kind"] == "pretool_bypass"
        assert not list((state / "pending").glob("undelivered-*.json"))
    assert not list((state / "pending").glob("*.json"))
    leases = json.loads((state / "projects" / record["project_key"] / "scope_leases.json").read_text())
    assert leases["owners"][record["lease_owner"]]["state"] == "RELEASED"
    stopped = run("Stop", {"hook_event_name": "Stop", "session_id": "process-contract",
                            "cwd": str(repo), "stop_hook_active": False})
    assert stopped.returncode == 0


def test_mixed_go_pre_policy_deny_keeps_go_lifecycle_stop(repo, process, go_pre_binary):
    state, run = process
    result = run_go_pre(go_pre_binary, state, repo, pre(repo, prompt="missing routing contract"))
    denied = decision(result)["hookSpecificOutput"]
    assert denied["permissionDecision"] == "deny"
    assert not list(state.glob("pending/*.json"))
    hooks = json.loads((REPO_ROOT / "hooks" / "hooks.json").read_text(encoding="utf-8"))
    assert hooks == {"modules": ["./register.ts"]}  # Stop remains a native module lifecycle hook.
    register = (REPO_ROOT / "hooks" / "register.ts").read_text(encoding="utf-8")
    assert "on('classic.Stop', ($, e, next) => lifecycle($, e, next, 'Stop'))" in register


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

    adapter = (REPO_ROOT / "hooks" / "mod" / "enforcement.ts").read_text(encoding="utf-8")
    go_source = (REPO_ROOT / "go" / "internal" / "enforce" / "pre.go").read_text(encoding="utf-8")
    go_map = go_source.split("var codexTools = map[string]bool{", 1)[1].split("}", 1)[0]
    go_names = set(re.findall(r'"(mcp__[^" ]+)"\s*:', go_map))
    hooked_names = set()
    for name in ("syncTools", "startTools", "followupTools"):
        tool_set = re.search(rf"const {name} = new Set\(\[([\s\S]*?)\]\)", adapter)
        assert tool_set, name
        hooked_names.update(re.findall(r"'([^']+)'", tool_set.group(1)))
    assert hooked_names <= codex.CODEX_TOOLS
    assert hooked_names <= go_names


def test_go_post_mod_routes_success_and_failure_without_python_command_hooks():
    adapter = (REPO_ROOT / "hooks" / "mod" / "enforcement.ts").read_text(encoding="utf-8")
    assert 'on("classic.PostToolUse", ($, e, next) => handlePostToolUse(\'PostToolUse\', $, e, next))' in adapter
    assert 'on("classic.PostToolUseFailure", ($, e, next) => handlePostToolUse(\'PostToolUseFailure\', $, e, next))' in adapter
    assert "event === 'PostToolUse' && !postToolUseTools.has(e.tool_name)" in adapter
    assert "PURSER_MOD_POST" not in adapter
    assert "event === 'PostToolUseFailure' && !postToolUseFailureTools.has(e.tool_name)" in adapter
    post_tools = re.search(r"const postToolUseTools = new Set\(\[([\s\S]*?)\]\)", adapter)
    assert post_tools
    assert set(re.findall(r"'([^']+)'", post_tools.group(1))) == {
        "mcp__plugin_purser_codex-main__codex",
        "mcp__plugin_purser_codex-main__codex-reply",
        "mcp__plugin_purser_codex-main__codex_start",
        "mcp__plugin_purser_codex-main__codex_result",
        "mcp__plugin_purser_codex-main__codex_cancel",
        "mcp__plugin_purser_codex-main__codex_apply",
    }
    failure_tools = re.search(r"const postToolUseFailureTools = new Set\(\[([\s\S]*?)\]\)", adapter)
    assert failure_tools
    assert set(re.findall(r"'([^']+)'", failure_tools.group(1))) == {
        "mcp__plugin_purser_codex-main__codex",
        "mcp__plugin_purser_codex-main__codex-reply",
        "mcp__plugin_purser_codex-main__codex_start",
        "mcp__plugin_purser_codex-main__codex_result",
        "mcp__plugin_purser_codex-main__codex_cancel",
    }
    hooks = json.loads((REPO_ROOT / "hooks" / "hooks.json").read_text(encoding="utf-8"))
    assert hooks == {"modules": ["./register.ts"]}  # Post dispatch is routed by the module.


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
