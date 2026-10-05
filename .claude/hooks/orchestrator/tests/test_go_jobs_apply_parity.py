import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / ".claude" / "hooks" / "orchestrator"))
import ledger

JOBS_FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_jobs_apply" / "oracle.json"
JOB_CASES = [
    {"name": "clean_apply", "repo": "base\n", "worktree": "worker\n", "state": "SUCCEEDED"},
    {"name": "cached_repeat", "repo": "base\n", "worktree": "worker\n", "state": "SUCCEEDED", "repeat": True},
    {"name": "already_present", "repo": "worker\n", "worktree": "worker\n", "state": "SUCCEEDED"},
    {"name": "stale_review", "repo": "intervening\n", "worktree": "worker\n", "state": "SUCCEEDED", "review": True},
    {"name": "conflict", "repo": "other\n", "worktree": "worker\n", "state": "SUCCEEDED"},
    {"name": "not_succeeded", "repo": "base\n", "worktree": "worker\n", "state": "RUNNING"},
]


def _git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _case(root, case, monkeypatch):
    import codex_exec_mcp

    name = case["name"]
    repo = root / name / "repo"
    state = root / name / "state"
    repo.mkdir(parents=True)
    _git(repo, "init", "-q")
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    _git(repo, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base"], cwd=repo, env=env, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    worktree = repo.parent / "worktree"
    subprocess.run(["git", "worktree", "add", "--detach", str(worktree), "HEAD"], cwd=repo, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    snapshot = ledger.checkout_snapshot(worktree)
    reviewed = ledger.checkout_snapshot(repo) if case.get("review") else None
    (worktree / "tracked.txt").write_text(case["worktree"], encoding="utf-8")
    (repo / "tracked.txt").write_text(case["repo"], encoding="utf-8")
    monkeypatch.setenv("PURSER_STATE_DIR", str(state))
    server = codex_exec_mcp.Server(agent="codex")
    job_id = hashlib.sha256(name.encode()).hexdigest()
    job = {"jobId": job_id, "state": case["state"], "repoRoot": str(repo.resolve()),
           "worktreePath": str(worktree.resolve()), "snapshot": snapshot}
    if reviewed:
        job.update(reviewedSnapshot=reviewed, snapshotRevision=2)
        index = state / "async_jobs" / f"{job_id}.json"
        index.parent.mkdir(parents=True)
        index.write_text('{"stage":"APPLY"}', encoding="utf-8")
    server._write_job(job)
    result = server._apply_job({"jobId": job_id})
    if case.get("repeat"):
        result = server._apply_job({"jobId": job_id})
    result_value = result["structuredContent"]
    if "worktreePath" in result_value:
        result_value["worktreePath"] = "WORKTREE"
    record = json.loads((server.job_dir / f"{job_id}.json").read_text(encoding="utf-8"))
    record.pop("appliedAt", None)
    record["repoRoot"] = "REPO"
    record["worktreePath"] = "WORKTREE"
    if "applyResult" in record and "worktreePath" in record["applyResult"]:
        record["applyResult"]["worktreePath"] = "WORKTREE"
    return {"name": name, "setup": case, "snapshot": snapshot, "reviewedSnapshot": reviewed,
            "result": result_value, "record": record,
            "checkout": (repo / "tracked.txt").read_text(encoding="utf-8"),
            "index": json.loads((state / "async_jobs" / f"{job_id}.json").read_text(encoding="utf-8")) if case.get("review") else None}


def test_python_oracle_jobs_apply_fixture(tmp_path, monkeypatch):
    cases = [_case(tmp_path / "oracle", case, monkeypatch) for case in JOB_CASES]
    fixture = {"oracle": "real Server._apply_job with temporary git repositories and job worktrees", "cases": cases}
    if os.environ.get("PURSER_REGEN_GO_JOBS_APPLY") == "1":
        JOBS_FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        JOBS_FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(JOBS_FIXTURE.read_text(encoding="utf-8")) == fixture
