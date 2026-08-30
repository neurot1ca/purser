import json
from contextlib import contextmanager
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


@pytest.fixture()
def tmp_path():
    root = Path(__file__).parent / ".telemetry-test-tmp" / uuid.uuid4().hex
    root.mkdir(parents=True)
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
        "agent_id": "call-1",
        "agent_type": "codex-main",
    }
    telemetry.emit(
        "TASK_ROUTE_SELECTED",
        **common,
        phase="implementation",
        route="normal",
        model="gpt-5.6-sol",
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
        model="gpt-5.6-sol",
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
    "agent_id": action,
    "agent_type": "codex-main",
}
if action in {"pretool", "route"}:
    telemetry.emit(
        "TASK_ROUTE_SELECTED",
        **common,
        phase="implementation",
        route="consequential",
        model="gpt-5.6-sol",
        effort="high",
        routing_enforced=True,
    )
if action == "pretool":
    telemetry.emit(
        "IMPLEMENTER_STARTED",
        **common,
        phase="implementation",
        route="consequential",
        model="gpt-5.6-sol",
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
        model="gpt-5.6-sol",
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
    telemetry.update_task(task_id, state_root=root)
"""
    subprocess.run(
        [sys.executable, "-c", script, str(PACKAGE_ROOT), str(state_root), action, task_id],
        check=True,
    )


def _aggregate(state_root, task_id):
    return json.loads(
        (state_root / "telemetry" / "tasks" / f"{task_id}.json").read_text(encoding="utf-8")
    )


def test_jsonl_lines_are_valid_unique_and_unknown_usage_is_null(tmp_path):
    _emit_task(tmp_path)

    events = _events(tmp_path)
    assert len(events) == 3
    assert all(schema.valid_event(event) for event in events)
    assert len({event["event_id"] for event in events}) == len(events)

    aggregate = json.loads(
        (tmp_path / "telemetry" / "tasks" / "task-1.json").read_text(encoding="utf-8")
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
    assert aggregate["model"] == "gpt-5.6-sol"
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

    telemetry.rebuild(state_root=tmp_path)
    assert incremental == _aggregate(tmp_path, "same-as-rebuild")


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
        model="gpt-5.6-sol",
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

    task_path = (tmp_path / "telemetry" / "tasks" / f"{normalized}.json").resolve()
    assert task_path.is_file()
    task_path.relative_to((tmp_path / "telemetry").resolve())
    assert all(path.resolve().is_relative_to((tmp_path / "telemetry").resolve()) for path in tmp_path.rglob("*"))


def test_rebuild_reproduces_aggregate_and_folds_verdict(tmp_path):
    _emit_task(tmp_path)
    task_path = tmp_path / "telemetry" / "tasks" / "task-1.json"
    before = json.loads(task_path.read_text(encoding="utf-8"))
    telemetry.rebuild(state_root=tmp_path)
    assert json.loads(task_path.read_text(encoding="utf-8")) == before

    verdict_path = tmp_path / "telemetry" / "verdicts" / "task-1.json"
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
    task_path = tmp_path / "telemetry" / "tasks" / "remote-only.json"
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
    verdict_path = tmp_path / "telemetry" / "verdicts" / "bad-verdict.json"
    verdict_path.parent.mkdir(parents=True)
    verdict_path.write_text('{"verdict":"PASS"}', encoding="utf-8")
    telemetry.update_task("bad-verdict", state_root=tmp_path)

    aggregate = json.loads(
        (tmp_path / "telemetry" / "tasks" / "bad-verdict.json").read_text(encoding="utf-8")
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
        "model": "gpt-5.6-sol",
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
