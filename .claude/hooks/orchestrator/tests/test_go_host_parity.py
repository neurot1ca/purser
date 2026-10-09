"""Record real Python host outcomes for Go host parity replays."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ROOT / "go" / "internal" / "jobs" / "testdata" / "host_parity.json"
sys.path.insert(0, str(ORCH))
import codex_exec_mcp
import ledger

CALL = "host-parity-call"
JOB_ID = hashlib.sha256(CALL.encode()).hexdigest()
TOKEN = "host-owner-token"
SIGNAL = "host-signal-token"
CASES = ("success", "nonzero", "deadline", "cancel", "stale", "queued", "cancel_queued", "stall",
         "worktree_success", "worktree_create_failure", "worktree_setup_failure",
         "agent_success", "agent_failure")

FAKE_WORKER = r'''import json, os, sys, time
mode = os.environ.get("PURSER_HOST_ORACLE_MODE", "success")
agent = os.environ.get("PURSER_HOST_ORACLE_AGENT", "codex")
if mode in ("sleep", "deadline", "cancel", "cancel_queued", "stall"):
    time.sleep(5)
if agent == "opencode":
    print(json.dumps({"type":"step_start","sessionID":"fake-session"}), flush=True)
    if mode == "agent_failure":
        print(json.dumps({"type":"error","message":"fake agent failure"}), flush=True)
        sys.exit(1)
    print(json.dumps({"type":"text","sessionID":"fake-session","part":{"text":"fake answer"}}), flush=True)
    print(json.dumps({"type":"step_finish","sessionID":"fake-session","part":{"reason":"stop"}}), flush=True)
    sys.exit(0)
print(json.dumps({"type":"thread.started","thread_id":"fake-thread"}), flush=True)
print(json.dumps({"type":"item.completed","item":{"type":"agent_message","text":"fake answer"}}), flush=True)
print(json.dumps({"type":"turn.completed"}), flush=True)
sys.exit(7 if mode == "nonzero" else 0)
'''


def _write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value), encoding="utf-8")


def _queued_state(root, *, queued):
    repo = root / "repo"
    (repo / "src").mkdir(parents=True)
    (repo / "src" / "file").write_text("baseline\n", encoding="utf-8", newline="\n")
    env = os.environ.copy()
    env.update({"GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z",
                "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"})
    for args in (("init", "-q"), ("add", "src/file"), ("commit", "-qm", "baseline")):
        subprocess.run(["git", *args], cwd=repo, env=env, check=True, capture_output=True)
    project = root / "projects" / "project"
    leases = ledger.ScopeLeases(project)
    if queued:
        assert leases.acquire("blocker", "blocker", ["src"], job_id="blocker-job")["status"] == ledger.OK
    result = leases.acquire("owner-key", "task", ["src"], job_id=JOB_ID, queue_conflict=queued)
    assert result["status"] == ledger.OK
    pending = {"job_id": JOB_ID, "async_call": CALL, "lease_queued": True,
               "lease_owner": "owner-key", "project_key": "project", "repo_root": str(repo),
               "task_id": "task", "scope": ["src"]}
    _write(root / "pending" / "owner-key.json", pending)
    _write(root / "async_jobs" / f"{JOB_ID}.json",
           {"jobId": JOB_ID, "call": CALL, "call_key": "owner-key", "project_key": "project"})
    baseline = {"status": "UNKNOWN", "code": "QUEUED"}
    saved = ledger.SessionLedger(project).save(
        ledger.build_session_record("task", None, "", "", "", pending_call_baseline=baseline))
    assert saved["status"] == ledger.OK


def _normalize(value, root):
    if isinstance(value, dict):
        result = {}
        state = value.get("state")
        for key, item in value.items():
            if key in ("hostPid", "pid"):
                result[key] = "__PID__"
            elif key in ("ownershipToken", "signalToken"):
                result[key] = "__TOKEN__"
            elif key in ("hostCreatedAt", "signalPort"):
                result[key] = "__HOST_IDENTITY__"
            elif key == "hostContained":
                result[key] = "__HOST_CONTAINED__"
            elif key.endswith("At") or key.endswith("_at"):
                result[key] = "__TIME__"
            elif key == "longestSilentGapSeconds":
                result[key] = "__DURATION__"
            elif key == "stdoutBytes" and isinstance(item, (int, float)) and item > 0:
                result[key] = "__STDOUT_BYTES__"
            elif key == "exitCode" and state in ("TIMED_OUT", "CANCELLED"):
                result[key] = "__TERMINATION_EXIT_CODE__"
            elif key == "repoRoot" and isinstance(item, str) and Path(item).resolve() == ROOT:
                result[key] = "__ROOT__"
            else:
                result[key] = _normalize(item, root)
        return result
    if isinstance(value, list):
        return [_normalize(item, root) for item in value]
    if isinstance(value, str):
        escaped_root = str(root).replace("\\", "\\\\")
        value = value.replace(escaped_root, "__ROOT__").replace(str(root), "__ROOT__")
        if "__ROOT__" in value:
            return value.replace("\\\\", "/").replace("\\", "/")
        return value
    return value


def _signal(job, operation):
    with socket.create_connection(("127.0.0.1", job["signalPort"]), timeout=2) as connection:
        connection.sendall(f'{JOB_ID}:{SIGNAL}:{operation}'.encode("ascii"))
        connection.settimeout(5)
        return connection.recv(1) == b"1"


def _wait(path, predicate, seconds=4):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        try:
            job = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            job = {}
        if predicate(job):
            return job
        time.sleep(0.01)
    raise AssertionError(f"host did not reach expected state; last record={job!r}")


def _case(tmp_path, name):
    root = tmp_path / name
    jobs = root / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    if name in ("queued", "cancel_queued"):
        _queued_state(root, queued=True)
    now = time.time()
    job = {"jobId": JOB_ID, "call": CALL, "acceptedAt": now, "state": "QUEUED", "generation": 1,
           "ownershipToken": TOKEN, "signalToken": SIGNAL, "deadlineAt": now + (0.25 if name == "deadline" else 30),
           "leaseQueued": name in ("queued", "cancel_queued")}
    if name == "stale":
        job["generation"] = 2
    if name == "stall":
        job["liveness"] = {"lastActivityAt": now - 2000, "stdoutBytes": 0,
                           "longestSilentGapSeconds": 0.0}
    job_path = jobs / f"{JOB_ID}.json"
    _write(job_path, job)
    fake_path = root / "fake_worker.py"
    fake_path.write_text(FAKE_WORKER, encoding="utf-8")
    agent = "opencode" if name.startswith("agent_") else "codex"
    worktree = name.startswith("worktree_")
    if name in ("worktree_success", "worktree_setup_failure"):
        repo = root / "repo"
        (repo / "src").mkdir(parents=True)
        (repo / "src" / "file").write_text("baseline\n", encoding="utf-8", newline="\n")
        if name == "worktree_setup_failure":
            (repo / ".orchestrator").mkdir()
            (repo / ".orchestrator" / "profile.toml").write_text(
                'schema_version = 1\n[worktree]\nsetup = "exit 9"\n', encoding="utf-8")
            user_config = root / "home" / ".orchestrator" / "config.toml"
            user_config.parent.mkdir(parents=True)
            user_config.write_text(
                f'schema_version = 1\n[trust]\npaths = [{json.dumps(str(repo))}]\n', encoding="utf-8")
        env = os.environ.copy()
        env.update({"GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z",
                    "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
                    "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"})
        for args in (("init", "-q"), ("add", "."), ("commit", "-qm", "baseline")):
            subprocess.run(["git", *args], cwd=repo, env=env, check=True, capture_output=True)
    cwd = (tempfile.gettempdir() if name == "worktree_create_failure" else
           str(root / "repo") if worktree else str(root))
    launch_args = {"cwd": cwd, "prompt": "hello"}
    if worktree:
        launch_args["worktree"] = True
    if agent == "opencode":
        launch_args["model"] = "provider/model"
    _write(jobs / f"{JOB_ID}.launch", {
        "name": "codex", "agent": agent, "codexBin": json.dumps([sys.executable, str(fake_path)]),
        "args": launch_args,
    })
    prior_env = {key: os.environ.get(key) for key in ("PURSER_HOST_ORACLE_MODE", "PURSER_HOST_ORACLE_AGENT",
                                                        "PURSER_STATE_DIR", "HOME", "USERPROFILE", "HOMEDRIVE",
                                                        "HOMEPATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR",
                                                        "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA")}
    os.environ["PURSER_HOST_ORACLE_MODE"] = name
    os.environ["PURSER_HOST_ORACLE_AGENT"] = agent
    os.environ["PURSER_STATE_DIR"] = str(root)
    isolated_home = root / "home"
    os.environ["HOME"] = str(isolated_home)
    os.environ["USERPROFILE"] = str(isolated_home)
    os.environ["HOMEDRIVE"] = ""
    os.environ["HOMEPATH"] = ""
    os.environ["CODEX_HOME"] = str(root / "codex")
    os.environ["CLAUDE_CONFIG_DIR"] = str(root / "claude-config")
    os.environ["XDG_CONFIG_HOME"] = str(root / "xdg-config")
    os.environ["APPDATA"] = str(root / "appdata")
    os.environ["LOCALAPPDATA"] = str(root / "localappdata")
    if name == "worktree_setup_failure":
        os.environ["HOME"] = str(root / "home")
        os.environ["USERPROFILE"] = str(root / "home")
    outcome = {}
    worker = threading.Thread(target=lambda: codex_exec_mcp._host_main(JOB_ID, jobs, 1, TOKEN), daemon=True)
    worker.start()
    try:
        if name == "stale":
            worker.join(2)
        elif name == "queued":
            _wait(job_path, lambda current: current.get("state") == "QUEUED" and "signalPort" in current)
            ledger.ScopeLeases(root / "projects" / "project").release("blocker")
            worker.join(5)
        elif name == "cancel_queued":
            current = _wait(job_path, lambda item: item.get("state") == "QUEUED" and "signalPort" in item)
            assert _signal(current, "CANCEL")
            worker.join(5)
        elif name in ("cancel", "stall"):
            current = _wait(job_path, lambda item: item.get("state") == "RUNNING" and "signalPort" in item)
            if name == "stall":
                outcome["snapshot"] = _normalize(_wait(job_path, lambda item: item.get("errorCode") == "STALLED"), root)
            assert _signal(current, "CANCEL")
            worker.join(5)
        else:
            worker.join(6)
        assert not worker.is_alive(), f"Python host did not finish {name}"
    finally:
        for key, value in prior_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
    final = json.loads(job_path.read_text(encoding="utf-8"))
    return {"name": name, "snapshot": outcome.get("snapshot"), "record": _normalize(final, root)}


def test_go_host_fixture_matches_real_python_host(tmp_path):
    actual = [_case(tmp_path, name) for name in CASES]
    if os.environ.get("PURSER_REGEN_FIXTURES") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, indent=2) + "\n", encoding="utf-8", newline="\n")
    assert FIXTURE.is_file(), f"missing {FIXTURE}; run with PURSER_REGEN_FIXTURES=1"
    assert FIXTURE.stat().st_size < 30_000
    expected = _normalize(json.loads(FIXTURE.read_text(encoding="utf-8")), tmp_path / "fixture")
    assert expected == actual


@pytest.mark.parametrize("name", ("worktree_create_failure", "worktree_setup_failure"))
def test_worktree_failures_do_not_launch_worker(tmp_path, name):
    record = _case(tmp_path, name)["record"]
    assert record["state"] == "FAILED"
    assert record["errorCode"] in ("WORKTREE_SNAPSHOT_FAILED", "WORKTREE_SETUP_FAILED")
    assert "startedAt" not in record and "pid" not in record
