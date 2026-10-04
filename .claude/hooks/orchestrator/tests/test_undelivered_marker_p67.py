import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import codex
import ledger
import policy
import project_root


@pytest.fixture()
def config():
    return json.loads((Path(__file__).parents[1] / "config.json").read_text(encoding="utf-8"))


@pytest.fixture()
def workspace():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / "undelivered-p67" / uuid.uuid4().hex
    repo, state = root / "repo", root / "state"
    repo.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "config", "user.email", "tests@example.invalid"], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "config", "user.name", "Hook Tests"], cwd=repo, check=True, timeout=10)
    (repo / "file.py").write_text("value = 1\n", encoding="utf-8")
    subprocess.run(["git", "add", "."], cwd=repo, check=True, timeout=10)
    subprocess.run(["git", "commit", "-qm", "initial"], cwd=repo, check=True, timeout=10)
    try:
        yield repo, state
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)
        shutil.rmtree(root, onerror=make_writable)


def routed_event():
    return {
        "session_id": "p67-session", "tool_name": codex.CODEX_TOOL,
        "tool_use_id": "p67-call", "tool_input": {
            "prompt": "ORCH|task=p67-task|phase=implementation|route=normal|reason=new_task|basis=test|scope=file.py\nInspect file.py",
            "label": "p67 task",
        },
    }


def marker_path(state, event):
    return state / "pending" / f"undelivered-{codex._call_key(event)}.json"


def seed_pending(repo, state, config, event):
    codex.pre_tool_use(event, config=config, state_root=state, repo_root=repo)
    pending_path = codex._pending_path(state, event)
    pending = json.loads(pending_path.read_text(encoding="utf-8"))
    project = project_root.project_key(repo.resolve())
    session = ledger.SessionLedger(state / "projects" / project).load("p67-task")["record"]
    assert session["pending_call_baseline"]
    leases = ledger.ScopeLeases(state / "projects" / project)
    assert leases._read()["owners"]
    return pending_path, project, session, leases


def post_event(event):
    return {**event, "hook_event_name": "PostToolUse", "tool_response": {
        "structuredContent": {"threadId": "p67-thread", "content": "Done"},
    }}


def test_post_marker_forces_full_and_cleans_routed_pending(workspace, config, monkeypatch):
    repo, state = workspace
    event = routed_event()
    pending_path, project, _session, leases = seed_pending(repo, state, config, event)
    marker = marker_path(state, event)
    marker.write_text(json.dumps({"schema": "purser.undelivered.v1", "call_key": codex._call_key(event),
                                  "tool_name": event["tool_name"], "reason": "adapter failed",
                                  "recorded_at": "2026-10-04T00:00:00Z"}), encoding="utf-8")
    observations = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observations.append((args, kwargs)))

    response = codex.post_tool_use(post_event(event), config=config, state_root=state, repo_root=repo,
                                   reviewer=lambda **kwargs: pytest.fail("marker calls must not use AUDIT review"))

    bundle = json.loads(response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"])
    assert bundle["claude_review_mode"] == policy.FULL
    assert not pending_path.exists() and not marker.exists()
    assert all(owner["state"] == "RELEASED" for owner in leases._read()["owners"].values())
    assert ledger.SessionLedger(state / "projects" / project).load("p67-task")["record"]["pending_call_baseline"] is None
    assert [args[1] for args, _ in observations if len(args) > 1 and args[0] == "emit"].count("ROUTING_PROTOCOL_VIOLATION") == 1


def test_post_tool_use_failure_marker_releases_and_cleans_routed_pending(workspace, config, monkeypatch):
    repo, state = workspace
    event = routed_event()
    pending_path, project, _session, leases = seed_pending(repo, state, config, event)
    marker = marker_path(state, event)
    marker.write_text(json.dumps({"schema": "purser.undelivered.v1", "call_key": codex._call_key(event),
                                  "tool_name": event["tool_name"], "reason": "adapter failed",
                                  "recorded_at": "2026-10-04T00:00:00Z"}), encoding="utf-8")
    observations = []
    monkeypatch.setattr(codex, "_observe", lambda *args, **kwargs: observations.append((args, kwargs)))

    response = codex.post_tool_use_failure({**event, "hook_event_name": "PostToolUseFailure",
                                            "error": "tool failed"}, state_root=state)

    assert "FULL Claude review is required" in response["hookSpecificOutput"]["additionalContext"]
    assert not pending_path.exists() and not marker.exists()
    assert all(owner["state"] == "RELEASED" for owner in leases._read()["owners"].values())
    assert ledger.SessionLedger(state / "projects" / project).load("p67-task")["record"]["pending_call_baseline"] is None
    assert [args[1] for args, _ in observations if len(args) > 1 and args[0] == "emit"].count("ROUTING_PROTOCOL_VIOLATION") == 1


def test_post_without_marker_keeps_existing_review_path(workspace, config):
    repo, state = workspace
    event = routed_event()
    pending_path, _project, _session, _leases = seed_pending(repo, state, config, event)
    reviewed = []

    response = codex.post_tool_use(post_event(event), config=config, state_root=state, repo_root=repo,
                                   reviewer=lambda **kwargs: reviewed.append(kwargs) or {
                                       "result": policy.NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE,
                                       "validation": "PASS", "findings": [],
                                   })

    bundle = json.loads(response["hookSpecificOutput"][config["post_tool_output_field"]][0]["text"])
    assert not reviewed and bundle["routing_enforced"] is True
    assert not pending_path.exists()
