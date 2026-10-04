import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "pre.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex
import project_root


def _repo(path):
    path.mkdir(parents=True)
    subprocess.run(["git", "init", "-q"], cwd=path, check=True)
    return path.resolve()


def _guard_index(case, runtime):
    path = codex._thread_index_path(runtime, case["thread_id"])
    if case["name"] == "missing":
        assert path is not None and not path.exists()
    elif case["name"] == "invalid-id":
        assert path is None
    elif case["name"] == "malformed":
        assert case["index"] == "{" and path is not None
    elif case["name"] == "stale-thread":
        assert case["index"]["thread_id"] != case["thread_id"]
    elif case["name"] == "relative-root":
        assert not Path(case["index"]["project_root"]).is_absolute()
    elif case["name"] == "non-git-root":
        assert not (Path(case["index"]["project_root"]) / ".git").exists()


def test_python_oracle_matches_pre_fixture(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python project_root and codex indexed reply helpers"
    roots = {name: _repo(tmp_path / name) for name in ("explicit_repo", "input_repo", "event_repo")}
    roots["missing_repo"] = tmp_path / "missing_repo"
    roots["outside"] = tmp_path / "outside"
    roots["outside"].mkdir()
    roots["invalid"] = "\0"
    monkeypatch.setenv("GIT_CEILING_DIRECTORIES", str(tmp_path))
    for case in fixture["root_cases"]:
        event = {}
        if "input" in case:
            event["tool_input"] = {"cwd": str(roots[case["input"]])}
        if "event" in case:
            event["cwd"] = str(roots[case["event"]])
        explicit = str(roots[case["explicit"]]) if "explicit" in case else None
        actual, reason = project_root.resolve_project_root(event, explicit)
        expected = str(roots[case["expected"]]) if case["expected"] else None
        assert (str(actual) if actual else None, reason) == (expected, case["reason"]), case["name"]
    for case in fixture["project_keys"]:
        value = Path(case["input"]) if case["input"] else None
        assert project_root.project_key(value) == case["expected"]
    for case in fixture["thread_cases"]:
        path = codex._thread_index_path(tmp_path, case["thread_id"])
        assert (path.name if path else None) == case["expected"]
    repo = _repo(tmp_path / "indexed_repo")
    outside = tmp_path / "outside"
    runtime = tmp_path / "runtime"
    for case in fixture["index_cases"]:
        _guard_index(case, runtime)
        index = case["index"]
        if index != "missing":
            path = codex._thread_index_path(runtime, case["thread_id"])
            path.parent.mkdir(parents=True, exist_ok=True)
            content = index if isinstance(index, str) else json.dumps({
                key: str(repo) if value == "__REPO__" else str(outside) if value == "__OUTSIDE__" else value
                for key, value in index.items()
            })
            path.write_text(content, encoding="utf-8")
        actual, reason = codex._indexed_reply_root(runtime, case["thread_id"])
        expected = str(repo) if case["expected"] == "__REPO__" else None
        assert (str(actual) if actual else None, reason) == (expected, case["reason"]), case["name"]


def test_go_pre_fixture():
    scratch = ROOT / ".tmp"
    scratch.mkdir(parents=True, exist_ok=True)
    cache = scratch / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(scratch), "TEMP": str(scratch),
           "TMPDIR": str(scratch), "PYTHONDONTWRITEBYTECODE": "1"}
    subprocess.run(["go", "test", "-run", "^TestPreParityFixture$", "-count=1", "./internal/enforce"],
                   cwd=ROOT / "go", env=env, check=True)
