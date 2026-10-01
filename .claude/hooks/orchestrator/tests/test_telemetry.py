import json
from contextlib import contextmanager
from datetime import datetime, timedelta, timezone
import hashlib
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import time
import uuid

import pytest


PACKAGE_ROOT = Path(__file__).parents[1]
sys.path.insert(0, str(PACKAGE_ROOT))

import codex
import reviewer
import telemetry
import telemetry_schema as schema

UNRESOLVED_PROJECT_KEY = "unresolved"


@pytest.fixture()
def tmp_path():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "telemetry" / uuid.uuid4().hex
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)

        shutil.rmtree(root, onerror=make_writable)
        try:
            root.parent.rmdir()
        except OSError:
            pass


@pytest.fixture(autouse=True)
def disable_configured_projection_for_telemetry_tests(monkeypatch, tmp_path):
    config_path = tmp_path / "telemetry" / "config.json"
    config_path.parent.mkdir()
    config_path.write_text(
        json.dumps({"obsidian_projection": {"enabled": False}}),
        encoding="utf-8",
    )
    monkeypatch.setattr(telemetry, "_CONFIG_PATH", config_path)


def _events(state_root):
    values = []
    for path in (state_root / "telemetry" / "events").glob("????-??/session_*.jsonl"):
        values.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return values


def _emit_task(state_root, task_id="task-1"):
    common = {
        "task_id": task_id,
        "state_root": state_root,
        "session_id": "session-1",
        "repo": "repo",
        "branch": "main",
        "agent_id": "call-1",
        "agent_type": "codex-main",
    }
    telemetry.emit(
        "TASK_ROUTE_SELECTED",
        **common,
        phase="implementation",
        route="normal",
        model="gpt-6-sol",
        effort="medium",
        routing_enforced=True,
        fresh_thread=True,
        reply_call=False,
    )
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        **common,
        phase="implementation",
        route="normal",
        model="gpt-6-sol",
        effort="medium",
        routing_enforced=True,
        fresh_thread=True,
        reply_call=False,
    )
    telemetry.emit(
        "IMPLEMENTER_FINISHED",
        **common,
        duration_ms=12.5,
        changed_paths_count=2,
        validation="PASS",
        thread_id="thread-1",
        claude_review_mode="AUDIT",
        routing_enforced=True,
        codex_usage_known=False,
    )
    telemetry.update_task(task_id, state_root=state_root)


def test_reviewer_failure_error_code_schema_and_stderr_evidence(tmp_path):
    event = {"session_id": "session-1", "tool_use_id": "review-call-1"}
    payload = {
        "failure_code": "REVIEWER1_UNAVAILABLE",
        "reviewer1_error_code": "REVIEWER_TIMEOUT",
        "reviewer1_provider": "agy",
        "reviewer1_model_requested": "gemini-3.8-flash-high",
        "served_model_attestation": "UNAVAILABLE",
        "reviewer1_result": "REVIEWER1_UNAVAILABLE",
        "duration_ms": 2500.0,
        "gemini_usage_known": False,
    }

    valid = schema.build_event("REVIEWER1_FAILED", task_id="task-1", payload=payload)
    assert schema.valid_event(valid)
    with pytest.raises(ValueError, match="reviewer error code"):
        schema.build_event(
            "REVIEWER1_FAILED",
            task_id="task-1",
            payload={**payload, "reviewer1_error_code": "reviewer_timeout"},
        )

    report = reviewer.UnavailableReview(
        "REVIEWER_TIMEOUT",
        "Reviewer-1 exceeded its bounded timeout.",
        "gemini-3.8-flash-high",
        elapsed_seconds=2.5,
        stderr_tail="server stalled",
    )
    findings = codex._reviewer_findings_with_evidence(report, tmp_path, event)

    assert "stderr:" in findings[0]["message"]
    evidence_path = Path(findings[0]["message"].split("stderr: ", 1)[1])
    assert evidence_path.exists()
    assert json.loads(evidence_path.read_text(encoding="utf-8")) == "server stalled"


def test_legacy_call_event_without_reason_fields_still_parses():
    legacy = schema.build_event(
        "IMPLEMENTER_STARTED",
        task_id="legacy-task",
        payload={
            "phase": "implementation",
            "route": "normal",
            "model": "gpt-6-sol",
            "effort": "medium",
            "routing_enforced": True,
            "fresh_thread": True,
            "reply_call": False,
        },
    )

    assert schema.valid_event(legacy)
    assert "eta_minutes" not in legacy["payload"]
    assert legacy["payload"].get("reason", "unknown") == "unknown"


def test_legacy_call_event_without_eta_minutes_still_parses():
    legacy = schema.build_event(
        "IMPLEMENTER_FINISHED",
        task_id="legacy-task",
        payload={
            "duration_ms": 1000,
            "changed_paths_count": 1,
            "validation": "PASS",
            "thread_id": "thread-1",
            "claude_review_mode": "FULL",
            "routing_enforced": True,
            "codex_usage_known": False,
        },
    )

    assert schema.valid_event(legacy)


def _decision(state_root, decision, source, *, task_id="decision-task", task_type="implementation"):
    telemetry.emit(
        "CLAUDE_DECISION_RECORDED", task_id=task_id, project_key="unresolved",
        state_root=state_root, decision=decision, task_type=task_type, source=source,
    )


def test_claude_decision_schema_rejects_invalid_decision_and_source_kind():
    payload = {"decision": "accepted", "task_type": "implementation",
               "source": {"kind": "apply", "job_id": "job-1"}}
    assert schema.valid_event(schema.build_event("CLAUDE_DECISION_RECORDED",
                                                  task_id="decision-task", payload=payload))
    for invalid in ({**payload, "decision": "maybe"},
                    {**payload, "source": {"kind": "manual", "job_id": "job-1"}}):
        with pytest.raises(ValueError):
            schema.build_event("CLAUDE_DECISION_RECORDED", task_id="decision-task", payload=invalid)


def test_claude_decision_duplicate_source_counts_once(tmp_path):
    pointer = {"kind": "reply", "job_id": "reply-1"}
    _decision(tmp_path, "send_back", pointer)
    _decision(tmp_path, "send_back", pointer)
    telemetry.update_task("decision-task", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "decision-task.json").read_text(encoding="utf-8"))
    assert aggregate["claude_acceptance"]["send_backs"] == 1
    assert len(aggregate["claude_acceptance"]["sources"]) == 1

    _decision(tmp_path, "accepted", {"kind": "apply", "job_id": "apply-as-is"}, task_id="as-is")
    telemetry.update_task("as-is", state_root=tmp_path)
    as_is = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                        "tasks" / "as-is.json").read_text(encoding="utf-8"))
    assert as_is["claude_acceptance"]["applied_as_is"] is True
    assert as_is["task_type_source"] == "decision"
    projected = telemetry.obsidian_projection._prepare_aggregate(as_is)
    assert projected["task_type"] == "implementation"
    assert projected["claude_acceptance"]["sources"][0]["ts_utc"]


def test_claude_send_back_then_acceptance_projects_applied_as_is_false(tmp_path):
    _decision(tmp_path, "send_back", {"kind": "reply", "job_id": "reply-1"})
    _decision(tmp_path, "accepted", {"kind": "apply", "job_id": "apply-1"})
    telemetry.update_task("decision-task", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "decision-task.json").read_text(encoding="utf-8"))
    assert aggregate["task_type"] == "implementation"
    assert aggregate["claude_acceptance"] == {
        "outcome": "accepted", "send_backs": 1, "applied_as_is": False,
        "sources": aggregate["claude_acceptance"]["sources"],
    }
    assert [source["ts_utc"] for source in aggregate["claude_acceptance"]["sources"]]


def test_task_fold_preserves_selected_and_served_identity_after_fallback(tmp_path):
    common = {"task_id": "fallback-task", "state_root": tmp_path,
              "session_id": "session-1", "repo": "repo", "branch": "main",
              "agent_id": "call-1", "agent_type": "codex-main"}
    telemetry.emit("TASK_ROUTE_SELECTED", **common, phase="implementation", route="mechanical",
                   model="open-code/candidate", agent="opencode", provider="open-code",
                   effort="low", routing_enforced=True, task_type="counting_status")
    telemetry.emit("IMPLEMENTER_FINISHED", **common, duration_ms=1, implementer="codex-main",
                   served_model="gpt-6-sol", served_agent="codex", served_provider="openai",
                   codex_usage_known=False)
    telemetry.update_task("fallback-task", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "fallback-task.json").read_text(encoding="utf-8"))
    assert (aggregate["selected_agent"], aggregate["selected_provider"], aggregate["selected_model"]) == (
        "opencode", "open-code", "open-code/candidate")
    assert (aggregate["served_agent"], aggregate["served_provider"], aggregate["served_model"]) == (
        "codex", "openai", "gpt-6-sol")


def test_route_phase_projects_as_route_task_type(tmp_path):
    common = {"task_id": "route-phase", "state_root": tmp_path,
              "session_id": "session-1", "repo": "repo", "branch": "main",
              "agent_id": "call-1", "agent_type": "codex-main"}
    telemetry.emit("TASK_ROUTE_SELECTED", **common, phase="implementation", route="normal",
                   model="gpt-6-sol", effort="medium", routing_enforced=True,
                   task_type="implementation")
    telemetry.update_task("route-phase", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "route-phase.json").read_text(encoding="utf-8"))
    assert (aggregate["task_type"], aggregate["task_type_source"]) == ("implementation", "route")


def test_reply_usage_is_summed_once_per_finished_call(tmp_path):
    common = {"task_id": "usage-task", "state_root": tmp_path,
              "session_id": "session-1", "repo": "repo", "branch": "main",
              "agent_id": "call-1", "agent_type": "codex-main"}
    for index, tokens in enumerate((10, 5)):
        telemetry.emit("IMPLEMENTER_STARTED", **common, phase="implementation", route="normal",
                       model="gpt-6-sol", effort="medium", routing_enforced=True,
                       fresh_thread=index == 0, reply_call=index > 0)
        telemetry.emit("IMPLEMENTER_FINISHED", **common, duration_ms=1, implementer="codex-main",
                       served_model="gpt-6-sol", codex_usage_known=True,
                       codex_input_tokens=tokens)
    telemetry.update_task("usage-task", state_root=tmp_path)
    telemetry.update_task("usage-task", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "usage-task.json").read_text(encoding="utf-8"))
    assert aggregate["usage"]["codex_input_tokens"] == 15
    assert aggregate["usage"]["codex_call_count"] == 2


def test_missing_codex_usage_remains_null_and_unknown(tmp_path):
    _emit_task(tmp_path, "no-usage")
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "no-usage.json").read_text(encoding="utf-8"))
    assert aggregate["usage"]["codex_usage_known"] is False
    assert aggregate["usage"]["codex_input_tokens"] is None


def test_task_without_code_map_event_keeps_code_map_unknown(tmp_path):
    telemetry.emit(
        "IMPLEMENTER_FINISHED", task_id="no-code-map-event", state_root=tmp_path,
        session_id="session-1", duration_ms=12.5, implementer="codex-main",
    )
    telemetry.update_task("no-code-map-event", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "no-code-map-event.json").read_text(encoding="utf-8"))
    assert aggregate["has_code_map"] is None


def test_task_fold_keeps_routed_type_and_latest_claude_decision(tmp_path):
    common = {"task_id": "decision-latest", "state_root": tmp_path,
              "session_id": "session-1", "repo": "repo", "branch": "main",
              "agent_id": "call-1", "agent_type": "codex-main"}
    telemetry.emit("TASK_ROUTE_SELECTED", **common, phase="operation", route="operation",
                   model="gpt-6-sol", effort="medium", routing_enforced=True,
                   task_type="counting_status")
    _decision(tmp_path, "send_back", {"kind": "reply", "job_id": "reply-1"},
              task_id="decision-latest", task_type="operation")
    _decision(tmp_path, "accepted", {"kind": "apply", "job_id": "apply-1"},
              task_id="decision-latest", task_type="operation")
    telemetry.update_task("decision-latest", state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "decision-latest.json").read_text(encoding="utf-8"))
    assert aggregate["task_type"] == "counting_status"
    assert aggregate["task_type_source"] == "route"
    assert aggregate["claude_decision"]["decision"] == "accepted"


def test_conflicting_claude_terminal_decisions_project_unknown_and_warning(tmp_path):
    _decision(tmp_path, "accepted", {"kind": "apply", "job_id": "apply-1"})
    _decision(tmp_path, "rejected", {"kind": "discard", "job_id": "discard-1"})
    telemetry.rebuild(state_root=tmp_path)
    telemetry.rebuild(state_root=tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "decision-task.json").read_text(encoding="utf-8"))
    assert aggregate["claude_acceptance"]["outcome"] == "unknown"
    assert sum(event["event_type"] == "TELEMETRY_WARNING" and
               event["payload"]["warning_code"] == "CLAUDE_DECISION_CONFLICT"
               for event in _events(tmp_path)) == 1


def test_task_without_claude_decisions_projects_unknown(tmp_path):
    _emit_task(tmp_path)
    aggregate = json.loads((tmp_path / "projects" / "unresolved" / "telemetry" /
                            "tasks" / "task-1.json").read_text(encoding="utf-8"))
    assert aggregate["task_type"] == "unknown"
    assert aggregate["claude_acceptance"] == {
        "outcome": "unknown", "send_backs": 0, "applied_as_is": False, "sources": []
    }


def _run_task_process(state_root, action, task_id):
    script = """
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import telemetry

root = Path(sys.argv[2])
action = sys.argv[3]
task_id = sys.argv[4]
common = {
    "task_id": task_id,
    "state_root": root,
    "session_id": action,
    "repo": "repo",
    "branch": "main",
    "agent_id": action,
    "agent_type": "codex-main",
}
if action in {"pretool", "route"}:
    telemetry.emit(
        "TASK_ROUTE_SELECTED",
        **common,
        phase="implementation",
        route="consequential",
        model="gpt-6-sol",
        effort="high",
        routing_enforced=True,
    )
if action == "pretool":
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        **common,
        phase="implementation",
        route="consequential",
        model="gpt-6-sol",
        effort="high",
        routing_enforced=True,
        fresh_thread=True,
        reply_call=True,
    )
elif action == "started":
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        **common,
        phase="implementation",
        route="consequential",
        model="gpt-6-sol",
        effort="high",
        routing_enforced=True,
        fresh_thread=True,
        reply_call=True,
    )
elif action == "finished":
    telemetry.emit(
        "IMPLEMENTER_FINISHED",
        **common,
        duration_ms=12.5,
        changed_paths_count=2,
        validation="PASS",
        thread_id="thread-1",
        claude_review_mode="AUDIT",
        routing_enforced=True,
        codex_usage_known=False,
    )
elif action == "routing":
    telemetry.emit(
        "ROUTING_PROTOCOL_VIOLATION",
        **common,
        violation_code="ROUTE_MISMATCH",
    )
elif action == "continuity":
    telemetry.emit(
        "THREAD_CONTINUITY_VIOLATION",
        **common,
        violation_code="THREAD_ID_MISMATCH",
    )
elif action == "update":
    telemetry.update_task(task_id, state_root=root, projection_enabled=False)
"""
    subprocess.run(
        [sys.executable, "-c", script, str(PACKAGE_ROOT), str(state_root), action, task_id],
        check=True,
    )


def _aggregate(state_root, task_id):
    return json.loads(
        (
            state_root
            / "projects"
            / UNRESOLVED_PROJECT_KEY
            / "telemetry"
            / "tasks"
            / f"{task_id}.json"
        ).read_text(encoding="utf-8")
    )


def _write_claude_snapshot(state_root, **changes):
    captured_at = datetime.now(timezone.utc)
    snapshot = {
        "schema_version": 1,
        "captured_at": captured_at.isoformat().replace("+00:00", "Z"),
        "session_id": "claude-session-1",
        "context_used_percentage": 41.2,
        "context_window_size": 200000,
        "context_total_tokens": 7300,
        "context_input_tokens": 1200,
        "context_output_tokens": 800,
        "cache_creation_input_tokens": 300,
        "cache_read_input_tokens": 5000,
        "session_estimated_cost_usd": 1.23,
        "session_total_duration_ms": 456789,
        "five_hour": {"used_percentage": 34.0, "resets_at": "2099-08-30T15:00:00Z"},
        "seven_day": {"used_percentage": 19.0, "resets_at": "2099-09-06T15:00:00Z"},
        "rate_limit_delta_scope": "account_window_observation",
    }
    snapshot.update(changes)
    path = state_root / "telemetry" / "latest" / "claude_usage.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(snapshot), encoding="utf-8")


def _emit_started_or_finished(state_root, event_type, task_id="claude-task"):
    common = {
        "task_id": task_id,
        "state_root": state_root,
        "session_id": "orchestrator-session",
        "repo": "repo",
        "branch": "main",
        "agent_id": "call-1",
        "agent_type": "codex-main",
    }
    if event_type == "IMPLEMENTER_STARTED":
        telemetry.emit(
            event_type,
            **common,
            phase="implementation",
            route="normal",
            model="gpt-6-sol",
            effort="medium",
            routing_enforced=True,
            fresh_thread=True,
            reply_call=False,
        )
    else:
        telemetry.emit(
            event_type,
            **common,
            duration_ms=12.5,
            changed_paths_count=1,
            validation="PASS",
            thread_id="thread-1",
            claude_review_mode="AUDIT",
            routing_enforced=True,
            codex_usage_known=False,
        )


def test_absent_claude_snapshot_stays_unknown_and_never_zero(tmp_path):
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED")
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED")
    telemetry.update_task("claude-task", state_root=tmp_path)

    usage = _aggregate(tmp_path, "claude-task")["usage"]
    for field in (
        "claude_5h_start_pct",
        "claude_5h_end_pct",
        "claude_5h_delta_pct",
        "claude_7d_start_pct",
        "claude_7d_end_pct",
        "claude_context_used_percentage",
        "claude_session_estimated_cost_usd",
    ):
        assert usage[field] is None
    assert usage["claude_usage_known"] is False
    assert usage["claude_rate_limit_delta_scope"] == "account_window_observation"


def test_present_claude_snapshot_is_folded_and_rebuilt_identically(tmp_path):
    _write_claude_snapshot(tmp_path)
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED")
    _write_claude_snapshot(
        tmp_path,
        context_used_percentage=47.0,
        session_estimated_cost_usd=1.8,
        five_hour={"used_percentage": 39.0, "resets_at": "2099-08-30T15:00:00Z"},
        seven_day={"used_percentage": 24.0, "resets_at": "2099-09-06T15:00:00Z"},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED")
    telemetry.update_task("claude-task", state_root=tmp_path)

    events = _events(tmp_path)
    start = next(event for event in events if event["event_type"] == "IMPLEMENTER_STARTED")
    end = next(event for event in events if event["event_type"] == "IMPLEMENTER_FINISHED")
    assert start["payload"]["claude_5h_start_pct"] == 34.0
    assert end["payload"]["claude_5h_end_pct"] == 39.0
    usage = _aggregate(tmp_path, "claude-task")["usage"]
    assert usage["claude_5h_start_pct"] == 34.0
    assert usage["claude_5h_end_pct"] == 39.0
    assert usage["claude_5h_delta_pct"] == 5.0
    assert usage["claude_7d_start_pct"] == 19.0
    assert usage["claude_7d_end_pct"] == 24.0
    assert usage["_claude_5h_start_resets_at"] == "2099-08-30T15:00:00Z"
    assert usage["_claude_5h_end_resets_at"] == "2099-08-30T15:00:00Z"
    assert usage["claude_context_used_percentage"] == 47.0
    assert usage["claude_session_estimated_cost_usd"] == 1.8
    assert usage["claude_usage_known"] is True
    assert usage["claude_rate_limit_delta_scope"] == "account_window_observation"

    incremental = _aggregate(tmp_path, "claude-task")
    telemetry.rebuild(state_root=tmp_path)
    assert _aggregate(tmp_path, "claude-task") == incremental


def test_different_claude_snapshot_sessions_keep_endpoints_but_not_delta(tmp_path):
    _write_claude_snapshot(tmp_path)
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "different-session")
    _write_claude_snapshot(tmp_path, session_id="claude-session-2")
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED", "different-session")
    telemetry.update_task("different-session", state_root=tmp_path)

    usage = _aggregate(tmp_path, "different-session")["usage"]
    assert usage["claude_5h_start_pct"] == 34.0
    assert usage["claude_5h_end_pct"] == 34.0
    assert usage["claude_5h_delta_pct"] is None
    assert usage["claude_usage_known"] is True


def test_different_claude_five_hour_resets_keep_endpoints_but_not_delta(tmp_path):
    now = datetime.now(timezone.utc)
    start_captured_at = (now - timedelta(seconds=60)).isoformat().replace("+00:00", "Z")
    end_captured_at = (now - timedelta(seconds=30)).isoformat().replace("+00:00", "Z")
    start_reset_at = "2099-08-30T16:00:00Z"
    end_reset_at = "2099-08-30T21:00:00Z"

    _write_claude_snapshot(
        tmp_path,
        captured_at=start_captured_at,
        five_hour={"used_percentage": 80.0, "resets_at": start_reset_at},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "different-resets")
    _write_claude_snapshot(
        tmp_path,
        captured_at=end_captured_at,
        five_hour={"used_percentage": 3.0, "resets_at": end_reset_at},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED", "different-resets")
    telemetry.update_task("different-resets", state_root=tmp_path)

    events = _events(tmp_path)
    start = next(event for event in events if event["event_type"] == "IMPLEMENTER_STARTED")
    end = next(event for event in events if event["event_type"] == "IMPLEMENTER_FINISHED")
    assert start["payload"]["claude_5h_resets_at"] == start_reset_at
    assert end["payload"]["claude_5h_resets_at"] == end_reset_at

    incremental = _aggregate(tmp_path, "different-resets")
    usage = incremental["usage"]
    assert usage["claude_5h_start_pct"] == 80.0
    assert usage["claude_5h_end_pct"] == 3.0
    assert usage["claude_5h_delta_pct"] is None
    assert usage["_claude_5h_start_resets_at"] == start_reset_at
    assert usage["_claude_5h_end_resets_at"] == end_reset_at

    telemetry.rebuild(state_root=tmp_path)
    assert _aggregate(tmp_path, "different-resets") == incremental


def test_missing_claude_five_hour_reset_keeps_existing_delta_behavior(tmp_path):
    _write_claude_snapshot(
        tmp_path,
        five_hour={"used_percentage": 34.0, "resets_at": None},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "missing-reset")
    _write_claude_snapshot(
        tmp_path,
        five_hour={"used_percentage": 39.0, "resets_at": "2099-08-30T15:00:00Z"},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED", "missing-reset")
    telemetry.update_task("missing-reset", state_root=tmp_path)

    usage = _aggregate(tmp_path, "missing-reset")["usage"]
    assert usage["claude_5h_start_pct"] == 34.0
    assert usage["claude_5h_end_pct"] == 39.0
    assert usage["claude_5h_delta_pct"] == 5.0
    assert usage["_claude_5h_start_resets_at"] is None
    assert usage["_claude_5h_end_resets_at"] == "2099-08-30T15:00:00Z"


def test_stale_claude_snapshot_is_not_attributed(tmp_path):
    stale = (datetime.now(timezone.utc) - timedelta(seconds=telemetry.CLAUDE_USAGE_MAX_STALENESS_SECONDS + 1))
    _write_claude_snapshot(tmp_path, captured_at=stale.isoformat().replace("+00:00", "Z"))
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "stale-session")
    telemetry.update_task("stale-session", state_root=tmp_path)

    usage = _aggregate(tmp_path, "stale-session")["usage"]
    assert usage["claude_5h_start_pct"] is None
    assert usage["claude_context_used_percentage"] is None
    assert usage["claude_usage_known"] is False


def test_claude_window_reset_does_not_become_consumption(tmp_path):
    now = datetime.now(timezone.utc)
    before_reset = (now - timedelta(seconds=60)).isoformat().replace("+00:00", "Z")
    reset_at = (now - timedelta(seconds=1)).isoformat().replace("+00:00", "Z")
    _write_claude_snapshot(
        tmp_path,
        captured_at=before_reset,
        five_hour={"used_percentage": 85.0, "resets_at": reset_at},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "reset-session")
    _write_claude_snapshot(
        tmp_path,
        five_hour={"used_percentage": 5.0, "resets_at": None},
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED", "reset-session")
    telemetry.update_task("reset-session", state_root=tmp_path)

    usage = _aggregate(tmp_path, "reset-session")["usage"]
    assert usage["claude_5h_start_pct"] == 85.0
    assert usage["claude_5h_end_pct"] == 5.0
    assert usage["claude_5h_delta_pct"] is None
    assert usage["_claude_5h_start_resets_at"] == reset_at
    assert usage["_claude_5h_end_resets_at"] is None


def test_malformed_and_partially_null_claude_snapshots_are_safe(tmp_path):
    path = tmp_path / "telemetry" / "latest" / "claude_usage.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("not-json", encoding="utf-8")
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "malformed-session")

    _write_claude_snapshot(
        tmp_path,
        context_used_percentage=None,
        context_window_size=None,
        context_total_tokens=None,
        context_input_tokens=12,
        context_output_tokens=None,
        five_hour={"used_percentage": None, "resets_at": None},
        seven_day=None,
    )
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_FINISHED", "malformed-session")
    telemetry.update_task("malformed-session", state_root=tmp_path)

    usage = _aggregate(tmp_path, "malformed-session")["usage"]
    assert usage["claude_usage_known"] is True
    assert usage["claude_context_used_percentage"] is None
    assert usage["claude_5h_start_pct"] is None
    assert usage["claude_5h_end_pct"] is None


def test_jsonl_lines_are_valid_unique_and_unknown_usage_is_null(tmp_path):
    _emit_task(tmp_path)

    events = _events(tmp_path)
    assert len(events) == 3
    assert all(schema.valid_event(event) for event in events)
    assert len({event["event_id"] for event in events}) == len(events)

    aggregate = json.loads(
        (
            tmp_path
            / "projects"
            / UNRESOLVED_PROJECT_KEY
            / "telemetry"
            / "tasks"
            / "task-1.json"
        ).read_text(encoding="utf-8")
    )
    assert aggregate["usage"]["codex_usage_known"] is False
    assert aggregate["usage"]["codex_input_tokens"] is None
    assert aggregate["usage"]["gemini_usage_known"] is False
    assert aggregate["usage"]["deepseek_usage_known"] is False
    assert 0 not in [
        value
        for key, value in aggregate["usage"].items()
        if key.endswith("_tokens") and value is not None
    ]


def test_events_from_another_process_are_folded(tmp_path):
    _run_task_process(tmp_path, "pretool", "cross-process")
    _run_task_process(tmp_path, "update", "cross-process")

    aggregate = _aggregate(tmp_path, "cross-process")
    assert aggregate["route"] == "consequential"
    assert aggregate["model"] == "gpt-6-sol"
    assert aggregate["started_at"] is not None
    assert aggregate["fresh_threads"] == 1
    assert aggregate["reply_calls"] == 1


def test_repeated_update_is_idempotent_for_scalars_and_counters(tmp_path):
    for action in ("route", "started", "routing", "continuity"):
        _run_task_process(tmp_path, action, "idempotent")
    _run_task_process(tmp_path, "update", "idempotent")
    first = _aggregate(tmp_path, "idempotent")

    _run_task_process(tmp_path, "update", "idempotent")
    assert _aggregate(tmp_path, "idempotent") == first
    assert first["fresh_threads"] == 1
    assert first["reply_calls"] == 1
    assert first["routing_protocol_violations"] == 1
    assert first["thread_continuity_violations"] == 1


def test_incremental_aggregate_equals_rebuild_field_for_field(tmp_path):
    for action in ("route", "started"):
        _run_task_process(tmp_path, action, "same-as-rebuild")
    _run_task_process(tmp_path, "update", "same-as-rebuild")
    for action in ("finished", "routing", "continuity"):
        _run_task_process(tmp_path, action, "same-as-rebuild")
    _run_task_process(tmp_path, "update", "same-as-rebuild")
    incremental = _aggregate(tmp_path, "same-as-rebuild")
    assert {field: incremental[field] for field in ("repo", "branch", "validation")} == {
        "repo": "repo",
        "branch": "main",
        "validation": "PASS",
    }

    telemetry.rebuild(state_root=tmp_path)
    rebuilt = _aggregate(tmp_path, "same-as-rebuild")
    assert incremental == rebuilt
    assert {field: incremental[field] for field in ("repo", "branch", "validation")} == {
        field: rebuilt[field] for field in ("repo", "branch", "validation")
    }


def test_first_known_repository_branch_and_validation_values_are_stable(tmp_path):
    common = {
        "task_id": "stable-values",
        "state_root": tmp_path,
        "session_id": "session",
        "agent_id": "call",
        "agent_type": "codex-main",
    }
    telemetry.emit(
        "TASK_ROUTE_SELECTED",
        **common,
        repo="repo-start",
        branch="main",
        phase="implementation",
        route="normal",
        model="gpt-6-sol",
        effort="medium",
        routing_enforced=True,
    )
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        **common,
        repo="repo-later",
        branch="feature",
        phase="implementation",
        route="normal",
        model="gpt-6-sol",
        effort="medium",
        routing_enforced=True,
    )
    for validation in ("FAIL", "PASS"):
        telemetry.emit(
            "IMPLEMENTER_FINISHED",
            **common,
            repo="repo-later",
            branch="feature",
            duration_ms=1,
            changed_paths_count=0,
            validation=validation,
            thread_id="thread",
            claude_review_mode="AUDIT",
            routing_enforced=True,
            codex_usage_known=False,
        )
    telemetry.update_task("stable-values", state_root=tmp_path)

    aggregate = _aggregate(tmp_path, "stable-values")
    assert aggregate["repo"] == "repo-start"
    assert aggregate["branch"] == "main"
    assert aggregate["validation"] == "FAIL"


def test_event_without_branch_does_not_erase_known_branch(tmp_path):
    _emit_started_or_finished(tmp_path, "IMPLEMENTER_STARTED", "known-branch")
    telemetry.emit(
        "IMPLEMENTER_FINISHED",
        task_id="known-branch",
        state_root=tmp_path,
        session_id="orchestrator-session",
        repo="repo",
        branch=None,
        agent_id="call-1",
        agent_type="codex-main",
        duration_ms=12.5,
        changed_paths_count=1,
        validation="PASS",
        thread_id="thread-1",
        claude_review_mode="AUDIT",
        routing_enforced=True,
        codex_usage_known=False,
    )
    telemetry.update_task("known-branch", state_root=tmp_path)

    assert _aggregate(tmp_path, "known-branch")["branch"] == "main"


def test_concurrent_process_append_never_corrupts_jsonl(tmp_path):
    script = (
        "import sys; from pathlib import Path; "
        f"sys.path.insert(0, {str(PACKAGE_ROOT)!r}); import telemetry; "
        "telemetry.emit('IMPLEMENTER_FAILED', task_id=sys.argv[2], state_root=Path(sys.argv[1]), "
        "session_id='shared', agent_type='codex-main', failure_code='CODEX_CALL_FAILED')"
    )
    processes = [
        subprocess.Popen([sys.executable, "-c", script, str(tmp_path), f"task-{index}"])
        for index in range(2)
    ]
    assert [process.wait(timeout=15) for process in processes] == [0, 0]
    telemetry.emit(
        "TELEMETRY_WARNING",
        state_root=tmp_path,
        session_id="merge",
        warning_code="MALFORMED_VERDICT",
    )

    events = _events(tmp_path)
    failures = [event for event in events if event["event_type"] == "IMPLEMENTER_FAILED"]
    assert len(failures) == 2
    assert all(schema.valid_event(event) for event in events)


def test_lock_timeout_spools_and_recovery_is_idempotent(tmp_path, monkeypatch):
    real_lock = telemetry._try_lock

    @contextmanager
    def unavailable_lock(_root):
        yield False

    monkeypatch.setattr(telemetry, "_try_lock", unavailable_lock)
    telemetry.emit(
        "IMPLEMENTER_FAILED",
        task_id="spooled",
        state_root=tmp_path,
        session_id="session",
        failure_code="CODEX_CALL_FAILED",
    )
    spool_file = next((tmp_path / "telemetry" / "events" / "spool").glob("*.json"))
    spooled = json.loads(spool_file.read_text(encoding="utf-8"))

    monkeypatch.setattr(telemetry, "_try_lock", real_lock)
    telemetry.emit(
        "TELEMETRY_WARNING",
        state_root=tmp_path,
        session_id="merge-1",
        warning_code="MALFORMED_VERDICT",
    )
    assert not spool_file.exists()
    spool_file.write_text(json.dumps(spooled), encoding="utf-8")
    telemetry.emit(
        "TELEMETRY_WARNING",
        state_root=tmp_path,
        session_id="merge-2",
        warning_code="MALFORMED_VERDICT",
    )

    assert sum(event["event_id"] == spooled["event_id"] for event in _events(tmp_path)) == 1


def test_spooled_event_folded_before_merge_is_not_folded_again(tmp_path, monkeypatch):
    real_lock = telemetry._try_lock

    @contextmanager
    def unavailable_lock(_root):
        yield False

    monkeypatch.setattr(telemetry, "_try_lock", unavailable_lock)
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        task_id="spooled-update",
        state_root=tmp_path,
        session_id="session",
        phase="implementation",
        route="normal",
        model="gpt-6-sol",
        effort="medium",
        routing_enforced=True,
        fresh_thread=True,
        reply_call=True,
    )
    telemetry.update_task("spooled-update", state_root=tmp_path)
    before_merge = _aggregate(tmp_path, "spooled-update")

    monkeypatch.setattr(telemetry, "_try_lock", real_lock)
    telemetry.emit(
        "TELEMETRY_WARNING",
        state_root=tmp_path,
        session_id="merge",
        warning_code="MALFORMED_VERDICT",
    )
    telemetry.update_task("spooled-update", state_root=tmp_path)

    assert _aggregate(tmp_path, "spooled-update") == before_merge


@pytest.mark.parametrize("task_id", ["../escape", r"C:\absolute\escape", "CON"])
def test_task_id_normalization_cannot_escape_telemetry(tmp_path, task_id):
    normalized = schema.normalize_identifier(task_id)
    assert normalized != task_id
    assert normalized.endswith(hashlib.sha256(task_id.encode("utf-8")).hexdigest()[:8])
    telemetry.emit(
        "IMPLEMENTER_FAILED",
        task_id=task_id,
        state_root=tmp_path,
        session_id=task_id,
        failure_code="CODEX_CALL_FAILED",
    )
    telemetry.update_task(task_id, state_root=tmp_path)

    task_path = (
        tmp_path
        / "projects"
        / UNRESOLVED_PROJECT_KEY
        / "telemetry"
        / "tasks"
        / f"{normalized}.json"
    ).resolve()
    assert task_path.is_file()
    task_path.relative_to((tmp_path / "projects" / UNRESOLVED_PROJECT_KEY / "telemetry").resolve())
    telemetry_files = [path for path in tmp_path.rglob("*") if path.is_file()]
    assert all(
        path.resolve().is_relative_to(
            (tmp_path / "projects" / UNRESOLVED_PROJECT_KEY / "telemetry").resolve()
        )
        or path.resolve().is_relative_to((tmp_path / "telemetry").resolve())
        for path in telemetry_files
    )


def test_rebuild_reproduces_aggregate_and_folds_verdict(tmp_path):
    _emit_task(tmp_path)
    task_path = (
        tmp_path / "projects" / UNRESOLVED_PROJECT_KEY / "telemetry" / "tasks" / "task-1.json"
    )
    before = json.loads(task_path.read_text(encoding="utf-8"))
    telemetry.rebuild(state_root=tmp_path)
    assert json.loads(task_path.read_text(encoding="utf-8")) == before

    verdict_path = (
        tmp_path / "projects" / UNRESOLVED_PROJECT_KEY / "telemetry" / "verdicts" / "task-1.json"
    )
    verdict_path.parent.mkdir(parents=True)
    verdict_path.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "task_id": "task-1",
                "verdict": "VERIFIED",
                "recorded_at": "2026-08-30T08:00:00Z",
            }
        ),
        encoding="utf-8",
    )
    telemetry.update_task("task-1", state_root=tmp_path)
    aggregate = json.loads(task_path.read_text(encoding="utf-8"))
    assert aggregate["status"] == "VERIFIED"
    assert aggregate["ended_at"] == "2026-08-30T08:00:00Z"


def test_rebuild_preserves_aggregate_without_raw_events(tmp_path):
    task_path = (
        tmp_path / "projects" / UNRESOLVED_PROJECT_KEY / "telemetry" / "tasks" / "remote-only.json"
    )
    task_path.parent.mkdir(parents=True)
    aggregate = {
        "task_id": "remote-only",
        "status": "VERIFIED",
        "policy_fingerprint": "frozen-fingerprint",
        "policy_hashes": {"hook_sha256": "frozen-hook-hash"},
    }
    task_path.write_text(json.dumps(aggregate), encoding="utf-8")

    telemetry.rebuild(state_root=tmp_path)

    assert json.loads(task_path.read_text(encoding="utf-8")) == aggregate


def test_hook_event_identity_and_repository_branch_are_observed(tmp_path):
    git_dir = tmp_path / ".git"
    git_dir.mkdir()
    (git_dir / "HEAD").write_text("ref: refs/heads/feature/telemetry\n", encoding="utf-8")
    envelope = codex._telemetry_envelope(
        {
            "session_id": "session",
            "tool_use_id": "tool-call-id",
            "agent_id": "agent-7",
            "agent_type": "reviewer",
        },
        tmp_path,
    )
    assert envelope == {
        "session_id": "session",
        "project_key": codex.project_root.project_key(tmp_path),
        "repo": tmp_path.name,
        "branch": "feature/telemetry",
        "agent_id": "agent-7",
        "agent_type": "reviewer",
    }

    absent = codex._telemetry_envelope(
        {"session_id": "session", "tool_use_id": "tool-call-id"}, tmp_path
    )
    assert absent["agent_id"] is None
    assert absent["agent_type"] is None
    assert "tool-call-id" not in absent.values()


def test_repository_branch_is_null_for_detached_or_unreadable_head(tmp_path, monkeypatch):
    head_path = tmp_path / ".git" / "HEAD"
    head_path.parent.mkdir()
    head_path.write_text("1e85bd1f4fa2d73c1ae011388c3504663a892db5\n", encoding="utf-8")
    assert codex._telemetry_envelope({}, tmp_path)["branch"] is None

    original_open = Path.open

    def unreadable_head(path, *args, **kwargs):
        if path == head_path:
            raise PermissionError("unreadable")
        return original_open(path, *args, **kwargs)

    monkeypatch.setattr(Path, "open", unreadable_head)
    assert codex._telemetry_envelope({}, tmp_path)["branch"] is None


def test_pending_cleanup_is_conservative_and_preserves_current_call(tmp_path, monkeypatch):
    event = {
        "session_id": "session",
        "tool_name": codex.CODEX_TOOL,
        "tool_use_id": "current-call",
        "tool_input": {"prompt": "current"},
    }
    pending_root = tmp_path / "pending"
    pending_root.mkdir()
    young = pending_root / "young.json"
    old = pending_root / "old.json"
    unknown = pending_root / "unknown.json"
    current = codex._pending_path(tmp_path, event)
    for path in (young, old, unknown, current):
        path.write_text("{}", encoding="utf-8")
    old_time = time.time() - codex.PENDING_MAX_AGE_SECONDS - 60
    os.utime(old, (old_time, old_time))
    os.utime(current, (old_time, old_time))

    original_stat = Path.stat

    def unknown_age(path, *args, **kwargs):
        if path == unknown:
            raise OSError("age unavailable")
        return original_stat(path, *args, **kwargs)

    monkeypatch.setattr(Path, "stat", unknown_age)
    codex._cleanup_stale_pending(tmp_path, event)

    assert young.exists()
    assert not old.exists()
    assert current.exists()
    assert unknown.read_text(encoding="utf-8") == "{}"


def test_malformed_verdict_is_ignored_and_warned(tmp_path):
    _emit_task(tmp_path, "bad-verdict")
    verdict_path = (
        tmp_path
        / "projects"
        / UNRESOLVED_PROJECT_KEY
        / "telemetry"
        / "verdicts"
        / "bad-verdict.json"
    )
    verdict_path.parent.mkdir(parents=True)
    verdict_path.write_text('{"verdict":"PASS"}', encoding="utf-8")
    telemetry.update_task("bad-verdict", state_root=tmp_path)

    aggregate = json.loads(
        (
            tmp_path
            / "projects"
            / UNRESOLVED_PROJECT_KEY
            / "telemetry"
            / "tasks"
            / "bad-verdict.json"
        ).read_text(encoding="utf-8")
    )
    assert aggregate["status"] == "IN_PROGRESS"
    assert any(
        event["event_type"] == "TELEMETRY_WARNING"
        and event["payload"] == {"warning_code": "MALFORMED_VERDICT"}
        for event in _events(tmp_path)
    )


def test_reviewer_usage_is_carried_without_changing_public_mapping():
    stdout = json.dumps(
        {
            "conversation_id": "conversation-1",
            "status": "SUCCESS",
            "response": "unused prose",
            "structured_output": {
                "result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
                "validation": "PASS",
                "findings": [],
            },
            "duration_seconds": 1.25,
            "num_turns": 1,
            "usage": {"input_tokens": 10, "output_tokens": 5, "unsafe_text": "secret"},
        }
    )
    public = reviewer.parse_envelope(stdout)
    enriched = reviewer.parse_envelope(stdout, include_metadata=True)

    assert len(public) == 2
    assert enriched[2:] == (1.25, {"input_tokens": 10, "output_tokens": 5})
    outcome = reviewer.CompletedReview(
        result="NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
        validation="PASS",
        findings=(),
        conversation_id="conversation-1",
        reviewer1_model_requested="gemini-3.7-flash-high",
        duration_seconds=enriched[2],
        usage=tuple(enriched[3].items()),
    )
    assert "duration_seconds" not in dict(outcome)
    assert "usage" not in dict(outcome)


def test_forbidden_content_cannot_reach_any_telemetry_file(tmp_path):
    forbidden = [
        "prompt body api_" + "key=" + "abcdefghijklmnopqrstuvwxyz012345",
        "raw Codex output",
        "diff --git a/secret.py b/secret.py",
        "arbitrary exception credential text",
    ]
    for key, value in zip(("prompt", "output", "diff", "exception"), forbidden):
        telemetry.emit(
            "TELEMETRY_WARNING",
            state_root=tmp_path,
            warning_code="MALFORMED_VERDICT",
            **{key: value},
        )
    persisted = "\n".join(
        path.read_text(encoding="utf-8", errors="replace")
        for path in (tmp_path / "telemetry").rglob("*")
        if path.is_file()
    )
    assert not any(value in persisted for value in forbidden)


@pytest.mark.parametrize("entrypoint", ["emit", "update_task", "record_health", "rebuild"])
def test_every_telemetry_entrypoint_is_total_on_forced_failure(tmp_path, monkeypatch, entrypoint):
    def fail(*_args, **_kwargs):
        raise OSError("arbitrary exception text must not escape")

    if entrypoint == "emit":
        monkeypatch.setattr(telemetry, "_append_event", fail)
        result = telemetry.emit(
            "IMPLEMENTER_FAILED",
            task_id="task",
            state_root=tmp_path,
            failure_code="CODEX_CALL_FAILED",
        )
    elif entrypoint == "update_task":
        monkeypatch.setattr(telemetry, "_build_task", fail)
        result = telemetry.update_task("task", state_root=tmp_path)
    elif entrypoint == "record_health":
        monkeypatch.setattr(telemetry, "_atomic_json", fail)
        result = telemetry.record_health("OK", state_root=tmp_path)
    else:
        monkeypatch.setattr(telemetry, "_raw_events", fail)
        result = telemetry.rebuild(state_root=tmp_path)
    assert result is None
    if entrypoint in {"emit", "update_task", "rebuild"}:
        health = json.loads(
            (tmp_path / "telemetry" / "telemetry_health.json").read_text(encoding="utf-8")
        )
        assert health["status"] == "DEGRADED"
        assert health["failure_count"] >= 1
        assert "arbitrary exception text" not in json.dumps(health)


def test_hook_outputs_are_identical_when_telemetry_raises(tmp_path, monkeypatch):
    denial_event = {
        "hook_event_name": "PreToolUse",
        "tool_name": codex.CODEX_TOOL,
        "tool_use_id": "call-denied",
        "session_id": "session",
        "tool_input": {"prompt": "missing protocol"},
    }
    monkeypatch.setattr(codex, "_observe", lambda *_args, **_kwargs: None)
    expected_denial = codex.pre_tool_use(denial_event, state_root=tmp_path)
    monkeypatch.undo()
    monkeypatch.setattr(telemetry, "emit", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    monkeypatch.setattr(telemetry, "record_health", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    assert codex.pre_tool_use(denial_event, state_root=tmp_path) == expected_denial

    post_event = {
        "hook_event_name": "PostToolUse",
        "tool_name": codex.CODEX_TOOL,
        "tool_use_id": "call-post",
        "session_id": "session",
        "tool_response": json.dumps({"threadId": "thread-1", "content": "done"}),
    }
    pending = {
        "kind": "pretool_bypass",
        "reason": "fixed",
        "task_id": "task",
        "phase": "implementation",
        "route": "normal",
        "model": "gpt-6-sol",
        "effort": "medium",
        "repo_root": str(tmp_path),
    }
    pending_path = codex._bypass_path(tmp_path, post_event)
    codex._atomic_json_write(pending_path, pending)
    monkeypatch.setattr(codex, "_observe", lambda *_args, **_kwargs: None)
    expected_post = codex.post_tool_use(post_event, state_root=tmp_path)
    codex._atomic_json_write(pending_path, pending)
    monkeypatch.setattr(codex, "_observe", codex.__dict__["_observe"])
    # Call the real wrapper directly after undoing only the temporary no-op.
    monkeypatch.undo()
    codex._atomic_json_write(pending_path, pending)
    monkeypatch.setattr(telemetry, "emit", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    monkeypatch.setattr(telemetry, "update_task", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    monkeypatch.setattr(telemetry, "record_health", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    assert codex.post_tool_use(post_event, state_root=tmp_path) == expected_post


def test_task_type_and_suspected_route_mismatch_schema():
    payload = {
        "phase": "implementation",
        "route": "subtle",
        "task_type": "counting_status",
        "suspected_route_mismatch": {
            "declared_route": "subtle",
            "derived_type": "counting_status",
        },
    }
    event = schema.build_event("TASK_ROUTE_SELECTED", task_id="task-1", payload=payload)
    assert schema.valid_event(event)
    finished = schema.build_event(
        "IMPLEMENTER_FINISHED",
        task_id="task-1",
        payload={
            "task_type": "counting_status",
            "suspected_route_mismatch": payload["suspected_route_mismatch"],
        },
    )
    assert schema.valid_event(finished)
