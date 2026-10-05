import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_reviewer.json"
sys.path.insert(0, str(ORCH))
import codex

CASES = [
    {"name": "first_available", "fallback_family": "openai", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}, {"agent": "opencode", "model": "anthropic/claude-sonnet"}], "responses": ["completed"]},
    {"name": "unavailable_fallback", "fallback_family": "openai", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}, {"agent": "opencode", "model": "anthropic/claude-sonnet"}], "responses": ["unavailable", "completed"]},
    {"name": "same_family_filtered", "fallback_family": "google", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}, {"agent": "opencode", "model": "anthropic/claude-sonnet"}], "responses": ["completed"]},
    {"name": "all_filtered", "fallback_family": "google", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}, {"agent": "opencode", "model": "google/gemini-2.5-pro"}], "responses": []},
    {"name": "deadline_across_candidates", "fallback_family": "openai", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}, {"agent": "opencode", "model": "anthropic/claude-sonnet"}], "responses": ["slow_unavailable", "completed"]},
    {"name": "bridge_exception", "fallback_family": "openai", "candidates": [
        {"agent": "agy", "model": "gemini-3.8-flash-high"}], "responses": ["exception"]},
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
    config["micro_repair_enabled"] = False
    config["max_total_hook_budget_seconds"] = 1.0
    config["reviewer1_timeout_seconds"] = 0.8
    prompt = "ORCH|task=fixture-task|phase=implementation|route=normal|reason=new_task|basis=fixture|scope=scope\nObjective: inspect. Acceptance criteria: preserve scope."
    event = {"session_id": "post-reviewer-fixture", "hook_event_name": "PreToolUse", "tool_name": codex.CODEX_TOOL,
             "tool_use_id": "call-" + case["name"], "tool_input": {"prompt": prompt, "label": "fixture"}}
    pre = codex.pre_tool_use(event, config=config, state_root=state, repo_root=root)
    assert pre.get("hookSpecificOutput", {}).get("permissionDecision") != "deny"
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    pending.update({"task_contract_grounded": True, "reviewer_candidates": case["candidates"],
                    "fallback_family": case["fallback_family"], "skip_max_lines": 0, "skip_max_files": 0})
    codex._atomic_json_write(pending_path, pending)
    for index in range(3):
        (root / "scope" / f"file-{index}.txt").write_text("changed\n", encoding="utf-8")
    event.update(hook_event_name="PostToolUse", tool_response={"structuredContent": {"threadId": "fixture-thread", "content": "VALIDATION: {\"source\":\"WORKER_REPORTED\",\"status\":\"PASS\",\"summary\":\"fixture\",\"commands\":[\"fixture\"],\"exit_codes\":[0],\"artifacts\":[]}"}})
    calls = []

    def fake_reviewer(**kwargs):
        calls.append({"agent": kwargs.get("reviewer_agent"), "model": kwargs.get("reviewer_model")})
        response = case["responses"][len(calls) - 1]
        if response == "slow_unavailable":
            time.sleep(1.05)
            response = "unavailable"
        if response == "exception":
            raise RuntimeError("fixture bridge exception")
        return {"review_status": "UNAVAILABLE" if response == "unavailable" else "COMPLETED",
                "provider": kwargs.get("reviewer_agent"), "reviewer1_model_requested": kwargs.get("reviewer_model"),
                "result": "REVIEWER1_UNAVAILABLE" if response == "unavailable" else "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
                "validation": "UNKNOWN" if response == "unavailable" else "PASS", "findings": []}

    response = codex.post_tool_use(event, config=config, state_root=state, repo_root=root, reviewer=fake_reviewer)
    result = json.loads(response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"])
    return {"name": case["name"], "candidates": case["candidates"], "fallback_family": case["fallback_family"],
            "responses": case["responses"], "calls": calls,
            "attempts": result.get("reviewer1_attempts", []),
            "skipped_reason": result.get("reviewer1_skipped_reason"),
            "finding_codes": [item.get("code") for item in result.get("findings", [])]}


def test_go_post_reviewer_fixture(tmp_path):
    scratch = ROOT / ".tmp" / ("post-reviewer-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    try:
        fixture = {"oracle": "Python codex post_tool_use Reviewer-1 candidate selection with the reviewer callable faked", "cases": []}
        for index, case in enumerate(CASES):
            root = scratch / str(index)
            root.mkdir()
            fixture["cases"].append(_oracle(case, root))
        if os.environ.get("POST_REVIEWER_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestRunPostReviewerPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
