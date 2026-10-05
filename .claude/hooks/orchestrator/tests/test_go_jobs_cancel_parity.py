"""M4 pass 156 cancellation fixtures captured from Server._cancel_job."""
import json, os, sys
from pathlib import Path

ORCH = Path(__file__).resolve().parents[1]
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_cancel" / "cancel.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge

IDS = {name: letter * 64 for name, letter in zip(
    ("queued", "running_ack", "running_unconfirmed", "terminal"), "abcd")}


def _clean(value):
    if isinstance(value, dict):
        return {k: ("TIME" if k in ("finishedAt", "discardedAt") else
                    "PORT" if k == "signalPort" else _clean(v)) for k, v in value.items()}
    if isinstance(value, list):
        return [_clean(v) for v in value]
    return value


def test_go_cancel_matches_python_oracle(tmp_path, monkeypatch):
    records = {
        "queued": {"jobId": IDS["queued"], "state": "QUEUED", "acceptedAt": 10,
                   "leaseQueued": True},
        "running_ack": {"jobId": IDS["running_ack"], "state": "RUNNING", "acceptedAt": 10,
                        "signalPort": 1, "signalToken": "secret"},
        "running_unconfirmed": {"jobId": IDS["running_unconfirmed"], "state": "RUNNING",
                                 "acceptedAt": 10},
        "terminal": {"jobId": IDS["terminal"], "state": "SUCCEEDED", "acceptedAt": 10,
                     "finishedAt": 11, "worktreePath": "C:/repo/worktree", "applyState": "pending",
                     "taskId": "task-terminal"},
    }
    cases = []
    for name in ("unknown", "queued", "running_ack", "running_unconfirmed", "terminal"):
        side_effects = []
        monkeypatch.setenv("PURSER_STATE_DIR", str(tmp_path / name))
        server = bridge.Server(agent="codex")
        server.listener = None
        server._release_job_scope_lease = lambda _root, jid: side_effects.append(
            {"call": "releaseScopeLease", "jobId": jid}) or True
        monkeypatch.setattr(bridge, "_record_claude_decision",
                            lambda job, decision, kind, source: side_effects.append(
                                {"call": "recordDecision", "jobId": job["jobId"], "decision": decision,
                                 "kind": kind, "sourceJobId": source}))
        monkeypatch.setattr(bridge, "_record_scope_conflict_outcome",
                            lambda job, action: side_effects.append(
                                {"call": "recordScopeConflictOutcome", "jobId": job["jobId"], "action": action}))
        job_id = IDS.get(name)
        record = records.get(name)
        if record is not None:
            server._write_job(dict(record))
        if name == "running_ack":
            class Connection:
                def __enter__(self): return self
                def __exit__(self, *_): return False
                def settimeout(self, *_): pass
                def sendall(self, *_): pass
                def recv(self, *_):
                    server._finish_job(job_id, "CANCELLED", "CANCELLED")
                    return b"1"
            monkeypatch.setattr(bridge.socket, "create_connection", lambda *_a, **_k: Connection())
        elif name == "running_unconfirmed":
            monkeypatch.setattr(bridge.socket, "create_connection", lambda *_a, **_k: (_ for _ in ()).throw(OSError("offline")))
        result = server._cancel_job({"jobId": job_id})
        output = {p.name: _clean(json.loads(p.read_text(encoding="utf-8")))
                  for p in server.job_dir.glob("*.json")} if server.job_dir.exists() else {}
        cases.append({"name": name, "jobId": job_id or "missing", "record": record,
                      "result": {"structuredContent": result["structuredContent"],
                                 "isError": result["isError"]}, "files": output,
                      "sideEffects": side_effects})
    fixture = {"oracle": "real Server._cancel_job with monkeypatched host signal", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_CANCEL") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
