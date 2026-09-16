import json
from pathlib import Path
import sys

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import policy


CONFIG_PATH = Path(__file__).parents[1] / "config.json"


@pytest.fixture()
def config():
    return json.loads(CONFIG_PATH.read_text(encoding="utf-8"))


def orch(**overrides):
    values = {
        "task": "task-1",
        "phase": "implementation",
        "route": "normal",
        "reason": "new_task",
        "basis": "specific evidence",
        "scope": ".claude/hooks/orchestrator",
    }
    values.update(overrides)
    for key in tuple(values):
        if values[key] is None:
            del values[key]
    return "ORCH|" + "|".join(f"{key}={value}" for key, value in values.items())


def thread(**overrides):
    values = {
        "task_id": "task-1",
        "route": "normal",
        "model": "gpt-5.6-sol",
        "effort": "medium",
    }
    values.update(overrides)
    return values


def test_valid_orch_is_structured_and_removed(config):
    result = policy.parse_orch_prompt(orch() + "\nDo the work.", config)

    assert result.valid is True
    assert result.code == policy.VALID
    assert result.task_id == "task-1"
    assert result.prompt == "Do the work."
    assert result.scope == (".claude/hooks/orchestrator",)


@pytest.mark.parametrize(
    ("prompt", "code"),
    [
        ("Do the work.", policy.MISSING_ORCH_LINE),
        ("ORCH task=task-1", policy.MALFORMED_ORCH_LINE),
        (orch(extra="x"), policy.UNKNOWN_KEY),
        (orch(task=None), policy.MISSING_REQUIRED_KEY),
        (orch(phase=None), policy.MISSING_REQUIRED_KEY),
        (orch(route=None), policy.MISSING_REQUIRED_KEY),
        (orch(scope=None), policy.MISSING_REQUIRED_KEY),
        (orch(phase="not-a-phase"), policy.INVALID_ENUM_VALUE),
        (orch(route="not-a-route"), policy.INVALID_ENUM_VALUE),
    ],
)
def test_orch_failures_are_typed_denials_and_remove_first_line(config, prompt, code):
    result = policy.parse_orch_prompt(prompt + "\nbody", config)

    assert result.valid is False
    assert result.code == code
    assert result.message
    assert result.prompt == (prompt + "\nbody" if code == policy.MISSING_ORCH_LINE else "body")


def test_missing_or_malformed_orch_never_resolves_a_route(config):
    for prompt in ("work", "ORCH task=task-1"):
        parsed = policy.parse_orch_prompt(prompt, config)
        assert parsed.valid is False
        assert parsed.route is None
        assert policy.resolve_route(parsed.route, config).valid is False


def test_implementation_scope_is_required_bounded_and_normalized(config):
    missing = policy.parse_orch_prompt(orch(scope=None), config)
    whole_repo = policy.parse_orch_prompt(orch(scope="."), config)
    traversal = policy.parse_orch_prompt(orch(scope="src/../state"), config)
    absolute = policy.parse_orch_prompt(orch(scope="C:\\repo\\src"), config)
    valid = policy.parse_orch_prompt(orch(scope=" tests ,src\\module,tests "), config)

    assert missing.code == policy.MISSING_REQUIRED_KEY
    assert whole_repo.code == policy.UNBOUNDED_SCOPE
    assert traversal.code == policy.INVALID_SCOPE
    assert absolute.code == policy.INVALID_SCOPE
    assert valid.valid is True
    assert valid.scope == ("src/module", "tests")


def test_reason_and_basis_are_optional_but_recorded_and_validated_when_supplied(config):
    absent = policy.parse_orch_prompt(orch(reason=None, basis=None), config)
    supplied = policy.parse_orch_prompt(orch(reason="continue", basis="complex"), config)
    empty_basis = policy.parse_orch_prompt(orch(basis=""), config)
    invalid_reason = policy.parse_orch_prompt(orch(reason="invented"), config)

    assert absent.valid is True
    assert absent.reason is None
    assert absent.basis is None
    assert supplied.valid is True
    assert supplied.reason == "continue"
    assert supplied.basis == "complex"
    assert empty_basis.valid is True
    assert empty_basis.basis == ""
    assert invalid_reason.code == policy.INVALID_ENUM_VALUE


def test_operation_scope_is_optional_and_ignored(config):
    absent = policy.parse_orch_prompt(orch(phase="operation", scope=None), config)
    root = policy.parse_orch_prompt(orch(phase="operation", scope="."), config)

    assert absent.valid is True
    assert root.valid is True
    assert absent.scope == root.scope == ()


def test_route_table_is_exact_and_has_no_fallback(config):
    expected = {
        "mechanical": ("gpt-5.6-luna", "high"),
        "subtle": ("gpt-5.6-luna", "high"),
        "normal": ("gpt-5.6-sol", "medium"),
        "consequential": ("gpt-5.6-sol", "high"),
        "escalated": ("gpt-5.6-sol", "xhigh"),
        "operation": ("gpt-5.6-terra", "high"),
    }
    for route, pair in expected.items():
        result = policy.resolve_route(route, config)
        assert result.valid is True
        assert result.pair == pair

    unknown = policy.resolve_route("future-route", config)
    assert unknown.valid is False
    assert unknown.model is None
    assert unknown.effort is None


def test_invalid_config_is_rejected_on_load_and_resolution(config):
    missing_route = json.loads(json.dumps(config))
    del missing_route["routes"]["normal"]
    with pytest.raises(policy.ConfigError):
        policy.load_config(missing_route)
    assert policy.resolve_route("normal", missing_route).code == policy.CONFIG_INVALID

    invalid_effort = json.loads(json.dumps(config))
    invalid_effort["routes"]["normal"]["effort"] = "instant"
    with pytest.raises(policy.ConfigError):
        policy.validate_config(invalid_effort)

    invalid_budget = json.loads(json.dumps(config))
    invalid_budget["manifest_budget_seconds"] = 0
    with pytest.raises(policy.ConfigError):
        policy.validate_config(invalid_budget)


def test_vague_sol_basis_configuration_is_no_longer_required(config):
    assert "vague_sol_bases" not in config
    assert policy.validate_config(config) is None


def test_reviewer_gate_reaches_every_skip_reason(config):
    operation = policy.reviewer1_gate("operation", 100, 1000, "normal", True, config)
    empty = policy.reviewer1_gate("implementation", 0, 0, "normal", True, config)
    small = policy.reviewer1_gate("implementation", 1, 1, "normal", True, config)
    unavailable = policy.reviewer1_gate("implementation", 3, 51, "normal", False, config)

    assert isinstance(operation, policy.SkipReviewerDecision)
    assert {operation.skip_reason, empty.skip_reason, small.skip_reason, unavailable.skip_reason} == {
        policy.PHASE_OPERATION,
        policy.NO_DELTA,
        policy.BELOW_THRESHOLD,
        policy.REVIEWER_UNAVAILABLE,
    }


def test_reviewer_gate_runs_for_large_available_delta_and_not_for_operation(config):
    run = policy.reviewer1_gate("implementation", 3, 51, "normal", True, config)
    operation = policy.reviewer1_gate("operation", 3, 51, "normal", True, config)

    assert isinstance(run, policy.RunReviewerDecision)
    assert run.action == "run reviewer"
    assert isinstance(operation, policy.SkipReviewerDecision)
    assert operation.action == "skip reviewer"


def test_gate_skip_shape_feeds_review_mode_without_becoming_a_reviewer_pass(config):
    skipped = policy.reviewer1_gate("implementation", 0, 0, "normal", True, config)

    assert isinstance(skipped, policy.SkipReviewerDecision)
    assert policy.claude_review_mode("normal", skipped, config).mode == policy.AUDIT
    assert not hasattr(skipped, "result")


def test_unclassifiable_gate_input_is_an_explicit_safe_run(config):
    result = policy.reviewer1_gate("implementation", -1, 10, "normal", True, config)

    assert isinstance(result, policy.RunReviewerDecision)
    assert result.code == "UNCLASSIFIABLE_INPUT"
    assert not hasattr(result, "skip_reason")


@pytest.mark.parametrize(
    ("route", "outcome", "mode"),
    [
        ("normal", policy.ReviewerSkipped(policy.NO_DELTA), policy.AUDIT),
        ("normal", policy.ReviewerSkipped(policy.BELOW_THRESHOLD), policy.AUDIT),
        ("normal", policy.ReviewerSkipped(policy.REVIEWER_UNAVAILABLE), policy.FULL),
        ("normal", policy.ReviewerSkipped(policy.PHASE_OPERATION), policy.FULL),
        ("normal", policy.ReviewerReport(policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE), policy.AUDIT),
        ("normal", policy.ReviewerReport(policy.CHANGES_REQUIRED), policy.FULL),
        ("normal", policy.ReviewerReport(policy.UNCERTAIN), policy.FULL),
        ("normal", policy.ReviewerReport(policy.REVIEWER1_UNAVAILABLE), policy.FULL),
        ("consequential", policy.ReviewerSkipped(policy.NO_DELTA), policy.FULL),
        ("escalated", policy.ReviewerReport(policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE), policy.FULL),
    ],
)
def test_claude_review_mode(config, route, outcome, mode):
    assert policy.claude_review_mode(route, outcome, config).mode == mode


def test_unknown_review_outcome_fails_to_full(config):
    assert policy.claude_review_mode("normal", object(), config).mode == policy.FULL


def test_compatible_thread_reuses_and_fresh_codex_is_denied():
    requested = thread()
    compatible = policy.thread_compatibility(requested, thread())
    assert compatible.action == "reuse via codex-reply"
    assert compatible.compatible is True

    inverse = policy.fresh_codex_decision(requested, thread())
    assert inverse.allowed is False
    assert inverse.code == "USE_CODEX_REPLY"
    assert "codex-reply" in inverse.message


def test_corrupt_recorded_thread_allows_fresh_codex_with_anomaly_code():
    requested = thread()
    corrupt_recorded = {
        "task_id": "task-1",
        "route": "normal",
        "model": "gpt-5.6-sol",
    }

    result = policy.fresh_codex_decision(requested, corrupt_recorded)

    assert result.allowed is True
    assert result.action == "allow fresh codex"
    assert result.code == "INVALID_LIVE_THREAD"
    assert "corrupt" in result.message


def test_malformed_requested_thread_allows_fresh_codex_with_anomaly_code():
    malformed_requested = {
        "task_id": "task-1",
        "route": "normal",
        "effort": "medium",
    }

    result = policy.fresh_codex_decision(malformed_requested, thread())

    assert result.allowed is True
    assert result.action == "allow fresh codex"
    assert result.code == "INVALID_REQUEST"
    assert "malformed" in result.message


@pytest.mark.parametrize("field", ["task_id", "route", "model", "effort"])
def test_any_thread_identity_difference_requires_fresh(field):
    requested = thread()
    recorded = thread(**{field: "different"})

    result = policy.thread_compatibility(requested, recorded)
    assert result.action == "require a fresh thread"
    assert result.compatible is False
    assert policy.fresh_codex_decision(requested, recorded).allowed is True
