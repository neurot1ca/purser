"""Closed, metadata-only schema for orchestrator telemetry."""

from __future__ import annotations

from collections.abc import Mapping
from datetime import datetime, timezone
import hashlib
import math
import re
from typing import Any
import uuid


SCHEMA_VERSION = 1
EVENT_TYPES = frozenset(
    {
        "TASK_ROUTE_SELECTED",
        "IMPLEMENTER_STARTED",
        "IMPLEMENTER_FINISHED",
        "IMPLEMENTER_FAILED",
        "MECHANICAL_MODEL_RANK_CHANGED",
        "REVIEWER1_STARTED",
        "REVIEWER1_FINISHED",
        "COMPACTION_ADVICE_OUTCOME",
        "STOP_BLOCKED",
        "SHADOW_FINISHED",
        "SCOPE_CONFLICT_OUTCOME",
        "REVIEWER1_SKIPPED",
        "REVIEWER1_FAILED",
        "ROUTING_PROTOCOL_VIOLATION",
        "THREAD_CONTINUITY_VIOLATION",
        "UNKNOWN_CONCURRENT_WRITER",
        "WORKFLOW_VERDICT_RECORDED",
        "TELEMETRY_WARNING",
        "CLAUDE_TASK_USAGE_RECORDED",
        "CONTEXT_IO_TURN_RECORDED",
        "CODEX_RESULT_UNFETCHED",
        "CLAUDE_DECISION_RECORDED",
        "MECHANICAL_EDIT_OBSERVED",
        "MAINTENANCE_REPORT_FINISHED",
        "MAINTENANCE_REPORT_ACTED",
    }
)

TERMINAL_VERDICTS = frozenset({"VERIFIED", "NOT_VERIFIED", "INCONCLUSIVE", "NO_CHANGE"})
TASK_STATUSES = frozenset({"IN_PROGRESS", *TERMINAL_VERDICTS, "UNKNOWN_OR_IN_PROGRESS"})
TOKEN_FIELDS = frozenset(
    {
        "input_tokens",
        "output_tokens",
        "total_tokens",
        "prompt_tokens",
        "completion_tokens",
        "cached_tokens",
        "cache_read_tokens",
        "cache_write_tokens",
        "thinking_tokens",
    }
)
CLAUDE_USAGE_PAYLOAD_FIELDS = frozenset(
    {
        "claude_usage_session_id",
        "claude_usage_captured_at",
        "claude_5h_start_pct",
        "claude_5h_end_pct",
        "claude_5h_resets_at",
        "claude_7d_start_pct",
        "claude_7d_end_pct",
        "claude_7d_resets_at",
        "claude_context_used_percentage",
        "claude_context_window_size",
        "claude_context_total_tokens",
        "claude_context_input_tokens",
        "claude_context_output_tokens",
        "claude_cache_creation_input_tokens",
        "claude_cache_read_input_tokens",
        "claude_session_estimated_cost_usd",
        "claude_session_total_duration_ms",
        "claude_usage_known",
        "claude_rate_limit_delta_scope",
    }
)
CLAUDE_TASK_USAGE_PAYLOAD_FIELDS = frozenset(
    {
        "claude_task_usage_source",
        "claude_task_usage_known",
        "claude_task_usage_complete",
        "claude_task_input_tokens",
        "claude_task_output_tokens",
        "claude_task_cache_creation_input_tokens",
        "claude_task_cache_read_input_tokens",
        "claude_task_total_observed_tokens",
        "claude_task_request_count",
        "claude_task_usage_boundary",
        "claude_task_usage_dedup",
        "claude_task_usage_unknown_reason",
        "claude_task_subagent_usage_included",
    }
)
CONTEXT_IO_THRESHOLDS = (200, 350, 500)
CONTEXT_IO_BOOLEAN_FIELDS = frozenset({"repeat_known", "task_association_known"})
CONTEXT_IO_INTEGER_FIELDS = frozenset(
    {
        "read_count",
        "full_read_count",
        "targeted_read_count",
        "unknown_read_count",
        "observed_bytes_total",
        "exact_line_read_count",
        "exact_line_count_total",
        "lower_bound_line_read_count",
        "lower_bound_line_count_total",
        "oversized_read_count",
        "binary_suspected_read_count",
        "repeat_count",
        "pre_codex_read_count",
        "post_codex_read_count",
        "hook_sample_count",
        "internal_processing_us_total",
        "internal_processing_us_max",
        "hook_wall_time_us_total",
        "hook_wall_time_us_max",
        *(f"threshold_{threshold}_{suffix}" for threshold in CONTEXT_IO_THRESHOLDS for suffix in ("known_count", "candidate_count")),
    }
)
CONTEXT_IO_PAYLOAD_FIELDS = frozenset(
    {*CONTEXT_IO_BOOLEAN_FIELDS, *CONTEXT_IO_INTEGER_FIELDS, "unknown_reason"}
)
CONTEXT_IO_UNKNOWN_REASONS = frozenset(
    {
        "BINARY_SUSPECTED",
        "BOUNDED_SCAN",
        "CORRUPT_STATE",
        "DIRECTORY",
        "INVALID_INPUT",
        "JOURNAL_LIMIT",
        "MISSING_FILE",
        "MISSING_USER_PROMPT",
        "OBSERVATION_LIMIT",
        "OUTSIDE_ROOT",
        "PRIOR_TURN_MISSING_STOP",
        "PROTECTED_PATH",
        "RESOURCE_LIMIT",
        "STALE_STATE",
        "STATE_CONTENTION",
        "STAT_UNAVAILABLE",
    }
)

_IDENTIFIER = re.compile(r"[^A-Za-z0-9._-]")
_SAFE_IDENTIFIER = re.compile(r"[A-Za-z0-9._-]{1,128}\Z")
_PROJECT_KEY = re.compile(r"(?:[0-9a-f]{16}|unresolved)\Z")
_WINDOWS_RESERVED = frozenset(
    {"CON", "PRN", "AUX", "NUL", *(f"COM{i}" for i in range(1, 10)), *(f"LPT{i}" for i in range(1, 10))}
)
_ROUTES = frozenset({"mechanical", "subtle", "normal", "consequential", "escalated", "operation", "plan_standard"})
_PHASES = frozenset({"implementation", "operation", "plan"})
CLAUDE_TASK_TYPES = frozenset({"implementation", "operation", "plan", "unknown"})
DERIVED_TASK_TYPES = frozenset({"counting_status", "operation", "unknown"})
_EFFORTS = frozenset({"low", "medium", "high", "xhigh"})
_VALIDATIONS = frozenset({"PASS", "FAIL", "UNKNOWN"})
_REVIEW_RESULTS = frozenset(
    {
        "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
        "CHANGES_REQUIRED",
        "UNCERTAIN",
        "REVIEWER1_UNAVAILABLE",
    }
)
_SKIP_REASONS = frozenset(
    {"PHASE_OPERATION", "NO_DELTA", "BELOW_THRESHOLD", "REVIEWER_UNAVAILABLE"}
)
_FAILURE_CODES = frozenset(
    {
        "CODEX_CALL_FAILED",
        "CODEX_CALL_INTERRUPTED",
        "REVIEWER1_UNAVAILABLE",
        "REVIEWER1_INCOMPLETE",
    }
)
_WARNING_CODES = frozenset({"MALFORMED_VERDICT", "MALFORMED_SPOOL_EVENT", "CLAUDE_DECISION_CONFLICT"})
_VIOLATION_CODES = frozenset(
    {
        "MALFORMED_TOOL_INPUT",
        "MISSING_ORCH_LINE",
        "MALFORMED_ORCH_LINE",
        "UNKNOWN_KEY",
        "MISSING_REQUIRED_KEY",
        "INVALID_ENUM_VALUE",
        "INVALID_SCOPE",
        "VAGUE_SOL_BASIS",
        "UNBOUNDED_SCOPE",
        "CODEX_REPLY_INPUT_INVALID",
        "THREAD_ID_MISMATCH",
        "TASK_ID_MISMATCH",
        "ROUTE_MISMATCH",
        "MODEL_MISMATCH",
        "EFFORT_MISMATCH",
        "THREAD_TASK_MISMATCH",
        "THREAD_PHASE_MISMATCH",
        "THREAD_ROUTE_MISMATCH",
        "USE_CODEX_REPLY",
        "NO_LIVE_THREAD",
        "INVALID_LIVE_THREAD",
        "INVALID_REQUEST",
        "SCOPE_LEASE_CONFLICT",
        "LEASE_ADMISSION_UNCERTAIN",
        "LEASE_OWNER_CONFLICT",
        "LEASE_INVALID",
    }
)

_COMMON_CALL_FIELDS = frozenset(
    {
        "phase", "route", "model", "model_source", "effort", "routing_enforced", "fresh_thread", "reply_call",
        "reason", "reason_source", "previous_route", "new_route", "eta_minutes",
        "task_type", "suspected_route_mismatch", "agent", "provider",
    }
)
_CALL_CONTEXT_FIELDS = frozenset({"reason", "reason_source", "previous_route", "new_route", "eta_minutes"})
_CLAUDE_TIMESTAMP_FIELDS = frozenset(
    {"claude_usage_captured_at", "claude_5h_resets_at", "claude_7d_resets_at"}
)
_PAYLOAD_FIELDS = {
    "TASK_ROUTE_SELECTED": _COMMON_CALL_FIELDS,
    "IMPLEMENTER_STARTED": (
        _COMMON_CALL_FIELDS | CLAUDE_USAGE_PAYLOAD_FIELDS | frozenset({"implementer"})
    ),
    "IMPLEMENTER_FINISHED": frozenset(
        {
            "duration_ms",
            "implementer",
            "served_model",
            "served_route",
            "served_effort",
            "served_agent",
            "served_provider",
            "route",
            "phase",
            "task_type",
            "has_code_map",
            "suspected_route_mismatch",
            "changed_paths_count",
            "known_concurrent_scopes",
            "validation",
            "worker_validation_status",
            "thread_id",
            "claude_review_mode",
            "routing_enforced",
            "codex_usage_known",
            *_CALL_CONTEXT_FIELDS,
            *(f"codex_{name}" for name in TOKEN_FIELDS),
            *CLAUDE_USAGE_PAYLOAD_FIELDS,
        }
    ),
    "IMPLEMENTER_FAILED": (
        frozenset({"failure_code", "duration_ms"})
        | _CALL_CONTEXT_FIELDS
        | CLAUDE_USAGE_PAYLOAD_FIELDS
    ),
    "MECHANICAL_MODEL_RANK_CHANGED": frozenset({"served_model", "old_rank", "new_rank"}),
    "REVIEWER1_STARTED": frozenset({"reviewer1_provider", "reviewer1_model_requested"}),
    "REVIEWER1_FINISHED": frozenset(
        {
            "reviewer1_provider",
            "reviewer1_model_requested",
            "served_model_attestation",
            "reviewer1_result",
            "reviewer1_initial_finding_count",
            "duration_ms",
            "gemini_usage_known",
            "reviewer1_retry_count",
            "reviewer1_retry_recovered",
            *(f"gemini_{name}" for name in TOKEN_FIELDS),
        }
    ),
    "REVIEWER1_SKIPPED": frozenset(
        {"reviewer1_skipped_reason", "reviewer1_model_requested", "served_model_attestation"}
    ),
    "COMPACTION_ADVICE_OUTCOME": frozenset({
        "notice_id", "advice_written", "compaction_observed", "pre_context_tokens",
        "post_context_tokens", "elapsed_turns",
    }),
    "STOP_BLOCKED": frozenset({"reason_kinds", "reason_count"}),
    "SHADOW_FINISHED": frozenset({
        "candidate", "status", "agreement", "elapsed_ms", "input_tokens", "output_tokens", "total_tokens",
    }),
    "SCOPE_CONFLICT_OUTCOME": frozenset({"action", "job_id", "queue_wait_ms"}),
    "REVIEWER1_FAILED": frozenset(
        {
            "failure_code",
            "reviewer1_error_code",
            "reviewer1_retry_count",
            "reviewer1_retry_recovered",
            "reviewer1_provider",
            "reviewer1_model_requested",
            "served_model_attestation",
            "reviewer1_result",
            "duration_ms",
            "gemini_usage_known",
            *(f"gemini_{name}" for name in TOKEN_FIELDS),
        }
    ),
    "ROUTING_PROTOCOL_VIOLATION": frozenset({"violation_code"}),
    "THREAD_CONTINUITY_VIOLATION": frozenset({"violation_code"}),
    "UNKNOWN_CONCURRENT_WRITER": frozenset({"concurrent_writer", "code", "halt_paths", "halt_path_count"}),
    "WORKFLOW_VERDICT_RECORDED": frozenset({"verdict"}),
    "TELEMETRY_WARNING": frozenset({"warning_code"}),
    "CLAUDE_TASK_USAGE_RECORDED": CLAUDE_TASK_USAGE_PAYLOAD_FIELDS,
    "CONTEXT_IO_TURN_RECORDED": CONTEXT_IO_PAYLOAD_FIELDS,
    "CODEX_RESULT_UNFETCHED": frozenset({"route", "code"}),
    "CLAUDE_DECISION_RECORDED": frozenset({"decision", "task_type", "source"}),
    "MECHANICAL_EDIT_OBSERVED": frozenset(
        {"thread_id", "classification", "fingerprint", "changed_file_count", "hunk_count"}
    ),
    "MAINTENANCE_REPORT_FINISHED": frozenset({
        "report_id", "report_type", "outcome", "reason_code", "elapsed_seconds",
        "quota_change_active_window_pct", "findings_high_count", "findings_medium_count",
        "findings_low_count", "line_coverage_pct",
    }),
    "MAINTENANCE_REPORT_ACTED": frozenset({"report_id", "findings_marked_done", "findings_listed"}),
}


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def normalize_identifier(value: Any, *, fallback: str = "unknown") -> str:
    """Return a bounded filename-safe identifier, hashing whenever it changed."""

    original = value if isinstance(value, str) else ""
    normalized = _IDENTIFIER.sub("_", original).strip(". ")
    normalized = normalized[:96]
    changed = normalized != original or not normalized
    if normalized.split(".", 1)[0].upper() in _WINDOWS_RESERVED:
        changed = True
        normalized = f"_{normalized}"
    if not normalized:
        normalized = fallback
    if changed:
        suffix = hashlib.sha256(original.encode("utf-8")).hexdigest()[:8]
        normalized = f"{normalized[:87]}-{suffix}"
    return normalized


def _number(value: Any, *, integer: bool = False) -> int | float:
    expected = int if integer else (int, float)
    if not isinstance(value, expected) or isinstance(value, bool) or not math.isfinite(value) or value < 0:
        raise ValueError("numeric telemetry field is invalid")
    return int(value) if integer else value


def _identifier(value: Any) -> str:
    if not isinstance(value, str) or not _SAFE_IDENTIFIER.fullmatch(value):
        raise ValueError("telemetry identifier is invalid")
    return value


def _project_key(value: Any, *, required: bool) -> str | None:
    if value is None and not required:
        return None
    if not isinstance(value, str) or not _PROJECT_KEY.fullmatch(value):
        raise ValueError("telemetry project key is invalid")
    return value


def _payload_value(name: str, value: Any) -> Any:
    if name == "reason_kinds":
        kinds = {"maintenance", "receipt", "dispatch", "compaction", "version", "leader", "fallback", "flow"}
        if (not isinstance(value, list) or len(value) > len(kinds)
                or any(not isinstance(kind, str) or kind not in kinds for kind in value)
                or value != sorted(set(value))):
            raise ValueError("telemetry Stop reason kinds are invalid")
        return value
    if name == "reason_count":
        count = _number(value, integer=True)
        if count < 1 or count > 20:
            raise ValueError("telemetry Stop reason count is invalid")
        return count
    if name in {"report_id", "reason_code"}:
        return _identifier(value)
    if name == "report_type" and value in {"cleanup", "test-gaps"}:
        return value
    if name == "outcome" and value in {"completed", "yielded", "failed"}:
        return value
    if name in {"elapsed_seconds", "findings_high_count", "findings_medium_count",
                "findings_low_count", "findings_marked_done", "findings_listed"}:
        return _number(value, integer=name not in {"elapsed_seconds"})
    if name in {"quota_change_active_window_pct", "line_coverage_pct"}:
        if (isinstance(value, (int, float)) and not isinstance(value, bool)
                and math.isfinite(value)
                and (name == "quota_change_active_window_pct" or 0 <= value <= 100)):
            return value
        raise ValueError("maintenance telemetry percentage is invalid")
    if name == "decision" and isinstance(value, str) and value in {"accepted", "send_back", "rejected"}:
        return value
    if name == "classification" and value in {"rename", "unclassified"}:
        return value
    if name == "fingerprint":
        if not isinstance(value, str) or re.fullmatch(r"[0-9a-f]{64}", value) is None:
            raise ValueError("mechanical fingerprint is invalid")
        return value
    if name == "task_type" and isinstance(value, str) and value in CLAUDE_TASK_TYPES | DERIVED_TASK_TYPES:
        return value
    if name == "suspected_route_mismatch":
        if value is None:
            return None
        if (not isinstance(value, Mapping) or set(value) != {"declared_route", "derived_type"}
                or value.get("declared_route") not in _ROUTES
                or value.get("derived_type") not in {"counting_status", "operation"}):
            raise ValueError("suspected route mismatch is invalid")
        return {"declared_route": value["declared_route"], "derived_type": value["derived_type"]}
    if name == "source":
        if (not isinstance(value, Mapping) or set(value) != {"kind", "job_id"}
                or not isinstance(value.get("kind"), str)
                or value["kind"] not in {"apply", "reply", "discard", "result", "redispatch"}):
            raise ValueError("telemetry decision source is invalid")
        return {"kind": value["kind"], "job_id": _identifier(value["job_id"])}
    if name == "known_concurrent_scopes":
        if not isinstance(value, list) or any(
            not isinstance(item, dict) or set(item) != {"path", "task_id"}
            or not all(isinstance(item[key], str) and item[key] for key in ("path", "task_id"))
            for item in value
        ):
            raise ValueError("known concurrent scopes are invalid")
        return value
    if name == "eta_minutes":
        if value is None or (isinstance(value, int) and not isinstance(value, bool) and 1 <= value <= 600):
            return value
        raise ValueError("telemetry eta_minutes is invalid")
    if name == "reviewer1_retry_count":
        if isinstance(value, int) and not isinstance(value, bool) and value in (0, 1):
            return value
        raise ValueError("telemetry reviewer retry count is invalid")
    if name == "reviewer1_initial_finding_count":
        return _number(value, integer=True)
    if name in {"advice_written", "compaction_observed"}:
        if not isinstance(value, bool):
            raise ValueError("telemetry compaction outcome boolean is invalid")
        return value
    if name in {"pre_context_tokens", "post_context_tokens", "elapsed_turns"}:
        return _number(value, integer=True)
    if name in {"notice_id", "candidate", "job_id"}:
        return _identifier(value)
    if name == "status" and value in {"completed", "failed"}:
        return value
    if name == "agreement":
        if isinstance(value, bool):
            return value
        raise ValueError("telemetry shadow agreement is invalid")
    if name in {"input_tokens", "output_tokens", "total_tokens"}:
        return _number(value, integer=True)
    if name == "action" and value in {"denied", "queued", "promoted", "applied", "discarded"}:
        return value
    if name in {"elapsed_ms", "queue_wait_ms"}:
        return _number(value)
    if name in {
        "routing_enforced",
        "fresh_thread",
        "reply_call",
        "codex_usage_known",
        "has_code_map",
        "gemini_usage_known",
        "reviewer1_retry_recovered",
        "claude_usage_known",
        "concurrent_writer",
        "claude_task_usage_known",
        "claude_task_usage_complete",
        "claude_task_subagent_usage_included",
        *CONTEXT_IO_BOOLEAN_FIELDS,
    }:
        if not isinstance(value, bool):
            raise ValueError("telemetry boolean is invalid")
        return value
    if name in {
        "changed_paths_count",
        "changed_file_count",
        "hunk_count",
        "old_rank",
        "new_rank",
        "halt_path_count",
        "claude_task_input_tokens",
        "claude_task_output_tokens",
        "claude_task_cache_creation_input_tokens",
        "claude_task_cache_read_input_tokens",
        "claude_task_total_observed_tokens",
        "claude_task_request_count",
        *CONTEXT_IO_INTEGER_FIELDS,
    }:
        return _number(value, integer=True)
    if name in {
        *(f"codex_{field}" for field in TOKEN_FIELDS),
        *(f"gemini_{field}" for field in TOKEN_FIELDS),
        *(CLAUDE_USAGE_PAYLOAD_FIELDS - _CLAUDE_TIMESTAMP_FIELDS - {"claude_usage_session_id", "claude_rate_limit_delta_scope", "claude_usage_known"}),
    }:
        return _number(value)
    if name in _CLAUDE_TIMESTAMP_FIELDS:
        if not isinstance(value, str) or len(value) > 40:
            raise ValueError("telemetry timestamp is invalid")
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError as exc:
            raise ValueError("telemetry timestamp is invalid") from exc
        if parsed.utcoffset() != timezone.utc.utcoffset(parsed):
            raise ValueError("telemetry timestamp is not UTC")
        return value
    if name == "claude_usage_session_id":
        return _identifier(value)
    if name == "claude_rate_limit_delta_scope" and value == "account_window_observation":
        return value
    if name == "claude_task_usage_source" and value == "desktop_jsonl_observed":
        return value
    if name == "claude_task_usage_boundary" and value == "anchored_pre_tool_to_stop":
        return value
    if name == "claude_task_usage_dedup" and value == "message_id_request_id":
        return value
    if name == "claude_task_usage_unknown_reason":
        return _identifier(value)
    if name == "unknown_reason" and value in CONTEXT_IO_UNKNOWN_REASONS:
        return value
    if name == "duration_ms":
        return _number(value)
    if name == "code":
        return _identifier(value)
    if name == "halt_paths":
        if not isinstance(value, list) or len(value) > 20 or any(
            not isinstance(path, str) for path in value
        ):
            raise ValueError("telemetry halt paths are invalid")
        return value
    if name == "phase" and value in _PHASES:
        return value
    if name in {"route", "previous_route", "new_route", "served_route"} and value in _ROUTES:
        return value
    if name == "reason" and value in {
        "new_task", "continue", "fix", "route_change", "reanchor", "repeated_failure", "user_override"
    }:
        return value
    if name == "reason_source" and value in {"explicit", "inherited"}:
        return value
    if name == "model_source" and value in {"override", "route"}:
        return value
    if name == "effort" and value in _EFFORTS | {"none"}:
        return value
    if name == "served_effort" and value in _EFFORTS | {"none"}:
        return value
    if name == "validation" and value in _VALIDATIONS:
        return value
    if name == "worker_validation_status" and value in {"PASS", "FAIL", "UNKNOWN"}:
        return value
    if name == "claude_review_mode" and value in {"AUDIT", "FULL"}:
        return value
    if name == "reviewer1_result" and value in _REVIEW_RESULTS:
        return value
    if name == "reviewer1_skipped_reason" and value in _SKIP_REASONS:
        return value
    if name == "failure_code" and value in _FAILURE_CODES:
        return value
    if name == "reviewer1_error_code":
        if (
            not isinstance(value, str)
            or not 1 <= len(value) <= 80
            or re.fullmatch(r"[A-Z0-9_]+", value) is None
        ):
            raise ValueError("telemetry reviewer error code is invalid")
        return value
    if name == "warning_code" and value in _WARNING_CODES:
        return value
    if name == "violation_code" and value in _VIOLATION_CODES:
        return value
    if name == "verdict" and value in TERMINAL_VERDICTS:
        return value
    if name == "served_model_attestation" and value == "UNAVAILABLE":
        return value
    if name in {"thread_id", "implementer", "reviewer1_provider", "reviewer1_model_requested",
                "agent", "provider", "served_agent", "served_provider"}:
        return _identifier(value)
    if name in {"model", "served_model"} and isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9._/-]{1,128}", value):
        return value
    raise ValueError("telemetry enum is invalid")


def build_event(
    event_type: str,
    *,
    task_id: Any = None,
    project_key: Any = None,
    session_id: Any = None,
    repo: Any = None,
    branch: Any = None,
    agent_id: Any = None,
    agent_type: Any = None,
    payload: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    if event_type not in EVENT_TYPES:
        raise ValueError("unknown telemetry event type")
    supplied = dict(payload or {})
    if not set(supplied).issubset(_PAYLOAD_FIELDS[event_type]):
        raise ValueError("telemetry payload contains an unregistered field")
    clean_payload = {
        name: _payload_value(name, value)
        for name, value in supplied.items()
        if value is not None or name == "eta_minutes"
    }
    return {
        "schema_version": SCHEMA_VERSION,
        "event_id": str(uuid.uuid4()),
        "ts_utc": utc_now(),
        "event_type": event_type,
        "task_id": normalize_identifier(task_id) if task_id is not None else None,
        "project_key": (
            _project_key(project_key, required=True)
            if project_key is not None
            else "unresolved" if task_id is not None else None
        ),
        "session_id": normalize_identifier(session_id, fallback="session") if session_id is not None else None,
        "repo": normalize_identifier(repo, fallback="repo") if repo is not None else None,
        "branch": normalize_identifier(branch, fallback="branch") if branch is not None else None,
        "agent_id": normalize_identifier(agent_id, fallback="agent") if agent_id is not None else None,
        "agent_type": normalize_identifier(agent_type, fallback="agent") if agent_type is not None else None,
        "payload": clean_payload,
    }


def valid_event(value: Any) -> bool:
    if not isinstance(value, Mapping) or set(value) != {
        "schema_version", "event_id", "ts_utc", "event_type", "task_id", "project_key",
        "session_id", "repo", "branch", "agent_id", "agent_type", "payload"
    }:
        return False
    try:
        uuid.UUID(str(value["event_id"]))
        timestamp = value["ts_utc"]
        if not isinstance(timestamp, str) or len(timestamp) > 40:
            return False
        parsed_timestamp = datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
        if parsed_timestamp.utcoffset() != timezone.utc.utcoffset(parsed_timestamp):
            return False
        rebuilt = build_event(
            value["event_type"],
            task_id=value["task_id"],
            project_key=value["project_key"],
            session_id=value["session_id"],
            repo=value["repo"],
            branch=value["branch"],
            agent_id=value["agent_id"],
            agent_type=value["agent_type"],
            payload=value["payload"],
        )
    except (TypeError, ValueError):
        return False
    envelope_fields = ("task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type", "payload")
    return value["schema_version"] == SCHEMA_VERSION and all(
        rebuilt[field] == value[field] for field in envelope_fields
    )


__all__ = [
    "CLAUDE_USAGE_PAYLOAD_FIELDS", "CONTEXT_IO_PAYLOAD_FIELDS", "CONTEXT_IO_UNKNOWN_REASONS", "EVENT_TYPES", "SCHEMA_VERSION", "TASK_STATUSES", "TERMINAL_VERDICTS", "TOKEN_FIELDS",
    "build_event", "normalize_identifier", "utc_now", "valid_event",
]
