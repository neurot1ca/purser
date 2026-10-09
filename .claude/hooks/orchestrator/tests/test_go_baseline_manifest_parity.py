import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "manifest.json"
GO_ENV = {**os.environ, "GOCACHE": str(ROOT / ".tmp" / "go-cache")}
sys.path.insert(0, str(ORCHESTRATOR))
import ledger


def _git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.DEVNULL)


def _repository(root):
    subprocess.run(["git", "init", "-q"], cwd=root, check=True)
    (root / "scope" / "__pycache__").mkdir(parents=True)
    files = {
        ".gitignore": "scope/ignored.txt\n",
        "scope/before.txt": "rename me\n",
    }
    for name, content in files.items():
        path = root / name
        path.write_bytes(content.encode())
        path.chmod(0o666)
    _git(root, "add", "--all")
    _git(root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
    (root / "scope" / "before.txt").rename(root / "scope" / "after.txt")
    for name, content in {
        "scope/untracked.txt": "one\ntwo",
        "scope/ignored.txt": "ignored\n",
        "scope/cache.pyc": "bytecode",
        "scope/__pycache__/module.pyc": "bytecode",
    }.items():
        path = root / name
        path.write_bytes(content.encode())
        path.chmod(0o666)
    _git(root, "add", "-A", "--", "scope/before.txt", "scope/after.txt")


def test_python_and_go_manifest_fixture(tmp_path):
    repo = tmp_path / "repo"
    repo.mkdir()
    _repository(repo)
    statuses = ledger._status_entries(repo, 2)
    actual = {
        "status": statuses,
        "tracked_snapshots": ledger._tracked_snapshots(repo, statuses, 2),
        "manifest": ledger._scoped_manifest(repo, ["scope"], 2, 3),
    }
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert actual == {key: fixture[key] for key in actual}

    before = ledger._tracked_snapshots(repo, statuses, 2)["scope/untracked.txt"]["worktree"]["mode"]
    (repo / "scope" / "untracked.txt").chmod(0o444 if os.name == "nt" else 0o644)
    changed_status = ledger._status_entries(repo, 2)
    after = ledger._tracked_snapshots(repo, changed_status, 2)["scope/untracked.txt"]["worktree"]["mode"]
    assert after != before, "permission change disappeared from the baseline snapshot"

    subprocess.run(
        ["go", "test", "./internal/ledger", "-run", "^(TestScopedManifestPythonFixture|TestScopedManifestBudgetFailures|TestScopedManifestFilesystemFailureIsNotPartialEvidence|TestFilesystemFingerprintKinds)$", "-count=1"],
        cwd=ROOT / "go",
        env=GO_ENV,
        check=True,
    )
