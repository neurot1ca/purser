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
import pytest

_PYTHON_LEGACY_REVIEWER_CANDIDATES = [
    {"agent": "agy", "model": "gemini-3.8-flash-high"},
]
_GO_DEFAULT_REVIEWER_CANDIDATES = [
    {"agent": "agy", "model": "gemini-3.8-flash-high"},
    {"agent": "claude", "model": "claude-haiku-5-5"},
    {"agent": "codex", "model": "gpt-6-luna", "effort": "low"},
]


def _encode_go_reviewer_default(output):
    if list(output["reviewer_candidates"]) == _PYTHON_LEGACY_REVIEWER_CANDIDATES:
        output["reviewer_candidates"] = [dict(item) for item in _GO_DEFAULT_REVIEWER_CANDIDATES]
    return output


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

LOADS = [
    {"name": "trusted_full", "default_user": True,
     "user": '''schema_version = 1\n[trust]\npaths = ["$ROOT"]\n[roles]\nreviewer = "opencode"\n[reviewer.selected]\nagent = "codex"\nmodel = "review-model"\n[[reviewer.alternatives]]\nagent = "agy"\nmodel = "backup"\n[review]\nskip_max_lines = 40\nskip_max_files = 3\n[paths]\nprotected = ["user-secret"]\n[telemetry]\nvault_root = "/vault"\n''',
     "project": '''schema_version = 1\n[project]\nid = "demo"\n[review]\nskip_max_lines = 20\nskip_max_files = 1\nfull_suite_required = true\n[paths]\nprotected = ["project-secret"]\n[authority]\nsources = [{ path = "docs/guide.md", headings = ["Scope"], when_changed = "src/**" }]\n[[checks]]\nname = "lint"\nargv = ["go", "test"]\nwhen = ["go/**"]\ntimeout_seconds = 30\n[worktree]\ncopy = [".env"]\nsetup = "setup"\nsetup_timeout_seconds = 90\n'''},
    {"name": "missing_project", "default_user": False,
     "user": "schema_version = 1\n", "project": None},
    {"name": "missing_user", "default_user": False,
     "user": None, "project": "schema_version = 1\n"},
    {"name": "rejected_tiers", "default_user": False,
     "user": "schema_version = 2\n", "project": "schema_version = 2\n"},
    {"name": "outer_recovery", "default_user": False, "outer_failure": True},
]


def _resolved_fixture():
    profiles = []
    for index, tiers in enumerate(PROFILES):
        # Normalize the runtime root while asking Python to resolve the trust decision.
        real_root = Path.cwd()
        user = tiers["user"]
        values = dict(user.values)
        values["trust.paths"] = tuple(str(real_root if path == "$ROOT" else real_root / "child") for path in values.get("trust.paths", ()))
        user = project_profile._Tier(user.status, values, user.warnings)
        resolved = project_profile._resolve_profile(real_root, user, tiers["project"])
        output = asdict(resolved)
        if index in (1, 2):
            output = _encode_go_reviewer_default(output)
        profiles.append({"input": {"user": asdict(tiers["user"]), "project": asdict(tiers["project"])}, "output": output})
    protected = [{"path": path, "patterns": patterns, "output": project_profile.is_protected(path, patterns)} for path, patterns in PROTECTED]
    return json.loads(json.dumps({"profiles": profiles, "protected": protected}))


def _loaded_fixture(tmp_path, monkeypatch):
    home = tmp_path / "home"
    root = tmp_path / "project"
    (home / ".orchestrator").mkdir(parents=True)
    root.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    loads = []
    for case in LOADS:
        if case.get("outer_failure"):
            def fail(*args):
                raise RuntimeError("forced recovery")
            with monkeypatch.context() as patcher:
                patcher.setattr(project_profile, "_load_tier", fail)
                output = asdict(project_profile.load_profile(root))
        else:
            user_path = home / ".orchestrator" / "config.toml" if case["default_user"] else tmp_path / (case["name"] + ".toml")
            user = case.get("user")
            if user is not None:
                user_path.write_text(user.replace("$ROOT", root.as_posix()), encoding="utf-8")
            project_path = root / ".orchestrator" / "profile.toml"
            if project_path.exists():
                project_path.unlink()
            project = case.get("project")
            if project is not None:
                project_path.parent.mkdir(parents=True, exist_ok=True)
                project_path.write_text(project.replace("$ROOT", root.as_posix()), encoding="utf-8")
            output = asdict(project_profile.load_profile(root, user_config_path=None if case["default_user"] else user_path))
            if case["name"] in {"missing_project", "missing_user", "rejected_tiers"}:
                output = _encode_go_reviewer_default(output)
        loads.append({"input": case, "output": output})
    return json.loads(json.dumps(loads))


def test_python_matches_profile_resolution_golden_fixture():
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert {key: fixture[key] for key in ("profiles", "protected")} == _resolved_fixture()


def test_python_load_profile_golden_fixture(tmp_path, monkeypatch):
    fixture = json.loads(FIXTURE.read_text(encoding="utf-8"))
    assert fixture["loads"] == _loaded_fixture(tmp_path, monkeypatch)


def test_go_profile_package():
    env = os.environ.copy()
    env["GOCACHE"] = str(ROOT / ".tmp" / "go-cache")
    Path(env["GOCACHE"]).mkdir(parents=True, exist_ok=True)
    result = subprocess.run(
        ["go", "test", "./internal/profile/..."], cwd=ROOT / "go",
        text=True, capture_output=True, check=False, env=env,
    )
    assert result.returncode == 0, result.stdout + result.stderr
