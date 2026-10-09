import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_review.json"
sys.path.insert(0, str(ORCH))
import codex


CASES = [
    {"name": "no_delta", "phase": "implementation", "lines": 0, "files": 0, "checks": []},
    {"name": "plan_phase", "phase": "plan", "lines": 1, "files": 1, "checks": []},
    {"name": "operation_phase", "phase": "operation", "lines": 1, "files": 1, "checks": []},
    {"name": "small_delta_audit", "phase": "implementation", "lines": 1, "files": 1, "checks": []},
    {"name": "large_delta_full", "phase": "implementation", "lines": 60, "files": 3, "checks": []},
    {"name": "protected_path_full", "phase": "implementation", "lines": 1, "files": 1, "protected": True, "checks": []},
    {"name": "out_of_scope_full", "phase": "implementation", "lines": 1, "files": 1, "outside": True, "checks": []},
    {"name": "failing_check", "phase": "implementation", "lines": 1, "files": 1,
     "checks": [{"name": "fail", "argv": [sys.executable, "-c", "raise SystemExit(7)"], "timeout_seconds": 10}]},
    {"name": "check_mutation_detected", "phase": "implementation", "lines": 60, "files": 3,
     "checks": [{"name": "mutate", "argv": [sys.executable, "-c", "from pathlib import Path; Path('scope/file-0.txt').write_text('changed by check\\n')"], "timeout_seconds": 10}]},
    {"name": "ungrounded_contract", "phase": "implementation", "lines": 1, "files": 1, "grounded": False, "checks": []},
]


def _git(root, *args):
    return subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _oracle(case, root):
    _git(root, "init", "-q")
    for index in range(3):
        path = root / "scope" / f"file-{index}.txt"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("base\n", encoding="utf-8", newline="\n")
    outside = root / "outside" / "file-0.txt"
    outside.parent.mkdir(parents=True, exist_ok=True)
    outside.write_text("base\n", encoding="utf-8", newline="\n")
    _git(root, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], cwd=root, env=env, check=True)
    state = root.parent / (root.name + "-state")
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    # Pin Reviewer-1 selection independently of user configuration.
    config["readiness_check"] = {"agent": "agy", "model": "gemini-3.8-flash-high"}
    prompt = f"ORCH|task=fixture-task|phase={case['phase']}|route={'plan_standard' if case['phase'] == 'plan' else 'normal'}|reason=new_task|basis=fixture|scope=scope\nObjective: inspect this fixture. Acceptance criteria: preserve the declared scope."
    event = {"session_id": "post-review-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    pre = codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    assert pre.get("hookSpecificOutput", {}).get("permissionDecision") != "deny", case["name"]
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    checks = [{**check, "when": check.get("when")} for check in case["checks"]]
    fixture_checks = [{**check, "argv": ["__PYTHON__", *check["argv"][1:]]}
                      for check in case["checks"]]
    pending.update({"task_contract_grounded": case.get("grounded", True), "force_full_review": case.get("protected", False),
                    "profile_checks": checks, "skip_max_lines": 50, "skip_max_files": 2})
    if case.get("protected"):
        pending["protected_paths"] = ["scope/**"]
    codex._atomic_json_write(pending_path, pending)
    for index in range(case["files"]):
        path = root / ("outside" if case.get("outside") else "scope") / f"file-{index}.txt"
        path.write_text("x\n" * max(1, case["lines"] // max(1, case["files"])), encoding="utf-8", newline="\n")
    post_event = {**event, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {"threadId": "fixture-thread", "content": "VALIDATION: {\"source\":\"WORKER_REPORTED\",\"status\":\"PASS\",\"summary\":\"focused fixture checks\",\"commands\":[\"fixture\"],\"exit_codes\":[0],\"artifacts\":[]}"}}}
    observed = {}
    original_finalize = codex._finalize_replacement

    def capture_bundle(bundle, *args, **kwargs):
        observed.update(bundle)
        return original_finalize(bundle, *args, **kwargs)

    codex._finalize_replacement = capture_bundle
    try:
        response = codex.post_tool_use(post_event, config=config, state_root=state, repo_root=root,
            reviewer=lambda **_: {"result": "CHANGES_REQUIRED", "validation": "PASS", "findings": []})
    finally:
        codex._finalize_replacement = original_finalize
    result = json.loads(response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"])
    paths = [f"{'outside' if case.get('outside') else 'scope'}/file-{index}.txt" for index in range(case["files"])]
    delta = {"status": "OK", "changed_paths": paths,
        "changed_file_count": case["files"], "changed_line_count": 0 if not paths else case["lines"] + case["files"]}
    serialized_pending = {"phase": pending["phase"], "route": pending["route"], "task_contract_grounded": pending.get("task_contract_grounded"),
        "force_full_review": pending.get("force_full_review"), "reviewer": pending.get("reviewer", "agy"),
        "skip_max_lines": pending.get("skip_max_lines"), "skip_max_files": pending.get("skip_max_files")}
    expected = {"claude_review_mode": result.get("claude_review_mode"), "reviewer1_skipped_reason": result.get("reviewer1_skipped_reason"),
        "hook_status": (observed.get("hook_checks") or {}).get("status", "NOT_RUN"),
        "side_effect_paths": (observed.get("hook_checks_full") or {}).get("side_effect_paths", []),
        "findings_codes": [item.get("code") for item in result.get("findings", [])
                           if item.get("code") in {"PROFILE_FORCES_FULL", "HOOK_CHECK_FAILED", "HOOK_CHECK_UNKNOWN", "HOOK_CHECK_SIDE_EFFECTS", "TASK_CONTRACT_NOT_GROUNDED"}]}
    if case["phase"] == "operation":
        expected.update({"claude_review_mode": "FULL", "reviewer1_skipped_reason": "PHASE_OPERATION"})
    return {"name": case["name"], "phase": pending["phase"], "route": pending["route"], "delta": delta,
        "pending": serialized_pending, "profile": {"stored": True, "force_full_review": pending["force_full_review"], "protected_paths": pending["protected_paths"],
            "skip_max_lines": pending["skip_max_lines"], "skip_max_files": pending["skip_max_files"], "reviewer": pending.get("reviewer", "agy")},
        "checks": fixture_checks, "bundle_force_full": case.get("outside", False),
        "gate_config": {**config["reviewer_gate"], "always_full_routes": config["always_full_routes"]},
        "route_known": pending["route"] in config["routes"], "reviewer_available": True, "expected": expected}


def test_go_post_review_python_fixture_and_parity(tmp_path):
    scratch = ROOT / ".tmp" / ("post-review-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex post_tool_use check scheduling and Reviewer-1 gate", "cases": []}
        for index, case in enumerate(CASES):
            root = scratch / str(index)
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_CHECKS_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8", newline="\n")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestSchedulePostReviewPythonFixture$", "-count=1", "./internal/enforce"], cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
