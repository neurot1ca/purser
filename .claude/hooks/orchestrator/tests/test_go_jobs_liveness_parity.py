"""Python oracle for Go job liveness checkpoints and restart persistence."""
import json
import os
from pathlib import Path
import shutil
import sys
import uuid

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_liveness" / "liveness.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge


def _initial(job_id, *, generation=1, token="owner", activity=None):
    return {
        "job": {"jobId": job_id, "state": "RUNNING", "generation": generation,
                "ownershipToken": token, "route": "implementation", "etaMinutes": 1,
                "liveness": {}},
        "activity": activity or {
            "route": "implementation", "agent": "codex", "lastActivityAt": 100.0,
            "stdoutBytes": 0, "processAlive": True, "processAliveAt": 100.0,
            "longestSilentGapSeconds": 0.0, "stallThresholdSeconds": 900,
            "checkpointAt": 100.0,
        },
    }


def _cases():
    throttle = _initial("a" * 64)
    silent = _initial("b" * 64, activity={
        "route": "implementation", "agent": "codex", "lastActivityAt": 100.0,
        "stdoutBytes": 0, "processAlive": True, "processAliveAt": 100.0,
        "longestSilentGapSeconds": 0.0, "stallThresholdSeconds": 200,
        "checkpointAt": 100.0,
    })
    restart = _initial("c" * 64, activity={
        "route": "implementation", "agent": "codex", "lastActivityAt": 0.0,
        "stdoutBytes": 0, "processAlive": True, "processAliveAt": 0.0,
        "longestSilentGapSeconds": 0.0, "stallThresholdSeconds": 900,
        "checkpointAt": 0.0,
    })
    stale = _initial("d" * 64, generation=2, token="new-owner")
    wrong_token = _initial("e" * 64, token="right-owner")
    return [
        {"name": "throttled_checkpoints", **throttle, "generation": 1, "token": "owner",
         "steps": [{"kind": "activity", "at": 103.0, "bytes": 2},
                   {"kind": "checkpoint", "at": 103.0},
                   {"kind": "activity", "at": 106.0, "bytes": 3},
                   {"kind": "checkpoint", "at": 106.0},
                   {"kind": "activity", "at": 161.0, "bytes": 4},
                   {"kind": "checkpoint", "at": 161.0}]},
        {"name": "silent_gap_and_stall", **silent, "generation": 1, "token": "owner",
         "steps": [{"kind": "activity", "at": 301.0, "bytes": 1},
                   {"kind": "checkpoint", "at": 301.0},
                   {"kind": "activity", "at": 302.0, "bytes": 1},
                   {"kind": "checkpoint", "at": 302.0}]},
        {"name": "restart_then_continue", **restart, "generation": 1, "token": "owner",
         "steps": [{"kind": "checkpoint", "at": 901.0},
                   {"kind": "restart", "at": 902.0},
                   {"kind": "checkpoint", "at": 902.0},
                   {"kind": "activity", "at": 903.0, "bytes": 1},
                   {"kind": "checkpoint", "at": 903.0}]},
        {"name": "stale_generation_rejected", **stale, "generation": 1, "token": "old-owner",
         "steps": [{"kind": "checkpoint", "at": 201.0, "force": True}]},
        {"name": "wrong_token_rejected", **wrong_token, "generation": 1, "token": "wrong-owner",
         "steps": [{"kind": "checkpoint", "at": 201.0, "force": True}]},
    ]


def _replay(case, job_dir):
    server = bridge.Server(job_dir=job_dir)
    job_id = case["job"]["jobId"]
    server._write_job(case["job"])
    activity = dict(case["activity"])
    outputs = []
    for step in case["steps"]:
        if step["kind"] == "activity":
            activity["stdoutBytes"] += step["bytes"]
            bridge._record_activity(activity, step["at"])
            written = None
        elif step["kind"] == "restart":
            saved = server._read_job(job_id)
            previous = saved.get("liveness", {})
            if not isinstance(previous, dict):
                previous = {}
            activity = {
                "route": saved.get("route"), "agent": "codex",
                "lastActivityAt": previous.get("lastActivityAt", step["at"]),
                "stdoutBytes": previous.get("stdoutBytes", 0),
                "processAlive": True, "processAliveAt": step["at"],
                "longestSilentGapSeconds": previous.get("longestSilentGapSeconds", 0.0),
                "stallThresholdSeconds": 900,
                "checkpointAt": step["at"],
                **({"stallDetectedAt": saved["stallDetectedAt"]}
                   if isinstance(saved.get("stallDetectedAt"), (int, float)) else {}),
            }
            written = None
        else:
            written = server._checkpoint_liveness(
                job_id, case["generation"], case["token"], activity,
                now=step["at"], process_alive=True, force=step.get("force", False))
        outputs.append({"written": written, "job": server._read_job(job_id)})
    return outputs


def _expected(case):
    directory = ROOT / ".tmp" / ("go-jobs-liveness-" + uuid.uuid4().hex)
    directory.mkdir(parents=True)
    try:
        return _replay(case, directory)
    finally:
        shutil.rmtree(directory)


def _threshold_cases():
    return [
        {"name": "eta_bounded", "job": {"route": "implementation", "activeAgent": "codex", "etaMinutes": 8},
         "history": []},
        {"name": "agy_cold_start", "job": {"route": "implementation", "activeAgent": "agy", "etaMinutes": 2},
         "history": []},
        {"name": "learned_p90", "job": {"route": "implementation", "activeAgent": "codex", "etaMinutes": 8},
         "history": [1000, 1100, 1200, 1300, 1400]},
    ]


def _expected_threshold(case):
    directory = ROOT / ".tmp" / ("go-jobs-liveness-threshold-" + uuid.uuid4().hex)
    directory.mkdir(parents=True)
    try:
        server = bridge.Server(job_dir=directory)
        for index, gap in enumerate(case["history"]):
            server._write_job({
                "jobId": f"{index:064x}", "state": "SUCCEEDED",
                "route": "implementation", "activeAgent": "codex",
                "liveness": {"agent": "codex", "longestSilentGapSeconds": gap},
            })
        return server._stall_threshold(case["job"])
    finally:
        shutil.rmtree(directory)


def _write_fixture():
    FIXTURE.parent.mkdir(parents=True, exist_ok=True)
    cases = []
    for case in _cases():
        expected = _expected(case)
        cases.append({**case, "expected": expected})
    thresholds = [{**case, "expected": _expected_threshold(case)} for case in _threshold_cases()]
    FIXTURE.write_text(json.dumps({"cases": cases, "thresholds": thresholds},
                                  ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


if os.environ.get("PURSER_REGEN_GO_JOBS_LIVENESS") == "1":
    _write_fixture()


def test_go_jobs_liveness_fixture_matches_python_oracle():
    assert FIXTURE.is_file(), f"missing {FIXTURE}; run with PURSER_REGEN_GO_JOBS_LIVENESS=1"
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert len(fixture["cases"]) == len(_cases())
    for actual, case in zip(fixture["cases"], _cases()):
        assert {key: actual[key] for key in case} == case
        assert actual["expected"] == _expected(case)
    for actual, case in zip(fixture["thresholds"], _threshold_cases()):
        assert {key: actual[key] for key in case} == case
        assert actual["expected"] == _expected_threshold(case)
