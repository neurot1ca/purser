"""154a start validation fixtures are captured from the real Python bridge."""
import hashlib, inspect, json, os, subprocess, sys
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


def _clean(value, root=None, git_values=None):
    if isinstance(value, dict): return {k:("TIME" if k=="acceptedAt" else "DEADLINE" if k=="deadlineAt" else "TOKEN" if k in ("signalToken","ownershipToken") else _clean(v,root,git_values)) for k,v in value.items()}
    if isinstance(value, list): return [_clean(v,root,git_values) for v in value]
    if isinstance(value,str):
        for original, normalized in (git_values or {}).items():
            if value == original: return normalized
        if root and value.startswith(str(root)): return "<ROOT>" + value[len(str(root)):]
    return value


def _start_case(name, calls, tmp_path, monkeypatch, seed=None, thread=None, git_values=None):
    root=tmp_path/name; server = _server(monkeypatch,root); launches=[]
    decisions=[]
    for record in seed or []: server._write_job(record)
    if thread: bridge._save_record(server.state_dir, thread)
    monkeypatch.setattr(bridge,"_launch_host",lambda _dir,jid,gen,token:launches.append({"jobId":jid,"generation":gen,"token":"TOKEN"}))
    monkeypatch.setattr(bridge,"_record_claude_decision",lambda job,decision,kind,source:decisions.append({"jobId":job["jobId"],"decision":decision,"kind":kind,"sourceJobId":source}))
    server._await_host = lambda jid: bridge._job_result({"jobId":jid,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})
    result=None
    for args in calls: result=server._start_job(args)
    files={}
    if server.job_dir.exists():
        for p in sorted(server.job_dir.iterdir()): files[p.name]=True if p.is_dir() or p.name.endswith(".owner-lock") else _clean(json.loads(p.read_text(encoding="utf-8")),root,git_values)
    return {"name":name,"calls":_clean(calls,root,git_values),"seed":_clean(seed,root,git_values),"thread":_clean(thread,root,git_values),"gitCase":git_values is not None,"result":_clean(result,root,git_values),"files":files or None,"launches":launches,"decisions":decisions}


def _git(root, *args):
    return subprocess.check_output(["git", *args], cwd=root, stderr=subprocess.PIPE).decode().strip()


def _applied_reply_case(tmp_path, monkeypatch):
    root=tmp_path/"reply_applied_rebase"; repo=root/"repo"; worktree=root/"worktree"; repo.mkdir(parents=True)
    _git(repo,"init","-b","main"); _git(repo,"config","user.name","Oracle"); _git(repo,"config","user.email","oracle@example.invalid")
    (repo/"base.txt").write_text("base\n",encoding="utf-8"); _git(repo,"add","base.txt"); _git(repo,"commit","-m","base")
    old_head=_git(repo,"rev-parse","HEAD"); old_tree=_git(repo,"rev-parse","HEAD^{tree}"); _git(repo,"worktree","add","--detach",str(worktree),"HEAD")
    (worktree/"applied.txt").write_text("already applied\n",encoding="utf-8"); (repo/"applied.txt").write_text("already applied\n",encoding="utf-8")
    _git(repo,"add","applied.txt"); _git(repo,"commit","-m","apply"); head=_git(repo,"rev-parse","HEAD"); tree=_git(repo,"rev-parse","HEAD^{tree}"); tid="thread-rebase"
    thread={"thread_id":tid,"cwd":str(worktree),"config_args":[],"worktreePath":str(worktree),"snapshot":old_tree,"baselineSnapshot":old_tree,"head":old_head,"repoRoot":str(repo)}
    seed=[{"jobId":"e"*64,"threadId":tid,"state":"SUCCEEDED","applyState":"pending","worktreePath":str(worktree),"snapshot":old_tree,"baselineSnapshot":old_tree,"head":old_head,"repoRoot":str(repo),"taskId":"task-rebase","acceptedAt":1}]
    paths={str(root):"<ROOT>",old_tree:"<OLD_TREE>",tree:"<TREE>",old_head:"<OLD_HEAD>",head:"<HEAD>"}
    return _start_case("reply_applied_rebase",[{"call":CALL,"prompt":"reply","mode":"reply","threadId":tid}],tmp_path,monkeypatch,seed,thread,paths)


def test_go_start_validation_matches_python_oracle(tmp_path, monkeypatch):
    monkeypatch.delenv("PURSER_AGENT", raising=False)
    cases = [
        _invalid("invalid_agent", {"agent": "other", "call": CALL, "prompt": "p"}, tmp_path, monkeypatch),
        _invalid("invalid_config", {"call": CALL, "prompt": "p"}, tmp_path, monkeypatch, True),
        _invalid("required", {"call": CALL}, tmp_path, monkeypatch),
        _invalid("uuid", {"call": "bad", "prompt": "p"}, tmp_path, monkeypatch),
        _invalid("timeout", {"call": CALL, "prompt": "p", "timeoutSeconds": True}, tmp_path, monkeypatch),
        _invalid("mode", {"call": CALL, "prompt": "p", "mode": "resume"}, tmp_path, monkeypatch),
        _invalid("cwd_type", {"call": CALL, "prompt": "p", "cwd": 1}, tmp_path, monkeypatch),
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
        _start_case("reply_unapplied",[{"call":CALL,"prompt":"reply","mode":"reply","threadId":"thread-1"}],tmp_path,monkeypatch,
            [{"jobId":"a"*64,"threadId":"thread-1","state":"SUCCEEDED","applyState":"pending","worktreePath":"C:/old-worktree","snapshot":"tree-1","taskId":"task-old","acceptedAt":1}],
            {"thread_id":"thread-1","cwd":"C:/repo","config_args":[],"snapshot":"tree-0","baselineSnapshot":"tree-0"}),
        _start_case("redispatch_reject_and_skip",[{"call":CALL,"prompt":"new","mode":"new","taskId":"task-x"}],tmp_path,monkeypatch,
            [{"jobId":"b"*64,"state":"SUCCEEDED","taskId":"task-x","worktreePath":"C:/wt-a","applyState":"pending","acceptedAt":1},
             {"jobId":"c"*64,"state":"SUCCEEDED","taskId":"task-x","worktreePath":"C:/wt-b","applyState":"applied","acceptedAt":2},
             {"jobId":"d"*64,"state":"SUCCEEDED","taskId":"task-x","worktreePath":"C:/wt-c","applyState":"pending","decisionRecorded":True,"acceptedAt":3}]),
        _applied_reply_case(tmp_path,monkeypatch),
    ])
    fixture = {"oracle": "real Server._start_job before jobs_lock", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_START") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
