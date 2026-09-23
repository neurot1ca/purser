import json
from pathlib import Path
import shutil
import sys
import uuid

import pytest


HERE = Path(__file__).resolve().parents[1]
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))
import purser_cutover as cutover


ROOT = HERE.parents[2]


@pytest.fixture
def fake_home():
    scratch = ROOT / ".tmp" / ("purser-cutover-" + uuid.uuid4().hex)
    home = scratch / "home"
    claude_dir = home / ".claude"
    claude_dir.mkdir(parents=True)
    settings = {
        "theme": "dark",
        "hooks": {
            "PreToolUse": [
                {"matcher": "^mcp__codex-main__(codex|codex-reply)$", "hooks": [{"type": "command", "command": 'python "C:/Users/test/.orchestrator/engine/.claude/hooks/orchestrator/hook.py"'}]},
                {"matcher": "Read", "hooks": [{"type": "command", "command": "python C:/Users/test/context_watch.py"}]},
            ],
            "UserPromptSubmit": [
                {"hooks": [{"type": "command", "command": 'python "C:/Users/test/.orchestrator/engine/.claude/hooks/orchestrator/context_io_hook.py"'}]}
            ],
        },
        "otherSetting": {"keep": [1, 2]},
    }
    claude = {
        "mcpServers": {
            "codex-main": {"command": "python", "args": ["C:/Users/test/.orchestrator/engine/.claude/hooks/orchestrator/codex_exec_mcp.py"]},
            "other": {"command": "other", "args": ["keep"]},
        },
        "otherTopLevel": True,
    }
    settings_path = claude_dir / "settings.json"
    claude_path = home / ".claude.json"
    settings_path.write_text(json.dumps(settings, indent=2) + "\n", encoding="utf-8")
    claude_path.write_text(json.dumps(claude, indent=2) + "\n", encoding="utf-8")
    yield home, settings_path, claude_path, settings, claude
    shutil.rmtree(scratch, ignore_errors=True)


def test_plan_writes_nothing(fake_home, capsys):
    home, settings_path, claude_path, _, _ = fake_home
    before = (settings_path.read_bytes(), claude_path.read_bytes())
    assert cutover.run("--plan", home, ROOT) == 0
    output = capsys.readouterr().out
    assert "event='PreToolUse'" in output
    assert "command=" in output
    assert "codex-main" in output
    assert "Everything else will remain unchanged." in output
    assert (settings_path.read_bytes(), claude_path.read_bytes()) == before
    assert not list(home.rglob("*.bak-purser-cutover-*"))


def test_apply_removes_only_purser_registration(fake_home, capsys):
    home, settings_path, claude_path, original_settings, original_claude = fake_home
    assert cutover.run("--apply", home, ROOT) == 0
    out = capsys.readouterr().out
    actual_settings = json.loads(settings_path.read_text(encoding="utf-8"))
    actual_claude = json.loads(claude_path.read_text(encoding="utf-8"))
    assert actual_settings["theme"] == original_settings["theme"]
    assert actual_settings["otherSetting"] == original_settings["otherSetting"]
    assert actual_settings["hooks"] == {"PreToolUse": [original_settings["hooks"]["PreToolUse"][1]]}
    assert actual_claude == {
        "mcpServers": {"other": original_claude["mcpServers"]["other"]},
        "otherTopLevel": True,
    }
    assert len(list(home.rglob("*.bak-purser-cutover-*"))) == 2
    assert "/plugin marketplace add " in out
    assert "/plugin install purser@purser-local" in out
    assert "Restart Claude Code" in out
    assert "/purser:check" in out


def test_other_codex_main_is_kept(fake_home):
    home, _, claude_path, _, _ = fake_home
    config = json.loads(claude_path.read_text(encoding="utf-8"))
    config["mcpServers"]["codex-main"] = {"command": "python", "args": ["unrelated_server.py"]}
    claude_path.write_text(json.dumps(config), encoding="utf-8")
    assert cutover.run("--apply", home, ROOT) == 0
    assert json.loads(claude_path.read_text(encoding="utf-8"))["mcpServers"]["codex-main"] == config["mcpServers"]["codex-main"]


def test_second_apply_is_noop_and_creates_no_backup(fake_home, capsys):
    home, settings_path, claude_path, _, _ = fake_home
    assert cutover.run("--apply", home, ROOT) == 0
    before = (settings_path.read_bytes(), claude_path.read_bytes())
    backups_before = sorted(p.name for p in home.rglob("*.bak-purser-cutover-*"))
    assert cutover.run("--apply", home, ROOT) == 0
    assert (settings_path.read_bytes(), claude_path.read_bytes()) == before
    assert sorted(p.name for p in home.rglob("*.bak-purser-cutover-*")) == backups_before
    assert "No changes needed; no backup created." in capsys.readouterr().out


def test_rollback_restores_latest_backups(fake_home):
    home, settings_path, claude_path, _, _ = fake_home
    original = (settings_path.read_bytes(), claude_path.read_bytes())
    assert cutover.run("--apply", home, ROOT) == 0
    assert cutover.run("--rollback", home, ROOT) == 0
    assert (settings_path.read_bytes(), claude_path.read_bytes()) == original


def test_malformed_json_aborts_without_writing(fake_home):
    home, settings_path, claude_path, _, _ = fake_home
    settings_path.write_text('{"hooks": [}', encoding="utf-8")
    before = (settings_path.read_bytes(), claude_path.read_bytes())
    assert cutover.run("--apply", home, ROOT) == 2
    assert (settings_path.read_bytes(), claude_path.read_bytes()) == before
    assert not list(home.rglob("*.bak-purser-cutover-*"))
