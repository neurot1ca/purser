"""M4 pass 155 finish/result fixtures come from the real Python Server."""
import hashlib, json, os, sys
from pathlib import Path

ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_finish" / "finish.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge
import ledger

JOB = "0123456789abcdef" * 4
NOW = 2_000_000_000.0


def _server(tmp_path, monkeypatch):
    monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path))
    return bridge.Server(agent="codex", job_dir=tmp_path / "codex_exec_mcp" / "jobs")


def _clean(value):
    if isinstance(value, dict):
        return {k: ("TIME" if k in ("acceptedAt", "finishedAt") else "DEADLINE" if k == "deadlineAt" else _clean(v)) for k, v in value.items()}
    if isinstance(value, (list, tuple)): return [_clean(v) for v in value]
    return value


def _summary(value):
    if isinstance(value, dict): return {k: _summary(v) for k, v in value.items()}
    if isinstance(value, list): return [_summary(v) for v in value]
    if isinstance(value, str) and len(value) > 10_000:
        return {"length": len(value), "sha256": hashlib.sha256(value.encode()).hexdigest(),
                "head": value[:32], "tail": value[-32:]}
    return value


def _record(state="RUNNING", **extra):
    return {"jobId": JOB, "state": state, "acceptedAt": NOW - 20, "deadlineAt": NOW + 30, **extra}


def _capture(name, server, finish=None, result_args=None, decisions=None, releases=None):
    if finish:
        server._finish_job(JOB, *finish)
    result = server._result_job(result_args or {"jobId": JOB})
    files = {}
    for p in sorted(server.job_dir.iterdir()) if server.job_dir.exists() else []:
        if p.name.endswith(".json"):
            files[p.name] = _clean(json.loads(p.read_text(encoding="utf-8")))
        elif p.name.endswith(".output"):
            files[p.name] = p.read_text(encoding="utf-8")
    return {"name": name, "finish": _summary(_clean(finish)), "now": NOW,
            "result": _summary(_clean(result)), "files": _summary(files or None),
            "decisions": decisions or [], "releases": releases or []}


def test_go_jobs_finish_and_result_match_python(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge.time, "time", lambda: NOW)
    cases = []
    s = _server(tmp_path / "success", monkeypatch); s._write_job(_record())
    cases.append(_capture("succeeded_output", s, ("SUCCEEDED", None, {"content": "done", "threadId": "thread-1"}, None)))
    s = _server(tmp_path / "oversized", monkeypatch); s._write_job(_record())
    cases.append(_capture("output_over_limit", s, ("SUCCEEDED", None, {"content": "x" * 1_000_001, "threadId": "thread-1"}, None)))
    s = _server(tmp_path / "failed", monkeypatch); s._write_job(_record())
    cases.append(_capture("failed_error_code", s, ("FAILED", "CODEX_FAILED", None, None)))
    s = _server(tmp_path / "expired", monkeypatch); s._write_job(_record("FAILED", finishedAt=NOW - bridge.RETENTION_SECONDS - 1, errorCode="CODEX_FAILED", message="Codex failed"))
    cases.append(_capture("expired_record", s))
    s = _server(tmp_path / "missing", monkeypatch); s._write_job(_record("SUCCEEDED", finishedAt=NOW, threadId="thread-1", outputFile=JOB + ".output"))
    cases.append(_capture("terminal_missing_output", s))
    s = _server(tmp_path / "unknown", monkeypatch)
    cases.append(_capture("unknown_job", s))
    s = _server(tmp_path / "running", monkeypatch); s._write_job(_record())
    monkeypatch.setattr(s, "recover_job", lambda *_a, **_k: "UNCONFIRMED")
    cases.append(_capture("still_running", s))
    s = _server(tmp_path / "setup", monkeypatch); s._write_job(_record(setupError={"timedOut": True, "output": "setup log"}))
    cases.append(_capture("worktree_setup_failed", s, ("FAILED", "WORKTREE_SETUP_FAILED", None, None)))
    s = _server(tmp_path / "lease", monkeypatch); s._write_job(_record(leaseQueued=True)); releases=[]
    root=s.job_dir.parent.parent; (root / "async_jobs").mkdir(parents=True)
    (root / "async_jobs" / (JOB + ".json")).write_text(json.dumps({"project_key":"project-1", "call_key":"call-1"}), encoding="utf-8")
    class FakeLeases:
        def __init__(self, path): self.project=Path(path).name
        def release(self, call): releases.append({"project":self.project, "callKey":call})
    monkeypatch.setattr(ledger, "ScopeLeases", FakeLeases)
    cases.append(_capture("queued_lease_release", s, ("CANCELLED", "CANCELLED", None, None), releases=releases))
    s = _server(tmp_path / "decision", monkeypatch); decisions=[]
    s._write_job(_record("SUCCEEDED", finishedAt=NOW, threadId="thread-1", outputFile=JOB+".output", taskId="task-accepted"))
    (s.job_dir / (JOB+".output")).write_text("accepted", encoding="utf-8")
    monkeypatch.setattr(bridge, "_record_claude_decision", lambda job,decision,kind,source: decisions.append({"jobId":job["jobId"],"decision":decision,"kind":kind,"sourceJobId":source}))
    cases.append(_capture("accepted_decision", s, decisions=decisions))
    fixture = {"oracle": "real Server._finish_job and Server._result_job", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_FINISH") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
