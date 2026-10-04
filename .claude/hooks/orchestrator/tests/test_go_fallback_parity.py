import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "fallback.json"
sys.path.insert(0, str(ORCHESTRATOR))
import active_coder_state


def test_p33_python_active_coder_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python active_coder_state.read"
    for case in fixture["cases"]:
        project = tmp_path / case["name"]
        project.mkdir()
        path = active_coder_state.state_path(project, tmp_path / "runtime")
        if case["raw"] is not None:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(case["raw"], encoding="utf-8")
        try:
            actual = active_coder_state.read(project, tmp_path / "runtime")
        except (ValueError, json.JSONDecodeError):
            assert case.get("error") is True, case["name"]
        else:
            assert not case.get("error"), case["name"]
            assert actual == case["expected"], case["name"]
