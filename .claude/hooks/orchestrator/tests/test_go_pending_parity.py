import json
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
