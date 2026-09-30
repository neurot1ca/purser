"""Silent, fail-open entrypoint for passive context-I/O lifecycle hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
import os
from pathlib import Path
import re
import sys
import time
from typing import Any

import context_io_telemetry
import coder_config
import async_session_maintenance
import compaction_advisor
import project_profile
import project_root
import state_root
import model_catalog
import model_listing
import model_version_notice


_PLANNING_WORD = re.compile(
    r"\b(?:plans?|planning|planned|planla\w*|planı\w*|planın\w*|plan[ıiuü]\w*)\b",
    re.IGNORECASE,
)
_PLAN_COMMAND = re.compile(r"^\s*/purser:plan(?:\s|$)", re.IGNORECASE)
_CHANGELOG_ENTRY = re.compile(r"^##\s+(\S+)\s+—\s+\d{4}-\d{2}-\d{2}\s*$", re.MULTILINE)


def _plugin_root() -> Path | None:
    root = os.environ.get("CLAUDE_PLUGIN_ROOT")
    return Path(root) if root else None


def _mechanical_leader_context(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    try:
        path = state_root.default_state_root() / "opencode" / "leader.json"
        leader = json.loads(path.read_text(encoding="utf-8"))
        if (not isinstance(leader, dict) or leader.get("announced") is not False
                or not isinstance(leader.get("model"), str)
                or type(leader.get("successes")) is not int
                or type(leader.get("sample_count")) is not int):
            return None
        model = leader["model"]
        successes = leader["successes"]
        sample_count = leader["sample_count"]
        if not compaction_advisor.queue_leader_announcement(
                payload, repository, model, successes, sample_count):
            return None
        return (
            "On your next reply, render exactly one line in the user's language in this form: "
            f"> ⚓ **Ücretsiz model sıralaması değişti:** `{model}` son {sample_count} denemeden "
            f"{successes} tanesinde başarılı oldu ve yarışta ilk sıraya yerleşti. "
            "Mekanik işler önce bu modele gidecek. "
            f"Translate the line for other languages, keeping ⚓, `{model}`, and the numbers."
        )
    except BaseException:
        return None


def _plugin_version_context(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    try:
        root = _plugin_root()
        if root is None:
            return None
        manifest = json.loads((root / ".claude-plugin" / "plugin.json").read_text(encoding="utf-8"))
        version = manifest.get("version") if isinstance(manifest, dict) else None
        if not isinstance(version, str) or not version:
            return None
        announced = compaction_advisor.announced_plugin_version()
        if announced is None:
            compaction_advisor.record_announced_plugin_version(version)
            return None
        if announced == version:
            return None
        changelog = (root / "CHANGELOG.md").read_text(encoding="utf-8")
        match = next((entry for entry in _CHANGELOG_ENTRY.finditer(changelog) if entry.group(1) == version), None)
        if match is None:
            return None
        end = changelog.find("\n## ", match.end())
        section = changelog[match.end():] if end < 0 else changelog[match.end():end]
        bullets = [line.strip() for line in section.splitlines() if re.match(r"^\s*[-*+]\s+", line)]
        if not bullets or not compaction_advisor.queue_version_announcement(payload, repository, version):
            return None
        return (
            f"On your next reply, render exactly one line in the user's language in this form: "
            f"`> ⚓ **Purser {version} sürümüne yükseltildi:** <one short sentence summarizing the bullets>`. "
            f"Translate the bold lead for other languages, keeping ⚓ and {version}. "
            "Source bullets (summarize them in one short sentence):\n" + "\n".join(bullets)
        )
    except BaseException:
        return None


def _plugin_update_context(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    try:
        leader_context = _mechanical_leader_context(payload, repository)
        version_context = _plugin_version_context(payload, repository)
        return "\n".join(part for part in (leader_context, version_context) if part) or None
    except BaseException:
        return None


def _model_version_context(repository: Path | None) -> str | None:
    """Check pinned routes against a bounded live CLI listing at most daily."""
    try:
        if repository is None or not model_version_notice.due_today():
            return None
        config = coder_config.load(Path(__file__).with_name("config.json"))
        agent = config.get("coder_agent", "codex")
        routes = config.get("routes", {})
        pinned = list(dict.fromkeys(
            route.get("model") if isinstance(route, dict) else route
            for route in routes.values()
        ))
        pinned = [model for model in pinned if isinstance(model, str)]
        if not pinned:
            return None
        catalog = model_catalog.load_cached_catalog()
        listing = model_listing.list_models(agent, catalog["providers"], timeout_seconds=2)
        if listing.get("status") != "available":
            return None
        candidates = [item.get("id") for item in listing.get("models", []) if isinstance(item, dict)]
        state = model_version_notice.read_state()
        for current in pinned:
            newer = model_version_notice.newer_version(current, candidates)
            if newer and newer not in state["asked"] and newer not in state["declined"]:
                state["pending"] = {"current": current, "new": newer, "blocked": False}
                model_version_notice.write_state(state)
                model_version_notice.record_check()
                return (
                    "On your next reply, render exactly these three lines in the user's language; "
                    "translate from Turkish and keep ⚓ and both model names unchanged. "
                    f"If the user answers 1/yes, apply only these routes with "
                    f"`python \"${{CLAUDE_PLUGIN_ROOT}}/.claude/hooks/orchestrator/purser_init.py\" --adopt-model-version {newer}`. "
                    f"If they answer 2/no, record that choice with `python \"${{CLAUDE_PLUGIN_ROOT}}/.claude/hooks/orchestrator/purser_init.py\" --decline-model-version {newer}`. "
                    "Do not change routes unless the user says yes.\n"
                    f"> ⚓ **Yeni model sürümü çıktı: `{newer}`, şu an `{current}` kullanılıyor.**\n"
                    "> *Purser'ın bu sürüme geçmesini ister misin? (1-evet 2-hayır)*\n"
                    "> *Hayır dersen bu sürüm için bir daha sorulmaz. İstediğin zaman `/purser:init` ile geçebilirsin.*"
                )
        model_version_notice.record_check()
        return None
    except BaseException:
        return None


def _queue_maintenance_notice(payload: Mapping[str, Any], repository: Path | None,
                             state: Path) -> None:
    import maintenance_reports
    if repository is None:
        return
    notice = maintenance_reports.notice_context(repository, state=state)
    if notice is None:
        return
    compaction_advisor.queue_maintenance_notice(
        payload, repository, notice["notice_file"], maintenance_reports.format_notice(notice),
        evidence=notice.get("confirmation_evidence", []))


def _planning_hint(prompt: Any) -> bool:
    if not isinstance(prompt, str) or _PLAN_COMMAND.search(prompt):
        return False
    return _PLANNING_WORD.search(prompt) is not None


def main() -> int:
    started = time.perf_counter_ns()
    try:
        stdin_buffer = getattr(sys.stdin, "buffer", None)
        if stdin_buffer is None:
            value: Any = json.load(sys.stdin)
        else:
            value = json.loads(stdin_buffer.read().decode("utf-8", errors="strict"))
        if isinstance(value, Mapping):
            repository, _reason = project_root.resolve_project_root(value)
            protected_paths = (
                project_profile.load_profile(repository).protected_paths
                if repository is not None
                else project_profile._ENGINE_PROTECTED
            )
            context_io_telemetry.handle(
                value,
                repo_root=repository,
                protected_paths=protected_paths,
                hook_started_ns=started,
            )
            if value.get("hook_event_name") == "PreCompact":
                try:
                    compaction_advisor.refresh_handoff(value, repository)
                except BaseException:
                    pass
            elif value.get("hook_event_name") == "Stop":
                try:
                    response = compaction_advisor.stop(value, repository)
                    if response:
                        stdout_buffer = getattr(sys.stdout, "buffer", None)
                        if stdout_buffer is not None:
                            stdout_buffer.write(json.dumps(response, ensure_ascii=False).encode("utf-8"))
                            stdout_buffer.flush()
                except BaseException:
                    pass
            elif value.get("hook_event_name") == "UserPromptSubmit":
                try:
                    import maintenance_reports
                    if repository is not None:
                        maintenance_reports.record_activity(repository)
                except BaseException:
                    pass
                try:
                    try:
                        language = compaction_advisor.recorded_user_language(repository)
                    except BaseException:
                        language = None
                    if language is None:
                        language_instruction = "Reply in the language the user writes in."
                    elif language.casefold() == "english":
                        language_instruction = ""
                    else:
                        language_instruction = f"Reply to the user in {language}."
                    try:
                        compaction_advice = compaction_advisor.advise(value, repository)
                    except BaseException:
                        compaction_advice = None
                    try:
                        retry_context = compaction_advisor.prompt_retry_context(value, repository)
                    except BaseException:
                        retry_context = None
                    try:
                        plan_hint = (
                            "The user mentioned planning; offer /purser:plan once in this session if you have not already."
                            if _planning_hint(value.get("prompt"))
                            else ""
                        )
                    except BaseException:
                        plan_hint = ""
                    additional_context = "\n".join(
                        part for part in (language_instruction, compaction_advice, retry_context, plan_hint) if part
                    )
                    if additional_context:
                        output = {
                            "hookSpecificOutput": {
                                "hookEventName": "UserPromptSubmit",
                                "additionalContext": additional_context,
                            }
                        }
                        stdout_buffer = getattr(sys.stdout, "buffer", None)
                        if stdout_buffer is not None:
                            stdout_buffer.write(
                                json.dumps(output, ensure_ascii=False).encode("utf-8")
                            )
                            stdout_buffer.flush()
                except BaseException:
                    pass
            elif value.get("hook_event_name") == "SessionStart":
                try:
                    import maintenance_reports
                    if repository is not None:
                        maintenance_reports.record_activity(repository)
                        maintenance_reports.wake_background_check(repository)
                except BaseException:
                    pass
                try:
                    compaction_advisor.clear_pending(value, repository)
                    try:
                        async_session_maintenance.cleanup_jobs(state_root.default_state_root())
                    except BaseException:
                        pass
                    additional_context = compaction_advisor.restore(value, repository)
                    try:
                        _queue_maintenance_notice(value, repository, state_root.default_state_root())
                    except BaseException:
                        pass
                    update_context = _plugin_update_context(value, repository)
                    if update_context:
                        additional_context = "\n".join(part for part in (additional_context, update_context) if part)
                    model_context = _model_version_context(repository)
                    if model_context:
                        additional_context = "\n".join(part for part in (additional_context, model_context) if part)
                    try:
                        if repository is not None:
                            selected = coder_config.load(Path(__file__).with_name("config.json")).get("coder_agent", "codex")
                            hint = async_session_maintenance.duration_hint(
                                state_root.default_state_root(), project_root.project_key(repository),
                                implementer="agy-main" if selected == "agy" else "opencode-main" if selected == "opencode" else "codex-main",
                            )
                            if hint:
                                additional_context = "\n".join(
                                    part for part in (additional_context, hint) if part
                                )
                    except BaseException:
                        pass
                    try:
                        if repository is not None:
                            followup = async_session_maintenance.followup_hints(
                                state_root.default_state_root(),
                                project_root.project_key(repository), value.get("session_id")
                            )
                            if followup:
                                additional_context = "\n".join(
                                    part for part in (additional_context, followup) if part
                                )
                    except BaseException:
                        pass
                    try:
                        if repository is not None and value.get("source") == "compact":
                            accuracy = async_session_maintenance.compact_accuracy_hint(
                                state_root.default_state_root(), project_root.project_key(repository)
                            )
                            if accuracy:
                                additional_context = "\n".join(
                                    part for part in (additional_context, accuracy) if part
                                )
                    except BaseException:
                        pass
                    if additional_context:
                        output = {
                            "hookSpecificOutput": {
                                "hookEventName": "SessionStart",
                                "additionalContext": additional_context,
                            }
                        }
                        stdout_buffer = getattr(sys.stdout, "buffer", None)
                        if stdout_buffer is not None:
                            stdout_buffer.write(
                                json.dumps(output, ensure_ascii=False).encode("utf-8")
                            )
                            stdout_buffer.flush()
                except BaseException:
                    pass
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
