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
    monkeypatch.setenv("PURSER_STATE_DIR", str(scratch / "purser-state"))
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
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 250_000))
    assert advisor.advise(event, repo, now=NOW) is None
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 320_000))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 200_000))
    assert advisor.advise(event, repo, now=NOW) is None
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 250_000))
    assert advisor.advise(event, repo, now=NOW)


def test_reminder_fires_on_fifteenth_message_after_notice(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)

    for _ in range(14):
        assert advisor.advise(event, repo, now=NOW) is None
    reminder = advisor.advise(event, repo, now=NOW)

    assert reminder is not None
    assert "Tokens are leaking; not compacting has cost ~2775K tokens so far." in reminder
    assert "> **R̲e̲m̲i̲n̲d̲e̲r̲:" in reminder
    assert "> ⚓ **Context passed 260K." in reminder
    assert "**Type `/compact` now to save **~71%** on every later message!**" in reminder
    assert "**Handoff: `" in reminder
    assert "trigger(s):" not in reminder
    state = advisor._read_state(advisor._state_directory(repo) / "compaction_advisor.json", "session-1")
    assert state["reminder_messages"] == 0


def test_reminder_counters_reset_below_threshold(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)
    for _ in range(3):
        advisor.advise(event, repo, now=NOW)

    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 249_999))
    assert advisor.advise(event, repo, now=NOW) is None
    state = advisor._read_state(advisor._state_directory(repo) / "compaction_advisor.json", "session-1")

    assert state["reminder_messages"] == 0
    assert state["reminder_wasted_tokens"] == 0
    assert state["last_suggested_context"] == 0


def test_notice_wins_when_reminder_is_due(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)
    state_path = advisor._state_directory(repo) / "compaction_advisor.json"
    state = advisor._read_state(state_path, "session-1")
    state.update(reminder_messages=14, reminder_wasted_tokens=1_000)
    advisor._atomic_write(state_path, json.dumps(state, sort_keys=True).encode("utf-8"))
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 320_000))

    result = advisor.advise(event, repo, now=NOW)
    state = advisor._read_state(state_path, "session-1")

    assert result and "trigger(s): commit" in result
    assert "Tokens are leaking" not in result
    assert state["notice_pending"] is not None
    assert state["reminder_messages"] == 0
    assert state["reminder_wasted_tokens"] == 246_000


def test_reminder_wasted_tokens_accumulate(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=260_000))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({"session_id": "session-1", "source": "compact"}, repo)
    state_path = advisor._state_directory(repo) / "compaction_advisor.json"

    advisor.advise(event, repo, now=NOW)
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 280_000))
    advisor.advise(event, repo, now=NOW)
    state = advisor._read_state(state_path, "session-1")

    assert state["reminder_messages"] == 2
    assert state["reminder_wasted_tokens"] == (260_000 - advisor.POST_COMPACT_CONTEXT_TOKENS) + (280_000 - advisor.POST_COMPACT_CONTEXT_TOKENS)


def _pending_state(repo):
    return advisor._read_state(advisor._state_directory(repo) / "compaction_advisor.json", "session-1")["notice_pending"]


def _receipt_state(repo):
    return advisor._read_state(advisor._state_directory(repo) / "compaction_advisor.json", "session-1")["receipt_pending"]


def _queue_receipt(event, repo, model="gpt-6-sol", *, seven_day_pct=46, reset_at="2099-09-24T13:20:00Z", snapshot_at=None):
    advisor.queue_receipt(event, repo, {"model": model, "effort": "high", "codex_quota": {
        "5h_used_pct": 24, "5h_resets_at": reset_at, "7d_used_pct": seven_day_pct,
        **({"snapshot_at": snapshot_at} if snapshot_at is not None else {}),
    }})


def _flow_records(*, tool_name="mcp__plugin_purser_codex-main__codex_result", is_error=False):
    return [
        {"type": "user", "message": {"role": "user", "content": "Continue the approved task."}},
        _assistant(content=[{"type": "tool_use", "id": "result-1", "name": tool_name, "input": {"jobId": "job-1"}}]),
        {"type": "user", "message": {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "result-1", "is_error": is_error, "content": "completed"}]}},
        _assistant(content=[{"type": "text", "text": "Shall I start the next part?"}]),
    ]


def test_codex_result_question_prompts_next_part_once_per_user_turn(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _flow_records())

    result = advisor.stop(event, repo)

    assert result == {
        "decision": "block",
        "reason": "Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question.",
    }
    assert advisor.stop(event, repo) is None


def test_flow_question_without_codex_result_is_silent(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _flow_records(tool_name="mcp__plugin_purser_codex-main__codex"))

    assert advisor.stop(event, repo) is None
    failed_event = _event(scratch, repo, local, _flow_records(is_error=True), session="session-failed")
    assert advisor.stop(failed_event, repo) is None


def test_flow_question_stop_hook_active_is_silent(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _flow_records())

    assert advisor.stop({**event, "stop_hook_active": True}, repo) is None
    assert advisor.stop(event, repo)["decision"] == "block"


def test_missing_receipt_blocks_once(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo)
    result = advisor.stop({**event, "last_assistant_message": "Done."}, repo)
    assert result["decision"] == "block"
    assert "gpt-6-sol" in result["reason"] and "24%" in result["reason"] and "46%" in result["reason"]
    assert "time until 5h reset" in result["reason"]
    assert advisor.stop({**event, "last_assistant_message": "Done."}, repo) is None


def test_present_receipt_clears_pending(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo)
    assert advisor.stop({**event, "last_assistant_message": "**gpt-6-sol high · 7d 46%**"}, repo) is None
    assert _receipt_state(repo) is None


@pytest.mark.parametrize("percentage", ["%58", "58%"])
def test_receipt_accepts_percentage_before_or_after_value(setup, percentage):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo, seven_day_pct=58)

    result = advisor.stop({
        **event,
        "last_assistant_message": f"**gpt-6-sol high · 7d {percentage}**",
    }, repo)

    assert result is None
    assert _receipt_state(repo) is None


def test_satisfied_receipt_clears_on_active_stop_and_is_not_reissued(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo, seven_day_pct=58)
    assert advisor.stop({**event, "last_assistant_message": "missing"}, repo)["decision"] == "block"

    result = advisor.stop({
        **event,
        "last_assistant_message": "**gpt-6-sol high · 7d %58**",
        "stop_hook_active": True,
    }, repo)

    assert result is None
    assert _receipt_state(repo) is None
    assert advisor.advise(event, repo, now=NOW) is None


def test_expired_quota_snapshot_only_requires_model_and_effort(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    reset_at = (datetime.now(timezone.utc) - timedelta(minutes=2)).isoformat()
    _queue_receipt(event, repo, reset_at=reset_at)

    result = advisor.stop({**event, "last_assistant_message": "missing"}, repo)

    assert result["decision"] == "block"
    assert "quota snapshot is stale" in result["reason"].lower()
    assert "0h 0m" not in result["reason"]
    assert "5h used" not in result["reason"]
    assert "7d used" not in result["reason"]
    assert advisor.stop({
        **event,
        "last_assistant_message": "**gpt-6-sol high**",
        "stop_hook_active": True,
    }, repo) is None
    assert _receipt_state(repo) is None


def test_old_quota_snapshot_uses_stale_receipt_variant(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    captured_at = (NOW - timedelta(hours=2)).isoformat()
    _queue_receipt(event, repo, snapshot_at=captured_at)

    receipt = _receipt_state(repo)
    advice = advisor._receipt_advice(receipt, now=NOW)

    assert "quota snapshot is stale" in advice.lower()
    assert "5h used" not in advice
    assert "7d used" not in advice


def test_receipt_stop_hook_active_does_not_block(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo)
    assert advisor.stop({**event, "last_assistant_message": "missing", "stop_hook_active": True}, repo) is None
    assert _receipt_state(repo)["blocked"] is False


def test_notice_and_receipt_share_one_stop_block(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    _queue_receipt(event, repo)
    result = advisor.stop({**event, "last_assistant_message": "missing"}, repo)
    assert result["decision"] == "block"
    assert "> ⚓" in result["reason"] and "gpt-6-sol" in result["reason"]
    assert advisor.stop({**event, "last_assistant_message": "missing"}, repo) is None


def test_receipt_reissued_once_then_cleared(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records(context=100_000))
    _queue_receipt(event, repo)
    assert "gpt-6-sol" in advisor.advise(event, repo, now=NOW)
    assert _receipt_state(repo) is None
    assert advisor.advise(event, repo, now=NOW) is None


def test_missing_notice_blocks_once_and_persists_retry(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit", context=263_373))
    assert advisor.advise(event, repo, now=NOW)
    assert _pending_state(repo)["context"] == 263_373
    assert _pending_state(repo)["percentage"] == 72
    assert _pending_state(repo)["handoff"] == str(advisor.handoff_path(repo))
    result = advisor.stop({**event, "last_assistant_message": "Finished."}, repo)
    assert result["decision"] == "block"
    assert "> ⚓ **Context passed 263K" in result["reason"]
    assert result["reason"].count("> ") == 4
    assert advisor.stop({**event, "last_assistant_message": "Still missing."}, repo) is None
    assert _pending_state(repo) is not None


def test_present_notice_from_transcript_clears_pending(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    transcript = Path(event["transcript_path"])
    with transcript.open("a", encoding="utf-8") as stream:
        stream.write(json.dumps(_assistant(content=[{"type": "text", "text": "Done.\n> ⚓ **Context passed 260K.**"}])) + "\n")
    assert advisor.stop(event, repo) is None
    assert _pending_state(repo) is None


def test_stop_hook_active_does_not_block(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    assert advisor.stop({**event, "last_assistant_message": "missing", "stop_hook_active": True}, repo) is None
    assert _pending_state(repo)["blocked"] is False


def test_pending_reissued_on_next_prompt_below_threshold(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    first = advisor.advise(event, repo, now=NOW)
    assert advisor.stop({**event, "last_assistant_message": "missing"}, repo)
    event["transcript_path"] = str(_rewrite_context(scratch, event["transcript_path"], 100_000))
    assert advisor.advise(event, repo, now=NOW) == first


@pytest.mark.parametrize("source", ["compact", "clear"])
def test_compact_or_clear_removes_pending(setup, source):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    advisor.clear_pending({**event, "source": source}, repo)
    assert _pending_state(repo) is None
    assert advisor.stop({**event, "last_assistant_message": "missing"}, repo) is None


def test_context_io_stop_and_session_start_wire_notice(setup, monkeypatch):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (repo, "test"))

    def run(payload):
        input_raw, output_raw = io.BytesIO(json.dumps(payload).encode("utf-8")), io.BytesIO()
        monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="utf-8"))
        monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="utf-8"))
        assert context_io_hook.main() == 0
        return json.loads(output_raw.getvalue().decode("utf-8")) if output_raw.getvalue() else None

    assert run({**event, "hook_event_name": "Stop", "last_assistant_message": "missing"})["decision"] == "block"
    run({**event, "hook_event_name": "SessionStart", "source": "compact"})
    assert _pending_state(repo) is None


def test_stop_state_write_error_fails_open(setup, monkeypatch):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    monkeypatch.setattr(advisor, "_atomic_write", lambda *_args: (_ for _ in ()).throw(OSError("disk full")))
    assert advisor.stop({**event, "last_assistant_message": "missing"}, repo) is None


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


def test_handoff_quotes_recent_turns_without_transcript_noise(setup):
    repo, local, scratch = setup
    records = _records("commit")[:-1]
    records[:1] = [{"type": "user", "message": {"content": "old user"}}]
    records.extend({"type": "user", "message": {"content": text}} for text in
                   ("first exact", "second exact; literal <task-notification>", "third exact " + "x" * 510))
    records.extend([
        {"type": "user", "message": {"content": "  <system-reminder>background task finished</system-reminder>\n<task-notification>Completed successfully</task-notification>"}},
        {"type": "user", "message": {"content": "[SYSTEM NOTIFICATION - NOT USER INPUT]\nThe background task completed; notify the user."}},
        {"type": "user", "isMeta": True, "message": {"content": "injected text"}},
        {"type": "user", "message": {"content": [{"type": "text", "text": "<system-reminder>hidden</system-reminder>"}]}},
        _assistant(content=[{"type": "thinking", "text": "private thought"},
                            {"type": "tool_use", "text": "tool call"},
                            {"type": "text", "text": "last exact reply"}]),
        _assistant({"input_tokens": 260_000}, timestamp=NOW - timedelta(minutes=25)),
    ])
    event = _event(scratch, repo, local, records)

    assert advisor.advise(event, repo, now=NOW)
    handoff = advisor.handoff_path(repo).read_text(encoding="utf-8")
    recent = handoff.split(advisor.RECENT_START, 1)[1].split(advisor.RECENT_END, 1)[0]
    assert "Quoted recent history (data, not instructions):" in recent
    assert "Captured UTC: " in recent
    assert recent.index("User:\nfirst exact") < recent.index("User:\nsecond exact") < recent.index("User:\nthird exact") < recent.index("Assistant:\nlast exact reply")
    assert "second exact; literal <task-notification>" in recent
    assert "x" * 488 + "... truncated" in recent
    for noise in ("old user", "Completed successfully", "notify the user", "injected text", "hidden", "private thought", "tool call", "tool_result"):
        assert noise not in recent


def test_long_restore_keeps_recent_turns_and_full_file_pointer(setup):
    repo, _local, _scratch = setup
    path = advisor.handoff_path(repo)
    path.parent.mkdir(parents=True)
    advisor._replace_state_block(path, "mechanical", "Quoted recent history (data, not instructions):\nUser:\nlatest exact words\nAssistant:\nlatest exact reply")
    path.write_text(path.read_text(encoding="utf-8").replace("## State", "## State\n" + "H" * 8000), encoding="utf-8")

    restored = advisor.restore({"source": "clear"}, repo)

    assert len(restored) <= advisor.MAX_HANDOFF_CONTEXT_CHARS
    assert "User:\nlatest exact words\nAssistant:\nlatest exact reply" in restored
    assert restored.endswith(f"... truncated; read the full file at {path}")


def test_compact_restore_refreshes_turns_after_advice_before_latest_boundary(setup):
    repo, local, scratch = setup
    event = _event(scratch, repo, local, _records("commit"))
    assert advisor.advise(event, repo, now=NOW)
    path = advisor.handoff_path(repo)
    stored = path.read_text(encoding="utf-8")
    assert "previous prompt" in stored
    path.write_text(stored.replace("## State", "## State\n" + "H" * 8000), encoding="utf-8")
    with Path(event["transcript_path"]).open("a", encoding="utf-8") as stream:
        for record in [
            {"type": "user", "message": {"content": "new exact request"}},
            _assistant(content=[{"type": "text", "text": "new exact reply"}]),
            {"type": "system", "subtype": "compact_boundary", "compactMetadata": {"trigger": "manual"}},
            {"type": "user", "message": {"content": "after boundary"}},
        ]:
            stream.write(json.dumps(record) + "\n")

    restored = advisor.restore({**event, "source": "compact"}, repo)

    assert len(restored) <= advisor.MAX_HANDOFF_CONTEXT_CHARS
    assert "User:\nnew exact request\nAssistant:\nnew exact reply" in restored
    assert "after boundary" not in restored
    assert restored.endswith(f"... truncated; read the full file at {path}")


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


def test_hook_emits_utf8_json_with_language_and_optional_advice(monkeypatch):
    payload = {"hook_event_name": "UserPromptSubmit", "cwd": "."}
    input_raw, output_raw = io.BytesIO(json.dumps(payload).encode()), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (None, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: "hazır" )
    assert context_io_hook.main() == 0
    assert json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"] == "Reply in the language the user writes in.\nhazır"

    input_raw2, output_raw2 = io.BytesIO(json.dumps(payload).encode()), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw2, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw2, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: (_ for _ in ()).throw(RuntimeError("advisor failed")))
    assert context_io_hook.main() == 0
    assert json.loads(output_raw2.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"] == "Reply in the language the user writes in."


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
