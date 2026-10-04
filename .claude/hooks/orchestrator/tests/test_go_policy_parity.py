import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1]))
import policy


ROOT = Path(__file__).parents[1]
FIXTURE = ROOT / "tests" / "fixtures" / "go_policy" / "config_cases.json"
ORCH_FIXTURE = ROOT / "tests" / "fixtures" / "go_policy" / "orch.json"
ROUTE_THREAD_FIXTURE = ROOT / "tests" / "fixtures" / "go_policy" / "route_thread.json"


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


def _orch_config():
    config = _valid()
    config["allowed_phases"] = ["implementation", "plan", "operation"]
    config["allowed_reasons"] = ["normal", "incident"]
    config["required_routes"] = ["normal", "subtle", "consequential", "escalated", "plan_standard"]
    config["routes"] = {
        route: {"model": "gpt-5", "effort": "low"}
        for route in config["required_routes"]
    }
    return config


def _orch_cases():
    config = _orch_config()
    cases = []

    def add(name, prompt, *, case_config=None, expected_code=None, expected_message=None):
        case = {"name": name, "input": {"prompt": prompt, "config": case_config or config}}
        if expected_code is not None:
            case["expected_code"] = expected_code
        if expected_message is not None:
            case["expected_message"] = expected_message
        cases.append(case)

    add("valid-implementation", "ORCH|task=ship|phase=implementation|route=normal|reason=normal|basis=choice|scope=src\\pkg, docs/guide.md|eta=600m\r\nbody\n")
    add("valid-plan", "ORCH|task=design|phase=plan|route=plan_standard|scope=docs,docs/./docs2\nplan body")
    add("valid-operation", "ORCH|task=inspect|phase=operation|route=subtle|reason=incident\r\n\r\nbody")
    add("missing-prefix", "  ORCH|task=x\nbody", expected_code="MISSING_ORCH_LINE", expected_message="The prompt must start with an ORCH line.")
    add("non-string-prompt", None, expected_code="MISSING_ORCH_LINE", expected_message="The prompt must start with an ORCH line.")
    add("malformed-prefix", "ORCHx|task=x\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="The ORCH line has an invalid prefix.")
    add("missing-field-value-syntax", "ORCH|task=x|phase\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="Each ORCH field must use key=value syntax.")
    add("eta-without-value-syntax", "ORCH|eta\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="ORCH eta must use eta=<N>m with N an integer from 1..600.")
    add("empty-key", "ORCH|=x\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="An ORCH field has an empty key.")
    add("unknown-key", "ORCH|task=x|phase=operation|route=normal|mystery=x\nbody", expected_code="UNKNOWN_KEY", expected_message="Unknown ORCH key 'mystery'.")
    add("duplicate-key", "ORCH|task=x|task=y\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="ORCH key 'task' occurs more than once.")
    add("empty-task", "ORCH|task= |phase=operation|route=normal\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="The ORCH task value cannot be empty.")
    add("invalid-eta", "ORCH|task=x|phase=operation|route=normal|eta=0m\nbody", expected_code="MALFORMED_ORCH_LINE", expected_message="ORCH eta must use eta=<N>m with N an integer from 1..600.")
    add("missing-phase", "ORCH|task=x|route=normal\nbody", expected_code="MISSING_REQUIRED_KEY", expected_message="Missing required ORCH key 'phase'.")
    add("missing-implementation-scope", "ORCH|task=x|phase=implementation|route=normal\nbody", expected_code="MISSING_REQUIRED_KEY", expected_message="Missing required ORCH key 'scope' for implementation phase.")
    bad_config = _orch_config()
    bad_config["allowed_phases"] = []
    add("invalid-config", "ORCH|task=x|phase=operation|route=normal\nbody", case_config=bad_config, expected_code="CONFIG_INVALID", expected_message="Policy configuration is invalid: allowed_phases must be a non-empty list of unique non-empty strings")
    add("invalid-phase-enum", "ORCH|task=x|phase=bogus|route=normal\nbody", expected_code="INVALID_ENUM_VALUE", expected_message="ORCH phase value 'bogus' is not allowed.")
    add("invalid-scope-traversal", "ORCH|task=x|phase=implementation|route=normal|scope=src/../secret\nbody", expected_code="INVALID_SCOPE", expected_message="Scope must contain repository-relative paths without traversal.")
    add("unbounded-scope", "ORCH|task=x|phase=implementation|route=normal|scope=.\nbody", expected_code="UNBOUNDED_SCOPE", expected_message="Scope cannot resolve to the whole repository.")
    add("invalid-plan-pairing", "ORCH|task=x|phase=plan|route=normal\nbody", expected_code="INVALID_ENUM_VALUE", expected_message="The valid plan pairing is phase=plan with route=plan_standard; phase=plan cannot use other routes and route=plan_standard cannot use other phases.")
    add("missing-sol-basis", "ORCH|task=x|phase=operation|route=normal\nbody", expected_code="SOL_ROUTE_NEEDS_BASIS", expected_message="route=normal runs gpt-6-sol. Add basis=<the design choice left to the worker> (for escalated: the concrete miss). If the prompt already states the decisions, use route=subtle (gpt-6-luna) instead.")
    agy = _orch_config()
    agy["coder_agent"] = "agy"
    for route in agy["routes"].values():
        route.update(model="gemini-3.8-pro", effort="max")
    add("missing-agy-basis", "ORCH|task=x|phase=operation|route=normal\nbody", case_config=agy, expected_code="SOL_ROUTE_NEEDS_BASIS", expected_message="route=normal requires basis=<the design choice left to the worker> (for escalated: the concrete miss).")
    return cases


def _orch_oracle_results():
    from dataclasses import asdict

    results = []
    for case in _orch_cases():
        item = dict(case)
        parsed = policy.parse_orch_prompt(**case["input"])
        item["ok"] = parsed.valid
        item["result"] = json.loads(json.dumps(asdict(parsed)))
        results.append(item)
    return results


def test_python_orch_golden_fixture():
    actual = _orch_oracle_results()
    expected = json.loads(ORCH_FIXTURE.read_text(encoding="utf-8"))
    assert actual == expected
    for case in actual:
        result = case["result"]
        if case["name"].startswith("valid-"):
            assert case["ok"] is True and result["code"] == policy.VALID, case["name"]
        else:
            assert case.get("expected_code"), f"{case['name']} must declare its expected code"
            assert case["ok"] is False and result["code"] == case["expected_code"], case["name"]
            assert result["message"] == case["expected_message"], (
                f"{case['name']} failed on the wrong branch: {result['message']}"
            )


def test_go_orch_golden_parity():
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        ["go", "test", "./internal/policy", "-run", "^TestPythonOrchGoldenParity$", "-count=1"],
        cwd=ROOT.parents[2] / "go", text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def _route_thread_cases():
    config = _valid()
    request = {"task_id": "task-1", "route": "normal", "model": "gpt-5", "effort": "low"}
    cases = []

    def add(name, kind, inputs, *, expected_code=None):
        case = {"name": name, "kind": kind, "input": inputs}
        if expected_code is not None:
            case["expected_code"] = expected_code
        cases.append(case)

    add("valid-route", "route", {"route": "normal", "config": config})
    add("invalid-route-no-fallback", "route", {"route": "future", "config": config}, expected_code="INVALID_ENUM_VALUE")
    add("non-string-route", "route", {"route": 7, "config": config}, expected_code="INVALID_ENUM_VALUE")
    bad_config = _valid()
    bad_config["allowed_phases"] = []
    add("invalid-route-config", "route", {"route": "normal", "config": bad_config}, expected_code="CONFIG_INVALID")

    add("valid-thread", "compatibility", {"requested": request, "recorded": request})
    add("invalid-request-identity", "compatibility", {"requested": {"task_id": ""}, "recorded": request}, expected_code="INVALID_REQUEST")
    add("missing-live-thread", "compatibility", {"requested": request, "recorded": None}, expected_code="NO_LIVE_THREAD")
    add("corrupt-live-thread", "compatibility", {"requested": request, "recorded": {"task_id": "task-1"}}, expected_code="INVALID_LIVE_THREAD")
    for field, code in (("task_id", "TASK_ID_MISMATCH"), ("route", "ROUTE_MISMATCH"), ("model", "MODEL_MISMATCH"), ("effort", "EFFORT_MISMATCH")):
        recorded = dict(request)
        recorded[field] = "different"
        add(f"{field}-mismatch", "compatibility", {"requested": request, "recorded": recorded}, expected_code=code)
    add("mismatch-precedence", "compatibility", {
        "requested": request,
        "recorded": {key: "different" for key in request},
    }, expected_code="TASK_ID_MISMATCH")

    add("fresh-compatible-denied", "fresh", {"requested": request, "recorded": request, "reason": None}, expected_code="USE_CODEX_REPLY")
    for reason in ("user_override", "reanchor"):
        add(f"fresh-{reason}-override", "fresh", {"requested": request, "recorded": request, "reason": reason}, expected_code="FRESH_THREAD_OVERRIDE")
    add("fresh-invalid-request", "fresh", {"requested": {"task_id": ""}, "recorded": request, "reason": None}, expected_code="INVALID_REQUEST")
    add("fresh-missing-live-thread", "fresh", {"requested": request, "recorded": None, "reason": None}, expected_code="NO_LIVE_THREAD")
    add("fresh-corrupt-live-thread", "fresh", {"requested": request, "recorded": {"task_id": "task-1"}, "reason": None}, expected_code="INVALID_LIVE_THREAD")
    add("fresh-mismatch", "fresh", {"requested": request, "recorded": {**request, "task_id": "different"}, "reason": None}, expected_code="TASK_ID_MISMATCH")
    return cases


def _route_thread_oracle_results():
    from dataclasses import asdict

    results = []
    for case in _route_thread_cases():
        item = dict(case)
        inputs = case["input"]
        if case["kind"] == "route":
            result = policy.resolve_route(inputs["route"], inputs["config"])
        elif case["kind"] == "compatibility":
            result = policy.thread_compatibility(inputs["requested"], inputs["recorded"])
        else:
            result = policy.fresh_codex_decision(inputs["requested"], inputs["recorded"], inputs["reason"])
        item["result"] = asdict(result)
        results.append(item)
    return results


def test_python_route_thread_golden_fixture():
    actual = _route_thread_oracle_results()
    assert actual == json.loads(ROUTE_THREAD_FIXTURE.read_text(encoding="utf-8"))
    for case in actual:
        result = case["result"]
        if case["name"].startswith("valid-"):
            assert result.get("valid", result.get("compatible")) is True, case["name"]
        if "expected_code" in case:
            assert result["code"] == case["expected_code"], case["name"]


def test_go_route_thread_golden_parity():
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        ["go", "test", "./internal/policy", "-run", "^TestPythonRouteThreadGoldenParity$", "-count=1"],
        cwd=ROOT.parents[2] / "go", text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
