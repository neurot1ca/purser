import json
import os
from pathlib import Path
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "post_session.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger

NOW = 1_700_000_000.0
PROJECT = "fixture-project"
CASES = [
    {"name": "sync_success_releases", "phase": "implementation", "lease": True},
    {"name": "worktree_success_retains", "phase": "implementation", "lease": True, "retain": True},
    {"name": "plan_without_lease", "phase": "plan"},
    {"name": "ledger_save_failure", "phase": "implementation", "lease": True, "fail_save": True},
    {"name": "pending_already_gone", "phase": "operation", "lease": True, "gone": True},
    {"name": "bypass_without_routing", "phase": "implementation", "bypass": True},
]


def _capture(case, root):
    runtime, repo = root / "state", root / "repo"
    runtime.mkdir(parents=True)
    repo.mkdir()
    project_state = runtime / "projects" / PROJECT
    project_state.mkdir(parents=True)
    task = "session-" + case["name"]
    event = {"tool_use_id": "call-" + case["name"], "session_id": "session-p109"}
    owner = "owner-" + case["name"]
    pending = {"task_id": task, "project_key": PROJECT, "route": "operation", "model": "gpt-6-luna",
               "effort": "high", "phase": case["phase"], "scope": [], "agent": "codex",
               "task_contract_grounded": False}
    if case.get("lease"):
        pending["lease_owner"] = owner
    if case.get("retain"):
        pending["worktree_path"] = str(repo)
    if case.get("bypass"):
        pending["kind"] = "pretool_bypass"
        pending["route"] = pending["model"] = pending["effort"] = ""
    pending_file = codex._pending_path(runtime, event)
    pending_file.parent.mkdir(parents=True)
    if not case.get("gone"):
        pending_file.write_text(json.dumps(pending), encoding="utf-8")
    if case.get("lease"):
        assert ledger.ScopeLeases(project_state).acquire(owner, task, ["src"])["status"] == ledger.OK
    if case.get("fail_save"):
        (project_state / "sessions").write_text("blocked", encoding="utf-8")
    if case.get("retain"):
        # Python records unapplied worktree retention before the post tail.
        assert ledger.ScopeLeases(project_state).set_retention_after(owner, NOW + 7200)["status"] == ledger.OK
    bundle = {"findings": [], "claude_review_mode": "AUDIT", "status": "OK"}
    old_time = ledger.time.time
    ledger.time.time = lambda: NOW
    try:
        if case.get("bypass"):
            identity_fields = ("task_id", "route", "model", "effort")
            if all(isinstance(pending.get(field), str) and pending.get(field) for field in identity_fields):
                session = ledger.SessionLedger(project_state)
                saved = session.save(ledger.build_session_record(task, "thread-p109", pending["route"], pending["model"], pending["effort"],
                    agent="codex", phase=pending["phase"], scope=[], pending_call_baseline=None, last_outcome=bundle, label=None))
                if saved["status"] != ledger.OK:
                    bundle["findings"].append({"code": saved.get("code", "LEDGER_WRITE_FAILED"), "message": saved.get("message", "Bypass outcome ledger update failed.")})
            codex._remove_pending(runtime, event)
        else:
            options = {"agent": "codex", "phase": pending["phase"], "scope": [], "pending_call_baseline": None,
                       "last_outcome": bundle, "task_contract_grounded": False}
            if case["phase"] == "implementation":
                options.pop("phase")
                options.pop("scope")
            record = ledger.build_session_record(task, "thread-p109", pending["route"], pending["model"], pending["effort"], **options)
            saved = ledger.SessionLedger(project_state).save(record)
            if saved["status"] != ledger.OK:
                bundle["claude_review_mode"] = "FULL"
                bundle["findings"].append({"code": saved.get("code", "LEDGER_WRITE_FAILED"), "message": saved.get("message", "Ledger update failed.")})
            else:
                codex._record_thread_root(runtime, "thread-p109", repo)
            if case.get("lease") and not case.get("retain"):
                codex._release_scope_lease(runtime, pending)
            codex._remove_pending(runtime, event)
    finally:
        ledger.time.time = old_time
    session_file = next(iter((project_state / "sessions").glob("*.json")), None) if (project_state / "sessions").exists() else None
    lease_file = project_state / "scope_leases.json"
    thread_file = next(iter((runtime / "threads").glob("*.json")), None) if (runtime / "threads").exists() else None
    def read(path):
        return json.loads(path.read_text(encoding="utf-8")) if path and path.exists() else None
    return {"name": case["name"], "pending": pending_file.exists(), "session": read(session_file),
            "lease": read(lease_file), "thread": read(thread_file), "bundle": bundle,
            "repository": "__REPO__"}


def test_post_session_python_fixture_and_go_parity(tmp_path):
    scratch = ROOT / ".tmp" / ("post-session-" + uuid.uuid4().hex)
    scratch.mkdir(parents=True)
    old_time = ledger.time.time
    ledger.time.time = lambda: NOW
    try:
        actual = [_capture(case, scratch / str(index)) for index, case in enumerate(CASES)]
        for case in actual:
            if case["thread"]:
                case["thread"]["project_root"] = "__REPO__"
            if case["session"]:
                payload = case["session"]["payload"]
                if payload.get("last_outcome"):
                    payload["last_outcome"]["findings"] = []
        fixture = {"oracle": "Python codex post_tool_use session and cleanup tail", "cases": actual}
        if os.environ.get("POST_SESSION_WRITE_FIXTURE") == "1":
            FIXTURE.parent.mkdir(parents=True, exist_ok=True)
            FIXTURE.write_text(json.dumps(fixture, indent=2, ensure_ascii=False, sort_keys=True) + "\n", encoding="utf-8")
        assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
    finally:
        ledger.time.time = old_time
        import shutil
        shutil.rmtree(scratch, ignore_errors=True)
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"), "TEMP": str(ROOT / ".tmp"),
           "TMPDIR": str(ROOT / ".tmp"), "PYTHONDONTWRITEBYTECODE": "1"}
    result = subprocess.run(["go", "test", "-run", "^TestFinishPostSessionPythonFixture$", "-count=1", "./internal/enforce"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
