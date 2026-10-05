"""Post implementer observation oracle and Go parity fixture."""
import json
import os
from pathlib import Path
import subprocess
import sys
from datetime import datetime, timezone
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "enforce" / "testdata" / "post_observations_python_fixture.json"
sys.path.insert(0, str(ORCH))
import codex
import telemetry
import telemetry_schema


CASES = [
    {"name": "implementation_delta_usage_claude", "pending": {"task_id": "post-impl", "project_key": "unresolved", "repo_root": None, "agent": "codex", "model": "openai/gpt-5", "effort": "high", "route": "normal", "phase": "implementation", "task_type": "implementation", "has_code_map": True, "eta_minutes": 15}, "bundle": {"model": "openai/gpt-5", "effort": "high", "delta_files": 2, "validation": "PASS", "routing_enforced": True, "thread_id": "thread-impl", "claude_review_mode": "FULL", "worker_validation": {"status": "PASS"}, "known_concurrent_scopes": []}, "response": {"structuredContent": {"usage": {"input_tokens": 11, "cached_input_tokens": 3, "output_tokens": 7, "reasoning_output_tokens": 2}}}, "claude": {"schema_version": 1, "captured_at": "NOW", "session_id": "claude-fixture", "context_used_percentage": 41.5, "context_window_size": 200000, "context_total_tokens": 8300, "context_input_tokens": 1200, "context_output_tokens": 800, "cache_creation_input_tokens": 300, "cache_read_input_tokens": 5000, "session_estimated_cost_usd": 1.2, "session_total_duration_ms": 456789, "five_hour": {"used_percentage": 35.0, "resets_at": "2099-10-05T15:00:00Z"}, "seven_day": {"used_percentage": 21.0, "resets_at": "2099-10-10T15:00:00Z"}}, "duration_ms": 123.5},
    {"name": "plan_validation_unknown_quota_absent", "pending": {"task_id": "post-plan", "project_key": "unresolved", "repo_root": None, "agent": "agy", "model": "gemini-2.5-pro", "effort": "medium", "route": "plan_standard", "phase": "plan", "task_type": "plan"}, "bundle": {"model": "gemini-2.5-pro", "effort": "medium", "delta_files": 0, "validation": "UNKNOWN", "routing_enforced": True, "thread_id": "thread-plan", "worker_validation": {"status": "UNKNOWN"}}, "response": {}, "duration_ms": 0},
    {"name": "bypass_unrouted", "pending": {"task_id": "post-bypass", "project_key": "unresolved", "repo_root": None, "agent": "codex", "model": "openai/gpt-5", "effort": "low", "route": "normal", "phase": "implementation", "task_type": "unknown"}, "bundle": {"model": "openai/gpt-5", "effort": "low", "delta_files": None, "validation": "UNKNOWN", "routing_enforced": False, "worker_validation": {"status": "UNKNOWN"}}, "response": {"usage": {"input_tokens": -1, "output_tokens": 0}}, "duration_ms": None},
]


def _python_fixture_case(case, state_root, captured_at, monkeypatch):
    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            return captured_at.astimezone(tz or timezone.utc)

    monkeypatch.setattr(telemetry, "datetime", FrozenDateTime)
    monkeypatch.setattr(telemetry_schema, "datetime", FrozenDateTime)
    monkeypatch.setattr(telemetry, "_policy_hashes", lambda: ({"skill_sha256": None, "config_sha256": None, "hook_sha256": None}, None))
    monkeypatch.setattr(telemetry_schema.uuid, "uuid4", lambda: uuid.UUID("10600000-0000-4000-8000-000000000001"))
    if "claude" in case:
        latest = state_root / "telemetry" / "latest"
        latest.mkdir(parents=True)
        claude = dict(case["claude"], captured_at=captured_at.isoformat().replace("+00:00", "Z"))
        (latest / "claude_usage.json").write_text(json.dumps(claude), encoding="utf-8")

    original_observe = codex._observe
    def observe(entrypoint, *args, **kwargs):
        if entrypoint == "emit" and args[:1] == ("IMPLEMENTER_FINISHED",):
            return telemetry.emit(*args, **kwargs)
        if entrypoint == "update_task":
            return telemetry.update_task(*args, **kwargs)
        return None
    monkeypatch.setattr(codex, "_observe", observe)
    event = {"session_id": "session-post-observations", "agent_id": "agent-post", "agent_type": "worker",
             "duration_ms": case["duration_ms"], "tool_response": case["response"]}
    codex._post_observations(event, state_root, case["pending"], case["bundle"])
    event_path = next((state_root / "telemetry" / "events").glob("????-??/session_*.jsonl"))
    event_line = event_path.read_text(encoding="utf-8").splitlines()[0]
    task_path = state_root / "projects" / "unresolved" / "telemetry" / "tasks" / f"{case['pending']['task_id']}.json"
    result = {"name": case["name"], "input": {key: case.get(key) for key in ("pending", "bundle", "response", "duration_ms", "claude")},
              "event_line": event_line, "task": json.loads(task_path.read_text(encoding="utf-8"))}
    monkeypatch.setattr(codex, "_observe", original_observe)
    return result


def test_post_observations_python_fixture_and_go_parity(tmp_path, monkeypatch):
    now = datetime(2026, 10, 5, 12, 0, tzinfo=timezone.utc)
    actual = []
    for index, case in enumerate(CASES):
        state = tmp_path / str(index) / "state"
        state.mkdir(parents=True)
        actual.append(_python_fixture_case(case, state, now, monkeypatch))
    fixture = {"oracle": "codex._post_observations and telemetry.update_task", "cases": actual}
    if os.environ.get("POST_OBSERVATIONS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set POST_OBSERVATIONS_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestPostImplementerObservationsP106$", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)
