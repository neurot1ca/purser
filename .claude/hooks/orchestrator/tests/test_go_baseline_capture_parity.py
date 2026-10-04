import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_baseline" / "capture.json"
sys.path.insert(0, str(ORCHESTRATOR))
import ledger


def _repo(path, committed=True):
    path.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=path, check=True)
    if committed:
        (path / "tracked.txt").write_text("base\n", encoding="utf-8", newline="\n")
        subprocess.run(["git", "add", "tracked.txt"], cwd=path, check=True)
        env = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00+00:00",
               "GIT_COMMITTER_DATE": "2000-01-01T00:00:00+00:00"}
        subprocess.run(["git", "-c", "user.name=fixture", "-c",
                        "user.email=fixture@example.invalid", "commit", "-m", "fixture"],
                       cwd=path, env=env, check=True, capture_output=True)
        (path / "tracked.txt").write_text("work\n", encoding="utf-8", newline="\n")
        (path / "scope").mkdir()
        (path / "scope" / "new.txt").write_text("new\n", encoding="utf-8", newline="\n")
    return path


def _capture(case, root, monkeypatch):
    name = case["name"]
    if name == "invalid_scope":
        return ledger.capture_baseline(root, ["../escape"])
    if name == "git_failure":
        return ledger.capture_baseline(_repo(root / "empty", committed=False), None)
    if name == "manifest_deadline":
        ticks = iter(range(100))
        monkeypatch.setattr(ledger.time, "monotonic", lambda: next(ticks) / 10)
        return ledger.capture_baseline(root, ["scope"], manifest_budget_seconds=0.05)
    if name == "filesystem_failure":
        victim = root / "scope" / "new.txt"
        original = ledger._manifest_paths

        def fail_manifest(raw, deadline):
            paths = original(raw, deadline)
            if victim.relative_to(root).as_posix() in paths:
                victim.unlink()
            return paths

        monkeypatch.setattr(ledger, "_manifest_paths", fail_manifest)
        return ledger.capture_baseline(root, ["scope"])
    return ledger.capture_baseline(root, ["scope"] if name == "scoped" else None)


def test_python_oracle_matches_complete_baseline_golden(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python .claude/hooks/orchestrator/ledger.py capture_baseline"
    for case in fixture["cases"]:
        root = _repo(tmp_path / case["name"], committed=case["name"] != "git_failure")
        with monkeypatch.context() as patch:
            actual = _capture(case, root, patch)
        assert actual == case["expected"], case["name"]
        assert actual["code"] == case["code"], case["name"]
