import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[4]
FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_profile" / "entries.json"
PROFILE = ROOT / ".claude" / "hooks" / "orchestrator" / "project_profile.py"

CASES = [
    {"operation": "checks", "raw": []},
    {"operation": "checks", "raw": [{"name": "lint", "argv": ["go", "vet", "./..."]}]},
    {"operation": "checks", "raw": [{"name": "second", "argv": ["b"], "when": ["./src/**"], "timeout_seconds": 12}, {"name": "first", "argv": ["a"], "when": None}]},
    {"operation": "checks", "raw": None},
    {"operation": "checks", "raw": [{"name": "ok", "argv": ["x"], "extra": True}]},
    {"operation": "checks", "raw": [{"name": "Bad", "argv": ["x"]}]},
    {"operation": "checks", "raw": [{"name": "same", "argv": ["x"]}, {"name": "same", "argv": ["y"]}]},
    {"operation": "checks", "raw": [{"name": "ok", "argv": []}]},
    {"operation": "checks", "raw": [{"name": "ok", "argv": ["x"], "when": ["../private"]}]},
    {"operation": "checks", "raw": [{"name": "ok", "argv": ["x"], "timeout_seconds": True}]},
    {"operation": "checks", "raw": [{"name": "ok", "argv": ["x"], "timeout_seconds": 301}]},
    {"operation": "authority", "tier": "project", "raw": None},
    {"operation": "authority", "tier": "project", "raw": ["bad", {"path": "x.md", "headings": ["One"]}, {"path": "./docs/../bad.md", "headings": ["Bad"]}, {"path": "C:\\repo\\RULES.md", "headings": ["Drive"]}, {"path": "rules.md", "headings": []}, {"path": "rules.md", "headings": ["One"], "when_changed": 4}, {"path": "./docs\\RULES.md", "headings": ["Authority", "Scope"], "when_changed": "src/**"}, {"path": "ok.md", "headings": ["One"], "extra": 1}]},
    {"operation": "path", "value": "./docs\\RULES.md"},
    {"operation": "path", "value": "../outside"},
    {"operation": "path", "value": "C:relative\\file"},
    {"operation": "path", "value": "\\\\server\\share\\file"},
    {"operation": "authority_defaults"},
]

ORACLE = r"""
import json, sys
from dataclasses import asdict
from project_profile import _DEFAULT_AUTHORITY_SOURCES, _parse_checks, _parse_authority_sources, _authority_path
case = json.load(sys.stdin)
if case['operation'] == 'checks':
    values, error = _parse_checks(case['raw'])
    result = {'checks': [asdict(item) for item in values], 'error': error}
elif case['operation'] == 'authority':
    values, warnings = _parse_authority_sources(case['tier'], case['raw'])
    result = {'sources': [asdict(item) for item in values], 'warnings': list(warnings)}
elif case['operation'] == 'authority_defaults':
    result = {'sources': [asdict(item) for item in _DEFAULT_AUTHORITY_SOURCES]}
else:
    value = _authority_path(case['value'])
    result = {'path': value, 'valid': value is not None}
print(json.dumps(result))
"""


def _oracle(case):
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        [sys.executable, "-B", "-c", ORACLE], input=json.dumps(case), text=True,
        capture_output=True, cwd=PROFILE.parent, env=env, check=True,
    )
    return json.loads(result.stdout)


def test_python_matches_entries_golden_fixture():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    expected = [{"input": case, "output": _oracle(case)} for case in CASES]
    assert fixture == expected


def test_go_profile_package():
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-cache")
    Path(env["GOCACHE"]).mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["go", "test", "./internal/profile/..."], cwd=ROOT / "go",
        text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
