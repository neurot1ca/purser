import json
import os
from pathlib import Path
import subprocess
import sys
import tomllib


ROOT = Path(__file__).resolve().parents[4]
FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_profile" / "tier.json"
PROFILE = ROOT / ".claude" / "hooks" / "orchestrator" / "project_profile.py"
CASES = [
    {"tier": "user", "toml": 'schema_version = 1\n[telemetry]\nvault_root = "/vault"\n'},
    {"tier": "project", "toml": '''schema_version = 1
[unknown]
"quoted.key" = "quoted"
dotted.key = "dotted"
multiline = """first line
second line"""
mixed = [1, 1.5, true]
inline = { integer = 2, float = 2.5, boolean = false }
[[unknown.rows]]
name = "row"
[project]
id = 7
[authority]
sources = ["bad"]
[worktree]
setup_timeout_seconds = 3601
'''},
    {"tier": "user", "toml": 'schema_version = 1\n[review]\nskip_max_lines = -1\n'},
    {"tier": "user", "toml": 'schema_version = 2\n'},
    {"tier": "user", "toml": 'schema_version = 1\nchecks = []\n'},
    {"tier": "project", "toml": 'schema_version = 1\n[[checks]]\nname = "lint"\nargv = ["go", "vet", "./..."]\n[worktree]\ncopy = [".env", "config.toml"]\nsetup = "go test ./..."\nsetup_timeout_seconds = 30\n'},
]

ORACLE = r"""
import json, sys
from dataclasses import asdict
from pathlib import Path
import tomllib
from project_profile import _load_tier, _USER_SECURITY, _USER_NON_SECURITY, _PROJECT_SECURITY, _PROJECT_NON_SECURITY
case = json.load(sys.stdin)
data = tomllib.loads(case['toml'])
security, non_security = (_USER_SECURITY, _USER_NON_SECURITY) if case['tier'] == 'user' else (_PROJECT_SECURITY, _PROJECT_NON_SECURITY)
path = Path.cwd().parents[2] / '.tmp' / 'm3-p28' / ('oracle-' + str(__import__('os').getpid()) + '.toml')
path.write_text(case['toml'], encoding='utf-8')
try:
    result = _load_tier(path, case['tier'], security, non_security)
finally:
    path.unlink(missing_ok=True)
print(json.dumps({'data': data, 'output': asdict(result)}))
"""


def _oracle(case):
    env = os.environ.copy()
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(
        [sys.executable, "-B", "-c", ORACLE], input=json.dumps(case), text=True,
        capture_output=True, cwd=PROFILE.parent, env=env, check=True,
    )
    return json.loads(result.stdout)


def test_python_matches_tier_golden_fixture():
    (ROOT / ".tmp" / "m3-p28").mkdir(parents=True, exist_ok=True)
    golden = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert len(CASES) == len(golden)
    for index, case in enumerate(CASES):
        oracle = _oracle(case)
        assert oracle["output"] == golden[index]["output"]
        if index == 1:
            unknown = oracle["data"]["unknown"]
            assert unknown["quoted.key"] == "quoted"
            assert unknown["dotted"]["key"] == "dotted"
            assert unknown["multiline"] == "first line\nsecond line"
            assert unknown["mixed"] == [1, 1.5, True]
            assert unknown["inline"] == {"integer": 2, "float": 2.5, "boolean": False}
            assert unknown["rows"] == [{"name": "row"}]


def test_go_profile_package():
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-cache")
    Path(env["GOCACHE"]).mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["go", "test", "./internal/profile/..."], cwd=ROOT / "go",
        text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
