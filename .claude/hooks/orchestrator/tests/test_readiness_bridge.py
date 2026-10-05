import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).parents[1]
BRIDGE = ROOT / "readiness_bridge.py"
sys.path.insert(0, str(ROOT))
_CHILD = r'''
import os, runpy, sys
import readiness_check
case = os.environ.get("BRIDGE_CASE")
if case == "raise":
    readiness_check.run_checker = lambda *args, **kwargs: (_ for _ in ()).throw(RuntimeError("stub"))
else:
    readiness_check.run_checker = lambda *args, **kwargs: {
        "verdict": "NO_UNMET_FOUND", "criterion": None, "evidence": []}
runpy.run_path(sys.argv[1], run_name="__main__")
'''


def _input():
    return {"grounded": True, "objective": "objective", "acceptance_criteria": "criterion",
            "attributed_diff": "diff", "worker_report_untrusted": "done", "agent": "agy",
            "model": "gemini-test", "repo_root": "/repo"}


def _run(payload, case=None):
    env = dict(os.environ)
    if case:
        env["BRIDGE_CASE"] = case
    else:
        env.pop("BRIDGE_CASE", None)
    return subprocess.run([sys.executable, "-c", _CHILD, str(BRIDGE)], input=payload,
                          text=True, capture_output=True, env=env, cwd=ROOT, check=False)


def test_readiness_bridge_contract():
    proc = _run(json.dumps(_input()))
    assert proc.returncode == 0
    assert proc.stderr == ""
    assert json.loads(proc.stdout) == {"verdict": "NO_UNMET_FOUND", "criterion": None, "evidence": []}


def test_readiness_bridge_rejects_malformed_input():
    proc = _run("{")
    assert proc.returncode != 0
    assert json.loads(proc.stdout)["error"] == "INVALID_INPUT"


def test_readiness_bridge_checker_exception_is_unavailable():
    proc = _run(json.dumps(_input()), "raise")
    assert proc.returncode == 0
    assert json.loads(proc.stdout) == {"verdict": "UNAVAILABLE", "criterion": None, "evidence": []}
