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
