import json
import os
import subprocess
from pathlib import Path

import pytest

import ledger

FIXTURE = Path(__file__).parent / "fixtures" / "go_baseline" / "job_worktree.json"

def _git(root, *args):
    return subprocess.run(["git", *args], cwd=root, check=True, capture_output=True).stdout

def _files(root):
    result = {}
    for path in sorted(root.rglob("*")):
        if ".git" in path.parts or path.is_dir():
            continue
        relative = path.relative_to(root).as_posix()
        result[relative] = os.readlink(path) if path.is_symlink() else path.read_text()
    return result

@pytest.mark.parametrize("name,trusted,copy_path", [
    ("trusted_copy", True, "ignored.dat"),
    ("untrusted_ignores_copy", False, "ignored.dat"),
    ("copy_failure_cleans_up", True, "../escape"),
])
def test_create_job_worktree_matches_go_fixture(tmp_path, monkeypatch, name, trusted, copy_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    _git(repo, "init")
    _git(repo, "config", "user.name", "fixture")
    _git(repo, "config", "user.email", "fixture@example.invalid")
    (repo / "tracked.txt").write_text("base\n")
    _git(repo, "add", "tracked.txt")
    _git(repo, "commit", "-m", "base")
    (repo / "tracked.txt").write_text("dirty\n")
    (repo / "new.txt").write_text("untracked\n")
    (repo / ".gitignore").write_text("ignored.dat\n")
    (repo / "ignored.dat").write_text("ignored\n")
    profile_dir = repo / ".orchestrator"
    profile_dir.mkdir()
    (profile_dir / "profile.toml").write_text(f'schema_version = 1\n[worktree]\ncopy = ["{copy_path}"]\n')
    home = tmp_path / "home"
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir(parents=True)
    paths = f'["{repo.as_posix()}"]' if trusted else "[]"
    config.write_text(f"schema_version = 1\n[trust]\npaths = {paths}\n")
    monkeypatch.setattr(Path, "home", lambda: home)

    try:
        result = ledger.create_job_worktree(repo, "job-worktree-fixture", tmp_path / "jobs")
        recorded = {"head": "@HEAD@", "snapshot": "@SNAPSHOT@", "files": _files(Path(result["path"])), "error": False, "cleaned": False,
                    "index_matches_snapshot": _git(Path(result["path"]), "write-tree").decode().strip() == result["snapshot"]}
    except Exception:
        worktrees = _git(repo, "worktree", "list", "--porcelain").decode().splitlines()
        recorded = {"head": "", "snapshot": "", "files": {}, "error": True,
                    "cleaned": not (tmp_path / "jobs" / "job-worktree").exists()
                    and sum(line.startswith("worktree ") for line in worktrees) == 1,
                    "index_matches_snapshot": False}

    case = {"name": name, "trusted": trusted, "copy": copy_path, "want": recorded}
    if os.environ.get("PURSER_REGEN_GO_JOB_WORKTREE") == "1":
        fixture = json.loads(FIXTURE.read_text()) if FIXTURE.exists() else {"cases": []}
        fixture["cases"] = [item for item in fixture["cases"] if item["name"] != name]
        fixture["cases"].append(case)
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n")
    else:
        fixture = json.loads(FIXTURE.read_text())
        expected = next(item for item in fixture["cases"] if item["name"] == name)
        assert {key: expected[key] for key in ("name", "trusted", "copy")} == {key: case[key] for key in ("name", "trusted", "copy")}
        assert case["want"] == expected["want"]
    assert recorded["error"] == (name == "copy_failure_cleans_up")
    assert recorded["cleaned"] == (name == "copy_failure_cleans_up")
