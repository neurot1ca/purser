import json
import os
from pathlib import Path
import subprocess
import sys
from dataclasses import asdict

ROOT = Path(__file__).resolve().parents[4]
FIXTURE = ROOT / ".claude" / "hooks" / "orchestrator" / "tests" / "fixtures" / "go_profile" / "resolve.json"
sys.path.insert(0, str(ROOT / ".claude" / "hooks" / "orchestrator"))
import project_profile


PROFILES = [
    {
        "user": project_profile._Tier("ok", {
            "review.skip_max_lines": 40, "review.skip_max_files": 3,
            "roles.reviewer": "none",
            "reviewer.selected": {"agent": "opencode", "model": "review-1"},
            "reviewer.alternatives": [
                {"agent": "codex", "model": "review-2"},
                {"agent": "agy", "model": "review-3"},
            ],
            "paths.protected": ["user-secret"], "trust.paths": ["$ROOT"],
            "telemetry.vault_root": "/vault",
        }, ("user warning",)),
        "project": project_profile._Tier("ok", {
            "project.id": "demo", "paths.protected": ["project-secret"],
            "review.skip_max_lines": 20, "review.skip_max_files": 1,
            "review.force_full": False, "review.full_suite_required": True,
            "checks": (project_profile.Check("lint", ("go", "test"), None, 30),),
            "worktree.copy": (".env",), "worktree.setup": "setup",
            "worktree.setup_timeout_seconds": 90,
        }, ("project warning",)),
    },
    {
        "user": project_profile._Tier("ok", {
            "review.skip_max_lines": 40, "review.skip_max_files": 3,
            "trust.paths": ["$ROOT/child"],
        }, ()),
        "project": project_profile._Tier("ok", {
            "review.skip_max_lines": 41, "review.skip_max_files": 2,
            "paths.protected": ["project-secret"], "checks": (),
            "worktree.setup": "setup",
        }, ()),
    },
    {
        "user": project_profile._Tier("missing", {}, ()),
        "project": project_profile._Tier("rejected", {"review.skip_max_lines": 1}, ("project tier rejected: bad",)),
    },
]

PROTECTED = [
    ("src/deep/file.go", ["src/**/file.go"]),
    ("src/deep/file.go", ["src/*.go"]),
    ("src/file.go", ["src/f[ai]le.go"]),
    ("src/FILE.go", ["src/file.go"]),
    ("/src/file.go", ["src/file.go"]),
    ("src/file.go", ["/src/file.go"]),
    (".\\SRC\\deep\\file.go", ["src/**"]),
    (".orchestrator", [".orchestrator/**"]),
    ("foobar/file.go", ["foo/**"]),
    ("nested/SECRET.PEM", ["*.pem"]),
    ("nested/a/file.go", ["file.go"]),
    ("./src//file.go", ["src/file.go"]),
]


def _resolved_fixture():
    profiles = []
    for tiers in PROFILES:
        # Normalize the runtime root while asking Python to resolve the trust decision.
        real_root = Path.cwd()
        user = tiers["user"]
        values = dict(user.values)
        values["trust.paths"] = tuple(str(real_root if path == "$ROOT" else real_root / "child") for path in values.get("trust.paths", ()))
        user = project_profile._Tier(user.status, values, user.warnings)
        resolved = project_profile._resolve_profile(real_root, user, tiers["project"])
        profiles.append({"input": {"user": asdict(tiers["user"]), "project": asdict(tiers["project"])}, "output": asdict(resolved)})
    protected = [{"path": path, "patterns": patterns, "output": project_profile.is_protected(path, patterns)} for path, patterns in PROTECTED]
    return json.loads(json.dumps({"profiles": profiles, "protected": protected}))


def test_python_matches_profile_resolution_golden_fixture():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture == _resolved_fixture()


def test_go_profile_package():
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-cache")
    Path(env["GOCACHE"]).mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["go", "test", "./internal/profile/..."], cwd=ROOT / "go",
        text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
