import json
import sys
from pathlib import Path

import pytest


ORCHESTRATOR_ROOT = Path(__file__).resolve().parents[1]
if str(ORCHESTRATOR_ROOT) not in sys.path:
    sys.path.insert(0, str(ORCHESTRATOR_ROOT))

import install_claude_task_telemetry as installer


def _repo(tmp_path: Path) -> Path:
    root = tmp_path / "repo"
    hook = root / ".claude" / "hooks" / "orchestrator" / "hook.py"
    hook.parent.mkdir(parents=True)
    hook.write_text("# synthetic hook\n", encoding="utf-8")
    return root


def test_install_preserves_unrelated_settings_and_is_idempotent(tmp_path):
    root = _repo(tmp_path)
    settings_path = root / ".claude" / "settings.local.json"
    unrelated = {"matcher": "^Read$", "hooks": [{"type": "command", "command": "other", "timeout": 5}]}
    original = {"permissions": {"allow": ["Read"]}, "hooks": {"PreToolUse": [unrelated]}}
    settings_path.write_text(json.dumps(original), encoding="utf-8")

    status = installer.install(
        repo_root=root,
        settings_path=settings_path,
        python_executable=Path(sys.executable),
        apply=True,
    )
    assert status == "INSTALLED"
    installed = json.loads(settings_path.read_text(encoding="utf-8"))
    assert installed["permissions"] == original["permissions"]
    assert installed["hooks"]["PreToolUse"][0] == unrelated
    for event_name, matcher, timeout in installer.HOOK_SPECS:
        managed = [entry for entry in installed["hooks"][event_name] if event_name != "PreToolUse" or entry != unrelated]
        assert len(managed) == 1
        if matcher is None:
            assert "matcher" not in managed[0]
        else:
            assert managed[0]["matcher"] == matcher
        assert managed[0]["hooks"][0]["timeout"] == timeout

    before = settings_path.read_bytes()
    assert installer.install(
        repo_root=root,
        settings_path=settings_path,
        python_executable=Path(sys.executable),
        apply=True,
    ) == "ALREADY_INSTALLED"
    assert settings_path.read_bytes() == before
    assert len(list(settings_path.parent.glob("settings.local.json.bak-*"))) == 1


def test_check_only_does_not_write(tmp_path):
    root = _repo(tmp_path)
    settings_path = root / ".claude" / "settings.local.json"
    assert installer.install(
        repo_root=root,
        settings_path=settings_path,
        python_executable=Path(sys.executable),
        apply=False,
    ) == "INSTALL_REQUIRED"
    assert not settings_path.exists()


def test_existing_managed_entries_are_replaced_without_duplicates(tmp_path):
    root = _repo(tmp_path)
    settings_path = root / ".claude" / "settings.local.json"
    hook_path = root / ".claude" / "hooks" / "orchestrator" / "hook.py"
    stale = {
        "matcher": installer.HOOK_SPECS[0][1],
        "hooks": [{"type": "command", "command": f'"old-python" "{hook_path}"', "timeout": 1}],
    }
    settings_path.parent.mkdir(parents=True, exist_ok=True)
    settings_path.write_text(json.dumps({"hooks": {"PreToolUse": [stale]}}), encoding="utf-8")
    installer.install(
        repo_root=root,
        settings_path=settings_path,
        python_executable=Path(sys.executable),
        apply=True,
    )
    installed = json.loads(settings_path.read_text(encoding="utf-8"))
    assert len(installed["hooks"]["PreToolUse"]) == 1
    assert "old-python" not in installed["hooks"]["PreToolUse"][0]["hooks"][0]["command"]


def test_invalid_json_and_outside_path_fail_without_writing(tmp_path):
    root = _repo(tmp_path)
    settings_path = root / ".claude" / "settings.local.json"
    settings_path.parent.mkdir(parents=True, exist_ok=True)
    settings_path.write_text("{broken", encoding="utf-8")
    before = settings_path.read_bytes()
    with pytest.raises(installer.InstallError):
        installer.install(
            repo_root=root,
            settings_path=settings_path,
            python_executable=Path(sys.executable),
            apply=True,
        )
    assert settings_path.read_bytes() == before

    outside = tmp_path / "outside.json"
    with pytest.raises(installer.InstallError):
        installer.install(
            repo_root=root,
            settings_path=outside,
            python_executable=Path(sys.executable),
            apply=True,
        )
    assert not outside.exists()
