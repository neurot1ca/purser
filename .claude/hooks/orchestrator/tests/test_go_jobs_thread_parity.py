"""Python-oracle fixtures for Go Codex thread persistence and reply options."""
import json
import os
from pathlib import Path
import shutil
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_thread" / "records.json"
sys.path.insert(0, str(ORCH))
import codex_exec_mcp


def _case(name, thread_id, record=None, initial=None, corrupt=None):
    state = ROOT / ".tmp" / "go-jobs-thread-oracle" / name
    state.mkdir(parents=True, exist_ok=True)
    path = codex_exec_mcp._record_path(state, thread_id)
    if name == "missing":
        loaded = codex_exec_mcp._load_record(state, thread_id)
        return {"name": name, "thread_id": thread_id, "loaded": loaded}
    if corrupt is not None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(corrupt, encoding="utf-8")
        return {"name": name, "thread_id": thread_id, "corrupt": corrupt,
                "loaded": codex_exec_mcp._load_record(state, thread_id)}
    if initial is not None:
        codex_exec_mcp._save_record(state, initial)
    codex_exec_mcp._save_record(state, record)
    raw = path.read_text(encoding="utf-8")
    loaded = codex_exec_mcp._load_record(state, thread_id)
    value = {"name": name, "thread_id": thread_id, "record": record,
             "record_json": raw, "loaded": loaded}
    if initial is not None:
        value["initial"] = initial
    return value


def _reply_case(monkeypatch):
    root = ROOT / ".tmp" / "go-jobs-thread-oracle" / "reply"
    shutil.rmtree(root, ignore_errors=True)
    state = root / "state"
    jobs = root / "jobs"
    state.mkdir(parents=True)
    jobs.mkdir()
    monkeypatch.setenv("PURSER_CODEX_SHIM_STATE", str(state))
    record = {"thread_id": "thread-reply", "cwd": "C:/saved/cwd", "model": "codex-z",
              "config_args": ["model_reasoning_effort=high", "tool_output_token_limit=9000",
                              'sandbox_mode="workspace-write"'],
              "sandbox": "read-only", "agent": "codex", "worktreePath": "thread-worktree",
              "future": [1, "二"]}
    codex_exec_mcp._save_record(state, record)
    codex_exec_mcp._save_record(state, {"thread_id": "thread-metadata", "cwd": "C:/saved/cwd",
                                         "model": "codex-z", "config_args": [], "sandbox": None,
                                         "agent": "codex", "repoRoot": str(ROOT)})
    server = codex_exec_mcp.Server(codex_bin='["unused-codex"]', agent="codex", job_dir=jobs)
    launched = {}

    class StopBeforeLaunch(BaseException):
        pass

    real_is_dir = Path.is_dir
    monkeypatch.setattr(Path, "is_dir", lambda path: str(path) == "thread-worktree" or real_is_dir(path))

    def capture_agent_command(agent, prefix, name, cwd, model, config_args, sandbox,
                              thread_id, output_path, prompt):
        launched.update(agent=agent, name=name, cwd=cwd, model=model,
                        config_args=config_args, sandbox=sandbox, thread_id=thread_id)
        return ["unused"]

    with monkeypatch.context() as patch:
        patch.setattr(codex_exec_mcp, "_agent_command", capture_agent_command)
        patch.setattr(codex_exec_mcp.subprocess, "Popen", lambda *args, **kwargs: (_ for _ in ()).throw(StopBeforeLaunch()))
        try:
            server._run_tool_once("codex-reply", {"threadId": "thread-reply", "prompt": "continue"}, "reply")
        except StopBeforeLaunch:
            pass
        else:
            raise AssertionError("reply did not reach the process-launch seam")

    # Exercise the real durable reply option merge twice: explicit job* values
    # override aliases, while aliases supply values when job* is absent.
    written = []
    metadata_inputs = []
    server._write_job = lambda job: written.append(dict(job))
    server._await_host = lambda job_id: {"jobId": job_id, "state": "QUEUED"}
    with monkeypatch.context() as patch:
        patch.setattr(codex_exec_mcp, "_launch_host", lambda *args, **kwargs: None)
        for call, options in (
            ("00000000-0000-4000-8000-000000000001", {
                "model": "alias-model", "jobModel": "explicit-model",
                "effort": "low", "jobEffort": "high",
                "label": "alias-label", "jobLabel": "explicit-label",
                "phase": "queued", "jobPhase": "review"}),
            ("00000000-0000-4000-8000-000000000002", {
                "model": "alias-model", "effort": "medium",
                "label": "alias-label", "phase": "queued"}),
        ):
            server._start_job({"call": call, "prompt": "continue", "mode": "reply",
                               "threadId": "thread-metadata", **options})
            metadata_inputs.append(options)

    fields = ("model", "effort", "label", "phase")
    return {"name": "reply option merge", "thread_id": record["thread_id"],
            "record": record, "record_json": codex_exec_mcp._record_path(state, record["thread_id"]).read_text(encoding="utf-8"),
            "loaded": codex_exec_mcp._load_record(state, record["thread_id"]),
            "reply_config": {key: launched[key] for key in
                             ("cwd", "model", "config_args", "sandbox", "agent", "thread_id")},
            "metadata": [{"args": args, "recorded": {key: job[key] for key in fields if key in job}}
                         for args, job in zip(metadata_inputs, written)]}


def _oracle(monkeypatch):
    thread = "thread-雪-42"
    base = {"thread_id": thread, "cwd": "C:/work/repo", "model": "codex-x",
            "config_args": ["model_reasoning_effort=high", "tool_output_token_limit=9000",
                            'sandbox_mode="workspace-write"'],
            "sandbox": "read-only", "agent": "codex"}
    overwritten = {"thread_id": "thread-overwrite", "cwd": "C:/work/updated",
                   "model": "codex-y", "config_args": ["model_reasoning_effort=medium"],
                   "sandbox": "workspace-write", "agent": "codex", "future_field": {"kept": "✓"}}
    return [
        _case("new save", thread, base),
        _case("overwrite unknown fields", "thread-overwrite", overwritten,
              {"thread_id": "thread-overwrite", "cwd": "C:/old", "model": "old",
               "config_args": [], "sandbox": None, "agent": "codex"}),
        _case("missing", "missing-thread"),
        _case("corrupt", "corrupt-thread", corrupt="{bad json"),
        _reply_case(monkeypatch),
    ]


def test_go_thread_matches_python_oracle(monkeypatch):
    cases = _oracle(monkeypatch)
    fixture = {"oracle": "codex_exec_mcp record functions and real Server reply paths",
               "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_THREAD") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, separators=(",", ":")) + "\n",
                           encoding="utf-8")
    committed = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert committed == fixture
