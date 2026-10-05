"""Stop migration map and subprocess oracle for registered Stop hooks."""
import ast
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from contract_hook_runner import run_contract_hook
from install_claude_task_telemetry import CONTEXT_IO_HOOK_SPECS, HOOK_SPECS

PACKAGE = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).parents[4]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "stop_process.json"
RETRY_FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "stop_retry.json"
sys.path.insert(0, str(PACKAGE))
import compaction_advisor

STOP_CALL_CONTRACTS = {
    "_observe": "PENDING:130",
    "_state_root": "PENDING:130",
    "claude_transcript_usage.finalize_stop_task": "PENDING:128",
    "state_root.default_state_root": "PENDING:130",
    "telemetry.record_health": "PENDING:130",
    "getattr": "PENDING:128",
    "get": "PENDING:128",
    "finalize_stop_task": "PENDING:128",
}


def test_stop_call_diff_is_mapped_and_targets_exist():
    tree = ast.parse((PACKAGE / "codex.py").read_text(encoding="utf-8"))
    functions = {node.name: node for node in tree.body if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
    reached, todo = set(), ["stop"]
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
    assert internal <= set(STOP_CALL_CONTRACTS), f"unmapped Python callees: {sorted(internal - set(STOP_CALL_CONTRACTS))}"
    aliases = {}
    for node in tree.body:
        if isinstance(node, ast.Import):
            aliases.update({item.asname or item.name.split(".")[0]: item.name for item in node.names})
        elif isinstance(node, ast.ImportFrom):
            aliases.update({item.asname or item.name: f"{node.module}.{item.name}" for item in node.names})
    imported = {f"{aliases[node.func.value.id]}.{node.func.attr}" for node in calls
                if isinstance(node.func, ast.Attribute) and isinstance(node.func.value, ast.Name)
                and node.func.value.id in aliases}
    imported.add("claude_transcript_usage.finalize_stop_task")  # dynamically imported by stop
    assert imported <= set(STOP_CALL_CONTRACTS), f"unmapped imported calls: {sorted(imported - set(STOP_CALL_CONTRACTS))}"
    methods = {node.func.attr for node in calls if isinstance(node.func, ast.Attribute)
               and not (isinstance(node.func.value, ast.Name) and node.func.value.id in aliases)}
    assert methods <= set(STOP_CALL_CONTRACTS), f"unmapped methods: {sorted(methods - set(STOP_CALL_CONTRACTS))}"
    bare = {node.func.id for node in calls if isinstance(node.func, ast.Name)
            and node.func.id not in functions and node.func.id not in aliases}
    assert bare <= set(STOP_CALL_CONTRACTS), f"unmapped builtins: {sorted(bare - set(STOP_CALL_CONTRACTS))}"

    source = (PACKAGE / "codex.py").read_text(encoding="utf-8")
    for target in STOP_CALL_CONTRACTS.values():
        if target.startswith("PENDING:"):
            assert 125 <= int(target.split(":", 1)[1]) <= 131, target
            continue
        path, symbol = re.fullmatch(r"(go/[^.]+(?:/[^.]+)*)\.([A-Za-z_][A-Za-z_0-9]*) \(([^)]+)\)", target).groups()
        go_file = REPO_ROOT / path / "*.go"
        matches = [candidate for candidate in go_file.parent.glob(go_file.name)
                   if re.search(rf"\bfunc(?:\s+\([^)]*\)\s+)?{re.escape(symbol)}\s*\(", candidate.read_text(encoding="utf-8"))]
        assert matches, f"missing Go symbol: {target}"


def _project_key(repo):
    return hashlib.sha256(os.path.normcase(str(repo.resolve())).encode("utf-8")).hexdigest()[:16]


def _advisor_path(state, repo):
    return state / "projects" / _project_key(repo) / "telemetry" / "compaction_advisor.json"


def _context_and_codex_stop(repo, state, payload):
    event = {"hook_event_name": "Stop", "cwd": str(repo), **payload}
    codex = run_contract_hook("Stop", event, state, entrypoint="enforcement")
    context = run_contract_hook("Stop", event, state, entrypoint="context")
    assert context.returncode == codex.returncode == 0
    return context, codex


def _records(*items):
    return "".join(json.dumps(item, ensure_ascii=False) + "\n" for item in items)


def test_python_registered_stop_process_oracles(tmp_path):
    assert any(name == "Stop" for name, _matcher, _timeout in HOOK_SPECS)
    assert any(name == "Stop" for name, _matcher, _timeout in CONTEXT_IO_HOOK_SPECS)
    repo = tmp_path / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q", str(repo)], check=True, timeout=10)
    state = tmp_path / "state"
    cases = []

    def run_case(name, session, *, transcript=None, setup=None):
        case_state = state / name
        if setup:
            setup(case_state, session)
        payload = {"session_id": session, "stop_hook_active": False,
                   "last_assistant_message": "Done."}
        if transcript:
            payload["transcript_path"] = str(transcript)
        context, codex = _context_and_codex_stop(repo, case_state, payload)
        context_out = context.stdout.decode("utf-8")
        decision = json.loads(context_out) if context_out else {}
        advisor = _advisor_path(case_state, repo)
        advisor_state = json.loads(advisor.read_text(encoding="utf-8")) if advisor.exists() else {}
        cases.append({"name": name, "context_decision": decision.get("decision", "allow"),
                      "context_reason": decision.get("reason", ""),
                      "retry_count": advisor_state.get("codex_retry_count", 0),
                      "retry_pending": advisor_state.get("codex_retry_pending", False),
                      "codex_stop_output": json.loads(codex.stdout or b"{}")})

    run_case("no_pending_work", "stop-clear")

    job_id = "a" * 64
    def finished_job(case_state, session):
        jobdir = case_state / "codex_exec_mcp" / "jobs"
        jobdir.mkdir(parents=True)
        call = "codex_start call"
        (jobdir / f"{job_id}.json").write_text(json.dumps({"jobId": job_id, "call": call,
            "state": "SUCCEEDED", "label": "Build result", "repoRoot": str(repo)}), encoding="utf-8")
        indexdir = case_state / "async_jobs"
        indexdir.mkdir(parents=True)
        (indexdir / f"{job_id}.json").write_text(json.dumps({"jobId": job_id,
            "call": call, "project_key": _project_key(repo), "stage": "FINISHED", "repoRoot": str(repo)}), encoding="utf-8")
    transcript = tmp_path / "finished.jsonl"
    transcript.write_text(_records(
        {"type": "user", "message": {"content": "Continue the task."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "start-1",
            "name": "mcp__plugin_purser_codex-main__codex_start", "input": {}}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "start-1",
            "content": json.dumps({"jobId": job_id})}]}}), encoding="utf-8")
    run_case("finished_uncollected_job", "stop-finished", transcript=transcript, setup=finished_job)

    retry_transcript = tmp_path / "denied.jsonl"
    retry_transcript.write_text(_records(
        {"type": "user", "message": {"content": "Try Codex."}},
        {"type": "assistant", "message": {"content": [{"type": "tool_use", "id": "deny-1",
            "name": "mcp__plugin_purser_codex-main__codex_start", "input": {}}]}},
        {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "deny-1",
            "is_error": True, "content": "classifier denied permission"}]}}), encoding="utf-8")
    def retry_bound(case_state, session):
        advisor = _advisor_path(case_state, repo)
        advisor.parent.mkdir(parents=True, exist_ok=True)
        advisor.write_text(json.dumps({"session_id": session, "codex_retry_count": 2}), encoding="utf-8")
    run_case("retry_bound_reached", "stop-retry-bound", transcript=retry_transcript, setup=retry_bound)

    run_case("classifier_denied_call", "stop-denied", transcript=retry_transcript)

    def completed_task(case_state, session):
        active = case_state / "active_tasks" / f"{session}.json"
        active.parent.mkdir(parents=True, exist_ok=True)
        active.write_text(json.dumps({"session_id": session, "task_id": "completed-task", "completed": True}), encoding="utf-8")
    run_case("completed_task_finalization", "stop-completed", setup=completed_task)

    run_case("registered_codex_and_context_stop", "stop-composition")

    expected = {"oracle": "Python registered Stop hook process behavior", "cases": cases}
    if os.environ.get("STOP_PROCESS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set STOP_PROCESS_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == expected


def _stop_retry_cases():
    job_id = "a" * 64

    def transcript(result=None, *, tool="codex_start", error=False):
        records = [{"type": "user", "message": {"content": "Continue."}}]
        blocks = [{"type": "tool_use", "id": "call-1",
                   "name": f"mcp__plugin_purser_codex-main__{tool}", "input": {}}]
        records.append({"type": "assistant", "message": {"content": blocks}})
        if result is not None:
            block = {"type": "tool_result", "tool_use_id": "call-1", "content": result}
            if error:
                block["is_error"] = True
            records.append({"type": "user", "message": {"content": [block]}})
        return records

    job = {"jobId": job_id, "call": "call-1", "state": "SUCCEEDED",
           "label": "Build result"}
    index = {"jobId": job_id, "call": "call-1", "project_key": "__PROJECT_KEY__",
             "stage": "FINISHED"}
    finished = transcript(json.dumps({"jobId": job_id}))
    denied = transcript("classifier denied permission", error=True)
    return [
        {"name": "none_pending", "records": transcript(), "index": None, "job_raw": None},
        {"name": "finished_uncollected_job", "records": finished, "index": index,
         "job_raw": json.dumps(job)},
        {"name": "another_project_ignored", "records": finished,
         "index": {**index, "project_key": "another-project"}, "job_raw": json.dumps(job)},
        {"name": "collected_job_ignored", "records": finished,
         "index": {**index, "stage": "CONSUMED"}, "job_raw": json.dumps(job)},
        {"name": "classifier_denied_call", "records": denied, "index": None, "job_raw": None},
        {"name": "malformed_job_record", "records": finished, "index": index, "job_raw": "{"},
    ]


def test_python_stop_retry_oracle_fixture(tmp_path, monkeypatch):
    cases = []
    for case in _stop_retry_cases():
        repo = tmp_path / case["name"]
        repo.mkdir()
        root = tmp_path / (case["name"] + "-state")
        monkeypatch.setattr(compaction_advisor.state_root, "default_state_root", lambda root=root: root)
        index = dict(case["index"]) if case["index"] else None
        if index is not None and index.get("project_key") == "__PROJECT_KEY__":
            index["project_key"] = compaction_advisor.project_root.project_key(repo)
        job_raw = case["job_raw"]
        if index is not None:
            index_dir = root / "async_jobs"
            index_dir.mkdir(parents=True)
            (index_dir / f'{index["jobId"]}.json').write_text(json.dumps(index), encoding="utf-8")
            job_dir = root / "codex_exec_mcp" / "jobs"
            job_dir.mkdir(parents=True)
            (job_dir / f'{index["jobId"]}.json').write_text(job_raw, encoding="utf-8")
        item = {**case, "expected": compaction_advisor._codex_retry_reason(case["records"], 0, repo) or ""}
        cases.append(item)
    expected = {"oracle": "compaction_advisor._codex_retry_reason", "cases": cases}
    if os.environ.get("STOP_RETRY_WRITE_FIXTURE") == "1":
        RETRY_FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        RETRY_FIXTURE.write_text(json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n",
                                 encoding="utf-8")
    assert RETRY_FIXTURE.exists(), f"missing fixture: {RETRY_FIXTURE} (set STOP_RETRY_WRITE_FIXTURE=1 to capture)"
    saved = json.loads(RETRY_FIXTURE.read_text(encoding="utf-8"))
    assert {key: value for key, value in saved.items() if key != "state_write"} == expected


def test_python_stop_retry_state_write_fixture(tmp_path, monkeypatch):
    state_cases = []
    retry_cases = _stop_retry_cases()
    denial = next(case for case in retry_cases if case["name"] == "classifier_denied_call")
    finished = next(case for case in retry_cases if case["name"] == "finished_uncollected_job")
    finished = {**finished, "job_raw": json.dumps({"jobId": "a" * 64, "call": "call-1",
                                                    "state": "SUCCEEDED", "label": "a" * 64})}
    scenarios = [
        ("first_retry", denial, 0, False),
        ("second_retry", denial, 1, False),
        ("third_retry_job_id", finished, 2, False),
        ("malformed_state", denial, "invalid", ["bad"]),
    ]
    for name, case, initial_count, initial_pending in scenarios:
        repository = tmp_path / name
        repository.mkdir()
        root = tmp_path / (name + "-state")
        session = "stop-retry-" + name
        advisor = root / "projects" / _project_key(repository) / "telemetry" / "compaction_advisor.json"
        advisor.parent.mkdir(parents=True)
        advisor.write_text(json.dumps({"session_id": session, "codex_retry_count": initial_count,
                                       "codex_retry_pending": initial_pending}), encoding="utf-8")
        monkeypatch.setattr(compaction_advisor, "_state_directory", lambda _repo, path=advisor.parent: path)
        monkeypatch.setattr(compaction_advisor.state_root, "default_state_root", lambda root=root: root)
        index = case["index"]
        if index:
            index = dict(index)
            index["project_key"] = _project_key(repository)
            index_dir = root / "async_jobs"
            index_dir.mkdir(parents=True)
            (index_dir / f'{index["jobId"]}.json').write_text(json.dumps(index), encoding="utf-8")
            job_path = root / "codex_exec_mcp" / "jobs" / f'{index["jobId"]}.json'
            job_path.parent.mkdir(parents=True)
            job_path.write_text(case["job_raw"], encoding="utf-8")
        transcript = tmp_path / (name + ".jsonl")
        transcript.write_text(_records(*case["records"]), encoding="utf-8")
        result = compaction_advisor.stop({"session_id": session, "transcript_path": str(transcript)}, repository)
        saved = json.loads(advisor.read_text(encoding="utf-8"))
        fixture_index = {**index, "project_key": "__PROJECT_KEY__"} if index else None
        state_cases.append({"name": name, "records": case["records"], "index": fixture_index,
                            "job_raw": case["job_raw"], "initial_count": initial_count,
                            "initial_pending": initial_pending,
                            "expected_block": result is not None and result.get("decision") == "block",
                            "expected_count": saved["codex_retry_count"],
                            "expected_pending": saved["codex_retry_pending"]})

    prompt_state = {"session_id": "prompt-retry", "codex_retry_count": 2,
                    "codex_retry_pending": "Retry with job."}
    monkeypatch.setattr(compaction_advisor, "_state_directory", lambda _repo: tmp_path)
    (tmp_path / "compaction_advisor.json").write_text(json.dumps(prompt_state), encoding="utf-8")
    prompt_text = compaction_advisor.prompt_retry_context({"session_id": "prompt-retry"}, tmp_path)
    prompt_saved = json.loads((tmp_path / "compaction_advisor.json").read_text(encoding="utf-8"))
    expected = {"oracle": "compaction_advisor.stop retry state and prompt_retry_context",
                "cases": state_cases, "prompt": {"context": prompt_text,
                                                   "count": prompt_saved["codex_retry_count"],
                                                   "pending": prompt_saved["codex_retry_pending"],
                                                   "input_pending": prompt_state["codex_retry_pending"]}}
    fixture = json.loads(RETRY_FIXTURE.read_text(encoding="utf-8"))
    fixture["state_write"] = expected
    if os.environ.get("STOP_RETRY_STATE_WRITE_FIXTURE") == "1":
        RETRY_FIXTURE.write_text(json.dumps(fixture, sort_keys=True, separators=(",", ":")) + "\n",
                                 encoding="utf-8")
    else:
        assert json.loads(RETRY_FIXTURE.read_text(encoding="utf-8")).get("state_write") == expected
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["GOCACHE"] = str(REPO_ROOT / ".tmp" / "go-build-cache")
    result = subprocess.run(
        ["go", "test", "./internal/contextio/", "-run", "^TestPythonStopRetryGoldenParity$", "-count=1"],
        cwd=REPO_ROOT / "go", text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
