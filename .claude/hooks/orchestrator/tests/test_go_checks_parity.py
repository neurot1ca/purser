import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[4]
ORCH = ROOT / ".claude" / "hooks" / "orchestrator"
FIXTURE = ORCH / "tests" / "fixtures" / "go_policy" / "checks.json"
sys.path.insert(0, str(ORCH))
import checks
from project_profile import Check


def test_checks_match_go_fixture():
    definitions = [
        {"name": "always", "argv": ["__PYTHON__", "-c", "print('ok')"], "timeout_seconds": 2},
        {"name": "src", "argv": ["__PYTHON__", "-c", "print('bad'); raise SystemExit(7)"], "when": ["src/**"], "timeout_seconds": 2},
        {"name": "basename", "argv": ["__PYTHON__", "-c", "print('ok')"], "when": ["*.go"], "timeout_seconds": 2},
        {"name": "docs", "argv": ["__PYTHON__", "-c", "print('ok')"], "when": ["docs/*.md"], "timeout_seconds": 2},
        {"name": "empty", "argv": ["__PYTHON__", "-c", "print('ok')"], "when": [], "timeout_seconds": 2},
    ]
    changed = ["./src/main.go", "docs/guide.md", "elsewhere/readme.txt"]
    selected = [c.name for c in checks.select_checks(tuple(
        Check(d["name"], (sys.executable, *d["argv"][1:]), tuple(d["when"]) if "when" in d else None,
              d["timeout_seconds"]) for d in definitions), changed)]
    mutation_cases = _mutation_oracle_cases()
    fixture = {"oracle": "Python checks.select_checks and run_checks results", "definitions": definitions,
               "changed_paths": changed, "selected": selected, "cases": mutation_cases}
    if os.environ.get("GO_CHECKS_WRITE_FIXTURE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
    expected = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert expected == fixture
    cache = ROOT / ".tmp" / "go-cache"
    cache.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "GOCACHE": str(cache), "TMP": str(ROOT / ".tmp"),
           "TEMP": str(ROOT / ".tmp"), "TMPDIR": str(ROOT / ".tmp")}
    result = subprocess.run(["go", "test", "-run", "^TestChecksParityFixture$", "-count=1", "./internal/review"],
                            cwd=ROOT / "go", env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr


def _mutation_oracle_cases():
    cases = [
        {"name": "tracked-content", "tracked_files": {"tracked.txt": "committed\n"},
         "pre_files": {"tracked.txt": "dirty before\n"},
         "script": "from pathlib import Path; Path('tracked.txt').write_text('dirty after\\n')"},
        {"name": "untracked-create", "tracked_files": {}, "pre_files": {},
         "script": "from pathlib import Path; Path('created.txt').write_text('new\\n')"},
        {"name": "mtime-only", "tracked_files": {"tracked.txt": "same\n"}, "pre_files": {},
         "script": "import os; os.utime('tracked.txt', None)"},
        {"name": "tracked-delete", "tracked_files": {"deleted.txt": "remove me\n"}, "pre_files": {},
         "script": "from pathlib import Path; Path('deleted.txt').unlink()"},
        {"name": "no-mutation", "tracked_files": {}, "pre_files": {}, "script": "pass"},
        {"name": "status-failure", "tracked_files": {}, "pre_files": {}, "script": "pass", "no_git": True},
    ]
    temp_root = ROOT / ".tmp" / "checks-oracle"
    temp_root.mkdir(parents=True, exist_ok=True)
    for case in cases:
        repo = temp_root / case["name"]
        repo.mkdir(parents=True, exist_ok=True)
        if not case.get("no_git"):
            subprocess.run(["git", "-C", str(repo), "init", "-q"], cwd=ROOT, check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.email", "parity@example.invalid"], cwd=ROOT, check=True)
            subprocess.run(["git", "-C", str(repo), "config", "user.name", "Parity"], cwd=ROOT, check=True)
            if subprocess.run(["git", "-C", str(repo), "rev-parse", "--verify", "HEAD"], cwd=ROOT,
                              capture_output=True).returncode == 0:
                subprocess.run(["git", "-C", str(repo), "reset", "--hard"], cwd=ROOT, check=True, capture_output=True)
            subprocess.run(["git", "-C", str(repo), "clean", "-fdx"], cwd=ROOT, check=True, capture_output=True)
            for name, content in case["tracked_files"].items():
                (repo / name).write_text(content, encoding="utf-8")
            if case["tracked_files"]:
                subprocess.run(["git", "-C", str(repo), "add", "."], cwd=ROOT, check=True)
                subprocess.run(["git", "-C", str(repo), "commit", "--allow-empty", "-qm", "baseline"], cwd=ROOT, check=True)
            for name, content in case["pre_files"].items():
                (repo / name).write_text(content, encoding="utf-8")
        check = Check(case["name"], (sys.executable, "-c", case["script"]), None, 5)
        old_ceiling = os.environ.get("GIT_CEILING_DIRECTORIES")
        if case.get("no_git"):
            os.environ["GIT_CEILING_DIRECTORIES"] = str(repo.parent)
        try:
            result = checks.run_checks(repo, (check,), deadline_seconds=10)
        finally:
            if old_ceiling is None:
                os.environ.pop("GIT_CEILING_DIRECTORIES", None)
            else:
                os.environ["GIT_CEILING_DIRECTORIES"] = old_ceiling
        for check_result in result["results"]:
            check_result["duration_ms"] = 0
        case["argv"] = ["__PYTHON__", "-c", case.pop("script")]
        case["expected"] = result
    return cases
