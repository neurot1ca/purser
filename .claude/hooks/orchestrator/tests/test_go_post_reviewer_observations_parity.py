import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_reviewer_observations.json"
sys.path.insert(0, str(ORCH))
import codex
import reviewer
import telemetry


CASES = [
    {"name": "completed_findings", "started": True, "outcome": "completed", "result": "CHANGES_REQUIRED",
     "findings": [{"code": "MISSING_TEST", "message": "Add a regression test."}], "duration_ms": 1250,
     "usage": {"input_tokens": 21, "output_tokens": 8}, "retry_count": 1, "retry_recovered": True},
    {"name": "completed_no_finding", "started": True, "outcome": "completed", "result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
     "findings": [], "duration_ms": 400, "usage": {}, "retry_count": 0, "retry_recovered": False},
    {"name": "unavailable", "started": True, "outcome": "unavailable", "result": "REVIEWER1_UNAVAILABLE",
     "findings": [{"code": "REVIEWER_TIMEOUT", "message": "Timed out."}], "duration_ms": 2500,
     "usage": {}, "retry_count": 0, "retry_recovered": False},
    {"name": "incomplete_packet", "started": True, "outcome": "incomplete", "result": "UNCERTAIN",
     "findings": [{"code": "INCOMPLETE_PACKET", "message": "Required material was omitted."}], "duration_ms": 0,
     "usage": {}, "retry_count": 0, "retry_recovered": False},
    {"name": "skipped_no_delta", "started": False, "skipped_reason": "NO_DELTA"},
    {"name": "skipped_phase_plan", "started": False, "skipped_reason": "PHASE_PLAN"},
    {"name": "bridge_exception", "started": True, "outcome": "unavailable", "result": "REVIEWER1_UNAVAILABLE",
     "findings": [{"code": "REVIEWER_BRIDGE_EXCEPTION", "message": "Reviewer-1 failed unexpectedly (RuntimeError)."}],
     "error_code": "REVIEWER_BRIDGE_EXCEPTION", "duration_ms": 75, "usage": {},
     "retry_count": 0, "retry_recovered": False},
]


def _report(case):
    findings = tuple((item["code"], item["message"]) for item in case.get("findings", []))
    if case["outcome"] == "completed":
        return reviewer.CompletedReview(
            result=case["result"], validation="PASS", findings=findings, conversation_id="fixture-conversation",
            reviewer1_model_requested="gemini-pro", duration_seconds=case["duration_ms"] / 1000,
            usage=tuple(sorted(case["usage"].items())), provider="gemini",
            served_model_attestation="UNAVAILABLE", retry_count=case["retry_count"], retry_recovered=case["retry_recovered"],
        )
    if case["outcome"] == "unavailable":
        return reviewer.UnavailableReview(
            error_code=case.get("error_code", case["findings"][0]["code"]), message=case["findings"][0]["message"],
            reviewer1_model_requested="gemini-pro", elapsed_seconds=case["duration_ms"] / 1000,
            provider="gemini", retry_count=case["retry_count"], retry_recovered=case["retry_recovered"],
        )
    return reviewer.IncompleteReview(
        code=case["findings"][0]["code"], message=case["findings"][0]["message"], omitted=("diff",),
        reviewer1_model_requested="gemini-pro", provider="gemini",
    )


def _oracle(case, root):
    state = root / "state"
    state.mkdir()
    sequence = iter(range(1, 20))
    original_uuid4, original_now = telemetry.schema.uuid.uuid4, telemetry.schema.utc_now
    telemetry.schema.uuid.uuid4 = lambda: uuid.UUID(int=next(sequence))
    telemetry.schema.utc_now = lambda: "2026-01-02T03:04:05Z"
    try:
        report = _report(case) if case["started"] else None
        bundle = {"reviewer1_provider": "gemini", "reviewer1_model_requested": "gemini-pro",
                  "reviewer1_result": case.get("result"), "reviewer1_skipped_reason": case.get("skipped_reason"),
                  "validation": "UNKNOWN", "claude_review_mode": "FULL", "routing_enforced": True}
        pending = {"task_id": "reviewer-task", "project_key": "0123456789abcdef", "repo_root": None}
        event = {"session_id": "reviewer-fixture-session", "repo": "fixture-repo", "branch": "main"}
        if case["started"]:
            codex._observe("emit", "REVIEWER1_STARTED", task_id="reviewer-task", state_root=state,
                           **codex._telemetry_envelope(event, project_key="0123456789abcdef"),
                           reviewer1_provider="gemini", reviewer1_model_requested="gemini-pro")
        codex._post_observations(event, state, pending, bundle, reviewer_report=report,
                                 reviewer_started=case["started"], reviewer_elapsed_seconds=case.get("duration_ms", 0) / 1000)
        path = state / "telemetry" / "events" / "by_task" / "reviewer-task.jsonl"
        events = []
        if path.exists():
            for line in path.read_text(encoding="utf-8").splitlines():
                item = json.loads(line)
                if item["event_type"].startswith("REVIEWER1_"):
                    item.pop("event_id", None)
                    item.pop("ts_utc", None)
                    events.append(item)
        return {"name": case["name"], **case,
                "provider": "gemini", "requested_model": "gemini-pro",
                "initial_finding_count": len(case.get("findings", [])),
                "attributes": _attributes(case), "events": events}
    finally:
        telemetry.schema.uuid.uuid4, telemetry.schema.utc_now = original_uuid4, original_now


def _attributes(case):
    if not case["started"]:
        return {}
    return {"provider": "gemini", "reviewer1_model_requested": "gemini-pro", "result": case["result"],
            "review_status": "COMPLETED" if case["outcome"] == "completed" else
                "NOT_RUN_INCOMPLETE_PACKET" if case["outcome"] == "incomplete" else "UNAVAILABLE",
            "findings": case.get("findings", []), "error_code": case.get("error_code") or
                (case.get("findings") or [{}])[0].get("code"),
            "retry_count": case.get("retry_count", 0), "retry_recovered": case.get("retry_recovered", False),
            "reviewer1_retry_count": case.get("retry_count", 0),
            "reviewer1_retry_recovered": case.get("retry_recovered", False),
            "usage": case.get("usage", {})}


def test_post_reviewer_observations_python_fixture_and_go_parity(tmp_path):
    fixture = {"oracle": "codex._post_observations with reviewer.py result types", "cases": []}
    for case in CASES:
        root = tmp_path / case["name"]
        root.mkdir()
        fixture["cases"].append(_oracle(case, root))
    if os.environ.get("POST_REVIEWER_OBSERVATIONS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestPostReviewerObservationsPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
