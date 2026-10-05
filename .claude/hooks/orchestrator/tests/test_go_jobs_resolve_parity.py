import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / ".claude" / "hooks" / "orchestrator"))
import ledger

FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_jobs_resolve" / "oracle.json"
CASES = [
    {"name": "not_in_conflict", "outcome": "not_in_conflict"},
    {"name": "markers_remain", "outcome": "markers_remain", "repo": "<<<<<<< ours\nresolved\n>>>>>>> theirs\n"},
    {"name": "unmerged_path", "outcome": "unmerged_path", "merge": True},
    {"name": "clean_resolution", "outcome": "clean_resolution", "repo": "hand resolved\n", "shared": True},
]


def _git(root, *args):
    return subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE).stdout


def _case(tmp_path, case, monkeypatch):
    import codex_exec_mcp

    base = tmp_path / case["name"]
    repo, state, worktree = base / "repo", base / "state", base / "worktree"
    repo.mkdir(parents=True)
    _git(repo, "init", "-q")
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    _git(repo, "add", "--all")
    _git(repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
    branch = _git(repo, "branch", "--show-current").decode().strip()
    _git(repo, "worktree", "add", "--detach", str(worktree), "HEAD")
    snapshot = ledger.checkout_snapshot(worktree)
    (worktree / "tracked.txt").write_text("worker change\n", encoding="utf-8")
    if case.get("merge"):
        _git(repo, "checkout", "-b", "side")
        (repo / "tracked.txt").write_text("side change\n", encoding="utf-8")
        _git(repo, "add", "tracked.txt")
        _git(repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "side")
        _git(repo, "checkout", branch)
        (repo / "tracked.txt").write_text("main change\n", encoding="utf-8")
        _git(repo, "add", "tracked.txt")
        _git(repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "main")
        subprocess.run(["git", "merge", "side"], cwd=repo, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    elif "repo" in case:
        (repo / "tracked.txt").write_text(case["repo"], encoding="utf-8")
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    server = codex_exec_mcp.Server(agent="codex")
    job_id = "0" * 64
    apply_state = None if case["outcome"] == "not_in_conflict" else "conflict"
    job = {"jobId": job_id, "state": "SUCCEEDED", "applyState": apply_state,
           "repoRoot": str(repo.resolve()), "worktreePath": str(worktree.resolve()), "snapshot": snapshot,
           "taskId": "fixture-task"}
    server._write_job(job)
    sibling_id = "f" * 64
    if case.get("shared"):
        server._write_job({"jobId": sibling_id, "state": "SUCCEEDED", "repoRoot": str(repo.resolve()),
                           "worktreePath": str(worktree.resolve()), "snapshot": snapshot})
    side_effects = []
    monkeypatch.setattr(codex_exec_mcp, "_record_scope_conflict_outcome",
                        lambda record, action: side_effects.append({"call": "scope", "jobId": record["jobId"], "action": action}))
    monkeypatch.setattr(codex_exec_mcp, "_record_claude_decision",
                        lambda record, decision, kind, source, *a: side_effects.append({"call": "decision", "jobId": record["jobId"], "decision": decision, "kind": kind, "sourceJobId": source}))
    monkeypatch.setattr(codex_exec_mcp.Server, "_release_job_scope_lease",
                        staticmethod(lambda root, ident: side_effects.append({"call": "release", "jobId": ident}) or True))
    result = server._apply_job({"jobId": job_id, "resolved": True})["structuredContent"]
    record = json.loads((server.job_dir / f"{job_id}.json").read_text(encoding="utf-8"))
    record.pop("appliedAt", None)
    record["repoRoot"], record["worktreePath"] = "REPO", "WORKTREE"
    if isinstance(record.get("applyResult"), dict) and "worktreePath" in record["applyResult"]:
        record["applyResult"]["worktreePath"] = "WORKTREE"
    return {"name": case["name"], "setup": case, "jobId": job_id, "siblingJobId": sibling_id if case.get("shared") else None,
            "snapshot": snapshot, "result": result, "record": record,
            "checkout": (repo / "tracked.txt").read_text(encoding="utf-8"), "sideEffects": side_effects}


def test_python_oracle_jobs_resolve_fixture(tmp_path, monkeypatch):
    cases = [_case(tmp_path / "oracle", case, monkeypatch) for case in CASES]
    fixture = {"oracle": "real Server._apply_job(resolved=True) with temporary git repositories", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_RESOLVE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == fixture
