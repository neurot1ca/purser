from pathlib import Path

import state_root


def test_purser_state_dir_has_highest_precedence(tmp_path, monkeypatch):
    configured = tmp_path / "configured"
    monkeypatch.setenv("PURSER_STATE_DIR", str(configured))
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "claude"))
    monkeypatch.setenv("LOCALAPPDATA", str(tmp_path / "local-app-data"))

    assert state_root.default_state_root() == configured.resolve()
    assert not configured.exists()


def test_empty_purser_state_dir_falls_through_to_claude_config(tmp_path, monkeypatch):
    config_dir = tmp_path / "claude-config"
    monkeypatch.setenv("PURSER_STATE_DIR", "")
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(config_dir))

    assert state_root.default_state_root() == (config_dir / "purser").resolve()


def test_default_uses_home_when_no_purser_or_claude_config_dir(tmp_path, monkeypatch):
    home = tmp_path / "home"
    monkeypatch.setattr(Path, "home", classmethod(lambda _cls: home))
    monkeypatch.delenv("PURSER_STATE_DIR", raising=False)
    monkeypatch.delenv("CLAUDE_CONFIG_DIR", raising=False)
    monkeypatch.setenv("LOCALAPPDATA", str(tmp_path / "irrelevant"))

    assert state_root.default_state_root() == (home / ".claude" / "purser").resolve()
    assert not (home / ".claude" / "purser").exists()


def test_localappdata_does_not_affect_configured_root(tmp_path, monkeypatch):
    configured = tmp_path / "shared-root"
    monkeypatch.setenv("PURSER_STATE_DIR", str(configured))
    monkeypatch.setenv("LOCALAPPDATA", str(tmp_path / "different"))

    assert state_root.default_state_root() == configured.resolve()
