import json
import hashlib
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "pending.json"
sys.path.insert(0, str(ORCH))
import codex
import ledger


def _repo(path, content):
    path.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "init", "-q"], cwd=path, check=True)
    (path / "tracked.txt").write_text(content, encoding="utf-8", newline="\n")
    subprocess.run(["git", "add", "tracked.txt"], cwd=path, check=True)
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"], cwd=path, env=env, check=True, capture_output=True)
    return path.resolve()


def _normalize_paths(value, repo, worktree):
    if isinstance(value, str):
        return value.replace(str(repo), "__REPO__").replace(str(worktree), "__WORKTREE__")
    if isinstance(value, list):
        return [_normalize_paths(item, repo, worktree) for item in value]
    if isinstance(value, dict):
        return {key: _normalize_paths(item, repo, worktree) for key, item in value.items()}
    return value


def _expected(case, root, repo, worktree):
    scope = []
    if repo is None:
        baseline = {"status": ledger.UNKNOWN, "code": "PROJECT_ROOT_UNRESOLVED", "message": case["project_root_reason"], "baseline_head": None}
    else:
        baseline = ledger.capture_baseline(worktree if case.get("worktree") else repo, None)
    live = None
    if case.get("handoff"):
        post = dict(baseline)
        if case["handoff"] == "mismatch":
            post["post_only"] = True
        live = {"origin_baseline": {"status": "OK", "origin": case["name"]}, "origin_scope": [], "post_state": post, "profile_checks": [{"name": "inherited"}]}
        if case.get("live_from_disk"):
            live_path = root / "live-thread.json"
            live_path.write_text(json.dumps(live), encoding="utf-8")
            live = json.loads(live_path.read_text(encoding="utf-8"))
        if codex._handoff_matches(baseline, live, scope):
            baseline = live["origin_baseline"]
    checks = codex._check_snapshot([codex.project_profile.Check(name="build", argv=["go", "test"], when=None, timeout_seconds=30)])
    pending = {
        "kind": "call", "task_id": "task-p46", "project_key": "repo-p46", "phase": "implementation",
        "route": "operation", "model": "gpt-test", "effort": "high", "label": "pending parity", "agent": "codex",
        "fallback_family": None, "mechanical_opencode": False, "shadow_prompt": None, "scope": scope,
        "repo_root": str(repo) if repo else None, "project_root_reason": case.get("project_root_reason"),
        "baseline": baseline, "prior_thread_id": "thread-p46" if case.get("reply") else None,
        "task_objective": "Inspect pending construction", "acceptance_criteria": "Every field matches Python", "task_contract_grounded": True,
        "readiness_check": {"agent": "off"}, "task_type": "operation", "suspected_route_mismatch": None,
        "has_code_map": False, "reason": "verification", "eta_minutes": None, "reason_source": "explicit",
        "previous_route": None, "new_route": None,
        "profile_checks": live["profile_checks"] if live and isinstance(live.get("profile_checks"), list) else checks,
        "protected_paths": ["AGENTS.md"], "force_full_review": True, "skip_max_lines": 50, "skip_max_files": 2,
        "reviewer_executable": None, "reviewer": "agy", "reviewer_candidates": [{"agent": "agy", "model": "reviewer-model"}],
        "authority_sources": [{"path": "AGENTS.md", "headings": ["Authority"], "when_changed": None}],
        "vault_root": None, "configured": True, "user_status": "ok", "project_status": "ok", "first_warning": None,
    }
    if case.get("worktree"):
        pending["worktree_path"] = str(worktree)
    if case.get("queued"):
        baseline = {"status": ledger.UNKNOWN, "code": "QUEUED"}
        pending["lease_queued"] = True
        pending["baseline"] = baseline
    return {"baseline": baseline, "pending": pending}


def test_p46_python_oracle_golden(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "codex._handoff_matches+ledger.capture_baseline+codex.Pre pending fields"
    repo = _repo(tmp_path / "repo", "base\n")
    worktree = _repo(tmp_path / "worktree", "worktree\n")
    for golden in fixture["cases"]:
        case = golden["input"]
        actual = _expected(case, tmp_path, repo if case["repository"] else None, worktree)
        # Only temporary-root identities are normalized in the checked-in golden.
        assert _normalize_paths(actual, repo, worktree) == golden["expected"], case["name"]


def test_p46_go_pending_fixture():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(scratch), "TEMP": str(scratch), "TMPDIR": str(scratch), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestPendingCallPythonFixture$", "-count=1", "./internal/enforce"], cwd=ROOT / "go", env=env, check=True)


def test_p47_python_admission_oracle(tmp_path, monkeypatch):
    fixture_path = ORCH / "tests" / "fixtures" / "go_policy" / "pending_admission.json"
    fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "codex._reconcile_rejected_starts+ledger.ScopeLeases.acquire+pending/session/thread-index writes"
    monkeypatch.setattr(ledger.time, "time", lambda: 1000.0)
    for case in fixture["cases"]:
        setup = case["setup"]
        root = tmp_path / case["name"]
        root.mkdir()
        project = root / "projects" / "project-p47"
        project.mkdir(parents=True)
        pending_dir = root / "pending"
        pending_dir.mkdir()
        pending_path = pending_dir / "current.json"
        if setup in {"conflict", "stale"}:
            old_call = "call-old"
            old_job = hashlib.sha256(old_call.encode()).hexdigest()
            seeded = ledger.ScopeLeases(project).acquire("oldowner", "old-task", ["src"], job_id=old_job)
            assert seeded["status"] == ledger.OK
            if setup == "stale":
                codex._atomic_json_write(pending_dir / "stale.json", {
                    "start_rejected": True, "project_key": "project-p47",
                    "async_call": old_call, "lease_owner": "oldowner",
                })
        elif setup == "queued":
            store = ledger.ScopeLeases(project)
            assert store.acquire("blocker", "blocking-task", ["src"], job_id="blocker-job")["status"] == ledger.OK
            assert store.acquire("owner-p47", "task-p47", ["src"], job_id="job-p47", queue_conflict=True)["code"] == "LEASE_QUEUED"
        elif setup == "lease_uncertain":
            (project / "scope_leases.json").write_text("{", encoding="utf-8")
        if setup == "fail_pending":
            pending_dir.rmdir()
            pending_dir.write_text("blocked", encoding="utf-8")
        elif setup == "fail_session":
            (project / "sessions").write_text("blocked", encoding="utf-8")
        elif setup == "fail_index":
            (root / "threads").write_text("blocked", encoding="utf-8")

        scope = [] if setup == "plan" else ["src"]
        repository = None if setup == "unresolved" else tmp_path / "repo"
        if repository is not None:
            repository.mkdir(exist_ok=True)
        pending = {"task_id": "task-p47", "project_key": "project-p47",
                   "phase": "plan" if setup == "plan" else "implementation",
                   "baseline": {"status": "OK", "head": "base"}}
        session = ledger.build_session_record(
            "task-p47", "thread-p47" if repository is not None else None,
            "operation", "model", "high", phase=pending["phase"], scope=scope,
            pending_call_baseline=pending["baseline"], task_contract_grounded=True,
            task_objective="fixture objective", acceptance_criteria="fixture acceptance",
        )
        owner = "owner-p47"
        admission = {"status": ledger.OK, "code": "LEASE_ACQUIRED"}
        if repository is not None:
            if setup in {"implementation", "plan", "stale", "conflict", "queued"}:
                codex._reconcile_rejected_starts(root, "project-p47")
            admission = ledger.ScopeLeases(project).acquire(
                owner, "task-p47", scope, read_only=setup == "plan",
            )
        actual = {"status": admission["status"], "code": admission["code"], "queued": False}
        if admission["status"] == ledger.OK:
            pending["lease_owner"] = owner if repository is not None else None
            if repository is None:
                pending.pop("lease_owner")
            if admission["code"] == "LEASE_QUEUED":
                queued = {"status": ledger.UNKNOWN, "code": "QUEUED"}
                pending.update(lease_queued=True, baseline=queued)
                session["pending_call_baseline"] = queued
                actual["queued"] = True
            try:
                codex._atomic_json_write(pending_path, pending)
                saved = ledger.SessionLedger(project).save(session)
                if saved["status"] != ledger.OK:
                    actual.update(status=saved["status"], code=saved["code"])
                    raise OSError("session write failed")
                if repository is not None:
                    codex._record_thread_root(root, "thread-p47", repository)
            except OSError:
                if repository is not None:
                    ledger.ScopeLeases(project).release(owner)
                try:
                    pending_path.unlink(missing_ok=True)
                except OSError:
                    pass
                if setup == "fail_pending":
                    actual.update(status=ledger.UNKNOWN, code="PENDING_WRITE_FAILED")
                elif setup == "fail_session":
                    actual.update(status=ledger.UNKNOWN, code="LEDGER_WRITE_FAILED")
                elif setup == "fail_index":
                    actual.update(status=ledger.UNKNOWN, code="THREAD_INDEX_WRITE_FAILED")
                else:
                    raise
        actual.update(
            pending=pending_path.is_file(),
            session=ledger.SessionLedger(project).path_for_task("task-p47").is_file(),
            thread_index=(codex._thread_index_path(root, "thread-p47").is_file()
                          if repository is not None else False),
        )
        if setup == "stale":
            actual["stale_pending"] = (pending_dir / "stale.json").exists()
        assert actual == case["expected"], case["name"]


def test_p47_go_admission_fixture():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(scratch), "TEMP": str(scratch), "TMPDIR": str(scratch), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestPendingAdmissionP47Fixture$", "-count=1", "./internal/enforce"], cwd=ROOT / "go", env=env, check=True)
