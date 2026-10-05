import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_delta.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger


CASES = [
    {"name": "sync_in_scope", "phase": "implementation", "scope": ["scope"], "changes": [{"path": "scope/file.txt", "content": "changed\n"}]},
    {"name": "sync_out_of_scope", "phase": "implementation", "scope": ["scope"], "changes": [{"path": "outside/file.txt", "content": "changed\n"}]},
    {"name": "protected_path", "phase": "implementation", "scope": ["secrets"], "protected_paths": ["secrets/**"], "changes": [{"path": "secrets/key.txt", "content": "changed\n"}]},
    {"name": "known_concurrent_scope", "phase": "implementation", "scope": ["scope"], "owners": [{"task_id": "foreign-task", "scope": ["outside"]}], "changes": [{"path": "outside/file.txt", "content": "changed\n"}]},
    {"name": "plan_phase_repo_mutation", "phase": "plan", "scope": ["scope"], "changes": [{"path": "scope/file.txt", "content": "changed\n"}]},
    {"name": "worktree_job_delta", "phase": "implementation", "scope": ["scope"], "worktree": True, "changes": [{"path": "scope/file.txt", "content": "changed\n"}]},
    {"name": "worktree_filter_git_failure", "phase": "implementation", "scope": ["scope"], "worktree": True, "filter_failure": True, "changes": [{"path": "scope/file.txt", "content": "changed\n"}]},
]


def _git(root, *args, check=True):
    return subprocess.run(["git", *args], cwd=root, check=check, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _oracle(case, root):
    case = {**case, "lease_owner": "fixture-owner"}
    _git(root, "init", "-q")
    for name, content in {"scope/file.txt": "base\n", "outside/file.txt": "base\n", "secrets/key.txt": "base\n"}.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8", newline="\n")
    _git(root, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], cwd=root, env=env, check=True)
    snapshot = _git(root, "rev-parse", "HEAD^{tree}").stdout.decode().strip()

    state = root.parent / (root.name + "-state")
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    route = "plan_standard" if case["phase"] == "plan" else "normal"
    prompt = f"ORCH|task=fixture-task|phase={case['phase']}|route={route}|reason=new_task|basis=fixture scope|scope={','.join(case['scope'])}\nInspect the fixture."
    event = {"session_id": "post-delta-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    pre = codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    assert pre.get("hookSpecificOutput", {}).get("permissionDecision") != "deny", case["name"]
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    if case.get("protected_paths") is not None:
        pending["protected_paths"] = case["protected_paths"]
    if case.get("worktree"):
        pending["worktree_path"], pending["worktree_snapshot"] = str(root), snapshot
    codex._atomic_json_write(pending_path, pending)
    baseline = pending["baseline"]
    for change in case["changes"]:
        path = root / change["path"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(change["content"], encoding="utf-8", newline="\n")
    post_event = {**event, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {"threadId": "fixture-thread", "content": "Done."}}}
    observed = {}
    original_finalize = codex._finalize_replacement

    def capture_bundle(bundle, *args, **kwargs):
        observed.update(bundle)
        return original_finalize(bundle, *args, **kwargs)

    original_owners = ledger.ScopeLeases.overlapping_owners
    original_filter = ledger._filter_job_paths
    ledger.ScopeLeases.overlapping_owners = lambda self, owner: case.get("owners", [])
    if case.get("filter_failure"):
        ledger._filter_job_paths = lambda *_: (_ for _ in ()).throw(ledger._InspectionError("GIT_FAILED", "Git could not filter ignored job patch paths."))
    codex._finalize_replacement = capture_bundle
    try:
        response = codex.post_tool_use(post_event, config=config, state_root=state, repo_root=root,
                                       reviewer=lambda **_: {"review_status": "UNAVAILABLE", "result": "REVIEWER1_UNAVAILABLE", "validation": "UNKNOWN", "findings": []})
    finally:
        codex._finalize_replacement = original_finalize
        ledger.ScopeLeases.overlapping_owners = original_owners
        ledger._filter_job_paths = original_filter
    output = response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"]
    final_bundle = json.loads(output)
    if case.get("filter_failure"):
        final_bundle["raw_output_path"] = "<RAW_OUTPUT>"
        if final_bundle.get("full_bundle_path"):
            final_bundle["full_bundle_path"] = "<FULL_BUNDLE>"
        return {"case": case, "baseline": baseline, "snapshot": snapshot, "post_tool_use_result": final_bundle}
    relevant = {"PLAN_MUTATED_REPO", "EXTERNAL_REPOSITORY_CHANGE", "OUT_OF_SCOPE_MODIFICATION", "LEASE_HISTORY_UNAVAILABLE", "DELTA_INSPECTION_FAILED", "JOB_DELTA_INSPECTION_FAILED"}
    expected = {key: observed.get(key) for key in ("whole_tree_anomaly", "whole_tree_delta", "known_concurrent_scopes", "protected_paths_touched", "concurrent_writer")}
    expected.update({key: final_bundle.get(key) for key in ("changed_paths", "changed_path_count", "delta_lines", "delta_files")})
    expected["findings"] = [item for item in observed.get("findings", []) if item.get("code") in relevant]
    return {"case": case, "baseline": baseline, "snapshot": snapshot, "expected": expected}


def test_post_delta_python_fixture_and_go_parity(tmp_path):
    scratch = ROOT / ".tmp" / ("post-delta-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex post_tool_use delta selection using ledger.py", "cases": []}
        for index, case in enumerate(CASES):
            root = scratch / str(index)
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_DELTA_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestAssemblePostDeltaPythonFixture$", "-count=1", "./internal/enforce"], cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
