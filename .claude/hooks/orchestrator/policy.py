"""Pure policy decisions for the Claude Code orchestrator.

The caller supplies the already-loaded configuration mapping.  This module
does not read files or perform any I/O, so every decision can be tested with
plain Python data.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass
import math
from pathlib import PurePosixPath
import re
from typing import Any, Union


VALID = "VALID"
MISSING_ORCH_LINE = "MISSING_ORCH_LINE"
MALFORMED_ORCH_LINE = "MALFORMED_ORCH_LINE"
UNKNOWN_KEY = "UNKNOWN_KEY"
MISSING_REQUIRED_KEY = "MISSING_REQUIRED_KEY"
INVALID_ENUM_VALUE = "INVALID_ENUM_VALUE"
INVALID_SCOPE = "INVALID_SCOPE"
UNBOUNDED_SCOPE = "UNBOUNDED_SCOPE"
CONFIG_INVALID = "CONFIG_INVALID"

PHASE_OPERATION = "PHASE_OPERATION"
NO_DELTA = "NO_DELTA"
BELOW_THRESHOLD = "BELOW_THRESHOLD"
REVIEWER_UNAVAILABLE = "REVIEWER_UNAVAILABLE"

NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE = "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
CHANGES_REQUIRED = "CHANGES_REQUIRED"
UNCERTAIN = "UNCERTAIN"
REVIEWER1_UNAVAILABLE = "REVIEWER1_UNAVAILABLE"

AUDIT = "AUDIT"
FULL = "FULL"

_ORCH_KEYS = ("task", "phase", "route", "reason", "basis", "scope")
_REQUIRED_ORCH_KEYS = ("task", "phase", "route")
_REVIEWER_SKIP_PRECEDENCE = (
    PHASE_OPERATION,
    NO_DELTA,
    BELOW_THRESHOLD,
    REVIEWER_UNAVAILABLE,
)
_AUDIT_SKIP_REASONS = frozenset({NO_DELTA, BELOW_THRESHOLD})
_FULL_REVIEW_RESULTS = frozenset(
    {CHANGES_REQUIRED, UNCERTAIN, REVIEWER1_UNAVAILABLE}
)


class ConfigError(ValueError):
    """A supplied policy configuration cannot safely be used."""

    def __init__(self, code: str, message: str) -> None:
        self.code = code
        self.message = message
        super().__init__(f"{code}: {message}")


def _string_list(config: Mapping[str, Any], key: str) -> list[str]:
    value = config.get(key)
    if (
        not isinstance(value, Sequence)
        or isinstance(value, (str, bytes))
        or not value
        or any(not isinstance(item, str) or not item for item in value)
        or len(set(value)) != len(value)
    ):
        raise ConfigError(
            "INVALID_CONFIG_FIELD",
            f"{key} must be a non-empty list of unique non-empty strings",
        )
    return list(value)


def validate_config(config: Mapping[str, Any]) -> None:
    """Validate the complete config contract, raising ``ConfigError`` safely."""

    if not isinstance(config, Mapping):
        raise ConfigError("INVALID_CONFIG", "config must be a mapping")

    required = {
        "allowed_phases",
        "allowed_reasons",
        "allowed_efforts",
        "required_routes",
        "routes",
        "sol_model",
        "always_full_routes",
        "reviewer_gate",
        "manifest_budget_seconds",
        "reviewer_skip_reasons",
        "reviewer_results",
    }
    missing = sorted(required.difference(config))
    if missing:
        raise ConfigError(
            "MISSING_CONFIG_FIELD",
            f"config is missing required field(s): {', '.join(missing)}",
        )

    _string_list(config, "allowed_phases")
    _string_list(config, "allowed_reasons")
    allowed_efforts = set(_string_list(config, "allowed_efforts"))
    required_routes = set(_string_list(config, "required_routes"))

    routes = config["routes"]
    if not isinstance(routes, Mapping) or not routes:
        raise ConfigError("INVALID_CONFIG_FIELD", "routes must be a non-empty mapping")
    if set(routes) != required_routes:
        raise ConfigError(
            "ROUTE_TABLE_INCOMPLETE",
            "routes must contain exactly the configured required_routes",
        )
    for route, entry in routes.items():
        if not isinstance(route, str) or not isinstance(entry, Mapping):
            raise ConfigError("INVALID_ROUTE_ENTRY", f"invalid route entry for {route!r}")
        model = entry.get("model")
        effort = entry.get("effort")
        if not isinstance(model, str) or not model:
            raise ConfigError("INVALID_ROUTE_ENTRY", f"route {route!r} has no model")
        if not isinstance(effort, str) or effort not in allowed_efforts:
            raise ConfigError(
                "INVALID_ROUTE_EFFORT",
                f"route {route!r} names unsupported effort {effort!r}",
            )

    sol_model = config["sol_model"]
    if not isinstance(sol_model, str) or not sol_model:
        raise ConfigError("INVALID_CONFIG_FIELD", "sol_model must be a non-empty string")
    if not any(entry["model"] == sol_model for entry in routes.values()):
        raise ConfigError("INVALID_CONFIG_FIELD", "sol_model is absent from the route table")

    always_full_routes = set(_string_list(config, "always_full_routes"))
    if not always_full_routes.issubset(routes):
        raise ConfigError(
            "INVALID_CONFIG_FIELD",
            "always_full_routes must name configured routes",
        )

    gate = config["reviewer_gate"]
    if not isinstance(gate, Mapping):
        raise ConfigError("INVALID_CONFIG_FIELD", "reviewer_gate must be a mapping")
    for key in ("small_delta_max_lines", "small_delta_max_files"):
        value = gate.get(key)
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            raise ConfigError(
                "INVALID_CONFIG_FIELD",
                f"{key} must be a non-negative integer",
            )

    manifest_budget = config["manifest_budget_seconds"]
    if (
        not isinstance(manifest_budget, (int, float))
        or isinstance(manifest_budget, bool)
        or not math.isfinite(manifest_budget)
        or manifest_budget <= 0
    ):
        raise ConfigError(
            "INVALID_CONFIG_FIELD",
            "manifest_budget_seconds must be a positive finite number",
        )

    for key in (
        "bundle_non_report_max_bytes",
        "implementation_report_max_chars",
        "operation_report_max_chars",
    ):
        if key in config:
            value = config[key]
            if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
                raise ConfigError(
                    "INVALID_CONFIG_FIELD",
                    f"{key} must be a positive integer",
                )

    configured_skip_reasons = set(_string_list(config, "reviewer_skip_reasons"))
    if configured_skip_reasons != set(_REVIEWER_SKIP_PRECEDENCE):
        raise ConfigError(
            "INVALID_CONFIG_FIELD",
            "reviewer_skip_reasons must contain the four specified skip codes",
        )
    configured_results = set(_string_list(config, "reviewer_results"))
    expected_results = {
        NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
        CHANGES_REQUIRED,
        UNCERTAIN,
        REVIEWER1_UNAVAILABLE,
    }
    if configured_results != expected_results:
        raise ConfigError(
            "INVALID_CONFIG_FIELD",
            "reviewer_results must contain the four specified result codes",
        )

    if "micro_repair_timeout_seconds" in config:
        repair_timeout = config["micro_repair_timeout_seconds"]
        if (
            not isinstance(repair_timeout, (int, float))
            or isinstance(repair_timeout, bool)
            or not math.isfinite(repair_timeout)
            or repair_timeout <= 0
        ):
            raise ConfigError(
                "INVALID_CONFIG_FIELD",
                "micro_repair_timeout_seconds must be a positive finite number",
            )

    if "max_total_hook_budget_seconds" in config:
        hook_budget = config["max_total_hook_budget_seconds"]
        if (
            not isinstance(hook_budget, (int, float))
            or isinstance(hook_budget, bool)
            or not math.isfinite(hook_budget)
            or hook_budget <= 0
        ):
            raise ConfigError(
                "INVALID_CONFIG_FIELD",
                "max_total_hook_budget_seconds must be a positive finite number",
            )

    if "micro_repair_enabled" in config:
        if not isinstance(config["micro_repair_enabled"], bool):
            raise ConfigError(
                "INVALID_CONFIG_FIELD",
                "micro_repair_enabled must be a boolean",
            )


def load_config(config: Mapping[str, Any]) -> Mapping[str, Any]:
    """Validate and return a caller-owned config mapping without mutating it."""

    validate_config(config)
    return config


@dataclass(frozen=True)
class OrchParseResult:
    valid: bool
    code: str
    message: str
    prompt: str
    task_id: str | None = None
    phase: str | None = None
    route: str | None = None
    reason: str | None = None
    basis: str | None = None
    scope: tuple[str, ...] = ()

    @property
    def clean_prompt(self) -> str:
        return self.prompt

    @property
    def error_code(self) -> str:
        return self.code


def _orch_failure(
    prompt: str,
    code: str,
    message: str,
    values: Mapping[str, str] | None = None,
) -> OrchParseResult:
    values = values or {}
    return OrchParseResult(
        valid=False,
        code=code,
        message=message,
        prompt=prompt,
        task_id=values.get("task"),
        phase=values.get("phase"),
        route=values.get("route"),
        reason=values.get("reason"),
        basis=values.get("basis"),
    )


def _first_line_and_remainder(prompt: Any) -> tuple[str | None, str]:
    if not isinstance(prompt, str):
        return None, ""
    first, separator, remainder = prompt.partition("\n")
    return first.removesuffix("\r"), remainder if separator else ""


def _implementation_scope(value: str) -> tuple[str, ...]:
    parts = value.split(",")
    if not parts or any(not part.strip() for part in parts):
        raise ValueError(INVALID_SCOPE)
    normalized: list[str] = []
    for raw in parts:
        item = raw.strip().replace("\\", "/")
        path = PurePosixPath(item)
        if (
            "\x00" in item
            or path.is_absolute()
            or ".." in path.parts
            or re.match(r"^[A-Za-z]:", item)
        ):
            raise ValueError(INVALID_SCOPE)
        canonical = path.as_posix()
        if canonical in ("", "."):
            raise ValueError(UNBOUNDED_SCOPE)
        normalized.append(canonical)
    return tuple(sorted(set(normalized)))


def parse_orch_prompt(prompt: str, config: Mapping[str, Any]) -> OrchParseResult:
    """Parse and validate the first-line ORCH protocol record."""

    first_line, remainder = _first_line_and_remainder(prompt)
    if first_line is None:
        return _orch_failure(
            "",
            MISSING_ORCH_LINE,
            "The prompt must start with an ORCH line.",
        )
    if not first_line.startswith("ORCH"):
        return _orch_failure(
            prompt,
            MISSING_ORCH_LINE,
            "The prompt must start with an ORCH line.",
        )
    if first_line != "ORCH" and not first_line.startswith("ORCH|"):
        return _orch_failure(
            remainder,
            MALFORMED_ORCH_LINE,
            "The ORCH line has an invalid prefix.",
        )

    values: dict[str, str] = {}
    for field in first_line.split("|")[1:]:
        if "=" not in field:
            return _orch_failure(
                remainder,
                MALFORMED_ORCH_LINE,
                "Each ORCH field must use key=value syntax.",
                values,
            )
        key, value = field.split("=", 1)
        if not key:
            return _orch_failure(
                remainder,
                MALFORMED_ORCH_LINE,
                "An ORCH field has an empty key.",
                values,
            )
        if key not in _ORCH_KEYS:
            return _orch_failure(
                remainder,
                UNKNOWN_KEY,
                f"Unknown ORCH key {key!r}.",
                values,
            )
        if key in values:
            return _orch_failure(
                remainder,
                MALFORMED_ORCH_LINE,
                f"ORCH key {key!r} occurs more than once.",
                values,
            )
        if key == "task" and not value.strip():
            return _orch_failure(
                remainder,
                MALFORMED_ORCH_LINE,
                "The ORCH task value cannot be empty.",
                values,
            )
        values[key] = value

    missing = [key for key in _REQUIRED_ORCH_KEYS if key not in values]
    if missing:
        return _orch_failure(
            remainder,
            MISSING_REQUIRED_KEY,
            f"Missing required ORCH key {missing[0]!r}.",
            values,
        )

    try:
        validate_config(config)
    except ConfigError as error:
        return _orch_failure(
            remainder,
            CONFIG_INVALID,
            f"Policy configuration is invalid: {error.message}",
            values,
        )

    enum_fields = {
        "phase": set(config["allowed_phases"]),
        "reason": set(config["allowed_reasons"]),
        "route": set(config["routes"]),
    }
    for key, allowed in enum_fields.items():
        if key in values and values[key] not in allowed:
            return _orch_failure(
                remainder,
                INVALID_ENUM_VALUE,
                f"ORCH {key} value {values[key]!r} is not allowed.",
                values,
            )

    scope: tuple[str, ...] = ()
    if values["phase"] == "implementation":
        if "scope" not in values:
            return _orch_failure(
                remainder,
                MISSING_REQUIRED_KEY,
                "Missing required ORCH key 'scope' for implementation phase.",
                values,
            )
        try:
            scope = _implementation_scope(values["scope"])
        except ValueError as error:
            code = str(error)
            message = (
                "Implementation scope cannot resolve to the whole repository."
                if code == UNBOUNDED_SCOPE
                else "Implementation scope must contain repository-relative paths without traversal."
            )
            return _orch_failure(remainder, code, message, values)

    return OrchParseResult(
        valid=True,
        code=VALID,
        message="ORCH metadata is valid.",
        prompt=remainder,
        task_id=values["task"],
        phase=values["phase"],
        route=values["route"],
        reason=values.get("reason"),
        basis=values.get("basis"),
        scope=scope,
    )


parse_prompt = parse_orch_prompt


@dataclass(frozen=True)
class RouteResolution:
    valid: bool
    code: str
    message: str
    route: str | None = None
    model: str | None = None
    effort: str | None = None

    @property
    def pair(self) -> tuple[str, str] | None:
        if not self.valid:
            return None
        assert self.model is not None and self.effort is not None
        return self.model, self.effort


def resolve_route(route: str, config: Mapping[str, Any]) -> RouteResolution:
    """Resolve a configured route; failures never contain a fallback pair."""

    try:
        validate_config(config)
    except ConfigError as error:
        return RouteResolution(False, CONFIG_INVALID, error.message, route=route)
    if not isinstance(route, str) or route not in config["routes"]:
        return RouteResolution(
            False,
            INVALID_ENUM_VALUE,
            f"Unknown route {route!r}; no model or effort was selected.",
            route=route if isinstance(route, str) else None,
        )
    entry = config["routes"][route]
    return RouteResolution(
        True,
        VALID,
        "Route is valid.",
        route=route,
        model=entry["model"],
        effort=entry["effort"],
    )


@dataclass(frozen=True)
class RunReviewerDecision:
    """The reviewer must run; this shape has no skip-reason field."""

    action: str
    code: str
    message: str

    @property
    def should_run(self) -> bool:
        return True


@dataclass(frozen=True)
class SkipReviewerDecision:
    """The reviewer is skipped for exactly one named reason."""

    action: str
    skip_reason: str
    message: str

    @property
    def code(self) -> str:
        return self.skip_reason

    @property
    def reason(self) -> str:
        return self.skip_reason

    @property
    def should_run(self) -> bool:
        return False


ReviewerGateDecision = Union[RunReviewerDecision, SkipReviewerDecision]


def _run_reviewer(code: str, message: str) -> RunReviewerDecision:
    return RunReviewerDecision("run reviewer", code, message)


def _skip_reviewer(reason: str, message: str) -> SkipReviewerDecision:
    return SkipReviewerDecision("skip reviewer", reason, message)


def reviewer1_gate(
    phase: str,
    delta_files: int,
    delta_lines: int,
    route: str,
    reviewer_available: bool,
    config: Mapping[str, Any],
) -> ReviewerGateDecision:
    """Apply the mechanical Reviewer-1 gate.

    Precedence is the order in BUILD_SPEC.md: operation, empty delta,
    below-threshold delta, then unavailable transport.  A type/config/route
    value that cannot be classified is an explicit run decision for safety.
    """

    # This check is deliberately first: operation can never trigger Reviewer-1,
    # even if another supplied value would otherwise be unclassifiable.
    if phase == "operation":
        return _skip_reviewer(PHASE_OPERATION, "Operation phase does not run Reviewer-1.")

    try:
        validate_config(config)
    except ConfigError as error:
        return _run_reviewer(CONFIG_INVALID, f"Reviewer gate configuration is invalid: {error.message}")

    if phase not in config["allowed_phases"] or phase != "implementation":
        return _run_reviewer(
            "UNCLASSIFIABLE_INPUT",
            "Reviewer gate input could not be classified safely.",
        )
    if not isinstance(route, str) or route not in config["routes"]:
        return _run_reviewer(
            "UNCLASSIFIABLE_INPUT",
            "Reviewer gate route could not be classified safely.",
        )
    if (
        not isinstance(delta_files, int)
        or isinstance(delta_files, bool)
        or not isinstance(delta_lines, int)
        or isinstance(delta_lines, bool)
        or delta_files < 0
        or delta_lines < 0
        or not isinstance(reviewer_available, bool)
    ):
        return _run_reviewer(
            "UNCLASSIFIABLE_INPUT",
            "Reviewer gate input could not be classified safely.",
        )

    if delta_files == 0 and delta_lines == 0:
        return _skip_reviewer(NO_DELTA, "The change has no delta.")

    gate = config["reviewer_gate"]
    if (
        delta_lines <= gate["small_delta_max_lines"]
        and delta_files <= gate["small_delta_max_files"]
    ):
        return _skip_reviewer(BELOW_THRESHOLD, "The change is below the Reviewer-1 threshold.")

    if not reviewer_available:
        return _skip_reviewer(
            REVIEWER_UNAVAILABLE,
            "Reviewer-1 transport is unavailable.",
        )

    return _run_reviewer("RUN_REVIEWER", "Reviewer-1 is required for this delta.")


@dataclass(frozen=True)
class ReviewerReport:
    """A result reported by a reviewer that actually ran."""

    result: str


@dataclass(frozen=True)
class ReviewerSkipped:
    """A skipped reviewer, with no reviewer result field by construction."""

    skip_reason: str


ReviewerOutcome = Union[ReviewerReport, ReviewerSkipped, SkipReviewerDecision]


@dataclass(frozen=True)
class ReviewModeDecision:
    mode: str
    code: str
    message: str


def claude_review_mode(
    route: str,
    outcome: ReviewerOutcome,
    config: Mapping[str, Any],
) -> ReviewModeDecision:
    """Choose Claude AUDIT/FULL mode, failing closed to FULL."""

    try:
        validate_config(config)
    except ConfigError as error:
        return ReviewModeDecision(FULL, CONFIG_INVALID, error.message)

    if not isinstance(route, str) or route not in config["routes"]:
        return ReviewModeDecision(
            FULL,
            "UNCLASSIFIABLE_INPUT",
            "Unknown route requires FULL Claude review.",
        )
    if route in config["always_full_routes"]:
        return ReviewModeDecision(
            FULL,
            "ALWAYS_FULL_ROUTE",
            "This route always requires FULL Claude review.",
        )

    if isinstance(outcome, (ReviewerSkipped, SkipReviewerDecision)):
        if isinstance(outcome.skip_reason, str) and outcome.skip_reason in _AUDIT_SKIP_REASONS:
            return ReviewModeDecision(
                AUDIT,
                outcome.skip_reason,
                "The skipped reviewer reason establishes a trivial change.",
            )
        return ReviewModeDecision(
            FULL,
            outcome.skip_reason,
            "The skipped reviewer reason does not establish a trivial change.",
        )

    if isinstance(outcome, ReviewerReport):
        if outcome.result == NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE:
            return ReviewModeDecision(
                AUDIT,
                outcome.result,
                "Reviewer-1 found no material finding in its reviewed scope.",
            )
        if isinstance(outcome.result, str) and outcome.result in _FULL_REVIEW_RESULTS:
            return ReviewModeDecision(
                FULL,
                outcome.result,
                "Reviewer-1 reported a result requiring FULL Claude review.",
            )

    return ReviewModeDecision(
        FULL,
        "UNCLASSIFIABLE_INPUT",
        "Unclassifiable reviewer outcome requires FULL Claude review.",
    )


decide_claude_review_mode = claude_review_mode


@dataclass(frozen=True)
class ThreadCompatibilityDecision:
    action: str
    compatible: bool
    code: str
    message: str

    @property
    def reuse_via_codex_reply(self) -> bool:
        return self.compatible


@dataclass(frozen=True)
class FreshCodexDecision:
    action: str
    allowed: bool
    code: str
    message: str


def _thread_values(value: Mapping[str, Any] | None) -> tuple[Any, Any, Any, Any] | None:
    if not isinstance(value, Mapping):
        return None
    fields = tuple(value.get(key) for key in ("task_id", "route", "model", "effort"))
    if any(not isinstance(field, str) or not field for field in fields):
        return None
    return fields  # type: ignore[return-value]


def thread_compatibility(
    requested: Mapping[str, Any],
    recorded: Mapping[str, Any] | None,
) -> ThreadCompatibilityDecision:
    """Decide whether an existing live thread can be used via codex-reply."""

    requested_values = _thread_values(requested)
    if requested_values is None:
        return ThreadCompatibilityDecision(
            "require a fresh thread",
            False,
            "INVALID_REQUEST",
            "Requested thread identity is incomplete; a fresh thread is required.",
        )
    if recorded is None:
        return ThreadCompatibilityDecision(
            "require a fresh thread",
            False,
            "NO_LIVE_THREAD",
            "No compatible live thread was recorded.",
        )
    recorded_values = _thread_values(recorded)
    if recorded_values is None:
        return ThreadCompatibilityDecision(
            "require a fresh thread",
            False,
            "INVALID_LIVE_THREAD",
            "Recorded live thread identity is incomplete; a fresh thread is required.",
        )

    labels = ("task id", "route", "model", "effort")
    for label, requested_value, recorded_value in zip(
        labels, requested_values, recorded_values
    ):
        if requested_value != recorded_value:
            return ThreadCompatibilityDecision(
                "require a fresh thread",
                False,
                f"{label.upper().replace(' ', '_')}_MISMATCH",
                f"The recorded live thread has a different {label}; use a fresh thread.",
            )

    return ThreadCompatibilityDecision(
        "reuse via codex-reply",
        True,
        "COMPATIBLE",
        "The live thread is compatible; use codex-reply.",
    )


decide_thread_compatibility = thread_compatibility


def fresh_codex_decision(
    requested: Mapping[str, Any],
    recorded: Mapping[str, Any] | None,
) -> FreshCodexDecision:
    """Return the inverse guard for a fresh codex call."""

    compatibility = thread_compatibility(requested, recorded)
    if compatibility.compatible:
        return FreshCodexDecision(
            "deny fresh codex",
            False,
            "USE_CODEX_REPLY",
            "A compatible live thread exists; use codex-reply instead.",
        )
    if compatibility.code == "INVALID_REQUEST":
        return FreshCodexDecision(
            "allow fresh codex",
            True,
            compatibility.code,
            "Requested thread identity is malformed; allowing a fresh codex call and surfacing the anomaly.",
        )
    if compatibility.code == "INVALID_LIVE_THREAD":
        return FreshCodexDecision(
            "allow fresh codex",
            True,
            compatibility.code,
            "Recorded live thread identity is corrupt; allowing a fresh codex call and surfacing the anomaly.",
        )
    return FreshCodexDecision(
        "allow fresh codex",
        True,
        compatibility.code,
        "No compatible live thread exists; a fresh codex call is allowed.",
    )


decide_fresh_codex = fresh_codex_decision


__all__ = [
    "AUDIT",
    "BELOW_THRESHOLD",
    "CHANGES_REQUIRED",
    "CONFIG_INVALID",
    "ConfigError",
    "FULL",
    "INVALID_SCOPE",
    "INVALID_ENUM_VALUE",
    "MALFORMED_ORCH_LINE",
    "MISSING_ORCH_LINE",
    "MISSING_REQUIRED_KEY",
    "NO_DELTA",
    "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
    "OrchParseResult",
    "REVIEWER1_UNAVAILABLE",
    "REVIEWER_UNAVAILABLE",
    "ReviewerReport",
    "ReviewerSkipped",
    "RouteResolution",
    "ReviewModeDecision",
    "RunReviewerDecision",
    "SkipReviewerDecision",
    "ThreadCompatibilityDecision",
    "UNCERTAIN",
    "UNKNOWN_KEY",
    "UNBOUNDED_SCOPE",
    "claude_review_mode",
    "decide_claude_review_mode",
    "decide_fresh_codex",
    "decide_thread_compatibility",
    "fresh_codex_decision",
    "load_config",
    "parse_orch_prompt",
    "parse_prompt",
    "resolve_route",
    "reviewer1_gate",
    "thread_compatibility",
    "validate_config",
]
