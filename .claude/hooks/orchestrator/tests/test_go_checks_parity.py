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
        {"name": "always", "argv": [sys.executable, "-c", "print('ok')"], "timeout_seconds": 2},
        {"name": "src", "argv": [sys.executable, "-c", "print('bad'); raise SystemExit(7)"], "when": ["src/**"], "timeout_seconds": 2},
        {"name": "basename", "argv": [sys.executable, "-c", "print('ok')"], "when": ["*.go"], "timeout_seconds": 2},
        {"name": "docs", "argv": [sys.executable, "-c", "print('ok')"], "when": ["docs/*.md"], "timeout_seconds": 2},
        {"name": "empty", "argv": [sys.executable, "-c", "print('ok')"], "when": [], "timeout_seconds": 2},
    ]
    checks_to_run = tuple(Check(d["name"], tuple(d["argv"]), tuple(d["when"]) if "when" in d else None, d["timeout_seconds"]) for d in definitions)
    changed = ["./src/main.go", "docs/guide.md", "elsewhere/readme.txt"]
    selected = [c.name for c in checks.select_checks(checks_to_run, changed)]
    run = checks.run_checks(ROOT, checks_to_run, deadline_seconds=10)["results"]
    for result in run:
        result["duration_ms"] = 0
    fixture = {"oracle": "Python checks.select_checks and run_checks results", "definitions": definitions,
               "changed_paths": changed, "selected": selected, "results": run}
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
