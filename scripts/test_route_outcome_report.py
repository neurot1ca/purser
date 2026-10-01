import json
from pathlib import Path

from route_outcome_report import markdown, report, timestamp


def _write_tasks(state_root, tasks):
    task_dir = state_root / "projects" / "0123456789abcdef" / "telemetry" / "tasks"
    task_dir.mkdir(parents=True)
    for task in tasks:
        task.setdefault("project_key", "0123456789abcdef")
        task.setdefault("started_at", "2026-09-01T00:00:00Z")
        (task_dir / f"{task['task_id']}.json").write_text(json.dumps(task), encoding="utf-8")


def _report(state_root, *, min_samples=1):
    return report(state_root, timestamp("2026-09-01T00:00:00Z"),
                  project_key="0123456789abcdef", min_samples=min_samples)


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


def test_fallback_comparison_attributes_to_served_route_and_preserves_both(tmp_path):
    state_root = tmp_path / "fallback"
    _write_tasks(state_root, [{
        "task_id": "fallback", "route": "normal", "model": "selected-model", "effort": "high",
        "task_type": "implementation", "selected_agent": "codex", "selected_provider": "openai",
        "selected_model": "selected-model", "served_agent": "claude", "served_provider": "anthropic",
        "served_model": "served-model", "served_effort": "medium", "duration_ms": 40,
        "claude_decision": {"decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z",
                            "source": {"kind": "apply", "job_id": "job-1"}},
        "claude_acceptance": {"outcome": "accepted", "sources": [{"kind": "apply", "job_id": "job-1",
                           "decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z"}]},
        "usage": {"codex_usage_known": True, "codex_total_tokens": 30,
                  "claude_task_usage_known": True, "claude_task_total_observed_tokens": 20},
    }])

    group, = _report(state_root, min_samples=5)["groups"]
    assert group["served_route"] == "claude"
    assert group["served_model"] == "served-model"
    assert group["served_effort"] == "medium"
    assert group["selected_route"] == "codex"
    assert group["selected_model"] == "selected-model"
    assert group["selected_effort"] == "high"
    assert group["fallback"] is True
    assert group["sources"][0]["decision_time"] == "2026-09-01T00:01:00Z"
    assert group["decision"] == "no decision"


def test_rejected_and_sent_back_attempt_cost_is_in_cost_per_acceptance(tmp_path):
    state_root = tmp_path / "rework-cost"
    common_usage = {"codex_usage_known": True, "codex_total_tokens": 100,
                    "claude_task_usage_known": True, "claude_task_total_observed_tokens": 50}
    _write_tasks(state_root, [
        {"task_id": "sent-back-accepted", "route": "normal", "model": "m", "effort": "low",
         "task_type": "implementation", "duration_ms": 1000,
         "claude_acceptance": {"outcome": "accepted", "send_backs": 1, "sources": [{"kind": "apply",
           "job_id": "a", "decision": "send_back", "ts_utc": "2026-09-01T00:00:30Z"},
           {"kind": "apply", "job_id": "a", "decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z"}]},
         "claude_decision": {"decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z"}, "usage": common_usage},
        {"task_id": "rejected", "route": "normal", "model": "m", "effort": "low",
         "task_type": "implementation", "duration_ms": 3000,
         "claude_acceptance": {"outcome": "rejected", "sources": [{"kind": "apply", "job_id": "b",
           "decision": "rejected", "ts_utc": "2026-09-01T00:02:00Z"}]}, "usage": common_usage},
    ])

    group, = _report(state_root)["groups"]
    assert group["cost_per_accepted"]["worker_tokens"] == 200
    assert group["cost_per_accepted"]["claude_tokens"] == 100
    assert group["cost_per_accepted"]["elapsed_ms"] == 4000
    assert group["usage"]["worker_tokens"] == {"known": 2, "unknown": 0, "total": 2}
    assert group["attempt_elapsed_ms"] == {"known": 2, "unknown": 0, "total": 2}


def test_fetch_only_operation_acceptance_is_separate_from_applied_work(tmp_path):
    state_root = tmp_path / "fetched-operation"
    _write_tasks(state_root, [{
        "task_id": "fetched", "route": "normal", "model": "m", "effort": "low", "task_type": "operation",
        "phase": "operation", "duration_ms": 30,
        "claude_acceptance": {"outcome": "accepted", "sources": [{"kind": "result", "job_id": "r",
                           "decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z"}]},
        "claude_decision": {"decision": "accepted", "ts_utc": "2026-09-01T00:01:00Z"},
    }])

    group, = _report(state_root)["groups"]
    assert group["work_kind"] == "fetched_operation"
    assert group["accepted"] == 1
    assert group["applied_accepted"] == 0
    assert group["fetched_operation_accepted"] == 1


def test_comparison_type_uses_pre_outcome_label_only(tmp_path):
    state_root = tmp_path / "pre-outcome-type"
    _write_tasks(state_root, [
        {
            "task_id": "decision-label", "route": "normal", "model": "m", "effort": "low",
            "task_type": "implementation", "task_type_source": "decision",
            "claude_decision": {"decision": "accepted", "task_type": "implementation",
                                "ts_utc": "2026-09-01T00:01:00Z"},
            "claude_acceptance": {"outcome": "accepted"},
        },
        {
            "task_id": "route-label", "route": "normal", "model": "m", "effort": "low",
            "task_type": "counting_status", "task_type_source": "route",
            "claude_decision": {"decision": "accepted", "task_type": "implementation",
                                "ts_utc": "2026-09-01T00:01:00Z"},
            "claude_acceptance": {"outcome": "accepted"},
        },
    ])

    groups = _report(state_root)["groups"]
    assert {group["task_type"] for group in groups} == {"unknown", "counting_status"}


def test_missing_outcome_and_usage_are_reported_as_unknown(tmp_path):
    state_root = tmp_path / "missing-evidence"
    _write_tasks(state_root, [{
        "task_id": "unknown", "route": "normal", "model": "m", "effort": "low", "task_type": "implementation",
    }])

    group, = _report(state_root)["groups"]
    rendered = markdown(_report(state_root), state_root / "report.md", state_root)
    assert group["unknown"] == 1
    assert group["outcome_denominator"] == {"known": 0, "unknown": 1, "total": 1}
    assert group["usage"]["worker_tokens"] == {"known": 0, "unknown": 1, "total": 1}
    assert group["usage"]["claude_tokens"] == {"known": 0, "unknown": 1, "total": 1}
    assert group["decision"] == "no decision"
    assert "unknown" in rendered
