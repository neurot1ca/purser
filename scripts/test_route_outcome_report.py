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


def test_task_evidence_distribution_and_historical_records(tmp_path):
    state_root = tmp_path / "route-evidence-fixture"
    task_dir = state_root / "projects" / "0123456789abcdef" / "telemetry" / "tasks"
    task_dir.mkdir(parents=True)
    tasks = [
        {
            "task_id": "fallback-mismatch", "project_key": "0123456789abcdef",
            "started_at": "2026-09-01T00:00:00Z", "route": "normal", "model": "coder",
            "task_type": "counting_status", "selected_agent": "opencode",
            "selected_provider": "openai", "selected_model": "gpt-coder",
            "served_agent": "claude", "served_provider": "anthropic", "served_model": "sonnet",
            "suspected_route_mismatch": {"declared_route": "normal", "derived_type": "operation"},
            "claude_decision": {"decision": "send_back"},
            "reviewer1_result": "CHANGES_REQUIRED", "reviewer1_error_code": "TEMPORARY_ERROR",
            "reviewer1_retry_count": 1, "reviewer1_retry_recovered": True,
            "usage": {"codex_usage_known": True, "codex_total_tokens": 125},
        },
        {
            "task_id": "new-accepted", "project_key": "0123456789abcdef",
            "started_at": "2026-09-02T00:00:00Z", "route": "deep",
            "task_type": "operation", "selected_agent": "codex", "selected_provider": "openai",
            "selected_model": "gpt-coder", "served_agent": "codex", "served_provider": "openai",
            "served_model": "gpt-coder", "suspected_route_mismatch": None,
            "claude_decision": {"decision": "accepted"}, "reviewer1_result": "APPROVED",
            "reviewer1_retry_count": 0, "reviewer1_retry_recovered": False,
            "usage": {"codex_usage_known": False},
        },
    ]
    for task in tasks:
        (task_dir / f"{task['task_id']}.json").write_text(json.dumps(task), encoding="utf-8")

    result = report(state_root, timestamp("2026-09-01T00:00:00Z"), project_key="0123456789abcdef")
    rendered = markdown(result, state_root / "report.md", state_root)
    assert result["distribution"]["historical_records"] == 0
    assert result["distribution"]["codex_known_count"] == 1
    assert result["distribution"]["codex_unknown_count"] == 1
    assert result["distribution"]["codex_total_tokens"] == 125
    assert result["distribution"]["route_mismatches"][0]["task_ids"] == ["fallback-mismatch"]
    assert "Task evidence distribution" in rendered
    assert "selected_agent" in rendered and "opencode" in rendered
    assert "served_agent" in rendered and "claude" in rendered
    assert "fallback" in rendered
    assert "historical" in rendered
    assert "Unknown / total" in rendered and "1 / 2" in rendered
    assert "Suspected route mismatches" in rendered
    assert "reviewer1_error_code" in rendered and "TEMPORARY_ERROR" in rendered
    assert "reviewer1_retry_count" in rendered and "reviewer1_retry_recovered" in rendered


def test_old_task_records_are_historical_and_unknown(tmp_path):
    state_root = tmp_path / "route-old-records"
    task_dir = state_root / "projects" / "old-project" / "telemetry" / "tasks"
    task_dir.mkdir(parents=True)
    task = {"task_id": "old-task", "project_key": "old-project", "started_at": "2026-09-01T00:00:00Z",
            "route": "normal", "model": "coder", "effort": "medium",
            "claude_acceptance": {"outcome": "accepted"}}
    (task_dir / "old-task.json").write_text(json.dumps(task), encoding="utf-8")

    result = report(state_root, timestamp("2026-09-01T00:00:00Z"), project_key="old-project")
    rendered = markdown(result, state_root / "report.md", state_root)
    assert result["distribution"]["historical_records"] == 1
    assert result["distribution"]["dimensions"]["selected_agent"] == {
        "counts": {"unknown": 1}, "unknown": 1, "total": 1,
    }
    assert "historical (no milestone 2–3 fields): 1" in rendered
    assert "Unknown / total" in rendered
