import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_rereview.json"
sys.path.insert(0, str(ORCH))
import codex
import policy

REAL_RUN = subprocess.run
CASES = [
    {"name": "rereview_clears", "initial": "CHANGES_REQUIRED", "repair": True, "rereview": "clear"},
    {"name": "rereview_still_finds", "initial": "CHANGES_REQUIRED", "repair": True, "rereview": "findings"},
    {"name": "rereview_unavailable", "initial": "CHANGES_REQUIRED", "repair": True, "rereview": "unavailable"},
    {"name": "rereview_replaces_pre_repair_findings", "initial": "CHANGES_REQUIRED", "repair": True, "rereview": "findings"},
    {"name": "rereview_failure", "initial": "CHANGES_REQUIRED", "repair": True, "rereview": "failure"},
    {"name": "no_repair_initial_review_receipt", "initial": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "repair": False},
]


def _oracle(case, root):
    REAL_RUN(["git", "init", "-q"], cwd=root, check=True)
    source = root / "scope" / "file with spaces.py"
    source.parent.mkdir()
    source.write_text("value = 1\n", encoding="utf-8")
    REAL_RUN(["git", "add", "--all"], cwd=root, check=True)
    REAL_RUN(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "base"], cwd=root, check=True)
    state = root.parent / (root.name + "-state")
    config = json.loads((ORCH / "config.json").read_text(encoding="utf-8"))
    config.update(micro_repair_enabled=True, micro_repair_timeout_seconds=15, max_total_hook_budget_seconds=90,
                  reviewer1_timeout_seconds=2, reviewer_gate={"small_delta_max_lines": 0, "small_delta_max_files": 0})
    prompt = ("ORCH|task=fixture-task|phase=implementation|route=normal|reason=new_task|basis=fixture|scope=scope\n"
              "Objective: inspect. Acceptance criteria: preserve scope.")
    event = {"session_id": "post-rereview-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending["task_contract_grounded"] = True
    codex._atomic_json_write(pending_path, pending)
    source.write_text("value = 2\n", encoding="utf-8")
    post = {**event, "hook_event_name": "PostToolUse", "tool_response": {"structuredContent": {
        "threadId": "fixture-thread", "content": 'VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["fixture"],"exit_codes":[0]}'}}}
    reports = []

    def fake_reviewer(**kwargs):
        reports.append(kwargs)
        if len(reports) == 1:
            result = case["initial"]
        else:
            if case["rereview"] == "failure":
                return None
            result = {"clear": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "findings": "CHANGES_REQUIRED",
                      "unavailable": policy.REVIEWER1_UNAVAILABLE}[case["rereview"]]
        return {"review_status": "UNAVAILABLE" if result == policy.REVIEWER1_UNAVAILABLE else "COMPLETED",
                "provider": "agy", "reviewer1_model_requested": "gemini-3.8-flash-high", "result": result,
                "validation": "UNKNOWN" if result == policy.REVIEWER1_UNAVAILABLE else "PASS",
                "findings": ([{"code": "PRE_REPAIR", "message": "first pass"}] if len(reports) == 1 and result == "CHANGES_REQUIRED"
                             else [{"code": "REVIEW", "message": "still required"}] if result == "CHANGES_REQUIRED" else [])}

    def fake_run(argv, **kwargs):
        if argv and argv[0] == "codex" and argv[1:3] == ["exec", "resume"]:
            return subprocess.CompletedProcess(argv, 0, "repaired", "")
        return REAL_RUN(argv, **kwargs)

    if case["repair"]:
        subprocess.run = fake_run
    try:
        response = codex.post_tool_use(post, config=config, state_root=state, repo_root=root, reviewer=fake_reviewer)
    finally:
        subprocess.run = REAL_RUN
    text = response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"]
    bundle = json.loads(text)
    return {"name": case["name"], "input": {
        "repair_ran": bundle.get("micro_repair") is not None,
        "repair_status": (bundle.get("micro_repair") or {}).get("status"),
        "reviewer1_result": bundle.get("reviewer1_result"),
        "reviewer1_skipped_reason": bundle.get("reviewer1_skipped_reason"),
        "reviewer1_provider": bundle.get("reviewer1_provider"),
        "reviewer1_model_requested": bundle.get("reviewer1_model_requested"),
        "implementer": bundle.get("implementer"), "model": bundle.get("model"), "effort": bundle.get("effort"),
        "validation": bundle.get("validation"), "worker_validation": bundle.get("worker_validation"),
        "hook_checks": bundle.get("hook_checks"), "thread_id": bundle.get("thread_id"),
        "changed_paths": bundle.get("changed_paths"), "delta_lines": bundle.get("delta_lines"),
        "delta_files": bundle.get("delta_files"), "delta": {"status": "OK"},
        "task_contract_grounded": pending.get("task_contract_grounded"), "task_id": pending.get("task_id"),
        "route": pending.get("route"), "phase": pending.get("phase"),
        "reviewer1_scope": bundle.get("reviewer1_scope"),
        "pre_repair_findings": ([{"code": "PRE_REPAIR", "message": "first pass"}]
                                if case["name"] == "rereview_replaces_pre_repair_findings" else []),
        "rereview": (None if not case["repair"] else {
            "result": bundle.get("reviewer1_result") or policy.UNCERTAIN, "review_status": "UNAVAILABLE" if bundle.get("reviewer1_result") == policy.REVIEWER1_UNAVAILABLE else "COMPLETED",
            "validation": bundle.get("validation"), "provider": bundle.get("reviewer1_provider"),
            "reviewer1_model_requested": bundle.get("reviewer1_model_requested"),
            "findings": ([{"code": "REVIEW", "message": "still required"}] if bundle.get("reviewer1_result") == "CHANGES_REQUIRED" else [])}),
    }, "expected": {"micro_repair_cleared_review": bundle.get("micro_repair_cleared_review"),
                    "repair_status": (bundle.get("micro_repair") or {}).get("status"),
                    "claude_review_mode": bundle.get("claude_review_mode"), "audit_receipt": bundle.get("audit_receipt"),
                    "findings": bundle.get("findings"), "validation": bundle.get("validation")}}


def test_go_post_rereview_matches_python_fixture(tmp_path):
    scratch = ROOT / ".tmp" / ("post-rereview-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex.post_tool_use Reviewer-1 post-repair re-review and final audit receipt", "cases": []}
        for index, case in enumerate(CASES):
            root = scratch / str(index)
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_REREVIEW_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestPostRereviewPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
