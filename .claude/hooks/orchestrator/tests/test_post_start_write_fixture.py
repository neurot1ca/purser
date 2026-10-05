"""Capture the real Python codex_start PostToolUse contract for the Go port."""
import hashlib
import json
import os
import sys
from pathlib import Path

import codex
import ledger


FIXTURE = Path(__file__).parent / "fixtures" / "go_policy" / "post_start.json"


def _replace(value, root, project):
    if isinstance(value, str):
        return value.replace(str(root), "__ROOT__").replace(str(project), "__PROJECT_KEY__")
    if isinstance(value, list):
        return [_replace(item, root, project) for item in value]
    if isinstance(value, dict):
        return {key: _replace(child, root, project) for key, child in value.items()}
    return value


def test_post_start_write_fixture(tmp_path):
    cases = []
    for name in ("running", "failed", "malformed", "another_project", "duplicate"):
        root = tmp_path / name
        root.mkdir()
        repo = root / "repo"
        repo.mkdir()
        project = codex.project_root.project_key(repo)
        event = {"hook_event_name": "PostToolUse", "tool_name": sorted(codex.CODEX_START_TOOL_NAMES)[0],
                 "tool_use_id": "start-" + name, "session_id": "post-start-fixture-session", "tool_input": {}}
        call = "fixture-call-" + name
        job_id = hashlib.sha256(call.encode()).hexdigest()
        owner = codex._call_key(event)
        pending = {"async_call": call, "async_ready": True, "session_id": event["session_id"],
                   "tool_use_id": event["tool_use_id"], "project_key": project, "repo_root": str(repo),
                   "lease_owner": owner, "task_id": "post-start-" + name, "route": "normal",
                   "model": "gpt-test", "effort": "medium", "phase": "implementation", "scope": ["src"]}
        pending_path = codex._pending_path(root, event)
        pending_path.parent.mkdir(parents=True)
        codex._atomic_json_write(pending_path, pending)
        acquired = ledger.ScopeLeases(root / "projects" / project).acquire(
            owner, pending["task_id"], pending["scope"], job_id=job_id)
        assert acquired["status"] == ledger.OK
        response_value = {"jobId": job_id, "state": "RUNNING"}
        if name == "failed":
            response_value["state"] = "FAILED"
            response_value["errorCode"] = "START_FAILED"
        elif name == "malformed":
            response_value = {"state": "RUNNING"}
        elif name == "another_project":
            response_value["jobId"] = "a" * 64
        elif name == "duplicate":
            pending["job_id"] = job_id
            codex._atomic_json_write(pending_path, pending)
            codex._atomic_json_write(root / "async_jobs" / f"{job_id}.json", {
                "jobId": job_id, "call": call, "call_key": owner, "session_id": event["session_id"],
                "project_key": project,
            })
        event["tool_response"] = {"structuredContent": response_value}
        result = codex._post_start(event, None, root)
        output = result.get("hookSpecificOutput", {})
        if "additionalContext" in output:
            output["additionalContext"] = output["additionalContext"].replace(str(root), "__ROOT__")
        for field in ("updatedToolOutput", "updatedMCPToolOutput"):
            for item in output.get(field, []):
                for source, target in ((str(root), "__ROOT__"), (sys.executable, "__PYTHON__"),
                                       (str(Path(codex.__file__).resolve().parent / "purser_wait.py"), "__WAITER__")):
                    item["text"] = item["text"].replace(source, target).replace(source.replace("\\", "\\\\"), target)
        index_path = root / "async_jobs" / f"{job_id}.json"
        index = json.loads(index_path.read_text()) if index_path.exists() else None
        pending_after = json.loads(pending_path.read_text()) if pending_path.exists() else None
        lease_file = root / "projects" / project / "scope_leases.json"
        owners = json.loads(lease_file.read_text())["owners"] if lease_file.exists() else {}
        cases.append({"name": name, "output": _replace(result, root, project), "index": _replace(index, root, project),
                      "pending": _replace(pending_after, root, project),
                      "lease_states": sorted(owner["state"] for owner in owners.values())})
    fixture = {"oracle": "codex._post_start+post_tool_use_failure", "cases": cases}
    if os.environ.get("POST_START_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    assert FIXTURE.exists(), f"missing fixture: {FIXTURE} (set POST_START_WRITE_FIXTURE=1 to capture)"
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
