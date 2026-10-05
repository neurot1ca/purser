"""M4 pass 157 recovery classification fixtures from the real Python Server."""
import json
import os
import sys
from contextlib import nullcontext
from pathlib import Path

ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_recover" / "oracle.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge

JOB = "0123456789abcdef" * 4
NOW = 2_000_000_000.0


def _case(name, state="RUNNING", **extra):
    return {"name": name, "job": {"jobId": JOB, "call": "call-1", "state": state,
            "acceptedAt": NOW - 10, "deadlineAt": NOW + 100, "generation": 1,
            "hostPid": 123, "hostCreatedAt": 456, "hostContained": True,
            "pid": 789, **extra}}


def test_go_jobs_recover_matches_python(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge.time, "time", lambda: NOW)
    monkeypatch.setattr(bridge, "_ownership_lock", lambda *_args: nullcontext())
    cases = [
        _case("terminal", "FAILED", finishedAt=NOW),
        _case("consumed", "RUNNING"),
        _case("unclaimed_expired", "QUEUED", leaseQueued=True, hostPid=None, deadlineAt=NOW - 1),
        _case("unclaimed_queued", "QUEUED", leaseQueued=True, hostPid=None),
        _case("missing_host", "RUNNING", hostPid=None),
        _case("live_host", "RUNNING"),
        _case("dead_child_live", "RUNNING"),
        _case("missing_generation", "RUNNING", generation=None),
        _case("invalid_attempts", "RUNNING", resumedAfterInterruption=-1),
        _case("exhausted_attempts", "RUNNING", resumedAfterInterruption=2),
        _case("missing_reply_thread", "RUNNING"),
        _case("dead_resumable", "RUNNING", threadId="thread-1", threadConfig={"cwd": ".", "config_args": []}),
    ]
    for case in cases:
        case["index"] = {"stage": "CONSUMED"} if case["name"] == "consumed" else None
        case["launch"] = {"name": "codex-reply" if case["name"] == "missing_reply_thread" else "codex",
                          "args": {"prompt": "resume", "timeoutSeconds": 30}}
        case["probes"] = {"host": "ALIVE" if case["name"] == "live_host" else "DEAD",
                           "groupStopped": case["name"] != "dead_child_live"}
        if case["name"] == "missing_host":
            case["job"]["hostPid"] = None

    output = []
    for case in cases:
        root = tmp_path / case["name"]
        job_dir = root / "codex_exec_mcp" / "jobs"
        server = bridge.Server(agent="codex", job_dir=job_dir)
        server._write_job(case["job"])
        if case["index"] is not None:
            path = root / "async_jobs" / f"{JOB}.json"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(case["index"]), encoding="utf-8")
        (job_dir / f"{JOB}.launch").write_text(json.dumps(case["launch"]), encoding="utf-8")
        monkeypatch.setattr(server, "_classify_host", lambda _job, state=case["probes"]["host"]: state)
        monkeypatch.setattr(bridge, "_posix_process_group_stopped",
                            lambda _pid, result=case["probes"]["groupStopped"]: result)
        monkeypatch.setattr(bridge, "_launch_host", lambda *_args: None)
        # Exercise the child-tree proof branch on Windows without a real process.
        with monkeypatch.context() as patch:
            patch.setattr(bridge.os, "name", "posix")
            result = server.recover_job(JOB, now=NOW)
        record = server._read_job(JOB)
        output.append({"name": case["name"], "job": case["job"], "index": case["index"],
                       "launch": case["launch"], "probes": case["probes"], "result": result,
                       # Pass 158 owns replacement record mutation and launch.
                       "record": None if result == "RECOVERED" else record})

    fixture = {"oracle": "real Server.recover_job classification before pass 158 replacement", "cases": output}
    if os.environ.get("PURSER_REGEN_GO_JOBS_RECOVER") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
