import io
import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import context_io_hook


def _run_user_prompt_hook(monkeypatch, repo, *, prompt="", language_handoff=None, advice=None, unreadable=False):
    payload = {"hook_event_name": "UserPromptSubmit", "cwd": str(repo), "prompt": prompt}
    input_raw = io.BytesIO(json.dumps(payload).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (repo, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: advice)
    handoff = repo / "purser-handoff.md"
    monkeypatch.setattr(context_io_hook.compaction_advisor, "handoff_path", lambda _repo: handoff)
    if language_handoff is not None:
        handoff.parent.mkdir(parents=True, exist_ok=True)
        handoff.write_text(language_handoff, encoding="utf-8")
    if unreadable:
        original_read_text = Path.read_text

        def read_text(path, *args, **kwargs):
            if path == handoff:
                raise OSError("unreadable handoff")
            return original_read_text(path, *args, **kwargs)

        monkeypatch.setattr(Path, "read_text", read_text)
    assert context_io_hook.main() == 0
    if not output_raw.getvalue():
        return None
    return json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"]


def test_context_io_hook_reads_utf8_with_cp1254_wrapper(monkeypatch):
    payload = {"hook_event_name": "PostToolUse", "prompt": "❯ ı"}
    input_raw = io.BytesIO(json.dumps(payload, ensure_ascii=False).encode("utf-8"))
    output_raw = io.BytesIO()
    observed = []
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (None, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda event, **_kwargs: observed.append(event))

    assert context_io_hook.main() == 0
    assert observed == [payload]
    # This lifecycle hook is intentionally silent; the UTF-8 transport change
    # preserves its existing empty stdout contract.
    assert output_raw.getvalue() == b""


def test_session_start_appends_duration_hint_and_runs_cleanup(monkeypatch, tmp_path):
    payload = {"hook_event_name": "SessionStart", "cwd": str(tmp_path)}
    input_raw = io.BytesIO(json.dumps(payload).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _root: "project-key")
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: "restored context")
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: tmp_path / "state")
    cleaned = []
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "cleanup_jobs", lambda root: cleaned.append(root))
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint",
                        lambda root, key: "Purser: typical Codex durations here (median/p90, n): mechanical 2/6 min (n=41).")

    assert context_io_hook.main() == 0
    result = json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]
    assert result["additionalContext"] == (
        "restored context\nPurser: typical Codex durations here (median/p90, n): mechanical 2/6 min (n=41)."
    )
    assert cleaned == [tmp_path / "state"]


def test_user_prompt_adds_recorded_non_english_reply_language(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path,
        language_handoff="# Handoff\n## Goal\nUser language: Turkish\n",
    )

    assert actual == "Reply to the user in Turkish."


@pytest.mark.parametrize("language_handoff", [None, "## Goal\nContinue\n"])
def test_user_prompt_without_recorded_language_adds_generic_reminder(monkeypatch, tmp_path, language_handoff):
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, language_handoff=language_handoff)

    assert actual == "Reply in the language the user writes in."


def test_user_prompt_omits_reminder_for_english(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path,
        language_handoff="## Goal\nUser language: eNgLiSh\n",
    )

    assert actual is None


def test_user_prompt_puts_language_before_compaction_advice(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path,
        language_handoff="## Goal\nUser language: Turkish\n",
        advice="compact notice",
    )

    assert actual == "Reply to the user in Turkish.\ncompact notice"


def test_user_prompt_unreadable_handoff_falls_back_to_generic(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, unreadable=True)

    assert actual == "Reply in the language the user writes in."


@pytest.mark.parametrize("prompt", [
    "Please help me plan the rollout.",
    "Let's start planning the change.",
    "We have several plans to compare.",
    "Projeyi planla ve riskleri çıkar.",
    "Bunu planlayalım.",
    "Planı gözden geçir, planın kapsamı nedir?",
])
def test_user_prompt_offers_plan_for_english_and_turkish_planning_words(monkeypatch, tmp_path, prompt):
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt=prompt)

    assert actual == (
        "Reply in the language the user writes in.\n"
        "The user mentioned planning; offer /purser:plan once in this session if you have not already."
    )


@pytest.mark.parametrize("prompt", ["planet", "explanation", "airplane"])
def test_user_prompt_does_not_offer_plan_for_unrelated_words(monkeypatch, tmp_path, prompt):
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt=prompt)

    assert actual == "Reply in the language the user writes in."


def test_user_prompt_does_not_offer_plan_when_command_is_invoked(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt="/purser:plan design the migration")

    assert actual == "Reply in the language the user writes in."


def test_plan_hint_matcher_failure_preserves_existing_context(monkeypatch, tmp_path):
    monkeypatch.setattr(context_io_hook, "_planning_hint", lambda _prompt: (_ for _ in ()).throw(RuntimeError("matcher failed")))

    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path, prompt="plan the migration",
        language_handoff="## Goal\nUser language: Turkish\n", advice="compact notice",
    )

    assert actual == "Reply to the user in Turkish.\ncompact notice"
