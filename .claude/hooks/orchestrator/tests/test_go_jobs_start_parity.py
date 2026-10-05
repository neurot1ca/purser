"""154a start validation fixtures are captured from the real Python bridge."""
import hashlib, inspect, json, os, sys
from pathlib import Path
import pytest

ORCH = Path(__file__).resolve().parents[4] / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_jobs_start" / "start.json"
sys.path.insert(0, str(ORCH))
sys.dont_write_bytecode = True
import codex_exec_mcp as bridge

CALL = "00000000-0000-4000-8000-000000000001"


def _server(monkeypatch, state_root, config_error=False):
    monkeypatch.setenv("PURSER_STATE_DIR", str(state_root))
    server = bridge.Server(agent="codex"); server._agent_prefix = lambda *_: ["codex"]
    if config_error:
        server._fixed_agent = False; server._configured_agent = lambda: (_ for _ in ()).throw(ValueError("bad configuration"))
    return server


def _invalid(name, args, tmp_path, monkeypatch, config_error=False):
    server = _server(monkeypatch, tmp_path / name, config_error); result = server._start_job(args); assert not server.job_dir.exists()
    return {"name": name, "args": args, "result": result}


def _clean(value):
    if isinstance(value, dict): return {k:("TIME" if k=="acceptedAt" else "DEADLINE" if k=="deadlineAt" else "TOKEN" if k in ("signalToken","ownershipToken") else _clean(v)) for k,v in value.items()}
    if isinstance(value, list): return [_clean(v) for v in value]
    return value


def _start_case(name, calls, tmp_path, monkeypatch):
    server = _server(monkeypatch,tmp_path/name); launches=[]
    monkeypatch.setattr(bridge,"_launch_host",lambda _dir,jid,gen,token:launches.append({"jobId":jid,"generation":gen,"token":"TOKEN"}))
    server._await_host = lambda jid: bridge._job_result({"jobId":jid,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})
    result=None
    for args in calls: result=server._start_job(args)
    files={}
    if server.job_dir.exists():
        for p in sorted(server.job_dir.iterdir()): files[p.name]=True if p.is_dir() else _clean(json.loads(p.read_text(encoding="utf-8")))
    return {"name":name,"calls":calls,"result":_clean(result),"files":files or None,"launches":launches}


def test_go_start_validation_matches_python_oracle(tmp_path, monkeypatch):
    monkeypatch.delenv("PURSER_AGENT", raising=False)
    cases = [
        _invalid("invalid_agent", {"agent": "other", "call": CALL, "prompt": "p"}, tmp_path, monkeypatch),
        _invalid("invalid_config", {"call": CALL, "prompt": "p"}, tmp_path, monkeypatch, True),
        _invalid("required", {"call": CALL}, tmp_path, monkeypatch),
        _invalid("uuid", {"call": "bad", "prompt": "p"}, tmp_path, monkeypatch),
        _invalid("timeout", {"call": CALL, "prompt": "p", "timeoutSeconds": True}, tmp_path, monkeypatch),
        _invalid("mode", {"call": CALL, "prompt": "p", "mode": "resume"}, tmp_path, monkeypatch),
        _invalid("cwd", {"call": CALL, "prompt": "p", "cwd": ""}, tmp_path, monkeypatch),
        _invalid("model", {"call": CALL, "prompt": "p", "model": 1}, tmp_path, monkeypatch),
        _invalid("config_type", {"call": CALL, "prompt": "p", "config": []}, tmp_path, monkeypatch),
        _invalid("config_value", {"call": CALL, "prompt": "p", "config": {"x": None}}, tmp_path, monkeypatch),
        _invalid("sandbox", {"call": CALL, "prompt": "p", "sandbox": "bad"}, tmp_path, monkeypatch),
        _invalid("worktree", {"call": CALL, "prompt": "p", "worktree": 1}, tmp_path, monkeypatch),
        _invalid("approval", {"call": CALL, "prompt": "p", "approval-policy": "always"}, tmp_path, monkeypatch),
        _invalid("instructions", {"call": CALL, "prompt": "p", "compact-prompt": None}, tmp_path, monkeypatch),
    ]

    server = _server(monkeypatch, tmp_path / "valid")
    observed = {}
    class StopAtLock:
        def __enter__(self):
            local = inspect.currentframe().f_back.f_locals
            observed.update({dst: local[src] for dst, src in (("jobId","job_id"),("call","call"),("prompt","prompt"),("agent","job_agent"),("mode","mode"),("name","name"),("timeoutSeconds","timeout"),("args","args"))})
            raise RuntimeError("validation boundary")
        __exit__ = lambda self, *_: False
    server.jobs_lock = StopAtLock()
    valid_args = {"call":"urn:00000000uuid:0000-4000-8000-000000000001","prompt":"p","cwd":"C:/repo"}
    with pytest.raises(RuntimeError,match="validation boundary"): server._start_job(valid_args)
    assert not server.job_dir.exists()
    cases.append({"name":"uuid_cleanup","args":valid_args,"request":observed})
    server = _server(monkeypatch, tmp_path / "duplicate")
    job_id = hashlib.sha256(CALL.encode()).hexdigest()
    server._write_job({"jobId":job_id,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})
    server._await_host = lambda _: bridge._job_result({"jobId":job_id,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})
    duplicate_args = {"call":CALL,"prompt":"p","timeoutSeconds":False,"mode":"invalid"}
    cases.append({"name":"duplicate_before_validation","args":duplicate_args,"existing":True,"result":server._start_job(duplicate_args)})
    cases.extend([
        _start_case("unknown_thread",[{"call":CALL,"prompt":"p","mode":"reply","threadId":"missing"}],tmp_path,monkeypatch),
        _start_case("valid_new",[{"call":CALL,"prompt":"p","cwd":"C:/repo","taskId":"task-1","jobModel":"model-x"}],tmp_path,monkeypatch),
        _start_case("duplicate_same",[{"call":CALL,"prompt":"p","cwd":"C:/repo"}]*2,tmp_path,monkeypatch),
        _start_case("duplicate_different",[{"call":CALL,"prompt":"p","cwd":"C:/repo"},{"call":CALL,"prompt":"changed","timeoutSeconds":0}],tmp_path,monkeypatch),
    ])
    fixture = {"oracle": "real Server._start_job before jobs_lock", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_START") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
