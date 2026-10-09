import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_artifacts.json"

import sys
sys.path.insert(0, str(ORCH))
import codex


def _oracle():
    event = {"session_id": "post-artifacts-session", "tool_use_id": "post-artifacts-call"}
    key = codex._call_key(event)
    plans = []
    for name, task_id, report in (
        ("under", "task-5", "# Summary: Short plan\nDetails"),
        ("exact", "task-5", "x" * 15000),
        ("over", "task /5", "x" * 15001 + "\n\n## Final milestone\nRisks\nHandoff\nAppendix"),
        ("long-summary", "task-6", "# Summary:\n" + "s" * 5001 + "\n# Goal and boundaries\nPlan"),
        ("nested-summary", "task-7", "# Summary: Overview\n## Nested detail\nIncluded in summary\n# Goal and boundaries\nOutside summary"),
    ):
        bundle = {"findings": []}
        root = Path(".tmp") / "post-artifacts-oracle"
        path = root / "plans" / f"{task_id.replace(' ', '_').replace('/', '_')}-{key}.md"
        codex._store_plan(root, event, task_id, report, bundle)
        # The plan filename and persisted text are stable; absolute temp roots are not.
        stored = (root / "plans" / path.name).read_text(encoding="utf-8")
        plans.append({"name": name, "task_id": task_id, "report": report,
                      "filename": path.name, "content": stored,
                      "summary": bundle["plan_summary"], "findings": bundle["findings"]})

    bundle = {
        "task_id": "task-5", "phase": "implementation", "implementer": "codex-main",
        "thread_id": "thread-5", "routing_enforced": True, "claude_review_mode": "FULL",
        "implementer_report": "R" * 80, "findings": [{"code": "F", "message": "m" * 1600}],
        "changed_paths": [], "plan_path": None, "plan_summary": None,
        "codex_quota": {"source": "snapshot", "remaining": "q" * 200}, "raw_output_path": "__RAW__",
        "full_bundle_path": None,
    }
    cap = 1800
    compact, _ = codex._compact_bundle(bundle, cap)
    compact["implementer_report"] = compact.get("implementer_report", "")[:codex._report_limit({}, bundle["phase"])]
    compact["codex_quota"] = bundle["codex_quota"]
    excess = codex._non_report_bytes(compact) - cap
    if excess > 0:
        smaller, overflow = codex._compact_bundle(bundle, max(0, cap - excess))
        smaller["codex_quota"] = bundle["codex_quota"]
        if not overflow and codex._non_report_bytes(smaller) <= cap:
            compact = smaller
            compact["implementer_report"] = compact.get("implementer_report", "")[:codex._report_limit({}, bundle["phase"])]
        else:
            compact.pop("codex_quota")

    raw_name = key + ".json"
    full_name = key + ".json"
    raw_payload = {"content": "raw-output", "threadId": "thread-5"}
    expected_messages = compact.get("findings", [{}])[0].get("message", "")
    return {"oracle": "codex._store_plan+_compact_bundle+_finalize_replacement artifact rules",
            "event": event, "call_key": key, "plans": plans,
            "report_limits": {"implementation": codex._report_limit({}, "implementation"),
                              "operation": codex._report_limit({}, "operation"),
                              "custom": codex._report_limit({"operation_report_max_chars": 7}, "operation"),
                              "report": "R" * 80,
                              "truncated": ("R" * 80)[:codex._report_limit({"operation_report_max_chars": 7}, "operation")]},
            "quota": {"bundle": bundle, "cap": cap, "expected": {
                "quota": bundle["codex_quota"],
                "message_was_compacted": len(expected_messages) < len(bundle["findings"][0]["message"])}},
            "artifacts": {"raw_filename": raw_name, "raw_payload": raw_payload,
                          "full_filename": full_name, "bundle": bundle}}


def test_post_artifacts_match_python_fixture():
    fixture = _oracle()
    over_plan = next(plan for plan in fixture["plans"] if plan["name"] == "over")
    long_summary = next(plan for plan in fixture["plans"] if plan["name"] == "long-summary")
    nested_summary = next(plan for plan in fixture["plans"] if plan["name"] == "nested-summary")
    assert over_plan["content"].endswith("## Final milestone\nRisks\nHandoff\nAppendix")
    assert over_plan["findings"] == [{
        "code": "PLAN_SHAPE_MISSING",
        "message": "Plan lacks the required sections: Summary:, Goal and boundaries, Current evidence, Decision, Milestones, Risks/open decisions, Implementation handoff.",
    }]
    assert long_summary["summary"] == "s" * 5001
    assert nested_summary["summary"] == "Overview\n## Nested detail\nIncluded in summary"
    if os.environ.get("POST_ARTIFACTS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"),
           "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestPostArtifactsPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr


def test_multibyte_plan_summary_budget_uses_complete_artifact():
    summary = "界🙂" * 1000
    path = str(Path(".tmp") / "complete-plan.md")
    bundle = {"plan_path": path, "plan_summary": summary, "findings": [], "implementer_report": ""}
    notice = f"Complete plan summary is available at {path}."
    cap = len(summary) + 50
    assert codex._non_report_bytes({**bundle, "plan_summary": notice}) <= cap < len(summary.encode("utf-8"))
    compact, overflow = codex._compact_bundle(bundle, cap)
    assert not overflow
    assert compact["plan_path"] == path
    assert compact["plan_summary"] == notice
    assert codex._non_report_bytes(compact) <= cap
