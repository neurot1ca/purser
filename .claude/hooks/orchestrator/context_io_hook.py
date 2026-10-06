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
            "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged, in this form: "
            f"> ⚓ **Free model ranking changed:** `{model}` succeeded in {successes} of its last {sample_count} attempts "
            "and moved to the top of the ranking. Mechanical tasks will go to this model first."
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
            f"On your next reply, render exactly one line in the user's language in this form, translating from English: "
            f"`> ⚓ **Purser upgraded to {version}:** <one short sentence summarizing the bullets>`. "
            f"Keep ⚓ and {version}. "
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


def discover_model_version(repository: Path | None) -> tuple[str, str] | None:
    """Queue a newer pinned model version after bounded listings, at most daily."""
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
        catalog = model_catalog.load_cached_catalog()
        state = model_version_notice.read_state()
        checks = [(agent, current) for current in pinned]
        reviewer_model = config.get("reviewer1_model_requested")
        if isinstance(reviewer_model, str):
            checks.append(("agy", reviewer_model))
        listings = {}
        for listing_agent, current in checks:
            if listing_agent not in listings:
                listings[listing_agent] = model_listing.list_models(
                    listing_agent, catalog["providers"], timeout_seconds=2
                )
            listing = listings[listing_agent]
            if listing.get("status") != "available":
                continue
            candidates = [item.get("id") for item in listing.get("models", []) if isinstance(item, dict)]
            newer = model_version_notice.newer_version(current, candidates)
            if newer and newer not in state["asked"] and newer not in state["declined"]:
                state["pending"] = {"current": current, "new": newer, "blocked": False}
                model_version_notice.write_state(state)
                model_version_notice.record_check()
                return current, newer
        model_version_notice.record_check()
        return None
    except BaseException:
        return None


def _model_version_context(repository: Path | None) -> str | None:
    version = discover_model_version(repository)
    if version is None:
        return None
    current, newer = version
    return (
        "On your next reply, render exactly these three lines in the user's language; "
        "translate from English, keeping ⚓, both model names, and `/purser:init` unchanged. "
        f"If the user answers 1/yes, apply only these routes with "
        f"`python \"${{CLAUDE_PLUGIN_ROOT}}/.claude/hooks/orchestrator/purser_init.py\" --adopt-model-version {newer}`. "
        f"If they answer 2/no, record that choice with `python \"${{CLAUDE_PLUGIN_ROOT}}/.claude/hooks/orchestrator/purser_init.py\" --decline-model-version {newer}`. "
        "Do not change routes unless the user says yes.\n"
        f"> ⚓ **New model version available: `{newer}` (you are on `{current}`).**\n"
        "> *Should Purser switch to it? (1-yes 2-no)*\n"
        "> *If you say no, Purser won't ask about this version again. You can switch anytime with `/purser:init`.*"
    )


def _queue_maintenance_notice(payload: Mapping[str, Any], repository: Path | None,
                             state: Path, *, allow_consent: bool = False,
                             allow_no_window: bool = True) -> None:
    import maintenance_reports
    if repository is None:
        return
    if allow_consent:
        notice = maintenance_reports.consent_context(repository, state=state)
        if notice is not None:
            compaction_advisor.queue_maintenance_notice(
                payload, repository, notice["notice_file"], maintenance_reports.format_notice(notice),
                evidence=notice.get("confirmation_evidence", []))
            return
    notice = maintenance_reports.notice_context(repository, state=state, allow_no_window=allow_no_window)
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
                        _queue_maintenance_notice(
                            value, repository, state_root.default_state_root(), allow_no_window=False)
                except BaseException:
                    pass
                try:
                    try:
                        language = compaction_advisor.recorded_user_language(repository)
                    except BaseException:
                        language = None
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
                    language_instruction, inject_plan = compaction_advisor.context_injections(
                        value, repository, language, planning=bool(plan_hint)
                    )
                    if not inject_plan:
                        plan_hint = ""
                    lifecycle_context = compaction_advisor.lifecycle_guidance(value, repository)
                    additional_context = "\n".join(
                        part for part in (language_instruction, compaction_advice, retry_context, plan_hint,
                                          lifecycle_context) if part
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
                            if lifecycle_context:
                                compaction_advisor.mark_lifecycle_guidance_delivered(value, repository)
                            if compaction_advice:
                                compaction_advisor.record_advice_written(value, repository)
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
                        language = compaction_advisor.recorded_user_language(repository)
                    except BaseException:
                        language = None
                    language_instruction, _inject_plan = compaction_advisor.context_injections(
                        value, repository, language
                    )
                    if not isinstance(value.get("session_id"), str) or not value.get("session_id"):
                        language_instruction = None
                    try:
                        async_session_maintenance.cleanup_jobs(state_root.default_state_root())
                    except BaseException:
                        pass
                    additional_context = compaction_advisor.restore(value, repository)
                    if (additional_context and language_instruction
                            and language_instruction.removesuffix(".") + ";" in additional_context):
                        language_instruction = None
                    if language_instruction:
                        additional_context = "\n".join(
                            part for part in (language_instruction, additional_context) if part
                        )
                    try:
                        _queue_maintenance_notice(value, repository, state_root.default_state_root(),
                                                  allow_consent=True)
                    except BaseException:
                        pass
                    update_context = _plugin_update_context(value, repository)
                    session_delivered_kinds = set()
                    if update_context:
                        additional_context = "\n".join(part for part in (additional_context, update_context) if part)
                        if "Purser upgraded to " in update_context:
                            session_delivered_kinds.add("version")
                        if "Free model ranking changed:" in update_context:
                            session_delivered_kinds.add("leader")
                    model_context = _model_version_context(repository)
                    if model_context:
                        additional_context = "\n".join(part for part in (additional_context, model_context) if part)
                        session_delivered_kinds.add("model")
                    lifecycle_context = compaction_advisor.lifecycle_guidance(
                        value, repository, exclude_kinds=session_delivered_kinds
                    )
                    if lifecycle_context:
                        additional_context = "\n".join(part for part in (additional_context, lifecycle_context) if part)
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
                            compaction_advisor.mark_lifecycle_guidance_delivered(
                                value, repository, kinds=session_delivered_kinds | (
                                    {"maintenance", "version", "leader", "model", "fallback"}
                                    if lifecycle_context else set()
                                )
                            )
                except BaseException:
                    pass
    except BaseException:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
