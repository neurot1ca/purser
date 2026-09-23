from pathlib import Path
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import project_profile
from project_profile import Check, load_profile


@pytest.fixture
def profile_root():
    root = Path(__file__).parents[4] / ".tmp" / "orchestrator-tests" / uuid.uuid4().hex
    root.mkdir(parents=True)
    return root


def _load(root, project_text="schema_version = 1\n", user_text=None):
    project = root / ".orchestrator" / "profile.toml"
    project.parent.mkdir(parents=True, exist_ok=True)
    project.write_text(project_text, encoding="utf-8")
    user = root / "user.toml"
    if user_text is not None:
        user.write_text(user_text, encoding="utf-8")
    return load_profile(root, user_config_path=user)


def test_valid_checks_and_absent_section(profile_root):
    plain = _load(profile_root)
    assert plain.project_status == "ok" and plain.checks == ()
    valid = _load(profile_root, '''schema_version = 1
[[checks]]
name = "lint"
argv = ["npm", "run", "lint"]
when = ["**/*.ts", "app/**"]
timeout_seconds = 30
[[checks]]
name = "test"
argv = ["python", "-V"]
''', user_text=f'schema_version = 1\n[trust]\npaths = ["{profile_root.as_posix()}"]\n')
    assert valid.project_status == "ok"
    assert valid.trusted is True
    assert valid.checks == (Check("lint", ("npm", "run", "lint"), ("**/*.ts", "app/**"), 30), Check("test", ("python", "-V"), None, 60))
    assert not any("unknown" in warning for warning in valid.warnings)


def test_untrusted_project_checks_are_ignored_with_warning(profile_root):
    result = _load(profile_root, 'schema_version = 1\n[[checks]]\nname = "lint"\nargv = ["python", "-V"]\n')
    assert result.project_status == "ok"
    assert result.trusted is False
    assert result.checks == ()
    assert "project: [[checks]] ignored because the project is not trusted" in result.warnings


@pytest.mark.parametrize("section", [
    '[[checks]]\nname = "Bad!"\nargv = ["python"]\n',
    '[[checks]]\nname = "same"\nargv = ["python"]\n[[checks]]\nname = "same"\nargv = ["python"]\n',
    '[[checks]]\nname = "x"\nargv = []\n',
    '[[checks]]\nname = "x"\nargv = ["python", 3]\n',
    '[[checks]]\nname = "x"\nargv = ["python"]\ntimeout_seconds = 0\n',
    '[[checks]]\nname = "x"\nargv = ["python"]\ntimeout_seconds = 301\n',
    ''.join(f'[[checks]]\nname = "c{i}"\nargv = ["python"]\n' for i in range(9)),
])
def test_invalid_project_checks_reject_profile(profile_root, section):
    result = _load(profile_root, "schema_version = 1\n" + section)
    assert result.project_status == "rejected"
    assert result.checks == ()
    assert result.force_full_review


def test_user_tier_checks_rejected(profile_root):
    result = _load(profile_root, user_text='schema_version = 1\n[[checks]]\nname = "x"\nargv = ["python"]\n')
    assert result.user_status == "rejected"
    assert result.checks == ()


def test_unknown_project_top_level_key_warns(profile_root):
    result = _load(profile_root, "schema_version = 1\nunknown_section = true\n")
    assert "project: unknown top-level key or section [unknown_section] ignored" in result.warnings


def test_roles_defaults_when_user_section_absent(profile_root):
    result = _load(profile_root)
    assert result.roles == project_profile.Roles("claude-code", "codex", "agy")


def test_user_roles_load_valid_values(profile_root):
    result = _load(
        profile_root,
        user_text='schema_version = 1\n[roles]\norchestrator = "claude-code"\ncoder = "claude"\nreviewer = "none"\n',
    )
    assert result.user_status == "ok"
    assert result.roles == project_profile.Roles("claude-code", "claude", "none")


@pytest.mark.parametrize(
    "roles",
    ['reviewer = "codex"', 'extra = "agy"'],
    ids=["invalid-value", "unknown-key"],
)
def test_invalid_user_roles_reject_user_tier(profile_root, roles):
    result = _load(profile_root, user_text=f"schema_version = 1\n[roles]\n{roles}\n")
    assert result.user_status == "rejected"
    assert result.roles == project_profile.Roles("claude-code", "codex", "agy")
    assert result.force_full_review


def test_project_roles_do_not_take_effect(profile_root):
    result = _load(
        profile_root,
        project_text='schema_version = 1\n[roles]\nreviewer = "none"\n',
    )
    assert result.project_status == "ok"
    assert result.roles.reviewer == "agy"
    assert any("[roles]" in warning for warning in result.warnings)
