import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1]))
import policy


ROOT = Path(__file__).parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "go_policy" / "config_cases.json"


def _valid():
    return {
        "allowed_phases": ["implementation", "plan"],
        "allowed_reasons": ["normal"],
        "allowed_efforts": ["low", "high"],
        "required_routes": ["normal"],
        "routes": {"normal": {"model": "gpt-5", "effort": "low"}},
        "always_full_routes": ["normal"],
        "reviewer_gate": {"small_delta_max_lines": 2, "small_delta_max_files": 1},
        "manifest_budget_seconds": 2.5,
        "reviewer_skip_reasons": [
            "PHASE_OPERATION", "PHASE_PLAN", "NO_DELTA", "BELOW_THRESHOLD", "REVIEWER_UNAVAILABLE"
        ],
        "reviewer_results": [
            "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "CHANGES_REQUIRED", "UNCERTAIN", "REVIEWER1_UNAVAILABLE"
        ],
    }


def _cases():
    cases = []

    def add(name, config, *, expected_field=None, expected_code=None):
        case = {"name": name, "input": config}
        if expected_field is not None:
            case["expected_field"] = expected_field
        if expected_code is not None:
            case["expected_code"] = expected_code
        cases.append(case)

    add("valid-default-agent", _valid())
    for agent, model, effort in (
        ("codex", "gpt-5", "low"),
        ("opencode", "openai/gpt-5", "high"),
        ("agy", "gemini-3.8-pro", "max"),
    ):
        config = _valid()
        config["coder_agent"] = agent
        config["routes"]["normal"] = {"model": model, "effort": effort}
        add(f"valid-{agent}", config)

    add("not-a-mapping", None, expected_code="INVALID_CONFIG")
    config = _valid()
    del config["allowed_phases"]
    add("missing-required-field", config, expected_code="MISSING_CONFIG_FIELD")
    for value in ("implementation", [], ["x", "x"], ["x", ""]):
        config = _valid()
        config["allowed_phases"] = value
        add(f"invalid-allowed-phases-{len(cases)}", config, expected_field="allowed_phases")
    config = _valid()
    config["coder_agent"] = "unknown"
    add("invalid-agent", config, expected_field="coder_agent")
    for routes, expected_field, expected_code in (
        ({}, "routes", None),
        ([], "routes", None),
        ({"other": {"model": "gpt-5", "effort": "low"}}, None, "ROUTE_TABLE_INCOMPLETE"),
    ):
        config = _valid()
        config["routes"] = routes
        add(f"invalid-route-table-{len(cases)}", config, expected_field=expected_field, expected_code=expected_code)
    config = _valid()
    config["routes"]["normal"] = None
    add("invalid-route-entry", config, expected_code="INVALID_ROUTE_ENTRY")
    config = _valid()
    config["required_routes"] = ["coder's"]
    config["routes"] = {"coder's": None}
    add("quoted-invalid-route-entry", config, expected_code="INVALID_ROUTE_ENTRY")
    config = _valid()
    config["routes"]["normal"]["model"] = ""
    add("missing-model", config, expected_code="INVALID_ROUTE_ENTRY")
    config = _valid()
    config["coder_agent"] = "opencode"
    config["routes"]["normal"]["model"] = "gpt-5"
    add("invalid-opencode-model", config, expected_code="INVALID_ROUTE_ENTRY")
    config = _valid()
    config["coder_agent"] = "agy"
    config["routes"]["normal"]["model"] = "gpt-5"
    add("invalid-agy-model", config, expected_code="INVALID_ROUTE_ENTRY")
    config = _valid()
    config["routes"]["normal"]["effort"] = "extreme"
    add("invalid-route-effort", config, expected_code="INVALID_ROUTE_EFFORT")
    config = _valid()
    config["always_full_routes"] = ["unknown"]
    add("unknown-always-full-route", config, expected_field="always_full_routes")
    config = _valid()
    config["reviewer_gate"] = []
    add("invalid-reviewer-gate", config, expected_field="reviewer_gate")
    for key, value in (("small_delta_max_lines", True), ("small_delta_max_files", -1)):
        config = _valid()
        config["reviewer_gate"][key] = value
        add(f"invalid-{key}", config, expected_field=key)
    config = _valid()
    config["reviewer_gate"]["small_delta_max_lines"] = 1.0
    add("float-reviewer-gate-limit", config, expected_field="small_delta_max_lines")
    config = _valid()
    config["manifest_budget_seconds"] = 0
    add("invalid-manifest-budget", config, expected_field="manifest_budget_seconds")
    for key, value in (
        ("bundle_non_report_max_bytes", True),
        ("implementation_report_max_chars", 0),
        ("operation_report_max_chars", -1),
    ):
        config = _valid()
        config[key] = value
        add(f"invalid-{key}", config, expected_field=key)
    config = _valid()
    config["reviewer_skip_reasons"] = ["NO_DELTA"]
    add("invalid-reviewer-skip-reasons", config, expected_field="reviewer_skip_reasons")
    config = _valid()
    config["reviewer_results"] = ["UNCERTAIN"]
    add("invalid-reviewer-results", config, expected_field="reviewer_results")
    config = _valid()
    config["micro_repair_timeout_seconds"] = 0
    add("invalid-micro-repair-timeout", config, expected_field="micro_repair_timeout_seconds")
    config = _valid()
    config["max_total_hook_budget_seconds"] = 0
    add("invalid-hook-budget", config, expected_field="max_total_hook_budget_seconds")
    config = _valid()
    config["micro_repair_enabled"] = 1
    add("invalid-micro-repair-enabled", config, expected_field="micro_repair_enabled")

    return cases


def oracle_results():
    results = []
    for case in _cases():
        item = dict(case)
        try:
            policy.validate_config(case["input"])
        except policy.ConfigError as exc:
            item.update(ok=False, error=f"{exc.code}: {exc.message}", loaded=None)
        else:
            item["ok"], item["error"] = True, ""
            item["loaded"] = policy.load_config(case["input"])
        results.append(item)
    return results


def test_python_policy_golden_fixture():
    actual = oracle_results()
    expected = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert actual == expected
    for case in actual:
        if case["name"].startswith("valid-"):
            assert case["ok"] is True and case["loaded"] is not None, case["name"]
        if case["name"].startswith("invalid-"):
            assert case["ok"] is False, case["name"]
            assert case.get("expected_field") or case.get("expected_code"), case["name"]
        if not case["ok"]:
            expected_error = case.get("expected_field") or case.get("expected_code")
            assert expected_error, f"{case['name']} must declare its expected field or code"
            assert expected_error in case["error"], (
                f"{case['name']} failed on the wrong branch: {case['error']}"
            )
