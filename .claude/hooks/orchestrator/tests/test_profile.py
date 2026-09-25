import os
from pathlib import Path
import shutil
import stat
import sys
import uuid

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import project_profile


@pytest.fixture()
def tmp_path(monkeypatch):
    root = (
        Path(__file__).parents[4]
        / ".tmp"
        / "orchestrator-tests"
        / "profile"
        / uuid.uuid4().hex
    )
    root.mkdir(parents=True, exist_ok=True)
    isolated_home = root / "home"
    monkeypatch.setenv("USERPROFILE", str(isolated_home))
    monkeypatch.setenv("HOME", str(isolated_home))
    monkeypatch.setattr(Path, "home", lambda: isolated_home)
    try:
        yield root
    finally:
        def make_writable(function, target, _error):
            os.chmod(target, stat.S_IWRITE)
            function(target)

        shutil.rmtree(root, onerror=make_writable)


def _write(path: Path, text: str) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


def test_is_protected_normalizes_paths_and_applies_pattern_scopes():
    patterns = ("tree/**", "docs/*.md", "AGENTS.md", "*.pem")

    assert project_profile.is_protected("./tree", patterns)
    assert project_profile.is_protected("tree/nested/file.txt", patterns)
    assert project_profile.is_protected("docs/README.md", patterns)
    assert project_profile.is_protected("nested/agents.md", patterns)
    assert project_profile.is_protected("keys/server.PEM", patterns)
    assert project_profile.is_protected("docs/nested/README.md", patterns)
    assert not project_profile.is_protected("other/nested/README.md", patterns)
    assert not project_profile.is_protected("src/agent.txt", patterns)


def test_engine_protected_paths_survive_without_a_project_profile(tmp_path):
    resolved = project_profile.load_profile(tmp_path / "project")

    assert all(
        project_profile.is_protected(path, resolved.protected_paths)
        for path in ("AGENTS.md", ".git/config", "nested/.git/HEAD", "keys/server.pem")
    )


def test_polymarket_profile_retains_predecessor_protected_paths(tmp_path):
    source = Path(__file__).parents[4] / "profiles" / "polymarket-btc5m-lab.toml"
    project_root = tmp_path / "project"
    _write(project_root / ".orchestrator" / "profile.toml", source.read_text(encoding="utf-8"))

    resolved = project_profile.load_profile(
        project_root, user_config_path=tmp_path / "missing-user-config.toml"
    )

    assert all(
        project_profile.is_protected(path, resolved.protected_paths)
        for path in (
            "state/current.yaml",
            "docs/EVIDENCE_RULES.md",
            ".gitattributes",
            "inputs/a.csv",
            "review_artifacts/x/y.json",
        )
    )


def test_project_can_only_tighten_review_and_protected_paths(tmp_path):
    user_config = _write(
        tmp_path / "user.toml",
        f'''schema_version = 1

[paths]
protected = ["user/**"]

[review]
skip_max_lines = 40
skip_max_files = 1
''',
    )
    project_root = tmp_path / "project"
    _write(
        project_root / ".orchestrator" / "profile.toml",
        '''schema_version = 1

[paths]
protected = ["project/**"]

[review]
skip_max_lines = 100
skip_max_files = 5
''',
    )

    resolved = project_profile.load_profile(project_root, user_config_path=user_config)

    assert resolved.skip_max_lines == 40
    assert resolved.skip_max_files == 1
    assert resolved.loosenings_ignored == 2
    assert {".git/**", "user/**", "project/**"}.issubset(
        resolved.protected_paths
    )


def test_rejected_project_tier_preserves_user_and_forces_full_review(tmp_path):
    user_config = _write(
        tmp_path / "user.toml",
        '''schema_version = 1

[review]
force_full = false
skip_max_lines = 10
skip_max_files = 1
''',
    )
    project_root = tmp_path / "project"
    _write(
        project_root / ".orchestrator" / "profile.toml",
        '''schema_version = 1

[project]
id = "should-be-ignored"

[review]
unknown = true
''',
    )

    resolved = project_profile.load_profile(project_root, user_config_path=user_config)

    assert resolved.project_status == "rejected"
    assert resolved.project_id is None
    assert resolved.skip_max_lines == 10
    assert resolved.skip_max_files == 1
    assert resolved.force_full_review is True
    assert any("unknown security key" in warning for warning in resolved.warnings)


def test_non_security_wrong_type_warns_without_rejecting_user_tier(tmp_path):
    user_config = _write(
        tmp_path / "user.toml",
        f'''schema_version = 1

[reviewer]
executable = "{(tmp_path / "reviewer.exe").as_posix()}"

[telemetry]
vault_root = 123
''',
    )

    resolved = project_profile.load_profile(tmp_path / "project", user_config_path=user_config)

    assert resolved.user_status == "ok"
    assert resolved.reviewer_executable is not None
    assert resolved.vault_root is None
    assert any("vault_root must be a string" in warning for warning in resolved.warnings)


def test_invalid_project_authority_sources_warn_and_fall_back(tmp_path):
    project_root = tmp_path / "project"
    _write(
        project_root / ".orchestrator" / "profile.toml",
        '''schema_version = 1

[[authority.sources]]
path = "../outside.md"
headings = ["Secret"]
''',
    )

    resolved = project_profile.load_profile(
        project_root, user_config_path=tmp_path / "missing-user-config.toml"
    )

    assert resolved.project_status == "ok"
    assert resolved.authority_sources == (
        project_profile.AuthoritySource(
            "AGENTS.md",
            ("Authority", "Working scope", "Proportional validation", "Test and comment quality"),
        ),
    )
    assert any("sources[0].path" in warning for warning in resolved.warnings)


def test_trust_requires_exact_canonical_path(tmp_path):
    trusted_root = tmp_path / "Trusted"
    child = trusted_root / "child"
    child.mkdir(parents=True)
    user_config = _write(
        tmp_path / "user.toml",
        f'''schema_version = 1

[trust]
paths = ["{trusted_root.as_posix()}"]
''',
    )

    assert project_profile.load_profile(child, user_config_path=user_config).trusted is False

    if os.name == "nt":
        case_variant = Path(str(trusted_root).swapcase())
        assert project_profile.load_profile(
            case_variant, user_config_path=user_config
        ).trusted is True

    relative_config = _write(
        tmp_path / "relative-user.toml",
        '''schema_version = 1

[trust]
paths = ["."]
''',
    )
    relative = project_profile.load_profile(Path.cwd(), user_config_path=relative_config)
    assert relative.user_status == "rejected"
    assert relative.trusted is False
