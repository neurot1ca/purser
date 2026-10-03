from pathlib import Path
import sys

import pytest


sys.path.insert(0, str(Path(__file__).parents[1]))
import coder_config


def test_user_reviewer_executable_overrides_shipped_config(tmp_path, monkeypatch):
    user_home = tmp_path / "home"
    user_config = user_home / ".orchestrator" / "config.toml"
    user_config.parent.mkdir(parents=True)
    executable = str((tmp_path / "custom reviewer").resolve())
    user_config.write_text(
        f"[coding]\nreviewer1_executable = '{executable}'\n", encoding="utf-8"
    )
    monkeypatch.setattr(coder_config.Path, "home", lambda: user_home)
    shipped_config = Path(__file__).parents[1] / "config.json"

    loaded = coder_config.load(shipped_config)

    assert loaded["reviewer1_executable"] == executable


def test_user_reviewer_executable_must_be_absolute(tmp_path, monkeypatch):
    user_home = tmp_path / "home"
    user_config = user_home / ".orchestrator" / "config.toml"
    user_config.parent.mkdir(parents=True)
    user_config.write_text('[coding]\nreviewer1_executable = "agy.exe"\n', encoding="utf-8")
    monkeypatch.setattr(coder_config.Path, "home", lambda: user_home)
    shipped_config = Path(__file__).parents[1] / "config.json"

    with pytest.raises(ValueError, match="reviewer1_executable must be an absolute path"):
        coder_config.load(shipped_config)


def test_readiness_checker_is_user_selectable_and_defaults_to_agy(tmp_path, monkeypatch):
    user_home = tmp_path / "home"
    monkeypatch.setattr(coder_config.Path, "home", lambda: user_home)
    shipped_config = Path(__file__).parents[1] / "config.json"

    assert coder_config.load(shipped_config)["readiness_check"] == {
        "agent": "agy", "model": "gemini-3.8-flash-high"
    }
    user_config = user_home / ".orchestrator" / "config.toml"
    user_config.parent.mkdir(parents=True)
    user_config.write_text(
        '[coding.readiness_check]\nagent = "off"\nmodel = "gemini-3.7-flash"\n',
        encoding="utf-8",
    )
    assert coder_config.load(shipped_config)["readiness_check"] == {
        "agent": "off", "model": "gemini-3.7-flash"
    }
