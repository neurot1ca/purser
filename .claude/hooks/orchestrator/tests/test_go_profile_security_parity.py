import json
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parents[4]
FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_profile" / "security.json"
PROFILE = ROOT / ".claude" / "hooks" / "orchestrator" / "project_profile.py"

CASES = [
    {"tier": "user", "section": "roles", "key": "orchestrator", "value": "claude-code"},
    {"tier": "user", "section": "roles", "key": "coder", "value": "agy"},
    {"tier": "user", "section": "roles", "key": "reviewer", "value": "none"},
    {"tier": "user", "section": "roles", "key": "coder", "value": "unknown"},
    {"tier": "user", "section": "coding", "key": "coder_agent", "value": "opencode"},
    {"tier": "user", "section": "coding", "key": "coder_agent", "value": True},
    {"tier": "user", "section": "coding", "key": "family", "value": "Model-family_1"},
    {"tier": "user", "section": "coding", "key": "family", "value": "bad family"},
    {"tier": "user", "section": "coding", "key": "routes", "value": {"codex": "gpt-x", "agy": {"model": "gemini", "effort": "high"}}},
    {"tier": "user", "section": "coding", "key": "routes", "value": {"codex": {"model": "gpt-x", "effort": "high", "extra": "x"}}},
    {"tier": "user", "section": "reviewer", "key": "executable", "value": "/rooted"},
    {"tier": "user", "section": "reviewer", "key": "executable", "value": "reviewer.exe"},
    {"tier": "user", "section": "reviewer", "key": "selected", "value": {"agent": "agy", "model": "gemini"}},
    {"tier": "user", "section": "reviewer", "key": "selected", "value": {"agent": "agy", "model": "gemini", "extra": "x"}},
    {"tier": "user", "section": "reviewer", "key": "alternatives", "value": [{"agent": "agy", "model": "gemini"}]},
    {"tier": "user", "section": "reviewer", "key": "alternatives", "value": {"agent": "agy", "model": "gemini"}},
    {"tier": "user", "section": "trust", "key": "paths", "value": ["/rooted"]},
    {"tier": "user", "section": "trust", "key": "paths", "value": ["relative/trusted"]},
    {"tier": "user", "section": "paths", "key": "protected", "value": [".env", "src/private/**"]},
    {"tier": "user", "section": "paths", "key": "protected", "value": [".env", 4]},
    {"tier": "user", "section": "review", "key": "force_full", "value": False},
    {"tier": "user", "section": "review", "key": "skip_max_lines", "value": 0},
    {"tier": "user", "section": "review", "key": "skip_max_files", "value": True},
    {"tier": "user", "section": "review", "key": "skip_max_lines", "value": -1},
    {"tier": "user", "section": "review", "key": "skip_max_files", "value": 1.0},
    {"tier": "project", "section": "review", "key": "full_suite_required", "value": True},
    {"tier": "user", "section": "review", "key": "full_suite_required", "value": True, "operation": "key"},
    {"tier": "project", "section": "coding", "key": "routes", "value": {}, "operation": "key"},
    {"tier": "user", "section": "review", "key": "skip_max_files", "value": 2, "operation": "key"},
    {"tier": "user", "section": "coding", "key": "unknown", "value": {}, "operation": "key"},
]

ORACLE = r"""
import json, sys
from project_profile import _USER_SECURITY, _PROJECT_SECURITY, _valid_security_value
case = json.load(sys.stdin)
if case.get('operation') == 'key':
    allowed = (_USER_SECURITY if case['tier'] == 'user' else _PROJECT_SECURITY)
    valid = case['section'] in allowed and case['key'] in allowed[case['section']]
    reason = '' if valid else f"[{case['section']}] unknown security key {case['key']!r}"
else:
    valid, reason = _valid_security_value(case['section'], case['key'], case['value'])
print(json.dumps({'valid': valid, 'reason': reason}))
"""


def _oracle(case):
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        [sys.executable, "-B", "-c", ORACLE], input=json.dumps(case), text=True,
        capture_output=True, cwd=PROFILE.parent, env=env, check=True,
    )
    return json.loads(result.stdout)


def test_python_matches_security_golden_fixture():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    expected = [{"input": case, **_oracle(case)} for case in CASES]
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
