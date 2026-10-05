import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(ROOT / ".claude" / "hooks" / "orchestrator"))
import ledger

FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_ledger_apply" / "oracle.json"
PATCH = "diff --git a/tracked.txt b/tracked.txt\n--- a/tracked.txt\n+++ b/tracked.txt\n@@ -1 +1 @@\n-base\n+worker\n"
CASES = [
    {"name": "clean_apply", "checkout": "base\n", "patch": PATCH, "paths": ["tracked.txt"]},
    {"name": "already_present", "checkout": "worker\n", "patch": PATCH, "paths": ["tracked.txt"]},
    {"name": "drifted_path", "checkout": "checkout\n", "patch": "", "paths": ["tracked.txt"]},
    {"name": "conflict", "checkout": "checkout\n", "patch": PATCH, "paths": ["tracked.txt"]},
    {"name": "unsafe_path", "checkout": "base\n", "patch": PATCH.replace("tracked.txt", "../escape.txt"), "paths": []},
]


def _git(root, *args):
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def _case(root, case):
    repo = root / case["name"]
    repo.mkdir()
    _git(repo, "init", "-q")
    (repo / "tracked.txt").write_text("base\n", encoding="utf-8")
    _git(repo, "add", "--all")
    env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
    subprocess.run(["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base"], cwd=repo, env=env, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    snapshot = ledger.checkout_snapshot(repo)
    if case["name"] != "unsafe_path":
        (repo / "tracked.txt").write_text(case["checkout"], encoding="utf-8")
    drift = ledger.snapshot_drift_paths(repo, snapshot, case["paths"])
    outcome = ledger.apply_job_patch(repo, case["patch"])
    return {**case, "drift": drift, "outcome": outcome}


def test_python_oracle_apply_fixture(tmp_path):
    FIXTURE.parent.mkdir(parents=True, exist_ok=True)
    result = {"oracle": "Python ledger snapshot_drift_paths/apply_job_patch", "cases": [_case(tmp_path, case) for case in CASES]}
    FIXTURE.write_text(json.dumps(result, ensure_ascii=True, indent=2) + "\n", encoding="utf-8")
