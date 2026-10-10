import json
import os
from datetime import datetime, timezone
from pathlib import Path

import codex
import compaction_advisor

PACKAGE = Path(__file__).resolve().parents[1]
FIXTURE = PACKAGE / "tests" / "fixtures" / "go_policy" / "post_board.json"
NOW = 1_800_000_000


def _job(job_id, **values):
    return {"jobId": job_id, "call": "call-" + job_id, "label": "Task " + job_id[:4],
            "state": "SUCCEEDED", "finishedAt": NOW - 10, **values}


def _case(name, jobs=(), extras=None):
    files = {}
    for index, job in enumerate(jobs):
        job_id = job["jobId"]
        files[f"async_jobs/{index}.json"] = {"project_key": "fixture", "jobId": job_id,
                                              "call": job["call"], "call_key": "key-" + job_id}
        files[f"codex_exec_mcp/jobs/{job_id}.json"] = job
    files.update(extras or {})
    return {"name": name, "files": {key: json.dumps(value) if not isinstance(value, str) else value
                                     for key, value in files.items()}}


def test_post_board_python_fixture(tmp_path, monkeypatch):
    monkeypatch.setattr(codex.time, "time", lambda: NOW)
    cases = []
    for name, count in (("missing_count", "missing"), ("null_count", None), ("zero_count", 0), ("two_count", 2)):
        job = _job("a" * 64)
        if count != "missing":
            job["boardDisplayCount"] = count
        cases.append(_case(name, [job]))
    applied = _job("b" * 64, applyState="applied", appliedAt=NOW - 10, finishedAt=NOW - 7200,
                   boardDisplayCount=0)
    cases.append(_case("applied_at_wins", [applied]))
    for name, job, bundle in (
        ("discarded", _job("7" * 64, discarded=True, boardDisplayCount=0),
         {"phase": "implementation", "apply": "codex_apply", "changed_paths": ["src/a"],
          "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"}),
        ("in_review", _job("f" * 64, boardDisplayCount=0),
         {"phase": "implementation", "apply": "codex_apply", "changed_paths": ["src/a"]}),
        ("accepted", _job("g" * 64, acceptance={}, boardDisplayCount=0),
         {"phase": "implementation", "apply": "codex_apply", "changed_paths": ["src/a"]}),
        ("done_not_applicable", _job("h" * 64, phase="operation", boardDisplayCount=0),
         {"phase": "operation", "apply": "codex_apply"}),
        ("done_no_change", _job("i" * 64, boardDisplayCount=0),
         {"phase": "implementation", "apply": "codex_apply", "changed_paths": []}),
    ):
        call_key = "key-" + job["jobId"]
        cases.append(_case(name, [job], {f"bundle_artifacts/{call_key}.json": bundle}))
    cases.append(_case("older_than_hour", [_job("c" * 64, finishedAt=NOW - 3601, boardDisplayCount=0)]))
    cases.append(_case("newest_thread_job", [
        _job("d" * 64, threadId="same", acceptedAt=NOW - 20, state="RUNNING"),
        _job("e" * 64, threadId="same", acceptedAt=NOW - 10, boardDisplayCount=0),
    ]))
    active, queued = _job("1" * 64, label="Holder", state="RUNNING"), _job("2" * 64, state="QUEUED")
    leases = {"version": 1, "owners": {
        "active": {"state": "ACTIVE", "job_id": active["jobId"], "scope": ["src"]},
        "queued": {"state": "QUEUED", "job_id": queued["jobId"], "scope": ["src/file.go"]},
    }}
    cases.append(_case("queued_behind_overlap", [active, queued], {
        "projects/fixture/scope_leases.json": json.dumps(leases)}))
    cases.append(_case("stalled_missing_effort", [_job("3" * 64, state="RUNNING", errorCode="STALLED",
                                                       startedAt=NOW - 600, model="gpt-test")]))
    cases.append(_case("running_coding", [_job("9" * 64, state="RUNNING", startedAt=NOW - 300,
                                                 etaMinutes=2, boardDisplayCount=0)]))
    cases.append(_case("running_fetching_queued_order", [
        _job("a" * 64, state="RUNNING", acceptedAt=NOW - 30),
        _job("b" * 64, state="SUCCEEDED", acceptedAt=NOW - 20, boardDisplayCount=0),
        _job("c" * 64, state="QUEUED", acceptedAt=NOW - 10),
    ]))
    cases.append(_case("nothing_visible", [_job("4" * 64, boardDisplayCount=1)]))
    cases.append(_case("malformed_indexes_and_jobs", [], {
        "async_jobs/bad.json": "{broken",
        "async_jobs/valid.json": json.dumps({"project_key": "fixture", "jobId": "5" * 64, "call": "x"}),
        "codex_exec_mcp/jobs/" + "5" * 64 + ".json": json.dumps({"jobId": "5" * 64, "call": "wrong"}),
        "async_jobs/malformed_job.json": json.dumps({"project_key": "fixture", "jobId": "6" * 64, "call": "y"}),
        "codex_exec_mcp/jobs/" + "6" * 64 + ".json": "{broken",
    }))
    for case in cases:
        state = tmp_path / case["name"]
        state.mkdir()
        for relative, content in case["files"].items():
            path = state / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
        board, ids = codex._project_job_board_details(state, "fixture")
        case["board"], case["ids"] = board, sorted(ids)
        if case["name"] == "discarded":
            assert "| discarded | — | — |" in board

    clock = datetime(2027, 1, 15, tzinfo=timezone.utc)
    base_receipt = {"model": "gpt-test", "effort": "high", "5h_used_pct": 31,
                    "5h_resets_at": "2027-01-15T02:00:00+00:00", "7d_used_pct": 44,
                    "7d_resets_at": "2027-01-17T00:00:00+00:00", "snapshot_at": "2027-01-14T23:30:00+00:00"}
    advice = [{"receipt": base_receipt, "now": clock.isoformat(),
               "expected": compaction_advisor._receipt_advice(base_receipt, clock)},
              {"receipt": {**base_receipt, "snapshot_at": "2027-01-14T21:00:00+00:00"},
               "now": clock.isoformat(), "expected": compaction_advisor._receipt_advice(
                   {**base_receipt, "snapshot_at": "2027-01-14T21:00:00+00:00"}, clock)}]

    dispatch = {}
    original_eta, original_queue, original_guidance = codex._route_eta_minutes, compaction_advisor.queue_dispatch, codex._append_pending_guidance
    codex._route_eta_minutes = lambda *_args, **_kwargs: 5
    codex._append_pending_guidance = lambda response, *_args, **_kwargs: response
    try:
        for name, prior in (("new_session", None), ("reply", "thread-1")):
            lines = []
            compaction_advisor.queue_dispatch = lambda _event, _repo, line=None, **_kwargs: lines.append(line)
            pending = {"label": "Build", "model": "gpt-test", "effort": "high", "route": "normal",
                       "repo_root": str(tmp_path)}
            if prior:
                pending["prior_thread_id"] = prior
            codex._append_dispatch_receipt({}, pending, tmp_path, {"tool_name": "codex"})
            dispatch[name] = lines[0]
    finally:
        codex._route_eta_minutes, compaction_advisor.queue_dispatch = original_eta, original_queue
        codex._append_pending_guidance = original_guidance
    receipt_lines = []
    for name, prior in (("new_session", None), ("reply", "thread-1")):
        pending = {"label": "Build", "model": "gpt-test", "effort": "high", "route": "normal"}
        if prior:
            pending["prior_thread_id"] = prior
        receipt_lines.append({"pending": pending, "eta": 5, "expected": dispatch[name]})
    fixture = {"cases": cases, "advice": advice, "dispatch": receipt_lines,
               "guidance": codex._append_worktree_apply_context({}, "0123")["systemMessage"]}
    if os.environ.get("POST_BOARD_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8", newline="\n")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
