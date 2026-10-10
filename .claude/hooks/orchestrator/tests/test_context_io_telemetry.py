import io
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import time
import uuid

import pytest

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
if str(PACKAGE_ROOT) not in sys.path:
    sys.path.insert(0, str(PACKAGE_ROOT))

import context_io_hook
import context_io_telemetry as context_io
import telemetry

UNRESOLVED_PROJECT_KEY = "unresolved"
from contract_hook_runner import run_contract_hook as _run_contract_hook

@pytest.fixture()
def tmp_path():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "context-io" / uuid.uuid4().hex
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

def _event(session, name, **changes):
    value = {"session_id": session, "hook_event_name": name}
    value.update(changes)
    return value

def _read_event(session, root, path, **tool_input):
    supplied = {"file_path": str(path), **tool_input}
    return _event(
        session,
        "PreToolUse",
        tool_name="Read",
        cwd=str(root),
        tool_input=supplied,
    )

def _events(state):
    events = []
    root = state / "telemetry" / "events"
    for path in root.glob("????-??/session_*.jsonl") if root.exists() else ():
        events.extend(json.loads(line) for line in path.read_text(encoding="utf-8").splitlines())
    return sorted(
        (event for event in events if event["event_type"] == "CONTEXT_IO_TURN_RECORDED"),
        key=lambda event: event["ts_utc"],
    )

def _journal(state, session):
    key = context_io._session_key(session)
    return state / "context_io" / "turns" / f"{key}.json"

def test_subprocess_malformed_json_is_silent_and_successful(tmp_path):
    result = _run_contract_hook("Read", b"{invalid json", tmp_path / "state")

    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")

@pytest.fixture(autouse=True)
def disable_projection(monkeypatch):
    monkeypatch.setattr(
        telemetry,
        "_projection_settings",
        lambda: (False, telemetry.obsidian_projection.DEFAULT_VAULT_ROOT),
    )

def test_python_task_aggregate_projection(tmp_path):
    root = tmp_path / "repo"
    state = tmp_path / "state"
    root.mkdir()
    short = root / "short.txt"
    long = root / "long.txt"
    short.write_bytes(b"x\n" * 199)
    long.write_bytes(b"x\n" * 501)
    session = "synthetic-session"

    context_io.user_prompt_submit(_event(session, "UserPromptSubmit", prompt="must-not-persist"), state_root=state)
    context_io.read_pre_tool_use(_read_event(session, root, short), state_root=state, repo_root=root)
    assert context_io.associate_turn(state, session_id=session, task_id="task-context-1")
    context_io.read_pre_tool_use(_read_event(session, root, long), state_root=state, repo_root=root)
    context_io.read_pre_tool_use(
        _read_event(session, root, long, offset=3, limit=7), state_root=state, repo_root=root
    )
    context_io.stop(_event(session, "Stop"), state_root=state)

    [event] = _events(state)
    payload = event["payload"]
    aggregate = json.loads(
        (
            state
            / "projects"
            / UNRESOLVED_PROJECT_KEY
            / "telemetry"
            / "tasks"
            / "task-context-1.json"
        ).read_text(encoding="utf-8")
    )
    assert aggregate["context_io"] == payload


@pytest.mark.parametrize("failing_append", ["_append_event", "_append_task_index"])
def test_failed_context_io_append_is_recoverable_and_retry_emits_once(
    tmp_path, monkeypatch, failing_append
):
    state = tmp_path / "state"
    session = "recoverable-" + failing_append
    task_id = "task-recoverable"
    context_io.user_prompt_submit(_event(session, "UserPromptSubmit"), state_root=state)
    assert context_io.associate_turn(state, session_id=session, task_id=task_id)
    turn = json.loads(_journal(state, session).read_text(encoding="utf-8"))
    event_id = str(uuid.uuid5(uuid.NAMESPACE_URL, "context-io:" + turn["turn_nonce"]))

    original = getattr(telemetry, failing_append)
    failed = False

    def fail_once(*args, **kwargs):
        nonlocal failed
        if not failed:
            failed = True
            raise OSError("injected append failure")
        return original(*args, **kwargs)

    monkeypatch.setattr(telemetry, failing_append, fail_once)
    context_io.stop(_event(session, "Stop"), state_root=state)
    assert _journal(state, session).exists()

    marker = state / "telemetry" / "events" / "spool" / ".merged" / event_id
    marker.parent.mkdir(parents=True, exist_ok=True)
    marker.touch(exist_ok=True)
    context_io.stop(_event(session, "Stop"), state_root=state)

    events = [event for event in _events(state) if event["event_id"] == event_id]
    assert len(events) == 1
    indexed = [
        json.loads(line)
        for line in (
            state / "telemetry" / "events" / "by_task" / f"{task_id}.jsonl"
        ).read_text(encoding="utf-8").splitlines()
        if json.loads(line)["event_id"] == event_id
    ]
    assert indexed == events
    assert not _journal(state, session).exists()

def test_pending_intent_relocates_after_valid_event_occupies_offset(tmp_path, monkeypatch):
    state = tmp_path / "state"
    session = "relocated-intent"
    task_one = "task-one"
    task_two = "task-two"
    context_io.user_prompt_submit(_event(session, "UserPromptSubmit"), state_root=state)
    assert context_io.associate_turn(state, session_id=session, task_id=task_one)

    original_append = telemetry._append_event
    monkeypatch.setattr(
        telemetry,
        "_append_event",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("injected failure before canonical write")
        ),
    )
    context_io.stop(_event(session, "Stop"), state_root=state)
    assert _journal(state, session).exists()
    event_one = json.loads(_journal(state, session).read_text(encoding="utf-8"))[
        "final_event"
    ]

    monkeypatch.setattr(telemetry, "_append_event", original_append)
    event_two = telemetry.build_context_io_turn_event(
        event_one["payload"], task_id=task_two, event_id=str(uuid.uuid4()),
        session_id=event_one["session_id"],
    )
    assert telemetry.emit_context_io_turn(event_two, state_root=state)
    assert telemetry.emit_context_io_turn(event_one, state_root=state)
    assert _journal(state, session).exists()
    context_io.stop(_event(session, "Stop"), state_root=state)
    assert not _journal(state, session).exists()

    telemetry_root = state / "telemetry"
    canonical = telemetry._event_path(telemetry_root, event_one).read_bytes().splitlines(
        keepends=True
    )
    encoded_one = telemetry._encoded_event(event_one)
    encoded_two = telemetry._encoded_event(event_two)
    assert canonical.count(encoded_one) == 1
    assert canonical.count(encoded_two) == 1
    assert telemetry._task_index_path(telemetry_root, task_one).read_bytes() == encoded_one
    assert telemetry._task_index_path(telemetry_root, task_two).read_bytes() == encoded_two

def test_pending_intent_relocates_valid_task_index_occupier(tmp_path, monkeypatch):
    state = tmp_path / "state"
    task_id = "shared-task"
    event_one = telemetry.build_context_io_turn_event(
        {}, task_id=task_id, event_id=str(uuid.uuid4())
    )
    event_two = telemetry.build_context_io_turn_event(
        {}, task_id=task_id, event_id=str(uuid.uuid4())
    )
    original_append = telemetry._append_task_index
    monkeypatch.setattr(
        telemetry,
        "_append_task_index",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("injected failure before index write")
        ),
    )
    assert not telemetry.emit_context_io_turn(event_one, state_root=state)
    monkeypatch.setattr(telemetry, "_append_task_index", original_append)

    assert telemetry.emit_context_io_turn(event_two, state_root=state)
    assert telemetry.emit_context_io_turn(event_one, state_root=state)
    telemetry_root = state / "telemetry"
    canonical = telemetry._event_path(telemetry_root, event_one).read_bytes().splitlines(
        keepends=True
    )
    indexed = telemetry._task_index_path(telemetry_root, task_id).read_bytes().splitlines(
        keepends=True
    )
    for event in (event_one, event_two):
        encoded = telemetry._encoded_event(event)
        assert canonical.count(encoded) == 1
        assert indexed.count(encoded) == 1

@pytest.mark.parametrize("occupier", [b'{\"partial\":', b"not-json\n"])
def test_pending_intent_does_not_relocate_inconclusive_occupier(
    tmp_path, monkeypatch, occupier
):
    state = tmp_path / "state"
    session = "inconclusive-" + str(len(occupier))
    context_io.user_prompt_submit(_event(session, "UserPromptSubmit"), state_root=state)
    original_append = telemetry._append_event
    monkeypatch.setattr(
        telemetry,
        "_append_event",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(
            OSError("injected failure before canonical write")
        ),
    )
    context_io.stop(_event(session, "Stop"), state_root=state)
    frozen = json.loads(_journal(state, session).read_text(encoding="utf-8"))[
        "final_event"
    ]
    event_path = telemetry._event_path(state / "telemetry", frozen)
    event_path.parent.mkdir(parents=True, exist_ok=True)
    event_path.write_bytes(occupier)
    monkeypatch.setattr(telemetry, "_append_event", original_append)

    context_io.stop(_event(session, "Stop"), state_root=state)
    assert _journal(state, session).exists()
    assert event_path.read_bytes() == occupier
    assert telemetry._encoded_event(frozen) not in occupier

def test_receipt_dedup_is_constant_bounded_after_log_growth(tmp_path, monkeypatch):
    state = tmp_path / "state"
    session = "receipt-bounded"
    task_id = "task-receipt-bounded"
    context_io.user_prompt_submit(_event(session, "UserPromptSubmit"), state_root=state)
    assert context_io.associate_turn(state, session_id=session, task_id=task_id)
    context_io.stop(_event(session, "Stop"), state_root=state)
    [event] = _events(state)

    telemetry_root = state / "telemetry"
    event_path = telemetry._event_path(telemetry_root, event)
    index_path = telemetry._task_index_path(telemetry_root, task_id)
    intent_path, receipt_path = telemetry._context_io_metadata_paths(
        telemetry_root, event["event_id"]
    )
    assert receipt_path.is_file()
    assert not intent_path.exists()
    unrelated = dict(event)
    unrelated["event_id"] = str(uuid.uuid4())
    unrelated_record = telemetry._encoded_event(unrelated)
    repetitions = telemetry.CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES // len(unrelated_record) + 2
    with event_path.open("ab") as handle:
        handle.write(unrelated_record * repetitions)
    with index_path.open("ab") as handle:
        handle.write(unrelated_record * repetitions)
    assert event_path.stat().st_size > telemetry.CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES
    assert index_path.stat().st_size > telemetry.CONTEXT_IO_FALLBACK_SCAN_MAX_BYTES

    monkeypatch.setattr(
        telemetry,
        "_bounded_event_by_id",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(
            AssertionError("receipt dedup must not scan the log")
        ),
    )
    assert telemetry.emit_context_io_turn(event, state_root=state)
    assert sum(
        json.loads(line)["event_id"] == event["event_id"]
        for line in event_path.read_text(encoding="utf-8").splitlines()
    ) == 1
    assert sum(
        json.loads(line)["event_id"] == event["event_id"]
        for line in index_path.read_text(encoding="utf-8").splitlines()
    ) == 1
    receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
    receipt["canonical_offset"] += receipt["canonical_length"]
    receipt_path.write_text(json.dumps(receipt), encoding="utf-8")
    assert not telemetry.emit_context_io_turn(event, state_root=state)

def test_python_rebuild_restores_task_projection(tmp_path):
    state = tmp_path / "state"
    associated = "rebuild"
    context_io.user_prompt_submit(_event(associated, "UserPromptSubmit"), state_root=state)
    assert context_io.associate_turn(state, session_id=associated, task_id="rebuild-task")
    context_io.stop(_event(associated, "Stop"), state_root=state)
    aggregate = (
        state
        / "projects"
        / UNRESOLVED_PROJECT_KEY
        / "telemetry"
        / "tasks"
        / "rebuild-task.json"
    )
    aggregate.unlink()
    telemetry.rebuild(state_root=state)
    rebuilt = json.loads(aggregate.read_text(encoding="utf-8"))
    assert rebuilt["context_io"]["task_association_known"] is True

def test_telemetry_failures_and_entrypoint_errors_are_silent_and_fail_open(
    tmp_path, monkeypatch, capsys
):
    state = tmp_path / "state"
    session = "failure"
    context_io.user_prompt_submit(_event(session, "UserPromptSubmit"), state_root=state)

    monkeypatch.setattr(
        telemetry,
        "emit_context_io_turn",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("private path")),
    )
    context_io.stop(_event(session, "Stop"), state_root=state)
    assert _journal(state, session).exists()

    monkeypatch.setattr(
        context_io_hook.context_io_telemetry,
        "handle",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(RuntimeError("prompt content")),
    )
    monkeypatch.setattr(sys, "stdin", io.StringIO('{"hook_event_name":"UserPromptSubmit"}'))
    assert context_io_hook.main() == 0
    assert capsys.readouterr() == ("", "")

def test_entrypoint_uses_its_repo_root_not_event_cwd(tmp_path, monkeypatch):
    root = tmp_path / "repo"
    subdirectory = root / "worktree" / "nested"
    subdirectory.mkdir(parents=True)
    sibling = root / "sibling.txt"
    sibling.write_text("one\ntwo\n", encoding="utf-8")
    protected = root / "AGENTS.md"
    protected.write_text("protected", encoding="utf-8")
    outside = tmp_path / "outside.txt"
    outside.write_text("outside", encoding="utf-8")
    runtime_parent = tmp_path / "runtime"
    state = runtime_parent / "purser-state"
    session = "entrypoint-root"
    monkeypatch.setattr(
        context_io_hook.project_root,
        "resolve_project_root",
        lambda _event: (root, ""),
    )
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))

    calls = (
        _event(session, "UserPromptSubmit"),
        _read_event(session, subdirectory, sibling),
        _read_event(session, subdirectory, outside),
        _read_event(session, subdirectory, protected),
        _event(session, "Stop"),
    )
    for call in calls:
        monkeypatch.setattr(sys, "stdin", io.StringIO(json.dumps(call)))
        assert context_io_hook.main() == 0

    payload = _events(state)[0]["payload"]
    assert payload["read_count"] == 3
    assert payload["full_read_count"] == 1
    assert payload["unknown_read_count"] == 2
    assert payload["exact_line_count_total"] == 2
    assert payload["unknown_reason"] == "OUTSIDE_ROOT"

@pytest.mark.parametrize("module", [context_io, telemetry])
def test_atomic_json_retries_transient_replace_holder(tmp_path, monkeypatch, module):
    destination = tmp_path / module.__name__ / "state.json"
    original_replace = module.os.replace
    attempts = 0

    def transient_replace(source, target):
        nonlocal attempts
        attempts += 1
        if attempts < 3:
            raise PermissionError("transient scanner holder")
        return original_replace(source, target)

    monkeypatch.setattr(module.os, "replace", transient_replace)
    monkeypatch.setattr(module.time, "sleep", lambda _seconds: None)
    module._atomic_json(destination, {"schema_version": 1, "value": 7})

    assert attempts == 3
    assert json.loads(destination.read_text(encoding="utf-8")) == {
        "schema_version": 1,
        "value": 7,
    }


def test_dedicated_entrypoint_has_no_heavy_hook_dependency():
    source = (PACKAGE_ROOT / "context_io_hook.py").read_text(encoding="utf-8")
    assert "import hook" not in source
    assert "import codex" not in source
    assert "parents[" not in source
