"""Pass 158 replacement oracle cases from the real Python Server."""
import hashlib
import json
import os
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import nullcontext
from pathlib import Path
import pytest

ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_recover_replace" / "oracle.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge
from mixed_owner_support import go_test_tools

JOB = "0123456789abcdef" * 4
NOW = 2_000_000_000.0
TOKEN = "a1" * 16
PROMPT = bridge.CONTINUATION_PROMPT


def _base(name, state="RUNNING", **extra):
    job = {"jobId": JOB, "call": "call-1", "state": state, "acceptedAt": NOW - 10,
           "deadlineAt": NOW + 100, "generation": 2, "ownershipToken": "old-token",
           "hostPid": 123, "hostCreatedAt": 456, "hostContained": True, "pid": 789,
           "resumedAfterInterruption": 0, **extra}
    launch = {"name": "codex", "args": {"prompt": "resume", "timeoutSeconds": 30},
              "codexBin": None, "agent": "codex"}
    return {"name": name, "job": job, "launch": launch, "probes": {"host": "DEAD", "groupStopped": True},
            "index": None, "pending": None, "lease": None, "launchError": ""}


def _lease_data():
    return {"version": 1, "owners": {"owner-1": {"state": "ACTIVE", "scope": ["src"],
            "task_id": "task-1", "job_id": JOB, "acquired_at": NOW - 20, "retention_after": NOW + 10}},
            "conflicts": []}


def test_go_jobs_recover_replace_matches_python(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge.time, "time", lambda: NOW)
    monkeypatch.setattr(bridge, "_ownership_lock", lambda *_args: nullcontext())
    class UUID:
        hex = TOKEN
    monkeypatch.setattr(bridge.uuid, "uuid4", lambda: UUID())
    cases = [_base("unclaimed_queued", "QUEUED", leaseQueued=True, hostPid=None),
             _base("unclaimed_launch_failure", "QUEUED", leaseQueued=True, hostPid=None),
             _base("dead_queued", "QUEUED", leaseQueued=True),
             _base("dead_queued_launch_failure", "QUEUED", leaseQueued=True),
             _base("running_new_session", stallDetectedAt=NOW - 1),
             _base("running_thread", threadId="thread-1", threadConfig={"cwd": ".", "config_args": ["x=1"]}),
             _base("thread_config_missing", threadId="thread-1"),
             _base("thread_missing", launch={"name": "codex-reply", "args": {"prompt": "reply"}}),
             _base("launch_missing"), _base("timeout_invalid"), _base("ownership_missing"),
             _base("replacement_launch_failure")]
    cases[1]["launchError"] = "launch failed"
    cases[3]["launchError"] = "launch failed"
    cases[6]["expectedCode"] = "RECOVERY_THREAD_CONFIG_MISSING"
    cases[7]["expectedCode"] = "RECOVERY_THREAD_MISSING"
    cases[8]["launch"] = None
    cases[9]["launch"]["args"]["timeoutSeconds"] = 0
    for c in (cases[4], cases[10]):
        c["index"] = {"jobId": JOB, "call": "call-1", "call_key": "owner-1", "project_key": "project-1"}
        c["lease"] = _lease_data()
    cases[4]["pending"] = {"job_id": JOB, "async_call": "call-1", "lease_owner": "owner-1"}
    cases[11]["launchError"] = "launch failed"
    for c in cases:
        if c["name"] == "replacement_launch_failure":
            c["expectedCode"] = "RECOVERY_HOST_LAUNCH_FAILED"

    output = []
    for case in cases:
        root = tmp_path / case["name"]
        job_dir = root / "codex_exec_mcp" / "jobs"
        state_dir = root / "codex_exec_mcp" / "threads"
        monkeypatch.setenv("PURSER_CODEX_SHIM_STATE", str(state_dir))
        server = bridge.Server(agent="codex", job_dir=job_dir)
        server._write_job(case["job"])
        if case["launch"] is not None:
            (job_dir / f"{JOB}.launch").write_text(json.dumps(case["launch"]), encoding="utf-8")
        if case["index"] is not None:
            path = root / "async_jobs" / f"{JOB}.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(case["index"]), encoding="utf-8")
        if case["pending"] is not None:
            path = root / "pending" / "owner-1.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(case["pending"]), encoding="utf-8")
        if case["lease"] is not None:
            path = root / "projects" / "project-1" / "scope_leases.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(case["lease"]), encoding="utf-8")
        monkeypatch.setattr(server, "_classify_host", lambda _job: "DEAD")
        monkeypatch.setattr(bridge, "_posix_process_group_stopped", lambda _pid: True)
        launches = []
        def launch(*args):
            launches.append({"generation": args[2], "token": args[3]})
            if case["launchError"]:
                raise OSError(case["launchError"])
        monkeypatch.setattr(bridge, "_launch_host", launch)
        result = server.recover_job(JOB, now=NOW)
        record = server._read_job(JOB)
        if case["name"] in {"launch_missing", "ownership_missing"} and record is not None:
            prefix = "Recovery launch data unavailable: " if case["name"] == "launch_missing" else "Async ownership unavailable: "
            record["message"] = prefix + "<OS_ERROR>"
        launch_record = json.loads((job_dir / f"{JOB}.launch").read_text(encoding="utf-8")) if (job_dir / f"{JOB}.launch").exists() else None
        thread_path = state_dir / (hashlib.sha256(b"thread-1").hexdigest() + ".json")
        thread_record = json.loads(thread_path.read_text(encoding="utf-8")) if thread_path.exists() else None
        lease_record = json.loads((root / "projects" / "project-1" / "scope_leases.json").read_text(encoding="utf-8")) if case["lease"] is not None else None
        output.append({"name": case["name"], "job": case["job"], "launch": case["launch"], "probes": case["probes"],
                       "index": case["index"], "pending": case["pending"], "lease": case["lease"],
                       "launchError": case["launchError"], "expectedCode": case.get("expectedCode"),
                       "result": result, "record": record, "launchRecord": launch_record,
                       "threadRecord": thread_record, "leaseRecord": lease_record, "launches": launches})
    fixture = {"oracle": "Python Server.recover_job replacement parity", "cases": output}
    if os.environ.get("PURSER_REGEN_GO_JOBS_RECOVER_REPLACE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, separators=(",", ":")) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture


def test_python_and_go_recovery_contend_for_python_record_once(go_test_tools, tmp_path, monkeypatch):
    """Python and Go contend on a Python-created record; one replacement owns generation two."""
    jobs = tmp_path / "codex_exec_mcp" / "jobs"
    jobs.mkdir(parents=True)
    go_hook_binary, fake = go_test_tools["hook"], go_test_tools["fake"]
    marker = tmp_path / "worker-launches.txt"
    now = time.time()
    server = bridge.Server(agent="codex", job_dir=jobs)
    child_pid = None
    if os.name != "nt":
        child = subprocess.Popen([sys.executable, "-c", "pass"], start_new_session=True)
        child.wait(timeout=5)
        child_pid = child.pid
    job = {"jobId": JOB, "call": "python-created", "state": "RUNNING",
                       "acceptedAt": now - 1, "deadlineAt": now + 30, "generation": 1,
                       "ownershipToken": "python-owner-token", "signalToken": "python-signal-token",
                       "hostPid": 2**30, "hostCreatedAt": 0, "hostContained": True,
                       "resumedAfterInterruption": 0}
    if child_pid is not None:
        job["pid"] = child_pid
    server._write_job(job)
    launch = {"name": "codex", "args": {"cwd": str(tmp_path), "prompt": "recover",
                                           "timeoutSeconds": 30},
              "codexBin": str(fake), "agent": "codex"}
    (jobs / f"{JOB}.launch").write_text(json.dumps(launch), encoding="utf-8")
    env = os.environ.copy()
    env.update(PURSER_STATE_DIR=str(tmp_path), PURSER_CODEX_BIN=str(fake),
               MIXED_LAUNCH_MARKER=str(marker), PURSER_CODEX_TEST_PARENT_PID=str(os.getpid()))
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    monkeypatch.setenv("PURSER_CODEX_TEST_PARENT_PID", str(os.getpid()))
    monkeypatch.setenv("MIXED_LAUNCH_MARKER", str(marker))
    barrier = threading.Barrier(2)

    def go_result():
        barrier.wait()
        return subprocess.run([str(go_hook_binary), "op", "codex_result"],
                              input=json.dumps({"args": {"jobId": JOB}}), text=True,
                              capture_output=True, env=env, timeout=15, check=True)

    def python_cleanup():
        import async_session_maintenance as maintenance
        barrier.wait()
        maintenance.cleanup_jobs(tmp_path, now=time.time())

    with ThreadPoolExecutor(max_workers=2) as pool:
        go_future = pool.submit(go_result)
        python_future = pool.submit(python_cleanup)
        result = go_future.result(timeout=20)
        python_future.result(timeout=20)
    assert json.loads(result.stdout)["status"] == "ok"

    record = jobs / f"{JOB}.json"
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        job = json.loads(record.read_text(encoding="utf-8"))
        if job.get("state") == "SUCCEEDED":
            break
        time.sleep(0.02)
    assert job["state"] == "SUCCEEDED", job
    assert job["generation"] == 2
    assert job["ownershipToken"] != "python-owner-token"
    assert marker.read_text(encoding="utf-8").splitlines() == ["launch"]
