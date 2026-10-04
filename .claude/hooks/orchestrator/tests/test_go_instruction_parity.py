import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "instructions.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex


def test_p42_python_oracle_matches_fixture(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python codex.py pass 42 rewrite and instruction helpers"

    for case in fixture["full_suite_cases"]:
        repo = tmp_path / case["name"].replace(":", "-")
        repo.mkdir()
        (repo / "AGENTS.md").write_text(case["policy"], encoding="utf-8")
        acceptance = codex._claude_runs_full_suite_at_acceptance(repo)
        asks = codex._asks_worker_to_run_full_suite(case["prompt"])
        assert acceptance is case["acceptance"], case["name"]
        assert asks is case["asks"], case["name"]
        assert (acceptance and asks) is case["required"], case["name"]

    for case in fixture["append_cases"]:
        actual = codex._append_worker_validation_instruction(
            case["prompt"], full_suite_required=case["full_suite"]
        )
        assert actual.encode("utf-8") == case["expected"].encode("utf-8"), case["name"]

    for case in fixture["rewrite_cases"]:
        source = case["input"]
        expected = dict(source)
        if isinstance(expected.get("prompt"), str) and case["phase"] == "implementation":
            expected["prompt"] = codex._append_worker_validation_instruction(
                expected["prompt"], full_suite_required=case["full_suite"]
            )
        if case["phase"] == "plan" and case["new_call"] and isinstance(expected.get("config"), dict):
            config = dict(expected["config"])
            for key in ("sandbox_mode", "approval_policy", "sandbox_workspace_write"):
                config.pop(key, None)
            expected["config"] = config
        if case["new_call"] and case["agent"] == "codex" and "developer-instructions" not in source:
            expected["developer-instructions"] = codex.INDEPENDENCE_INSTRUCTIONS
        assert expected == case["expected"], case["name"]

    assert codex.INDEPENDENCE_INSTRUCTIONS == fixture["rewrite_cases"][1]["expected"]["developer-instructions"]
