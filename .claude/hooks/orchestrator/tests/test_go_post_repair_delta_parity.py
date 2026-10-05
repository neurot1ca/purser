import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_repair_delta.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger

CASES = [
    {"name": "repair_in_scope", "repair": [{"path": "scope/file.txt", "content": "repaired\n"}]},
    {"name": "repair_out_of_scope", "repair": [{"path": "outside/file.txt", "content": "repaired\n"}]},
    {"name": "repair_protected_path", "protected": ["secrets/**"], "repair": [{"path": "secrets/key.txt", "content": "repaired\n"}]},
    {"name": "repair_no_change", "repair": []},
    {"name": "repair_delta_failure", "delta_failure": True, "repair": [{"path": "scope/file.txt", "content": "repaired\n"}]},
]


def _git(root, *args):
    return subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _oracle(case, root):
    _git(root, "init", "-q")
    for name, value in {"scope/file.txt": "base\n", "outside/file.txt": "base\n", "secrets/key.txt": "base\n"}.items():
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value, encoding="utf-8", newline="\n")
    _git(root, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], cwd=root, env=env, check=True)
    state = root.parent / (root.name + "-state")
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    config.update(micro_repair_enabled=True, max_total_hook_budget_seconds=240.0, reviewer1_timeout_seconds=1,
                  micro_repair_timeout_seconds=15)
    prompt = "ORCH|task=repair-fixture|phase=implementation|route=normal|reason=new_task|basis=fixture|scope=scope\nrepair test"
    event = {"session_id": "post-repair-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["protected_paths"] = case.get("protected", [])
    codex._atomic_json_write(pending_path, pending)
    (root / "scope" / "file.txt").write_text("initial\n" * 60, encoding="utf-8", newline="\n")
    observed, review_calls, repair_calls = {}, 0, 0
    repair_delta = {}

    def reviewer(**kwargs):
        nonlocal review_calls
        review_calls += 1
        return {"review_status": "COMPLETED", "result": "CHANGES_REQUIRED" if review_calls == 1 else "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
                "validation": "PASS", "findings": [{"code": "FIX", "message": "repair"}] if review_calls == 1 else []}

    original_run, original_compute, original_finalize = codex.subprocess.run, ledger.compute_delta, codex._finalize_replacement
    compute_calls = 0

    def fake_run(argv, **kwargs):
        nonlocal repair_calls
        if argv and argv[0] == "codex":
            repair_calls += 1
            for change in case["repair"]:
                path = root / change["path"]
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(change["content"], encoding="utf-8", newline="\n")
            return subprocess.CompletedProcess(argv, 0, "repaired", "")
        return original_run(argv, **kwargs)

    def compute(*args, **kwargs):
        nonlocal compute_calls
        compute_calls += 1
        if case.get("delta_failure") and compute_calls >= 2:
            value = {"status": ledger.UNKNOWN, "code": "DELTA_INSPECTION_FAILED", "message": "fixture delta failure", "changed_paths": None}
        else:
            value = original_compute(*args, **kwargs)
        if compute_calls >= 2:
            repair_delta.update(value)
        return value

    def capture(bundle, *args, **kwargs):
        observed.update(bundle)
        return original_finalize(bundle, *args, **kwargs)

    codex.subprocess.run, ledger.compute_delta, codex._finalize_replacement = fake_run, compute, capture
    try:
        response = codex.post_tool_use({**event, "hook_event_name": "PostToolUse",
            "tool_response": {"structuredContent": {"threadId": "fixture-thread", "content": "Done"}}},
            config=config, state_root=state, repo_root=root, reviewer=reviewer)
    finally:
        codex.subprocess.run, ledger.compute_delta, codex._finalize_replacement = original_run, original_compute, original_finalize
    text = response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"]
    final = json.loads(text)
    if repair_calls != 1:
        raise AssertionError(f"expected exactly one fake repair subprocess, got {repair_calls}: {final}")
    fields = ("whole_tree_delta", "whole_tree_anomaly", "known_concurrent_scopes", "changed_paths",
              "delta_lines", "delta_files", "current_head", "protected_paths_touched", "concurrent_writer",
              "findings", "force_full_review", "micro_repair", "micro_repair_cleared_review")
    return {"case": case, "baseline": pending["baseline"], "scope": pending["scope"],
            "repair_delta": repair_delta,
            "expected": {**{key: observed.get(key) for key in fields}, "delta_status": repair_delta.get("status")},
            "observed": {key: observed.get(key) for key in fields}}


def test_post_repair_delta_python_fixture_and_go_parity(tmp_path):
    scratch = ROOT / ".tmp" / ("post-repair-delta-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex.post_tool_use post-repair recomputation", "cases": []}
        for case in CASES:
            root = scratch / case["name"]
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_REPAIR_DELTA_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestPostRepairDeltaPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
