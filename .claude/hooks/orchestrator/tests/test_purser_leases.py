"""CLI checks for durable scope lease inspection and explicit release."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import subprocess
import sys

import ledger


CLI = Path(__file__).resolve().parents[1] / "purser_leases.py"


def _run(*args: str) -> dict:
    completed = subprocess.run([sys.executable, str(CLI), *args], cwd=CLI.parent,
                               capture_output=True, text=True, check=True)
    return json.loads(completed.stdout)


def test_list_shows_active_task_scope_age_and_job_state(tmp_path, monkeypatch):
    state = tmp_path / "state"
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    store = ledger.ScopeLeases(state / "projects" / "project-one")
    assert store.acquire("sync", "task-sync", ["src/a.py"])["status"] == ledger.OK
    call = "cli-job"
    job_id = hashlib.sha256(call.encode()).hexdigest()
    assert store.acquire("async", "task-async", ["src/b.py"], job_id=job_id)["status"] == ledger.OK
    path = state / "codex_exec_mcp" / "jobs" / f"{job_id}.json"
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps({"jobId": job_id, "call": call, "state": "RUNNING"}),
                    encoding="utf-8")

    rows = _run("--list")["active"]
    assert {row["task_id"] for row in rows} == {"task-sync", "task-async"}
    assert {tuple(row["scope"]) for row in rows} == {("src/a.py",), ("src/b.py",)}
    assert all(row["project_key"] == "project-one" and row["age_seconds"] >= 0 for row in rows)
    assert next(row for row in rows if row["task_id"] == "task-sync")["job_state"] is None
    assert next(row for row in rows if row["task_id"] == "task-async")["job_state"] == "RUNNING"


def test_release_names_only_requested_tasks_active_leases(tmp_path, monkeypatch):
    state = tmp_path / "state"
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    first = ledger.ScopeLeases(state / "projects" / "project-one")
    second = ledger.ScopeLeases(state / "projects" / "project-two")
    first.acquire("one", "target-task", ["src/a.py"])
    first.acquire("other", "other-task", ["src/b.py"])
    second.acquire("two", "target-task", ["docs"])

    released = _run("--release", "target-task")["released"]
    assert {(row["project_key"], tuple(row["scope"])) for row in released} == {
        ("project-one", ("src/a.py",)), ("project-two", ("docs",))}
    assert [row["task_id"] for row in _run("--list")["active"]] == ["other-task"]
    assert _run("--release", "target-task")["released"] == []
