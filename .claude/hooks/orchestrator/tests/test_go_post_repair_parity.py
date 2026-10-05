import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_repair.json"
sys.path.insert(0, str(ORCH))
import codex
REAL_RUN = subprocess.run


def _oracle(monkeypatch):
    cases = []
    base_findings = [{"code": "F1", "message": "mechanical fix"}]
    specs = [
        ("not-eligible", False, "success", base_findings),
        ("success", True, "success", base_findings),
        ("timeout", True, "timeout", base_findings),
        ("nonzero", True, "nonzero", base_findings),
        ("malformed", True, "malformed", base_findings),
        ("prompt-bound", True, "success", [{"code": "F", "message": "é" * 900}]),
    ]
    monkeypatch.setenv("POST_REPAIR_ENV_SENTINEL", "present")
    for name, eligible, outcome, findings in specs:
        captured = {}

        def recording_run(argv, **kwargs):
            captured.update(argv=argv, cwd=kwargs["cwd"], stdin=kwargs["input"], timeout=kwargs["timeout"],
                            env_sentinel=os.environ.get("POST_REPAIR_ENV_SENTINEL"))
            if outcome == "timeout":
                raise subprocess.TimeoutExpired(argv, kwargs["timeout"])
            if outcome == "malformed":
                return object()
            if outcome == "nonzero":
                return subprocess.CompletedProcess(argv, 7, "", "repair rejected")
            return subprocess.CompletedProcess(argv, 0, "repaired", "")

        bundle = {"micro_repair": None, "micro_repair_cleared_review": False}
        if eligible:
            result = None
        else:
            result = None
        # The launcher contract is the fake subprocess trace plus Python result.
        if eligible:
            codex.subprocess.run = recording_run
            try:
                result = codex._attempt_in_hook_micro_repair(
                    "thread-5", findings, Path("fixture-repo"), scope=["src"], timeout_seconds=45,
                )
            finally:
                codex.subprocess.run = REAL_RUN
        cases.append({"name": name, "eligible": eligible, "outcome": outcome,
                      "findings": findings, "expected_result": result, "capture": captured})
    return {"oracle": "codex._attempt_in_hook_micro_repair subprocess contract", "cases": cases}


def test_post_repair_matches_python_fixture(monkeypatch):
    fixture = _oracle(monkeypatch)
    if os.environ.get("POST_REPAIR_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp")}
    result = subprocess.run(["go", "test", "-run", "^TestPostRepairPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
