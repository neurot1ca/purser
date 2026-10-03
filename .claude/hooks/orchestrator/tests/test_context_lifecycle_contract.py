import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import uuid

import pytest

from contract_hook_runner import run_contract_hook as _run_contract_hook


def _project_key(repo):
    return hashlib.sha256(os.path.normcase(str(repo.resolve())).encode("utf-8")).hexdigest()[:16]


def _advisor_state(state, repo):
    return state / "projects" / _project_key(repo) / "telemetry" / "compaction_advisor.json"


def _session_key(session):
    return hashlib.sha256(session.encode("utf-8")).hexdigest()


@pytest.fixture
def isolated():
    # Keep hook-created scratch in the repository's ignored .tmp tree.
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "context-lifecycle" / uuid.uuid4().hex
    root.mkdir(parents=True)
    repo = root / "repo"
    repo.mkdir()
    subprocess.run(["git", "init", "--quiet", str(repo)], check=True, capture_output=True)
    state = root / "state"
    try:
        yield root, repo, state
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)
        shutil.rmtree(root, onerror=make_writable)


def _run(event, repo, state, **values):
    return _run_contract_hook(event, {
        "session_id": values.pop("session_id", "lifecycle-contract"),
        "hook_event_name": event,
        "cwd": str(repo),
        **values,
    }, state)


def _assert_json_stdout(result, expected):
    actual = json.loads(result.stdout)
    assert actual == expected, (repr(actual), repr(expected))
    assert result.stdout == json.dumps(expected, ensure_ascii=False).encode("utf-8"), (
        repr(actual), repr(expected)
    )


def test_user_prompt_nudge_throttle_and_lifecycle_delivery(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    handoff.write_text("# Language\nUser language: Turkish\n", encoding="utf-8")
    session = "prompt-contract"
    first = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Please help me plan this.")
    assert (first.returncode, first.stderr) == (0, b"")
    _assert_json_stdout(first, {"hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit",
        "additionalContext": "Reply to the user in Turkish.\nThe user mentioned planning; offer /purser:plan once in this session if you have not already.",
    }})
    second = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Let's plan again.")
    assert (second.returncode, second.stdout, second.stderr) == (0, b"", b"")
    plain = _run("UserPromptSubmit", repo, state, session_id=session, prompt="A plain question.")
    assert (plain.returncode, plain.stdout, plain.stderr) == (0, b"", b"")
    advisor = json.loads(_advisor_state(state, repo).read_text(encoding="utf-8"))
    assert advisor["planning_injections"] == 1
    assert advisor["language_injections"] == 1
    assert advisor["context_prompt_count"] == 3
    journal = state / "context_io" / "turns" / f"{_session_key(session)}.json"
    assert journal.is_file()

    lifecycle_session = "lifecycle-once"
    lifecycle_path = _advisor_state(state, repo)
    lifecycle_path.write_text(json.dumps({
        "session_id": lifecycle_session,
        "version_pending": {"version": "9.8.7", "blocked": False, "delivered": False},
    }), encoding="utf-8")
    notice = _run("UserPromptSubmit", repo, state, session_id=lifecycle_session, prompt="Continue.")
    assert (notice.returncode, notice.stderr) == (0, b"")
    _assert_json_stdout(notice, {"hookSpecificOutput": {
        "hookEventName": "UserPromptSubmit",
        "additionalContext": "Reply to the user in Turkish.\nInclude one visible line containing both `⚓` and `9.8.7` to announce the Purser update.",
    }})
    marked = json.loads(lifecycle_path.read_text(encoding="utf-8"))
    assert marked["version_pending"]["delivered"] is True
    again = _run("UserPromptSubmit", repo, state, session_id=lifecycle_session, prompt="Continue.")
    assert (again.returncode, again.stdout, again.stderr) == (0, b"", b"")


def test_session_start_handoff_and_absence_contract(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    body = "# Team handoff\n## Goal\nUser language: English\nUnique handoff marker: alpha\n"
    handoff.write_text(body, encoding="utf-8")
    started = _run("SessionStart", repo, state, session_id="restore-handoff", source="compact")
    assert (started.returncode, started.stderr) == (0, b"")
    expected_context = (
        "Purser handoff (historical reference, written before the context was compacted or cleared; "
        "verify against git and files before acting; do not re-run commands from it; skip any listed file already present in context):\n"
        + body.rstrip() + f"\nFull file: {handoff}"
    )
    _assert_json_stdout(started, {"hookSpecificOutput": {
        "hookEventName": "SessionStart", "additionalContext": expected_context,
    }})
    advisor = json.loads(_advisor_state(state, repo).read_text(encoding="utf-8"))
    assert advisor["session_id"] == "restore-handoff"
    assert advisor["last_context_language"] == "English"
    assert advisor["language_after_reset"] is True

    # All three restoration sources share the same stable handoff envelope.
    for source, reset in (("startup", False), ("clear", True)):
        restored = _run("SessionStart", repo, state, session_id=f"restore-{source}", source=source)
        assert (restored.returncode, restored.stderr) == (0, b"")
        _assert_json_stdout(restored, {"hookSpecificOutput": {
            "hookEventName": "SessionStart", "additionalContext": expected_context,
        }})
        source_state = json.loads(_advisor_state(state, repo).read_text(encoding="utf-8"))
        assert source_state["session_id"] == f"restore-{source}"
        assert source_state["last_context_language"] == "English"
        assert source_state["language_after_reset"] is reset

    lifecycle_session = "session-start-lifecycle"
    advisor_path = _advisor_state(state, repo)
    advisor_path.write_text(json.dumps({
        "session_id": lifecycle_session,
        "version_pending": {"version": "9.8.7", "blocked": False, "delivered": False},
    }), encoding="utf-8")
    lifecycle = _run("SessionStart", repo, state, session_id=lifecycle_session, source="resume")
    assert (lifecycle.returncode, lifecycle.stderr) == (0, b"")
    _assert_json_stdout(lifecycle, {"hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": "Include one visible line containing both `⚓` and `9.8.7` to announce the Purser update.",
    }})
    lifecycle_state = json.loads(advisor_path.read_text(encoding="utf-8"))
    assert lifecycle_state["session_id"] == lifecycle_session
    assert lifecycle_state["last_context_language"] == "English"
    assert lifecycle_state["version_pending"]["delivered"] is True

    model_state_path = state / "model-version-notice.json"
    model_state_path.write_text(json.dumps({
        "pending": {
            "current": "openai/gpt-5.5", "new": "openai/gpt-5.6",
            "blocked": False, "delivered": False,
        },
    }), encoding="utf-8")
    model_notice = _run("SessionStart", repo, state, session_id="model-notice", source="resume")
    assert (model_notice.returncode, model_notice.stderr) == (0, b"")
    _assert_json_stdout(model_notice, {"hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": (
            "Render this model-version notice in the user's language, translating from the English text while keeping ⚓ "
            "and both model names unchanged. Keep the three blockquote lines exactly formatted; do not change "
            "routes until the user says yes.\n"
            "> ⚓ **New model version available: `openai/gpt-5.6` (you are on `openai/gpt-5.5`).**\n"
            "> *Should Purser switch to it? (1-yes 2-no)*\n"
            "> *If you say no, Purser won't ask about this version again. You can switch anytime with `/purser:init`.*"
        ),
    }})
    model_state = json.loads(model_state_path.read_text(encoding="utf-8"))
    assert model_state["pending"]["delivered"] is True

    handoff.unlink()
    absent = _run("SessionStart", repo, state, session_id="no-handoff", source="compact")
    assert (absent.returncode, absent.stderr) == (0, b"")
    _assert_json_stdout(absent, {"hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": "Reply in the language the user writes in.",
    }})


def test_precompact_refreshes_existing_handoff_only(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    handoff.write_text(
        "# Handoff\n## Goal\nKeep me\n<!-- PURSER:STATE:START -->\nold state\n<!-- PURSER:STATE:END -->\n",
        encoding="utf-8",
    )
    transcript = _root / "transcript.jsonl"
    transcript.write_text(json.dumps({
        "type": "assistant", "message": {"role": "assistant", "usage": {"input_tokens": 321}},
    }) + "\n" + json.dumps({"type": "user", "message": {"role": "user", "content": "retain recent marker"}}) + "\n",
        encoding="utf-8",
    )
    refreshed = _run("PreCompact", repo, state, session_id="compact-refresh", transcript_path=str(transcript))
    assert (refreshed.returncode, refreshed.stdout, refreshed.stderr) == (0, b"", b"")
    content = handoff.read_text(encoding="utf-8")
    assert "Context tokens: 321 [transcript]" in content
    assert "old state" not in content
    assert "<!-- PURSER:STATE:START -->" in content and "<!-- PURSER:STATE:END -->" in content
    assert "retain recent marker" in content

    missing = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    missing.unlink()
    no_handoff = _run("PreCompact", repo, state, session_id="compact-no-handoff", transcript_path=str(transcript))
    assert (no_handoff.returncode, no_handoff.stdout, no_handoff.stderr) == (0, b"", b"")
    assert not missing.exists()


def test_stop_silent_and_fallback_block_contract(isolated):
    _root, repo, state = isolated
    satisfied = _run("Stop", repo, state, session_id="satisfied-stop", last_assistant_message="Done.")
    assert (satisfied.returncode, satisfied.stdout, satisfied.stderr) == (0, b"", b"")

    session = "fallback-stop"
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({
        "session_id": session,
        "fallback_pending": {
            "line": "> ⚓ **Switched to `model-x` for project `repo`.**",
            "project": "repo", "model": "model-x", "blocked": False,
            "notice": False, "delivered": False,
        },
    }), encoding="utf-8")
    blocked = _run("Stop", repo, state, session_id=session, last_assistant_message="The work is complete.")
    expected_reason = (
        "On your next reply, render exactly one bold quoted question line in the user's language as `> ⚓ **...**`. "
        "Translate the line below while keeping ⚓ and the model name; include no other question line.\n\n"
        "> ⚓ **Switched to `model-x` for project `repo`.**\n\n"
        "Your previous reply is already visible to the user: write only the lines requested above, without repeating any of its text."
    )
    assert blocked.returncode == 0 and blocked.stderr == b""
    _assert_json_stdout(blocked, {"decision": "block", "reason": expected_reason})
    pending = json.loads(path.read_text(encoding="utf-8"))["fallback_pending"]
    assert pending["blocked"] is True
    events = [json.loads(line) for file in (state / "telemetry" / "events").glob("????-??/*.jsonl")
              for line in file.read_text(encoding="utf-8").splitlines()]
    [event] = [event for event in events if event["event_type"] == "STOP_BLOCKED"]
    assert event["session_id"] == session
    assert event["payload"] == {"reason_kinds": ["fallback"], "reason_count": 1}


@pytest.mark.parametrize("event", ["UserPromptSubmit", "SessionStart", "PreCompact", "Stop"])
def test_malformed_input_is_silent_success_for_each_lifecycle_event(isolated, event):
    _root, _repo, state = isolated
    result = _run_contract_hook(event, b"{invalid json", state)
    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")
