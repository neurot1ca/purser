import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
from threading import Thread
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "enforce" / "testdata" / "post_failure_start_python_fixture.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, separators=(",", ":")), encoding="utf-8")


def _case(root, name, bridge=False, foreign_index=False, release_failure=False):
    root.mkdir(parents=True, exist_ok=True)
    call = "failed start " + name
    job_id = hashlib.sha256(call.encode()).hexdigest()
    event = {"hook_event_name": "PostToolUseFailure", "tool_name": "mcp__codex-main__codex_start",
             "session_id": "start-failure-session", "tool_use_id": name, "error": "start failed",
             "is_interrupt": False, "duration_ms": 1234}
    key = codex._call_key(event)
    project = "start-failure-project"
    pending = {"tool_use_id": name, "async_call": call, "job_id": job_id, "async_ready": True,
               "task_id": name, "project_key": project, "route": "normal", "model": "gpt-fixture",
               "effort": "medium", "lease_owner": key, "repo_root": str(root), "kind": "call"}
    _write(root / "pending" / f"{key}.json", pending)
    if bridge:
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        port = listener.getsockname()[1]
        _write(root / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
               {"jobId": job_id, "call": call, "state": "RUNNING", "signalPort": port,
                "signalToken": "fixture-token"})

        def cancel():
            connection, _ = listener.accept()
            with connection:
                connection.recv(1024)
                _write(root / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
                       {"jobId": job_id, "call": call, "state": "CANCELLED", "signalPort": port,
                        "signalToken": "fixture-token"})
                connection.sendall(b"1")

        thread = Thread(target=cancel)
        thread.start()
    else:
        listener = thread = None
    if name in {"terminal_job", "live_job_rejected"}:
        state = "FAILED" if name == "terminal_job" else "RUNNING"
        _write(root / "codex_exec_mcp" / "jobs" / f"{job_id}.json",
               {"jobId": job_id, "call": call, "state": state})
    own_index = root / "async_jobs" / f"{job_id}.json"
    _write(own_index, {"jobId": job_id, "project_key": project, "call": call})
    foreign_id = hashlib.sha256((call + " foreign").encode()).hexdigest()
    foreign_path = root / "async_jobs" / f"{foreign_id}.json"
    if foreign_index:
        _write(foreign_path, {"jobId": foreign_id, "project_key": "another-project"})
    ledger.ScopeLeases(root / "projects" / project).acquire(key, name, ["src"])
    with patch.object(codex, "_release_scope_lease", side_effect=codex.WiringError("LEASE_RELEASE_FAILED")) if release_failure else _noop():
        response = codex.post_tool_use_failure(event, state_root=root)
    if thread is not None:
        thread.join(timeout=3)
        listener.close()
        assert not thread.is_alive()
    saved_pending = root / "pending" / f"{key}.json"
    context = response["hookSpecificOutput"]["additionalContext"].replace(
        str(root).replace("\\", "\\\\"), "__ROOT__").replace(str(root), "__ROOT__")
    context = context.replace("\\", "/") if "__ROOT__" in context else context
    result = {"response_context": context, "pending_exists": saved_pending.exists(),
              "pending_start_rejected": (json.loads(saved_pending.read_text()).get("start_rejected", False)
                                          if saved_pending.exists() else False),
              "own_index_exists": own_index.exists(), "foreign_index_exists": foreign_path.exists() if foreign_index else None}
    return result


class _noop:
    def __enter__(self):
        return self

    def __exit__(self, *_):
        return False


def test_failure_start_python_fixture_and_go_parity(tmp_path):
    cases = []
    for name, options in (("no_job", {}), ("terminal_job", {}), ("live_job_rejected", {}),
                          ("cancel_succeeds", {"bridge": True}),
                          ("foreign_project_index", {"foreign_index": True}),
                          ("lease_release_failure", {"release_failure": True})):
        cases.append({"name": name, "expected": _case(tmp_path / name, name, **options)})
    actual = {"oracle": "codex.post_tool_use_failure codex_start branch", "cases": cases}
    if os.getenv("FAILURE_START_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8", newline="\n")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == actual
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-build-cache")
    result = subprocess.run(["go", "test", "./internal/enforce/", "-run", "^TestPostFailureStartPythonFixture$", "-count=1"],
                            cwd=ROOT / "go", env=env, text=True, capture_output=True, timeout=120, check=False)
    assert result.returncode == 0, result.stdout + result.stderr
