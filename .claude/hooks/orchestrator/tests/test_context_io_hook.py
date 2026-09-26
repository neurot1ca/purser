import io
import hashlib
import json
import os
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import context_io_hook
import async_session_maintenance
import codex_exec_mcp


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


def test_session_start_reminds_unconsumed_jobs_and_caps_at_five(monkeypatch, tmp_path):
    state = tmp_path / "state"
    project = "project-key"
    jobs = [("finished-task", "SUCCEEDED"), ("running-task", "RUNNING")]
    jobs += [(f"extra-{number}", "SUCCEEDED") for number in range(4)]
    jobs += [("own-task", "SUCCEEDED")]
    for number, (task, job_state) in enumerate(jobs):
        call = f"call-{number}"
        job_id = hashlib.sha256(call.encode()).hexdigest()
        call_key = hashlib.sha256(f"old-session\0tool-{number}".encode()).hexdigest()
        session = "current-session" if task == "own-task" else "old-session"
        if session == "current-session":
            call_key = hashlib.sha256(f"current-session\0tool-{number}".encode()).hexdigest()
        for folder, name, value in (
            ("async_jobs", job_id, {"jobId": job_id, "call": call, "call_key": call_key,
                                     "session_id": session, "project_key": project}),
            ("pending", call_key, {"job_id": job_id, "async_call": call, "session_id": session,
                                   "tool_use_id": f"tool-{number}", "project_key": project, "task_id": task}),
            ("codex_exec_mcp/jobs", job_id, {"jobId": job_id, "call": call, "state": job_state,
                                              **({"hostPid": 123, "hostCreatedAt": 456,
                                                  "hostContained": True} if job_state == "RUNNING" else {})}),
        ):
            path = state / folder / f"{name}.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(value), encoding="utf-8")
            if folder == "async_jobs":
                os.utime(path, (100 + number, 100 + number))
    payload = {"hook_event_name": "SessionStart", "cwd": str(tmp_path), "session_id": "current-session"}
    input_raw = io.BytesIO(json.dumps(payload).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _root: project)
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: state)
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *_args: None)
    monkeypatch.setattr(codex_exec_mcp, "_windows_process_identity", lambda _pid: ("ALIVE", 456))
    assert context_io_hook.main() == 0
    lines = json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"].splitlines()
    assert len(lines) == 5
    assert "finished-task" in lines[0] and "bitince haber" not in lines[0]
    assert "running-task" in lines[1] and "bitince haber" in lines[1]
    assert "own-task" not in "\n".join(lines)  # The five older hints fill the cap.
    assert all("jobId=" in line for line in lines)
    for path in (state / "async_jobs").glob("*.json"):
        if json.loads(path.read_text(encoding="utf-8"))["session_id"] != "current-session":
            path.unlink()
    own = async_session_maintenance.followup_hints(state, project, "current-session")
    assert "Call codex_result" in own and "jobId=" in own
    assert "adopt or asking the user" in own
    assert "Tell the user" not in own


def test_session_start_job_hint_failure_is_fail_open(monkeypatch, tmp_path):
    monkeypatch.setattr(async_session_maintenance, "followup_hints", lambda *_args: (_ for _ in ()).throw(OSError("bad state")))
    payload = {"hook_event_name": "SessionStart", "cwd": str(tmp_path), "session_id": "new"}
    input_raw = io.BytesIO(json.dumps(payload).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: "restored context")
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(async_session_maintenance, "duration_hint", lambda *_args: None)
    assert context_io_hook.main() == 0
    assert "restored context" in output_raw.getvalue().decode("utf-8")


def test_resumed_hint_uses_approved_wording_and_ambiguous_job_is_skipped(tmp_path):
    state = tmp_path / "state"
    call = "resumed-call"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    call_key = hashlib.sha256(b"old-session\0tool-1").hexdigest()
    for folder in ("async_jobs", "pending", "codex_exec_mcp/jobs"):
        (state / folder).mkdir(parents=True, exist_ok=True)
    (state / "async_jobs" / f"{job_id}.json").write_text(json.dumps({
        "jobId": job_id, "call": call, "call_key": call_key,
        "session_id": "old-session", "project_key": "project-key"}), encoding="utf-8")
    (state / "pending" / f"{call_key}.json").write_text(json.dumps({
        "job_id": job_id, "async_call": call, "session_id": "old-session",
        "project_key": "project-key", "tool_use_id": "tool-1", "task_id": "task-a"}), encoding="utf-8")
    record = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    job = {"jobId": job_id, "call": call, "state": "RUNNING", "resumedAfterInterruption": 1}
    record.write_text(json.dumps(job), encoding="utf-8")
    hint = async_session_maintenance.followup_hints(state, "project-key", "new-session")
    assert "⚓ Codex işi (task-a) bilgisayar kapanınca yarıda kalmıştı; kaldığı yerden devam ettirdim." in hint
    own = async_session_maintenance.followup_hints(state, "project-key", "old-session")
    assert "Start purser_wait.py" in own and "without adopt" in own
    assert "⚓ Codex işi (task-a) bilgisayar kapanınca yarıda kalmıştı; kaldığı yerden devam ettirdim." in own
    assert "uygulama kapalıyken" not in own
    job["state"] = "SUCCEEDED"
    record.write_text(json.dumps(job), encoding="utf-8")
    own = async_session_maintenance.followup_hints(state, "project-key", "old-session")
    assert "Call codex_result" in own and "Start purser_wait.py" not in own
    assert "uygulama kapalıyken" not in own
    job["state"] = "UNKNOWN_OR_IN_PROGRESS"
    record.write_text(json.dumps(job), encoding="utf-8")
    assert async_session_maintenance.followup_hints(state, "project-key", "new-session") is None


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
