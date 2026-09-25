"""Claude Code hook wiring for the codex-main MCP tools."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from datetime import datetime, timezone
import hashlib
import inspect
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any

import json_transport
import codex_quota
import checks
import ledger
import policy
import project_profile
import project_root
import reviewer as reviewer1
import telemetry
import telemetry_schema


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
CODEX_TOOLS = CODEX_TOOL_NAMES | CODEX_REPLY_TOOL_NAMES
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


def _append_worker_validation_instruction(prompt: str) -> str:
    """Append the worker-report contract once, preserving the caller's prompt text."""

    if _WORKER_VALIDATION_INSTRUCTION in prompt:
        return prompt
    separator = "" if not prompt or prompt.endswith("\n") else "\n"
    return f"{prompt}{separator}{_WORKER_VALIDATION_INSTRUCTION}"


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
        loaded = json.loads(_CONFIG_PATH.read_text(encoding="utf-8"))
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
    local_app_data = os.environ.get("LOCALAPPDATA")
    if not local_app_data:
        raise WiringError("LOCALAPPDATA is unavailable; inject a runtime state root")
    return Path(local_app_data) / "claude-agent-orchestrator"


def _repo_root(
    event: Mapping[str, Any],
    root: str | os.PathLike[str] | None,
) -> tuple[Path | None, str]:
    return project_root.resolve_project_root(event, root)


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
        scope=tuple(scope),
    )


def internal_pre_error_response(
    event: Mapping[str, Any],
    error: BaseException,
    *,
    state_root: str | os.PathLike[str] | None = None,
) -> dict[str, Any]:
    """Allow an internally failed PreToolUse call while making the bypass visible."""

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
    runtime_root: Path | None = None
    pending_written = False
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
        parsed = policy.parse_orch_prompt(tool_input.get("prompt"), loaded_config)
        reply_without_orch = (
            tool_name in CODEX_REPLY_TOOL_NAMES
            and parsed.code == policy.MISSING_ORCH_LINE
            and isinstance(tool_input.get("prompt"), str)
        )
        if not parsed.valid and not reply_without_orch:
            if parsed.code == policy.CONFIG_INVALID:
                raise WiringError(parsed.message)
            return _observed_denial(
                event, parsed.code, parsed.message, state_root=state_root, task_id=parsed.task_id
            )

        if (parsed.valid and tool_name in CODEX_TOOL_NAMES
                and parsed.reason != "user_override"):
            try:
                quota = codex_quota.read_latest_quota()
            except Exception:
                quota = None
            low_windows = []
            if isinstance(quota, Mapping):
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
                    "with reason=user_override in the ORCH line.",
                )

        runtime_root = _state_root(state_root)
        try:
            _cleanup_stale_pending(runtime_root, event)
        except BaseException:
            pass
        _call_key(event)
        indexed_root = None
        if tool_name in CODEX_REPLY_TOOL_NAMES:
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
        project_namespace = project_root.project_key(repository)
        session_ledger = ledger.SessionLedger(
            _project_state_root(runtime_root, project_namespace)
        )
        record: Mapping[str, Any] | None
        live_thread: Mapping[str, Any] | None
        if tool_name in CODEX_REPLY_TOOL_NAMES:
            forbidden = {"model", "config", "developer-instructions"}.intersection(tool_input)
            if forbidden:
                return _observed_denial(
                    event,
                    "CODEX_REPLY_INPUT_INVALID",
                    "codex-reply accepts only threadId and prompt; use a fresh thread to change routing.",
                    state_root=state_root,
                    task_id=parsed.task_id,
                )
        if reply_without_orch:
            supplied_thread_id = tool_input.get("threadId")
            recorded = session_ledger.find_by_thread_id(supplied_thread_id)
            if recorded is None:
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    "No recoverable ledger record exists for this codex-reply thread.",
                    state_root=state_root,
                )
            candidate_thread = recorded.get("live_thread")
            if not isinstance(candidate_thread, Mapping):
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    "The codex-reply thread record has no recoverable live thread context.",
                    state_root=state_root,
                )
            inherited = _inherited_reply_parse(tool_input.get("prompt"), candidate_thread)
            if inherited is None:
                return _observed_denial(
                    event,
                    "NO_LIVE_THREAD",
                    "The codex-reply thread record lacks recoverable phase or scope context.",
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
            loaded = session_ledger.load(parsed.task_id)
            if loaded["status"] == ledger.CORRUPT:
                raise WiringError("session ledger is unreadable")
            record = loaded.get("record") if loaded["status"] == ledger.OK else None
            live_thread = record.get("live_thread") if isinstance(record, Mapping) else None

        route = policy.resolve_route(parsed.route, loaded_config)
        if not route.valid:
            if route.code == policy.CONFIG_INVALID:
                raise WiringError(route.message)
            return _observed_denial(
                event, route.code, route.message, state_root=state_root, task_id=parsed.task_id
            )
        requested = _requested_thread(parsed, route)
        bypass_metadata.update(
            task_id=parsed.task_id,
            phase=parsed.phase,
            route=parsed.route,
            model=route.model,
            effort=route.effort,
            scope=list(parsed.scope),
        )

        if tool_name in CODEX_TOOL_NAMES:
            decision = policy.fresh_codex_decision(requested, live_thread)
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
                return _observed_denial(
                    event,
                    compatibility.code,
                    compatibility.message,
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
        updated["prompt"] = parsed.prompt
        if parsed.phase == "implementation":
            updated["prompt"] = _append_worker_validation_instruction(updated["prompt"])
        if tool_name in CODEX_TOOL_NAMES:
            updated["model"] = route.model
            original_codex_config = tool_input.get("config", {})
            if not isinstance(original_codex_config, Mapping):
                raise WiringError("tool_input.config must be an object when supplied")
            updated_config = dict(original_codex_config)
            updated_config["model_reasoning_effort"] = route.effort
            updated["config"] = updated_config
            if "developer-instructions" not in tool_input:
                updated["developer-instructions"] = INDEPENDENCE_INSTRUCTIONS

        resolved_profile = (
            project_profile.load_profile(repository) if repository is not None else None
        )
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
            baseline = ledger.capture_baseline(
                repository,
                declared_scope if declared_scope else None,
                manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
            )
        if (
            tool_name in CODEX_REPLY_TOOL_NAMES
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
            "model": route.model,
            "effort": route.effort,
            "scope": declared_scope,
            "repo_root": str(repository) if repository is not None else None,
            "project_root_reason": project_root_reason or None,
            "baseline": baseline,
            "prior_thread_id": continuation_thread_id,
            "task_objective": task_objective,
            "acceptance_criteria": acceptance_criteria,
            "task_contract_grounded": task_contract_grounded,
        }
        if resolved_profile is not None:
            inherited_checks = (
                live_thread.get("profile_checks")
                if tool_name in CODEX_REPLY_TOOL_NAMES and isinstance(live_thread, Mapping)
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
        _atomic_json_write(_pending_path(runtime_root, event), pending)
        pending_written = True

        session_record = ledger.build_session_record(
            parsed.task_id,
            pending["prior_thread_id"],
            parsed.route,
            route.model,
            route.effort,
            phase=parsed.phase,
            scope=declared_scope,
            pending_call_baseline=baseline,
            last_outcome=record.get("last_outcome") if isinstance(record, Mapping) else None,
            task_objective=task_objective,
            acceptance_criteria=acceptance_criteria,
            task_contract_grounded=task_contract_grounded,
            profile_checks=pending.get("profile_checks"),
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
            "model": route.model,
            "effort": route.effort,
            "routing_enforced": True,
            "fresh_thread": tool_name in CODEX_TOOL_NAMES,
            "reply_call": tool_name in CODEX_REPLY_TOOL_NAMES,
            **_telemetry_envelope(event, repository, project_namespace),
        }
        _observe("emit", "TASK_ROUTE_SELECTED", **observation)
        _observe("emit", "IMPLEMENTER_STARTED", **observation)

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
            model_value = updated.get("model") if "model" in updated else route.model
            model_name = model_value if isinstance(model_value, str) and model_value else "unknown"
            updated_config = updated.get("config")
            effort_value = (
                updated_config.get("model_reasoning_effort")
                if isinstance(updated_config, Mapping) and "model_reasoning_effort" in updated_config
                else route.effort
            )
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
        if pending_written and runtime_root is not None:
            try:
                _remove_pending(runtime_root, event)
            except Exception:
                pass
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


def _extract_codex_result(tool_response: Any) -> tuple[str, str]:
    if isinstance(tool_response, str):
        texts = [tool_response]
    else:
        blocks = tool_response
        if isinstance(blocks, Mapping) and isinstance(blocks.get("content"), list):
            blocks = blocks["content"]
        if not isinstance(blocks, list):
            raise WiringError("Codex MCP output is not a content-block array")
        texts = [
            block["text"]
            for block in blocks
            if isinstance(block, Mapping)
            and block.get("type") == "text"
            and isinstance(block.get("text"), str)
        ]
    for text in texts:
        try:
            decoded = json.loads(text)
        except json.JSONDecodeError:
            continue
        if not isinstance(decoded, Mapping):
            continue
        thread_id = decoded.get("threadId")
        content = decoded.get("content")
        if isinstance(thread_id, str) and thread_id:
            return thread_id, content if isinstance(content, str) else ""
    raise WiringError("thread id could not be recovered from Codex MCP text content")


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
        "implementer_report": bundle.get("implementer_report", ""),
    }
    return mandatory, _non_report_bytes(mandatory) > cap


def _finalize_replacement(
    bundle: dict[str, Any],
    config: Mapping[str, Any],
    runtime_root: Path,
    event: Mapping[str, Any],
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
        return _finalize_replacement(bundle, loaded_config, runtime_root, event)
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
        "changed_paths_count": bundle.get("delta_files"),
        "validation": bundle.get("validation"),
        "thread_id": (
            telemetry_schema.normalize_identifier(bundle.get("thread_id"), fallback="thread")
            if bundle.get("thread_id") is not None
            else None
        ),
        "claude_review_mode": bundle.get("claude_review_mode"),
        "routing_enforced": bundle.get("routing_enforced"),
        "codex_usage_known": bool(codex_tokens),
    }
    finished_payload.update({f"codex_{key}": value for key, value in codex_tokens.items()})
    _observe("emit", "IMPLEMENTER_FINISHED", task_id=task_id, **finished_payload)

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

    if event.get("tool_name") not in CODEX_TOOLS:
        return {}
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
        project_namespace = _pending_project_key(pending)
        project_runtime_root = _project_state_root(runtime_root, project_namespace)
        raw_path = _persist_raw_output(runtime_root, event, event.get("tool_response"))

        bundle = _base_bundle()
        bundle.update(
            {
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
            try:
                bundle["codex_quota"] = codex_quota.read_quota(thread_id)
            except Exception:
                bundle["codex_quota"] = None
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
                        phase=pending.get("phase"),
                        scope=pending.get("scope", []),
                        pending_call_baseline=None,
                        last_outcome=bundle,
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
            return _finalize_replacement(bundle, loaded_config, runtime_root, event)

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
        )
        if stored_profile:
            protected_paths = pending["protected_paths"]
            profile_force_full = pending["force_full_review"]
            profile_skip_max_lines = pending["skip_max_lines"]
            profile_skip_max_files = pending["skip_max_files"]
            profile_reviewer_executable = pending.get("reviewer_executable")
            profile_reviewer = pending.get("reviewer", "agy")
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
        scope = pending.get("scope")
        if not isinstance(scope, list) or (pending.get("phase") == "implementation" and not scope):
            raise WiringError("pending call scope is missing")
        baseline = pending.get("baseline")
        if not isinstance(baseline, Mapping):
            raise WiringError("pending call baseline is missing")
        if repository is None or baseline.get("status") != ledger.OK:
            delta = {
                "status": ledger.UNKNOWN,
                "code": baseline.get("code", "BASELINE_UNAVAILABLE"),
                "message": baseline.get("message", "The baseline is unavailable."),
                "baseline_head": baseline.get("baseline_head"),
                "current_head": None,
                "changed_paths": None,
            }
        else:
            delta = ledger.compute_delta(
                repository,
                baseline,
                scope if scope else None,
                manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
            )
        bundle["baseline_head"] = delta.get("baseline_head")
        bundle["current_head"] = delta.get("current_head")
        force_full_review = delta.get("status") != ledger.OK

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

        if (
            pending.get("phase") == "implementation"
            and scope
            and repository is not None
            and baseline.get("status") == ledger.OK
        ):
            out_of_scope_res = ledger.detect_out_of_scope_modifications(
                repository,
                baseline,
                scope,
            )
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
                if selected:
                    def run_selected_checks() -> None:
                        try:
                            check_state["result"] = checks.run_checks(
                                repository, selected, deadline_seconds=check_deadline
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
            if profile_reviewer == "none":
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
                reviewer_started = True
                _observe(
                    "emit",
                    "REVIEWER1_STARTED",
                    task_id=pending["task_id"],
                    state_root=runtime_root,
                    **_telemetry_envelope(event, repository, project_namespace),
                    reviewer1_provider="agy",
                    reviewer1_model_requested=loaded_config["reviewer1_model_requested"],
                )
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
                    "repo_root": repository,
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

                reviewer_report = reviewer_callable(**reviewer_kwargs)
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
                if loaded_config.get("micro_repair_enabled", True) and result == policy.CHANGES_REQUIRED and thread_id:
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
                            repo_root=repository,
                            scope=scope,
                            timeout_seconds=actual_timeout,
                        )
                        # Universal delta recomputation (C19-1): always recompute after attempt
                        re_delta = ledger.compute_delta(
                            repository,
                            baseline,
                            scope if scope else None,
                            manifest_budget_seconds=loaded_config["manifest_budget_seconds"],
                        )
                        if re_delta.get("status") == ledger.OK:
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
                            out_of_scope_res = ledger.detect_out_of_scope_modifications(
                                repository,
                                baseline,
                                scope,
                            )
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
                if val_status == "FAIL":
                    bundle["findings"].append(
                        _finding(
                            "WORKER_VALIDATION_FAILED",
                            f"Implementer reported validation failure: {worker_validation['summary'][:300]}",
                        )
                    )
                elif val_status == "UNKNOWN":
                    bundle["findings"].append(
                        _finding(
                            "WORKER_VALIDATION_UNVERIFIED",
                            f"Implementer validation was unverified: {worker_validation['summary'][:300]}",
                        )
                    )

                if not is_grounded:
                    bundle["findings"].append(
                        _finding(
                            "TASK_CONTRACT_NOT_GROUNDED",
                            "Task contract is not grounded (missing objective or criteria); forcing FULL Claude review.",
                        )
                    )

        if profile_forces_full:
            bundle["claude_review_mode"] = policy.FULL
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
            phase=pending.get("phase"),
            scope=pending.get("scope", []),
            pending_call_baseline=None,
            last_outcome=bundle,
            task_objective=pending.get("task_objective"),
            acceptance_criteria=pending.get("acceptance_criteria"),
            task_contract_grounded=pending.get("task_contract_grounded", False),
            profile_checks=pending.get("profile_checks"),
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
            _remove_pending(runtime_root, event)
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
        return _finalize_replacement(bundle, loaded_config, runtime_root, event)
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
    reviewer: Callable[..., Mapping[str, Any]] | None = None,
) -> dict[str, Any]:
    """Archive ambiguous failure evidence without running Reviewer-1."""

    del config, reviewer
    if event.get("tool_name") not in CODEX_TOOLS:
        return {}
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
                    phase=pending.get("phase"),
                    scope=pending.get("scope", []),
                    pending_call_baseline=None,
                    last_outcome=outcome,
                )
            )
            if saved["status"] != ledger.OK:
                raise WiringError(saved.get("message", "ambiguous failure could not be recorded"))
        elif routing_enforced:
            raise WiringError("normal pending call identity is incomplete")
        _remove_pending(runtime_root, event)
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
