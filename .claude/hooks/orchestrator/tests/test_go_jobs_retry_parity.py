"""Python oracle for Go retry and mechanical fallback decisions."""
import copy, json, os, sys, time, uuid
from pathlib import Path
import pytest

ROOT=Path(__file__).resolve().parents[4]; ORCH=ROOT/".claude"/"hooks"/"orchestrator"
FIXTURE=ORCH/"tests"/"fixtures"/"go_jobs_retry"/"retry.json"
sys.path.insert(0,str(ORCH)); import codex_exec_mcp as bridge
import coder_config, model_listing, ledger

def _cases():
    empty={"launchFailure":{"agent":"opencode","events":[],"stderr":"","started":True,"stdout":False,"output":False}}
    fail={"type":"PROVIDER_ERROR","noWorkProof":True}
    ambiguous={"type":"AMBIGUOUS","noWorkProof":False}
    def err(ev, missing=False):
        return {"isError":True,"errorCode":"MODEL_MISSING" if missing else "CODEX_FAILED",
                "retryableModelMissing":missing,"typedEvidence":ev}
    ok={"isError":False,"thread":"served"}
    return [
        {"name":"success_first_try","kind":"fallback","attempts":[ok]},
        {"name":"transient_retry_then_success","kind":"responsive","attempts":[empty,ok]},
        {"name":"retry_after_header","kind":"evidence","events":[{"type":"error","message":"HTTP 429","headers":{"Retry-After":"17"}}]},
        {"name":"exhausted","kind":"responsive","attempts":[empty for _ in range(5)]},
        {"name":"candidate_failover","kind":"fallback","attempts":[err(fail,True),ok],"candidates":["vendor/next"]},
        {"name":"model_missing","kind":"fallback","attempts":[err(fail,True)],"mark":False},
        {"name":"fallback_to_codex","kind":"fallback","attempts":[err(fail),ok],"candidates":[],"fallback":"codex"},
        {"name":"ambiguous_no_retry","kind":"fallback","attempts":[err(ambiguous)],"candidates":["vendor/next"]},
        {"name":"edited_tree_no_retry","kind":"fallback","attempts":[err(fail)],"changed":True,"fallback":"codex"},
    ]

def _project(result, retry_after=None):
    value=result.get("structuredContent",{})
    return {"attemptedModels":result.get("attemptedModels",value.get("attemptedModels",[])),
            "servedAgent":value.get("servedAgent"),"servedModel":value.get("servedModel"),
            "responsivenessAttempts":result.get("responsivenessAttempts"),"isError":result.get("isError",False),
            "errorCode":result.get("errorCode"),"retryAfterSeconds":retry_after}

def _replay(case, monkeypatch):
    if case["kind"]=="evidence":
        e=bridge._launch_failure_evidence("opencode",case["events"])
        return _project({"typedEvidence":e,"isError":True},e["retryAfterSeconds"])
    sequence=list(case["attempts"])
    if case["kind"]=="responsive":
        now=[0.0]
        def sleep(seconds): now[0]+=seconds
        def attempt():
            item=copy.deepcopy(sequence.pop(0))
            if "launchFailure" in item:
                spec=item["launchFailure"]
                item={"typedEvidence":bridge._launch_failure_evidence(spec["agent"],spec["events"],stderr=spec["stderr"],process_started=spec["started"],stdout_had_output=spec["stdout"],output_had_data=spec["output"])}
            return item
        result=bridge._run_with_responsiveness_retries(attempt,clock=lambda:now[0],sleep=sleep)
        return _project(result)
    candidates=case.get("candidates",[])
    monkeypatch.setattr(coder_config,"opencode_candidates",lambda:candidates)
    monkeypatch.setattr(model_listing,"mark_candidate_failed",lambda model:case.get("mark",True))
    monkeypatch.setattr(ledger,"compute_job_delta",lambda *a:{"status":ledger.OK,"changed_paths":["edit"] if case.get("changed") else []})
    server=bridge.Server(job_dir=Path(".")); jobid=uuid.uuid4().hex
    server._read_job=lambda _id:{"deadlineAt":time.time()+60,"worktreePath":"unused","snapshot":"s"}
    calls=[]
    def attempt(_name,args,_id,_async):
        calls.append((args.get("agent","opencode"),args.get("model")))
        item=copy.deepcopy(sequence.pop(0))
        return bridge._tool_result("t",content="ok") if not item["isError"] else item
    server._run_tool_once=attempt
    args={"agent":"opencode","model":"vendor/first","mechanicalFallback":True,
          "fallbackAgent":case.get("fallback","codex"),"fallbackModel":"gpt-6-sol","fallbackEffort":"medium"}
    result=server._run_tool("codex",args,jobid,async_job=True)
    return _project(result)

def test_python_retry_oracle(monkeypatch):
    cases=_cases(); actual=[{**c,"expected":_replay(c,monkeypatch)} for c in cases]
    if os.environ.get("PURSER_REGEN_GO_JOBS_RETRY")=="1":
        FIXTURE.parent.mkdir(parents=True,exist_ok=True); FIXTURE.write_text(json.dumps(actual,indent=2)+"\n",encoding="utf-8")
    assert FIXTURE.is_file(),f"missing {FIXTURE}; set PURSER_REGEN_GO_JOBS_RETRY=1 to capture"
    assert json.loads(FIXTURE.read_text(encoding="utf-8"))==actual
