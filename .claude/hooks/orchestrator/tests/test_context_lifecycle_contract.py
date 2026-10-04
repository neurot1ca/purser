import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import time
import uuid
from datetime import datetime, timedelta, timezone

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


def test_user_prompt_compaction_advice_and_regrow_contract(isolated):
    root, repo, state = isolated
    session = "compaction-advice-contract"
    transcript = root / "transcript.jsonl"
    initial_timestamp = datetime.now(timezone.utc) + timedelta(hours=1)
    records = [
        {"type": "user", "message": {"role": "user", "content": "Continue the task."}},
        {"type": "assistant", "timestamp": initial_timestamp.isoformat().replace("+00:00", "Z"), "message": {
            "role": "assistant", "content": "Working.", "usage": {"input_tokens": 260000},
        }},
    ]
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")

    first = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.",
                 transcript_path=str(transcript))
    assert first.returncode == 0 and first.stderr == b""
    advice = json.loads(first.stdout)["hookSpecificOutput"]["additionalContext"]
    assert "Purser: 260,000 context tokens; trigger(s): threshold." in advice
    assert "Context passed 260K." in advice
    saved = json.loads(_advisor_state(state, repo).read_text(encoding="utf-8"))
    notice = saved["notice_pending"]
    assert saved["last_suggested_context"] == 260000
    assert notice["context"] == 260000 and notice["triggers"] == ["threshold"]
    assert notice["advice_written"] is True and notice["advice_turn"] == 2
    assert saved["compaction_outcome_pending"] == {
        "notice_id": notice["notice_id"], "pre_context_tokens": 260000,
        "advice_turn": 2, "compaction_observed": False,
    }

    saved["notice_pending"] = None
    saved["compaction_outcome_pending"] = None
    _advisor_state(state, repo).write_text(json.dumps(saved), encoding="utf-8")
    records[-1]["timestamp"] = (initial_timestamp - timedelta(hours=2)).isoformat().replace("+00:00", "Z")
    records[-1]["message"]["usage"]["input_tokens"] = 319999
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")
    suppressed = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.",
                      transcript_path=str(transcript))
    assert (suppressed.returncode, suppressed.stdout, suppressed.stderr) == (0, b"", b"")
    records[-1]["message"]["usage"]["input_tokens"] = 320000
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")
    regrown = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.",
                   transcript_path=str(transcript))
    assert regrown.returncode == 0 and regrown.stderr == b""
    assert "Purser: 320,000 context tokens; trigger(s): idle." in json.loads(regrown.stdout)["hookSpecificOutput"]["additionalContext"]


def test_user_prompt_retry_context_is_consumed_and_reset(isolated):
    _root, repo, state = isolated
    session = "prompt-retry-contract"
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"session_id": session, "codex_retry_count": 2,
                                "codex_retry_pending": "Retry the denied codex_result call."}),
                    encoding="utf-8")
    result = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.")
    assert result.returncode == 0 and result.stderr == b""
    assert json.loads(result.stdout)["hookSpecificOutput"]["additionalContext"] == (
        "Previous turn ended with an unfinished Purser step: Retry the denied codex_result call."
    )
    saved = json.loads(path.read_text(encoding="utf-8"))
    assert saved["codex_retry_count"] == 0 and saved["codex_retry_pending"] is False


def test_user_prompt_queues_eligible_maintenance_notice_like_python(isolated, monkeypatch):
    root, repo, state = isolated
    go_command = os.environ.get("PURSER_HOOK_CMD_USERPROMPTSUBMIT")
    if not go_command:
        pytest.skip("set PURSER_HOOK_CMD_USERPROMPTSUBMIT to the Go hook to run the dual-runtime contract")
    monkeypatch.setenv("CLAUDE_PLUGIN_ROOT", str(Path(__file__).parents[4]))
    codex_home = root / "codex"
    monkeypatch.setenv("CODEX_HOME", str(codex_home))
    now = datetime.now(timezone.utc)
    rollout = codex_home / "sessions" / now.strftime("%Y/%m/%d") / "rollout-maintenance-contract.jsonl"
    rollout.parent.mkdir(parents=True)
    rollout.write_text(json.dumps({
        "timestamp": now.isoformat().replace("+00:00", "Z"),
        "payload": {"rate_limits": {"primary": {
            "window_minutes": 300, "used_percent": 40,
            "resets_at": (now + timedelta(hours=2)).timestamp(),
        }}},
    }) + "\n", encoding="utf-8")
    session = "maintenance-prompt-contract"
    maintenance = state / "projects" / _project_key(repo) / "maintenance"
    reports = maintenance / "reports" / "cleanup"
    reports.mkdir(parents=True)
    (maintenance / "opt-in.json").write_text(json.dumps({
        "project_root": str(repo.resolve()), "enabled": True,
    }), encoding="utf-8")
    report = reports / "maintenance.notice.json"
    report.write_text(json.dumps({
        "report_type": "cleanup", "report_path": str(reports / "maintenance.md"),
        "finding_count": 1, "findings": [{"path_line": "src/unused.py:12", "symbol": "unused", "confidence": "high", "evidence": "No references found."}],
        "cycles": {}, "done": False, "pending": False,
    }), encoding="utf-8")
    original_report = report.read_text(encoding="utf-8")
    monkeypatch.delenv("PURSER_HOOK_CMD_USERPROMPTSUBMIT", raising=False)
    python_result = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.")
    assert python_result.returncode == 0 and python_result.stderr == b""
    python_state = json.loads(_advisor_state(state, repo).read_text(encoding="utf-8"))
    python_report = json.loads(report.read_text(encoding="utf-8"))

    _advisor_state(state, repo).unlink()
    report.write_text(original_report, encoding="utf-8")
    monkeypatch.setenv("PURSER_HOOK_CMD_USERPROMPTSUBMIT", go_command)
    go_result = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.")
    assert go_result.returncode == 0 and go_result.stderr == b""
    if go_result.stdout != python_result.stdout:
        for index, (expected, actual) in enumerate(zip(python_result.stdout, go_result.stdout)):
            if expected != actual:
                pytest.fail(f"runtime output differs at {index}: Python={python_result.stdout[max(0, index-100):index+250]!r}; Go={go_result.stdout[max(0, index-100):index+250]!r}")
        pytest.fail(f"runtime output lengths differ: Python={len(python_result.stdout)}, Go={len(go_result.stdout)}")
    assert json.loads(_advisor_state(state, repo).read_text(encoding="utf-8")) == python_state
    assert json.loads(report.read_text(encoding="utf-8")) == python_report


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


def test_session_start_compact_source_refreshes_recent_turns_and_truncates_handoff(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    filler = "H" * 7000
    handoff.write_text(
        "# Purser handoff\n## Goal\nUser language: English\n## State\n" + filler
        + "UNCLIPPED_TAIL\n<!-- PURSER:RECENT:START -->\nstale turn\n"
        "<!-- PURSER:RECENT:END -->\n",
        encoding="utf-8",
    )
    transcript = _root / "transcript.jsonl"
    records = [
        {"type": "user", "message": {"role": "user", "content": "RECENT_USER_MARKER"}},
        {"type": "assistant", "message": {"role": "assistant", "content": "RECENT_ASSISTANT_MARKER"}},
        {"type": "system", "subtype": "compact_boundary"},
    ]
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")

    result = _run("SessionStart", repo, state, session_id="compact-recent", source="compact",
                  transcript_path=str(transcript))

    assert (result.returncode, result.stderr) == (0, b"")
    output = json.loads(result.stdout)
    context = output["hookSpecificOutput"]["additionalContext"]
    assert result.stdout == json.dumps(output, ensure_ascii=False).encode("utf-8")
    assert output["hookSpecificOutput"]["hookEventName"] == "SessionStart"
    assert len(context) == 6000
    assert context.startswith(
        "Purser handoff (historical reference, written before the context was compacted or cleared; "
        "verify against git and files before acting; do not re-run commands from it; skip any listed "
        "file already present in context):\n# Purser handoff\n## Goal\nUser language: English\n## State\n"
    )
    assert "UNCLIPPED_TAIL" not in context
    assert "stale turn" not in context
    assert "<!-- PURSER:RECENT:START -->" in context
    assert "<!-- PURSER:RECENT:END -->" in context
    assert "RECENT_USER_MARKER" in context
    assert "RECENT_ASSISTANT_MARKER" in context
    assert "... truncated; read the full file at " + str(handoff) in context
    # Captured UTC is the only volatile field in the refreshed recent-history block.
    assert re.search(r"Captured UTC: \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+00:00", context)


def test_session_start_async_duration_followup_and_accuracy_contract(isolated):
    import datetime

    root, repo, state = isolated
    key = _project_key(repo)
    events = state / "telemetry" / "events" / datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m") / "session_contract.jsonl"
    events.parent.mkdir(parents=True)
    # event_timestamp is volatile; the contract asserts only stable rendered text.
    timestamp = datetime.datetime.now(datetime.timezone.utc).isoformat()
    records = [{"event_type": "IMPLEMENTER_FINISHED", "project_key": key, "ts_utc": timestamp,
                "payload": {"route": "mechanical", "duration_ms": 120000, "implementer": "codex-main",
                            "eta_minutes": 2}} for _ in range(10)]
    events.write_text("".join(json.dumps(item) + "\n" for item in records), encoding="utf-8")
    accuracy = state / "projects" / key / "duration_accuracy.json"
    accuracy.parent.mkdir(parents=True)
    accuracy.write_text(json.dumps({"compacts": 4, "errors": {"codex-main": 100.0}}), encoding="utf-8")

    call = "codex_exec_async({})"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    session_id, tool_use_id = "prior-session", "tool-use-contract"
    call_key = hashlib.sha256(f"{session_id}\0{tool_use_id}".encode()).hexdigest()
    index = {"jobId": job_id, "project_key": key, "session_id": session_id, "call": call,
             "call_key": call_key, "stage": "WAITING"}
    pending = {"job_id": job_id, "async_call": call, "session_id": session_id,
               "project_key": key, "tool_use_id": tool_use_id, "task_id": "contract task"}
    job = {"jobId": job_id, "call": call, "state": "RUNNING"}
    for directory, name, value in ((state / "async_jobs", job_id + ".json", index),
                                   (state / "pending", call_key + ".json", pending),
                                   (state / "codex_exec_mcp" / "jobs", job_id + ".json", job)):
        directory.mkdir(parents=True, exist_ok=True)
        (directory / name).write_text(json.dumps(value), encoding="utf-8")

    expected_duration = "Purser: typical Codex durations here (median/p90, n): mechanical 2/2 min (n=10)."
    expected_accuracy = (
        'Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, '
        'model names, numbers, and commands unchanged: "> ⚓ **Codex\'s task duration estimates have improved '
        'through the self-learning system! The estimated margin of error is now 0%.**"'
    )
    expected_followup = (
        f"Purser (jobId={job_id}, for Claude only): Render the line in the user's language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: "
            '"> ⚓ **The Codex task I started before the app closed (contract task) is still running while the app is closed. I\'ll let you know when it finishes.**" '
        "When it finishes, ask for the user's agreement before calling codex_result with adopt: true. Do not show the jobId to the user."
    )
    result = _run("SessionStart", repo, state, session_id="async-successor", source="compact")
    assert (result.returncode, result.stderr) == (0, b"")
    _assert_json_stdout(result, {"hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": "Reply in the language the user writes in.\n" + expected_duration + "\n" + expected_followup + "\n" + expected_accuracy,
    }})


def _seed_async_followup(runtime_state, project_key, prior_session, task, **job_values):
    call = f'codex_exec_async({json.dumps({"task": task})})'
    job_id = hashlib.sha256(call.encode()).hexdigest()
    tool_use_id = "followup-" + task
    call_key = hashlib.sha256(f"{prior_session}\0{tool_use_id}".encode()).hexdigest()
    index = {"jobId": job_id, "project_key": project_key, "session_id": prior_session,
             "call": call, "call_key": call_key, "stage": "WAITING"}
    pending = {"job_id": job_id, "async_call": call, "session_id": prior_session,
               "project_key": project_key, "tool_use_id": tool_use_id, "task_id": task}
    job = {"jobId": job_id, "call": call, "state": "RUNNING", **job_values}
    for directory, name, value in ((runtime_state / "async_jobs", job_id + ".json", index),
                                   (runtime_state / "pending", call_key + ".json", pending),
                                   (runtime_state / "codex_exec_mcp" / "jobs", job_id + ".json", job)):
        directory.mkdir(parents=True, exist_ok=True)
        (directory / name).write_text(json.dumps(value), encoding="utf-8")
    return job_id


@pytest.mark.parametrize("case", [
    "same-session-terminal", "same-session-running", "observed-terminal",
    "other-session-terminal", "stalled", "resumed-other-session",
])
def test_session_start_async_followup_branches_match_contract(isolated, case):
    root, repo, state = isolated
    key = _project_key(repo)
    current = "current-session"
    prior = current if case in {"same-session-terminal", "same-session-running", "observed-terminal"} else "prior-session"
    task = "branch task"
    job_values = {}
    expected_line = ""
    if case in {"same-session-terminal", "observed-terminal"}:
        job_values["state"] = "FAILED"
        if case == "observed-terminal":
            job_values.update(waiterObservedTerminalState="FAILED", waiterObservedTerminalAt=123)
    elif case == "same-session-running":
        job_values.update(state="QUEUED", resumedAfterInterruption=1)
    elif case == "other-session-terminal":
        job_values["state"] = "SUCCEEDED"
    elif case == "stalled":
        last_activity = time.time() - (20 * 60 + 30)
        job_values.update(errorCode="STALLED", stallDetectedAt=time.time(),
                          liveness={"lastActivityAt": last_activity})
    elif case == "resumed-other-session":
        job_values["resumedAfterInterruption"] = 2
    job_id = _seed_async_followup(state, key, prior, task, **job_values)
    prefix = f"Purser (jobId={job_id}, for Claude only): "
    translate = ('Render the line in the user\'s language, translating from the English text and keeping ⚓, markdown, '
                 'model names, numbers, and commands unchanged: "')
    if case == "same-session-terminal":
        expected_line = prefix + "Call codex_result with this jobId now, without adopt or asking the user. Do not show the jobId to the user."
    elif case == "same-session-running":
        expected_line = (prefix + translate + '> ⚓ **The Codex task (branch task) was interrupted when the computer shut down; I resumed it from where it left off.**" '
                         "Start purser_wait.py for this jobId in the background (use the PowerShell tool on Windows); call codex_result when it finishes, without adopt or asking the user. Do not show the jobId to the user.")
    elif case == "observed-terminal":
        expected_line = ""
    elif case == "other-session-terminal":
        expected_line = (prefix + translate + '> ⚓ **The Codex task I started before the app closed (branch task) kept running while the app was closed and has finished. Would you like me to retrieve and review its result?**" '
                         "Ask for the user's agreement before calling codex_result with adopt: true. Do not show the jobId to the user.")
    elif case == "stalled":
        expected_line = (prefix + "Do not call codex_result yet. Restart purser_wait.py for this same jobId in the background (use the PowerShell tool on Windows); continue waiting. Tell the user in the user's language exactly one line, translated from: \""
                         "> ⚓ **Purser has seen no worker activity for 20 minutes.** The job is still running; I'm checking its status.\" Do not show the jobId to the user.")
    else:
        expected_line = (prefix + translate + '> ⚓ **The Codex task (branch task) was interrupted when the computer shut down; I resumed it from where it left off.**" '
                         + translate + '> ⚓ **The Codex task I started before the app closed (branch task) is still running while the app is closed. I\'ll let you know when it finishes.**" '
                         "When it finishes, ask for the user's agreement before calling codex_result with adopt: true. Do not show the jobId to the user.")
    result = _run("SessionStart", repo, state, session_id=current, source="resume")
    assert (result.returncode, result.stderr) == (0, b"")
    context = "Reply in the language the user writes in."
    if expected_line:
        context += "\n" + expected_line
    _assert_json_stdout(result, {"hookSpecificOutput": {
        "hookEventName": "SessionStart", "additionalContext": context,
    }})


def test_session_start_async_followup_file_order_and_five_job_cap(isolated):
    _root, repo, state = isolated
    key = _project_key(repo)
    now = time.time()
    records = []
    # Create in reverse order so the expected order is determined by mtime.
    for order in reversed(range(7)):
        task = f"ordered task {order}"
        job_id = _seed_async_followup(state, key, "prior-session", task)
        index_path = state / "async_jobs" / f"{job_id}.json"
        os.utime(index_path, (now + order, now + order))
        records.append((order, job_id, task))
    expected = [f"Purser (jobId={job_id}, for Claude only): Render the line in the user's language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged: \"> ⚓ **The Codex task I started before the app closed ({task}) is still running while the app is closed. I'll let you know when it finishes.**\" When it finishes, ask for the user's agreement before calling codex_result with adopt: true. Do not show the jobId to the user."
                for _order, job_id, task in sorted(records)[:5]]
    result = _run("SessionStart", repo, state, session_id="ordered-successor", source="resume")
    assert (result.returncode, result.stderr) == (0, b"")
    _assert_json_stdout(result, {"hookSpecificOutput": {
        "hookEventName": "SessionStart",
        "additionalContext": "Reply in the language the user writes in.\n" + "\n".join(expected),
    }})


def test_session_start_maintenance_activity_and_notice_contract(isolated, monkeypatch):
    import datetime

    _root, _temporary_repo, state = isolated
    # The Python hook intentionally detaches a 15-minute check with repo as cwd.
    # Use the checkout root so that its directory is not removed during teardown.
    repo = Path(__file__).parents[4].resolve()
    key = _project_key(repo)
    maintenance = state / "projects" / key / "maintenance"
    maintenance.mkdir(parents=True)
    (maintenance / "opt-in.json").write_text(json.dumps({"project_root": str(repo.resolve()), "enabled": True}), encoding="utf-8")
    rollout = _root / "codex" / "sessions" / datetime.datetime.now(datetime.timezone.utc).strftime("%Y/%m/%d") / "rollout-contract.jsonl"
    rollout.parent.mkdir(parents=True)
    # quota_timestamp and activity.at are volatile and checked only for freshness/type.
    stamp = datetime.datetime.now(datetime.timezone.utc).isoformat()
    rollout.write_text(json.dumps({"timestamp": stamp, "payload": {"rate_limits": {}}}), encoding="utf-8")
    monkeypatch.setenv("CODEX_HOME", str(rollout.parents[4]))
    result = _run("SessionStart", repo, state, session_id="maintenance-contract", source="resume")
    assert (result.returncode, result.stderr) == (0, b"")
    actual = json.loads(result.stdout)
    additional = actual["hookSpecificOutput"]["additionalContext"]
    # maintenance_cli is the only volatile text: it is the installed interpreter and hook path.
    maintenance_cli = f'"{__import__("sys").executable}" "{Path(__file__).parents[1] / "maintenance_reports.py"}"'
    additional = additional.replace(maintenance_cli, "<maintenance_cli>")
    expected_context = (
        "Reply in the language the user writes in.\n"
        "Render the maintenance API-cost consent question below in the user's language, translating from the English text and preserving ⚓ and bold. "
        "If the user says yes, run `<maintenance_cli> consent yes --repo \"<repository-root>\"`; "
        "if no, run `<maintenance_cli> consent no --repo \"<repository-root>\"`. Do not infer consent.\n"
        "> ⚓ **While you work, Purser can scan your project in the background with its proactive cleanup module. "
        "This scan is billed to your API account. Run it?**"
    )
    assert actual["hookSpecificOutput"]["hookEventName"] == "SessionStart"
    assert additional == expected_context, (repr(additional), repr(expected_context))
    activity = json.loads((maintenance / "last-activity.json").read_text(encoding="utf-8"))
    assert isinstance(activity["at"], (int, float))
    assert abs(activity["at"] - time.time()) < 10
    # The hook detaches its opt-in maintenance check with this repo as cwd.
    time.sleep(2)


@pytest.mark.parametrize("marker", [None, "malformed"])
def test_session_start_maintenance_missing_or_malformed_opt_in_is_ignored(isolated, marker):
    _root, repo, state = isolated
    maintenance = state / "projects" / _project_key(repo) / "maintenance"
    if marker is not None:
        maintenance.mkdir(parents=True)
        (maintenance / "opt-in.json").write_text("{", encoding="utf-8")
    result = _run("SessionStart", repo, state, session_id="maintenance-invalid", source="resume")
    assert (result.returncode, result.stderr) == (0, b"")
    _assert_json_stdout(result, {"hookSpecificOutput": {
        "hookEventName": "SessionStart", "additionalContext": "Reply in the language the user writes in.",
    }})
    assert not (maintenance / "last-activity.json").exists()


def test_session_start_maintenance_notice_is_delivered_once(isolated):
    _root, repo, state = isolated
    key = _project_key(repo)
    maintenance = state / "projects" / key / "maintenance"
    maintenance.mkdir(parents=True)
    consent_path = maintenance / "consent.json"
    advisor = _advisor_state(state, repo)
    advisor.parent.mkdir(parents=True)
    notice_text = ("> ⚓ **While you work, Purser can scan your project in the background with its proactive cleanup module. "
                   "This scan is billed to your API account. Run it?**")
    advisor.write_text(json.dumps({
        "session_id": "maintenance-delivered", "maintenance_pending": {
            "notice_file": str(consent_path), "text": notice_text, "evidence": ["API"],
            "report_type": "cleanup", "blocked": False, "delivered": True,
        },
    }), encoding="utf-8")
    result = _run("SessionStart", repo, state, session_id="maintenance-delivered", source="resume")
    assert (result.returncode, result.stderr) == (0, b"")
    _assert_json_stdout(result, {"hookSpecificOutput": {
        "hookEventName": "SessionStart", "additionalContext": "Reply in the language the user writes in.",
    }})
    persisted = json.loads(advisor.read_text(encoding="utf-8"))
    assert persisted["maintenance_pending"]["delivered"] is True


def test_session_start_emits_compaction_outcome_contract(isolated):
    _root, repo, state = isolated
    session = "outcome-contract"
    advisor = _advisor_state(state, repo)
    advisor.parent.mkdir(parents=True)
    advisor.write_text(json.dumps({
        "session_id": session, "context_prompt_count": 4,
        "notice_pending": {"notice_id": "0123456789abcdef0123456789abcdef", "context": 120000,
                           "percentage": 85, "handoff": "handoff", "advice_written": True, "advice_turn": 3},
        "compaction_outcome_pending": {"notice_id": "0123456789abcdef0123456789abcdef", "pre_context_tokens": 120000,
                                       "advice_turn": 3, "compaction_observed": False},
    }), encoding="utf-8")
    result = _run("SessionStart", repo, state, session_id=session, source="compact")
    assert (result.returncode, result.stderr) == (0, b"")
    event_files = list((state / "telemetry" / "events").glob("????-??/session_outcome-contract.jsonl"))
    assert len(event_files) == 1
    events = [json.loads(line) for path in event_files for line in path.read_text(encoding="utf-8").splitlines()]
    [event] = [item for item in events if item["event_type"] == "COMPACTION_ADVICE_OUTCOME"]
    assert set(event) == {
        "schema_version", "event_id", "ts_utc", "event_type", "task_id", "project_key",
        "session_id", "repo", "branch", "agent_id", "agent_type", "payload",
    }
    assert event["schema_version"] == 1
    uuid.UUID(event["event_id"])
    assert event["ts_utc"].endswith("Z")
    assert event["session_id"] == session
    assert event["payload"] == {
        "advice_written": True, "compaction_observed": True,
        "notice_id": "0123456789abcdef0123456789abcdef", "pre_context_tokens": 120000,
    }

    once_session = "outcome-once"
    transcript = _root / "outcome-transcript.jsonl"
    transcript.write_text("".join(json.dumps(record) + "\n" for record in (
        {"type": "system", "subtype": "compact_boundary"},
        {"type": "assistant", "message": {"role": "assistant", "usage": {
            "input_tokens": 300, "cache_read_input_tokens": 20,
            "cache_creation_input_tokens": 5,
        }}},
    )), encoding="utf-8")
    advisor.write_text(json.dumps({
        "session_id": once_session, "context_prompt_count": 6,
        "compaction_outcome_pending": {
            "notice_id": "0123456789abcdef0123456789abcdef", "pre_context_tokens": 120000,
            "advice_turn": 3, "compaction_observed": False,
        },
    }), encoding="utf-8")
    for _ in range(2):
        repeated = _run("SessionStart", repo, state, session_id=once_session, source="compact",
                        transcript_path=str(transcript))
        assert (repeated.returncode, repeated.stderr) == (0, b"")
    once_events = [json.loads(line) for path in (state / "telemetry" / "events").glob("????-??/session_outcome-once.jsonl")
                   for line in path.read_text(encoding="utf-8").splitlines()]
    [once_event] = [item for item in once_events if item["event_type"] == "COMPACTION_ADVICE_OUTCOME"]
    assert once_event["payload"] == {
        "advice_written": True, "compaction_observed": True,
        "notice_id": "0123456789abcdef0123456789abcdef", "pre_context_tokens": 120000,
        "post_context_tokens": 325, "elapsed_turns": 3,
    }
    assert json.loads(advisor.read_text(encoding="utf-8"))["compaction_outcome_pending"] is None

    # Invalid pending data is discarded, and clear events discard valid pending data without emission.
    malformed_session = "outcome-malformed"
    advisor.write_text(json.dumps({
        "session_id": malformed_session,
        "compaction_outcome_pending": {"notice_id": "bad", "pre_context_tokens": "120000", "advice_turn": 1},
    }), encoding="utf-8")
    malformed = _run("SessionStart", repo, state, session_id=malformed_session, source="compact")
    assert (malformed.returncode, malformed.stderr) == (0, b"")
    assert json.loads(advisor.read_text(encoding="utf-8"))["compaction_outcome_pending"] is None
    cleared_session = "outcome-cleared"
    advisor.write_text(json.dumps({
        "session_id": cleared_session,
        "compaction_outcome_pending": {
            "notice_id": "0123456789abcdef0123456789abcdef", "pre_context_tokens": 120000,
            "advice_turn": 3, "compaction_observed": False,
        },
    }), encoding="utf-8")
    cleared = _run("SessionStart", repo, state, session_id=cleared_session, source="clear")
    assert (cleared.returncode, cleared.stderr) == (0, b"")
    assert json.loads(advisor.read_text(encoding="utf-8"))["compaction_outcome_pending"] is None
    assert not list((state / "telemetry" / "events").glob("????-??/session_outcome-malformed.jsonl"))
    assert not list((state / "telemetry" / "events").glob("????-??/session_outcome-cleared.jsonl"))


def test_compact_boundary_removes_stale_recent_block_without_recent_turns(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    body = (
        "# Purser handoff\n## Goal\nUser language: English\n## State\nKeep this state\n"
        "<!-- PURSER:RECENT:START -->\nstale recent text\n<!-- PURSER:RECENT:END -->\n"
    )
    handoff.write_text(body, encoding="utf-8")
    transcript = _root / "transcript.jsonl"
    records = [
        {"type": "", "message": {"role": "user", "content": "TYPE_FALLBACK_MARKER"}},
        {"type": "user", "message": {"role": "user", "content": "\u00a0[SYSTEM NOTIFICATION - NOT USER INPUT]"}},
        {"type": "system", "subtype": "compact_boundary"},
    ]
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")

    result = _run("SessionStart", repo, state, session_id="compact-empty-recent", source="compact",
                  transcript_path=str(transcript))

    assert (result.returncode, result.stderr) == (0, b"")
    output = json.loads(result.stdout)
    context = output["hookSpecificOutput"]["additionalContext"]
    assert result.stdout == json.dumps(output, ensure_ascii=False).encode("utf-8")
    assert "Keep this state" in context
    assert "stale recent text" not in context
    assert "TYPE_FALLBACK_MARKER" not in context
    assert "<!-- PURSER:RECENT:START -->" not in context
    assert "<!-- PURSER:RECENT:END -->" not in context


def test_compact_handoff_rstrips_unicode_whitespace_byte_for_byte(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    body = "# Purser handoff\n## Goal\nUser language: English\nTrailing content\t \u00a0\n"
    handoff.write_text(body, encoding="utf-8")

    result = _run("SessionStart", repo, state, session_id="compact-rstrip", source="compact")

    assert (result.returncode, result.stderr) == (0, b"")
    expected_context = (
        "Purser handoff (historical reference, written before the context was compacted or cleared; "
        "verify against git and files before acting; do not re-run commands from it; skip any listed file already present in context):\n"
        + body.rstrip() + f"\nFull file: {handoff}"
    )
    expected = {"hookSpecificOutput": {
        "hookEventName": "SessionStart", "additionalContext": expected_context,
    }}
    assert result.stdout == json.dumps(expected, ensure_ascii=False).encode("utf-8")


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

    handoff.parent.mkdir(parents=True, exist_ok=True)
    malformed = "# Handoff\n<!-- PURSER:STATE:END -->\nunchanged\n<!-- PURSER:STATE:START -->\n"
    handoff.write_text(malformed, encoding="utf-8")
    skipped = _run("PreCompact", repo, state, session_id="compact-malformed-state", transcript_path=str(transcript))
    assert (skipped.returncode, skipped.stdout, skipped.stderr) == (0, b"", b"")
    assert handoff.read_text(encoding="utf-8") == malformed


def test_precompact_refreshes_recent_turns_without_changing_pending_outcome(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    handoff.write_text(
        "# Handoff\n<!-- PURSER:STATE:START -->\nold state\n<!-- PURSER:STATE:END -->\n"
        "<!-- PURSER:RECENT:START -->\nstale recent\n<!-- PURSER:RECENT:END -->\n",
        encoding="utf-8",
    )
    advisor = _advisor_state(state, repo)
    pending = {"notice_id": "contract-pending"}
    advisor.write_text(json.dumps({"session_id": "precompact-pending", "compaction_outcome_pending": pending}),
                       encoding="utf-8")
    transcript = _root / "transcript.jsonl"
    records = [
        {"type": "user", "message": {"role": "user", "content": "first old request"}},
        {"type": "user", "isMeta": True, "message": {"role": "user", "content": "exclude meta request"}},
        {"type": "assistant", "message": {"role": "assistant", "content": "latest assistant answer",
                                              "usage": {"input_tokens": 42}}},
        {"type": "user", "message": {"role": "user", "content": "last user request"}},
    ]
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")

    result = _run("PreCompact", repo, state, session_id="precompact-pending", transcript_path=str(transcript))

    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")
    content = handoff.read_text(encoding="utf-8")
    assert "Context tokens: 42 [transcript]" in content
    assert "old state" not in content
    assert "stale recent" not in content
    assert "first old request" in content and "last user request" in content
    assert "exclude meta request" not in content
    assert "latest assistant answer" in content
    assert json.loads(advisor.read_text(encoding="utf-8"))["compaction_outcome_pending"] == pending


def test_precompact_records_transcript_facts_and_active_plan(isolated):
    _root, repo, state = isolated
    handoff = state / "projects" / _project_key(repo) / "telemetry" / "purser-handoff.md"
    handoff.parent.mkdir(parents=True)
    handoff.write_text("<!-- PURSER:STATE:START -->old<!-- PURSER:STATE:END -->\n", encoding="utf-8")
    codex_call = "mcp__plugin_purser_codex-main__codex"
    transcript = _root / "facts-transcript.jsonl"
    records = [
        {"type": "user", "message": {"role": "user", "content": "Please commit and review"}},
        {"type": "assistant", "timestamp": "2020-01-01T00:00:00Z", "message": {"role": "assistant", "content": [
            {"type": "tool_use", "id": "commit-tool", "name": "Bash", "input": {"command": "git commit -am done"}},
            {"type": "tool_use", "id": "codex-tool", "name": codex_call,
             "input": {"prompt": "ORCH|task=review-1| review the change"}},
        ]}},
        {"type": "user", "message": {"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": "commit-tool", "content": "committed"},
            {"type": "tool_result", "tool_use_id": "codex-tool",
             "content": [{"type": "text", "text": json.dumps({"threadId": "thread-1", "claude_review_mode": "required"})}]},
        ]}},
    ]
    transcript.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")
    jobs = state / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    job_id = "a" * 64
    (jobs / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "taskId": "active-plan-m1of2", "phase": "implementation",
        "repoRoot": str(repo), "acceptedAt": time.time(), "state": "SUCCEEDED",
    }), encoding="utf-8")

    result = _run("PreCompact", repo, state, session_id="precompact-facts", transcript_path=str(transcript))

    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")
    content = handoff.read_text(encoding="utf-8")
    assert "Triggers: idle, commit [transcript]" in content
    assert f"- task=review-1 tool=codex threadId=thread-1 claude_review_mode=required" in content
    assert f"Plan active-plan: M1 SUCCEEDED (job={job_id}), next M1" in content


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


def test_stop_combines_pending_receipt_dispatch_and_version(isolated):
    _root, repo, state = isolated
    session = "combined-stop"
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({
        "session_id": session,
        "receipt_pending": {"model": "gpt-6-sol", "effort": "high", "blocked": False},
        "dispatch_pending": {"lines": ["task=review-1 threadId=thread-1"], "board": "BOARD: ready", "board_job_ids": [], "blocked": False},
        "version_pending": {"version": "9.8.7", "blocked": False, "delivered": True},
    }), encoding="utf-8")
    result = _run("Stop", repo, state, session_id=session, last_assistant_message="Done.")
    assert result.returncode == 0 and result.stderr == b""
    reason = json.loads(result.stdout)["reason"]
    assert reason.startswith("Write one line in the user's language as `> ⚓ **Codex done: ...**`")
    assert "task=review-1 threadId=thread-1" in reason
    assert "BOARD: ready" in reason
    assert "`9.8.7`" in reason
    pending = json.loads(path.read_text(encoding="utf-8"))
    assert all(pending[key]["blocked"] is True for key in ("receipt_pending", "dispatch_pending", "version_pending"))
    second = _run("Stop", repo, state, session_id=session, last_assistant_message="Done.")
    assert (second.returncode, second.stdout, second.stderr) == (0, b"", b"")


def test_stop_consumes_visible_pending_lines(isolated):
    _root, repo, state = isolated
    session = "visible-stop"
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True)
    active = state / "projects" / _project_key(repo) / "active-coder.json"
    active.write_text(json.dumps({
        "active_coder": "primary", "triggering_evidence": {"question_session": session, "question_shown": False},
        "reset_cycle_marker": None, "last_probe_time": None,
    }), encoding="utf-8")
    job_id = "b" * 64
    job_path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    job_path.parent.mkdir(parents=True)
    job_path.write_text(json.dumps({"jobId": job_id, "state": "SUCCEEDED", "repoRoot": str(repo),
                                    "boardDisplayCount": 0}), encoding="utf-8")
    path.write_text(json.dumps({
        "session_id": session,
        "receipt_pending": {"model": "gpt-6-sol", "effort": "high", "blocked": False},
        "dispatch_pending": {"lines": ["task=review-1 threadId=thread-1"], "board": "BOARD: ready", "board_job_ids": [job_id], "blocked": False},
        "version_pending": {"version": "9.8.7", "blocked": False, "delivered": True},
        "fallback_pending": {"line": "> ⚓ **Switched to `model-x`.**", "project": str(repo), "model": "model-x", "blocked": False, "notice": True},
    }), encoding="utf-8")
    message = "> ⚓ **Switched to `model-x`.**\nmodel gpt-6-sol effort high\ntask=review-1 threadId=thread-1\nBOARD: ready\n⚓ 9.8.7"
    result = _run("Stop", repo, state, session_id=session, last_assistant_message=message)
    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")
    pending = json.loads(path.read_text(encoding="utf-8"))
    assert all(pending[key] is None for key in ("receipt_pending", "dispatch_pending", "version_pending", "fallback_pending"))
    assert json.loads(active.read_text(encoding="utf-8"))["triggering_evidence"]["question_shown"] is True
    assert json.loads(job_path.read_text(encoding="utf-8"))["boardDisplayCount"] == 1


def test_p34_fallback_question_is_shown_before_answerable_on_next_turn_with_advice_disabled(isolated, monkeypatch):
    root, repo, state = isolated
    home = root / "home"
    (home / ".orchestrator").mkdir(parents=True)
    (home / ".orchestrator" / "config.toml").write_text("[compaction]\nenabled = false\n", encoding="utf-8")
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    session = "p34-fallback-lifecycle"
    question = "> ⚓ **Should Purser switch to `model-x`?**"
    active = state / "projects" / _project_key(repo) / "active-coder.json"
    active.parent.mkdir(parents=True, exist_ok=True)
    active.write_text(json.dumps({
        "active_coder": "primary",
        "triggering_evidence": {
            "question_session": session, "question_shown": False, "answerable": False,
        },
        "reset_cycle_marker": None, "last_probe_time": None,
    }), encoding="utf-8")
    advisor = _advisor_state(state, repo)
    advisor.parent.mkdir(parents=True, exist_ok=True)
    advisor.write_text(json.dumps({
        "session_id": session,
        "fallback_pending": {
            "line": question, "project": str(repo), "model": "model-x",
            "blocked": False, "notice": False, "delivered": False,
        },
    }), encoding="utf-8")

    first = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.")
    assert first.returncode == 0 and first.stderr == b""
    assert question in json.loads(first.stdout)["hookSpecificOutput"]["additionalContext"]
    evidence = json.loads(active.read_text(encoding="utf-8"))["triggering_evidence"]
    assert evidence["question_shown"] is False and evidence["answerable"] is False
    assert json.loads(advisor.read_text(encoding="utf-8"))["fallback_pending"]["delivered"] is True

    shown = _run("Stop", repo, state, session_id=session, last_assistant_message=question)
    assert (shown.returncode, shown.stdout, shown.stderr) == (0, b"", b"")
    evidence = json.loads(active.read_text(encoding="utf-8"))["triggering_evidence"]
    assert evidence["question_shown"] is True and evidence["answerable"] is False
    assert json.loads(advisor.read_text(encoding="utf-8"))["fallback_pending"] is None

    next_turn = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Yes.")
    assert (next_turn.returncode, next_turn.stdout, next_turn.stderr) == (0, b"", b"")
    evidence = json.loads(active.read_text(encoding="utf-8"))["triggering_evidence"]
    assert evidence["question_shown"] is True and evidence["answerable"] is True


@pytest.mark.parametrize("answer", [
    "The reply did not include the consent question.",
    "> ⚓ **Should Purser switch to `another-model`?**",
    "> ⚓ **Should Purser switch to `model-x`?**\n> ⚓ **An unrelated notice.**",
], ids=["p34-no-anchor", "p34-wrong-model", "p34-multiple-anchors"])
def test_p34_fallback_rejected_delivery_does_not_mark_question_shown(isolated, answer):
    _root, repo, state = isolated
    session = "p34-fallback-rejected"
    active = state / "projects" / _project_key(repo) / "active-coder.json"
    active.parent.mkdir(parents=True, exist_ok=True)
    active.write_text(json.dumps({
        "active_coder": "primary",
        "triggering_evidence": {
            "question_session": session, "question_shown": False, "answerable": False,
        },
        "reset_cycle_marker": None, "last_probe_time": None,
    }), encoding="utf-8")
    advisor = _advisor_state(state, repo)
    advisor.parent.mkdir(parents=True, exist_ok=True)
    advisor.write_text(json.dumps({
        "session_id": session,
        "fallback_pending": {
            "line": "> ⚓ **Should Purser switch to `model-x`?**",
            "project": str(repo), "model": "model-x", "blocked": False,
            "notice": False, "delivered": True,
        },
    }), encoding="utf-8")

    result = _run("Stop", repo, state, session_id=session, last_assistant_message=answer)
    assert result.returncode == 0 and result.stderr == b""
    assert json.loads(result.stdout)["decision"] == "block"
    evidence = json.loads(active.read_text(encoding="utf-8"))["triggering_evidence"]
    assert evidence["question_shown"] is False and evidence["answerable"] is False
    assert json.loads(advisor.read_text(encoding="utf-8"))["fallback_pending"]["blocked"] is True


def test_stop_closes_journal_once_and_records_version_block(isolated):
    _root, repo, state = isolated
    session = "journal-stop"
    prompt = _run("UserPromptSubmit", repo, state, session_id=session, prompt="Continue.")
    assert prompt.returncode == 0
    journal = state / "context_io" / "turns" / f"{_session_key(session)}.json"
    assert journal.is_file()
    turn = json.loads(journal.read_text(encoding="utf-8"))
    turn["read_count"] = turn["full_read_count"] = 1
    journal.write_text(json.dumps(turn), encoding="utf-8")
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({
        "session_id": session,
        "version_pending": {"version": "9.8.7", "blocked": False, "delivered": True},
    }), encoding="utf-8")
    result = _run("Stop", repo, state, session_id=session, last_assistant_message="Done.")
    assert json.loads(result.stdout)["decision"] == "block"
    assert not journal.exists()
    assert (state / "context_io" / "completed" / _session_key(session)).is_file()
    events = [json.loads(line) for file in (state / "telemetry" / "events").glob("????-??/*.jsonl")
              for line in file.read_text(encoding="utf-8").splitlines()]
    [turn_event] = [event for event in events if event["event_type"] == "CONTEXT_IO_TURN_RECORDED"]
    assert turn_event["payload"]["read_count"] == turn_event["payload"]["full_read_count"] == 1
    assert turn_event["event_id"] == str(uuid.uuid5(uuid.NAMESPACE_URL, "context-io:" + turn["turn_nonce"]))
    assert json.loads((state / "plugin-version.json").read_text(encoding="utf-8")) == {"announced_version": "9.8.7"}
    again = _run("Stop", repo, state, session_id=session, last_assistant_message="Done.")
    assert (again.returncode, again.stdout, again.stderr) == (0, b"", b"")
    events = [json.loads(line) for file in (state / "telemetry" / "events").glob("????-??/*.jsonl")
              for line in file.read_text(encoding="utf-8").splitlines()]
    assert len([event for event in events if event["event_type"] == "CONTEXT_IO_TURN_RECORDED"]) == 1


def test_stop_model_notice_shown_and_active_stop_silent(isolated):
    _root, repo, state = isolated
    session = "model-stop"
    model_path = state / "model-version-notice.json"
    model_path.parent.mkdir(parents=True)
    model_path.write_text(json.dumps({"pending": {
        "current": "openai/gpt-5.5", "new": "openai/gpt-5.6", "blocked": False,
    }}), encoding="utf-8")
    active = _run("Stop", repo, state, session_id=session, stop_hook_active=True,
                  last_assistant_message="Not shown.")
    assert (active.returncode, active.stdout, active.stderr) == (0, b"", b"")
    assert json.loads(model_path.read_text(encoding="utf-8"))["pending"]["blocked"] is False
    shown = _run("Stop", repo, state, session_id=session,
                 last_assistant_message="> ⚓ **New model version available: `openai/gpt-5.6` (you are on `openai/gpt-5.5`).**")
    assert (shown.returncode, shown.stdout, shown.stderr) == (0, b"", b"")
    saved = json.loads(model_path.read_text(encoding="utf-8"))
    assert saved["pending"] is None and saved["asked"] == ["openai/gpt-5.6"]


def test_stop_compaction_and_maintenance_notice(isolated):
    _root, repo, state = isolated
    session = "advice-stop"
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True)
    notice_file = state / "maintenance" / "consent.json"
    notice_file.parent.mkdir(parents=True)
    notice_file.write_text(json.dumps({"pending": True}), encoding="utf-8")
    path.write_text(json.dumps({
        "session_id": session,
        "notice_pending": {"notice_id": "a" * 32, "context": 120000, "percentage": 40,
                           "handoff": "handoff.md", "blocked": False},
        "maintenance_pending": {"notice_file": str(notice_file), "text": "> ⚓ **Run scan?**",
                                "evidence": ["Run scan?"], "blocked": False},
    }), encoding="utf-8")
    blocked = _run("Stop", repo, state, session_id=session, last_assistant_message="Done.")
    reason = json.loads(blocked.stdout)["reason"]
    assert "> ⚓ **Context passed 120K." in reason
    assert "maintenance API-cost consent question" in reason
    visible = _run("Stop", repo, state, session_id=session,
                   last_assistant_message="> ⚓ **Context passed 120K.**\n> ⚓ **Run scan?**")
    assert (visible.returncode, visible.stdout, visible.stderr) == (0, b"", b"")
    saved = json.loads(path.read_text(encoding="utf-8"))
    assert saved["notice_pending"] is None and saved["maintenance_pending"] is None
    assert json.loads(notice_file.read_text(encoding="utf-8")) == {"pending": False, "shown": True}


def test_stop_ignores_malformed_pending_state(isolated):
    _root, repo, state = isolated
    path = _advisor_state(state, repo)
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({
        "session_id": "malformed-stop",
        "fallback_pending": {"model": "model-x", "blocked": False},
        "dispatch_pending": {"lines": ["valid", 9], "board": "BOARD", "blocked": False},
        "receipt_pending": {"model": "model-x", "blocked": False},
    }), encoding="utf-8")
    result = _run("Stop", repo, state, session_id="malformed-stop", last_assistant_message="Done.")
    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")


@pytest.mark.parametrize("event", ["UserPromptSubmit", "SessionStart", "PreCompact", "Stop"])
def test_malformed_input_is_silent_success_for_each_lifecycle_event(isolated, event):
    _root, _repo, state = isolated
    result = _run_contract_hook(event, b"{invalid json", state)
    assert (result.returncode, result.stdout, result.stderr) == (0, b"", b"")
