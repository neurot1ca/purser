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
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_lease" / "retention.json"
GO_ENV = {**os.environ, "GOCACHE": str(ROOT / ".tmp" / "go-cache"),
          "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp")}
sys.path.insert(0, str(ORCHESTRATOR))
import ledger
@pytest.fixture()
def tmp_path():
    root = ROOT / ".tmp" / "go-lease-retention" / uuid.uuid4().hex
    root.mkdir(parents=True, exist_ok=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)
def _input():
    owners = {
        "active": {"state": "ACTIVE", "scope": ["src"], "task_id": "same", "job_id": "active-job", "acquired_at": 90, "retention_after": 200},
        "nested": {"state": "ACTIVE", "scope": ["src/nested"], "task_id": "nested", "acquired_at": 95},
        "disjoint": {"state": "ACTIVE", "scope": ["docs"], "task_id": "disjoint", "acquired_at": 96},
        "released": {"state": "RELEASED", "scope": ["old"], "task_id": "released", "acquired_at": 10, "released_at": 20},
        "expired": {"state": "ACTIVE", "scope": ["gone"], "task_id": "expired", "acquired_at": 1, "deadline_at": 99},
        "same-2": {"state": "QUEUED", "scope": ["src/other"], "task_id": "same", "job_id": "job", "queued_at": 97},
        "same-3": {"state": "QUEUED", "scope": ["archive"], "task_id": "same", "job_id": "job-2", "queued_at": 98},
    }
    ops = []
    for owner in ("active", "released", "unknown", "expired"):
        ops.extend([{"method": "set_retention_after", "owner": owner, "value": 150},
                    {"method": "owner_state", "owner": owner},
                    {"method": "overlapping_owners", "owner": owner}])
    ops += [
        {"method": "extend_retention", "owner": "same-2", "value": 250},
        {"method": "extend_retention", "owner": "active", "value": 120},
        {"method": "active"},
        {"method": "release_task", "task_id": "same"},
        {"method": "record_conflicts", "paths": sorted(["src/file", "../escape", "/absolute", "z.txt", "a.txt"])},
    ]
    return {"name": "retention-methods", "now": 100, "owners": owners,
            "conflicts": [{"path": "src/file", "recorded_at": 95},
                          {"path": "keep", "recorded_at": 99},
                          {"path": "stale", "recorded_at": -604701}], "operations": ops}
def _root(base):
    root = base / "project" / ".codex" / "state"
    root.mkdir(parents=True)
    return root
def _state(store):
    return store._read()
def _python(root, case, monkeypatch):
    monkeypatch.setattr(ledger.time, "time", lambda: case["now"])
    store = ledger.ScopeLeases(root)
    snapshots = []
    for op in case["operations"]:
        method = op["method"]
        if method == "set_retention_after": result = store.set_retention_after(op["owner"], op["value"])
        elif method == "owner_state": result = store.owner_state(op["owner"])
        elif method == "overlapping_owners":
            try: result = store.overlapping_owners(op["owner"])
            except ValueError as error: result = {"error": str(error)}
        elif method == "extend_retention": result = store.extend_retention(op["owner"], op["value"])
        elif method == "active": result = store.active()
        elif method == "release_task": result = store.release_task(op["task_id"])
        else:
            store.record_conflicts(op["paths"])
            result = None
        snapshots.append({"result": result, "state": _state(store)})
    return snapshots
def _go(root, fixture):
    env = GO_ENV | {"PURSER_LEASE_RETENTION_ROOT": str(root), "PURSER_LEASE_RETENTION_FIXTURE": str(fixture)}
    result = subprocess.run(["go", "test", "-run", "^TestRetentionParityReplay$", "-count=1", "-v", "./internal/lease"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True, check=True)
    line = next(line for line in result.stdout.splitlines() if "LEASE_RETENTION_RESULT:" in line)
    return json.loads(line.split("LEASE_RETENTION_RESULT:", 1)[1])
def test_python_and_go_retention_match(tmp_path, monkeypatch):
    case = _input()
    root = _root(tmp_path / "python")
    (root / "scope_leases.json").write_text(json.dumps({"version": 1, "owners": case["owners"],
                                                          "conflicts": case["conflicts"]}), encoding="utf-8")
    with monkeypatch.context() as scoped:
        expected = _python(root, case, scoped)
    fixture = {"oracle": "Python .claude/hooks/orchestrator/ledger.py ScopeLeases retention methods",
               "case": case, "snapshots": expected}
    if os.environ.get("LEASE_RETENTION_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, separators=(",", ":")) + "\n", encoding="utf-8")
    committed = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert committed["case"] == case
    assert committed["snapshots"] == expected
    go_root = _root(tmp_path / "go")
    (go_root / "scope_leases.json").write_text(json.dumps({"version": 1, "owners": case["owners"],
                                                             "conflicts": case["conflicts"]}), encoding="utf-8")
    got = _go(go_root, FIXTURE)
    assert got == committed["snapshots"]
    assert expected[0]["result"]["code"] == "LEASE_RETENTION_SET"
    assert expected[3]["result"]["code"] == "LEASE_OWNER_MISSING"
    assert expected[-1]["state"]["conflicts"] == [
        {"path": "src/file", "recorded_at": 100}, {"path": "a.txt", "recorded_at": 100},
        {"path": "z.txt", "recorded_at": 100}, {"path": "keep", "recorded_at": 99},
    ]
