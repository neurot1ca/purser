"""Python oracle for the Go queued-baseline admission replay."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_queued_baseline" / "admission.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger

CASES = ("missing_job", "already_bound", "lease_not_held", "index_not_ready", "success")
CALL = "queued-call"
JOB_ID = "28391999783af3cfac016146a4fb9b4dc0abc0e3cf8e3115692ee36fe25e10b3"
OWNER = "owner-key"
NOW = 1800000000.0


@pytest.fixture
def tmp_path():
    root = ROOT / ".tmp" / "go-queued-baseline" / uuid.uuid4().hex
    root.mkdir(parents=True)
    try:
        yield root
    finally:
        shutil.rmtree(root, ignore_errors=True)


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _setup(root, name):
    repo = root / "repo"
    (repo / "src").mkdir(parents=True)
    (repo / "src" / "file").write_text("baseline\n", encoding="utf-8")
    env = os.environ.copy()
    env.update({"GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z",
                "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"})
    for args in (("init", "-q"), ("add", "src/file"), ("commit", "-qm", "baseline")):
        subprocess.run(["git", *args], cwd=repo, env=env, check=True, capture_output=True)
    _write(root / "codex_exec_mcp" / "jobs" / f"{JOB_ID}.json",
           {"jobId": JOB_ID, "call": CALL, "state": "QUEUED"})
    project = root / "projects" / "project"
    pending_path = root / "pending" / f"{OWNER}.json"
    if name == "missing_job":
        return repo, project, pending_path
    leases = ledger.ScopeLeases(project)
    if name == "lease_not_held":
        assert leases.acquire("blocker", "blocker-task", ["src"], job_id="blocker-job")["status"] == ledger.OK
    assert leases.acquire(OWNER, "task", ["src"], job_id=JOB_ID, queue_conflict=True)["status"] == ledger.OK
    pending = {"job_id": JOB_ID, "async_call": CALL, "lease_queued": True, "lease_owner": OWNER,
               "project_key": "project", "repo_root": str(repo), "task_id": "task", "scope": ["src"]}
    _write(pending_path, pending)
    if name != "index_not_ready":
        _write(root / "async_jobs" / f"{JOB_ID}.json",
               {"jobId": JOB_ID, "call": CALL, "call_key": OWNER, "project_key": "project"})
    baseline = {"status": "UNKNOWN", "code": "QUEUED"}
    if name == "already_bound":
        baseline = ledger.capture_baseline(repo, ["src"])
        pending["baseline"] = baseline
        _write(pending_path, pending)
    saved = ledger.SessionLedger(project).save(
        ledger.build_session_record("task", None, "", "", "", pending_call_baseline=baseline)
    )
    assert saved["status"] == ledger.OK
    return repo, project, pending_path


def _read(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None


def _normalize(value, root):
    if isinstance(value, dict):
        return {key: "__TIME__" if key.endswith("_at") else _normalize(item, root)
                for key, item in value.items()}
    if isinstance(value, list):
        return [_normalize(item, root) for item in value]
    if isinstance(value, str):
        return value.replace(str(root), "__ROOT__")
    return value


def _oracle_case(root, name, monkeypatch):
    monkeypatch.setattr(ledger.time, "time", lambda: NOW)
    _, project, pending_path = _setup(root, name)
    try:
        ready = codex.bind_queued_baseline(root, JOB_ID)
    except FileNotFoundError:
        ready = False
    session_path = ledger.SessionLedger(project).path_for_task("task")
    return _normalize({"name": name, "ready": ready, "pending": _read(pending_path),
                       "session": _read(session_path), "leases": _read(project / "scope_leases.json")}, root)


def test_go_queued_baseline_fixture_matches_python_oracle(monkeypatch, tmp_path):
    actual = [_oracle_case(tmp_path / name, name, monkeypatch) for name in CASES]
    if os.environ.get("PURSER_REGEN_GO_QUEUED_BASELINE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, indent=2) + "\n", encoding="utf-8")
    assert FIXTURE.is_file(), f"missing {FIXTURE}; run with PURSER_REGEN_GO_QUEUED_BASELINE=1"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == actual
