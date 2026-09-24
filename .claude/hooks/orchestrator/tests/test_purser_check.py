import io
import json
import os
from pathlib import Path
import shutil
import sys
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import purser_check


ROOT = Path(__file__).parents[4]


@pytest.fixture
def fake_home(monkeypatch):
    scratch = ROOT / ".tmp"
    scratch.mkdir(exist_ok=True)
    home = scratch / f"purser-check-{uuid.uuid4().hex}"
    home.mkdir()
    monkeypatch.setattr(Path, "home", lambda: home)
    monkeypatch.setenv("CODEX_HOME", str(home / ".codex"))
    yield home
    shutil.rmtree(home, ignore_errors=True)


def settings(home, *, manual=False, plugin=False):
    payload = {"hooks": {}}
    if manual:
        payload["hooks"] = {"PreToolUse": [{"hooks": [{"type": "command", "command": "python ~/.orchestrator/hook.py"}]}]}
    if plugin:
        payload["enabledPlugins"] = {"purser@purser-local": True}
    (home / ".claude").mkdir(exist_ok=True)
    (home / ".claude" / "settings.json").write_text(json.dumps(payload), encoding="utf-8")


def no_environment_problems(monkeypatch):
    monkeypatch.setattr(purser_check.shutil, "which", lambda name: "C:/fake/codex.exe" if name == "codex" else None)
    monkeypatch.setattr(purser_check.subprocess, "run", lambda *args, **kwargs: type("Result", (), {"returncode": 0, "stdout": "codex 0.156.0", "stderr": ""})())
    monkeypatch.setattr(purser_check, "_newest_rollout_thread", lambda: None)


def test_double_registration_fails_and_exits_1(fake_home, monkeypatch):
    settings(fake_home, manual=True, plugin=True)
    no_environment_problems(monkeypatch)
    report = purser_check.collect_report()
    assert report["exit_code"] == 1
    assert next(row for row in report["checks"] if row["check"] == "Hook registration")["status"] == "FAIL"
    assert "double registration" in next(row for row in report["checks"] if row["check"] == "Hook registration")["detail"].lower()


def test_locator_row_reports_source(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    monkeypatch.setattr(purser_check, "_check_python_locator", lambda: purser_check._row("Python locator", "OK", "C:/Python312/python.exe (3.12.0) from PURSER_PYTHON."))
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Python locator")
    assert row["status"] == "OK" and "PURSER_PYTHON" in row["detail"]


def test_plugin_only_registration_is_ok(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    no_environment_problems(monkeypatch)
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Hook registration")
    assert row["status"] == "OK" and "plugin" in row["detail"].lower()


def test_manual_only_registration_is_ok(fake_home, monkeypatch):
    settings(fake_home, manual=True)
    no_environment_problems(monkeypatch)
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Hook registration")
    assert row["status"] == "OK" and "manual" in row["detail"].lower()


def test_neither_registration_fails(fake_home, monkeypatch):
    settings(fake_home)
    no_environment_problems(monkeypatch)
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Hook registration")
    assert row["status"] == "FAIL"


def test_missing_reviewer_executable_warns(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    no_environment_problems(monkeypatch)
    monkeypatch.setattr(purser_check.project_profile, "load_profile", lambda root: type("Profile", (), {"reviewer_executable": None, "trusted": False, "configured": False, "checks": (), "warnings": ()})())
    monkeypatch.setattr(purser_check, "_engine_config", lambda: {"reviewer1_executable": str(fake_home / "missing-reviewer.exe")})
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Reviewer")
    assert row["status"] == "WARN" and "FULL review" in row["detail"]


def test_roles_row_and_intentionally_disabled_reviewer_are_info(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    no_environment_problems(monkeypatch)
    user_config = fake_home / ".orchestrator" / "config.toml"
    user_config.parent.mkdir()
    user_config.write_text(
        'schema_version = 1\n[roles]\norchestrator = "claude-code"\n'
        'coder = "codex"\nreviewer = "none"\n',
        encoding="utf-8",
    )
    report = purser_check.collect_report()
    roles = next(row for row in report["checks"] if row["check"] == "Roles")
    reviewer = next(row for row in report["checks"] if row["check"] == "Reviewer")
    assert roles["status"] == "INFO"
    assert "every call is FULL review" in roles["detail"]
    assert "reviewer=none" in roles["detail"]
    assert reviewer["status"] == "INFO"


def test_missing_codex_fails(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    monkeypatch.setattr(purser_check.shutil, "which", lambda name: None)
    monkeypatch.setattr(purser_check, "_newest_rollout_thread", lambda: None)
    row = next(row for row in purser_check.collect_report()["checks"] if row["check"] == "Codex CLI")
    assert row["status"] == "FAIL"
    assert purser_check.collect_report()["exit_code"] == 1


def test_internal_check_error_becomes_warn(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    no_environment_problems(monkeypatch)
    monkeypatch.setattr(purser_check, "_check_python", lambda: (_ for _ in ()).throw(RuntimeError("broken probe")))
    report = purser_check.collect_report()
    row = next(row for row in report["checks"] if row["check"] == "Python")
    assert row["status"] == "WARN" and "broken probe" in row["detail"]
    assert report["exit_code"] == 0


def test_json_output_parses(fake_home, monkeypatch, capsys):
    settings(fake_home, plugin=True)
    no_environment_problems(monkeypatch)
    assert purser_check.main(["--json"]) == 0
    assert json.loads(capsys.readouterr().out)["checks"]


def test_json_output_is_utf8_with_cp1254_stdout(fake_home, monkeypatch):
    settings(fake_home, plugin=True)
    report = {
        "checks": [purser_check._row("Unicode", "WARN", "Vitest marker ❯ and Turkish ı")],
        "next_steps": [],
        "exit_code": 0,
    }
    monkeypatch.setattr(purser_check, "collect_report", lambda: report)
    output_raw = io.BytesIO()
    monkeypatch.setattr(purser_check.sys, "stdout", io.TextIOWrapper(output_raw, encoding="cp1254"))

    assert purser_check.main(["--json"]) == 0
    parsed = json.loads(output_raw.getvalue().decode("utf-8"))
    assert parsed["checks"][0]["detail"] == "Vitest marker ❯ and Turkish ı"
