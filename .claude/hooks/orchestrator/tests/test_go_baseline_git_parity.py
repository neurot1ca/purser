import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "git.json"
GO_ENV = {**os.environ, "GOCACHE": str(ROOT / ".tmp" / "go-cache")}
sys.path.insert(0, str(ORCHESTRATOR))
import ledger


def _git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.DEVNULL)


def _repository(root):
    _git(root, "init", "-q")
    (root / "dir").mkdir()
    (root / "dir" / "file[1].txt").write_text("literal")
    (root / "dir" / "file1.txt").write_text("wildcard match")
    (root / "old.txt").write_text("rename me\n")
    _git(root, "add", "--all")
    _git(root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
    (root / "old.txt").rename(root / "renamed.txt")
    (root / "dir" / "file[1].txt").write_text("changed")
    _git(root, "add", "-A")


def _python_result(root):
    scope = ledger._normalize_scope([" dir\\file[1].txt ", "old.txt", "renamed.txt", "dir/file[1].txt"])
    return {
        "scope": scope,
        "status": ledger._status_entries(root, 2),
        "index": ledger._index_entries(root, scope, 2),
    }


def _go_live_outcome(mode, root, path=None):
    env = GO_ENV.copy()
    env.update(PURSER_GIT_LIVE_OUTCOME=mode, PURSER_GIT_LIVE_ROOT=str(root))
    if path is not None:
        env["PURSER_GIT_LIVE_PATH"] = str(path)
    if mode == "failed":
        env["PURSER_GIT_LIVE_CEILING"] = str(root)
    completed = subprocess.run(
        ["go", "test", "-run", "^TestGitLiveOutcome$", "-count=1", "-v", "./internal/ledger"],
        cwd=ROOT / "go", env=env, capture_output=True, text=True, check=True,
    )
    return json.loads(next(line for line in completed.stdout.splitlines() if line.startswith('{"code"')))


def test_python_fixture_and_go_parity(tmp_path):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    repo = tmp_path / "repo"
    repo.mkdir()
    _repository(repo)
    assert _python_result(repo) == {key: fixture[key] for key in ("scope", "status", "index")}
    subprocess.run(["go", "test", "./internal/ledger/..."], cwd=ROOT / "go", env=GO_ENV, check=True)


def test_malformed_fake_git_output_matches_go(tmp_path, monkeypatch):
    real_run = subprocess.run

    def fake_git(*args, **kwargs):
        return subprocess.CompletedProcess(args[0], 0, stdout=b"bad\0", stderr=b"")

    monkeypatch.setattr(ledger.subprocess, "run", fake_git)
    with pytest.raises(ledger._InspectionError) as caught:
        ledger._status_entries(tmp_path, 1)
    python = {"code": caught.value.code, "message": caught.value.message}
    completed = real_run(
        ["go", "test", "-run", "^TestGitMalformedOutput$", "-count=1", "-v", "./internal/ledger"],
        cwd=ROOT / "go", env=GO_ENV, capture_output=True, text=True, check=True,
    )
    go = json.loads(next(line for line in completed.stdout.splitlines() if line.startswith('{"code"')))
    assert go == python


@pytest.mark.parametrize("mode", ["timeout", "unavailable", "failed"])
def test_live_git_failures_match_python_and_go(tmp_path, mode, monkeypatch):
    real_run = subprocess.run
    repo = tmp_path / "repo"
    repo.mkdir()
    _repository(repo)
    if mode == "failed":
        repo = tmp_path / "outside"
        repo.mkdir()

    env = {"PATH": str(tmp_path / "empty-bin")} if mode == "unavailable" else None
    if mode == "failed":
        env = {"GIT_CEILING_DIRECTORIES": str(repo)}
    if env:
        (tmp_path / "empty-bin").mkdir()
    if mode == "unavailable":
        def unavailable(*args, **kwargs):
            kwargs["executable"] = str(tmp_path / "empty-bin" / "git.exe")
            return real_run(*args, **kwargs)

        monkeypatch.setattr(ledger.subprocess, "run", unavailable)
    timeout = 0 if mode == "timeout" else 2
    try:
        ledger._run_git(repo, ["status"], timeout, "reading repository status", env=env)
        python = {"code": "OK", "message": ""}
    except ledger._InspectionError as error:
        python = {"code": error.code, "message": error.message}

    # Windows searches system paths even when PATH is restricted, so force the
    # missing-executable OSError at Python's subprocess boundary above.
    if mode == "unavailable":
        monkeypatch.setattr(ledger.subprocess, "run", real_run)
    go = _go_live_outcome(mode, repo, Path(env["PATH"]) if env and "PATH" in env else None)
    assert go == python
