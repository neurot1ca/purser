import io
import hashlib
import importlib
import json
import os
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import context_io_hook
import async_session_maintenance
import codex_exec_mcp
import compaction_advisor
import model_version_notice


def _run_user_prompt_hook(monkeypatch, repo, *, prompt="", language_handoff=None, advice=None, unreadable=False,
                          event="UserPromptSubmit", source="startup", session_id="test-session",
                          maintenance_calls=None):
    payload = {"hook_event_name": event, "cwd": str(repo), "prompt": prompt,
               "source": source, "session_id": session_id}
    input_raw = io.BytesIO(json.dumps(payload).encode("utf-8"))
    output_raw = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (repo, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _repo: "test-project")
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "advise", lambda *_args: advice)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "_state_directory", lambda _repo: repo / ".state")
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "clear_pending", lambda *_args: None)
    monkeypatch.setattr(context_io_hook, "_queue_maintenance_notice",
                        lambda *args, **kwargs: maintenance_calls.append((args, kwargs))
                        if maintenance_calls is not None else None)
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: repo / ".runtime")
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "cleanup_jobs", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "followup_hints", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "compact_accuracy_hint", lambda *_args: None)
    import maintenance_reports
    monkeypatch.setattr(maintenance_reports, "record_activity", lambda *_args: None)
    monkeypatch.setattr(maintenance_reports, "wake_background_check", lambda *_args: None)
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


def test_user_prompt_submit_checks_maintenance_notice_thresholds(monkeypatch, tmp_path):
    calls = []
    _run_user_prompt_hook(monkeypatch, tmp_path, maintenance_calls=calls)
    assert len(calls) == 1
    assert calls[0][0][1] == tmp_path
    assert calls[0][1]["allow_no_window"] is False


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


def test_context_hook_defers_maintenance_import_and_unopted_fast_paths(monkeypatch, tmp_path):
    monkeypatch.delitem(sys.modules, "maintenance_reports", raising=False)
    importlib.reload(context_io_hook)
    assert "maintenance_reports" not in sys.modules

    import maintenance_reports
    monkeypatch.setattr(maintenance_reports, "_canonical_root",
                        lambda _path: pytest.fail("unopted project must return before canonicalization"))
    assert maintenance_reports.record_activity(tmp_path, state=tmp_path / "state") is None
    assert not maintenance_reports.wake_background_check(tmp_path, state=tmp_path / "state")


def test_session_start_queues_notice_without_maintenance_context(monkeypatch, tmp_path):
    import maintenance_reports
    monkeypatch.setattr(maintenance_reports, "notice_context", lambda *_args, **_kwargs: {
        "notice_file": "report.notice.json", "report_path": "report.md", "findings": [],
        "finding_count": 0, "confirmation_evidence": ["report.md"],
    })
    queued = []
    monkeypatch.setattr(context_io_hook.compaction_advisor, "queue_maintenance_notice",
                        lambda *args, **kwargs: queued.append((args, kwargs)) or True)
    context = context_io_hook._queue_maintenance_notice(
        {"session_id": "maintenance-notice"}, tmp_path, tmp_path / "state")
    assert context is None
    assert queued and queued[0][1]["evidence"] == ["report.md"]


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
    hint_calls = []
    def duration_hint(root, key, *, now=None, implementer=async_session_maintenance.IMPLEMENTER):
        hint_calls.append((root, key, now, implementer))
        return "Purser: typical Codex durations here (median/p90, n): mechanical 2/6 min (n=41)."
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint", duration_hint)

    assert context_io_hook.main() == 0
    result = json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]
    assert result["additionalContext"] == (
        "restored context\nPurser: typical Codex durations here (median/p90, n): mechanical 2/6 min (n=41)."
    )
    assert cleaned == [tmp_path / "state"]
    assert hint_calls == [(tmp_path / "state", "project-key", None, "codex-main")]


def test_plugin_version_upgrade_context_and_first_install(monkeypatch, tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    plugin = tmp_path / "plugin"
    (plugin / ".claude-plugin").mkdir(parents=True)
    (plugin / ".claude-plugin" / "plugin.json").write_text('{"version":"2.0.0"}', encoding="utf-8")
    (plugin / "CHANGELOG.md").write_text(
        "## 2.0.0 — 2026-09-28\n\n- First change.\n- Second change.\n## 1.9.0 — 2026-09-20\n- Old change.\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("CLAUDE_PLUGIN_ROOT", str(plugin))
    payload = {"session_id": "session-update"}
    version_path = compaction_advisor.plugin_version_state_path()

    # An initial install records its version without showing a note.
    assert context_io_hook._plugin_update_context(payload, repo) is None
    assert json.loads(version_path.read_text(encoding="utf-8")) == {"announced_version": "2.0.0"}
    assert compaction_advisor._read_state(compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-update")["version_pending"] is None

    version_path.write_text('{"announced_version":"2.0.0"}', encoding="utf-8")
    assert context_io_hook._plugin_update_context(payload, repo) is None  # Same currently installed version.
    version_path.write_text('{"announced_version":"1.8.0"}', encoding="utf-8")
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (repo, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _root: "project-key")
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "cleanup_jobs", lambda *_args: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint", lambda *_args, **_kwargs: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "followup_hints", lambda *_args: None)
    input_raw, output_raw = io.BytesIO(json.dumps({
        "hook_event_name": "SessionStart", "source": "startup", "cwd": str(repo), "session_id": "session-update"
    }).encode("utf-8")), io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(input_raw, encoding="cp1254"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))
    assert context_io_hook.main() == 0
    context = json.loads(output_raw.getvalue().decode("utf-8"))["hookSpecificOutput"]["additionalContext"]
    assert context and "Purser 2.0.0" in context
    assert "> ⚓ **Purser 2.0.0 sürümüne yükseltildi:**" in context
    assert "First change." in context and "Second change." in context and "Old change." not in context
    assert compaction_advisor._read_state(compaction_advisor._state_directory(repo) / "compaction_advisor.json", "session-update")["version_pending"]["version"] == "2.0.0"


def test_plugin_update_context_is_silent_without_plugin_root(monkeypatch, tmp_path):
    monkeypatch.delenv("CLAUDE_PLUGIN_ROOT", raising=False)
    assert context_io_hook._plugin_update_context({"session_id": "session-update"}, tmp_path) is None


def test_model_version_notice_checks_once_daily_and_skips_declined(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {"normal": {"model": "gpt-6-luna"}}
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})
    calls = []
    monkeypatch.setattr(context_io_hook.model_listing, "list_models", lambda *_a, **_k: calls.append(True) or {
        "status": "available", "models": [{"id": "gpt-6.1-luna"}]
    })
    first = context_io_hook._model_version_context(tmp_path)
    assert first and (
        "> ⚓ **New model version available: `gpt-6.1-luna` (you are on `gpt-6-luna`).**\n"
        "> *Should Purser switch to it? (1-yes 2-no)*\n"
        "> *If you say no, Purser won't ask about this version again. You can switch anytime with `/purser:init`.*"
    ) in first
    assert context_io_hook._model_version_context(tmp_path) is None
    assert len(calls) == 1

    model_version_notice.decide("gpt-6.1-luna", decline=True)
    state = model_version_notice.read_state()
    state["checked_on"] = "2000-01-01"
    model_version_notice.write_state(state)
    assert context_io_hook._model_version_context(tmp_path) is None
    assert len(calls) == 2


def test_model_version_notice_checks_agy_reviewer_after_routes(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {"normal": {"model": "gpt-6-luna"}},
        "reviewer1_model_requested": "gemini-3.8-flash-high",
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})
    calls = []

    def listing(agent, *_args, **_kwargs):
        calls.append(agent)
        models = {"codex": ["gpt-6-luna"], "agy": ["gemini-3.9-flash-high"]}[agent]
        return {"status": "available", "models": [{"id": model} for model in models]}

    monkeypatch.setattr(context_io_hook.model_listing, "list_models", listing)
    notice = context_io_hook._model_version_context(tmp_path)
    assert notice and "`gemini-3.9-flash-high` (you are on `gemini-3.8-flash-high`)" in notice
    assert calls == ["codex", "agy"]


def test_model_version_notice_lists_once_per_agent_for_multiple_routes(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {
            "mechanical": {"model": "gpt-6-luna"},
            "normal": {"model": "gpt-6-sol"},
            "escalated": {"model": "gpt-6-mini"},
        }, "reviewer1_model_requested": "gemini-3.8-flash-high",
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})
    calls = []

    def listing(agent, *_args, **_kwargs):
        calls.append(agent)
        models = {"codex": ["gpt-6-luna", "gpt-6-sol", "gpt-6-mini"],
                  "agy": ["gemini-3.9-flash-high"]}[agent]
        return {"status": "available", "models": [{"id": model} for model in models]}

    monkeypatch.setattr(context_io_hook.model_listing, "list_models", listing)
    notice = context_io_hook._model_version_context(tmp_path)
    assert notice and "`gemini-3.9-flash-high`" in notice
    assert calls == ["codex", "agy"]


def test_model_version_notice_silently_skips_failed_agy_listing(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {"normal": {"model": "gpt-6-luna"}},
        "reviewer1_model_requested": "gemini-3.8-flash-high",
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})

    calls = []

    def listing(agent, *_args, **_kwargs):
        calls.append(agent)
        return ({"status": "available", "models": [{"id": "gpt-6-luna"}]}
                if agent == "codex" else {"status": "unavailable", "models": []})

    monkeypatch.setattr(context_io_hook.model_listing, "list_models", listing)
    assert context_io_hook._model_version_context(tmp_path) is None
    assert calls == ["codex", "agy"]


def test_model_version_notice_does_not_offer_different_reviewer_tier(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {"normal": {"model": "gpt-6-luna"}},
        "reviewer1_model_requested": "gemini-3.8-flash-high",
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})
    calls = []

    def listing(agent, *_args, **_kwargs):
        calls.append(agent)
        models = {"codex": ["gpt-6-luna"], "agy": ["gemini-3.9-pro-high"]}[agent]
        return {"status": "available", "models": [{"id": model} for model in models]}

    monkeypatch.setattr(context_io_hook.model_listing, "list_models", listing)
    assert context_io_hook._model_version_context(tmp_path) is None
    assert calls == ["codex", "agy"]


def test_model_version_notice_stop_records_visible_line(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(compaction_advisor, "_state_directory", lambda _repo: tmp_path / "advisor")
    model_version_notice.write_state({"checked_on": "2026-09-30", "asked": [], "declined": [],
                                      "pending": {"current": "gpt-6-luna", "new": "gpt-6.1-luna"}})
    visible = "> ⚓ **New model version available: `gpt-6.1-luna` (you are on `gpt-6-luna`).**"
    assert compaction_advisor.stop({"session_id": "model-notice", "last_assistant_message": visible,
                                    "stop_hook_active": True}, tmp_path) is None
    state = model_version_notice.read_state()
    assert state["asked"] == ["gpt-6.1-luna"]
    assert state["pending"] is None


def test_session_start_survives_model_listing_error(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _root: "project-key")
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_a, **_k: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_a: "restored context")
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "cleanup_jobs", lambda *_a: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint", lambda *_a, **_k: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "followup_hints", lambda *_a: None)
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda *_: {
        "coder_agent": "codex", "routes": {"normal": {"model": "gpt-6-luna"}}
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog",
                        lambda: (_ for _ in ()).throw(OSError("bad cache")))
    incoming = io.BytesIO(json.dumps({"hook_event_name": "SessionStart", "source": "startup",
                                      "cwd": str(tmp_path)}).encode())
    outgoing = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(incoming, encoding="utf-8"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(outgoing, encoding="utf-8"))
    assert context_io_hook.main() == 0
    assert json.loads(outgoing.getvalue())["hookSpecificOutput"]["additionalContext"] == "restored context"


def test_mechanical_leader_context_is_cleared_and_recorded_by_stop(monkeypatch, tmp_path):
    monkeypatch.delenv("CLAUDE_PLUGIN_ROOT", raising=False)
    state_root = tmp_path / "state"
    leader_path = state_root / "opencode" / "leader.json"
    leader_path.parent.mkdir(parents=True)
    leader_path.write_text(json.dumps({"model": "vendor/free-model", "successes": 7,
                                       "sample_count": 9, "announced": False}), encoding="utf-8")
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: state_root)
    advisor_state = tmp_path / "advisor-state"
    monkeypatch.setattr(compaction_advisor, "_state_directory", lambda _repo: advisor_state)
    payload = {"session_id": "session-leader"}

    context = context_io_hook._plugin_update_context(payload, tmp_path)
    assert ("> ⚓ **Ücretsiz model sıralaması değişti:** `vendor/free-model` son 9 denemeden "
            "7 tanesinde başarılı oldu ve yarışta ilk sıraya yerleşti. Mekanik işler önce bu modele gidecek.") in context
    blocked = compaction_advisor.stop({**payload, "last_assistant_message": "Not announced."}, tmp_path)
    assert blocked["decision"] == "block"
    assert "⚓" in blocked["reason"] and "vendor/free-model" in blocked["reason"]
    assert compaction_advisor.stop({**payload, "last_assistant_message": "Still missing."}, tmp_path) is None
    assert compaction_advisor.stop({**payload, "last_assistant_message": "⚓ `vendor/free-model`"}, tmp_path) is None
    state = compaction_advisor._read_state(advisor_state / "compaction_advisor.json", "session-leader")
    assert state["leader_pending"] is None
    assert json.loads(leader_path.read_text(encoding="utf-8"))["announced"] is True
    assert context_io_hook._plugin_update_context(payload, tmp_path) is None


def test_mechanical_leader_context_is_silent_when_already_announced(monkeypatch, tmp_path):
    monkeypatch.delenv("CLAUDE_PLUGIN_ROOT", raising=False)
    state_root = tmp_path / "state"
    leader_path = state_root / "opencode" / "leader.json"
    leader_path.parent.mkdir(parents=True)
    leader_path.write_text(json.dumps({"model": "vendor/free-model", "successes": 7,
                                       "sample_count": 9, "announced": True}), encoding="utf-8")
    monkeypatch.setattr(context_io_hook.state_root, "default_state_root", lambda: state_root)

    assert context_io_hook._plugin_update_context({"session_id": "session-leader"}, tmp_path) is None


def test_accuracy_runs_only_on_compact_and_preserves_context_on_failure(monkeypatch, tmp_path):
    seen = []
    monkeypatch.setattr(context_io_hook.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(context_io_hook.project_root, "project_key", lambda _root: "project-key")
    monkeypatch.setattr(context_io_hook.context_io_telemetry, "handle", lambda *_a, **_k: None)
    monkeypatch.setattr(context_io_hook.compaction_advisor, "restore", lambda *_a: "restored")
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "cleanup_jobs", lambda *_a: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "duration_hint", lambda *_a: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "followup_hints", lambda *_a: None)
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "compact_accuracy_hint",
                        lambda *_a: seen.append(True) or "Tell the user: message")
    for source in ("startup", "resume", "clear", "compact"):
        incoming = io.BytesIO(json.dumps({"hook_event_name": "SessionStart", "source": source,
                                          "cwd": str(tmp_path)}).encode())
        outgoing = io.BytesIO()
        monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(incoming, encoding="utf-8"))
        monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(outgoing, encoding="utf-8"))
        assert context_io_hook.main() == 0
        result = json.loads(outgoing.getvalue())["hookSpecificOutput"]["additionalContext"]
        assert result == ("restored\nTell the user: message" if source == "compact" else "restored")
    assert seen == [True]
    monkeypatch.setattr(context_io_hook.async_session_maintenance, "compact_accuracy_hint",
                        lambda *_a: (_ for _ in ()).throw(ValueError("bad state")))
    incoming = io.BytesIO(json.dumps({"hook_event_name": "SessionStart", "source": "compact",
                                      "cwd": str(tmp_path)}).encode())
    outgoing = io.BytesIO()
    monkeypatch.setattr(context_io_hook.sys, "stdin", io.TextIOWrapper(incoming, encoding="utf-8"))
    monkeypatch.setattr(context_io_hook.sys, "stdout", io.TextIOWrapper(outgoing, encoding="utf-8"))
    assert context_io_hook.main() == 0
    assert json.loads(outgoing.getvalue())["hookSpecificOutput"]["additionalContext"] == "restored"


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
    assert len(lines) == 6
    assert lines[0] == "Reply in the language the user writes in."
    assert "finished-task" in lines[1] and "bitince haber" not in lines[1]
    assert "running-task" in lines[2] and "bitince haber" in lines[2]
    assert "own-task" not in "\n".join(lines)  # The five older hints fill the cap.
    assert all("jobId=" in line for line in lines[1:])
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
    assert "> ⚓ **Codex işi (task-a) bilgisayar kapanınca yarıda kalmıştı; kaldığı yerden devam ettirdim.**" in hint
    own = async_session_maintenance.followup_hints(state, "project-key", "old-session")
    assert "Start purser_wait.py" in own and "PowerShell tool on Windows" in own and "without adopt" in own
    assert "> ⚓ **Codex işi (task-a) bilgisayar kapanınca yarıda kalmıştı; kaldığı yerden devam ettirdim.**" in own
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
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", language_handoff=language_handoff)

    assert actual == "Reply in the language the user writes in."


def test_user_prompt_omits_reminder_for_english(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path,
        language_handoff="## Goal\nUser language: eNgLiSh\n",
    )

    assert actual is None


def test_session_start_omits_reminder_for_english(monkeypatch, tmp_path):
    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path, event="SessionStart",
        language_handoff="## Goal\nUser language: English\n",
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
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", unreadable=True)

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
    _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart")
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt=prompt)

    assert actual == "The user mentioned planning; offer /purser:plan once in this session if you have not already."


@pytest.mark.parametrize("prompt", ["planet", "explanation", "airplane"])
def test_user_prompt_does_not_offer_plan_for_unrelated_words(monkeypatch, tmp_path, prompt):
    _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart")
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt=prompt)

    assert actual is None


def test_user_prompt_does_not_offer_plan_when_command_is_invoked(monkeypatch, tmp_path):
    _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart")
    actual = _run_user_prompt_hook(monkeypatch, tmp_path, prompt="/purser:plan design the migration")

    assert actual is None


def test_plan_hint_matcher_failure_preserves_existing_context(monkeypatch, tmp_path):
    monkeypatch.setattr(context_io_hook, "_planning_hint", lambda _prompt: (_ for _ in ()).throw(RuntimeError("matcher failed")))

    actual = _run_user_prompt_hook(
        monkeypatch, tmp_path, prompt="plan the migration",
        language_handoff="## Goal\nUser language: Turkish\n", advice="compact notice",
    )

    assert actual == "Reply to the user in Turkish.\ncompact notice"


def test_planning_hint_appears_once_per_session(monkeypatch, tmp_path):
    first = _run_user_prompt_hook(monkeypatch, tmp_path, prompt="plan this",
                                  language_handoff="## Goal\nUser language: Turkish\n")
    second = _run_user_prompt_hook(monkeypatch, tmp_path, prompt="plan the next step",
                                   language_handoff="## Goal\nUser language: Turkish\n")

    assert first.count("offer /purser:plan") == 1
    assert second is None or "offer /purser:plan" not in second
    state = compaction_advisor._read_state(tmp_path / ".state" / "compaction_advisor.json", "test-session")
    assert state["planning_injections"] == 1


def test_language_reminder_session_start_reset_change_and_repeats(monkeypatch, tmp_path):
    handoff = "## Goal\nUser language: Turkish\n"
    started = _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", language_handoff=handoff)
    assert started == "Reply to the user in Turkish."
    assert _run_user_prompt_hook(monkeypatch, tmp_path, language_handoff=handoff) is None
    assert _run_user_prompt_hook(monkeypatch, tmp_path, language_handoff=handoff) is None

    compacted = _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", source="compact",
                                      language_handoff=handoff)
    assert compacted == "Reply to the user in Turkish."
    assert _run_user_prompt_hook(monkeypatch, tmp_path, language_handoff=handoff) == "Reply to the user in Turkish."
    changed = _run_user_prompt_hook(monkeypatch, tmp_path,
                                    language_handoff="## Goal\nUser language: German\n")
    assert changed == "Reply to the user in German."

    cleared = _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", source="clear",
                                    language_handoff="## Goal\nUser language: German\n")
    assert cleared == "Reply to the user in German."
    assert _run_user_prompt_hook(monkeypatch, tmp_path,
                                 language_handoff="## Goal\nUser language: German\n") == "Reply to the user in German."
    state = compaction_advisor._read_state(tmp_path / ".state" / "compaction_advisor.json", "test-session")
    assert state["language_injections"] == 6


def test_language_reminder_fallback_every_twenty_prompts(monkeypatch, tmp_path):
    handoff = "## Goal\nUser language: Turkish\n"
    _run_user_prompt_hook(monkeypatch, tmp_path, event="SessionStart", language_handoff=handoff)
    results = [_run_user_prompt_hook(monkeypatch, tmp_path, language_handoff=handoff) for _ in range(20)]

    assert results[0] is None
    assert results[18] is None
    assert results[19] == "Reply to the user in Turkish."
