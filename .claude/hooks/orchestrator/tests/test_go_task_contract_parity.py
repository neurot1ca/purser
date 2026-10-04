import json
from pathlib import Path
import sys


ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
if str(ORCHESTRATOR) not in sys.path:
    sys.path.insert(0, str(ORCHESTRATOR))

import codex


FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_policy" / "task_contract.json"


def test_go_contract_fixture_matches_python_oracle():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "codex._parse_task_contract"
    for case in fixture["cases"]:
        assert codex._parse_task_contract(
            case["prompt"], case["task_id"], case["phase"], case["scope"]
        ) == (
            case["expected"]["objective"],
            case["expected"]["criteria"],
            case["expected"]["grounded"],
        ), case["name"]
