"""Claude Code hook wiring for the codex-main MCP tools."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import inspect
import json
import math
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from typing import Any

import json_transport
import active_coder_state
import coder_config
import compaction_advisor
import codex_quota
import codex_exec_mcp
import checks
import ledger
import policy
import project_profile
import project_root
import reviewer as reviewer1
import telemetry
import telemetry_schema
import state_root as purser_state_root


CODEX_TOOL = "mcp__codex-main__codex"
CODEX_REPLY_TOOL = "mcp__codex-main__codex-reply"
IMPLEMENTER_REPORT_LIMIT = 1200
OPERATION_REPORT_LIMIT = 24000
LOW_QUOTA_USED_PCT = 90
BUNDLE_NON_REPORT_MAX_BYTES = 8192
CHANGED_PATH_PREVIEW_LIMIT = 10
GIT_HEAD_READ_LIMIT = 4096
PENDING_MAX_AGE_SECONDS = 24 * 60 * 60
PENDING_CLEANUP_LIMIT = 32
INDEPENDENCE_INSTRUCTIONS = (
    "Codex must not blindly accept Claude's proposed solution. It should "
    "independently verify the relevant repository state and project rules, "
    "prefer the smallest correct implementation, and explicitly flag any "
    "instruction that would introduce a regression, unnecessary complexity, "
    "scope expansion, or conflict with existing contracts."
)

_PACKAGE_ROOT = Path(__file__).resolve().parent
_CONFIG_PATH = _PACKAGE_ROOT / "config.json"


def _configured_tool_names(key: str, default: str) -> frozenset[str]:
    """Read names without making a config read failure an import failure."""
    try:
        names = json.loads(_CONFIG_PATH.read_text(encoding="utf-8")).get(key)
        if isinstance(names, list) and names and all(isinstance(name, str) and name for name in names):
            return frozenset(names) | {default}
    except (OSError, ValueError, TypeError):
        pass
    return frozenset({default})


CODEX_TOOL_NAMES = _configured_tool_names("codex_tool_names", CODEX_TOOL)
CODEX_REPLY_TOOL_NAMES = _configured_tool_names("codex_reply_tool_names", CODEX_REPLY_TOOL)
CODEX_ASYNC_TOOL_NAMES = _configured_tool_names(
    "codex_async_tool_names", "mcp__codex-main__codex_start"
) | frozenset(
    f"{prefix}__{name}"
    for prefix in ("mcp__codex-main", "mcp__plugin_purser_codex-main")
    for name in ("codex_start", "codex_result", "codex_cancel")
)
CODEX_START_TOOL_NAMES = frozenset(name for name in CODEX_ASYNC_TOOL_NAMES if name.endswith("__codex_start"))
CODEX_RESULT_TOOL_NAMES = frozenset(name for name in CODEX_ASYNC_TOOL_NAMES if name.endswith("__codex_result"))
CODEX_CANCEL_TOOL_NAMES = frozenset(name for name in CODEX_ASYNC_TOOL_NAMES if name.endswith("__codex_cancel"))
CODEX_APPLY_TOOL_NAMES = frozenset(
    f"{prefix}__codex_apply"
    for prefix in ("mcp__codex-main", "mcp__plugin_purser_codex-main")
)
CODEX_TOOLS = CODEX_TOOL_NAMES | CODEX_REPLY_TOOL_NAMES | CODEX_ASYNC_TOOL_NAMES
_BASELINE_VOLATILE_MARKERS = ("timestamp", "duration", "elapsed", "budget")


def _baseline_state_fields(value: Mapping[str, Any]) -> dict[str, Any]:
    """Return capture state excluding timestamp, duration, and budget measurements."""

    return {
        str(key): item
        for key, item in value.items()
        if not any(marker in str(key).casefold() for marker in _BASELINE_VOLATILE_MARKERS)
    }


def _handoff_matches(
    captured: Mapping[str, Any],
    live_thread: Mapping[str, Any] | None,
    declared_scope: Sequence[str],
) -> bool:
    if not isinstance(live_thread, Mapping):
        return False
    origin_baseline = live_thread.get("origin_baseline")
    origin_scope = live_thread.get("origin_scope")
    post_state = live_thread.get("post_state")
    return (
        captured.get("status") == ledger.OK
        and isinstance(origin_baseline, Mapping)
        and origin_baseline.get("status") == ledger.OK
        and isinstance(origin_scope, list)
        and origin_scope == list(declared_scope)
        and isinstance(post_state, Mapping)
        and post_state.get("status") == ledger.OK
        and _baseline_state_fields(captured) == _baseline_state_fields(post_state)
    )


def _default_reviewer(**_: Any) -> dict[str, Any]:
    return {
        "available": False,
        "result": policy.REVIEWER1_UNAVAILABLE,
        "message": "Reviewer-1 is unavailable because reviewer.py is not installed.",
    }


_default_reviewer.available = False  # type: ignore[attr-defined]
REVIEWER: Callable[..., Mapping[str, Any]] = reviewer1.review


def _parse_task_contract(
    prompt_text: str, task_id: str, phase: str, scope: Sequence[str]
) -> tuple[str, str, bool]:
    """Parse task contract from prompt text.

    Returns (objective, criteria, grounded).
    grounded is True ONLY IF both objective and criteria are explicitly provided and validated.
    """
    default_obj = f"Task {task_id} ({phase} phase) bounded to declared scope."
    default_crit = (
        f"Verify correct implementation within declared scope ({', '.join(scope)}) "
        "with no material regression or scope expansion."
    )

    # 1. Check for explicit JSON TASK_CONTRACT block
    for line in prompt_text.splitlines():
        line_str = line.strip()
        if line_str.upper().startswith("TASK_CONTRACT:"):
            payload = line_str[len("TASK_CONTRACT:"):].strip()
            try:
                data = json.loads(payload)
                if isinstance(data, dict):
                    raw_obj = data.get("objective")
                    raw_crit = data.get("criteria")
                    if isinstance(raw_crit, list) and all(isinstance(x, str) for x in raw_crit):
                        crit = "; ".join(x.strip() for x in raw_crit if x.strip())
                    elif isinstance(raw_crit, str):
                        crit = raw_crit.strip()
                    else:
                        crit = ""

                    if isinstance(raw_obj, str):
                        obj = raw_obj.strip()
                    else:
                        obj = ""

                    if obj and crit:
                        return obj, crit, True
            except (json.JSONDecodeError, TypeError, ValueError):
                pass

    # 2. Check for explicit multiline or key-value headers
    lines = prompt_text.splitlines()
    found_obj: str | None = None
    criteria_lines: list[str] = []
    collecting_criteria = False

    for i, line in enumerate(lines):
        line_str = line.strip()
        lower = line_str.lower()
        if lower.startswith(("task objective:", "objective:")):
            collecting_criteria = False
            label = "task objective:" if lower.startswith("task objective:") else "objective:"
            val = line_str[len(label):].strip()
            if not val and i + 1 < len(lines) and not lines[i + 1].strip().lower().startswith("acceptance criteria:"):
                val = lines[i + 1].strip()
            if "acceptance criteria:" in val.lower():
                c_idx = val.lower().find("acceptance criteria:")
                crit_part = val[c_idx + len("acceptance criteria:"):].strip(". ")
                if crit_part:
                    criteria_lines.append(crit_part)
                val = val[:c_idx].strip(". ")
            if val and found_obj is None:
                found_obj = val
        elif lower.startswith("acceptance criteria:"):
            collecting_criteria = True
            val = line_str[len("acceptance criteria:"):].strip()
            if val:
                criteria_lines.append(val)
        elif collecting_criteria:
            if line_str.startswith(("ORCH|", "TASK_CONTRACT:", "```")):
                collecting_criteria = False
            elif re.match(r"^(?:[-*+]|\d+[.)])\s+", line_str):
                criteria_lines.append(re.sub(r"^(?:[-*+]|\d+[.)])\s+", "", line_str))
            elif line_str:
                criteria_lines.append(line_str)

    found_crit = "; ".join(criteria_lines).strip() if criteria_lines else None

    # 3. Check for Markdown contract sections. Fenced code is opaque so a
    # heading in an example cannot create or terminate a contract section.
    markdown_values: dict[str, str | None] = {"objective": None, "criteria": None}
    active_sections: list[dict[str, Any]] = []
    in_fence = False

    def finish_section(section: dict[str, Any]) -> None:
        body_lines = [line.strip() for line in section["lines"] if line.strip()]
        values: list[str] = []
        for body_line in body_lines:
            body_line = re.sub(r"^(?:[-*+]|\d+[.)])\s+", "", body_line)
            if body_line:
                values.append(body_line)
        value = "; ".join(values).strip()
        if value:
            markdown_values[section["kind"]] = value

    for line in lines:
        line_str = line.strip()
        if line_str.startswith("```"):
            in_fence = not in_fence
            continue
        if in_fence:
            continue

        heading_match = re.match(r"^(#{1,3})[ \t]+(.+?)\s*$", line_str)
        if heading_match:
            level = len(heading_match.group(1))
            heading_text = heading_match.group(2).strip()
            title, separator, inline_body = heading_text.partition(":")
            title = title.strip().lower()
            kind = (
                "objective"
                if title in {"objective", "task objective", "task", "goal"}
                else "criteria"
                if title in {"acceptance criteria", "criteria", "requirements"}
                else None
            )

            remaining: list[dict[str, Any]] = []
            for section in active_sections:
                if section["level"] >= level:
                    finish_section(section)
                else:
                    remaining.append(section)
            active_sections = remaining

            if kind:
                active_sections.append(
                    {
                        "kind": kind,
                        "level": level,
                        "lines": [inline_body.strip()] if separator and inline_body.strip() else [],
                    }
                )
            continue

        for section in active_sections:
            if not any(child["level"] > section["level"] for child in active_sections):
                section["lines"].append(line)

    for section in active_sections:
        finish_section(section)

    found_obj = markdown_values["objective"] or found_obj
    found_crit = markdown_values["criteria"] or found_crit

    # CRITICAL: grounded is True ONLY IF BOTH are found!
    if found_obj and found_crit:
        return found_obj, found_crit, True
    return (found_obj or default_obj), (found_crit or default_crit), False


_ALLOWED_VALIDATION_KEYS = frozenset({
    "source",
    "status",
    "summary",
    "commands",
    "exit_codes",
    "artifacts",
})

_WORKER_VALIDATION_INSTRUCTION = (
    "Worker validation: End your final report with exactly one `VALIDATION:` JSON line. "
    f"Use only these keys: {', '.join(f'`{key}`' for key in sorted(_ALLOWED_VALIDATION_KEYS))}. "
    'Set `source` to `WORKER_REPORTED`; set `status` to `PASS`, `FAIL`, or `UNKNOWN`; '
    "list the commands actually run in `commands` and matching integer `exit_codes`. "
    "List only final validation runs in `commands`; put any deliberate red check in the report prose, never in the `VALIDATION:` line. "
    "Do not emit another validation line."
)


_WORKER_TEST_INSTRUCTIONS = {
    False: "Run focused tests for what you changed; do not run the full test suite unless the task asks for it.",
    True: "Run focused tests while working, then run the full test suite once before your final report.",
}
_WORKER_CHANGE_INSTRUCTIONS = (
    "Make the smallest change that meets the criteria; add no speculative helpers, abstractions, flags, or refactors."
)
_WORKER_HANG_INSTRUCTION = (
    "If an unrelated test hangs or runs far too long, stop it, report it as skipped, and continue."
)


def _append_worker_validation_instruction(
    prompt: str, *, full_suite_required: bool = False
) -> str:
    """Append the worker-report contract once, preserving the caller's prompt text."""

    additions = (
        _WORKER_TEST_INSTRUCTIONS[bool(full_suite_required)],
        _WORKER_CHANGE_INSTRUCTIONS,
        _WORKER_HANG_INSTRUCTION,
    )
    instruction = " ".join((_WORKER_VALIDATION_INSTRUCTION, *additions))
    if instruction in prompt:
        return prompt
    if _WORKER_VALIDATION_INSTRUCTION in prompt:
        missing = tuple(sentence for sentence in additions if sentence not in prompt)
        if not missing:
            return prompt
        separator = "" if not prompt or prompt.endswith("\n") else " "
        return f"{prompt}{separator}{' '.join(missing)}"
    separator = "" if not prompt or prompt.endswith("\n") else "\n"
    return f"{prompt}{separator}{instruction}"


def _canonical_unknown_validation(summary: str | None = None) -> dict[str, Any]:
    """Return the canonical, fixed-shape UNKNOWN validation record within IMPLEMENTER_REPORT_LIMIT bytes."""
    default_summary = "No verifiable validation evidence was provided."
    target_summary = summary.strip() if isinstance(summary, str) and summary.strip() else default_summary

    record: dict[str, Any] = {
        "source": "WORKER_REPORTED",
        "status": "UNKNOWN",
        "summary": target_summary,
        "commands": [],
        "exit_codes": [],
        "artifacts": [],
    }

    try:
        encoded = json_transport.dumps(record).encode("utf-8")
        if len(encoded) <= IMPLEMENTER_REPORT_LIMIT:
            return record
    except (TypeError, ValueError):
        pass

    low = 0
    high = len(target_summary)
    best_summary = ""
    while low <= high:
        mid = (low + high) // 2
        candidate = target_summary[:mid]
        record["summary"] = candidate
        try:
            enc = json_transport.dumps(record).encode("utf-8")
            if len(enc) <= IMPLEMENTER_REPORT_LIMIT:
                best_summary = candidate
                low = mid + 1
            else:
                high = mid - 1
        except (TypeError, ValueError):
            high = mid - 1

    record["summary"] = best_summary
    return record


def _parse_worker_validation(report: str | None) -> dict[str, Any]:
    """Parse and structure worker validation evidence.

    Fails closed: Mechanical PASS or FAIL status is extracted strictly and
    exclusively from a single valid structured JSON record:
        VALIDATION: {"source":"WORKER_REPORTED","status":"PASS|FAIL|UNKNOWN", ...}

    Free-form text without a valid structured JSON record is preserved as
    narrative summary with status="UNKNOWN" to eliminate natural language
    heuristic classification errors.
    """
    if not isinstance(report, str) or not report.strip():
        return _canonical_unknown_validation("No implementer report was provided.")

    validation_lines: list[str] = []
    for line in report.splitlines():
        line_str = line.strip()
        if line_str.startswith("`") and line_str.endswith("`"):
            line_str = line_str[1:-1].strip()
        if line_str.startswith("VALIDATION:") and line_str[len("VALIDATION:"):].strip():
            validation_lines.append(line_str)

    if len(validation_lines) == 0:
        val_idx = report.lower().find("validation:")
        summary = report[val_idx:].strip() if val_idx != -1 else report.strip()
        return _canonical_unknown_validation(summary)

    if len(validation_lines) > 1:
        return _canonical_unknown_validation("Multiple conflicting VALIDATION records were reported.")

    line_str = validation_lines[0]

    def reject(reason: str) -> dict[str, Any]:
        return _canonical_unknown_validation(f"{reason}: {line_str}")

    payload = line_str[len("VALIDATION:"):].strip()
    try:
        data = json.loads(payload)
    except (json.JSONDecodeError, TypeError, ValueError):
        return reject("invalid_json")

    if not isinstance(data, dict):
        return reject("not_object")

    unknown_keys = set(data.keys()) - _ALLOWED_VALIDATION_KEYS
    if unknown_keys:
        return reject(f"unknown_keys={','.join(sorted(unknown_keys))}")

    if data.get("source") != "WORKER_REPORTED":
        return reject("bad_source")

    raw_status = data.get("status")
    if raw_status not in ("PASS", "FAIL", "UNKNOWN"):
        return reject("bad_status")

    if "summary" in data:
        summary_val = data.get("summary")
        if not isinstance(summary_val, str) or not summary_val.strip():
            return reject("bad_summary")
        clean_summary = summary_val.strip()
    else:
        if raw_status == "PASS":
            clean_summary = "Structured worker validation reported PASS."
        elif raw_status == "FAIL":
            clean_summary = "Structured worker validation reported FAIL."
        else:
            clean_summary = "Structured worker validation reported UNKNOWN."

    if "commands" in data:
        commands_val = data.get("commands")
        if not isinstance(commands_val, list):
            return reject("bad_commands")
        clean_commands = []
        for cmd in commands_val:
            if not isinstance(cmd, str) or not cmd.strip():
                return reject("bad_commands")
            clean_commands.append(cmd.strip())
    else:
        clean_commands = []

    if "artifacts" in data:
        artifacts_val = data.get("artifacts")
        if not isinstance(artifacts_val, list):
            return reject("bad_artifacts")
        clean_artifacts = []
        for art in artifacts_val:
            if not isinstance(art, str) or not art.strip():
                return reject("bad_artifacts")
            clean_artifacts.append(art.strip())
    else:
        clean_artifacts = []

    if "exit_codes" in data:
        exit_codes_val = data.get("exit_codes")
        if not isinstance(exit_codes_val, list):
            return reject("bad_exit_codes")
        clean_exit_codes = []
        for code in exit_codes_val:
            if isinstance(code, bool) or not isinstance(code, int):
                return reject("bad_exit_codes")
            clean_exit_codes.append(code)
    else:
        clean_exit_codes = []

    if len(clean_commands) != len(clean_exit_codes):
        return reject("commands_exit_codes_mismatch")

    if raw_status == "PASS":
        if not clean_commands:
            return reject("pass_without_commands")
        if any(code != 0 for code in clean_exit_codes):
            return reject("pass_with_nonzero_exit")
    elif raw_status == "FAIL":
        if not clean_commands:
            return reject("fail_without_commands")
        if not any(code != 0 for code in clean_exit_codes):
            return reject("fail_without_nonzero_exit")

    candidate = {
        "source": "WORKER_REPORTED",
        "status": raw_status,
        "summary": clean_summary,
        "commands": clean_commands,
        "exit_codes": clean_exit_codes,
        "artifacts": clean_artifacts,
    }

    try:
        encoded = json_transport.dumps(candidate).encode("utf-8")
        if len(encoded) > IMPLEMENTER_REPORT_LIMIT:
            return reject("record_too_large")
    except (TypeError, ValueError):
        return reject("serialization_error")

    return candidate


def _attempt_in_hook_micro_repair(
    thread_id: str,
    findings: Sequence[Mapping[str, str]],
    repo_root: Path,
    scope: Sequence[str] | None = None,
    timeout_seconds: int = 60,
) -> dict[str, Any]:
    """Attempt 1 bounded micro-repair via codex exec resume with workspace sandbox."""
    if not thread_id or not findings:
        return {"repaired": False, "reason": "NO_THREAD_OR_FINDINGS"}
    bullet_points = "\n".join(
        f"- {f.get('code')}: {f.get('message')}"
        for f in findings
        if isinstance(f, Mapping) and f.get("code")
    )[:500]
    scope_text = f"\nYou are strictly restricted to modifying files within scope: {', '.join(scope)}." if scope else ""
    repair_prompt = (
        "Micro-repair request: Reviewer-1 flagged the following issues in your previous diff:\n"
        f"{bullet_points}{scope_text}\n"
        "Please fix these issues directly in the worktree and ensure tests pass."
    )
    cmd = [
        "codex",
        "exec",
        "resume",
        thread_id,
        "-",
        "--sandbox",
        "workspace-write",
    ]
    try:
        sys.stderr.write(
            f"[orchestrator] Reviewer-1 requested changes; entering in-hook micro-repair with Codex (timeout={timeout_seconds}s)...\n"
        )
        sys.stderr.flush()
        proc = subprocess.run(
            cmd,
            input=repair_prompt,
            cwd=str(repo_root),
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            shell=False,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0x08000000) if os.name == "nt" else 0,
        )
        if proc.returncode == 0:
            sys.stderr.write("[orchestrator] In-hook micro-repair succeeded; re-evaluating with Reviewer-1...\n")
            sys.stderr.flush()
            return {"repaired": True, "output": proc.stdout}
        sys.stderr.write(f"[orchestrator] In-hook micro-repair failed (exit {proc.returncode}).\n")
        sys.stderr.flush()
        return {"repaired": False, "error": proc.stderr or proc.stdout}
    except subprocess.TimeoutExpired:
        sys.stderr.write(f"[orchestrator] In-hook micro-repair timed out after {timeout_seconds}s.\n")
        sys.stderr.flush()
        return {"repaired": False, "error": f"TimeoutExpired: {timeout_seconds}s"}
    except Exception as exc:
        sys.stderr.write(f"[orchestrator] In-hook micro-repair error: {_safe_error(exc)}\n")
        sys.stderr.flush()
        return {"repaired": False, "error": _safe_error(exc)}


MICRO_REPAIR: Callable[..., Mapping[str, Any]] = _attempt_in_hook_micro_repair


class WiringError(RuntimeError):
    pass


class AsyncAdoptionRequired(WiringError):
    """A same-project successor session needs explicit user consent."""


def _observe(entrypoint: str, *args: Any, **kwargs: Any) -> None:
    """Keep even a broken or monkeypatched telemetry entry point observational."""

    try:
        getattr(telemetry, entrypoint)(*args, **kwargs)
    except BaseException:
        try:
            telemetry.record_health(
                "DEGRADED",
                error_code="TELEMETRY_ENTRYPOINT_FAILURE",
                state_root=kwargs.get("state_root"),
            )
        except BaseException:
            pass


def _telemetry_envelope(
    event: Mapping[str, Any],
    repository: str | os.PathLike[str] | None = None,
    project_key: str | None = None,
) -> dict[str, Any]:
    if project_key is None:
        if repository is not None:
            project_key = project_root.project_key(Path(repository))
        else:
            resolved, _ = project_root.resolve_project_root(event)
            project_key = project_root.project_key(resolved)
    repo_name = Path(repository).name if repository is not None else None
    branch = None
    if repository is not None:
        try:
            with (Path(repository) / ".git" / "HEAD").open("rb") as handle:
                raw_head = handle.read(GIT_HEAD_READ_LIMIT + 1)
            if len(raw_head) > GIT_HEAD_READ_LIMIT:
                raw_head = b""
            head = raw_head.decode("utf-8").strip()
            prefix = "ref: refs/heads/"
            if head.startswith(prefix) and len(head) > len(prefix):
                branch = head[len(prefix) :]
        except (OSError, UnicodeDecodeError):
            pass
    agent_id = event.get("agent_id")
    agent_type = event.get("agent_type")
    return {
        "session_id": event.get("session_id"),
        "project_key": project_key,
        "repo": repo_name,
        "branch": branch,
        "agent_id": agent_id if isinstance(agent_id, str) and agent_id else None,
        "agent_type": agent_type if isinstance(agent_type, str) and agent_type else None,
    }


def _observed_denial(
    event: Mapping[str, Any],
    code: str,
    message: str,
    *,
    state_root: str | os.PathLike[str] | None,
    task_id: str | None = None,
    continuity: bool = False,
) -> dict[str, Any]:
    _observe(
        "emit",
        "THREAD_CONTINUITY_VIOLATION" if continuity else "ROUTING_PROTOCOL_VIOLATION",
        task_id=task_id,
        state_root=state_root,
        violation_code=code,
        **_telemetry_envelope(event),
    )
    return _deny(code, message)


def _numeric_duration_ms(value: Any) -> int | float | None:
    if (
        isinstance(value, (int, float))
        and not isinstance(value, bool)
        and value >= 0
        and math.isfinite(value)
    ):
        return value
    return None


def _codex_usage(tool_response: Any) -> dict[str, int | float]:
    candidates: list[Any] = [tool_response]
    if isinstance(tool_response, Mapping):
        candidates.append(tool_response.get("usage"))
        candidates.append(tool_response.get("content"))
    if isinstance(tool_response, list):
        candidates.extend(tool_response)
    usage: Mapping[str, Any] | None = None
    for candidate in candidates:
        if isinstance(candidate, Mapping) and isinstance(candidate.get("usage"), Mapping):
            usage = candidate["usage"]
            break
        text = candidate.get("text") if isinstance(candidate, Mapping) else candidate
        if not isinstance(text, str):
            continue
        try:
            decoded = json.loads(text)
        except json.JSONDecodeError:
            continue
        if isinstance(decoded, Mapping) and isinstance(decoded.get("usage"), Mapping):
            usage = decoded["usage"]
            break
    if usage is None:
        return {}
    return {
        key: value
        for key, value in usage.items()
        if key in telemetry_schema.TOKEN_FIELDS
        and isinstance(value, (int, float))
        and not isinstance(value, bool)
        and value >= 0
        and math.isfinite(value)
    }


def _load_config(config: Mapping[str, Any] | None) -> Mapping[str, Any]:
    if config is None:
        loaded = coder_config.load(_CONFIG_PATH)
    else:
        loaded = config
    policy.validate_config(loaded)
    if loaded.get("post_tool_output_field") not in {
        "updatedToolOutput",
        "updatedMCPToolOutput",
    }:
        raise WiringError("post_tool_output_field is not a supported hook field")
    reviewer_model = loaded.get("reviewer1_model_requested")
    if not isinstance(reviewer_model, str) or not reviewer_model:
        raise WiringError("reviewer1_model_requested must be a non-empty string")
    deadline = loaded.get("hook_checks_deadline_seconds", 150)
    if isinstance(deadline, bool) or not isinstance(deadline, (int, float)) or not math.isfinite(deadline) or deadline <= 0:
        raise WiringError("hook_checks_deadline_seconds must be positive")
    return loaded


def _check_snapshot(checks_to_store: Sequence[project_profile.Check]) -> list[dict[str, Any]]:
    return [{"name": check.name, "argv": list(check.argv),
             "when": list(check.when) if check.when is not None else None,
             "timeout_seconds": check.timeout_seconds} for check in checks_to_store]


def _restore_checks(snapshot: Any) -> tuple[project_profile.Check, ...]:
    if not isinstance(snapshot, list):
        raise ValueError("invalid check snapshot")
    restored = []
    for item in snapshot:
        if not isinstance(item, Mapping) or set(item) != {"name", "argv", "when", "timeout_seconds"}:
            raise ValueError("invalid check snapshot")
        name, argv, when, timeout = (item[key] for key in ("name", "argv", "when", "timeout_seconds"))
        if (not isinstance(name, str) or not isinstance(argv, list) or not argv
                or not all(isinstance(arg, str) and arg for arg in argv)
                or (when is not None and (not isinstance(when, list) or not all(isinstance(path, str) for path in when)))
                or type(timeout) is not int or not 1 <= timeout <= 300):
            raise ValueError("invalid check snapshot")
        restored.append(project_profile.Check(name, tuple(argv), tuple(when) if when is not None else None, timeout))
    return tuple(restored)


def _state_root(root: str | os.PathLike[str] | None) -> Path:
    if root is not None:
        return Path(root)
    return purser_state_root.default_state_root()


def _repo_root(
    event: Mapping[str, Any],
    root: str | os.PathLike[str] | None,
) -> tuple[Path | None, str]:
    return project_root.resolve_project_root(event, root)


def _last_launch_failure(root: Path, project_key: str) -> dict[str, Any] | None:
    jobs = root / "codex_exec_mcp" / "jobs"
    indexes = root / "async_jobs"
    latest: tuple[float, dict[str, Any], str] | None = None
    try:
        for index_path in indexes.glob("*.json"):
            try:
                index = json.loads(index_path.read_text(encoding="utf-8"))
                job_id = index.get("jobId")
                if (not isinstance(index, dict) or index.get("project_key") != project_key
                        or not isinstance(job_id, str) or not re.fullmatch(r"[0-9a-f]{64}", job_id)):
                    continue
                job = json.loads((jobs / f"{job_id}.json").read_text(encoding="utf-8"))
                evidence = job.get("launchFailureEvidence")
                stamp = job.get("finishedAt")
                if (job.get("jobId") != job_id
                        or job.get("phase", job.get("jobPhase")) != "implementation"
                        or not isinstance(stamp, (int, float)) or isinstance(stamp, bool)
                        or not math.isfinite(stamp)):
                    continue
                candidate = (float(stamp), job, job_id)
                if latest is None or candidate[0] > latest[0]:
                    latest = candidate
            except (OSError, ValueError, TypeError, AttributeError):
                continue
    except OSError:
        return None
    if latest is None or latest[1].get("state") not in {"FAILED", "TIMED_OUT"}:
        return None
    evidence = latest[1].get("launchFailureEvidence")
    if (not isinstance(evidence, dict)
            or evidence.get("type") not in {"CREDIT_EXHAUSTED", "RATE_LIMITED", "NO_RESPONSE"}):
        return None
    return {**evidence, "job_id": latest[2], "trigger_id": latest[2]}


def _fallback_question(evidence: Mapping[str, Any], fallback: Mapping[str, Any],
                       quota: Mapping[str, Any] | None = None) -> tuple[str, str]:
    model = str(fallback["model"])
    kind = evidence.get("type")
    if kind == "QUOTA":
        reset = quota.get("5h_resets_at") if isinstance(quota, Mapping) else None
        try:
            minutes = max(1, math.ceil((datetime.fromisoformat(reset.replace("Z", "+00:00"))
                                       - datetime.now(timezone.utc)).total_seconds() / 60))
        except (AttributeError, TypeError, ValueError):
            minutes = 5
        return "quota", f"⚓ Kotan dolmak üzere, sıradaki iş yarım kalabilir. Yenilenmesini ({minutes} dakika) bekleyecek misin, yoksa yedek kodlayıcıya ({model}) geçilsin mi?"
    if kind == "CREDIT_EXHAUSTED":
        return "credit", f"⚓ API kredin bitti, sıradaki iş başlayamaz. Kredi yükleyecek misin, yoksa yedek kodlayıcıya ({model}) geçilsin mi?"
    if kind == "RATE_LIMITED":
        seconds = evidence.get("retryAfterSeconds")
        minutes = max(1, math.ceil(seconds / 60)) if isinstance(seconds, (int, float)) and not isinstance(seconds, bool) else 5
        return "rate_limit", f"⚓ API hız sınırına takıldın. Açılmasını ({minutes} dakika) bekleyecek misin, yoksa yedek kodlayıcıya ({model}) geçilsin mi?"
    return "unresponsive", f"⚓ Codex 5 denemede yanıt vermedi. Biraz sonra yeniden denensin mi, yoksa yedek kodlayıcıya ({model}) geçilsin mi?"


def _pending_project_key(pending: Mapping[str, Any]) -> str:
    """Return the call's namespace; derive it from the stored root for older pending records."""

    stored = pending.get("project_key")
    if isinstance(stored, str) and stored:
        return stored
    repository = pending.get("repo_root")
    return project_root.project_key(
        Path(repository).resolve() if isinstance(repository, str) and repository else None
    )


def _project_state_root(runtime_root: Path, project_key: str) -> Path:
    if not isinstance(project_key, str) or not project_key:
        raise WiringError("project_key is missing")
    return runtime_root / "projects" / project_key


def _release_scope_lease(runtime_root: Path, pending: Mapping[str, Any]) -> None:
    owner = pending.get("lease_owner")
    if not isinstance(owner, str):
        return
    result = ledger.ScopeLeases(
        _project_state_root(runtime_root, _pending_project_key(pending))
    ).release(owner)
    if result["status"] != ledger.OK:
        raise WiringError(result["code"])


def _release_async_lease(runtime_root: Path, index: Mapping[str, Any],
                         pending: Mapping[str, Any] | None) -> None:
    if isinstance(pending, Mapping):
        _release_scope_lease(runtime_root, pending)
        return
    # A consumed index survives pending cleanup; its call key is the lease owner.
    result = ledger.ScopeLeases(
        _project_state_root(runtime_root, index["project_key"])
    ).release(index["call_key"])
    if result["status"] != ledger.OK and result["code"] != "LEASE_OWNER_MISSING":
        raise WiringError(result["code"])


def bind_queued_baseline(runtime_root: Path, job_id: str) -> bool:
    """Bind fresh review evidence after the queued lease becomes active."""
    index = json.loads((runtime_root / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
    pending_path = runtime_root / "pending" / f"{index['call_key']}.json"
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    if (index.get("jobId") != job_id or pending.get("job_id") != job_id
            or not pending.get("lease_queued")
            or index.get("call") != pending.get("async_call")
            or index.get("project_key") != pending.get("project_key")
            or index.get("call_key") != pending.get("lease_owner")
            or hashlib.sha256(index["call"].encode("utf-8")).hexdigest() != job_id):
        raise WiringError("queued job ownership is unavailable")
    project_state = _project_state_root(runtime_root, pending["project_key"])
    if ledger.ScopeLeases(project_state).owner_state(pending["lease_owner"]) != "ACTIVE":
        return False
    baseline = ledger.capture_baseline(
        Path(pending["repo_root"]), pending["scope"],
    )
    if baseline.get("status") != ledger.OK:
        raise WiringError("admission baseline is unavailable")
    session = ledger.SessionLedger(project_state)
    loaded = session.load(pending["task_id"])
    if loaded["status"] != ledger.OK:
        raise WiringError("queued session record is unavailable")
    record = dict(loaded["record"], pending_call_baseline=baseline)
    saved = session.save(record)
    if saved["status"] != ledger.OK:
        raise WiringError("admission baseline could not be bound")
    pending["baseline"] = baseline
    _atomic_json_write(pending_path, pending)
    return True


def _reconcile_rejected_starts(runtime_root: Path, project_key: str) -> None:
    """Clear a rejected start only after its durable bridge job is terminal."""
    for path in (runtime_root / "pending").glob("*.json"):
        try:
            pending = json.loads(path.read_text(encoding="utf-8"))
            if (not isinstance(pending, dict) or pending.get("start_rejected") is not True
                    or pending.get("project_key") != project_key
                    or not _start_job_terminal_or_absent(runtime_root, pending)):
                continue
            _release_scope_lease(runtime_root, pending)
            path.unlink(missing_ok=True)
        except (OSError, ValueError, TypeError, WiringError):
            continue


def _thread_index_path(runtime_root: Path, thread_id: Any) -> Path | None:
    if not isinstance(thread_id, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", thread_id):
        return None
    return runtime_root / "threads" / (hashlib.sha256(thread_id.encode("utf-8")).hexdigest() + ".json")


def _indexed_reply_root(runtime_root: Path, thread_id: str) -> tuple[Path | None, str]:
    path = _thread_index_path(runtime_root, thread_id)
    if path is None:
        return None, "INVALID_THREAD_ID"
    try:
        entry = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None, ""
    except (OSError, UnicodeError, ValueError):
        return None, "RECORDED_PROJECT_UNRESOLVED"
    if not isinstance(entry, dict) or entry.get("thread_id") != thread_id:
        return None, "RECORDED_PROJECT_UNRESOLVED"
    stored = entry.get("project_root")
    if not isinstance(stored, str) or not stored or not Path(stored).is_absolute():
        return None, "RECORDED_PROJECT_UNRESOLVED"
    resolved, _ = project_root.resolve_candidate(stored)
    try:
        same_root = resolved == Path(stored).resolve()
    except (OSError, RuntimeError, ValueError):
        same_root = False
    if resolved is None or not same_root:
        return None, "RECORDED_PROJECT_UNRESOLVED"
    return resolved, ""


def _record_thread_root(runtime_root: Path, thread_id: str | None, repository: Path | None) -> None:
    path = _thread_index_path(runtime_root, thread_id)
    if path is not None and repository is not None:
        _atomic_json_write(path, {"thread_id": thread_id, "project_root": str(repository)})


def _call_key(event: Mapping[str, Any]) -> str:
    tool_use_id = event.get("tool_use_id")
    if not isinstance(tool_use_id, str) or not tool_use_id:
        raise WiringError("tool_use_id is missing")
    session_id = event.get("session_id")
    identity = f"{session_id if isinstance(session_id, str) else ''}\0{tool_use_id}"
    return hashlib.sha256(identity.encode("utf-8")).hexdigest()


def _pending_path(root: Path, event: Mapping[str, Any]) -> Path:
    return root / "pending" / f"{_call_key(event)}.json"


def _bypass_path(root: Path, event: Mapping[str, Any]) -> Path:
    tool_input = event.get("tool_input")
    encoded_input = json_transport.dumps(
        tool_input,
        sort_keys=True,
        separators=(",", ":"),
        default=str,
    )
    session_id = event.get("session_id")
    identity = (
        f"{session_id if isinstance(session_id, str) else ''}\0"
        f"{event.get('tool_name')}\0{encoded_input}"
    )
    digest = hashlib.sha256(identity.encode("utf-8")).hexdigest()
    return root / "pending" / f"bypass-{digest}.json"


def _pending_candidates(root: Path, event: Mapping[str, Any]) -> list[Path]:
    candidates: list[Path] = []
    try:
        candidates.append(_pending_path(root, event))
    except WiringError:
        pass
    bypass = _bypass_path(root, event)
    if bypass not in candidates:
        candidates.append(bypass)
    return candidates


def _atomic_json_write(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            prefix=path.name + ".",
            suffix=".tmp",
            dir=path.parent,
            delete=False,
        ) as handle:
            temporary = Path(handle.name)
            handle.write(json_transport.dumps(value, sort_keys=True, separators=(",", ":")))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        temporary = None
    finally:
        if temporary is not None:
            try:
                temporary.unlink()
            except OSError:
                pass


def _read_pending(root: Path, event: Mapping[str, Any]) -> dict[str, Any]:
    for path in _pending_candidates(root, event):
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            continue
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
            raise WiringError("pending call record is unreadable") from error
        if not isinstance(value, dict):
            raise WiringError("pending call record is invalid")
        return value
    raise WiringError("pending call record is missing")


def _remove_pending(root: Path, event: Mapping[str, Any]) -> None:
    for path in _pending_candidates(root, event):
        try:
            path.unlink()
        except FileNotFoundError:
            pass


def _cleanup_stale_pending(root: Path, event: Mapping[str, Any]) -> None:
    """Best-effort bounded cleanup, biased toward preserving reconciliation state."""

    try:
        current = set(_pending_candidates(root, event))
        now = datetime.now(timezone.utc).timestamp()
        for index, path in enumerate((root / "pending").glob("*.json")):
            if index >= PENDING_CLEANUP_LIMIT:
                break
            if path in current:
                continue
            try:
                age_seconds = now - path.stat().st_mtime
            except OSError:
                continue
            if age_seconds <= PENDING_MAX_AGE_SECONDS:
                continue
            try:
                old = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(old, dict) and (isinstance(old.get("lease_owner"), str)
                                              or isinstance(old.get("async_call"), str)
                                              or isinstance(old.get("job_id"), str)):
                    continue
            except (OSError, ValueError, TypeError):
                continue
            try:
                path.unlink()
            except OSError:
                pass
    except BaseException:
        pass


def _safe_error(error: BaseException) -> str:
    message = str(error).strip()
    return f"{type(error).__name__}: {message}" if message else type(error).__name__


def _record_diagnostic(
    root: Path | None,
    event: Mapping[str, Any],
    phase: str,
    error: BaseException,
) -> None:
    record = {
        "at": datetime.now(timezone.utc).isoformat(),
        "phase": phase,
        "hook_event_name": event.get("hook_event_name"),
        "tool_name": event.get("tool_name"),
        "tool_use_id": event.get("tool_use_id"),
        "error": _safe_error(error),
    }
    if root is not None:
        try:
            path = root / "diagnostics.jsonl"
            path.parent.mkdir(parents=True, exist_ok=True)
            with path.open("a", encoding="utf-8") as handle:
                handle.write(json_transport.dumps(record, sort_keys=True) + "\n")
            return
        except OSError:
            pass
    try:
        sys.stderr.write(json_transport.dumps(record, sort_keys=True) + "\n")
    except Exception:
        pass


_PROTOCOL_CODES = frozenset(
    {
        policy.MISSING_ORCH_LINE,
        policy.MALFORMED_ORCH_LINE,
        policy.UNKNOWN_KEY,
        policy.MISSING_REQUIRED_KEY,
        policy.INVALID_ENUM_VALUE,
        policy.SOL_ROUTE_NEEDS_BASIS,
        "UNBOUNDED_SCOPE",
        "NO_LIVE_THREAD",
    }
)


_PROTOCOL_HELP = (
    "Prepend this as the FIRST line of the prompt; the hook removes it before Codex sees it:\n"
    "  ORCH|task=<stable-id>|phase=<implementation|operation>|route=<route>\n"
    "  Add |scope=<path[,path...]> for implementation; omit it for operation.\n"
    "  route  mechanical -> luna/high | subtle -> luna/high | normal -> sol/medium"
    " | consequential -> sol/high | escalated -> sol/xhigh | operation -> luna/high\n"
    "  reason is optional; basis is required for normal, consequential, and escalated, and optional otherwise."
    " eta=<N>m is optional; N must be an integer from 1..600."
    " Reason, when supplied, is one of new_task | continue |"
    " route_change | reanchor | repeated_failure | user_override.\n"
    "  phase  'operation' for deploy, runner control, status, or read-only investigation;"
    " it skips Reviewer-1. 'implementation' for anything that changes code.\n"
    "  scope  required for implementation; the paths the delegation may touch. It bounds"
    " the baseline scan, so it cannot be the whole repository.\n"
    "  codex-reply may omit ORCH only when its threadId identifies a recorded thread;"
    " otherwise the reply is denied.\n"
    "See .claude/skills/codex-orchestrator/SKILL.md for how to choose the route."
)


def _deny(code: str, message: str) -> dict[str, Any]:
    # The denial has to teach the protocol, not merely name the violation. A
    # session that has not loaded the skill would otherwise learn only that
    # something is missing, and retry blind.
    reason = f"{code}: {message}"
    if code in _PROTOCOL_CODES:
        reason = f"{reason}\n\n{_PROTOCOL_HELP}"
    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": reason,
        }
    }


def _internal_pre_allow(reason: str, recorded: bool) -> dict[str, Any]:
    persistence = "The bypass was recorded for PostToolUse." if recorded else (
        "The bypass record could not be persisted; this PreToolUse context is the only warning."
    )
    return {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "additionalContext": (
                "Orchestrator routing was NOT enforced for this Codex call because of an "
                f"internal error: {reason}. The call is allowed unmodified. {persistence}"
            ),
        }
    }


def _write_pre_bypass(
    root: Path,
    event: Mapping[str, Any],
    reason: str,
    metadata: Mapping[str, Any],
) -> None:
    record = {
        "kind": "pretool_bypass",
        "routing_enforced": False,
        "baseline_captured": False,
        "reason": reason,
        "task_id": metadata.get("task_id"),
        "project_key": metadata.get("project_key", project_root.PROJECT_KEY_UNRESOLVED),
        "phase": metadata.get("phase"),
        "route": metadata.get("route"),
        "model": metadata.get("model"),
        "effort": metadata.get("effort"),
        "scope": metadata.get("scope", []),
        "repo_root": metadata.get("repo_root"),
        "prior_thread_id": metadata.get("prior_thread_id"),
    }
    try:
        path = _pending_path(root, event)
    except WiringError:
        path = _bypass_path(root, event)
    _atomic_json_write(path, record)


def _reviewer_available(reviewer: Callable[..., Mapping[str, Any]]) -> bool:
    marker = getattr(reviewer, "available", None)
    if isinstance(marker, bool):
        return marker
    return reviewer is not _default_reviewer


def _requested_thread(parsed: policy.OrchParseResult, route: policy.RouteResolution) -> dict[str, str]:
    assert parsed.task_id and parsed.route and route.model and route.effort
    return {
        "task_id": parsed.task_id,
        "route": parsed.route,
        "model": route.model,
        "effort": route.effort,
    }


def _thread_worktree_path(thread_id: Any) -> str | None:
    if not isinstance(thread_id, str) or not thread_id:
        return None
    try:
        record = codex_exec_mcp._load_record(codex_exec_mcp._state_dir(), thread_id)
        path = record.get("worktreePath") if isinstance(record, Mapping) else None
        return path if isinstance(path, str) and path else None
    except Exception:
        return None


def _inherited_reply_parse(
    prompt: Any,
    live_thread: Mapping[str, Any],
) -> policy.OrchParseResult | None:
    """Build protocol metadata from the durable thread record for a bare reply."""

    task_id = live_thread.get("task_id")
    phase = live_thread.get("phase")
    route = live_thread.get("route")
    scope = live_thread.get("scope")
    if (
        not isinstance(prompt, str)
        or not isinstance(task_id, str)
        or not isinstance(phase, str)
        or not isinstance(route, str)
        or not isinstance(scope, list)
        or any(not isinstance(path, str) for path in scope)
    ):
        return None
    return policy.OrchParseResult(
        valid=True,
        code=policy.VALID,
        message="ORCH metadata inherited from the recorded reply thread.",
        prompt=prompt,
        task_id=task_id,
        phase=phase,
        route=route,
        reason=live_thread.get("reason") if isinstance(live_thread.get("reason"), str) else None,
        scope=tuple(scope),
        eta_minutes=None,
    )


def internal_pre_error_response(
    event: Mapping[str, Any],
    error: BaseException,
    *,
    state_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Allow an internally failed PreToolUse call while making the bypass visible."""

    # Async start cannot launch without a baseline for its later result review.
    if event.get("tool_name") in CODEX_START_TOOL_NAMES:
        return _deny("ASYNC_ENFORCEMENT_FAILED", "Async Codex hook failed; FULL review is required.")

    runtime_root: Path | None = None
    reason = _safe_error(error)
    recorded = False
    try:
        runtime_root = _state_root(state_root)
        _write_pre_bypass(runtime_root, event, reason, {})
        recorded = True
    except Exception as persistence_error:
        reason = f"{reason}; bypass persistence failed: {_safe_error(persistence_error)}"
    _record_diagnostic(runtime_root, event, "PreToolUse", error)
    return _internal_pre_allow(reason, recorded)


def pre_tool_use(
    event: Mapping[str, Any],
    *,
    config: Mapping[str, Any] | None = None,
    state_root: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Validate routing, capture a baseline, and rewrite a Codex MCP input."""

    if event.get("tool_name") not in CODEX_TOOLS:
        return {}
    if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES | CODEX_CANCEL_TOOL_NAMES:
        return _async_pre_followup(event, state_root, repo_root)
    runtime_root: Path | None = None
    pending_written = False
    lease_pending: dict[str, Any] | None = None
    bypass_metadata: dict[str, Any] = {}
    try:
        loaded_config = _load_config(config)
        tool_input = event.get("tool_input")
        if not isinstance(tool_input, Mapping):
            return _observed_denial(
                event,
                "MALFORMED_TOOL_INPUT",
                "tool_input must be an object",
                state_root=state_root,
            )

        tool_name = event["tool_name"]
        is_start = tool_name in CODEX_START_TOOL_NAMES
        mode = tool_input.get("mode", "new") if is_start else None
        if is_start and mode not in ("new", "reply"):
            return _deny("INVALID_ASYNC_MODE", "codex_start mode must be new or reply.")
        if is_start and "etaMinutes" in tool_input:
            supplied_eta = tool_input["etaMinutes"]
            if (isinstance(supplied_eta, bool) or not isinstance(supplied_eta, int)
                    or not 1 <= supplied_eta <= 600):
                return _deny("INVALID_ETA_MINUTES", "codex_start etaMinutes must be an integer from 1..600.")
        new_call = tool_name in CODEX_TOOL_NAMES or (is_start and mode == "new")
        reply_call = tool_name in CODEX_REPLY_TOOL_NAMES or (is_start and mode == "reply")
        task_label = None
        if "label" in tool_input:
            value = tool_input.get("label")
            if (not isinstance(value, str) or not value.strip()
                    or len(value.strip()) > 80 or not value.strip().isprintable()):
                return _observed_denial(
                    event, "INVALID_TASK_LABEL",
                    "Use a non-empty `label` of at most 80 characters in the user's language.",
                    state_root=state_root,
                )
            task_label = value.strip()
        elif not reply_call:
            return _observed_denial(
                event, "MISSING_TASK_LABEL",
                "Fix: add `label` with a short human description in the user's language (1–80 characters).",
                state_root=state_root,
            )
        if is_start:
            input_hash = hashlib.sha256(json_transport.dumps(
                {key: value for key, value in tool_input.items() if key != "call"},
                sort_keys=True, default=str,
            ).encode("utf-8")).hexdigest()
            try:
                previous = _read_pending(_state_root(state_root), event)
            except WiringError as error:
                if "missing" not in str(error):
                    return _deny("ASYNC_PENDING_INVALID", str(error))
            else:
                if (previous.get("async_ready") is True
                        and previous.get("async_input_hash") == input_hash
                        and isinstance(previous.get("async_updated_input"), dict)):
                    return {"hookSpecificOutput": {"hookEventName": "PreToolUse",
                                                   "updatedInput": previous["async_updated_input"]}}
                return _deny("ASYNC_CALL_CONFLICT", "This tool use already has a pending call.")
            call_uuid = str(uuid.uuid5(uuid.NAMESPACE_OID, _call_key(event)))
            prior_job = hashlib.sha256(call_uuid.encode("utf-8")).hexdigest()
            if (_state_root(state_root) / "codex_exec_mcp" / "jobs" / f"{prior_job}.json").exists():
                return _deny("ASYNC_CALL_ALREADY_ACCEPTED", "This tool use already has a bridge job.")
        parsed = policy.parse_orch_prompt(tool_input.get("prompt"), loaded_config)
        reply_without_orch = (
            reply_call
            and parsed.code == policy.MISSING_ORCH_LINE
            and isinstance(tool_input.get("prompt"), str)
        )
        if not parsed.valid and not reply_without_orch:
            if parsed.code == policy.CONFIG_INVALID:
                raise WiringError(parsed.message)
            return _observed_denial(
                event, parsed.code, parsed.message, state_root=state_root, task_id=parsed.task_id
            )

        if (loaded_config.get("coder_agent", "codex") == "codex" and parsed.valid and new_call):
            try:
                quota = codex_quota.read_latest_quota()
            except Exception:
                quota = None
            fallback = loaded_config.get("fallback")
            repository_for_fallback, _ = _repo_root(event, repo_root)
            active = (active_coder_state.read(repository_for_fallback, _state_root(state_root))
                      if repository_for_fallback is not None else None)
            evidence = active.get("triggering_evidence") if isinstance(active, Mapping) else None
            consent = evidence if isinstance(evidence, Mapping) and "question_session" in evidence else None
            if isinstance(fallback, Mapping) and active is not None and active["active_coder"] == "primary":
                if parsed.reason == "user_override":
                    if (consent is None or consent.get("question_session") != event.get("session_id")
                            or consent.get("question_shown") is not True
                            or consent.get("answerable") is not True):
                        return _deny("FALLBACK_CONSENT_REQUIRED",
                                     "reason=user_override requires a shown fallback question and a later user prompt.")
                    if not active_coder_state.approve_fallback(
                            repository_for_fallback, event["session_id"], _state_root(state_root)):
                        return _deny("FALLBACK_CONSENT_REQUIRED", "The fallback approval is no longer current.")
                elif consent is not None and consent.get("question_session") == event.get("session_id") and consent.get("answerable") is True:
                    declined_evidence = {
                        "declined": True, "trigger_id": consent.get("trigger_id"),
                        "type": consent.get("type"),
                    }
                    active_coder_state.update(
                        repository_for_fallback,
                        lambda value: value.update(triggering_evidence=declined_evidence),
                        _state_root(state_root),
                    )
                elif consent is not None:
                    return _deny("FALLBACK_CONSENT_REQUIRED",
                                 "Wait for the fallback question to be shown and answered before dispatch.")
                else:
                    trigger = None
                    five_hour_used = quota.get("5h_used_pct") if isinstance(quota, Mapping) else None
                    if (parsed.phase == "implementation"
                            and isinstance(five_hour_used, (int, float)) and not isinstance(five_hour_used, bool)
                            and five_hour_used >= 95 and isinstance(quota.get("5h_resets_at"), str)):
                        trigger = {"type": "QUOTA", "trigger_id": quota["5h_resets_at"]}
                    if trigger is None:
                        trigger = _last_launch_failure(
                            _state_root(state_root), project_root.project_key(repository_for_fallback)
                        )
                    declined = (evidence.get("declined") is True if isinstance(evidence, Mapping) else False)
                    if (parsed.phase == "implementation" and trigger is not None
                            and consent is None and not (declined and evidence.get("trigger_id") == trigger.get("trigger_id"))
                            and event.get("session_id")):
                        kind, question = _fallback_question(trigger, fallback, quota)
                        recorded = active_coder_state.record_fallback_question(
                            repository_for_fallback,
                            {**trigger, "kind": kind, "question": question},
                            event["session_id"], _state_root(state_root)
                        )
                        if recorded:
                            compaction_advisor.queue_fallback_question(
                                event, repository_for_fallback, question, fallback["model"]
                            )
                        return _deny("FALLBACK_CONSENT_REQUIRED", "A fallback question must be shown before another implementation dispatch.")
            low_windows = []
            five_hour_reset = quota.get("5h_resets_at") if isinstance(quota, Mapping) else None
            if isinstance(quota, Mapping) and (active is None or active.get("active_coder") == "primary") and parsed.reason != "user_override":
                for label in ("5h", "7d"):
                    used = quota.get(f"{label}_used_pct")
                    reset = quota.get(f"{label}_resets_at")
                    if (isinstance(used, (int, float)) and not isinstance(used, bool)
                            and used >= LOW_QUOTA_USED_PCT and isinstance(reset, str)):
                        low_windows.append(f"{label}: {used:g}% used, resets at {reset}")
            if low_windows:
                return _deny(
                    "LOW_CODEX_QUOTA",
                    "Codex quota is nearly exhausted (" + "; ".join(low_windows) + "). "
                    "Tell the user before this task starts; the user may switch the coding agent "
                    "or wait. If the user chooses to proceed with Codex, resend the same call "
                    "with reason=user_override in the ORCH line."
                    + (f" If the user chooses to wait, run a background sleep command until "
                       f"the 5h reset at {five_hour_reset} UTC, then resend the call when it "
                       "exits; waiting costs nothing." if isinstance(five_hour_reset, str) else ""),
                )

        runtime_root = _state_root(state_root)
        try:
            _cleanup_stale_pending(runtime_root, event)
        except BaseException:
            pass
        _call_key(event)
        indexed_root = None
        if reply_call:
            indexed_root, index_error = _indexed_reply_root(runtime_root, tool_input.get("threadId"))
            if index_error:
                message = (
                    "codex-reply threadId must be a simple identifier without path separators."
                    if index_error == "INVALID_THREAD_ID" else
                    "The recorded project for this codex-reply thread is no longer a git work tree."
                )
                return _observed_denial(event, index_error, message, state_root=state_root)
        repository, project_root_reason = (
            (indexed_root, "") if indexed_root is not None else _repo_root(event, repo_root)
        )
        resolved_profile = (
            project_profile.load_profile(repository) if repository is not None else None
        )
        project_namespace = project_root.project_key(repository)
        session_ledger = ledger.SessionLedger(
            _project_state_root(runtime_root, project_namespace)
        )
        record: Mapping[str, Any] | None
        live_thread: Mapping[str, Any] | None
        if reply_call:
            forbidden = {"model", "config", "developer-instructions"}.intersection(tool_input)
            if is_start:
                forbidden |= set(tool_input) - {
                    "prompt", "threadId", "mode", "call", "timeoutSeconds", "label"
                }
            if forbidden:
                caller = "codex_start mode=reply" if is_start else "codex-reply"
                allowed = "threadId, prompt and mode" if is_start else "threadId and prompt"
                fields = ", ".join(sorted(forbidden))
                return _observed_denial(
                    event,
                    "CODEX_REPLY_INPUT_INVALID",
                    f"{caller} accepts only {allowed}; field {fields} is not allowed on reply.",
                    state_root=state_root,
                    task_id=parsed.task_id,
                )
        if reply_without_orch:
            caller = "codex_start mode=reply" if is_start else "codex-reply"
            supplied_thread_id = tool_input.get("threadId")
            recorded = session_ledger.find_by_thread_id(supplied_thread_id)
            if recorded is None:
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    f"No recoverable ledger record exists for this {caller} thread.",
                    state_root=state_root,
                )
            candidate_thread = recorded.get("live_thread")
            if not isinstance(candidate_thread, Mapping):
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    f"The {caller} thread record has no recoverable live thread context.",
                    state_root=state_root,
                )
            inherited = _inherited_reply_parse(tool_input.get("prompt"), candidate_thread)
            if inherited is None:
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    f"The {caller} thread record lacks recoverable phase or scope context.",
                    state_root=state_root,
                )
            parsed = inherited
            record = recorded
            live_thread = candidate_thread
        else:
            if not parsed.valid:
                if parsed.code == policy.CONFIG_INVALID:
                    raise WiringError(parsed.message)
                return _observed_denial(
                    event, parsed.code, parsed.message, state_root=state_root, task_id=parsed.task_id
                )
            if reply_call:
                recorded = session_ledger.find_by_thread_id(tool_input.get("threadId"))
                if recorded is not None:
                    recorded_thread = recorded.get("live_thread")
                    recorded_task = recorded.get("task_id")
                    mismatches = []
                    if recorded_task != parsed.task_id:
                        mismatches.append(
                            ("THREAD_TASK_MISMATCH", "task", recorded_task, parsed.task_id)
                        )
                    if isinstance(recorded_thread, Mapping):
                        for field, code in (("phase", "THREAD_PHASE_MISMATCH"),
                                            ("route", "THREAD_ROUTE_MISMATCH")):
                            old_value = recorded_thread.get(field)
                            new_value = getattr(parsed, field)
                            if old_value != new_value:
                                mismatches.append((code, field, old_value, new_value))
                    if mismatches:
                        code, field, recorded_value, requested_value = mismatches[0]
                        caller = "codex_start mode=reply" if is_start else "codex-reply"
                        if field == "task":
                            message = (
                                f"task id differs from thread {tool_input.get('threadId')}'s task "
                                f"{recorded_value}; this {caller} requests {requested_value}. "
                                "Reuse that task or start a fresh thread."
                            )
                        else:
                            message = (
                                f"thread {tool_input.get('threadId')} was recorded for {field} "
                                f"{recorded_value}; this {caller} says {requested_value}. "
                                f"Reuse that {field} or start a fresh thread."
                            )
                        return _observed_denial(
                            event,
                            code,
                            message,
                            state_root=state_root,
                            task_id=parsed.task_id,
                            continuity=True,
                        )
            loaded = session_ledger.load(parsed.task_id)
            if loaded["status"] == ledger.CORRUPT:
                raise WiringError("session ledger is unreadable")
            record = loaded.get("record") if loaded["status"] == ledger.OK else None
            live_thread = record.get("live_thread") if isinstance(record, Mapping) else None

        if task_label is None:
            inherited_label = live_thread.get("label") if isinstance(live_thread, Mapping) else None
            if isinstance(inherited_label, str) and inherited_label.strip() and len(inherited_label.strip()) <= 80:
                task_label = inherited_label.strip()
            else:
                return _observed_denial(
                    event, "MISSING_TASK_LABEL",
                    "Fix: add `label` with a short human description in the user's language (1–80 characters); "
                    "this thread has no saved label to reuse.",
                    state_root=state_root, task_id=parsed.task_id,
                )

        route = policy.resolve_route(parsed.route, loaded_config)
        if not route.valid:
            if route.code == policy.CONFIG_INVALID:
                raise WiringError(route.message)
            return _observed_denial(
                event, route.code, route.message, state_root=state_root, task_id=parsed.task_id
            )
        requested = _requested_thread(parsed, route)
        agent = loaded_config.get("coder_agent", "codex")
        model = route.model
        effort = route.effort
        mechanical_candidate = False
        active_coder = active_coder_state.read(repository, runtime_root) if repository is not None else None
        fallback_active = bool(new_call and isinstance(active_coder, Mapping)
                               and active_coder.get("active_coder") == "fallback"
                               and isinstance(loaded_config.get("fallback"), Mapping))
        if reply_call and isinstance(live_thread, Mapping):
            thread_agent = live_thread.get("agent", "codex")
            fallback = loaded_config.get("fallback")
            allowed_agents = {agent}
            if isinstance(fallback, Mapping) and isinstance(fallback.get("agent"), str):
                allowed_agents.add(fallback["agent"])
            mechanical_candidate_thread = (
                thread_agent == "opencode"
                and live_thread.get("route") == "mechanical"
                and live_thread.get("phase") == "implementation"
                and loaded_config.get("mechanical_opencode_enabled", True)
            )
            if thread_agent not in allowed_agents and not mechanical_candidate_thread:
                return _observed_denial(event, "THREAD_AGENT_MISMATCH",
                                        "thread belongs to a different coding agent; start a fresh thread.",
                                        state_root=state_root, task_id=parsed.task_id, continuity=True)
        if reply_call and isinstance(live_thread, Mapping):
            agent = live_thread.get("agent", "codex")
            stored_model = live_thread.get("model")
            if isinstance(stored_model, str):
                requested["model"] = model = stored_model
            stored_effort = live_thread.get("effort")
            if isinstance(stored_effort, str):
                requested["effort"] = effort = stored_effort
        elif new_call and parsed.route == "mechanical":
            candidates = (coder_config.opencode_candidates()
                         if loaded_config.get("mechanical_opencode_enabled", True) else [])
            if candidates:
                candidate = candidates[0]
                if not isinstance(candidate, str) or not coder_config.MODEL.fullmatch(candidate):
                    raise WiringError("mechanical OpenCode candidate model is invalid")
                agent, model = "opencode", candidate
                mechanical_candidate = True
        if fallback_active:
            fallback = loaded_config["fallback"]
            agent, model = fallback["agent"], fallback["model"]
            effort = "medium" if agent == "agy" else "none" if agent == "opencode" else effort
            requested["model"] = model
            requested["effort"] = effort
            mechanical_candidate = False
        fallback_serving = (isinstance(loaded_config.get("fallback"), Mapping)
                            and agent == loaded_config["fallback"]["agent"]
                            and model == loaded_config["fallback"]["model"])
        if new_call and agent in ("opencode", "agy") and parsed.phase == "plan":
            return _observed_denial(event, "OPENCODE_PLAN_UNISOLATED" if agent == "opencode" else "AGY_PLAN_UNISOLATED",
                                    "OpenCode has no verified read-only sandbox for plan calls." if agent == "opencode"
                                    else "Antigravity has no verified read-only sandbox for plan calls.",
                                    state_root=state_root, task_id=parsed.task_id)
        effective_reason = parsed.reason
        reason_source = "explicit" if parsed.reason is not None and not reply_without_orch else None
        if reply_without_orch and parsed.reason is not None:
            reason_source = "inherited"
        if (
            reply_call
            and effective_reason is None
            and isinstance(live_thread, Mapping)
            and isinstance(live_thread.get("reason"), str)
        ):
            effective_reason = live_thread["reason"]
            reason_source = "inherited"
        previous_route = live_thread.get("route") if isinstance(live_thread, Mapping) else None
        route_changed = isinstance(previous_route, str) and previous_route != parsed.route
        bypass_metadata.update(
            task_id=parsed.task_id,
            phase=parsed.phase,
            route=parsed.route,
            model=model,
            effort=effort,
            scope=list(parsed.scope),
            reason=effective_reason,
            eta_minutes=parsed.eta_minutes,
        )

        if new_call:
            decision = policy.fresh_codex_decision(requested, live_thread, parsed.reason)
            if not decision.allowed:
                return _observed_denial(
                    event,
                    decision.code,
                    decision.message,
                    state_root=state_root,
                    task_id=parsed.task_id,
                    continuity=True,
                )
            continuation_thread_id = None
        else:
            compatibility = policy.thread_compatibility(requested, live_thread)
            if not compatibility.compatible:
                caller = "codex_start mode=reply" if is_start else "codex-reply"
                message = compatibility.message
                if compatibility.code == "NO_LIVE_THREAD":
                    message = f"{message} This {caller} cannot continue without a compatible recorded thread."
                return _observed_denial(
                    event,
                    compatibility.code,
                    message,
                    state_root=state_root,
                    task_id=parsed.task_id,
                    continuity=True,
                )
            supplied_thread_id = tool_input.get("threadId")
            if supplied_thread_id != live_thread.get("thread_id"):
                return _observed_denial(
                    event,
                    "THREAD_ID_MISMATCH",
                    "codex-reply names a different thread; a fresh thread is required.",
                    state_root=state_root,
                    task_id=parsed.task_id,
                    continuity=True,
                )
            continuation_thread_id = live_thread["thread_id"]
        bypass_metadata["prior_thread_id"] = continuation_thread_id

        updated = dict(tool_input)
        updated.pop("label", None)
        updated["prompt"] = parsed.prompt
        if mechanical_candidate:
            updated.update(mechanicalFallback=True, fallbackAgent=loaded_config.get("coder_agent", "codex"),
                           fallbackModel=route.model, fallbackEffort=route.effort)
        if agent != loaded_config.get("coder_agent", "codex") or mechanical_candidate:
            updated["agent"] = agent
        if parsed.phase == "plan" and new_call and agent == "codex":
            updated["sandbox"] = "read-only"
            updated["approval-policy"] = "never"
        if parsed.phase == "implementation":
            updated["prompt"] = _append_worker_validation_instruction(
                updated["prompt"],
                full_suite_required=(
                    resolved_profile.full_suite_required
                    if resolved_profile is not None else False
                ),
            )
        worktree_start = is_start and mode == "new" and parsed.phase == "implementation"
        worktree_reply_path = (_thread_worktree_path(tool_input.get("threadId"))
                               if is_start and mode == "reply" and parsed.phase == "implementation"
                               else None)
        worktree_reply = worktree_reply_path is not None
        if worktree_start:
            updated["worktree"] = True
            if repository is not None:
                requested_cwd = tool_input.get("cwd", event.get("cwd"))
                try:
                    launch_cwd = Path(requested_cwd).resolve() if isinstance(requested_cwd, str) else repository
                    relative_cwd = launch_cwd.relative_to(repository)
                except (OSError, RuntimeError, ValueError):
                    relative_cwd = Path()
                updated["cwd"] = str(repository / relative_cwd)
        if new_call:
            updated["model"] = model
            original_codex_config = tool_input.get("config", {})
            if not isinstance(original_codex_config, Mapping):
                raise WiringError("tool_input.config must be an object when supplied")
            updated_config = dict(original_codex_config)
            if effort != "none" and agent != "opencode":
                updated_config["model_reasoning_effort"] = effort
            output_limit = loaded_config.get("tool_output_token_limit") if agent == "codex" else None
            if output_limit is not None:
                if isinstance(output_limit, bool) or not isinstance(output_limit, int) or output_limit <= 0:
                    raise WiringError("tool_output_token_limit must be a positive integer")
                updated_config["tool_output_token_limit"] = output_limit
            if parsed.phase == "plan":
                for unsafe_key in ("sandbox_mode", "approval_policy", "sandbox_workspace_write"):
                    updated_config.pop(unsafe_key, None)
            updated["config"] = updated_config
            if agent == "codex" and "developer-instructions" not in tool_input:
                updated["developer-instructions"] = INDEPENDENCE_INSTRUCTIONS
        eta_minutes = tool_input.get("etaMinutes", parsed.eta_minutes) if is_start else parsed.eta_minutes
        if is_start:
            eta_minutes = _route_eta_minutes({
                "eta_minutes": eta_minutes, "route": parsed.route,
                "project_key": project_namespace, "agent": agent,
            }, runtime_root)
            updated["mode"] = mode
            updated["call"] = call_uuid
            updated["taskId"] = parsed.task_id
            updated["route"] = parsed.route
            updated["jobModel"] = model
            updated["jobEffort"] = effort
            updated["jobLabel"] = task_label
            updated["jobPhase"] = parsed.phase
            if eta_minutes is not None:
                updated["etaMinutes"] = eta_minutes

        protected_paths = (
            resolved_profile.protected_paths
            if resolved_profile is not None
            else project_profile._ENGINE_PROTECTED
        )
        bypass_metadata["repo_root"] = str(repository) if repository is not None else None
        bypass_metadata["project_key"] = project_namespace
        bypass_metadata["project_root_reason"] = project_root_reason or None
        declared_scope = list(parsed.scope)
        if repository is None:
            baseline = {
                "status": ledger.UNKNOWN,
                "code": "PROJECT_ROOT_UNRESOLVED",
                "message": project_root_reason,
                "baseline_head": None,
            }
        else:
            baseline_repository = Path(worktree_reply_path).resolve() if worktree_reply_path else repository
            baseline = ledger.capture_baseline(
                baseline_repository,
                None if parsed.phase == "plan" else (declared_scope if declared_scope else None),
                manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
            )
        if (
            reply_call
            and baseline.get("status") == ledger.OK
            and _handoff_matches(baseline, live_thread, declared_scope)
        ):
            baseline = live_thread["origin_baseline"]
        prompt_text = str(tool_input.get("prompt", ""))
        task_objective, acceptance_criteria, task_contract_grounded = _parse_task_contract(
            prompt_text, parsed.task_id, parsed.phase, declared_scope
        )
        if not task_contract_grounded and live_thread and isinstance(live_thread, Mapping):
            prior_obj = live_thread.get("task_objective")
            prior_crit = live_thread.get("acceptance_criteria")
            if prior_obj and prior_crit:
                task_objective = prior_obj
                acceptance_criteria = prior_crit
                task_contract_grounded = bool(live_thread.get("task_contract_grounded", True))

        pending = {
            "kind": "call",
            "task_id": parsed.task_id,
            "project_key": project_namespace,
            "phase": parsed.phase,
            "route": parsed.route,
            "model": model,
            "effort": effort,
            "label": task_label,
            "agent": agent,
            "fallback_family": (loaded_config["fallback"]["family"] if fallback_serving else None),
            "mechanical_opencode": (
                parsed.route == "mechanical"
                and parsed.phase == "implementation"
                and loaded_config.get("mechanical_opencode_enabled", True)
            ),
            "shadow_prompt": updated["prompt"] if (
                parsed.route == "mechanical"
                and parsed.phase == "implementation"
                and loaded_config.get("mechanical_opencode_enabled", True)
            ) else None,
            "scope": declared_scope,
            "repo_root": str(repository) if repository is not None else None,
            "project_root_reason": project_root_reason or None,
            "baseline": baseline,
            "prior_thread_id": continuation_thread_id,
            "task_objective": task_objective,
            "acceptance_criteria": acceptance_criteria,
            "task_contract_grounded": task_contract_grounded,
            "has_code_map": bool(re.search(
                r"(?m)^CODE_MAP=.+ STATUS=(?:FRESH|STALE|UNAVAILABLE)\r?$", updated["prompt"]
            )),
            "reason": effective_reason,
            "eta_minutes": eta_minutes,
            "reason_source": reason_source,
            "previous_route": previous_route if route_changed else None,
            "new_route": parsed.route if route_changed else None,
        }
        if worktree_reply_path:
            pending["worktree_path"] = worktree_reply_path
        if is_start:
            pending.update(async_call=updated["call"], async_mode=mode,
                           async_input_hash=input_hash, async_updated_input=updated,
                           async_ready=False, job_id=None,
                           session_id=event.get("session_id"),
                           tool_use_id=event.get("tool_use_id"))
        if resolved_profile is not None:
            inherited_checks = (
                live_thread.get("profile_checks")
                if reply_call and isinstance(live_thread, Mapping)
                else None
            )
            pending.update(
                {
                    "profile_checks": (
                        inherited_checks if isinstance(inherited_checks, list)
                        else _check_snapshot(resolved_profile.checks)
                    ),
                    "protected_paths": list(resolved_profile.protected_paths),
                    "force_full_review": resolved_profile.force_full_review,
                    "skip_max_lines": resolved_profile.skip_max_lines,
                    "skip_max_files": resolved_profile.skip_max_files,
                    "reviewer_executable": resolved_profile.reviewer_executable,
                    "reviewer": resolved_profile.roles.reviewer,
                    "reviewer_candidates": [
                        {"agent": candidate.agent, "model": candidate.model}
                        for candidate in resolved_profile.reviewer_candidates
                    ],
                    "authority_sources": [
                        {
                            "path": source.path,
                            "headings": list(source.headings),
                            "when_changed": source.when_changed,
                        }
                        for source in resolved_profile.authority_sources
                    ],
                    "vault_root": resolved_profile.vault_root,
                    "configured": resolved_profile.configured,
                    "user_status": resolved_profile.user_status,
                    "project_status": resolved_profile.project_status,
                    "first_warning": (
                        resolved_profile.warnings[0]
                        if resolved_profile.warnings
                        else None
                    ),
                }
            )
        # Unresolved roots keep the existing UNKNOWN/FULL path; there is no
        # repository identity against which to key a scope lease.
        if parsed.phase in {"implementation", "plan"} and repository is not None:
            _reconcile_rejected_starts(runtime_root, project_namespace)
            owner = _call_key(event)
            job_id = hashlib.sha256(updated["call"].encode("utf-8")).hexdigest() if is_start else None
            admitted = ledger.ScopeLeases(_project_state_root(runtime_root, project_namespace)).acquire(
                owner, parsed.task_id, declared_scope if parsed.phase == "implementation" else [],
                job_id=job_id, read_only=parsed.phase == "plan",
                timeout_seconds=updated.get("timeoutSeconds") if is_start else None,
                allow_overlap=worktree_start or worktree_reply,
            )
            if admitted["status"] != ledger.OK:
                return _observed_denial(
                    event, admitted["code"],
                    (f"Scope is leased by task {admitted['task_id']}. "
                     "To release a stuck lease, run: python "
                     ".claude/hooks/orchestrator/purser_leases.py --release <task-id>"
                     if admitted["code"] == "SCOPE_LEASE_CONFLICT" else
                     "Scope ownership could not be established."),
                    state_root=runtime_root, task_id=parsed.task_id,
                )
            pending["lease_owner"] = owner
            lease_pending = pending
            if admitted["code"] == "LEASE_QUEUED":
                pending["lease_queued"] = True
                pending["baseline"] = {"status": ledger.UNKNOWN, "code": "QUEUED"}
                baseline = pending["baseline"]
                updated["leaseQueued"] = True
        _atomic_json_write(_pending_path(runtime_root, event), pending)
        pending_written = True

        session_record = ledger.build_session_record(
            parsed.task_id,
            pending["prior_thread_id"],
            parsed.route,
            model,
            effort,
            agent=agent,
            phase=parsed.phase,
            scope=declared_scope,
            pending_call_baseline=baseline,
            last_outcome=record.get("last_outcome") if isinstance(record, Mapping) else None,
            task_objective=task_objective,
            acceptance_criteria=acceptance_criteria,
            task_contract_grounded=task_contract_grounded,
            profile_checks=pending.get("profile_checks"),
            reason=effective_reason,
            label=task_label,
        )
        saved = session_ledger.save(session_record)
        if saved["status"] != ledger.OK:
            raise WiringError(saved.get("message", "session ledger could not be updated"))
        if continuation_thread_id is not None:
            _record_thread_root(runtime_root, continuation_thread_id, repository)
        observation = {
            "task_id": parsed.task_id,
            "state_root": runtime_root,
            "project_key": project_namespace,
            "phase": parsed.phase,
            "route": parsed.route,
            "model": model,
            "effort": effort,
            "routing_enforced": True,
            "fresh_thread": new_call,
            "reply_call": reply_call,
            "reason": effective_reason,
            "eta_minutes": parsed.eta_minutes,
            "reason_source": reason_source,
            "previous_route": previous_route if route_changed else None,
            "new_route": parsed.route if route_changed else None,
            **_telemetry_envelope(event, repository, project_namespace),
        }
        _observe("emit", "TASK_ROUTE_SELECTED", **observation)
        _observe("emit", "IMPLEMENTER_STARTED", **observation)
        if is_start:
            pending["async_ready"] = True
            _atomic_json_write(_pending_path(runtime_root, event), pending)

        try:
            import claude_transcript_usage
            claude_transcript_usage.start_active_task(
                runtime_root,
                session_id=event.get("session_id"),
                task_id=parsed.task_id,
                project_key=project_namespace,
                tool_use_id=event.get("tool_use_id"),
                transcript_path=event.get("transcript_path"),
            )
        except ImportError:
            _observe("record_health", "DEGRADED", error_code="CLAUDE_TRANSCRIPT_PARSER_UNAVAILABLE", state_root=runtime_root)
        except Exception:
            _observe("record_health", "DEGRADED", error_code="CLAUDE_TRANSCRIPT_PARSER_FAILED", state_root=runtime_root)

        try:
            import context_io_telemetry
            context_io_telemetry.associate_turn(
                runtime_root,
                session_id=event.get("session_id"),
                task_id=parsed.task_id,
                project_key=project_namespace,
            )
        except BaseException:
            pass

        result = {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "updatedInput": updated,
            }
        }
        try:
            route_name = (
                parsed.route
                if isinstance(parsed.route, str) and parsed.route
                else "unknown"
            )
            model_value = model
            model_name = model_value if isinstance(model_value, str) and model_value else "unknown"
            effort_value = effort
            effort_name = (
                effort_value
                if isinstance(effort_value, str) and effort_value
                else "unknown"
            )
            result["systemMessage"] = (
                f"Purser: route={route_name} -> {model_name} / {effort_name}"
            )
        except Exception:
            pass
        return result
    except Exception as error:
        if lease_pending is not None and runtime_root is not None:
            try:
                _release_scope_lease(runtime_root, lease_pending)
            except Exception:
                pass
        if pending_written and runtime_root is not None:
            try:
                _remove_pending(runtime_root, event)
            except Exception:
                pass
        if event.get("tool_name") in CODEX_START_TOOL_NAMES:
            _record_diagnostic(runtime_root, event, "PreToolUse", error)
            return _deny("ASYNC_ENFORCEMENT_FAILED", "Async start lacked a durable route/baseline; FULL review is required.")
        recorded = False
        reason = _safe_error(error)
        try:
            runtime_root = runtime_root or _state_root(state_root)
            _write_pre_bypass(runtime_root, event, reason, bypass_metadata)
            recorded = True
        except Exception as persistence_error:
            reason = f"{reason}; bypass persistence failed: {_safe_error(persistence_error)}"
        _record_diagnostic(runtime_root, event, "PreToolUse", error)
        return _internal_pre_allow(reason, recorded)


def _decoded_codex_objects(tool_response: Any) -> list[dict[str, Any]]:
    objects: list[dict[str, Any]] = []
    if isinstance(tool_response, Mapping):
        structured = tool_response.get("structuredContent")
        if isinstance(structured, dict):
            objects.append(structured)
        payload = tool_response.get("content")
    else:
        payload = tool_response
    if isinstance(payload, str):
        texts = [payload]
    elif isinstance(payload, list):
        texts = [
            block["text"]
            for block in payload
            if isinstance(block, Mapping)
            and block.get("type") == "text"
            and isinstance(block.get("text"), str)
        ]
    else:
        texts = []
    for text in texts:
        try:
            decoded = json.loads(text)
        except json.JSONDecodeError:
            continue
        if isinstance(decoded, dict):
            objects.append(decoded)
    return objects


def _codex_response_is_error(tool_response: Any) -> bool:
    return ((isinstance(tool_response, Mapping) and bool(tool_response.get("isError")))
            or any(bool(value.get("isError")) for value in _decoded_codex_objects(tool_response)))


def _extract_codex_result(tool_response: Any) -> tuple[str, str]:
    for decoded in _decoded_codex_objects(tool_response):
        thread_id = decoded.get("threadId")
        content = decoded.get("content")
        if isinstance(thread_id, str) and thread_id:
            return thread_id, content if isinstance(content, str) else ""
    raise WiringError("thread id could not be recovered from Codex MCP text content")


def _start_value(tool_response: Any) -> dict[str, Any]:
    values = _decoded_codex_objects(tool_response)
    if values:
        return values[0]
    raise WiringError("codex_start result is unreadable")


def _cancel_start_job(root: Path, call_uuid: str, job_id: str) -> bool:
    """Ask the owning bridge to cancel via its authenticated local signal socket."""
    if not isinstance(call_uuid, str) or not isinstance(job_id, str):
        return False
    if hashlib.sha256(call_uuid.encode("utf-8")).hexdigest() != job_id:
        return False
    try:
        path = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
        job = json.loads(path.read_text(encoding="utf-8"))
        if job.get("jobId") != job_id or job.get("call") != call_uuid:
            return False
        with socket.create_connection(("127.0.0.1", job["signalPort"]), timeout=3) as connection:
            connection.sendall(f"{job_id}:{job['signalToken']}:CANCEL".encode("ascii"))
            return connection.recv(1) == b"1"
    except (OSError, ValueError, KeyError, TypeError, UnicodeError):
        return False


def _start_job_terminal_or_absent(root: Path, pending: Mapping[str, Any]) -> bool:
    call_uuid = pending.get("async_call")
    if not isinstance(call_uuid, str):
        return False
    job_id = hashlib.sha256(call_uuid.encode("utf-8")).hexdigest()
    path = root / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    try:
        job = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return True  # The durable bridge contract has no accepted job.
    except (OSError, ValueError, TypeError):
        return False
    return (isinstance(job, Mapping) and job.get("jobId") == job_id
            and job.get("call") == call_uuid
            and job.get("state") in {"SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT"})


def _job_host_classification(job: Mapping[str, Any]) -> str:
    return codex_exec_mcp.Server._classify_host(None, job)


_ROUTE_DURATION_DEFAULTS = {
    "mechanical": 3,
    "subtle": 5,
    "normal": 5,
    "consequential": 8,
    "escalated": 12,
    "operation": 3,
    "plan_standard": 8,
}


def _route_duration_median(pending: Mapping[str, Any], state_root: Path) -> int | None:
    try:
        import async_session_maintenance

        implementer = {"codex": "codex-main", "opencode": "opencode-main",
                       "agy": "agy-main"}.get(pending.get("agent"), "codex-main")
        hint = async_session_maintenance.duration_hint(
            state_root, str(pending.get("project_key", "")), implementer=implementer
        )
        route = pending.get("route")
        if isinstance(hint, str) and isinstance(route, str):
            match = re.search(rf"(?:^|: |, )({re.escape(route)}) (\d+)/(\d+) min \(n=\d+\)", hint)
            if match:
                return int(match.group(2))
    except BaseException:
        pass
    return None


def _route_eta_minutes(pending: Mapping[str, Any], state_root: Path) -> int | None:
    eta = pending.get("eta_minutes")
    if isinstance(eta, int) and not isinstance(eta, bool):
        return eta
    median = _route_duration_median(pending, state_root)
    if median is not None:
        return median
    route = pending.get("route")
    return _ROUTE_DURATION_DEFAULTS.get(route) if isinstance(route, str) else None


def _project_job_board_details(root: Path, project_key: str) -> tuple[str | None, list[str]]:
    """Render the current project job board and identify its displayed jobs."""
    try:
        jobs = []
        now = time.time()
        for path in (root / "async_jobs").glob("*.json"):
            try:
                index = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if not isinstance(index, dict) or index.get("project_key") != project_key:
                continue
            job_id = index.get("jobId")
            if not isinstance(job_id, str) or not re.fullmatch(r"[0-9a-f]{64}", job_id):
                continue
            try:
                job = json.loads((root / "codex_exec_mcp" / "jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if (not isinstance(job, dict) or job.get("jobId") != job_id
                    or job.get("call") != index.get("call")):
                continue
            jobs.append((job, index))

        newest_by_thread: dict[str, tuple[dict[str, Any], dict[str, Any]]] = {}
        for item in jobs:
            job, _index = item
            thread_id = job.get("threadId")
            key = thread_id if isinstance(thread_id, str) and thread_id else job["jobId"]
            previous = newest_by_thread.get(key)
            accepted_at = job.get("acceptedAt")
            previous_accepted_at = previous[0].get("acceptedAt") if previous else None
            accepted_stamp = (accepted_at if isinstance(accepted_at, (int, float))
                              and not isinstance(accepted_at, bool) else 0)
            previous_stamp = (previous_accepted_at if isinstance(previous_accepted_at, (int, float))
                              and not isinstance(previous_accepted_at, bool) else 0)
            if previous is None or (accepted_stamp, job["jobId"]) > (previous_stamp, previous[0]["jobId"]):
                newest_by_thread[key] = item

        in_progress = []
        finished_jobs = []
        for job, index in newest_by_thread.values():
            label = job.get("label")
            if not isinstance(label, str) or not label.strip():
                continue
            state = job.get("state")
            finished = job.get("appliedAt") if job.get("applyState") == "applied" else None
            if not isinstance(finished, (int, float)):
                finished = job.get("finishedAt")
            if state not in {"QUEUED", "RUNNING"}:
                if "boardDisplayCount" not in job:
                    continue
                if type(job["boardDisplayCount"]) is int and job["boardDisplayCount"] >= 2:
                    continue
            item = (job, index)
            if state in {"QUEUED", "RUNNING"}:
                in_progress.append(item)
            else:
                finished_jobs.append((finished if isinstance(finished, (int, float)) else 0, item))
        jobs = in_progress + [item for _finished, item in sorted(
            finished_jobs, key=lambda entry: entry[0], reverse=True
        )]
        if len(jobs) < 2:
            return None, []

        def board_row(item: tuple[dict[str, Any], dict[str, Any]]) -> tuple[int, int, float, str]:
            job, index = item
            raw_state = job.get("state")
            state = {"QUEUED": "queued", "RUNNING": "running", "SUCCEEDED": "done",
                     "FAILED": "failed", "TIMED_OUT": "failed", "CANCELLED": "cancelled",
                     "UNKNOWN_OR_IN_PROGRESS": "running"}.get(raw_state, "failed")
            call_key = index.get("call_key")
            bundle = {}
            try:
                if isinstance(call_key, str):
                    bundle = json.loads((root / "bundle_artifacts" / f"{call_key}.json").read_text(encoding="utf-8"))
            except (OSError, ValueError):
                pass
            review = "—"
            if isinstance(bundle, dict):
                if bundle.get("reviewer1_result"):
                    review = "failed" if bundle.get("reviewer1_result") == policy.REVIEWER1_UNAVAILABLE else "reviewed"
                elif bundle.get("reviewer1_skipped_reason"):
                    review = "failed" if bundle.get("reviewer1_skipped_reason") == "REVIEWER1_UNAVAILABLE" else "skipped"
                elif bundle.get("claude_review_mode") in {policy.FULL, policy.AUDIT}:
                    review = "failed"
                elif raw_state in {"FAILED", "TIMED_OUT"}:
                    review = "failed"
            apply = "—"
            if job.get("applyState") in {"applied", "conflict"}:
                apply = job["applyState"]
            elif isinstance(bundle, dict) and bundle:
                if job.get("phase", bundle.get("phase")) in {"operation", "plan"} or bundle.get("apply") != "codex_apply":
                    apply = "n/a"
            elif job.get("phase") in {"operation", "plan"}:
                apply = "n/a"
            changed_paths = job.get("changed_paths")
            if not isinstance(changed_paths, list) and isinstance(bundle, dict):
                changed_paths = bundle.get("changed_paths")
            if (apply == "—" and raw_state not in {"QUEUED", "RUNNING", "UNKNOWN_OR_IN_PROGRESS"}
                    and changed_paths == []):
                apply = "no change"
            eta_label = "—"
            if state in {"queued", "running"}:
                started = job.get("acceptedAt")
                elapsed = max(0.0, (now - started) / 60) if isinstance(started, (int, float)) else None
                expected = job.get("etaMinutes")
                if isinstance(expected, bool) or not isinstance(expected, int):
                    expected = _route_eta_minutes({"route": job.get("route"), "project_key": project_key,
                                                   "agent": job.get("agent", "codex")}, root)
                if elapsed is not None and expected is not None:
                    eta_label = "overdue" if elapsed > expected else f"~{max(0, math.ceil(expected - elapsed))} min"
            label = str(job.get("label", "—")).replace("|", "\\|").replace("\r", " ").replace("\n", " ")
            model, effort = job.get("model"), job.get("effort")
            model_label = (f"{model} ({effort})".replace("|", "\\|")
                           if isinstance(model, str) and isinstance(effort, str) else "—")
            text = f"| {label} | {model_label} | {state} | {eta_label} | {review} | {apply} |"
            if raw_state in {"QUEUED", "RUNNING"}:
                stamp = job.get("acceptedAt")
                return (0, 0 if raw_state == "RUNNING" else 1,
                        -float(stamp) if isinstance(stamp, (int, float)) else 0.0, text)
            stamp = job.get("appliedAt") if job.get("applyState") == "applied" else None
            if not isinstance(stamp, (int, float)):
                stamp = job.get("finishedAt")
            return 1, 0, -float(stamp) if isinstance(stamp, (int, float)) else 0.0, text

        lines = ["| Task | Model | State | ETA | Review | Apply |", "| --- | --- | --- | --- | --- | --- |"]
        lines.extend(row[3] for row in sorted(map(board_row, jobs), key=lambda row: row[:3]))
        block = "Purser jobs:\n" + "\n".join(lines)
        return block, [job["jobId"] for job, _index in jobs]
    except (OSError, ValueError, TypeError, KeyError, AttributeError):
        pass  # Status is observational; it cannot disable enforcement.
    return None, []


def _project_job_board(root: Path, project_key: str) -> str | None:
    """Render the current project job board without polling any worker."""
    return _project_job_board_details(root, project_key)[0]


def _append_project_job_status(response: dict[str, Any], root: Path,
                               project_key: str, event: Mapping[str, Any] | None = None,
                               *, pending_only: bool = False) -> dict[str, Any]:
    board, board_job_ids = _project_job_board_details(root, project_key)
    if event is not None and (board is not None or pending_only):
        repository, _reason = project_root.resolve_project_root(event)
        if pending_only:
            compaction_advisor.refresh_dispatch_board(event, repository, board,
                                                      board_job_ids=board_job_ids)
        elif board is not None:
            compaction_advisor.queue_dispatch(event, repository, board=board,
                                              board_job_ids=board_job_ids)
    return response


def _append_dispatch_receipt(response: dict[str, Any], pending: Mapping[str, Any],
                             state_root: Path, event: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """Render one admitted-call receipt from the recorded route and local duration history."""
    label, model = pending.get("label"), pending.get("model")
    effort, route = pending.get("effort"), pending.get("route")
    if not all(isinstance(value, str) and value for value in (label, model, effort, route)):
        return response
    median = _route_eta_minutes(pending, state_root)
    prior_thread_id = pending.get("prior_thread_id")
    continuity = "Continuing session" if isinstance(prior_thread_id, str) and prior_thread_id else "New session"
    fields = [f"{continuity} · model: {model} ({effort}) · task: {label}"]
    if median is not None:
        fields.append(f"estimated completion: {median} min")
    receipt = f"**{' · '.join(fields)}**"
    repository = Path(pending["repo_root"]) if isinstance(pending.get("repo_root"), str) else None
    if (event is not None
            and event.get("tool_name") not in (CODEX_RESULT_TOOL_NAMES | CODEX_APPLY_TOOL_NAMES)):
        compaction_advisor.queue_dispatch(event, repository, line=receipt)
    if pending.get("async_call") and isinstance(pending.get("project_key"), str):
        _append_project_job_status(response, state_root, pending["project_key"], event)
    return response


def _post_start(event: Mapping[str, Any], config: Mapping[str, Any] | None,
                state_root: str | os.PathLike[str] | None) -> dict[str, Any]:
    root = _state_root(state_root)
    try:
        pending = _read_pending(root, event)
        call_uuid = pending.get("async_call")
        if pending.get("async_ready") is not True or not isinstance(call_uuid, str):
            raise WiringError("codex_start has no enforced pending call")
        value = _start_value(event.get("tool_response"))
        job_id = value.get("jobId")
        expected = hashlib.sha256(call_uuid.encode("utf-8")).hexdigest()
        if (not isinstance(job_id, str) or job_id != expected
                or value.get("state") not in {"QUEUED", "RUNNING", "SUCCEEDED"}
                or _codex_response_is_error(event.get("tool_response"))):
            raise WiringError(value.get("errorCode", "codex_start failed or returned a mismatched job"))
        if pending.get("job_id") not in (None, job_id):
            raise WiringError("pending call already belongs to another job")
        if pending.get("job_id") is None:
            pending["job_id"] = job_id
            _atomic_json_write(_pending_path(root, event), pending)
        _atomic_json_write(root / "async_jobs" / f"{job_id}.json", {
            "jobId": job_id, "call": call_uuid, "call_key": _call_key(event),
            "session_id": pending.get("session_id"), "project_key": pending.get("project_key"),
        })
        waiter = Path(__file__).resolve().parent / "purser_wait.py"
        command = (f'Run in the background: "{sys.executable}" "{waiter}" {job_id} '
                   "; when it finishes, call codex_result with this jobId.")
        response = _replacement({"jobId": job_id, "state": value["state"], "next": command},
                                _load_config(config))
        return _append_dispatch_receipt(response, pending, root, event)
    except Exception as error:
        failure = post_tool_use_failure({**event, "error": _safe_error(error)},
                                        state_root=state_root)
        output = failure.get("hookSpecificOutput", {})
        return {"hookSpecificOutput": {"hookEventName": "PostToolUse",
                "additionalContext": output.get("additionalContext", "Codex start failed; FULL review is required."),
                _load_config(config)["post_tool_output_field"]: [{"type": "text", "text":
                    json_transport.dumps({"state": "FAILED", "ambiguous_outcome": True,
                                          "claude_review_mode": policy.FULL})}]}}


def _async_owned(root: Path, event: Mapping[str, Any], repo_root: str | os.PathLike[str] | None
                 ) -> tuple[Path, dict[str, Any], dict[str, Any] | None, dict[str, Any] | None]:
    tool_input = event.get("tool_input")
    job_id = tool_input.get("jobId") if isinstance(tool_input, Mapping) else None
    if not isinstance(job_id, str) or not re.fullmatch(r"[0-9a-f]{64}", job_id):
        raise WiringError("unknown async job")
    path = root / "async_jobs" / f"{job_id}.json"
    try:
        index = json.loads(path.read_text(encoding="utf-8"))
        if (not isinstance(index, dict)
                or index.get("jobId") != job_id
                or not isinstance(index.get("project_key"), str) or not index["project_key"]
                or not re.fullmatch(r"[0-9a-f]{64}", str(index.get("call_key")))
                or not isinstance(index.get("session_id"), str) or not index["session_id"]
                or not isinstance(index.get("call"), str)
                or hashlib.sha256(index["call"].encode("utf-8")).hexdigest() != job_id):
            raise WiringError("job is not owned by this session/project")
        pending = None
        original = None
        if index.get("stage") != "CONSUMED":
            pending = json.loads((root / "pending" / f"{index['call_key']}.json").read_text(encoding="utf-8"))
            original = dict(event, tool_name=CODEX_TOOL, session_id=index["session_id"],
                            tool_use_id=pending["tool_use_id"])
            if (_call_key(original) != index["call_key"] or pending.get("job_id") != job_id
                    or pending.get("async_call") != index.get("call")
                    or pending.get("session_id") != index["session_id"]
                    or pending.get("project_key") != index["project_key"]
                    or not isinstance(pending.get("task_id"), str) or not pending["task_id"]
                    or not isinstance(pending.get("baseline"), dict)):
                raise WiringError("job and pending call do not match")
        if index["session_id"] != event.get("session_id"):
            repository, _ = _repo_root(event, repo_root)
            if repository is None or index["project_key"] != project_root.project_key(repository):
                raise WiringError("job is not owned by this session/project")
            if event.get("tool_name") not in CODEX_RESULT_TOOL_NAMES:
                raise WiringError("job is not owned by this session/project")
            if index.get("stage") != "CONSUMED" and _async_job(root, index) is None:
                raise WiringError("job and durable result do not match")
            if not isinstance(tool_input, Mapping) or tool_input.get("adopt") is not True:
                raise AsyncAdoptionRequired("user consent is required for adoption")
        return path, index, pending, original
    except (OSError, ValueError, KeyError, TypeError) as error:
        raise WiringError("unknown async job") from error


def _async_job(root: Path, index: Mapping[str, Any]) -> dict[str, Any] | None:
    try:
        job = json.loads((root / "codex_exec_mcp" / "jobs" / f"{index['jobId']}.json").read_text(encoding="utf-8"))
        return job if job.get("jobId") == index["jobId"] and job.get("call") == index["call"] else None
    except (OSError, ValueError, KeyError, TypeError, AttributeError):
        return None


def _async_pre_followup(event: Mapping[str, Any], state_root: str | os.PathLike[str] | None,
                        repo_root: str | os.PathLike[str] | None) -> dict[str, Any]:
    try:
        root = _state_root(state_root)
        _, index, _, _ = _async_owned(root, event, repo_root)
    except AsyncAdoptionRequired:
        return _deny("ASYNC_ADOPTION_REQUIRES_USER_CONSENT",
                     "Ask the user before adopting this earlier session's job; after agreement call codex_result with adopt: true.")
    except WiringError:
        return _deny("UNKNOWN_ASYNC_JOB", "No pending job belongs to this session/project.")
    if index.get("stage") == "CONSUMED":
        return {} if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES else _deny(
            "ASYNC_ALREADY_CONSUMED", "This job has already been finalized.")
    if index.get("stage") == "FINALIZING":
        return {} if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES else _deny(
            "ASYNC_FINALIZING", "This job is already being finalized.")
    if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES:
        job = _async_job(root, index)
        if job is not None and job.get("state") in {"QUEUED", "RUNNING"}:
            status = "still running" if job["state"] == "RUNNING" else "queued"
            return _deny("ASYNC_STILL_RUNNING", f"{status}; wait for the completion signal")
    return {}


def _async_duration(job: Mapping[str, Any] | None) -> int | None:
    if job is None:
        return None
    start, finish = job.get("acceptedAt"), job.get("finishedAt")
    if all(isinstance(value, (int, float)) and not isinstance(value, bool)
           and math.isfinite(value) for value in (start, finish)) and finish >= start:
        return int((finish - start) * 1000)
    return None


@contextmanager
def _async_claim(path: Path):
    """Hold an OS lock for one finalizer; process exit releases a crashed claim."""
    claim = path.with_suffix(".claim.lock")
    claim.parent.mkdir(parents=True, exist_ok=True)
    with claim.open("a+b") as handle:
        if os.name == "nt":
            import msvcrt
            handle.seek(0)
            if not handle.read(1):
                handle.write(b"0")
                handle.flush()
            handle.seek(0)
            msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
        else:
            import fcntl
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            yield
        finally:
            if os.name == "nt":
                handle.seek(0)
                msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def _async_ledger_rejection(root: Path, index: Mapping[str, Any], pending: Mapping[str, Any],
                            artifact: Path, *, success: bool) -> dict[str, str] | None:
    outcome = ledger.SessionLedger(_project_state_root(root, index["project_key"])).load(pending["task_id"])
    last = outcome.get("record", {}).get("last_outcome") if outcome.get("status") == ledger.OK else None
    field = "raw_output_path" if success else "evidence_path"
    expected = (root / "raw_tool_outputs" / f"{index['call_key']}.json").resolve() if success else artifact
    if isinstance(last, Mapping) and last.get(field) == str(expected):
        return None
    detail = "The session ledger did not retain this job's thread and outcome."
    if success and artifact.is_file():
        bundle = json.loads(artifact.read_text(encoding="utf-8"))
        for item in bundle.get("findings", []) if isinstance(bundle, Mapping) else []:
            if isinstance(item, Mapping) and str(item.get("code", "")).startswith("LEDGER_"):
                detail = f"{item.get('code')}: {item.get('message', detail)}"
                break
    return _finding("LEDGER_RECORD_REJECTED", detail)


def _async_rejected_response(root: Path, original: Mapping[str, Any], artifact: Path,
                             finding: Mapping[str, str], config: Mapping[str, Any],
                             *, success: bool) -> dict[str, Any]:
    if success:
        bundle = json.loads(artifact.read_text(encoding="utf-8"))
        if not isinstance(bundle, dict):
            raise WiringError("async review bundle is invalid")
        if not any(item.get("code") == "LEDGER_RECORD_REJECTED" for item in bundle.get("findings", [])):
            bundle.setdefault("findings", []).append(dict(finding))
        bundle["claude_review_mode"] = policy.FULL
        bundle["audit_receipt"] = None
        return _finalize_replacement(bundle, config, root, original)
    return _replacement({"state": "FAILED", "claude_review_mode": policy.FULL,
                         "ambiguous_outcome": True, "findings": [dict(finding)]}, config)


def _append_worktree_apply_context(response: dict[str, Any], job_id: str) -> dict[str, Any]:
    sentence = (f"After accepting the review, call codex_apply with jobId {job_id} in the same turn; "
                "a conflict result lists the paths and keeps the worktree.")
    prior = response.get("systemMessage")
    response["systemMessage"] = f"{prior}\n{sentence}" if isinstance(prior, str) and prior else sentence
    return response


def _async_post_followup(event: Mapping[str, Any], *, config: Mapping[str, Any] | None,
                         state_root: str | os.PathLike[str] | None,
                         repo_root: str | os.PathLike[str] | None,
                         reviewer: Callable[..., Mapping[str, Any]] | None = None,
                         micro_repair: Callable[..., Mapping[str, Any]] | None = None,
                         failed: bool = False) -> dict[str, Any]:
    root = _state_root(state_root)
    def interrupted() -> dict[str, Any]:
        return _replacement({"status": "ALREADY_CONSUMED_OR_INTERRUPTED",
                             "claude_review_mode": policy.FULL, "ambiguous_outcome": True}, _load_config(config))
    claim = None
    claimed = False
    try:
        path, index, pending, original = _async_owned(root, event, repo_root)
        claim = _async_claim(path)
        claim.__enter__()
        claimed = True
        path, index, pending, original = _async_owned(root, event, repo_root)
        if index.get("stage") == "CONSUMED":
            _release_async_lease(root, index, pending)
            (root / "pending" / f"{index['call_key']}.json").unlink(missing_ok=True)
            return index.get("response") or interrupted()
        if index.get("stage") == "FINALIZING":
            artifact = root / "bundle_artifacts" / f"{index['call_key']}.json"
            raw = (root / "raw_tool_outputs" / f"{index['call_key']}.json").resolve()
            if artifact.is_file() and raw.is_file():
                bundle = json.loads(artifact.read_text(encoding="utf-8"))
                if (isinstance(bundle, dict) and bundle.get("task_id") == pending["task_id"]
                        and bundle.get("raw_output_path") == str(raw)):
                    rejection = _async_ledger_rejection(root, index, pending, artifact, success=True)
                    response = (_async_rejected_response(root, original, artifact, rejection,
                                                         _load_config(config), success=True)
                                if rejection else _finalize_replacement(bundle, _load_config(config), root, original,
                                                                       pending.get("repo_root")))
                    job = _async_job(root, index)
                    if bundle.get("apply") == "codex_apply" and job is not None:
                        response = _append_worktree_apply_context(response, index["jobId"])
                    index.update(stage="CONSUMED", bundle_path=str(artifact.resolve()), response=response)
                    _atomic_json_write(path, index)
                    _release_async_lease(root, index, pending)
                    _remove_pending(root, original)
                    return response
        index["stage"] = "FINALIZING"
        _atomic_json_write(path, index)
        job = _async_job(root, index)
        duration = _async_duration(job)
        try:
            value = {} if failed else _start_value(event.get("tool_response"))
        except WiringError:
            value = {}
        success = (event.get("tool_name") in CODEX_RESULT_TOOL_NAMES and not failed
                   and job is not None and job.get("state") == "SUCCEEDED"
                   and value.get("jobId") == index["jobId"] and value.get("state") == "SUCCEEDED"
                   and isinstance(value.get("threadId"), str) and bool(value["threadId"])
                   and value["threadId"] == job.get("threadId")
                   and isinstance(value.get("content"), str)
                   and not _codex_response_is_error(event.get("tool_response")))
        worktree_path = value.get("worktreePath") if isinstance(value.get("worktreePath"), str) else None
        if worktree_path is None and job is not None and isinstance(job.get("worktreePath"), str):
            worktree_path = job["worktreePath"]
        if success and worktree_path:
            pending = dict(pending, worktree_path=worktree_path,
                           worktree_snapshot=job.get("snapshot"))
            _atomic_json_write(_pending_path(root, original), pending)
        if success and value.get("servedAgent") in {"codex", "opencode", "agy"} and isinstance(value.get("servedModel"), str):
            pending = dict(pending, agent=value["servedAgent"], model=value["servedModel"],
                           attempted_models=value.get("attemptedModels", []))
            _atomic_json_write(_pending_path(root, original), pending)
        synthetic = dict(original, duration_ms=duration)
        if success:
            synthetic.update(hook_event_name="PostToolUse", tool_response={"content": [
                {"type": "text", "text": json_transport.dumps(value)}]})
            response = post_tool_use(synthetic, config=config, state_root=state_root,
                                     repo_root=pending.get("repo_root"), reviewer=reviewer,
                                     micro_repair=micro_repair)
            artifact = (root / "bundle_artifacts" / f"{index['call_key']}.json").resolve()
            expected = str((root / "raw_tool_outputs" / f"{index['call_key']}.json").resolve())
            if not artifact.is_file() or not Path(expected).is_file():
                raise WiringError("successful finalization was not durably recorded")
            rejection = _async_ledger_rejection(root, index, pending, artifact, success=True)
            if rejection:
                response = _async_rejected_response(root, original, artifact, rejection,
                                                    _load_config(config), success=True)
        else:
            synthetic.update(hook_event_name="PostToolUseFailure",
                             error=event.get("error") or value.get("errorCode") or "ASYNC_TERMINAL_FAILURE")
            response = post_tool_use_failure(synthetic, state_root=state_root)
            artifact = (root / "raw_tool_outputs" / f"{index['call_key']}-failure.json").resolve()
            if not artifact.is_file():
                raise WiringError("failed outcome was not durably recorded")
            rejection = _async_ledger_rejection(root, index, pending, artifact, success=False)
            if rejection:
                response = _async_rejected_response(root, original, artifact, rejection,
                                                    _load_config(config), success=False)
            if event.get("hook_event_name") == "PostToolUse":
                message = response.get("hookSpecificOutput", {}).get("additionalContext", "Async job failed; FULL review required.")
                if not rejection:
                    response = _replacement({"state": "FAILED", "ambiguous_outcome": True,
                                             "claude_review_mode": policy.FULL}, _load_config(config))
                response["hookSpecificOutput"]["additionalContext"] = message
        if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES:
            response = _append_project_job_status(response, root, index["project_key"], event)
        if success and worktree_path:
            response = _append_worktree_apply_context(response, index["jobId"])
        index.update(stage="CONSUMED", bundle_path=str(artifact), response=response)
        _atomic_json_write(path, index)
        _release_async_lease(root, index, pending)
        try:
            _remove_pending(root, original)
        except OSError:
            pass  # CONSUMED is durable; the next read finishes cleanup.
        return response
    except Exception as error:
        _record_diagnostic(root, event, "PostToolUse", error)
        return interrupted()
    finally:
        if claimed:
            claim.__exit__(None, None, None)


def _persist_raw_output(root: Path, event: Mapping[str, Any], value: Any, suffix: str = "") -> Path:
    path = root / "raw_tool_outputs" / f"{_call_key(event)}{suffix}.json"
    _atomic_json_write(path, value)
    return path.resolve()


def _finding(code: str, message: str) -> dict[str, str]:
    return {"code": code, "message": message}


def _reviewer_findings_with_evidence(
    report: Mapping[str, Any], root: Path, event: Mapping[str, Any]
) -> Any:
    findings = report.get("findings", [])
    error_code = report.get("error_code")
    stderr_tail = getattr(report, "stderr_tail", None)
    if error_code in {"REVIEWER_PROCESS_FAILED", "REVIEWER_TIMEOUT"} and stderr_tail:
        message = findings[0]["message"] if findings else "Reviewer-1 process failed."
        try:
            path = _persist_raw_output(root, event, stderr_tail, "-reviewer1-stderr")
        except Exception as error:
            message += f" stderr not saved ({type(error).__name__})"
        else:
            message += f" stderr: {path}"
        return [_finding(error_code, message)]
    raw_stdout = getattr(report, "raw_stdout", None)
    if error_code != "REVIEWER_RESPONSE_INVALID" or raw_stdout is None:
        return findings
    message = findings[0]["message"] if findings else "Reviewer-1 response was invalid."
    try:
        path = _persist_raw_output(root, event, raw_stdout, "-reviewer1")
    except Exception as error:
        message += f" raw output not saved ({type(error).__name__})"
    else:
        message += f" raw output: {path}; decoder detail: {getattr(report, 'decoder_detail', '')}"
    return [_finding("REVIEWER_RESPONSE_INVALID", message)]


def _concurrent_writer_message(classification: Mapping[str, Any]) -> str:
    paths = classification.get("halt_paths")
    if paths is None:
        return (
            "Concurrent-writer classification requires FULL Claude review; "
            f"halt paths unavailable ({classification.get('code', ledger.UNKNOWN_CONCURRENT_WRITER)})"
        )
    preview = ", ".join(paths[:10])
    more = " …" if len(paths) > 10 else ""
    return (
        "Concurrent-writer classification requires FULL Claude review; "
        f"halt paths ({len(paths)}): {preview}{more}"
    )


def _apply_out_of_scope_result(
    bundle: dict[str, Any],
    result: Mapping[str, Any],
    *,
    protected_paths: Sequence[str],
) -> bool:
    """Apply an out-of-scope inspection result and report whether it forces FULL."""

    if result.get("status") != ledger.OK:
        bundle["findings"].append(
            _finding(
                result.get("code", "OUT_OF_SCOPE_CHECK_FAILED"),
                result.get(
                    "message",
                    "Out-of-scope modification check failed; forcing FULL Claude review.",
                ),
            )
        )
        return True

    out_of_scope = result.get("paths", [])
    if not out_of_scope:
        return False

    bundle["findings"].append(
        _finding(
            "OUT_OF_SCOPE_MODIFICATION",
            f"{len(out_of_scope)} file(s) modified outside declared scope: "
            f"{', '.join(out_of_scope[:5])}",
        )
    )
    existing = bundle.get("protected_paths_touched", [])
    existing_paths = existing if isinstance(existing, list) else []
    bundle["protected_paths_touched"] = sorted(
        set(existing_paths)
        | set(ledger.detect_protected_paths(out_of_scope, protected_paths=protected_paths))
    )
    return True


def _base_bundle(error: str | None = None) -> dict[str, Any]:
    findings = [_finding("INTERNAL_POST_ERROR", error)] if error else []
    return {
        "task_id": None,
        "phase": None,
        "implementer": "codex-main",
        "thread_id": None,
        "route": None,
        "model": None,
        "effort": None,
        "baseline_head": None,
        "current_head": None,
        "changed_paths": None,
        "known_concurrent_scopes": [],
        "whole_tree_delta": None,
        "whole_tree_anomaly": None,
        "changed_path_count": None,
        "changed_paths_sha256": None,
        "delta_lines": None,
        "delta_files": None,
        "validation": "UNKNOWN",
        "reviewer1_provider": None,
        "reviewer1_result": None,
        "reviewer1_skipped_reason": None,
        "reviewer1_model_requested": None,
        "served_model_attestation": "UNAVAILABLE",
        "routing_enforced": False,
        "findings": findings,
        "claude_review_mode": policy.FULL,
        "raw_output_path": None,
        "full_bundle_path": None,
        "protected_paths_touched": [],
        "concurrent_writer": None,
        "implementer_report": "",
        "audit_receipt": None,
        "micro_repair": None,
        "micro_repair_cleared_review": False,
        "task_contract_grounded": False,
        "worker_validation": None,
        "hook_checks": None,
        "project_root_reason": None,
    }


def _build_audit_receipt(bundle: Mapping[str, Any], task_id: str | None) -> dict[str, Any] | None:
    if bundle.get("claude_review_mode") != policy.AUDIT:
        return None
    changed_paths = bundle.get("changed_paths")
    if not isinstance(changed_paths, list) or not changed_paths:
        return None
    files_str = " ".join(f'"{p}"' for p in changed_paths[:10])
    task_label = task_id or "task"
    return {
        "verdict": "READY_FOR_ONE_TURN_ACCEPT",
        "implementer": bundle.get("implementer"),
        "model": bundle.get("model"),
        "effort": bundle.get("effort"),
        "reviewer1_verdict": bundle.get("reviewer1_result") or bundle.get("reviewer1_skipped_reason"),
        "validation_summary": f"validation={bundle.get('validation')}, delta={bundle.get('delta_lines', 0)}L/{bundle.get('delta_files', 0)}F",
        "commit_command": f'git add {files_str} && git commit -m "feat({task_label}): implement changes"',
    }


def _replacement(bundle: Mapping[str, Any], config: Mapping[str, Any]) -> dict[str, Any]:
    field = config["post_tool_output_field"]
    text = json_transport.dumps(bundle, sort_keys=True, separators=(",", ":"))
    return {
        "hookSpecificOutput": {
            "hookEventName": "PostToolUse",
            field: [{"type": "text", "text": text}],
        }
    }


def _report_limit(config: Mapping[str, Any], phase: Any) -> int:
    key = (
        "operation_report_max_chars"
        if phase == "operation"
        else "implementation_report_max_chars"
    )
    default = OPERATION_REPORT_LIMIT if phase == "operation" else IMPLEMENTER_REPORT_LIMIT
    return int(config.get(key, default))


def _plan_summary(report: str) -> str:
    match = re.search(r"(?im)^#{0,6}\s*Summary:\s*(.*)$", report)
    if match:
        tail = report[match.start(1):]
        next_heading = re.search(r"(?m)^#{1,6}\s+\S", tail[1:])
        return tail[:next_heading.start() + 1 if next_heading else None].strip()[:4000]
    return report[:4000]


def _store_plan(runtime_root: Path, event: Mapping[str, Any], task_id: Any,
                report: str, bundle: dict[str, Any]) -> None:
    safe_task = re.sub(r"[^A-Za-z0-9_-]", "_", str(task_id or "task"))[:80]
    path = (runtime_root / "plans" / f"{safe_task}-{_call_key(event)}.md").resolve()
    if len(report) > 15000:
        bundle["findings"].append(_finding("PLAN_OUTPUT_TOO_LONG", "Plan exceeds 15,000 characters."))
    if not report.strip():
        bundle["findings"].append(_finding("PLAN_OUTPUT_EMPTY", "Plan output is empty."))
    content = report[:15000]
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    try:
        temp.write_text(content, encoding="utf-8")
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)
    bundle["plan_path"] = str(path)
    bundle["plan_summary"] = _plan_summary(content)


def _non_report_bytes(bundle: Mapping[str, Any]) -> int:
    non_report = {key: value for key, value in bundle.items() if key != "implementer_report"}
    return len(
        json_transport.dumps(non_report, sort_keys=True, separators=(",", ":")).encode("utf-8")
    )


def _changed_paths_summary(paths: Sequence[Any]) -> tuple[list[Any], int, str]:
    encoded = json_transport.dumps(list(paths), sort_keys=True, separators=(",", ":")).encode(
        "utf-8"
    )
    return list(paths[:CHANGED_PATH_PREVIEW_LIMIT]), len(paths), hashlib.sha256(encoded).hexdigest()


def _copy_findings(findings: Any, message_limit: int | None = None) -> list[dict[str, Any]]:
    if not isinstance(findings, list):
        return []
    copied: list[dict[str, Any]] = []
    for finding in findings:
        if not isinstance(finding, Mapping):
            continue
        code = finding.get("code")
        message = finding.get("message")
        item: dict[str, Any] = {"code": code}
        if isinstance(message, str):
            preview = message if message_limit is None else message[:message_limit]
            item["message"] = preview
            if preview != message:
                item["message_char_count"] = len(message)
                item["message_sha256"] = hashlib.sha256(
                    message.encode("utf-8", "backslashreplace")
                ).hexdigest()
        else:
            item["message"] = ""
        copied.append(item)
    return copied


def _compact_bundle(bundle: Mapping[str, Any], cap: int) -> tuple[dict[str, Any], bool]:
    """Return the Claude-facing bundle and whether mandatory fields exceeded the cap."""

    compact = dict(bundle)
    compact.pop("hook_checks_full", None)
    compact.pop("codex_quota", None)
    compact.pop("whole_tree_delta", None)
    compact.pop("whole_tree_anomaly", None)
    compact.pop("known_concurrent_scopes", None)
    known_scopes = bundle.get("known_concurrent_scopes")
    if isinstance(known_scopes, list) and known_scopes:
        compact["known_concurrent_scopes"] = {
            "count": len(known_scopes),
            "task_ids": sorted({item["task_id"] for item in known_scopes})[:5],
        }
    paths = bundle.get("changed_paths")
    if isinstance(paths, list):
        preview, count, digest = _changed_paths_summary(paths)
        compact["changed_paths"] = preview
        compact["changed_path_count"] = count
        compact["changed_paths_sha256"] = digest
    compact["findings"] = _copy_findings(bundle.get("findings"))

    repair = bundle.get("micro_repair")
    if isinstance(repair, Mapping) and isinstance(repair.get("initial_findings"), list):
        repair_copy = dict(repair)
        repair_copy["initial_findings"] = _copy_findings(repair["initial_findings"])
        compact["micro_repair"] = repair_copy

    if _non_report_bytes(compact) <= cap:
        return compact, False

    # Finding messages are the only free text allowed to consume the remainder.
    all_messages = [
        finding.get("message", "")
        for finding in compact["findings"]
        if isinstance(finding, Mapping)
    ]
    if isinstance(compact.get("micro_repair"), Mapping):
        all_messages.extend(
            finding.get("message", "")
            for finding in compact["micro_repair"].get("initial_findings", [])
            if isinstance(finding, Mapping)
        )
    high = max((len(message) for message in all_messages if isinstance(message, str)), default=0)
    low = 0
    best: dict[str, Any] | None = None
    while low <= high:
        limit = (low + high) // 2
        candidate = dict(compact)
        candidate["findings"] = _copy_findings(bundle.get("findings"), limit)
        if isinstance(repair, Mapping) and isinstance(repair.get("initial_findings"), list):
            repair_copy = dict(repair)
            repair_copy["initial_findings"] = _copy_findings(repair["initial_findings"], limit)
            candidate["micro_repair"] = repair_copy
        if _non_report_bytes(candidate) <= cap:
            best = candidate
            low = limit + 1
        else:
            high = limit - 1
    if best is not None:
        return best, False

    compact["findings"] = _copy_findings(bundle.get("findings"), 0)
    if isinstance(repair, Mapping) and isinstance(repair.get("initial_findings"), list):
        repair_copy = dict(repair)
        repair_copy["initial_findings"] = _copy_findings(repair["initial_findings"], 0)
        compact["micro_repair"] = repair_copy
    while isinstance(compact.get("changed_paths"), list) and compact["changed_paths"]:
        compact["changed_paths"] = compact["changed_paths"][:-1]
        if _non_report_bytes(compact) <= cap:
            return compact, False

    # For unusually small configured caps, discard non-contract metadata before
    # sacrificing any verdict, identity, safety field, finding code, or artifact pointer.
    audit = bundle.get("audit_receipt")
    micro_repair = bundle.get("micro_repair")
    mandatory = {
        "task_id": bundle.get("task_id"),
        "implementer": bundle.get("implementer"),
        "model": bundle.get("model"),
        "effort": bundle.get("effort"),
        "thread_id": bundle.get("thread_id"),
        "routing_enforced": bundle.get("routing_enforced"),
        "claude_review_mode": bundle.get("claude_review_mode"),
        "reviewer1_result": bundle.get("reviewer1_result"),
        "reviewer1_skipped_reason": bundle.get("reviewer1_skipped_reason"),
        "project_root_reason": bundle.get("project_root_reason"),
        "protected_paths_touched": bundle.get("protected_paths_touched"),
        "concurrent_writer": bundle.get("concurrent_writer"),
        "micro_repair": (
            {"status": micro_repair.get("status")}
            if isinstance(micro_repair, Mapping)
            else micro_repair
        ),
        "micro_repair_cleared_review": bundle.get("micro_repair_cleared_review"),
        "validation": bundle.get("validation"),
        "hook_checks": bundle.get("hook_checks"),
        "audit_receipt": (
            {"verdict": audit.get("verdict")} if isinstance(audit, Mapping) else audit
        ),
        "changed_paths": [],
        "changed_path_count": compact.get("changed_path_count"),
        "changed_paths_sha256": compact.get("changed_paths_sha256"),
        "findings": _copy_findings(bundle.get("findings"), 0),
        "full_bundle_path": bundle.get("full_bundle_path"),
        "plan_path": bundle.get("plan_path"),
        "plan_summary": bundle.get("plan_summary"),
        "implementer_report": bundle.get("implementer_report", ""),
    }
    if "known_concurrent_scopes" in compact:
        mandatory["known_concurrent_scopes"] = compact["known_concurrent_scopes"]
    if "code_map" in compact:
        mandatory["code_map"] = compact["code_map"]
    return mandatory, _non_report_bytes(mandatory) > cap


def _finalize_replacement(
    bundle: dict[str, Any],
    config: Mapping[str, Any],
    runtime_root: Path,
    event: Mapping[str, Any],
    repo_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    artifact_path = (runtime_root / "bundle_artifacts" / f"{_call_key(event)}.json").resolve()
    bundle["full_bundle_path"] = str(artifact_path)
    cap = int(config.get("bundle_non_report_max_bytes", BUNDLE_NON_REPORT_MAX_BYTES))
    compact, mandatory_overflow = _compact_bundle(bundle, cap)
    if mandatory_overflow and bundle.get("claude_review_mode") != policy.FULL:
        bundle["claude_review_mode"] = policy.FULL
        bundle["audit_receipt"] = None
        compact, _ = _compact_bundle(bundle, cap)
    compact["implementer_report"] = str(bundle.get("implementer_report", ""))[
        : _report_limit(config, bundle.get("phase"))
    ]
    if "codex_quota" in bundle:
        compact["codex_quota"] = bundle["codex_quota"]
        excess = _non_report_bytes(compact) - cap
        if excess > 0:
            smaller, overflow = _compact_bundle(bundle, max(0, cap - excess))
            smaller["codex_quota"] = bundle["codex_quota"]
            if not overflow and _non_report_bytes(smaller) <= cap:
                compact = smaller
                compact["implementer_report"] = str(bundle.get("implementer_report", ""))[
                    : _report_limit(config, bundle.get("phase"))
                ]
            else:
                compact.pop("codex_quota")
    _atomic_json_write(artifact_path, bundle)
    response = _replacement(compact, config)
    try:
        quota_message = _codex_quota_system_message(bundle.get("codex_quota"))
        if quota_message:
            existing_message = response.get("systemMessage")
            response["systemMessage"] = (
                f"{existing_message}\n{quota_message}"
                if isinstance(existing_message, str) and existing_message
                else quota_message
            )
    except Exception:
        pass
    try:
        repository, _reason = _repo_root(event, repo_root)
        compaction_advisor.queue_receipt(event, repository, bundle)
    except BaseException:
        pass
    return response


def _codex_quota_system_message(
    quota: Any, *, local_tz: Any = None
) -> str | None:
    """Render available Codex quota windows for the PostToolUse system message."""

    if not isinstance(quota, Mapping):
        return None
    months = (
        "Jan", "Feb", "Mar", "Apr", "May", "Jun",
        "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    )
    parts: list[str] = []
    for window, reset_key, used_key in (
        ("5h", "5h_resets_at", "5h_used_pct"),
        ("7d", "7d_resets_at", "7d_used_pct"),
    ):
        try:
            used = quota.get(used_key)
            reset = quota.get(reset_key)
            if isinstance(used, bool) or not isinstance(used, (int, float)):
                continue
            if not isinstance(reset, str) or not reset:
                continue
            parsed = datetime.fromisoformat(reset[:-1] + "+00:00" if reset.endswith("Z") else reset)
            if parsed.tzinfo is None or parsed.utcoffset() is None:
                continue
            local_reset = parsed.astimezone(local_tz) if local_tz is not None else parsed.astimezone()
            left = max(0, min(100, round(100 - used)))
            reset_label = (
                local_reset.strftime("%H:%M")
                if window == "5h"
                else f"{months[local_reset.month - 1]} {local_reset.day}"
            )
            parts.append(f"{window} {left}% (resets {reset_label})")
        except Exception:
            continue
    if not parts:
        return None
    return "⚓ **Purser:** Codex quota left: " + " · ".join(parts)


def _internal_post_response(
    event: Mapping[str, Any],
    error: BaseException,
    *,
    config: Mapping[str, Any] | None,
    state_root: str | os.PathLike[str] | None,
) -> dict[str, Any]:
    try:
        loaded_config = _load_config(config)
    except Exception:
        loaded_config = {
            "post_tool_output_field": "updatedToolOutput",
            "reviewer1_model_requested": None,
        }
    bundle = _base_bundle(_safe_error(error))
    bundle["reviewer1_model_requested"] = loaded_config.get("reviewer1_model_requested")
    runtime_root: Path | None = None
    pending: Mapping[str, Any] | None = None
    try:
        runtime_root = _state_root(state_root)
        raw_path = _persist_raw_output(runtime_root, event, event.get("tool_response"))
        bundle["raw_output_path"] = str(raw_path)
        try:
            pending = _read_pending(runtime_root, event)
        except WiringError:
            pending = None
        if isinstance(pending, Mapping):
            bundle.update(
                implementer=f"{pending.get('agent', 'codex')}-main",
                task_id=pending.get("task_id"),
                phase=pending.get("phase"),
                route=pending.get("route"),
                model=pending.get("model"),
                effort=pending.get("effort"),
                routing_enforced=pending.get("kind") != "pretool_bypass",
            )
        if isinstance(pending, Mapping) and pending.get("kind") == "pretool_bypass":
            reason = pending.get("reason")
            bundle["findings"].append(
                _finding(
                    "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED",
                    (
                        "Routing was NOT enforced for this call and no usable baseline was "
                        f"captured. Internal PreToolUse reason: {reason}"
                    ),
                )
            )
            try:
                thread_id, report = _extract_codex_result(event.get("tool_response"))
                bundle["thread_id"] = thread_id
                bundle["implementer_report"] = report
            except WiringError as thread_error:
                bundle["findings"].append(
                    _finding("THREAD_ID_UNRECOVERABLE", str(thread_error))
                )
            _remove_pending(runtime_root, event)
    except Exception:
        pass
    _record_diagnostic(runtime_root, event, "PostToolUse", error)
    if runtime_root is not None:
        response = _finalize_replacement(bundle, loaded_config, runtime_root, event,
                                         pending.get("repo_root") if isinstance(pending, Mapping) else None)
        if isinstance(pending, Mapping) and not pending.get("async_call"):
            try:
                _release_scope_lease(runtime_root, pending)
                _remove_pending(runtime_root, event)
            except Exception as release_error:
                _record_diagnostic(runtime_root, event, "PostToolUse", release_error)
        return response
    return _replacement(bundle, loaded_config)


def internal_post_error_response(
    event: Mapping[str, Any],
    error: BaseException,
    *,
    config: Mapping[str, Any] | None = None,
    state_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Best-effort FULL-review response for failures above the Post handler."""

    return _internal_post_response(
        event,
        error,
        config=config,
        state_root=state_root,
    )


def _post_observations(
    event: Mapping[str, Any],
    runtime_root: Path,
    pending: Mapping[str, Any],
    bundle: Mapping[str, Any],
    *,
    classification: Mapping[str, Any] | None = None,
    reviewer_report: Mapping[str, Any] | None = None,
    reviewer_started: bool = False,
) -> None:
    task_id = pending.get("task_id")
    project_namespace = _pending_project_key(pending)
    envelope = _telemetry_envelope(
        event,
        pending.get("repo_root"),
        project_namespace if isinstance(project_namespace, str) else None,
    )
    codex_tokens = _codex_usage(event.get("tool_response"))
    finished_payload: dict[str, Any] = {
        "state_root": runtime_root,
        **envelope,
        "duration_ms": _numeric_duration_ms(event.get("duration_ms")),
        "implementer": pending.get("agent", bundle.get("implementer")),
        "served_model": pending.get("model"),
        "route": pending.get("route"),
        "phase": pending.get("phase"),
        "changed_paths_count": bundle.get("delta_files"),
        "known_concurrent_scopes": bundle.get("known_concurrent_scopes", []),
        "validation": bundle.get("validation"),
        "thread_id": (
            telemetry_schema.normalize_identifier(bundle.get("thread_id"), fallback="thread")
            if bundle.get("thread_id") is not None
            else None
        ),
        "claude_review_mode": bundle.get("claude_review_mode"),
        "routing_enforced": bundle.get("routing_enforced"),
        "codex_usage_known": bool(codex_tokens),
        "eta_minutes": pending.get("eta_minutes"),
        "reason": pending.get("reason"),
        "reason_source": pending.get("reason_source"),
        "previous_route": pending.get("previous_route"),
        "new_route": pending.get("new_route"),
    }
    finished_payload.update({f"codex_{key}": value for key, value in codex_tokens.items()})
    _observe("emit", "IMPLEMENTER_FINISHED", task_id=task_id, **finished_payload)

    if pending.get("mechanical_opencode") is True:
        try:
            import mechanical_learning
            reviewer_skipped = bundle.get("reviewer1_skipped_reason") in {"NO_DELTA", "BELOW_THRESHOLD"}
            review_passed = (
                (bundle.get("worker_validation") or {}).get("status") == "PASS"
                if reviewer_skipped
                else (
                    bundle.get("reviewer1_skipped_reason") is None
                    and bundle.get("validation") == "PASS"
                    and bundle.get("reviewer1_result") == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE
                )
            )
            accepted = (
                review_passed
                and bundle.get("micro_repair") is None
                and bundle.get("micro_repair_cleared_review") is not True
            )
            if pending.get("agent") == "opencode":
                attempted_models = pending.get("attempted_models")
                if isinstance(attempted_models, list):
                    for missing_model in attempted_models:
                        if isinstance(missing_model, str) and missing_model != pending.get("model"):
                            missed_positions = mechanical_learning.record_sample(
                                missing_model, False, root=runtime_root
                            )
                            if missed_positions and missed_positions[0] != missed_positions[1]:
                                _observe("emit", "MECHANICAL_MODEL_RANK_CHANGED", state_root=runtime_root,
                                         project_key=project_namespace, served_model=missing_model,
                                         old_rank=missed_positions[0], new_rank=missed_positions[1])
                positions = mechanical_learning.record_sample(
                    str(pending.get("model")), accepted, root=runtime_root
                )
                if positions and positions[0] != positions[1]:
                    _observe("emit", "MECHANICAL_MODEL_RANK_CHANGED", state_root=runtime_root,
                             project_key=project_namespace, served_model=pending.get("model"),
                             old_rank=positions[0], new_rank=positions[1])
            if accepted:
                mechanical_learning.start_shadow(
                    project_key=str(project_namespace),
                    repository=str(pending.get("worktree_path") or pending.get("repo_root")),
                    prompt=str(pending.get("shadow_prompt", "")),
                    accepted_report=str(bundle.get("implementer_report", "")),
                    served_model=(str(pending.get("model", ""))
                                  if pending.get("agent") == "opencode" else ""), state=runtime_root,
                )
        except BaseException:
            pass

    model_requested = bundle.get("reviewer1_model_requested")
    if reviewer_started:
        usage = dict(getattr(reviewer_report, "usage", ()))
        duration_seconds = getattr(reviewer_report, "duration_seconds", None)
        duration_ms = (
            duration_seconds * 1000
            if isinstance(duration_seconds, (int, float)) and not isinstance(duration_seconds, bool)
            else None
        )
        elapsed_seconds = getattr(reviewer_report, "elapsed_seconds", None)
        if duration_ms is None and isinstance(elapsed_seconds, (int, float)) and not isinstance(elapsed_seconds, bool):
            duration_ms = elapsed_seconds * 1000
        review_payload: dict[str, Any] = {
            "state_root": runtime_root,
            **envelope,
            "reviewer1_provider": bundle.get("reviewer1_provider"),
            "reviewer1_model_requested": model_requested,
            "served_model_attestation": "UNAVAILABLE",
            "reviewer1_result": bundle.get("reviewer1_result"),
            "duration_ms": duration_ms,
            "gemini_usage_known": bool(usage),
        }
        review_payload.update({f"gemini_{key}": value for key, value in usage.items()})
        review_status = reviewer_report.get("review_status") if isinstance(reviewer_report, Mapping) else None
        if review_status == "COMPLETED" or (
            review_status is None
            and bundle.get("reviewer1_result") != policy.REVIEWER1_UNAVAILABLE
        ):
            event_type = "REVIEWER1_FINISHED"
        else:
            event_type = "REVIEWER1_FAILED"
            reviewer_error_code = reviewer_report.get("error_code") if isinstance(reviewer_report, Mapping) else None
            if reviewer_error_code is not None:
                review_payload["reviewer1_error_code"] = reviewer_error_code
            review_payload["failure_code"] = (
                "REVIEWER1_UNAVAILABLE"
                if bundle.get("reviewer1_result") == policy.REVIEWER1_UNAVAILABLE
                else "REVIEWER1_INCOMPLETE"
            )
        _observe("emit", event_type, task_id=task_id, **review_payload)
    elif bundle.get("reviewer1_skipped_reason") is not None:
        _observe(
            "emit",
            "REVIEWER1_SKIPPED",
            task_id=task_id,
            state_root=runtime_root,
            **envelope,
            reviewer1_skipped_reason=bundle.get("reviewer1_skipped_reason"),
            reviewer1_model_requested=model_requested,
            served_model_attestation="UNAVAILABLE",
        )
    if classification is not None and classification.get("decision") == ledger.UNKNOWN_CONCURRENT_WRITER:
        halt_paths = classification.get("halt_paths")
        _observe(
            "emit",
            "UNKNOWN_CONCURRENT_WRITER",
            task_id=task_id,
            state_root=runtime_root,
            **envelope,
            concurrent_writer=True,
            code=classification.get("code", ledger.UNKNOWN_CONCURRENT_WRITER),
            halt_paths=halt_paths[:20] if halt_paths is not None else None,
            halt_path_count=len(halt_paths) if halt_paths is not None else None,
        )
    repository = pending.get("repo_root")
    update_kwargs: dict[str, Any] = {
        "state_root": runtime_root,
        "project_root": repository if isinstance(repository, str) else None,
        "project_key": project_namespace,
    }
    vault_root = pending.get("vault_root")
    if isinstance(vault_root, str) and vault_root:
        update_kwargs["vault_root"] = vault_root
    _observe(
        "update_task",
        task_id,
        **update_kwargs,
    )


def post_tool_use(
    event: Mapping[str, Any],
    *,
    config: Mapping[str, Any] | None = None,
    state_root: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    reviewer: Callable[..., Mapping[str, Any]] | None = None,
    micro_repair: Callable[..., Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    """Replace a raw Codex result with the compact reviewed bundle."""

    if event.get("tool_name") in CODEX_APPLY_TOOL_NAMES:
        try:
            root = _state_root(state_root)
            tool_input = event.get("tool_input")
            job_id = tool_input.get("jobId") if isinstance(tool_input, Mapping) else None
            if isinstance(job_id, str):
                index = json.loads((root / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8"))
                job = _async_job(root, index)
                if (isinstance(index, dict) and job is not None
                        and job.get("applyState") in {"applied", "conflict"}):
                    return _append_project_job_status({}, root, index.get("project_key", ""), event,
                                                      pending_only=True)
        except (OSError, ValueError, TypeError, KeyError):
            pass
        return {}
    if event.get("tool_name") not in CODEX_TOOLS:
        return {}
    if event.get("tool_name") in CODEX_START_TOOL_NAMES:
        return _post_start(event, config, state_root)
    if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES | CODEX_CANCEL_TOOL_NAMES:
        return _async_post_followup(event, config=config, state_root=state_root,
                                    repo_root=repo_root, reviewer=reviewer, micro_repair=micro_repair)
    start_time = time.monotonic()
    try:
        loaded_config = _load_config(config)
        max_total_hook_budget = float(loaded_config.get("max_total_hook_budget_seconds", 240.0))
        runtime_root = _state_root(state_root)
        try:
            _cleanup_stale_pending(runtime_root, event)
        except BaseException:
            pass
        pending = _read_pending(runtime_root, event)
        try:
            served = _start_value(event.get("tool_response"))
        except WiringError:
            served = {}
        if served.get("servedAgent") in {"codex", "opencode", "agy"} and isinstance(served.get("servedModel"), str):
            pending = dict(pending, agent=served["servedAgent"], model=served["servedModel"],
                           attempted_models=served.get("attemptedModels", []))
            _atomic_json_write(_pending_path(runtime_root, event), pending)
        project_namespace = _pending_project_key(pending)
        project_runtime_root = _project_state_root(runtime_root, project_namespace)
        raw_path = _persist_raw_output(runtime_root, event, event.get("tool_response"))

        bundle = _base_bundle()
        bundle.update(
            {
                "implementer": f"{pending.get('agent', 'codex')}-main",
                "task_id": pending.get("task_id"),
                "phase": pending.get("phase"),
                "route": pending.get("route"),
                "model": pending.get("model"),
                "effort": pending.get("effort"),
                "reviewer1_model_requested": loaded_config["reviewer1_model_requested"],
                "raw_output_path": str(raw_path),
                "routing_enforced": pending.get("kind") != "pretool_bypass",
                "task_contract_grounded": pending.get("task_contract_grounded", False),
                "project_root_reason": pending.get("project_root_reason"),
            }
        )
        classification: Mapping[str, Any] | None = None
        reviewer_report: Mapping[str, Any] | None = None
        reviewer_started = False
        worker_validation: dict[str, Any] = {
            "source": "WORKER_REPORTED",
            "status": "UNKNOWN",
            "summary": "No report",
        }

        try:
            thread_id, report = _extract_codex_result(event.get("tool_response"))
            bundle["thread_id"] = thread_id
            bundle["implementer_report"] = report
            if pending.get("phase") == "plan":
                _store_plan(runtime_root, event, pending.get("task_id"), report, bundle)
            if pending.get("agent", "codex") == "codex":
                try:
                    bundle["codex_quota"] = codex_quota.read_quota(thread_id)
                except Exception:
                    bundle["codex_quota"] = None
            if pending.get("phase") != "plan":
                worker_validation = _parse_worker_validation(report)
                bundle["worker_validation"] = worker_validation
        except WiringError as error:
            thread_id = None
            bundle["findings"].append(_finding("THREAD_ID_UNRECOVERABLE", str(error)))
            bundle["claude_review_mode"] = policy.FULL

        if pending.get("kind") == "pretool_bypass":
            reason = pending.get("reason")
            if not isinstance(reason, str) or not reason:
                reason = "the PreToolUse bypass reason was unavailable"
            bundle["findings"].append(
                _finding(
                    "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED",
                    (
                        "Routing was NOT enforced for this call and no usable baseline was "
                        f"captured. Internal PreToolUse reason: {reason}"
                    ),
                )
            )
            bundle["claude_review_mode"] = policy.FULL
            identity_fields = ("task_id", "route", "model", "effort")
            if all(isinstance(pending.get(field), str) and pending.get(field) for field in identity_fields):
                saved = ledger.SessionLedger(project_runtime_root).save(
                    ledger.build_session_record(
                        pending["task_id"],
                        thread_id,
                        pending["route"],
                        pending["model"],
                        pending["effort"],
                        agent=pending.get("agent", "codex"),
                        phase=pending.get("phase"),
                        scope=pending.get("scope", []),
                        pending_call_baseline=None,
                        last_outcome=bundle,
                        label=pending.get("label"),
                    )
                )
                if saved["status"] != ledger.OK:
                    bundle["findings"].append(
                        _finding(
                            saved.get("code", "LEDGER_WRITE_FAILED"),
                            saved.get("message", "Bypass outcome ledger update failed."),
                        )
                    )
            _remove_pending(runtime_root, event)
            try:
                _post_observations(event, runtime_root, pending, bundle)
            except BaseException:
                _observe(
                    "update_task",
                    pending.get("task_id"),
                    state_root=runtime_root,
                    project_key=project_namespace,
                )
            return _finalize_replacement(bundle, loaded_config, runtime_root, event, pending.get("repo_root"))

        stored_profile = (
            isinstance(pending.get("protected_paths"), list)
            and all(isinstance(path, str) for path in pending["protected_paths"])
            and isinstance(pending.get("force_full_review"), bool)
            and isinstance(pending.get("skip_max_lines"), int)
            and not isinstance(pending.get("skip_max_lines"), bool)
            and isinstance(pending.get("skip_max_files"), int)
            and not isinstance(pending.get("skip_max_files"), bool)
            and isinstance(pending.get("configured"), bool)
            and isinstance(pending.get("user_status"), str)
            and isinstance(pending.get("project_status"), str)
            and (
                pending.get("reviewer_candidates") is None
                or (
                    isinstance(pending.get("reviewer_candidates"), list)
                    and all(
                        isinstance(candidate, Mapping)
                        and isinstance(candidate.get("agent"), str)
                        and isinstance(candidate.get("model"), str)
                        for candidate in pending["reviewer_candidates"]
                    )
                )
            )
        )
        if stored_profile:
            protected_paths = pending["protected_paths"]
            profile_force_full = pending["force_full_review"]
            profile_skip_max_lines = pending["skip_max_lines"]
            profile_skip_max_files = pending["skip_max_files"]
            profile_reviewer_executable = pending.get("reviewer_executable")
            profile_reviewer = pending.get("reviewer", "agy")
            profile_reviewer_candidates = pending.get("reviewer_candidates") or [
                {"agent": "agy", "model": loaded_config["reviewer1_model_requested"]}
            ]
            profile_authority_sources = pending.get("authority_sources")
            profile_configured = pending["configured"]
            profile_user_status = pending["user_status"]
            profile_project_status = pending["project_status"]
            profile_first_warning = pending.get("first_warning")
        else:
            protected_paths = project_profile._ENGINE_PROTECTED
            profile_force_full = True
            profile_skip_max_lines = None
            profile_skip_max_files = None
            profile_reviewer_executable = None
            profile_reviewer = "agy"
            profile_reviewer_candidates = [
                {"agent": "agy", "model": loaded_config["reviewer1_model_requested"]}
            ]
            profile_authority_sources = None
            profile_configured = None
            profile_user_status = None
            profile_project_status = None
            profile_first_warning = None

        profile_forces_full = bool(profile_force_full)
        if profile_forces_full:
            if profile_user_status == "rejected" or profile_project_status == "rejected":
                profile_finding = _finding(
                    "PROFILE_REJECTED",
                    profile_first_warning
                    if isinstance(profile_first_warning, str) and profile_first_warning
                    else "a profile tier was rejected",
                )
            else:
                profile_finding = _finding(
                    "PROFILE_FORCES_FULL",
                    "The resolved profile forces FULL Claude review.",
                )
            bundle["findings"].append(profile_finding)
        if stored_profile and profile_configured is False:
            bundle["profile"] = "not_configured"
        if profile_reviewer == "none":
            bundle["reviewer1_skipped_reason"] = "REVIEWER_DISABLED"
            bundle["findings"].append(
                _finding(
                    "REVIEWER_DISABLED",
                    "no independent reviewer configured; FULL review required",
                )
            )

        pending_repository = pending.get("repo_root")
        if isinstance(pending_repository, str) and pending_repository:
            repository = Path(pending_repository).resolve()
            resolved_reason = ""
        else:
            repository = None
            resolved_reason = "project root unresolved: pre-tool root was unavailable"
        project_root_reason = pending.get("project_root_reason") or resolved_reason
        bundle["project_root_reason"] = project_root_reason or None
        delta_repository = repository
        worktree_path = pending.get("worktree_path")
        if isinstance(worktree_path, str) and worktree_path:
            delta_repository = Path(worktree_path).resolve()
            bundle["apply"] = "codex_apply"
        scope = pending.get("scope")
        if not isinstance(scope, list) or (pending.get("phase") == "implementation" and not scope):
            raise WiringError("pending call scope is missing")
        baseline = pending.get("baseline")
        if not isinstance(baseline, Mapping):
            raise WiringError("pending call baseline is missing")
        if delta_repository is None or baseline.get("status") != ledger.OK:
            delta = {
                "status": ledger.UNKNOWN,
                "code": baseline.get("code", "BASELINE_UNAVAILABLE"),
                "message": baseline.get("message", "The baseline is unavailable."),
                "baseline_head": baseline.get("baseline_head"),
                "current_head": None,
                "changed_paths": None,
            }
        elif worktree_path:
            delta = ledger.compute_job_delta(delta_repository, pending.get("worktree_snapshot"))
            delta = ledger.filter_job_delta(delta_repository, delta)
        else:
            delta = ledger.compute_delta(
                delta_repository,
                baseline,
                None if pending.get("phase") == "plan" else (scope if scope else None),
                manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
            )
            if delta.get("status") == ledger.OK and pending.get("phase") != "plan" and (
                delta.get("current_head") != delta.get("baseline_head")
                or (pending.get("phase") == "operation" and delta["changed_paths"])
            ):
                delta = {"status": ledger.UNKNOWN, "code": "EXTERNAL_REPOSITORY_CHANGE",
                         "message": "Repository source changed during the job; the external change cannot be attributed to the worker.",
                         "baseline_head": delta.get("baseline_head"),
                         "current_head": delta.get("current_head"), "changed_paths": None}
        bundle["baseline_head"] = delta.get("baseline_head")
        bundle["current_head"] = delta.get("current_head")
        force_full_review = delta.get("status") != ledger.OK
        out_of_scope_res = None
        if (delta.get("status") == ledger.OK and pending.get("phase") == "implementation" and scope
                and delta_repository is not None and baseline.get("status") == ledger.OK):
            if worktree_path:
                out_of_scope_res = {"status": ledger.OK, "code": "OUT_OF_SCOPE_CHECK_COMPLETED",
                                    "paths": [path for path in delta["changed_paths"]
                                              if not any(path == item or path.startswith(item.rstrip("/") + "/")
                                                         or item == "." for item in scope)]}
            else:
                out_of_scope_res = ledger.detect_out_of_scope_modifications(
                    delta_repository, baseline, scope)
        bundle["whole_tree_anomaly"] = out_of_scope_res
        if delta.get("status") == ledger.OK and (pending.get("phase") == "plan" or
                                                  (pending.get("phase") == "implementation" and scope)):
            if pending.get("phase") == "implementation":
                bundle["whole_tree_delta"] = {"changed_paths": delta["changed_paths"],
                                               "changed_file_count": delta["changed_file_count"],
                                               "changed_line_count": delta["changed_line_count"]}
            owner = pending.get("lease_owner")
            if isinstance(owner, str):
                try:
                    foreign_owners = ledger.ScopeLeases(
                        _project_state_root(runtime_root, _pending_project_key(pending))
                    ).overlapping_owners(owner)
                    extra = (out_of_scope_res.get("paths", [])
                             if isinstance(out_of_scope_res, Mapping)
                             and out_of_scope_res.get("status") == ledger.OK else [])
                    delta, known = ledger.attribute_known_concurrent_scopes(
                        delta, scope if pending.get("phase") == "implementation" else [],
                        foreign_owners, protected_paths=protected_paths,
                        additional_paths=extra)
                    bundle["known_concurrent_scopes"] = known
                except (OSError, ValueError, TypeError, KeyError):
                    bundle["findings"].append(_finding(
                        "LEASE_HISTORY_UNAVAILABLE",
                        "Scope lease history could not be verified; forcing FULL Claude review."))
                    force_full_review = True
            else:
                force_full_review = True
                bundle["findings"].append(_finding(
                    "LEASE_HISTORY_UNAVAILABLE", "This call has no scope lease ownership record."))

        if delta.get("status") == ledger.OK and pending.get("phase") == "plan" and (
            delta["changed_paths"] or delta.get("current_head") != delta.get("baseline_head")
        ):
            if delta["changed_paths"]:
                bundle["findings"].append(_finding(
                    "PLAN_MUTATED_REPO",
                    "Repository source changed during a read-only plan call; "
                    "a concurrent writer may be the cause: " + ", ".join(delta["changed_paths"][:10]),
                ))
            delta = {"status": ledger.UNKNOWN, "code": "EXTERNAL_REPOSITORY_CHANGE",
                     "message": "Repository changes could not be attributed to this plan job.",
                     "baseline_head": delta.get("baseline_head"),
                     "current_head": delta.get("current_head"), "changed_paths": None}
            force_full_review = True

        if delta.get("status") == ledger.OK:
            bundle["changed_paths"] = delta["changed_paths"]
            bundle["delta_lines"] = delta["changed_line_count"]
            bundle["delta_files"] = delta["changed_file_count"]
            if scope:
                classification = ledger.classify_concurrent_writers(
                    delta, scope, protected_paths=protected_paths
                )
                bundle["protected_paths_touched"] = classification.get("protected_paths", [])
                bundle["concurrent_writer"] = classification.get("concurrent_writer")
            else:
                bundle["protected_paths_touched"] = ledger.detect_protected_paths(
                    delta, protected_paths=protected_paths
                )
        else:
            bundle["findings"].append(
                _finding(delta.get("code", "UNKNOWN_DELTA"), delta.get("message", "Delta is UNKNOWN."))
            )

        if out_of_scope_res is not None:
            if out_of_scope_res.get("status") == ledger.OK:
                known_paths = {entry["path"] for entry in bundle["known_concurrent_scopes"]}
                out_of_scope_res = {**out_of_scope_res, "paths": [
                    path for path in out_of_scope_res.get("paths", []) if path not in known_paths]}
            force_full_review = (
                _apply_out_of_scope_result(
                    bundle,
                    out_of_scope_res,
                    protected_paths=protected_paths,
                )
                or force_full_review
            )

        if delta.get("status") != ledger.OK:
            force_full_review = True
        else:
            check_state: dict[str, Any] = {}
            check_thread: threading.Thread | None = None
            check_started = time.monotonic()
            check_deadline = float(loaded_config.get("hook_checks_deadline_seconds", 150))
            try:
                selected = checks.select_checks(
                    _restore_checks(pending.get("profile_checks", [])), delta["changed_paths"]
                )
                if selected and pending.get("phase") != "plan":
                    def run_selected_checks() -> None:
                        try:
                            check_state["result"] = checks.run_checks(
                                delta_repository, selected, deadline_seconds=check_deadline
                            )
                        except Exception as error:
                            check_state["error"] = type(error).__name__

                    check_thread = threading.Thread(target=run_selected_checks, daemon=True)
                    check_thread.start()
            except Exception as error:
                check_state["error"] = type(error).__name__

            reviewer_config = dict(loaded_config)
            reviewer_gate_config = dict(loaded_config["reviewer_gate"])
            if isinstance(profile_skip_max_lines, int) and not isinstance(profile_skip_max_lines, bool):
                reviewer_gate_config["small_delta_max_lines"] = min(
                    reviewer_gate_config["small_delta_max_lines"], profile_skip_max_lines
                )
            if isinstance(profile_skip_max_files, int) and not isinstance(profile_skip_max_files, bool):
                reviewer_gate_config["small_delta_max_files"] = min(
                    reviewer_gate_config["small_delta_max_files"], profile_skip_max_files
                )
            reviewer_config["reviewer_gate"] = reviewer_gate_config
            reviewer_callable = reviewer if reviewer is not None else REVIEWER
            if pending.get("phase") == "plan":
                gate = policy.reviewer1_gate(
                    phase="plan", delta_files=0, delta_lines=0,
                    route=pending["route"], reviewer_available=False, config=reviewer_config,
                )
            elif profile_reviewer == "none":
                gate = policy.SkipReviewerDecision(
                    "skip reviewer",
                    "REVIEWER_DISABLED",
                    "no independent reviewer configured; FULL review required",
                )
            else:
                gate = policy.reviewer1_gate(
                    phase=pending["phase"],
                    delta_files=delta["changed_file_count"],
                    delta_lines=delta["changed_line_count"],
                    route=pending["route"],
                    reviewer_available=_reviewer_available(reviewer_callable),
                    config=reviewer_config,
                )
            if gate.should_run:
                validation_text = (
                    f"Worker-reported validation summary (status={worker_validation['status']}): "
                    f"{worker_validation['summary'][:IMPLEMENTER_REPORT_LIMIT]}"
                )

                try:
                    sig = inspect.signature(reviewer_callable)
                    accepts_kwargs = any(
                        p.kind == inspect.Parameter.VAR_KEYWORD
                        for p in sig.parameters.values()
                    )
                    params = sig.parameters
                except (ValueError, TypeError):
                    accepts_kwargs = True
                    params = {}

                reviewer_kwargs: dict[str, Any] = {
                    "task_id": pending["task_id"],
                    "phase": pending["phase"],
                    "route": pending["route"],
                    "model": pending["model"],
                    "effort": pending["effort"],
                    "repo_root": delta_repository,
                    "delta": delta,
                    "classification": classification,
                }
                if (accepts_kwargs or "task_objective" in params) and pending.get("task_objective"):
                    reviewer_kwargs["task_objective"] = pending["task_objective"]
                if (accepts_kwargs or "acceptance_criteria" in params) and pending.get("acceptance_criteria"):
                    reviewer_kwargs["acceptance_criteria"] = pending["acceptance_criteria"]
                if (accepts_kwargs or "validation_summary" in params) and validation_text:
                    reviewer_kwargs["validation_summary"] = validation_text
                if (
                    isinstance(profile_reviewer_executable, str)
                    and profile_reviewer_executable
                    and (accepts_kwargs or "reviewer_executable" in params)
                ):
                    reviewer_kwargs["reviewer_executable"] = profile_reviewer_executable
                if (
                    profile_authority_sources is not None
                    and (accepts_kwargs or "authority_sources" in params)
                ):
                    reviewer_kwargs["authority_sources"] = profile_authority_sources
                implementer_family = policy.vendor_family(pending.get("fallback_family"))
                if not implementer_family and pending.get("attempted_models"):
                    implementer_family = policy.vendor_family(policy.model_family(pending.get("model")))
                if not implementer_family:
                    implementer_family = (policy.vendor_family(loaded_config.get("coder_family"))
                                          or policy.vendor_family(policy.model_family(pending.get("model"))))
                reviewer_attempts: list[dict[str, str]] = []
                reviewer_report = None
                reviewer_invoked = False
                reviewer_started = False
                available_budget = max(
                    0.0,
                    max_total_hook_budget - (time.monotonic() - start_time),
                )
                reviewer_deadline = time.monotonic() + min(
                    float(loaded_config.get("reviewer1_timeout_seconds", 90)),
                    available_budget,
                )
                for candidate in profile_reviewer_candidates:
                    candidate_agent = candidate["agent"]
                    candidate_model = candidate["model"]
                    candidate_vendor = policy.vendor_family(policy.model_family(candidate_model))
                    if implementer_family and candidate_vendor and implementer_family == candidate_vendor:
                        reviewer_attempts.append({
                            "agent": candidate_agent, "model": candidate_model,
                            "outcome": "SKIPPED_SAME_VENDOR",
                        })
                        continue
                    if candidate_agent not in reviewer1.REVIEWER_ADAPTERS:
                        reviewer_attempts.append({
                            "agent": candidate_agent, "model": candidate_model,
                            "outcome": "SKIPPED_ADAPTER_UNREGISTERED",
                        })
                        continue
                    remaining_budget = reviewer_deadline - time.monotonic()
                    if remaining_budget <= 0:
                        reviewer_attempts.append({
                            "agent": candidate_agent, "model": candidate_model,
                            "outcome": "UNAVAILABLE_BUDGET_EXHAUSTED",
                        })
                        continue
                    candidate_kwargs = dict(reviewer_kwargs)
                    if accepts_kwargs or "reviewer_agent" in params:
                        candidate_kwargs["reviewer_agent"] = candidate_agent
                    if accepts_kwargs or "reviewer_model" in params:
                        candidate_kwargs["reviewer_model"] = candidate_model
                    if accepts_kwargs or "reviewer_timeout_seconds" in params:
                        candidate_kwargs["reviewer_timeout_seconds"] = remaining_budget
                    _observe(
                        "emit", "REVIEWER1_STARTED", task_id=pending["task_id"],
                        state_root=runtime_root,
                        **_telemetry_envelope(event, repository, project_namespace),
                        reviewer1_provider=candidate_agent,
                        reviewer1_model_requested=candidate_model,
                    )
                    reviewer_started = True
                    reviewer_invoked = True
                    reviewer_kwargs = candidate_kwargs
                    attempted_report = reviewer_callable(**candidate_kwargs)
                    unavailable = (
                        isinstance(attempted_report, Mapping)
                        and (
                            attempted_report.get("review_status") == "UNAVAILABLE"
                            or attempted_report.get("result") == policy.REVIEWER1_UNAVAILABLE
                        )
                    )
                    reviewer_attempts.append({
                        "agent": candidate_agent, "model": candidate_model,
                        "outcome": "UNAVAILABLE" if unavailable else "COMPLETED",
                    })
                    reviewer_report = attempted_report
                    if not unavailable:
                        break
                if len(reviewer_attempts) > 1:
                    bundle["reviewer1_attempts"] = reviewer_attempts
                if reviewer_report is None and not reviewer_invoked:
                    last_attempt = reviewer_attempts[-1] if reviewer_attempts else None
                    terminal_reason = (
                        last_attempt.get("outcome") if isinstance(last_attempt, Mapping) else None
                    )
                    skipped_code = (
                        "REVIEWER_ADAPTER_UNAVAILABLE"
                        if terminal_reason == "SKIPPED_ADAPTER_UNREGISTERED"
                        else "REVIEWER_VENDOR_CONFLICT"
                        if terminal_reason == "SKIPPED_SAME_VENDOR"
                        else policy.REVIEWER_UNAVAILABLE
                    )
                    bundle["reviewer1_skipped_reason"] = skipped_code
                    reviewer_report = {
                        "review_status": "UNAVAILABLE",
                        "provider": (
                            reviewer_attempts[-1]["agent"] if reviewer_attempts else "agy"
                        ),
                        "reviewer1_model_requested": (
                            reviewer_attempts[-1]["model"]
                            if reviewer_attempts else loaded_config["reviewer1_model_requested"]
                        ),
                        "result": policy.REVIEWER1_UNAVAILABLE,
                        "validation": "UNKNOWN",
                        "findings": [_finding(
                            skipped_code,
                            "No eligible registered Reviewer-1 adapter was available; FULL review required.",
                        )],
                    }
                elif isinstance(reviewer_report, Mapping) and (
                    reviewer_report.get("review_status") == "UNAVAILABLE"
                    or reviewer_report.get("result") == policy.REVIEWER1_UNAVAILABLE
                ):
                    bundle["reviewer1_skipped_reason"] = "REVIEWER1_UNAVAILABLE"
                result = reviewer_report.get("result") if isinstance(reviewer_report, Mapping) else None
                if result not in loaded_config["reviewer_results"]:
                    result = policy.UNCERTAIN
                    bundle["findings"].append(
                        _finding("REVIEWER_RESULT_INVALID", "Reviewer-1 returned an invalid result.")
                    )
                provider = reviewer_report.get("provider", "agy") if isinstance(reviewer_report, Mapping) else "agy"
                findings = _reviewer_findings_with_evidence(reviewer_report, runtime_root, event) if isinstance(reviewer_report, Mapping) else []
                validation = reviewer_report.get("validation", "UNKNOWN") if isinstance(reviewer_report, Mapping) else "UNKNOWN"

                # Priority 5: In-Hook Micro-Repair loop (1 bounded attempt with deadline & sandbox)
                if (pending.get("agent", "codex") == "codex" and loaded_config.get("micro_repair_enabled", True)
                        and result == policy.CHANGES_REQUIRED and thread_id):
                    configured_repair_timeout = int(loaded_config.get("micro_repair_timeout_seconds", 60))
                    elapsed = time.monotonic() - start_time
                    available_budget = max_total_hook_budget - elapsed - float(loaded_config.get("reviewer1_timeout_seconds", 90))
                    initial_findings = findings[:5] if isinstance(findings, list) else []

                    if available_budget < 15.0:
                        bundle["micro_repair"] = {
                            "attempted": False,
                            "status": "SKIPPED_BUDGET",
                            "initial_findings": initial_findings,
                        }
                    else:
                        actual_timeout = int(min(configured_repair_timeout, available_budget))
                        bundle["micro_repair"] = {
                            "attempted": True,
                            "status": "RUNNING",
                            "initial_findings": initial_findings,
                        }
                        repair_callable = micro_repair if micro_repair is not None else MICRO_REPAIR
                        repair_res = repair_callable(
                            thread_id=thread_id,
                            findings=findings if isinstance(findings, list) else [],
                            repo_root=delta_repository,
                            scope=scope,
                            timeout_seconds=actual_timeout,
                        )
                        # Universal delta recomputation (C19-1): always recompute after attempt
                        re_delta = (ledger.compute_job_delta(
                            delta_repository, pending.get("worktree_snapshot"))
                                    if worktree_path else ledger.compute_delta(
                                        delta_repository,
                                        baseline,
                                        scope if scope else None,
                                        manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
                                    ))
                        if worktree_path:
                            re_delta = ledger.filter_job_delta(delta_repository, re_delta)
                        if re_delta.get("status") == ledger.OK:
                            bundle["whole_tree_delta"] = {
                                "changed_paths": re_delta["changed_paths"],
                                "changed_file_count": re_delta["changed_file_count"],
                                "changed_line_count": re_delta["changed_line_count"],
                            }
                            re_anomaly = (
                                {"status": ledger.OK, "code": "OUT_OF_SCOPE_CHECK_COMPLETED",
                                 "paths": [path for path in re_delta["changed_paths"]
                                           if not any(path == item or path.startswith(item.rstrip("/") + "/")
                                                      or item == "." for item in scope)]}
                                if worktree_path and scope else
                                ledger.detect_out_of_scope_modifications(delta_repository, baseline, scope)
                                if scope else None)
                            bundle["whole_tree_anomaly"] = re_anomaly
                            if scope and isinstance(pending.get("lease_owner"), str):
                                try:
                                    foreign_owners = ledger.ScopeLeases(
                                        _project_state_root(runtime_root, _pending_project_key(pending))
                                    ).overlapping_owners(pending["lease_owner"])
                                    re_delta, known = ledger.attribute_known_concurrent_scopes(
                                        re_delta, scope, foreign_owners,
                                        protected_paths=protected_paths,
                                        additional_paths=(re_anomaly.get("paths", [])
                                                          if isinstance(re_anomaly, Mapping)
                                                          and re_anomaly.get("status") == ledger.OK else []))
                                    bundle["known_concurrent_scopes"] = known
                                except (OSError, ValueError, TypeError, KeyError):
                                    force_full_review = True
                                    bundle["findings"].append(_finding(
                                        "LEASE_HISTORY_UNAVAILABLE",
                                        "Post-repair scope lease history could not be verified."))
                            delta = re_delta
                            bundle["changed_paths"] = delta["changed_paths"]
                            bundle["delta_lines"] = delta["changed_line_count"]
                            bundle["delta_files"] = delta["changed_file_count"]
                            bundle["current_head"] = delta.get("current_head")
                            re_classification = (
                                ledger.classify_concurrent_writers(
                                    delta, scope, protected_paths=protected_paths
                                )
                                if scope
                                else None
                            )
                            bundle["protected_paths_touched"] = (
                                re_classification.get("protected_paths", [])
                                if re_classification
                                else ledger.detect_protected_paths(
                                    delta, protected_paths=protected_paths
                                )
                            )
                            bundle["concurrent_writer"] = (
                                re_classification.get("concurrent_writer")
                                if re_classification
                                else None
                            )
                            classification = re_classification
                        else:
                            delta = re_delta
                            force_full_review = True
                            bundle["findings"].append(
                                _finding(
                                    re_delta.get("code", "POST_REPAIR_DELTA_FAILED"),
                                    "Post-repair delta could not be computed; forcing FULL Claude review.",
                                )
                            )

                        if repair_res.get("repaired") is True:
                            # C22-1 & C23-1: Working-tree wide fail-closed out-of-scope check
                            out_of_scope_res = (re_anomaly if re_delta.get("status") == ledger.OK
                                                else ledger.detect_out_of_scope_modifications(
                                                    delta_repository, baseline, scope))
                            if out_of_scope_res.get("status") == ledger.OK:
                                known_paths = {entry["path"] for entry in bundle["known_concurrent_scopes"]}
                                out_of_scope_res = {**out_of_scope_res, "paths": [
                                    path for path in out_of_scope_res.get("paths", [])
                                    if path not in known_paths]}
                            force_full_review = (
                                _apply_out_of_scope_result(
                                    bundle,
                                    out_of_scope_res,
                                    protected_paths=protected_paths,
                                )
                                or force_full_review
                            )

                            if re_delta.get("status") == ledger.OK and not force_full_review:
                                re_kwargs = dict(reviewer_kwargs)
                                re_kwargs["delta"] = re_delta
                                re_kwargs["classification"] = re_classification
                                re_report = reviewer_callable(**re_kwargs)
                                result = re_report.get("result") if isinstance(re_report, Mapping) else policy.UNCERTAIN
                                findings = _reviewer_findings_with_evidence(re_report, runtime_root, event) if isinstance(re_report, Mapping) else []
                                validation = re_report.get("validation", "UNKNOWN") if isinstance(re_report, Mapping) else "UNKNOWN"
                                reviewer_report = re_report
                                if result == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE:
                                    bundle["micro_repair_cleared_review"] = True
                                    bundle["micro_repair"]["status"] = "SUCCEEDED"
                                else:
                                    bundle["micro_repair"]["status"] = "CHANGES_STILL_REQUIRED"
                            else:
                                bundle["micro_repair"]["status"] = "FAILED"
                        else:
                            force_full_review = True
                            error_text = str(repair_res.get("error", ""))
                            status = (
                                "TIMED_OUT"
                                if "timeout" in error_text.lower()
                                else "FAILED"
                            )
                            bundle["micro_repair"]["status"] = status

                bundle["reviewer1_provider"] = provider if isinstance(provider, str) else "agy"
                reported_model = (
                    reviewer_report.get("reviewer1_model_requested")
                    if isinstance(reviewer_report, Mapping) else None
                )
                if isinstance(reported_model, str) and reported_model:
                    bundle["reviewer1_model_requested"] = reported_model
                bundle["reviewer1_result"] = result
                if isinstance(findings, list):
                    bundle["findings"].extend(findings)
                bundle["validation"] = validation if validation in {"PASS", "FAIL", "UNKNOWN"} else "UNKNOWN"
                outcome: policy.ReviewerOutcome = policy.ReviewerReport(result)
            else:
                bundle["reviewer1_skipped_reason"] = gate.skip_reason
                outcome = gate
                result = None

            if check_thread is not None:
                check_thread.join(max(0.0, check_started + check_deadline + 2.0 - time.monotonic()))
                if check_thread.is_alive():
                    check_state["error"] = "TimeoutError"
            hook_status = None
            check_side_effects = False
            if "error" in check_state:
                hook_status = "UNKNOWN"
                bundle["hook_checks"] = {"status": "UNKNOWN", "results": []}
                bundle["findings"].append(_finding("HOOK_CHECK_UNKNOWN", check_state["error"]))
            elif "result" in check_state:
                try:
                    full_result = check_state["result"]
                    hook_status = full_result["status"]
                    results = full_result["results"]
                    bundle["hook_checks"] = {
                        "status": hook_status,
                        "results": [{key: item.get(key) for key in
                                     ("name", "status", "exit_code", "duration_ms")}
                                    for item in results],
                    }
                    bundle["hook_checks_full"] = full_result
                    tail_budget = 3000
                    for item in results:
                        if item.get("status") in {"FAIL", "UNKNOWN"}:
                            tail = str(item.get("tail", ""))[-tail_budget:] if tail_budget else ""
                            tail_budget -= len(tail)
                            code = "HOOK_CHECK_FAILED" if item["status"] == "FAIL" else "HOOK_CHECK_UNKNOWN"
                            bundle["findings"].append(_finding(code, f"{item['name']}: {tail}"))
                    side_effect_paths = full_result.get("side_effect_paths")
                    if side_effect_paths:
                        check_side_effects = True
                        bundle["findings"].append(_finding(
                            "HOOK_CHECK_SIDE_EFFECTS", ", ".join(side_effect_paths[:10])
                        ))
                except Exception as error:
                    hook_status = "UNKNOWN"
                    bundle["hook_checks"] = {"status": "UNKNOWN", "results": []}
                    bundle["findings"].append(_finding("HOOK_CHECK_UNKNOWN", type(error).__name__))
            mode = policy.claude_review_mode(pending["route"], outcome, loaded_config)
            is_grounded = bool(pending.get("task_contract_grounded"))
            val_status = worker_validation.get("status")
            effective_val_status = hook_status if hook_status in {"PASS", "FAIL"} else val_status

            reviewer_audit_eligible = (
                (result is not None and result == policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE)
                or (
                    isinstance(gate, (policy.ReviewerSkipped, policy.SkipReviewerDecision))
                    and gate.skip_reason in policy._AUDIT_SKIP_REASONS
                )
            )

            is_audit_eligible = (
                is_grounded
                and effective_val_status == "PASS"
                and reviewer_audit_eligible
                and delta.get("status") == ledger.OK
                and (classification is None or classification.get("decision") == ledger.PROCEED)
                and not force_full_review
                and not check_side_effects
                and (thread_id is not None)
                and mode.mode == policy.AUDIT
            )

            if is_audit_eligible:
                bundle["claude_review_mode"] = policy.AUDIT
            else:
                bundle["claude_review_mode"] = policy.FULL
                if classification is not None and classification.get("decision") != ledger.PROCEED:
                    bundle["findings"].append(
                        _finding(
                            classification.get("code", ledger.UNKNOWN_CONCURRENT_WRITER),
                            _concurrent_writer_message(classification),
                        )
                    )
                if pending.get("phase") != "plan" and val_status == "FAIL":
                    bundle["findings"].append(
                        _finding(
                            "WORKER_VALIDATION_FAILED",
                            f"Implementer reported validation failure: {worker_validation['summary'][:300]}",
                        )
                    )
                elif pending.get("phase") != "plan" and val_status == "UNKNOWN":
                    bundle["findings"].append(
                        _finding(
                            "WORKER_VALIDATION_UNVERIFIED",
                            f"Implementer validation was unverified: {worker_validation['summary'][:300]}",
                        )
                    )

                if pending.get("phase") != "plan" and not is_grounded:
                    bundle["findings"].append(
                        _finding(
                            "TASK_CONTRACT_NOT_GROUNDED",
                            "Task contract is not grounded (missing objective or criteria); forcing FULL Claude review.",
                        )
                    )

        if profile_forces_full:
            bundle["claude_review_mode"] = policy.FULL
        if pending.get("phase") == "plan":
            bundle["claude_review_mode"] = policy.FULL
            bundle["reviewer1_result"] = None
            bundle["reviewer1_skipped_reason"] = policy.PHASE_PLAN
        if thread_id is None:
            bundle["claude_review_mode"] = policy.FULL

        handoff_kwargs: dict[str, Any] = {}
        if (
            thread_id
            and repository is not None
            and pending.get("phase") == "implementation"
            and isinstance(scope, list)
            and bool(scope)
            and isinstance(baseline, Mapping)
            and baseline.get("status") == ledger.OK
        ):
            post_state = ledger.capture_baseline(
                repository,
                scope if scope else None,
                manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
            )
            if post_state.get("status") == ledger.OK:
                handoff_kwargs = {
                    "origin_baseline": baseline,
                    "origin_scope": scope,
                    "post_state": post_state,
                }

        session_ledger = ledger.SessionLedger(project_runtime_root)
        session_record = ledger.build_session_record(
            pending["task_id"],
            thread_id,
            pending["route"],
            pending["model"],
            pending["effort"],
            agent=pending.get("agent", "codex"),
            phase=pending.get("phase"),
            scope=pending.get("scope", []),
            pending_call_baseline=None,
            last_outcome=bundle,
            task_objective=pending.get("task_objective"),
            acceptance_criteria=pending.get("acceptance_criteria"),
            task_contract_grounded=pending.get("task_contract_grounded", False),
            reason=pending.get("reason"),
            profile_checks=pending.get("profile_checks"),
            label=pending.get("label"),
            **handoff_kwargs,
        )
        saved = session_ledger.save(session_record)
        if saved["status"] != ledger.OK:
            bundle["claude_review_mode"] = policy.FULL
            bundle["findings"].append(
                _finding(saved.get("code", "LEDGER_WRITE_FAILED"), saved.get("message", "Ledger update failed."))
            )
        else:
            _record_thread_root(runtime_root, thread_id, repository)
        try:
            _post_observations(
                event,
                runtime_root,
                pending,
                bundle,
                classification=classification,
                reviewer_report=reviewer_report,
                reviewer_started=reviewer_started,
            )
        except BaseException:
            _observe(
                "update_task",
                pending.get("task_id"),
                state_root=runtime_root,
                project_key=project_namespace,
            )
        if bundle.get("claude_review_mode") == policy.AUDIT:
            bundle["audit_receipt"] = _build_audit_receipt(bundle, pending.get("task_id"))
        else:
            bundle["audit_receipt"] = None
        if pending.get("has_code_map") and bundle.get("changed_paths"):
            bundle["code_map"] = "STALE"
        response = _finalize_replacement(bundle, loaded_config, runtime_root, event, pending.get("repo_root"))
        if not pending.get("async_call"):
            response = _append_dispatch_receipt(response, pending, runtime_root, event)
        if not pending.get("async_call"):
            _release_scope_lease(runtime_root, pending)
            _remove_pending(runtime_root, event)
        return response
    except Exception as error:
        return _internal_post_response(
            event,
            error,
            config=config,
            state_root=state_root,
        )


def post_tool_use_failure(
    event: Mapping[str, Any],
    *,
    config: Mapping[str, Any] | None = None,
    state_root: str | os.PathLike[str] | None = None,
    repo_root: str | os.PathLike[str] | None = None,
    reviewer: Callable[..., Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    """Archive ambiguous failure evidence without running Reviewer-1."""

    if event.get("tool_name") not in CODEX_TOOLS:
        return {}
    if event.get("tool_name") in CODEX_RESULT_TOOL_NAMES | CODEX_CANCEL_TOOL_NAMES:
        return _async_post_followup(event, config=config, state_root=state_root,
                                    repo_root=repo_root, reviewer=reviewer, failed=True)
    del config, reviewer
    if event.get("tool_name") in CODEX_START_TOOL_NAMES:
        try:
            root = _state_root(state_root)
            try:
                call_uuid = _read_pending(root, event).get("async_call")
            except WiringError:
                tool_input = event.get("tool_input")
                call_uuid = tool_input.get("call") if isinstance(tool_input, Mapping) else None
            if isinstance(call_uuid, str):
                _cancel_start_job(root, call_uuid,
                                  hashlib.sha256(call_uuid.encode("utf-8")).hexdigest())
        except Exception:
            pass
    runtime_root: Path | None = None
    pending: Mapping[str, Any] | None = None
    try:
        runtime_root = _state_root(state_root)
        try:
            _cleanup_stale_pending(runtime_root, event)
        except BaseException:
            pass
        pending = _read_pending(runtime_root, event)
        project_namespace = _pending_project_key(pending)
        project_runtime_root = _project_state_root(runtime_root, project_namespace)
        evidence = {
            "ambiguous_outcome": True,
            "work_performed": "UNKNOWN",
            "error": event.get("error"),
            "is_interrupt": event.get("is_interrupt"),
            "duration_ms": event.get("duration_ms"),
            "pending_call": pending,
        }
        evidence_path = _persist_raw_output(runtime_root, event, evidence, "-failure")
        routing_enforced = pending.get("kind") != "pretool_bypass"
        outcome = {
            "status": "AMBIGUOUS_TOOL_FAILURE",
            "ambiguous_outcome": True,
            "work_performed": "UNKNOWN",
            "routing_enforced": routing_enforced,
            "evidence_path": str(evidence_path),
            "claude_review_mode": policy.FULL,
        }
        identity_fields = ("task_id", "route", "model", "effort")
        if all(isinstance(pending.get(field), str) and pending.get(field) for field in identity_fields):
            saved = ledger.SessionLedger(project_runtime_root).save(
                ledger.build_session_record(
                    pending["task_id"],
                    pending.get("prior_thread_id"),
                    pending["route"],
                    pending["model"],
                    pending["effort"],
                    agent=pending.get("agent", "codex"),
                    phase=pending.get("phase"),
                    scope=pending.get("scope", []),
                    reason=pending.get("reason"),
                    pending_call_baseline=None,
                    last_outcome=outcome,
                    label=pending.get("label"),
                )
            )
            if saved["status"] != ledger.OK:
                raise WiringError(saved.get("message", "ambiguous failure could not be recorded"))
        elif routing_enforced:
            raise WiringError("normal pending call identity is incomplete")
        if pending.get("mechanical_opencode") is True and pending.get("agent") == "opencode":
            try:
                import mechanical_learning
                positions = mechanical_learning.record_sample(
                    str(pending.get("model")), False, root=runtime_root
                )
                if positions and positions[0] != positions[1]:
                    _observe("emit", "MECHANICAL_MODEL_RANK_CHANGED", state_root=runtime_root,
                             project_key=project_namespace, served_model=pending.get("model"),
                             old_rank=positions[0], new_rank=positions[1])
            except BaseException:
                pass
        if not pending.get("async_call"):
            _release_scope_lease(runtime_root, pending)
            _remove_pending(runtime_root, event)
        elif event.get("tool_name") in CODEX_START_TOOL_NAMES:
            if _start_job_terminal_or_absent(runtime_root, pending):
                _release_scope_lease(runtime_root, pending)
                _remove_pending(runtime_root, event)
            else:
                pending = dict(pending, start_rejected=True)
                _atomic_json_write(_pending_path(runtime_root, event), pending)
        if event.get("tool_name") in CODEX_START_TOOL_NAMES and re.fullmatch(r"[0-9a-f]{64}", str(pending.get("job_id"))):
            (runtime_root / "async_jobs" / f"{pending['job_id']}.json").unlink(missing_ok=True)
        routing_note = (
            " Routing was NOT enforced by PreToolUse."
            if not routing_enforced
            else ""
        )
        failure_code = (
            "CODEX_CALL_INTERRUPTED" if event.get("is_interrupt") is True else "CODEX_CALL_FAILED"
        )
        _observe(
            "emit",
            "IMPLEMENTER_FAILED",
            task_id=pending.get("task_id"),
            state_root=runtime_root,
            **_telemetry_envelope(
                event, pending.get("repo_root"), project_namespace
            ),
            failure_code=failure_code,
            duration_ms=_numeric_duration_ms(event.get("duration_ms")),
            eta_minutes=pending.get("eta_minutes"),
            reason=pending.get("reason"),
            reason_source=pending.get("reason_source"),
            previous_route=pending.get("previous_route"),
            new_route=pending.get("new_route"),
        )
        _observe(
            "update_task",
            pending.get("task_id"),
            state_root=runtime_root,
            project_key=project_namespace,
        )
        return {
            "hookSpecificOutput": {
                "hookEventName": "PostToolUseFailure",
                "additionalContext": (
                    "Codex failed with an ambiguous outcome; work_performed is UNKNOWN, "
                    f"FULL Claude review is required, and evidence is at {evidence_path}."
                    f"{routing_note}"
                ),
            }
        }
    except Exception as error:
        if runtime_root is not None and isinstance(pending, Mapping):
            _observe(
                "emit",
                "IMPLEMENTER_FAILED",
                task_id=pending.get("task_id"),
                state_root=runtime_root,
                **_telemetry_envelope(
                    event,
                    pending.get("repo_root"),
                    _pending_project_key(pending),
                ),
                failure_code=(
                    "CODEX_CALL_INTERRUPTED"
                    if event.get("is_interrupt") is True
                    else "CODEX_CALL_FAILED"
                ),
                duration_ms=_numeric_duration_ms(event.get("duration_ms")),
                eta_minutes=pending.get("eta_minutes"),
                reason=pending.get("reason"),
                reason_source=pending.get("reason_source"),
                previous_route=pending.get("previous_route"),
                new_route=pending.get("new_route"),
            )
            _observe(
                "update_task",
                pending.get("task_id"),
                state_root=runtime_root,
                project_key=_pending_project_key(pending),
            )
        _record_diagnostic(runtime_root, event, "PostToolUseFailure", error)
        return {
            "hookSpecificOutput": {
                "hookEventName": "PostToolUseFailure",
                "additionalContext": (
                    "Codex failed with an ambiguous outcome; work_performed is UNKNOWN and "
                    f"FULL Claude review is required. Evidence cleanup error: {_safe_error(error)}"
                ),
            }
        }


def stop(
    event: Mapping[str, Any],
    *,
    state_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Finalize Claude Desktop task token telemetry at Stop boundary."""
    try:
        if event.get("stop_hook_active") is True:
            return {}
        session_id = event.get("session_id")
        if not session_id:
            return {}
        runtime_root = _state_root(state_root)
        try:
            import claude_transcript_usage
        except ImportError:
            _observe("record_health", "DEGRADED", error_code="CLAUDE_TRANSCRIPT_PARSER_UNAVAILABLE", state_root=runtime_root)
            return {}
        try:
            claude_transcript_usage.finalize_stop_task(
                runtime_root,
                session_id=session_id,
                transcript_path=event.get("transcript_path"),
            )
        except Exception:
            _observe("record_health", "DEGRADED", error_code="CLAUDE_TRANSCRIPT_FINALIZATION_FAILED", state_root=runtime_root)
    except Exception:
        pass
    return {}


__all__ = [
    "CODEX_REPLY_TOOL",
    "CODEX_TOOL",
    "CODEX_TOOLS",
    "IMPLEMENTER_REPORT_LIMIT",
    "INDEPENDENCE_INSTRUCTIONS",
    "MICRO_REPAIR",
    "REVIEWER",
    "internal_pre_error_response",
    "internal_post_error_response",
    "post_tool_use",
    "post_tool_use_failure",
    "pre_tool_use",
    "stop",
]
