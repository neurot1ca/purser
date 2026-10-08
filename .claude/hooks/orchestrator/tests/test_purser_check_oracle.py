import io
import json
import os
from pathlib import Path
import shutil
import sys
import uuid

import pytest

HOOKS = Path(__file__).parents[1]
sys.path.insert(0, str(HOOKS))
import purser_check

_CHECK_REGISTRATION = purser_check._check_registration
_COLLECT_REPORT = purser_check.collect_report

FIXTURE = Path(__file__).parent / "fixtures" / "purser_check_oracle.json"
RUNTIME_ROWS = {"Python", "Python locator", "Runtime"}


def _settings(home, *, manual=False, plugin=True):
    settings = {"hooks": {}}
    if manual:
        settings["hooks"] = {"PreToolUse": [{"hooks": [{"type": "command", "command": "python ~/.orchestrator/hook.py"}]}]}
    if plugin:
        settings["enabledPlugins"] = {"purser@purser-local": True}
    (home / ".claude").mkdir(exist_ok=True)
    (home / ".claude" / "settings.json").write_text(json.dumps(settings), encoding="utf-8")


def _normalize(report):
    checks = [row for row in report["checks"] if row["check"] not in RUNTIME_ROWS]
    next_steps = [
        {"number": index, "check": row["check"], "action": row["next_step"]}
        for index, row in enumerate((row for row in checks if row["status"] in {"WARN", "FAIL"}), 1)
    ]
    return {"checks": checks, "next_steps": next_steps,
            "exit_code": int(any(row["status"] == "FAIL" for row in checks))}


def _render_main(report, monkeypatch):
    rendered = {}
    monkeypatch.setattr(purser_check, "collect_report", lambda: report)
    for mode, argv in (("markdown", []), ("json", ["--json"])):
        captured = []
        monkeypatch.setattr(purser_check, "_write_output", captured.append)
        assert purser_check.main(argv) == report["exit_code"]
        rendered[mode] = captured[0]
    return rendered


@pytest.fixture
def oracle_case(monkeypatch):
    home = HOOKS.parents[2] / ".tmp" / f"purser-check-{uuid.uuid4().hex}"
    home.mkdir(parents=True)
    project = home / "project"
    project.mkdir()
    monkeypatch.setattr(Path, "home", lambda: home)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    monkeypatch.setenv("CODEX_HOME", str(home / ".codex"))
    monkeypatch.chdir(project)
    _settings(home)
    monkeypatch.setattr(purser_check, "_check_python", lambda: purser_check._row("Python", "OK", "3.12.0; python"))
    monkeypatch.setattr(purser_check, "_check_python_locator", lambda: purser_check._row("Python locator", "OK", "python (3.12.0) from PATH."))
    monkeypatch.setattr(purser_check.shutil, "which", lambda name: "C:/fake/codex.exe" if name == "codex" else None)
    monkeypatch.setattr(purser_check.subprocess, "run", lambda *a, **kw: type("Result", (), {"returncode": 0, "stdout": "codex 0.156.0", "stderr": ""})())
    monkeypatch.setattr(purser_check, "_newest_rollout_thread", lambda: None)
    yield home, monkeypatch
    shutil.rmtree(home, ignore_errors=True)


def _capture(case, name, monkeypatch):
    home, _ = case
    _settings(home, plugin=True)
    monkeypatch.setattr(purser_check, "collect_report", _COLLECT_REPORT)
    monkeypatch.setattr(purser_check.shutil, "which", lambda tool: "C:/fake/codex.exe" if tool == "codex" else None)
    monkeypatch.setattr(purser_check, "_check_registration", _CHECK_REGISTRATION)
    if name == "missing_registration":
        _settings(home, plugin=False)
    elif name == "registration_conflict":
        _settings(home, manual=True, plugin=True)
    elif name == "missing_codex":
        monkeypatch.setattr(purser_check.shutil, "which", lambda _name: None)
    elif name == "internal_inspection_failure":
        monkeypatch.setattr(purser_check, "_check_registration", lambda: (_ for _ in ()).throw(RuntimeError("broken inspection")))

    python_report = _COLLECT_REPORT()
    python_rendered = _render_main(python_report, monkeypatch)
    go_report = _normalize(python_report)
    go_rendered = _render_main(go_report, monkeypatch)
    return {"python": {"report": python_report, "rendered": python_rendered},
            "go": {"report": go_report, "rendered": go_rendered}}


def test_python_check_oracle_matches_frozen_fixture(oracle_case, monkeypatch):
    names = ("healthy_install", "missing_registration", "registration_conflict", "missing_codex", "internal_inspection_failure")
    actual = {name: _capture(oracle_case, name, monkeypatch) for name in names}
    if os.environ.get("PURSER_CAPTURE_CHECK_ORACLE") == "1":
        FIXTURE.parent.mkdir(parents=True, exist_ok=True)
        FIXTURE.write_text(json.dumps(actual, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    assert json.loads(FIXTURE.read_text(encoding="utf-8")) == actual


def test_python_json_output_is_utf8(oracle_case, monkeypatch):
    report = {"checks": [purser_check._row("Unicode", "WARN", "Vitest marker ❯ and Turkish ı")], "next_steps": [], "exit_code": 0}
    monkeypatch.setattr(purser_check, "collect_report", lambda: report)
    raw = io.BytesIO()
    monkeypatch.setattr(purser_check.sys, "stdout", io.TextIOWrapper(raw, encoding="cp1254"))
    assert purser_check.main(["--json"]) == 0
    assert json.loads(raw.getvalue().decode("utf-8"))["checks"][0]["detail"] == "Vitest marker ❯ and Turkish ı"
