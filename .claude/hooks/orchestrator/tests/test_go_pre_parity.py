import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCHESTRATOR = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "pre.json"
DECISIONS_FIXTURE = ORCHESTRATOR / "tests" / "fixtures" / "go_policy" / "pre_decisions.json"
sys.path.insert(0, str(ORCHESTRATOR))
import codex
import code_map
import ledger
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


def _seed_decision_state(case, repo, runtime):
    if case["state"] == "thread":
        thread_id = "thread-p63"
        key = project_root.project_key(repo)
        record = ledger.build_session_record(
            "task-recorded", thread_id, "subtle", "gpt-6-luna", "high",
            phase="implementation", scope=["src/a.go"], label="saved label",
        )
        assert ledger.SessionLedger(runtime / "projects" / key).save(record)["status"] == ledger.OK
        codex._record_thread_root(runtime, thread_id, repo)
    elif case["state"] == "fallback":
        key = project_root.project_key(repo)
        path = runtime / "projects" / key / "active-coder.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({
            "active_coder": "fallback",
            "triggering_evidence": {"type": "RATE_LIMITED", "trigger_id": "rate-1",
                                    "switched_at": "99999999999", "retryAfterSeconds": 3600},
            "reset_cycle_marker": None, "last_probe_time": None,
        }), encoding="utf-8")


def _replace_fixture_roots(value, temporary_root):
    if isinstance(value, str):
        value = value.replace(str(temporary_root), "__TMP_ROOT__")
        if "__TMP_ROOT__" in value:
            value = value.replace("\\", "/")
        return re.sub(r"(?i)(projects[\\/])[0-9a-f]{16}([\\/])", r"\1__PROJECT_KEY__\2", value)
    if isinstance(value, dict):
        return {key: _replace_fixture_roots(item, temporary_root) for key, item in value.items()}
    if isinstance(value, list):
        return [_replace_fixture_roots(item, temporary_root) for item in value]
    return value


def test_python_oracle_matches_pre_decisions_p63(tmp_path, monkeypatch):
    fixture = json.loads(DECISIONS_FIXTURE.read_text(encoding="utf-8"))
    assert fixture["oracle"] == "Python codex.pre_tool_use"
    update = os.environ.get("UPDATE_P63_FIXTURE") == "1"
    for case in fixture["cases"]:
        case_root = tmp_path / case["name"]
        repo = _repo(case_root / "repo")
        (repo / "AGENTS.md").write_text(
            "Claude runs the full suite once before acceptance.\n", encoding="utf-8"
        )
        runtime, home, codex_home = case_root / "state", case_root / "home", case_root / "codex"
        home.mkdir()
        codex_home.mkdir()
        monkeypatch.setenv("PURSER_STATE_DIR", str(runtime))
        monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(case_root / "claude-config"))
        monkeypatch.setenv("CODEX_HOME", str(codex_home))
        monkeypatch.setenv("HOME", str(home))
        monkeypatch.setenv("USERPROFILE", str(home))
        monkeypatch.setenv("CLAUDE_PLUGIN_ROOT", str(ROOT))
        monkeypatch.setenv("GIT_CEILING_DIRECTORIES", str(case_root))
        if case["state"] == "fallback":
            config_dir = home / ".orchestrator"
            config_dir.mkdir()
            (config_dir / "config.toml").write_text(
                '[fallback]\nagent = "agy"\nmodel = "gemini-3.8-flash-high"\nfamily = "google"\n',
                encoding="utf-8",
            )
        _seed_decision_state(case, repo, runtime)
        event = {"hook_event_name": "PreToolUse", "tool_name": case["tool"],
                 "tool_input": {key: str(repo) if value == "__REPO__" else value
                                for key, value in case["input"].items()},
                 "session_id": "session-p63", "tool_use_id": "use-p63"}
        code_map.refresh(repo, state=runtime, budget_seconds=10.0)
        actual = codex.pre_tool_use(event, state_root=runtime, repo_root=repo)
        normalized = _replace_fixture_roots(actual, case_root)
        if update:
            case["expected"] = normalized
        else:
            assert case["expected"] is not None, f"fixture lacks oracle output: {case['name']}"
            assert normalized == case["expected"], case["name"]
    if update:
        DECISIONS_FIXTURE.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
