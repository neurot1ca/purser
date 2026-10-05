import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_outcome.json"
sys.path.insert(0, str(ORCH))
import codex


CASES = [
    {"name": "no_material_finding", "report": {"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []}},
    {"name": "changes_required", "report": {"result": "CHANGES_REQUIRED", "validation": "FAIL", "findings": [{"code": "BUG", "message": "evidence from reviewer"}]}},
    {"name": "invalid_json", "report": {"review_status": "UNAVAILABLE", "result": "REVIEWER1_UNAVAILABLE", "validation": "UNKNOWN", "error_code": "REVIEWER_RESPONSE_INVALID", "findings": [{"code": "REVIEWER_RESPONSE_INVALID", "message": "Reviewer-1 response was invalid (JSONDecodeError)."}]}},
    {"name": "incomplete_packet", "report": {"review_status": "NOT_RUN_INCOMPLETE_PACKET", "result": "UNCERTAIN", "validation": "UNKNOWN", "findings": [{"code": "REVIEW_PACKET_INCOMPLETE", "message": "Packet omitted required material."}]}},
    {"name": "out_of_scope_omitted", "report": {"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []}, "scope": "scope/file-0.txt"},
    {"name": "finding_evidence_cap", "report": {"result": "CHANGES_REQUIRED", "validation": "FAIL", "findings": [{"code": "BUG", "message": "x" * 2400}]}},
]


def _oracle(case, root):
    subprocess.run(["git", "init", "-q"], cwd=root, check=True)
    for index in range(3):
        path = root / "scope" / f"file-{index}.txt"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("base\n", encoding="utf-8")
    subprocess.run(["git", "add", "--all"], cwd=root, check=True)
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"], cwd=root, env=env, check=True)
    state = root.parent / (root.name + "-state")
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    config.update(micro_repair_enabled=False, max_total_hook_budget_seconds=1.0, reviewer1_timeout_seconds=0.8)
    paths = case.get("scope", "scope")
    prompt = f"ORCH|task=fixture-task|phase=implementation|route=normal|reason=new_task|basis=fixture|scope={paths}\nObjective: inspect. Acceptance criteria: preserve scope."
    event = {"session_id": "post-outcome-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    pre = codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    assert pre.get("hookSpecificOutput", {}).get("permissionDecision") != "deny"
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending.update(task_contract_grounded=True, skip_max_lines=0, skip_max_files=0)
    codex._atomic_json_write(pending_path, pending)
    for index in range(3):
        (root / "scope" / f"file-{index}.txt").write_text("changed\n", encoding="utf-8")
    event.update(hook_event_name="PostToolUse", tool_response={"structuredContent": {"threadId": "fixture-thread", "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","summary":"fixture","commands":["fixture"],"exit_codes":[0],"artifacts":[]}'}})

    def fake_reviewer(**kwargs):
        return {"review_status": case["report"].get("review_status", "COMPLETED"), "provider": "agy",
                "reviewer1_model_requested": "gemini-3.8-flash-high", **case["report"]}

    response = codex.post_tool_use(event, config=config, state_root=state, repo_root=root, reviewer=fake_reviewer)
    result = json.loads(response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"])
    findings = [item for item in result.get("findings", []) if item.get("code") in {"BUG", "REVIEWER_RESPONSE_INVALID", "REVIEW_PACKET_INCOMPLETE"}]
    expected = {"claude_review_mode": result.get("claude_review_mode"), "reviewer1_result": result.get("reviewer1_result"),
                "reviewer1_skipped_reason": result.get("reviewer1_skipped_reason"), "reviewer1_scope": result.get("reviewer1_scope"),
                "findings": findings}
    input_bundle = {"findings": [], "worker_validation": {"status": "PASS"}, "thread_id": "fixture-thread",
                    "protected_paths_touched": [], "whole_tree_delta": None}
    input_pending = {"phase": "implementation", "task_contract_grounded": True}
    input_delta = {"status": "OK", "changed_paths": [f"scope/file-{i}.txt" for i in range(3)]}
    input_classification = None
    attrs = {key: value for key, value in case["report"].items()}
    attrs.update(provider="agy", reviewer1_model_requested="gemini-3.8-flash-high")
    outcome = "incomplete" if case["name"] == "incomplete_packet" else "unavailable" if case["name"] == "invalid_json" else "completed"
    if case["name"] == "incomplete_packet":
        input_bundle["whole_tree_delta"] = {"changed_paths": input_delta["changed_paths"]}
    if case["name"] == "out_of_scope_omitted":
        input_classification = {"in_scope_paths": ["scope/file-0.txt"]}
    return {"name": case["name"], "outcome": outcome, "attributes": attrs, "pending": input_pending,
            "bundle": input_bundle, "delta": input_delta, "classification": input_classification,
            "expected": expected}


def test_go_post_outcome_python_fixture(tmp_path):
    scratch = ROOT / ".tmp" / ("post-outcome-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex post_tool_use initial Reviewer-1 outcome and scope mapping with reviewer callable faked", "cases": []}
        for index, case in enumerate(CASES):
            root = scratch / str(index)
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_OUTCOME_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestPostReviewerOutcomePythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
