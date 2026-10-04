"""P65 Python oracle for Go Pre partial-write recovery parity."""
import json
from pathlib import Path
import subprocess
import sys

import pytest

ROOT = Path(__file__).parents[4]
sys.path.insert(0, str(Path(__file__).parents[1]))
import codex
import ledger

FIXTURE = Path(__file__).parent / "fixtures" / "go_policy" / "pre_recovery.json"


@pytest.mark.parametrize("case", json.loads(FIXTURE.read_text(encoding="utf-8"))["cases"], ids=lambda c: c["name"])
def test_pre_recovery_p65_matches_fixture(case, tmp_path, monkeypatch):
    repo, state = tmp_path / "repo", tmp_path / "state"
    repo.mkdir()
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.email", "p65@example.invalid"], cwd=repo, check=True)
    subprocess.run(["git", "config", "user.name", "P65 fixture"], cwd=repo, check=True)
    (repo / "src").mkdir()
    (repo / "src" / "a.go").write_text("package p65\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-qm", "fixture baseline"], cwd=repo, check=True)
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.setenv("USERPROFILE", str(tmp_path / "home"))
    name, tool = case["name"], case["tool"]
    event = {"hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": name,
             "session_id": "p65-session", "cwd": str(repo),
             "tool_input": {"prompt": f"ORCH|task={name}|phase=implementation|route=subtle|scope=src/a.go\nImplement the change.",
                            "mode": "new", "timeoutSeconds": 90, "label": "P65 recovery"}}
    pending = codex._pending_path(state, event)
    original_atomic = codex._atomic_json_write
    if case["failure"] == "after_lease":
        failed = False
        def fail_pending(path, value):
            nonlocal failed
            if Path(path) == pending and not failed:
                failed = True
                raise RuntimeError("p65 injected failure")
            return original_atomic(path, value)
        monkeypatch.setattr(codex, "_atomic_json_write", fail_pending)
    elif case["failure"] == "after_pending":
        monkeypatch.setattr(ledger, "build_session_record", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("p65 injected failure")))
    elif case["failure"] == "after_session":
        original_observe = codex._observe
        def fail_after_save(action, event_type=None, **kwargs):
            if action == "emit" and event_type == "TASK_ROUTE_SELECTED":
                raise RuntimeError("p65 injected failure")
            return original_observe(action, event_type, **kwargs)
        monkeypatch.setattr(codex, "_observe", fail_after_save)
    elif case["failure"] == "async_ready":
        writes = 0
        def fail_ready(path, value):
            nonlocal writes
            if Path(path) == pending:
                writes += 1
                if writes == 2:
                    raise RuntimeError("p65 injected failure")
            return original_atomic(path, value)
        monkeypatch.setattr(codex, "_atomic_json_write", fail_ready)
    elif case["failure"] == "bypass_persist":
        monkeypatch.setattr(ledger, "build_session_record", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("p65 injected failure")))
        monkeypatch.setattr(codex, "_write_pre_bypass", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("p65 injected failure")))
    elif case["failure"] == "panic":
        monkeypatch.setattr(ledger, "capture_baseline", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("panic: p65 injected panic")))

    response = codex.pre_tool_use(event, state_root=state, repo_root=repo)["hookSpecificOutput"]
    assert response == case["output"]
    diagnostics = state / "diagnostics.jsonl"
    assert diagnostics.exists() is case["diagnostic"]
    record = json.loads(pending.read_text(encoding="utf-8")) if pending.exists() else None
    assert (record is None or record.get("kind") == "pretool_bypass") is case["pending_absent"]
    expected_bypass = case["bypass"]
    if expected_bypass is None:
        assert not record or record.get("kind") != "pretool_bypass"
    else:
        assert record and record["kind"] == "pretool_bypass"
        for field, expected in expected_bypass.items():
            if expected == "__PROJECT_KEY__":
                expected = codex.project_root.project_key(repo)
            elif expected == "__REPO__":
                expected = str(repo)
            assert record[field] == expected, field
    project = codex.project_root.project_key(repo)
    lease_file = state / "projects" / project / "scope_leases.json"
    lease_state = json.loads(lease_file.read_text(encoding="utf-8")) if lease_file.exists() else {"owners": {}}
    states = [owner.get("state") for owner in lease_state.get("owners", {}).values()]
    observed_lease_state = states[-1] if states else "NONE"
    assert observed_lease_state == case["lease_state"]
