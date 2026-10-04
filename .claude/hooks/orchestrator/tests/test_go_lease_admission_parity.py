import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_lease" / "admission.json"
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


@pytest.fixture()
def tmp_path():
    root = ROOT / ".tmp" / "go-lease-admission" / uuid.uuid4().hex
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


def _root(base):
    root = base / "project" / ".codex" / "state"
    root.mkdir(parents=True)
    return root


def _prepare(root, case):
    (root / "scope_leases.json").write_text(json.dumps(
        {"version": 1, "owners": case["owners"]}, sort_keys=True,
    ), encoding="utf-8")
    if case.get("lock_failure"):
        (root / "scope_leases.lock").mkdir()


def _python(root, case, monkeypatch):
    now = case["now"]
    monkeypatch.setattr(ledger.time, "time", lambda: now)
    if case.get("fail_write"):
        def fail_replace(_source, _target):
            raise PermissionError("injected durable write failure")
        monkeypatch.setattr(ledger.os, "replace", fail_replace)
    store = ledger.ScopeLeases(root)
    results = [store.acquire(
        request["owner_key"], request["task_id"], request["scope"], **request.get("options", {}),
    ) for request in case["requests"]]
    try:
        owners = store._read()["owners"]
    except (OSError, ValueError, TypeError):
        owners = case["owners"]
    return {"results": results, "owners": owners}


def _go(root, case):
    env = GO_ENV.copy()
    env.update(
        PURSER_LEASE_ADMISSION_ROOT=str(root),
        PURSER_LEASE_ADMISSION_INPUT=json.dumps({
            "now": case["now"], "requests": case["requests"], "fail_write": case.get("fail_write", False),
        }),
    )
    result = subprocess.run(
        ["go", "test", "-run", "^TestAdmissionParityOutcome$", "-count=1", "-v", "./internal/lease"],
        cwd=ROOT / "go", env=env, capture_output=True, text=True, check=True,
    )
    line = next(line for line in result.stdout.splitlines() if "LEASE_ADMISSION_RESULT:" in line)
    return json.loads(line.split("LEASE_ADMISSION_RESULT:", 1)[1])


def test_python_and_go_admission_match_golden_cases(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python .claude/hooks/orchestrator/ledger.py ScopeLeases.acquire"
    for case in fixture["cases"]:
        python_root = _root(tmp_path / "python" / case["name"])
        go_root = _root(tmp_path / "go" / case["name"])
        _prepare(python_root, case)
        _prepare(go_root, case)
        prior = {root: (root / "scope_leases.json").read_bytes() for root in (python_root, go_root)}
        with monkeypatch.context() as scoped:
            python = _python(python_root, case, scoped)
        go = _go(go_root, case)
        assert go == python, case["name"]
        assert [result["code"] for result in python["results"]] == case["codes"], case["name"]
        assert {key: owner["state"] for key, owner in python["owners"].items()} == case["states"], case["name"]
        if case.get("fail_write") or case.get("lock_failure"):
            for root in (python_root, go_root):
                assert (root / "scope_leases.json").read_bytes() == prior[root]
