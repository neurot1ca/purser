import io
import json
from datetime import datetime, timedelta, timezone
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import compaction_advisor as advisor
import context_io_hook


NOW = datetime(2026, 9, 24, 12, 0, tzinfo=timezone.utc)


def _assistant(usage=None, content=None, timestamp=None):
    message = {"role": "assistant", "content": content or []}
    if usage is not None:
        message["usage"] = usage
    record = {"type": "assistant", "message": message}
    if timestamp:
        record["timestamp"] = timestamp.isoformat()
    return record


def _records(trigger="none", *, task_id="task-a", failed=False, context=260_000, age_minutes=1):
    records = [{"type": "user", "message": {"role": "user", "content": "previous prompt"}}]
    stamp = NOW - timedelta(minutes=age_minutes)
    if trigger == "commit":
        records.append(_assistant(content=[{"type": "tool_use", "id": "commit-1", "name": "Bash", "input": {"command": "git commit -m done"}}], timestamp=stamp))
        records.append({"type": "user", "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "commit-1", "is_error": failed, "content": "done"}]}})
    elif trigger == "task":
        records.append(_assistant(content=[{"type": "tool_use", "id": "codex-1", "name": "mcp__plugin_purser_codex-main__codex", "input": {"prompt": f"ORCH|task={task_id}|execute"}}], timestamp=stamp))
        records.append({"type": "user", "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "codex-1", "content": json.dumps({"threadId": "thread-1", "claude_review_mode": "FULL"})}]}})
        records.append(_assistant(content=[{"type": "text", "text": "Codex task complete"}], timestamp=stamp))
    else:
        records.append(_assistant(content=[{"type": "text", "text": "turn complete"}], timestamp=stamp))
    records.append(_assistant({"input_tokens": context, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, timestamp=stamp))
    return records


@pytest.fixture
def scratch():
    root = Path.cwd() / ".tmp" / f"compaction-advisor-{uuid.uuid4().hex}"
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


@pytest.fixture
def setup(scratch, monkeypatch):
    repo = scratch / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    local = scratch / "local"
    monkeypatch.setenv("LOCALAPPDATA", str(local))
    monkeypatch.setattr(Path, "home", classmethod(lambda _cls: scratch / "home"))
    home = scratch / "home"
    home.mkdir()
    return repo, local, scratch


def _event(scratch, repo, local, records, session="session-1"):
    transcript = scratch / "transcript.jsonl"
    transcript.write_text("\n".join(json.dumps(r) for r in records) + "\n", encoding="utf-8")
    return {"session_id": session, "transcript_path": str(transcript), "cwd": str(repo)}


def _codex_call(tool_id, tool_name, prompt, result, *, timestamp=None):
    return [
        _assistant(content=[{"type": "tool_use", "id": tool_id, "name": tool_name, "input": {"prompt": prompt}}], timestamp=timestamp),
        {"type": "user", "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tool_id, "content": json.dumps(result)}]}},
        _assistant(content=[{"type": "text", "text": "Codex task complete"}], timestamp=timestamp),
    ]


@pytest.mark.parametrize("trigger", ["idle", "commit", "task"])
def test_fires_for_each_boundary_above_threshold(setup, trigger):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit" if trigger == "commit" else "task" if trigger == "task" else "none", context=263_373, age_minutes=25 if trigger == "idle" else 1))
    result = advisor.advise(event, repo, now=NOW)
    assert result and f"trigger(s): {trigger}" in result
    assert "---" in result
    assert "⚓ **Context passed 263K" in result
    assert "**~72%**" in result
    assert "`/compact`" in result
    assert "The ship stays dry" in result
    assert "Purser prepared. Nothing is lost.*" in result
    assert "Do not claim compaction is lossless" not in result
    assert "write that question under Next" in result
    assert "after every tool call" in result
    assert "End your reply, after everything else" not in result
    assert "every later message costs far less" not in result


@pytest.mark.parametrize("context", [75_000, 60_000])
def test_post_compact_estimate_does_not_go_negative(setup, context):
    repo, local, scratch = setup
    config = Path.home() / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    config.write_text("[compaction]\nthreshold_tokens = 50000\n", encoding="utf-8")
    event = _event(scratch, repo, local, _records("commit", context=context))

    result = advisor.advise(event, repo, now=NOW)

    assert result and "**~0%**" in result


def test_does_not_fire_below_threshold(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=249_999))
    assert advisor.advise(event, repo, now=NOW) is None


def test_does_not_fire_without_trigger(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("none", age_minutes=2))
    assert advisor.advise(event, repo, now=NOW) is None


def test_handoff_template_and_advice_preserve_user_language(setup):
    repo, local, scratch = setup
    goal_lines = advisor.HANDOFF_TEMPLATE.split("## Goal\n", 1)[1].splitlines()
    assert goal_lines[0] == "User language: <the language the user writes in>"

    event = _event(scratch, repo, local, _records("commit"))
    result = advisor.advise(event, repo, now=NOW)
    assert result
    assert "Keep the first Goal line `User language: <language>` set to the language the user writes in." in result


def test_failed_git_commit_is_not_a_boundary(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", failed=True))
    assert advisor.advise(event, repo, now=NOW) is None


def test_task_end_requires_new_task_id(setup):
    repo, local, scratch = setup
    state_dir = advisor._state_directory(repo)
    state_dir.mkdir(parents=True)
    (state_dir / "compaction_advisor.json").write_text(json.dumps({"last_suggested_context": 0, "last_task_id": "task-a", "session_id": "session-1"}), encoding="utf-8")
    event = _event(scratch, repo, local, _records("task", task_id="task-a"))
    assert advisor.advise(event, repo, now=NOW) is None


def test_codex_reply_bundle_task_id_appears_in_state_block(setup):
    repo, local, scratch = setup
    stamp = NOW - timedelta(minutes=1)
    records = _records("commit")[:-1]
    records.extend(_codex_call(
        "reply-1",
        "mcp__plugin_purser_codex-main__codex-reply",
        "Continue.",
        {"task_id": "t1", "threadId": "thread-1", "claude_review_mode": "FULL"},
        timestamp=stamp,
    ))
    records.append(_assistant({"input_tokens": 260_000}, timestamp=stamp))
    event = _event(scratch, repo, local, records)

    result = advisor.advise(event, repo, now=NOW)

    assert result
    state = advisor.handoff_path(repo).read_text(encoding="utf-8")
    assert "task=t1 tool=codex-reply" in state


def test_orch_task_id_wins_over_bundle_task_id(setup):
    repo, local, scratch = setup
    stamp = NOW - timedelta(minutes=1)
    records = _records("commit")[:-1]
    records.extend(_codex_call(
        "codex-1",
        "mcp__plugin_purser_codex-main__codex",
        "ORCH|task=t1|execute",
        {"task_id": "other", "threadId": "thread-1"},
        timestamp=stamp,
    ))
    records.append(_assistant({"input_tokens": 260_000}, timestamp=stamp))
    event = _event(scratch, repo, local, records)

    result = advisor.advise(event, repo, now=NOW)

    assert result
    state = advisor.handoff_path(repo).read_text(encoding="utf-8")
    assert "task=t1 tool=codex" in state
    assert "task=other tool=codex" not in state


def test_codex_reply_same_task_does_not_look_like_new_task_for_task_end(setup):
    repo, local, scratch = setup
    stamp = NOW - timedelta(minutes=1)
    records = [{"type": "user", "message": {"role": "user", "content": "continue"}}]
    records.extend(_codex_call(
        "reply-1",
        "mcp__plugin_purser_codex-main__codex-reply",
        "Continue.",
        {"task_id": "t1", "threadId": "thread-1"},
        timestamp=stamp,
    ))
    records.append(_assistant({"input_tokens": 260_000}, timestamp=stamp))
    event = _event(scratch, repo, local, records)
    state_dir = advisor._state_directory(repo)
    state_dir.mkdir(parents=True)
    (state_dir / "compaction_advisor.json").write_text(json.dumps({"last_suggested_context": 0, "last_task_id": "t1", "session_id": "session-1"}), encoding="utf-8")

    assert advisor._transcript_facts_all(records)[1] == "t1"
    assert advisor.advise(event, repo, now=NOW) is None


def test_dedupe_requires_regrow_or_drop_below_threshold(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW)
    assert advisor.advise(event, repo, now=NOW) is None
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 250_000))
    assert advisor.advise(event, repo, now=NOW) is None
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 320_000))
    assert advisor.advise(event, repo, now=NOW)
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 200_000))
    assert advisor.advise(event, repo, now=NOW) is None
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 250_000))
    assert advisor.advise(event, repo, now=NOW)


def _rewrite_context(tmp_path, source, value):
    records = [json.loads(line) for line in Path(source).read_text(encoding="utf-8").splitlines()]
    records[-1]["message"]["usage"]["input_tokens"] = value
    target = tmp_path / f"transcript-{value}.jsonl"
    target.write_text("\n".join(json.dumps(r) for r in records) + "\n", encoding="utf-8")
    return target


def test_config_threshold_override_and_invalid_fallback(setup):
    repo, local, scratch = setup
    config = Path.home() / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    config.write_text("[compaction]\nthreshold_tokens = 300000\n", encoding="utf-8")
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW) is None
    config.write_text('[compaction]\nthreshold_tokens = "bad"\n', encoding="utf-8")
    assert advisor.advise(event, repo, now=NOW)


def test_replaces_state_block_preserving_user_sections_and_caps_it(scratch):
    path = scratch / "purser-handoff.md"
    path.write_text("# Purser handoff\n## Goal\nKeep this\n## State\nUser state\n" + advisor.STATE_START + "\nold\n" + advisor.STATE_END + "\n", encoding="utf-8")
    advisor._replace_state_block(path, "x" * 2000)
    text = path.read_text(encoding="utf-8")
    body = text.split(advisor.STATE_START, 1)[1].split(advisor.STATE_END, 1)[0].strip()
    assert "## Goal\nKeep this" in text and "## State\nUser state" in text
    assert len(body) <= 1500 and body.endswith("... truncated")


@pytest.mark.parametrize("source", ["compact", "clear"])
def test_restore_after_compact_or_clear(setup, source):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text("# Purser handoff\n## Goal\nKeep working\n", encoding="utf-8")

    restored = advisor.restore({"source": source}, repo)

    assert restored.startswith("Purser handoff (historical reference, written before the context was compacted or cleared;")
    assert "skip any listed file already present in context" in restored
    assert "Keep working" in restored
    assert f"Full file: {path}" in restored


def test_restore_includes_user_language_instruction(setup):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text("# Purser handoff\n## Goal\nUser language: Turkish\nKeep working\n", encoding="utf-8")

    restored = advisor.restore({"source": "compact"}, repo)

    assert restored.startswith("Purser handoff (historical reference, written before the context was compacted or cleared;")
    assert "Reply to the user in Turkish; everything else stays as the project rules say." in restored


@pytest.mark.parametrize("language_line", ["User language: <the language the user writes in>", ""])
def test_restore_omits_user_language_instruction_for_placeholder_or_missing_line(setup, language_line):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text(f"# Purser handoff\n## Goal\n{language_line}\nKeep working\n", encoding="utf-8")

    restored = advisor.restore({"source": "compact"}, repo)

    assert restored
    assert "Reply to the user in" not in restored


def test_restore_startup_requires_recent_mtime_but_resume_is_ignored(setup):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text("## Goal\nRecent\n", encoding="utf-8")
    now = advisor.time.time()
    os.utime(path, (now - 11 * 60 * 60, now - 11 * 60 * 60))
    assert advisor.restore({"source": "startup"}, repo)
    assert advisor.restore({"source": "resume"}, repo) is None
    os.utime(path, (now - 12 * 60 * 60, now - 12 * 60 * 60))
    assert advisor.restore({"source": "startup"}, repo) is None
    os.utime(path, (now - 13 * 60 * 60, now - 13 * 60 * 60))
    assert advisor.restore({"source": "startup"}, repo) is None
    assert advisor.restore({"source": "resume"}, repo) is None


@pytest.mark.parametrize("source", ["compact", "clear", "startup", "resume", "unknown"])
def test_restore_ignores_empty_template_and_missing_file(setup, source):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    assert advisor.restore({"source": source}, repo) is None
    path.parent.mkdir(parents=True)
    path.write_text(advisor.HANDOFF_TEMPLATE, encoding="utf-8")
    assert advisor.restore({"source": source}, repo) is None


def test_restore_respects_disabled_setting_and_unknown_source(setup):
    repo, _local, scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text("## Goal\nKeep working\n", encoding="utf-8")
    config = scratch / "home" / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    config.write_text("[compaction]\nenabled = false\n", encoding="utf-8")
    assert advisor.restore({"source": "compact"}, repo) is None
    config.write_text("[compaction]\nenabled = true\n", encoding="utf-8")
    assert advisor.restore({"source": "other"}, repo) is None
    assert advisor.restore({"source": "compact"}, None) is None


def test_restore_caps_long_handoff_with_full_file_pointer(setup):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    path.write_text("H" * 8000, encoding="utf-8")

    restored = advisor.restore({"source": "clear"}, repo)

    assert len(restored) <= 6000
    assert restored.startswith("Purser handoff (historical reference, written before the context was compacted or cleared;")
    assert restored.endswith(f"... truncated; read the full file at {path}")


def test_malformed_transcript_line_is_ignored(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    Path(event["transcript_path"]).write_text("not json\n" + Path(event["transcript_path"]).read_text(encoding="utf-8"), encoding="utf-8")
    assert advisor.advise(event, repo, now=NOW)


def test_hook_emits_utf8_json_only_when_advisor_returns_text(monkeypatch):
    payload = {"hook_event_name": "UserPromptSubmit", "cwd": "."}
    input_raw, output_raw = io.BytesIO(json.dumps(payload).encode()), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (None, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: "hazır" )
    assert context_io_hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"] == "hazır"

    input_raw2, output_raw2 = io.BytesIO(json.dumps(payload).encode()), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw2, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw2, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: (_ for _ in ()).throw(RuntimeError("advisor failed")))
    assert context_io_hook.main() == 0
    assert output_raw2.getvalue() == b""


def test_session_start_hook_emits_utf8_json_only_when_restore_returns_text(monkeypatch):
    payload = {"hook_event_name": "SessionStart", "source": "compact", "cwd": "."}
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (None, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: "özel handoff")
    input_raw, output_raw = io.BytesIO(json.dumps(payload, ensure_ascii=False).encode("utf-8")), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))

    assert context_io_hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8")) == {
        "hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": "özel handoff"}
    }

    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: None)
    payload["source"] = "resume"
    input_raw2, output_raw2 = io.BytesIO(json.dumps(payload).encode("utf-8")), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw2, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw2, encoding="cp1254"))
    assert context_io_hook.main() == 0
    assert output_raw2.getvalue() == b""

    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: (_ for _ in ()).throw(RuntimeError("restore failed")))
    input_raw3, output_raw3 = io.BytesIO(json.dumps(payload).encode("utf-8")), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw3, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw3, encoding="cp1254"))
    assert context_io_hook.main() == 0
    assert output_raw3.getvalue() == b""
