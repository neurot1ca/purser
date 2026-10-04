import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_lease" / "reconcile.json"
GO_TMP = ROOT / ".tmp"
GO_ENV = {
    **os.environ,
    "GOCACHE": str(GO_TMP / "go-cache"),
    "TMP": str(GO_TMP),
    "TEMP": str(GO_TMP),
    "TMPDIR": str(GO_TMP),
}
sys.path.insert(0, str(ORCHESTRATOR))
import ledger


def _root(base):
    root = base / "project" / ".codex" / "state"
    root.mkdir(parents=True)
    return root


def _prepare(root, case):
    (root / "scope_leases.json").write_text(json.dumps(
        {"version": 1, "owners": case["owners"]}, sort_keys=True,
    ), encoding="utf-8")
    jobs = root.parent.parent / "codex_exec_mcp" / "jobs"
    for job_id, record in case["jobs"].items():
        jobs.mkdir(parents=True, exist_ok=True)
        (jobs / f"{job_id}.json").write_text(json.dumps(record), encoding="utf-8")


def _python(root, now):
    store = ledger.ScopeLeases(root)
    value = store._read()
    changed = store._reconcile(value, now)
    changed = store._promote(value, now) or changed
    return {"changed": changed, "owners": value["owners"]}


def _go(root, now):
    env = GO_ENV.copy()
    env.update(PURSER_LEASE_RECONCILE_ROOT=str(root), PURSER_LEASE_RECONCILE_NOW=str(now))
    result = subprocess.run(
        ["go", "test", "-run", "^TestReconcileParityOutcome$", "-count=1", "-v", "./internal/lease"],
        cwd=ROOT / "go", env=env, capture_output=True, text=True, check=True,
    )
    line = next(line for line in result.stdout.splitlines() if "LEASE_RECONCILE_RESULT:" in line)
    return json.loads(line.split("LEASE_RECONCILE_RESULT:", 1)[1])


def test_python_and_go_reconciliation_match_golden_cases(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python .claude/hooks/orchestrator/ledger.py ScopeLeases._reconcile and _promote"
    for case in fixture["cases"]:
        python_root = _root(tmp_path / "python" / case["name"])
        go_root = _root(tmp_path / "go" / case["name"])
        _prepare(python_root, case)
        _prepare(go_root, case)
        python = _python(python_root, case["now"])
        go = _go(go_root, case["now"])
        assert go == python, case["name"]
        assert python["changed"] is case["changed"], case["name"]
        assert {key: owner["state"] for key, owner in python["owners"].items()} == case["states"], case["name"]
