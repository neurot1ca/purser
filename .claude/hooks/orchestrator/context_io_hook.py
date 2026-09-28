"""Silent, fail-open entrypoint for passive context-I/O lifecycle hooks."""

from __future__ import annotations

from collections.abc import Mapping
import json
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


_PLANNING_WORD = re.compile(
    r"\b(?:plans?|planning|planned|planla\w*|planı\w*|planın\w*|plan[ıiuü]\w*)\b",
    re.IGNORECASE,
)
_PLAN_COMMAND = re.compile(r"^\s*/purser:plan(?:\s|$)", re.IGNORECASE)
_CHANGELOG_ENTRY = re.compile(r"^##\s+(\S+)\s+—\s+\d{4}-\d{2}-\d{2}\s*$", re.MULTILINE)


def _plugin_root() -> Path:
    return Path(__file__).resolve().parents[3]


def _plugin_update_context(payload: Mapping[str, Any], repository: Path | None) -> str | None:
    try:
        root = _plugin_root()
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
            f"`⚓ **Purser {version} sürümüne yükseltildi:** <one short sentence summarizing the bullets>`. "
            f"Translate the bold lead for other languages, keeping ⚓ and {version}. "
            "Source bullets (summarize them in one short sentence):\n" + "\n".join(bullets)
        )
    except BaseException:
        return None


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
            if value.get("hook_event_name") == "Stop":
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
                        plan_hint = (
                            "The user mentioned planning; offer /purser:plan once in this session if you have not already."
                            if _planning_hint(value.get("prompt"))
                            else ""
                        )
                    except BaseException:
                        plan_hint = ""
                    additional_context = "\n".join(
                        part for part in (language_instruction, compaction_advice, plan_hint) if part
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
                    compaction_advisor.clear_pending(value, repository)
                    try:
                        async_session_maintenance.cleanup_jobs(state_root.default_state_root())
                    except BaseException:
                        pass
                    additional_context = compaction_advisor.restore(value, repository)
                    update_context = _plugin_update_context(value, repository)
                    if update_context:
                        additional_context = "\n".join(part for part in (additional_context, update_context) if part)
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
