import hashlib
import json
import os
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "async_owned.json"
sys.path.insert(0, str(ORCH))
import codex
import project_root


NAMES = ("owned", "adoptable", "foreign_project", "missing_index", "index_without_pending",
         "index_job_id_mismatch", "missing_job_file", "consumed", "finalizing")


def _replace(value, repo, project_key):
    if isinstance(value, str):
        return value.replace(str(repo), "__REPO__").replace(project_key, "__PROJECT_KEY__")
    if isinstance(value, list):
        return [_replace(item, repo, project_key) for item in value]
    if isinstance(value, dict):
        return {key: _replace(item, repo, project_key) for key, item in value.items()}
    return value


def _capture(name, root):
    repo, state = root / "repo", root / "state"
    repo.mkdir(parents=True)
    state.mkdir()
    project_key = project_root.project_key(repo)
    call = f"async-call:{name}"
    job_id = hashlib.sha256(call.encode("utf-8")).hexdigest()
    owner_session, origin_id = "owner-session", f"origin-{name}"
    call_key = hashlib.sha256(f"{owner_session}\0{origin_id}".encode("utf-8")).hexdigest()
    event = {"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex_result",
             "tool_use_id": f"followup-{name}", "session_id": owner_session, "cwd": str(repo),
             "tool_input": {"jobId": job_id}}
    if name in {"adoptable", "foreign_project"}:
        event["session_id"] = "successor-session"
        event["tool_input"]["adopt"] = True
    if name in {"consumed", "finalizing"}:
        event["tool_name"] = "mcp__codex-main__codex_cancel"

    index = {"jobId": job_id, "project_key": project_key, "call_key": call_key,
             "session_id": owner_session, "call": call, "stage": "ACTIVE"}
    pending = {"tool_use_id": origin_id, "job_id": job_id, "async_call": call,
               "session_id": owner_session, "project_key": project_key,
               "task_id": f"task-{name}", "baseline": {}}
    job = {"jobId": job_id, "call": call, "state": "SUCCEEDED"}
    if name == "owned":
        job["state"] = "RUNNING"
    elif name == "foreign_project":
        index["project_key"] = "foreign-project"
        pending["project_key"] = "foreign-project"
    elif name == "index_job_id_mismatch":
        index["jobId"] = "f" * 64
    elif name == "consumed":
        index["stage"], pending = "CONSUMED", None
        job["worktreePath"] = "/tmp/async-worktree"
    elif name == "finalizing":
        index["stage"] = "FINALIZING"

    disk = {"index": None if name == "missing_index" else index,
            "pending": None if name in {"missing_index", "index_without_pending", "consumed"} else pending,
            "job": None if name in {"missing_index", "missing_job_file", "index_job_id_mismatch"} else job}
    if disk["index"] is not None:
        (state / "async_jobs").mkdir()
        (state / "async_jobs" / f"{job_id}.json").write_text(json.dumps(disk["index"]), encoding="utf-8")
    if disk["pending"] is not None:
        (state / "pending").mkdir()
        (state / "pending" / f"{call_key}.json").write_text(json.dumps(disk["pending"]), encoding="utf-8")
    if disk["job"] is not None:
        jobs = state / "codex_exec_mcp" / "jobs"
        jobs.mkdir(parents=True)
        (jobs / f"{job_id}.json").write_text(json.dumps(disk["job"]), encoding="utf-8")

    try:
        path, owned_index, owned_pending, original = codex._async_owned(state, event, repo)
        ownership = {"path": path.relative_to(state).as_posix(), "stage": owned_index["stage"],
                     "pending_task_id": owned_pending.get("task_id") if owned_pending else None,
                     "original_session_id": original.get("session_id") if original else None,
                     "original_tool_use_id": original.get("tool_use_id") if original else None}
    except codex.WiringError as error:
        ownership = {"error": type(error).__name__, "message": str(error)}
    async_job = codex._async_job(state, disk["index"]) if disk["index"] is not None else None
    followup = codex._async_pre_followup(event, state, repo)
    result = {"name": name, "input": event, "disk": disk,
              "python": {"async_owned": ownership, "async_job": async_job, "pre_followup": followup}}
    return _replace(result, repo, project_key)


def test_async_owned_python_oracle_fixture(tmp_path):
    actual = [_capture(name, tmp_path / name) for name in NAMES]
    if os.environ.get("ASYNC_OWNED_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        cases = []
        for case in actual:
            cases.append("    {\n      " + json.dumps("name") + ": " + json.dumps(case["name"]) + ",\n"
                          "      " + json.dumps("input") + ": " + json.dumps(case["input"], sort_keys=True) + ",\n"
                          "      " + json.dumps("disk") + ": " + json.dumps(case["disk"], sort_keys=True) + ",\n"
                          "      " + json.dumps("python") + ": " + json.dumps(case["python"], sort_keys=True) + "\n"
                          "    }")
        cases = ",\n".join(cases)
        FIXTURE.write_text('{\n  "oracle": "codex._async_owned+_async_job+_async_pre_followup",\n'
                           f'  "cases": [\n{cases}\n  ]\n}}\n', encoding="utf-8")
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "codex._async_owned+_async_job+_async_pre_followup"
    assert [case["name"] for case in fixture["cases"]] == list(NAMES)
    assert actual == fixture["cases"]
