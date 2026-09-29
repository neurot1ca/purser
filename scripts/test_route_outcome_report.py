import json
from pathlib import Path

from route_outcome_report import markdown, report, timestamp


def test_route_outcome_report_aggregates_outcomes_and_reply_usage(tmp_path):
    state_root = tmp_path / "route-outcome-report-fixture"
    task_dir = state_root / "projects" / "0123456789abcdef" / "telemetry" / "tasks"
    task_dir.mkdir(parents=True)
    tasks = [
        {
            "task_id": "accepted-one", "project_key": "0123456789abcdef", "started_at": "2026-09-01T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "medium", "task_type": "implementation",
            "duration_ms": 1000, "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
            "claude_acceptance": {"outcome": "accepted", "send_backs": 0, "applied_as_is": True},
            "usage": {"codex_usage_known": True, "codex_total_tokens": 100,
                      "claude_task_usage_known": True, "claude_task_total_observed_tokens": 50},
        },
        {
            "task_id": "accepted-replies", "project_key": "0123456789abcdef", "started_at": "2026-09-02T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "medium", "task_type": "implementation",
            "duration_ms": 3000, "reply_calls": 2, "reviewer1_result": "CHANGES_REQUIRED",
            "claude_acceptance": {"outcome": "accepted", "send_backs": 2, "applied_as_is": False},
            # The task projection contains worker and Claude usage from its original call and replies.
            "usage": {"codex_usage_known": True, "codex_total_tokens": 300,
                      "claude_task_usage_known": True, "claude_task_total_observed_tokens": 150},
        },
        {
            "task_id": "rejected", "project_key": "0123456789abcdef", "started_at": "2026-09-03T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "medium", "task_type": "implementation",
            "claude_acceptance": {"outcome": "rejected", "send_backs": 1, "applied_as_is": False},
            "reviewer1_result": "UNCERTAIN",
        },
        {
            "task_id": "unknown", "project_key": "0123456789abcdef", "started_at": "2026-09-04T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "medium", "task_type": "implementation",
            "claude_acceptance": {"outcome": "unknown", "send_backs": 0, "applied_as_is": False},
        },
    ]
    for task in tasks:
        (task_dir / f"{task['task_id']}.json").write_text(json.dumps(task), encoding="utf-8")

    result = report(state_root, timestamp("2026-09-01T00:00:00Z"), project_key="0123456789abcdef", min_samples=4)
    group, = result["groups"]
    assert (group["sample_count"], group["accepted"], group["rejected"], group["unknown"]) == (4, 2, 1, 1)
    assert group["outcome_coverage"] == 0.75
    assert group["applied_as_is_rate"] == 0.5
    assert group["send_backs"] == 3
    assert group["accepted_duration_ms"] == {"count": 2, "median": 2000.0, "p90": 3000}
    assert group["accepted_worker_tokens"] == {"count": 2, "median": 200.0, "p90": 300}
    assert group["accepted_claude_tokens"] == {"count": 2, "median": 100.0, "p90": 150}
    assert group["reviewer_outcomes"] == {
        "CHANGES_REQUIRED": 1, "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE": 1,
        "UNCERTAIN": 1, "unknown": 1,
    }
    rendered = markdown(result, state_root / "report.md", state_root)
    assert "[accepted-replies](<projects/0123456789abcdef/telemetry/tasks/accepted-replies.json>)" in rendered
    assert "Accepted worker tokens (n / median / p90)" in rendered
    assert "Accepted Claude tokens (n / median / p90)" in rendered
    assert "2 / 200.0 / 300" in rendered
    assert "2 / 100.0 / 150" in rendered


def test_small_groups_suppress_rates(tmp_path):
    state_root = tmp_path / "route-outcome-small-fixture"
    task_dir = state_root / "projects" / "fedcba9876543210" / "telemetry" / "tasks"
    task_dir.mkdir(parents=True)
    task = {"task_id": "one", "project_key": "fedcba9876543210", "started_at": "2026-09-01T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "low", "task_type": "operation",
            "claude_acceptance": {"outcome": "accepted", "send_backs": 0, "applied_as_is": True}}
    (task_dir / "one.json").write_text(json.dumps(task), encoding="utf-8")
    group, = report(state_root, timestamp("2026-09-01T00:00:00Z"))["groups"]
    assert group["insufficient_evidence"] is True
    assert group["outcome_coverage"] is None
    assert group["applied_as_is_rate"] is None
