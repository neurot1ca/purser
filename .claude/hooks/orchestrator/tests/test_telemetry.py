import json
import os
from pathlib import Path
import shutil
import stat
import time
import uuid

import pytest


PACKAGE_ROOT = Path(__file__).parents[1]
import codex
import reviewer
import telemetry


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
    config_path.write_text(json.dumps({"obsidian_projection": {"enabled": False}}), encoding="utf-8")
    monkeypatch.setattr(telemetry, "_CONFIG_PATH", config_path)


def test_reviewer_failure_stderr_evidence_is_persisted(tmp_path):
    event = {"session_id": "session-1", "tool_use_id": "review-call-1"}
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


def test_rebuild_entrypoint_is_total_on_forced_failure(tmp_path, monkeypatch):
    def fail(*_args, **_kwargs):
        raise OSError("arbitrary exception text must not escape")

    monkeypatch.setattr(telemetry, "_raw_events", fail)
    assert telemetry.rebuild(state_root=tmp_path) is None
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
    monkeypatch.undo()
    codex._atomic_json_write(pending_path, pending)
    monkeypatch.setattr(telemetry, "emit", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    monkeypatch.setattr(telemetry, "update_task", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    monkeypatch.setattr(telemetry, "record_health", lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError()))
    assert codex.post_tool_use(post_event, state_root=tmp_path) == expected_post
