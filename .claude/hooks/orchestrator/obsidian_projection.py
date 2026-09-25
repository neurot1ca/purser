"""Deterministic, local-only projection of telemetry aggregates into Markdown."""

from __future__ import annotations

from collections.abc import Iterable, Mapping
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import re
from datetime import timedelta
from typing import Any
import uuid
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import telemetry_schema as schema
import state_root as purser_state_root


DEFAULT_VAULT_ROOT = Path(r"C:\AI-Operations")
LOCAL_TIMEZONE = "Europe/Istanbul"
PROJECTION_SCHEMA_VERSION = 1
SAMPLE_SIZE = 30
MIN_RATE_DENOMINATOR = 2
LOCAL_TIMEZONE_FALLBACK_OFFSET_HOURS = 3
BATCHING_BASELINE = 1.08
BATCHING_TARGET = 1.5
BATCHING_SNAPSHOT_MAX_AGE = timedelta(hours=6)


def _resolve_zone() -> tuple[Any, str]:
    """Resolve the projection's display zone without ever failing at import.

    A host whose interpreter carries no tz database - Windows Python without the
    `tzdata` package - raised ZoneInfoNotFoundError here at module import time.
    Because `codex` imports `telemetry`, which imports this module, that single
    missing optional dependency killed the whole enforcement layer before
    `hook.py` reached its own error handling. Observability must degrade, never
    take enforcement down with it, so an unavailable zone falls back to the fixed
    offset and the projection records which one it used.
    """

    try:
        return ZoneInfo(LOCAL_TIMEZONE), LOCAL_TIMEZONE
    except (ZoneInfoNotFoundError, ValueError, OSError):
        offset = timezone(timedelta(hours=LOCAL_TIMEZONE_FALLBACK_OFFSET_HOURS))
        return offset, f"UTC+{LOCAL_TIMEZONE_FALLBACK_OFFSET_HOURS:02d}:00_FIXED_OFFSET_FALLBACK"


_ZONE, LOCAL_TIMEZONE_RESOLVED = _resolve_zone()
_CONFIG_PATH = Path(__file__).resolve().parent / "config.json"
_MARKER_PREFIX = "<!-- orchestrator projection v1 sha256="
_YAML_MARKER_PREFIX = "# orchestrator projection v1 sha256="
_MARKER_RE = re.compile(
    r"(?m)^(?:<!-- orchestrator projection v1 sha256=([0-9a-f]{64}) -->|# orchestrator projection v1 sha256=([0-9a-f]{64}))\n"
)
_SAFE_IDENTIFIER_RE = re.compile(r"[A-Za-z0-9._-]{1,128}\Z")

_ROUTES = frozenset({"mechanical", "subtle", "normal", "consequential", "escalated", "operation"})
_PHASES = frozenset({"implementation", "operation"})
_EFFORTS = frozenset({"low", "medium", "high", "xhigh"})
_VALIDATIONS = frozenset({"PASS", "FAIL", "UNKNOWN"})
_CLAUDE_MODES = frozenset({"AUDIT", "FULL"})
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

_USAGE_KEYS = (
    "codex_call_count",
    "codex_duration_ms",
    "codex_usage_known",
    "gemini_duration_ms",
    "gemini_usage_known",
    "deepseek_usage_known",
    "claude_usage_session_id",
    "claude_usage_captured_at",
    "claude_5h_start_pct",
    "claude_5h_end_pct",
    "claude_5h_delta_pct",
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
    *(f"{provider}_{field}" for provider in ("codex", "gemini", "deepseek") for field in sorted(schema.TOKEN_FIELDS)),
)
_INTERNAL_USAGE_KEYS = (
    "_claude_5h_start_session_id",
    "_claude_5h_start_captured_at",
    "_claude_5h_start_resets_at",
    "_claude_5h_end_session_id",
    "_claude_5h_end_captured_at",
    "_claude_5h_end_resets_at",
)
_USAGE_KEY_SET = frozenset((*_USAGE_KEYS, *_INTERNAL_USAGE_KEYS))
_CONTEXT_IO_KEYS = (
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
    "threshold_200_known_count",
    "threshold_200_candidate_count",
    "threshold_350_known_count",
    "threshold_350_candidate_count",
    "threshold_500_known_count",
    "threshold_500_candidate_count",
    "repeat_count",
    "repeat_known",
    "pre_codex_read_count",
    "post_codex_read_count",
    "task_association_known",
    "hook_sample_count",
    "internal_processing_us_total",
    "internal_processing_us_max",
    "hook_wall_time_us_total",
    "hook_wall_time_us_max",
    "unknown_reason",
)
_TOKEN_TOTAL_KEYS = {
    provider: f"{provider}_total_tokens" for provider in ("codex", "gemini", "deepseek")
}
_OPTIONAL_BOOLEAN_FIELDS = (
    "first_pass",
    "first_pass_success",
    "repair",
    "repair_attempted",
    "repaired",
    "accepted",
    "blocked",
)
_OPTIONAL_COUNT_FIELDS = ("repair_count",)


class ProjectionError(ValueError):
    """An aggregate or projection target is outside the closed contract."""


class ProjectionConflict(ProjectionError):
    """An existing file is not an intact projection owned by this module."""


def _state_root(state_root: str | os.PathLike[str] | None) -> Path:
    if state_root is not None:
        return Path(state_root)
    return purser_state_root.default_state_root()


def _vault_root(vault_root: str | os.PathLike[str] | None) -> Path:
    if vault_root is not None:
        return Path(vault_root)
    try:
        config = json.loads(_CONFIG_PATH.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError):
        return DEFAULT_VAULT_ROOT
    if not isinstance(config, Mapping):
        return DEFAULT_VAULT_ROOT
    setting = config.get("obsidian_projection")
    configured_root = setting.get("vault_root") if isinstance(setting, Mapping) else None
    if configured_root is None:
        configured_root = config.get("obsidian_vault_root")
    if configured_root is None:
        return DEFAULT_VAULT_ROOT
    if not isinstance(configured_root, str) or not configured_root.strip():
        raise ProjectionError("obsidian projection vault_root is invalid")
    return Path(configured_root)


def _identifier(name: str, value: Any, *, nullable: bool = True) -> str | None:
    if value is None and nullable:
        return None
    if not isinstance(value, str) or not _SAFE_IDENTIFIER_RE.fullmatch(value):
        raise ProjectionError(f"{name} is not a sanitized identifier")
    if schema.normalize_identifier(value) != value:
        raise ProjectionError(f"{name} is not normalized by telemetry_schema")
    return value


def _enum(name: str, value: Any, allowed: frozenset[str], *, nullable: bool = True) -> str | None:
    if value is None and nullable:
        return None
    if not isinstance(value, str) or value not in allowed:
        raise ProjectionError(f"{name} is outside its controlled enum")
    return value


def _number(name: str, value: Any, *, integer: bool = False, nullable: bool = True) -> int | float | None:
    if value is None and nullable:
        return None
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise ProjectionError(f"{name} is not numeric")
    if not math.isfinite(value) or value < 0:
        raise ProjectionError(f"{name} is not a finite non-negative number")
    if integer and not isinstance(value, int):
        raise ProjectionError(f"{name} is not an integer")
    return value


def _boolean(name: str, value: Any, *, nullable: bool = True) -> bool | None:
    if value is None and nullable:
        return None
    if not isinstance(value, bool):
        raise ProjectionError(f"{name} is not boolean")
    return value


def _utc_timestamp(name: str, value: Any, *, nullable: bool = True) -> tuple[str | None, datetime | None]:
    if value is None and nullable:
        return None, None
    if not isinstance(value, str) or len(value) > 40 or "\n" in value or "\r" in value:
        raise ProjectionError(f"{name} is not a UTC timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ProjectionError(f"{name} is not a UTC timestamp") from exc
    if parsed.utcoffset() != timezone.utc.utcoffset(parsed):
        raise ProjectionError(f"{name} is not a UTC timestamp")
    return value, parsed


def _usage_value(name: str, value: Any) -> Any:
    if value is None:
        return None
    if name.endswith("_usage_known") or name in {
        "claude_task_usage_complete",
        "claude_task_subagent_usage_included",
    }:
        return _boolean(name, value, nullable=False)
    if name == "claude_rate_limit_delta_scope":
        if value != "account_window_observation":
            raise ProjectionError(f"{name} is outside its controlled enum")
        return value
    if name == "claude_task_usage_source":
        if value != "desktop_jsonl_observed":
            raise ProjectionError(f"{name} is outside its controlled enum")
        return value
    if name == "claude_task_usage_boundary":
        if value != "anchored_pre_tool_to_stop":
            raise ProjectionError(f"{name} is outside its controlled enum")
        return value
    if name == "claude_task_usage_dedup":
        if value != "message_id_request_id":
            raise ProjectionError(f"{name} is outside its controlled enum")
        return value
    if name == "claude_task_usage_unknown_reason":
        return _identifier(name, value, nullable=False)
    if "session_id" in name:
        return _identifier(name, value, nullable=False)
    if "captured_at" in name or "resets_at" in name:
        return _utc_timestamp(name, value, nullable=False)[0]
    is_int = name.startswith("claude_task_") and (name.endswith("_tokens") or name.endswith("_count"))
    return _number(name, value, integer=is_int, nullable=False)


def _prepare_aggregate(aggregate: Mapping[str, Any]) -> dict[str, Any]:
    if not isinstance(aggregate, Mapping):
        raise ProjectionError("aggregate is not an object")
    task_id = _identifier("task_id", aggregate.get("task_id"), nullable=False)
    started_at, started_dt = _utc_timestamp("started_at", aggregate.get("started_at"))
    ended_at, ended_dt = _utc_timestamp("ended_at", aggregate.get("ended_at"))
    local_started = started_dt.astimezone(_ZONE).isoformat() if started_dt is not None else None
    local_ended = ended_dt.astimezone(_ZONE).isoformat() if ended_dt is not None else None

    status = _enum("status", aggregate.get("status"), schema.TASK_STATUSES)
    phase = _enum("phase", aggregate.get("phase"), _PHASES)
    route = _enum("route", aggregate.get("route"), _ROUTES)
    effort = _enum("effort", aggregate.get("effort"), _EFFORTS)
    validation = _enum("validation", aggregate.get("validation"), _VALIDATIONS)
    claude_review_mode = _enum("claude_review_mode", aggregate.get("claude_review_mode"), _CLAUDE_MODES)
    reviewer_result = _enum("reviewer1_result", aggregate.get("reviewer1_result"), _REVIEW_RESULTS)
    skip_reason = _enum("reviewer1_skipped_reason", aggregate.get("reviewer1_skipped_reason"), _SKIP_REASONS)
    attestation = aggregate.get("served_model_attestation")
    if attestation is not None and attestation != "UNAVAILABLE":
        raise ProjectionError("served_model_attestation is outside its controlled enum")

    usage = aggregate.get("usage")
    if usage is None:
        usage = {}
    if not isinstance(usage, Mapping):
        raise ProjectionError("usage is not an object")
    unknown_usage_keys = set(usage) - _USAGE_KEY_SET
    if unknown_usage_keys:
        raise ProjectionError("usage contains an unregistered field")
    prepared_usage = {key: _usage_value(key, usage.get(key)) for key in _USAGE_KEYS}
    context_value = aggregate.get("context_io")
    prepared_context: dict[str, Any] | None = None
    if context_value is not None:
        if not isinstance(context_value, Mapping) or not set(context_value).issubset(_CONTEXT_IO_KEYS):
            raise ProjectionError("context_io contains an unregistered field")
        prepared_context = {}
        for key in _CONTEXT_IO_KEYS:
            value = context_value.get(key)
            if key in {"repeat_known", "task_association_known"}:
                prepared_context[key] = _boolean(key, value) if value is not None else None
            elif key == "unknown_reason":
                if value is not None and value not in schema.CONTEXT_IO_UNKNOWN_REASONS:
                    raise ProjectionError("context_io unknown_reason is outside its controlled enum")
                prepared_context[key] = value
            else:
                prepared_context[key] = _number(key, value, integer=True) if value is not None else None

    prepared: dict[str, Any] = {
        "type": "orch-run",
        "schema_version": 1,
        "projection_schema_version": PROJECTION_SCHEMA_VERSION,
        "task_id": task_id,
        "repo": _identifier("repo", aggregate.get("repo")),
        "branch": _identifier("branch", aggregate.get("branch")),
        "timezone": LOCAL_TIMEZONE,
        "started_at": local_started,
        "started_at_utc": started_at,
        "ended_at": local_ended,
        "ended_at_utc": ended_at,
        "status": status,
        "phase": phase,
        "route": route,
        "model": _identifier("model", aggregate.get("model")),
        "effort": effort,
        "fresh_threads": _number("fresh_threads", aggregate.get("fresh_threads"), integer=True),
        "reply_calls": _number("reply_calls", aggregate.get("reply_calls"), integer=True),
        "routing_enforced": _boolean("routing_enforced", aggregate.get("routing_enforced")),
        "validation": validation,
        "duration_ms": _number("duration_ms", aggregate.get("duration_ms")),
        "thread_id": _identifier("thread_id", aggregate.get("thread_id")),
        "reviewer1_provider": _identifier("reviewer1_provider", aggregate.get("reviewer1_provider")),
        "reviewer1_model_requested": _identifier(
            "reviewer1_model_requested", aggregate.get("reviewer1_model_requested")
        ),
        "reviewer1_result": reviewer_result,
        "reviewer1_skipped_reason": skip_reason,
        "served_model_attestation": attestation,
        "claude_review_mode": claude_review_mode,
        "changed_paths_count": _number(
            "changed_paths_count", aggregate.get("changed_paths_count"), integer=True
        ),
        "routing_protocol_violations": _number(
            "routing_protocol_violations", aggregate.get("routing_protocol_violations"), integer=True
        ),
        "thread_continuity_violations": _number(
            "thread_continuity_violations", aggregate.get("thread_continuity_violations"), integer=True
        ),
        "concurrent_writer": _boolean("concurrent_writer", aggregate.get("concurrent_writer")),
        "policy_fingerprint": _identifier("policy_fingerprint", aggregate.get("policy_fingerprint")),
        "usage": prepared_usage,
        "context_io": prepared_context,
        "_started_dt": started_dt,
        "_ended_dt": ended_dt,
    }
    for key in (*_OPTIONAL_BOOLEAN_FIELDS, *_OPTIONAL_COUNT_FIELDS):
        if key in aggregate:
            prepared[key] = (
                _boolean(key, aggregate[key], nullable=False)
                if key in _OPTIONAL_BOOLEAN_FIELDS
                else _number(key, aggregate[key], integer=True, nullable=False)
            )
    return prepared


def _yaml_scalar(value: Any) -> str:
    if value is None:
        return "null"
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, int) and not isinstance(value, bool):
        return str(value)
    if isinstance(value, float):
        if not math.isfinite(value):
            raise ProjectionError("non-finite number cannot be rendered")
        return json.dumps(value, ensure_ascii=False, allow_nan=False)
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    raise ProjectionError("frontmatter value is not controlled")


def _frontmatter(prepared: Mapping[str, Any]) -> str:
    keys = (
        "type",
        "schema_version",
        "projection_schema_version",
        "task_id",
        "repo",
        "branch",
        "timezone",
        "started_at",
        "started_at_utc",
        "ended_at",
        "ended_at_utc",
        "status",
        "phase",
        "route",
        "model",
        "effort",
        "fresh_threads",
        "reply_calls",
        "routing_enforced",
        "validation",
        "duration_ms",
        "thread_id",
        "reviewer1_provider",
        "reviewer1_model_requested",
        "reviewer1_result",
        "reviewer1_skipped_reason",
        "served_model_attestation",
        "claude_review_mode",
        "changed_paths_count",
        "routing_protocol_violations",
        "thread_continuity_violations",
        "concurrent_writer",
        "policy_fingerprint",
        *_OPTIONAL_BOOLEAN_FIELDS,
        *_OPTIONAL_COUNT_FIELDS,
    )
    lines = ["---"]
    for key in keys:
        if key in prepared:
            lines.append(f"{key}: {_yaml_scalar(prepared[key])}")
    lines.append("usage:")
    usage = prepared["usage"]
    for key in _USAGE_KEYS:
        lines.append(f"  {key}: {_yaml_scalar(usage[key])}")
    if prepared["context_io"] is not None:
        lines.append("context_io:")
        for key in _CONTEXT_IO_KEYS:
            lines.append(f"  {key}: {_yaml_scalar(prepared['context_io'][key])}")
    lines.extend(("---", ""))
    return "\n".join(lines)


def _display(value: Any) -> str:
    return "unknown" if value is None else str(value)


def _format_duration_ms(value: int | float | None) -> str:
    if value is None:
        return "unknown"
    seconds = value / 1000
    if seconds < 60:
        return f"{seconds:.1f}s"
    minutes, remaining_seconds = divmod(seconds, 60)
    return f"{int(minutes)}m {remaining_seconds:.1f}s"


def render_run_note(aggregate: Mapping[str, Any]) -> str:
    """Render one run note without reading raw events or invoking external code."""

    prepared = _prepare_aggregate(aggregate)
    return _frontmatter(prepared) + _run_body(prepared)


def _marker_for(content_without_marker: str, *, yaml_comment: bool = False) -> str:
    digest = hashlib.sha256(content_without_marker.encode("utf-8")).hexdigest()
    return f"{_YAML_MARKER_PREFIX if yaml_comment else _MARKER_PREFIX}{digest}{'' if yaml_comment else ' -->'}\n"


def _with_marker(prefix: str, body: str, *, yaml_comment: bool = False) -> str:
    return prefix + _marker_for(prefix + body, yaml_comment=yaml_comment) + body


def _generated_content_is_intact(content: str) -> bool:
    match = _MARKER_RE.search(content)
    if match is None:
        return False
    without_marker = content[: match.start()] + content[match.end() :]
    marker_digest = match.group(1) or match.group(2)
    return hashlib.sha256(without_marker.encode("utf-8")).hexdigest() == marker_digest


def _atomic_write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        try:
            existing = path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as exc:
            raise ProjectionConflict(f"cannot inspect existing projection: {path}") from exc
        if not _generated_content_is_intact(existing):
            raise ProjectionConflict(f"refusing to overwrite non-projection note: {path}")
    temporary: Path | None = None
    try:
        temporary = path.parent / f".{path.name}.{uuid.uuid4().hex}.tmp"
        descriptor = os.open(
            os.fspath(temporary),
            os.O_WRONLY | os.O_CREAT | os.O_EXCL,
            0o600,
        )
        try:
            handle = os.fdopen(descriptor, mode="w", encoding="utf-8", newline="\n")
        except BaseException:
            os.close(descriptor)
            raise
        with handle:
            handle.write(content)
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


def _safe_run_path(vault_root: Path, prepared: Mapping[str, Any]) -> Path:
    anchor = prepared["_started_dt"] or prepared["_ended_dt"]
    if anchor is None:
        year, month = "0000", "00"
    else:
        local = anchor.astimezone(_ZONE)
        year, month = f"{local.year:04d}", f"{local.month:02d}"
    runs_root = vault_root / "Runs"
    candidate = runs_root / year / month / f"{prepared['task_id']}.md"
    try:
        resolved_vault = vault_root.resolve()
        resolved_runs = runs_root.resolve()
        resolved_runs.relative_to(resolved_vault)
        resolved_candidate = candidate.resolve()
        resolved_candidate.relative_to(resolved_vault)
        resolved_candidate.relative_to(resolved_runs)
    except ValueError as exc:
        raise ProjectionError("run note path escaped the Runs tree") from exc
    return candidate


def _safe_vault_path(vault_root: Path, *parts: str) -> Path:
    candidate = vault_root.joinpath(*parts)
    try:
        candidate.resolve().relative_to(vault_root.resolve())
    except ValueError as exc:
        raise ProjectionError("projection path escaped the vault root") from exc
    return candidate


def _read_task_aggregate(task_id: Any, state_root: str | os.PathLike[str] | None) -> Mapping[str, Any]:
    normalized = schema.normalize_identifier(task_id)
    path = _state_root(state_root) / "telemetry" / "tasks" / f"{normalized}.json"
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProjectionError(f"task aggregate cannot be read: {normalized}") from exc
    if not isinstance(value, Mapping) or value.get("task_id") != normalized:
        raise ProjectionError("task aggregate identity is invalid")
    return value


def project_task(
    aggregate_or_task_id: Mapping[str, Any] | str | os.PathLike[str],
    *,
    state_root: str | os.PathLike[str] | None = None,
    vault_root: str | os.PathLike[str] | None = None,
) -> Path:
    """Project one aggregate, or load one by task id, into its run note."""

    aggregate = (
        aggregate_or_task_id
        if isinstance(aggregate_or_task_id, Mapping)
        else _read_task_aggregate(aggregate_or_task_id, state_root)
    )
    prepared = _prepare_aggregate(aggregate)
    path = _safe_run_path(_vault_root(vault_root), prepared)
    _atomic_write(path, _with_marker(_frontmatter(prepared), _run_body(prepared)))
    return path


def project_health(
    *,
    state_root: str | os.PathLike[str] | None = None,
    vault_root: str | os.PathLike[str] | None = None,
    last_projection_write_at: str | None = None,
) -> Path:
    """Write the reader-facing health note from the current telemetry state."""

    vault = _vault_root(vault_root)
    aggregates = _read_aggregates(state_root)
    path = _safe_vault_path(vault, "System", "Telemetry Health.md")
    content = _with_marker(
        "",
        render_health(
            aggregates,
            state_root=state_root,
            last_projection_write_at=last_projection_write_at,
        ),
    )
    _atomic_write(path, content)
    return path


def _run_body(prepared: Mapping[str, Any]) -> str:
    task_id = prepared["task_id"]
    flow = (
        f"{_display(prepared['route'])} / {_display(prepared['model'])} "
        f"({_display(prepared['effort'])}), phase={_display(prepared['phase'])}; "
        f"fresh_threads={_display(prepared['fresh_threads'])}, reply_calls={_display(prepared['reply_calls'])}"
    )
    review = prepared["reviewer1_result"]
    if prepared["reviewer1_skipped_reason"] is not None:
        review = f"SKIPPED ({prepared['reviewer1_skipped_reason']})"
    elif review is None:
        review = "unknown"
    return (
        f"# Run: {task_id}\n\n"
        f"Flow: {flow}\n"
        f"Duration: {_format_duration_ms(prepared['duration_ms'])}\n"
        f"Validation: {_display(prepared['validation'])}\n"
        f"Review: {_display(review)}\n\n"
        "## Notes\n\n"
        f"- Status: {_display(prepared['status'])}\n"
        f"- Concurrent writer: {_display(prepared['concurrent_writer'])}\n"
        f"- Projection source: telemetry aggregate only; local timezone: {LOCAL_TIMEZONE}.\n"
    )


def _fmt_number(value: int | float) -> str:
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


def _rate(numerator: int, denominator: int) -> str:
    if denominator < MIN_RATE_DENOMINATOR:
        return f"unknown (n={denominator}; minimum n={MIN_RATE_DENOMINATOR})"
    return f"{100.0 * numerator / denominator:.1f}% (n={denominator})"


def _indicator_rate(sample: list[Mapping[str, Any]], keys: tuple[str, ...], label: str) -> str:
    selected = next((key for key in keys if any(key in run for run in sample)), None)
    if selected is None:
        return f"{label}: unknown (n=0; field not recorded; minimum n={MIN_RATE_DENOMINATOR})"
    values = [run[selected] for run in sample if isinstance(run.get(selected), bool)]
    if not values:
        return f"{label}: unknown (n=0; minimum n={MIN_RATE_DENOMINATOR})"
    return f"{label}: {_rate(sum(values), len(values))}"


def _distribution(sample: list[Mapping[str, Any]], key: str, categories: tuple[str, ...]) -> list[str]:
    counts = {category: 0 for category in categories}
    unknown = 0
    for run in sample:
        value = run.get(key)
        if value in counts:
            counts[value] += 1
        else:
            unknown += 1
    return [f"{category}={counts[category]}" for category in categories] + [f"unknown={unknown}"]


def _metric_block(label: str, sample: list[Mapping[str, Any]]) -> str:
    lines = [f"## {label}", "", f"Eligible aggregates: n={len(sample)}"]
    for field, title in (("accepted", "Accepted count"), ("blocked", "Blocked count")):
        values = [run[field] for run in sample if isinstance(run.get(field), bool)]
        lines.append(
            f"{title}: unknown (field not recorded)"
            if not values
            else f"{title}: {sum(values)} (n={len(values)})"
        )

    status_counts = {status: 0 for status in sorted(schema.TERMINAL_VERDICTS)}
    for run in sample:
        if run["status"] in status_counts:
            status_counts[run["status"]] += 1
    lines.append("Workflow verdicts: " + ", ".join(f"{key}={status_counts[key]}" for key in status_counts))
    lines.extend(("", "### Rates", "", "Rate threshold: minimum denominator n=2; smaller denominators are unknown."))
    lines.append(_indicator_rate(sample, ("first_pass", "first_pass_success"), "First-pass rate"))
    lines.append(_indicator_rate(sample, ("repair", "repair_attempted", "repaired"), "Repair rate"))

    known_models = [run["model"] for run in sample if run.get("model") is not None]
    model_counts: dict[str, int] = {}
    for model in known_models:
        model_counts[model] = model_counts.get(model, 0) + 1
    if not model_counts:
        lines.append(f"Model share: unknown (n=0; field not recorded; minimum n={MIN_RATE_DENOMINATOR})")
    else:
        lines.append("Model share: " + ", ".join(
            f"{model}={_rate(count, len(known_models))}" for model, count in sorted(model_counts.items())
        ))

    calls = [
        (run["fresh_threads"], run["reply_calls"])
        for run in sample
        if isinstance(run.get("fresh_threads"), int)
        and isinstance(run.get("reply_calls"), int)
        and run["fresh_threads"] + run["reply_calls"] > 0
    ]
    fresh = sum(pair[0] for pair in calls)
    replies = sum(pair[1] for pair in calls)
    lines.append(f"Fresh-thread share: {_rate(fresh, fresh + replies)}")
    lines.append(f"Reply share: {_rate(replies, fresh + replies)}")

    for key, title in (
        ("routing_protocol_violations", "Routing violation rate"),
        ("thread_continuity_violations", "Thread-continuity violation rate"),
    ):
        values = [run[key] for run in sample if isinstance(run.get(key), int)]
        lines.append(f"{title}: unknown (n=0; minimum n={MIN_RATE_DENOMINATOR})" if not values else
                     f"{title}: {_rate(sum(value > 0 for value in values), len(values))}")

    reviewer_counts = {category: 0 for category in (
        "SKIPPED",
        "UNAVAILABLE",
        "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
        "CHANGES_REQUIRED",
        "UNCERTAIN",
    )}
    reviewer_unknown = 0
    for run in sample:
        if run.get("reviewer1_skipped_reason") is not None:
            reviewer_counts["SKIPPED"] += 1
        elif run.get("reviewer1_result") == "REVIEWER1_UNAVAILABLE":
            reviewer_counts["UNAVAILABLE"] += 1
        elif run.get("reviewer1_result") in reviewer_counts:
            reviewer_counts[run["reviewer1_result"]] += 1
        else:
            reviewer_unknown += 1
    lines.append("Reviewer result distribution: " + ", ".join(
        f"{category}={reviewer_counts[category]}" for category in reviewer_counts
    ) + f", unknown={reviewer_unknown}")
    lines.append("Claude review mode distribution: " + ", ".join(
        _distribution(sample, "claude_review_mode", tuple(sorted(_CLAUDE_MODES)))
    ))

    durations = sorted(run["duration_ms"] for run in sample if isinstance(run.get("duration_ms"), (int, float)))
    if not durations:
        lines.append("Duration median: unknown (n=0)")
        lines.append("Duration p90: unknown (n=0)")
    else:
        middle = len(durations) // 2
        median = durations[middle] if len(durations) % 2 else (durations[middle - 1] + durations[middle]) / 2
        p90 = durations[max(0, math.ceil(len(durations) * 0.9) - 1)]
        lines.append(f"Duration median: {_format_duration_ms(median)} (n={len(durations)})")
        lines.append(f"Duration p90: {_format_duration_ms(p90)} (n={len(durations)})")
    return "\n".join(lines) + "\n"


def _eligible_aggregates(aggregates: Iterable[Mapping[str, Any]]) -> list[dict[str, Any]]:
    prepared = [_prepare_aggregate(aggregate) for aggregate in aggregates]
    eligible = [
        run
        for run in prepared
        if run["status"] in schema.TERMINAL_VERDICTS and run["_ended_dt"] is not None
    ]
    eligible.sort(key=lambda run: (run["_ended_dt"], run["task_id"]))
    return eligible[-SAMPLE_SIZE:]


def read_batching_snapshot(
    path: str | os.PathLike[str] | None = None,
) -> Mapping[str, Any] | None:
    """Read the audit-produced snapshot, treating missing/invalid data as unknown."""

    if path is None:
        return None
    target = Path(path)
    try:
        value = json.loads(target.read_text(encoding="utf-8"))
        if not isinstance(value, Mapping):
            return None
        measured_at, measured_dt = _utc_timestamp(
            "measured_at_utc", value.get("measured_at_utc"), nullable=False
        )
        sessions = _number("sessions", value.get("sessions"), integer=True, nullable=False)
        requests = _number("requests", value.get("requests"), integer=True, nullable=False)
        tool_bearing = _number(
            "tool_bearing_requests",
            value.get("tool_bearing_requests"),
            integer=True,
            nullable=False,
        )
        ratio = _number(
            "tools_per_request_avg",
            value.get("tools_per_request_avg"),
            nullable=False,
        )
        baseline = _number(
            "batching_baseline", value.get("batching_baseline"), nullable=False
        )
        target_ratio = _number(
            "batching_target", value.get("batching_target"), nullable=False
        )
        return {
            "measured_at_utc": measured_at,
            "_measured_dt": measured_dt,
            "sessions": sessions,
            "requests": requests,
            "tool_bearing_requests": tool_bearing,
            "tools_per_request_avg": ratio,
            "batching_baseline": baseline,
            "batching_target": target_ratio,
        }
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, ProjectionError):
        return None


def _batching_metric_block(
    snapshot: Mapping[str, Any] | None,
    *,
    now: datetime | None = None,
) -> str:
    lines = ["## Claude tool batching", ""]
    if snapshot is None:
        lines.extend(
            (
                "Average tool calls per tool-bearing request: unknown (no snapshot)",
                f"Baseline: {BATCHING_BASELINE:.2f}",
                f"Target: {BATCHING_TARGET:.2f}",
                "Coverage: unknown (no snapshot)",
                "Measured at (UTC): unknown (no snapshot)",
                "Freshness: unknown (no snapshot)",
            )
        )
        return "\n".join(lines) + "\n"

    measured_dt = snapshot.get("_measured_dt")
    current = now or datetime.now(timezone.utc)
    stale = not isinstance(measured_dt, datetime) or current - measured_dt > BATCHING_SNAPSHOT_MAX_AGE
    lines.extend(
        (
            f"Average tool calls per tool-bearing request: {snapshot['tools_per_request_avg']:.2f}",
            f"Baseline: {snapshot['batching_baseline']:.2f}",
            f"Target: {snapshot['batching_target']:.2f}",
            "Coverage: "
            f"{snapshot['sessions']} sessions, {snapshot['requests']} assistant requests, "
            f"{snapshot['tool_bearing_requests']} tool-bearing requests",
            f"Measured at (UTC): {snapshot['measured_at_utc']}",
            f"Freshness: {'STALE' if stale else 'current'} "
            f"(stale after {int(BATCHING_SNAPSHOT_MAX_AGE.total_seconds() // 3600)}h)",
        )
    )
    return "\n".join(lines) + "\n"


def render_metrics(
    aggregates: Iterable[Mapping[str, Any]],
    *,
    batching_snapshot: Mapping[str, Any] | None = None,
    now: datetime | None = None,
) -> str:
    sample = _eligible_aggregates(aggregates)
    lines = ["# Current Metrics", "", "Deterministic projection of the last 30 eligible terminal aggregates.", ""]
    lines.append(_batching_metric_block(batching_snapshot, now=now).rstrip())
    lines.append("")
    lines.append(_metric_block("Overall", sample).rstrip())
    groups: dict[str, list[Mapping[str, Any]]] = {}
    for run in sample:
        fingerprint = run.get("policy_fingerprint")
        if fingerprint is not None:
            groups.setdefault(fingerprint, []).append(run)
    if groups:
        lines.extend(("", "## Same-policy-fingerprint groups", ""))
        for fingerprint in sorted(groups):
            lines.append(_metric_block(f"Policy fingerprint: {fingerprint}", list(groups[fingerprint])).rstrip())
            lines.append("")
    else:
        lines.extend(("", "Same-policy-fingerprint groups: unknown (no policy fingerprint recorded).", ""))
    return "\n".join(lines).rstrip() + "\n"


def project_metrics(
    *,
    state_root: str | os.PathLike[str] | None = None,
    vault_root: str | os.PathLike[str] | None = None,
    batching_snapshot_path: str | os.PathLike[str] | None = None,
) -> Path:
    """Write Current Metrics from aggregates and the last valid audit snapshot."""

    vault = _vault_root(vault_root)
    path = _safe_vault_path(vault, "Calibration", "Current Metrics.md")
    content = _with_marker(
        "",
        render_metrics(
            _read_aggregates(state_root),
            batching_snapshot=read_batching_snapshot(batching_snapshot_path),
        ),
    )
    _atomic_write(path, content)
    return path


def render_provider_capacity(aggregates: Iterable[Mapping[str, Any]]) -> str:
    sample = [_prepare_aggregate(aggregate) for aggregate in aggregates]
    lines = [
        "# Current Capacity",
        "",
        "Capacity is not a quota reading. This note reports only usage fields recorded in telemetry.",
        "",
    ]
    for provider in ("codex", "gemini", "deepseek", "claude"):
        known = [run for run in sample if isinstance(run["usage"].get(f"{provider}_usage_known"), bool)]
        known_true = sum(run["usage"].get(f"{provider}_usage_known") is True for run in known)
        total_key = _TOKEN_TOTAL_KEYS.get(provider)
        totals = [run["usage"].get(total_key) for run in sample] if total_key else []
        totals = [value for value in totals if isinstance(value, (int, float)) and not isinstance(value, bool)]
        lines.extend((f"## {provider}", "", f"Usage known: {known_true} true of n={len(known)}"))
        lines.append(
            f"Observed total tokens: {_fmt_number(sum(totals))} (n={len(totals)})"
            if totals
            else "Observed total tokens: unknown (not recorded)"
        )
        lines.extend((
            "Operational mode: unknown (not tracked by this package).",
            "Capacity: unknown (no trusted local capacity source is recorded).",
            "Quota percentage: unknown (no trusted local quota source is recorded).",
            "",
        ))
    return "\n".join(lines).rstrip() + "\n"


def _health_value(state_root: str | os.PathLike[str] | None) -> dict[str, Any]:
    path = _state_root(state_root) / "telemetry" / "telemetry_health.json"
    if not path.exists():
        return {
            "status": None,
            "failure_count": None,
            "last_error_code": None,
            "updated_at": None,
            "last_projection_write_at": None,
        }
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProjectionError("telemetry health is unreadable") from exc
    if not isinstance(value, Mapping):
        raise ProjectionError("telemetry health is not an object")
    status = value.get("status")
    if status is not None and status not in {"OK", "DEGRADED"}:
        raise ProjectionError("telemetry health status is invalid")
    failure_count = _number("failure_count", value.get("failure_count"), integer=True)
    error_code = _identifier("last_error_code", value.get("last_error_code"))
    updated_at = _utc_timestamp("updated_at", value.get("updated_at"))[0]
    last_projection_write_at = _utc_timestamp(
        "last_projection_write_at", value.get("last_projection_write_at")
    )[0]
    return {
        "status": status,
        "failure_count": failure_count,
        "last_error_code": error_code,
        "updated_at": updated_at,
        "last_projection_write_at": last_projection_write_at,
    }


def render_health(
    aggregates: Iterable[Mapping[str, Any]],
    *,
    state_root: str | os.PathLike[str] | None = None,
    last_projection_write_at: str | None = None,
) -> str:
    health = _health_value(state_root)
    prepared = [_prepare_aggregate(aggregate) for aggregate in aggregates]
    if last_projection_write_at is not None:
        last_projection_write_at = _utc_timestamp(
            "last_projection_write_at", last_projection_write_at, nullable=False
        )[0]
    else:
        last_projection_write_at = health["last_projection_write_at"]
    return (
        "# Telemetry Health\n\n"
        "This is observational health for local telemetry and projection; it is not an authority for routing or acceptance.\n\n"
        f"- Status: {_display(health['status'])}\n"
        f"- Failure count: {_display(health['failure_count'])}\n"
        f"- Last error code: {_display(health['last_error_code'])}\n"
        f"- Health updated at (UTC): {_display(health['updated_at'])}\n"
        f"- Last successful projection write (UTC): {_display(last_projection_write_at)}\n"
        f"- Aggregate count: n={len(prepared)}\n"
        "- Projection mode: local-only, deterministic, no model/network/process access.\n"
    )


def _dashboard(aggregates: Iterable[Mapping[str, Any]], vault_root: Path) -> str:
    prepared = _eligible_aggregates(aggregates)
    lines = [
        "# Orchestrator Dashboard",
        "",
        "Deterministic downstream projection. The vault is not an orchestrator authority.",
        "",
        "- [[Calibration/Current Metrics|Current metrics]]",
        "- [[Providers/Current Capacity|Provider capacity]]",
        "- [[System/Telemetry Health|Telemetry health]]",
        "",
        "## Recent eligible runs",
        "",
    ]
    for run in reversed(prepared):
        path = _safe_run_path(vault_root, run)
        relative = path.relative_to(vault_root).with_suffix("").as_posix()
        lines.append(f"- [[{relative}|{run['task_id']}]] — {_display(run['status'])}")
    if not prepared:
        lines.append("- No eligible aggregates recorded.")
    return "\n".join(lines) + "\n"


def _readme() -> str:
    return (
        "# Orchestrator projection\n\n"
        "This vault is a deterministic, local-only Markdown projection of telemetry aggregates.\n\n"
        "It does not call a model, use the network, spawn a process, alter routing, or authorize work.\n"
        "Obsidian is optional: all generated content remains plain Markdown and YAML.\n"
        "Human-authored notes are never overwritten; remove or rename them before choosing a generated path.\n"
    )


def _base_runs() -> str:
    return (
        "filters:\n"
        "  and:\n"
        "    - 'type == \"orch-run\"'\n"
        "views:\n"
        "  - type: table\n"
        "    name: \"All runs\"\n"
        "    order:\n"
        "      - note.started_at\n"
        "      - note.task_id\n"
        "  - type: table\n"
        "    name: \"Active\"\n"
        "    filters:\n"
        "      and:\n"
        "        - 'status == \"IN_PROGRESS\"'\n"
        "    order:\n"
        "      - note.started_at\n"
        "  - type: table\n"
        "    name: \"Recent\"\n"
        "    filters:\n"
        "      and:\n"
        "        - 'ended_at > now() - \"30d\"'\n"
        "    order:\n"
        "      - note.ended_at\n"
        "  - type: table\n"
        "    name: \"Anomalies\"\n"
        "    filters:\n"
        "      and:\n"
        "        - or:\n"
        "            - 'routing_protocol_violations > 0'\n"
        "            - 'thread_continuity_violations > 0'\n"
        "            - 'concurrent_writer == true'\n"
        "    order:\n"
        "      - note.ended_at\n"
    )


def _base_routing() -> str:
    return (
        "filters:\n"
        "  and:\n"
        "    - 'type == \"orch-run\"'\n"
        "views:\n"
        "  - type: table\n"
        "    name: \"Route and model\"\n"
        "    groupBy:\n"
        "      property: note.route\n"
        "      direction: ASC\n"
        "    order:\n"
        "      - note.model\n"
        "      - note.started_at\n"
        "  - type: table\n"
        "    name: \"By model\"\n"
        "    groupBy:\n"
        "      property: note.model\n"
        "      direction: ASC\n"
        "    order:\n"
        "      - note.route\n"
        "      - note.started_at\n"
    )


def _base_efficiency() -> str:
    return (
        "filters:\n"
        "  and:\n"
        "    - 'type == \"orch-run\"'\n"
        "views:\n"
        "  - type: table\n"
        "    name: \"Duration and usage\"\n"
        "    order:\n"
        "      - note.duration_ms\n"
        "      - note.model\n"
        "      - note.usage.codex_total_tokens\n"
        "      - note.usage.gemini_total_tokens\n"
        "      - note.usage.deepseek_total_tokens\n"
    )


def _read_aggregates(state_root: str | os.PathLike[str] | None) -> list[Mapping[str, Any]]:
    tasks_root = _state_root(state_root) / "telemetry" / "tasks"
    if not tasks_root.exists():
        return []
    values: list[Mapping[str, Any]] = []
    for path in sorted(tasks_root.glob("*.json")):
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise ProjectionError(f"task aggregate cannot be read: {path.name}") from exc
        if not isinstance(value, Mapping) or value.get("task_id") != path.stem:
            raise ProjectionError(f"task aggregate identity is invalid: {path.name}")
        values.append(value)
    return values


def rebuild(
    *,
    state_root: str | os.PathLike[str] | None = None,
    vault_root: str | os.PathLike[str] | None = None,
) -> tuple[Path, ...]:
    """Regenerate all projection-owned files from aggregate caches."""

    vault = _vault_root(vault_root)
    aggregates = _read_aggregates(state_root)
    prepared = [_prepare_aggregate(aggregate) for aggregate in aggregates]
    outputs: list[tuple[Path, str]] = []
    for run in prepared:
        outputs.append((_safe_run_path(vault, run), _with_marker(_frontmatter(run), _run_body(run))))
    outputs.extend(
        (
            (_safe_vault_path(vault, "Dashboard.md"), _with_marker("", _dashboard(aggregates, vault))),
            (
                _safe_vault_path(vault, "Calibration", "Current Metrics.md"),
                _with_marker(
                    "",
                    render_metrics(
                        aggregates,
                        batching_snapshot=read_batching_snapshot(),
                    ),
                ),
            ),
            (_safe_vault_path(vault, "Providers", "Current Capacity.md"), _with_marker("", render_provider_capacity(aggregates))),
            (_safe_vault_path(vault, "System", "Telemetry Health.md"), _with_marker("", render_health(aggregates, state_root=state_root))),
            (_safe_vault_path(vault, "System", "README.md"), _with_marker("", _readme())),
            (_safe_vault_path(vault, "Views", "Runs.base"), _with_marker("", _base_runs(), yaml_comment=True)),
            (_safe_vault_path(vault, "Views", "Routing.base"), _with_marker("", _base_routing(), yaml_comment=True)),
            (_safe_vault_path(vault, "Views", "Efficiency.base"), _with_marker("", _base_efficiency(), yaml_comment=True)),
        )
    )
    for path, _ in outputs:
        if path.exists():
            try:
                existing = path.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError) as exc:
                raise ProjectionConflict(f"cannot inspect existing projection: {path}") from exc
            if not _generated_content_is_intact(existing):
                raise ProjectionConflict(f"refusing to overwrite non-projection note: {path}")
    for path, content in outputs:
        _atomic_write(path, content)
    return tuple(path for path, _ in outputs)


__all__ = [
    "DEFAULT_VAULT_ROOT",
    "MIN_RATE_DENOMINATOR",
    "ProjectionConflict",
    "ProjectionError",
    "project_health",
    "project_metrics",
    "read_batching_snapshot",
    "rebuild",
    "render_health",
    "render_metrics",
    "render_provider_capacity",
    "render_run_note",
    "project_task",
]
