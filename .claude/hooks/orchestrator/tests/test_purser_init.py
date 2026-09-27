import json
import os
from pathlib import Path
import shutil
import sys
import tomllib
import uuid

import pytest

sys.path.insert(0, str(Path(__file__).parents[1]))
import purser_init
import coder_config
import codex
import codex_exec_mcp
import project_profile


@pytest.fixture
def site(monkeypatch):
    root = Path(__file__).parents[4] / ".tmp" / f"purser-init-{uuid.uuid4().hex}"
    home, project = root / "home", root / "project"
    home.mkdir(parents=True, exist_ok=True)
    project.mkdir()
    monkeypatch.setattr(Path, "home", lambda: home)
    monkeypatch.setattr(purser_init.sys, "prefix", purser_init.sys.base_prefix)
    monkeypatch.setenv("LOCALAPPDATA", str(root / "local-app-data"))
    monkeypatch.chdir(project)
    yield home, project
    shutil.rmtree(root, ignore_errors=True)


def run(*args):
    return purser_init.main(list(args))


def test_plan_writes_nothing(site, capsys):
    home, project = site
    assert run("--plan", "--coder", "codex", "--reviewer", "none", "--trust", "--scaffold-profile") == 0
    output = capsys.readouterr().out
    assert "---" in output and "+++" in output and "schema_version = 1" in output
    handoff = purser_init.compaction_advisor.handoff_path(project)
    assert f"Create {handoff}:" in output
    assert not handoff.exists()
    assert not (home / ".orchestrator").exists()
    assert not (project / ".orchestrator").exists()


def test_apply_preserves_unrelated_bytes_and_backs_up(site):
    home, project = site
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    original = b'schema_version = 1\r\n# keep comment\r\n[telemetry]\r\nvault_root = "C:/vault" # keep exact\r\n[roles]\r\ncoder = "claude"\r\n[trust]\r\npaths = []\r\n'
    config.write_bytes(original)
    assert run("--apply", "--coder", "codex", "--reviewer", "none", "--trust") == 0
    changed = config.read_bytes()
    assert b'# keep comment\r\n[telemetry]\r\nvault_root = "C:/vault" # keep exact\r\n' in changed
    assert tomllib.loads(changed.decode())["roles"] == {"orchestrator": "claude-code", "coder": "codex", "reviewer": "none"}
    assert str(project) in tomllib.loads(changed.decode())["trust"]["paths"]
    backups = list(config.parent.glob("config.toml.bak-*"))
    assert len(backups) == 1 and backups[0].read_bytes() == original


def test_second_apply_noop(site):
    home, _ = site
    args = ("--apply", "--coder", "claude", "--reviewer", "none", "--trust", "--scaffold-profile")
    assert run(*args) == 0
    config = home / ".orchestrator" / "config.toml"
    before = config.read_bytes()
    assert run(*args) == 0
    assert config.read_bytes() == before
    assert (home / ".orchestrator" / "python").read_text(encoding="utf-8") == str(Path(sys.executable).resolve()) + "\n"
    assert len(list(config.parent.glob("config.toml.bak-*"))) == 0


def test_python_path_is_planned_and_second_apply_is_noop(site, capsys):
    home, _ = site
    args = ("--coder", "codex", "--reviewer", "none")
    assert run("--plan", *args) == 0
    plan = capsys.readouterr().out
    assert str(home / ".orchestrator" / "python") in plan
    assert str(Path(sys.executable).resolve()) in plan
    assert run("--apply", *args) == 0
    capsys.readouterr()
    assert run("--apply", *args) == 0
    assert capsys.readouterr().out.startswith("No changes.\nHandoff: ")


def test_existing_different_python_path_appears_in_plan(site, capsys):
    home, _ = site
    locator = home / ".orchestrator" / "python"
    locator.parent.mkdir()
    locator.write_bytes(b"C:/old/python.exe\n")
    assert run("--plan", "--coder", "codex", "--reviewer", "none") == 0
    plan = capsys.readouterr().out
    assert "-C:/old/python.exe" in plan
    assert str(Path(sys.executable).resolve()) in plan
    assert locator.read_bytes() == b"C:/old/python.exe\n"


def test_venv_uses_existing_python311_base_interpreter_in_plan_and_apply(site, monkeypatch, capsys):
    home, project = site
    base = project / "base-python"
    base.mkdir()
    base_python = base / "bin" / "python3"
    base_python.parent.mkdir()
    base_python.write_text("mock interpreter", encoding="utf-8")
    monkeypatch.setattr(purser_init.sys, "prefix", str(project / "venv"))
    monkeypatch.setattr(purser_init.sys, "base_prefix", str(base))
    monkeypatch.setattr(purser_init.sys, "_base_executable", str(base / "missing-python"), raising=False)
    monkeypatch.setattr(purser_init.sys, "platform", "linux")
    monkeypatch.setattr(purser_init.subprocess, "run", lambda *a, **k: type("Result", (), {
        "returncode": 0, "stdout": "Python 3.12.2", "stderr": ""})())
    args = ("--coder", "codex", "--reviewer", "none")
    assert run("--plan", *args) == 0
    plan = capsys.readouterr().out
    assert f"Python locator: {base_python.resolve()}" in plan
    assert str(Path(sys.executable).resolve()) not in plan
    assert run("--apply", *args) == 0
    assert (home / ".orchestrator" / "python").read_text(encoding="utf-8") == f"{base_python.resolve()}\n"


def test_venv_with_unusable_base_keeps_active_python_and_flags_plan(site, monkeypatch, capsys):
    _, project = site
    monkeypatch.setattr(purser_init.sys, "prefix", str(project / "venv"))
    monkeypatch.setattr(purser_init.sys, "base_prefix", str(project / "missing-base"))
    monkeypatch.setattr(purser_init.sys, "_base_executable", str(project / "missing-python"), raising=False)
    monkeypatch.setattr(purser_init.sys, "platform", "linux")
    assert run("--plan", "--coder", "codex", "--reviewer", "none") == 0
    plan = capsys.readouterr().out
    assert f"Python locator: {Path(sys.executable).resolve()}" in plan
    assert f"Note: this Python is inside a virtual environment ({sys.executable}); if it is removed, Purser hooks stop working." in plan


def test_already_trusted_literal_array_is_untouched(site, capsys):
    home, project = site
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    (config.parent / "python").write_bytes((str(Path(sys.executable).resolve()) + "\n").encode("utf-8"))
    equivalent = project / ".." / project.name
    original = (
        'schema_version = 1\n[roles]\norchestrator = "claude-code"\n'
        'coder = "codex"\nreviewer = "agy"\n[trust]\n'
        f"paths = ['{equivalent}', 'C:\\Projects\\other'] # keep\n"
    ).encode()
    config.write_bytes(original)
    args = ("--coder", "codex", "--reviewer", "agy", "--trust")
    assert run("--plan", *args) == 0
    handoff = purser_init.compaction_advisor.handoff_path(project)
    assert f"Create {handoff}:" in capsys.readouterr().out
    assert run("--apply", *args) == 0
    assert config.read_bytes() == original
    assert not list(config.parent.glob("config.toml.bak-*"))


def test_append_preserves_existing_literal_entries(site):
    home, project = site
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    prefix = (
        'schema_version = 1\n[roles]\norchestrator = "claude-code"\n'
        'coder = "codex"\nreviewer = "agy"\n[trust]\n'
    )
    existing = "paths = ['C:\\Projects\\one', 'C:\\Projects\\two'] # keep\n"
    config.write_text(prefix + existing, encoding="utf-8")
    assert run("--apply", "--coder", "codex", "--reviewer", "agy", "--trust") == 0
    changed = config.read_text(encoding="utf-8")
    assert "'C:\\Projects\\one', 'C:\\Projects\\two'" in changed
    assert f"'{project}'" in changed
    assert tomllib.loads(changed)["trust"]["paths"][-1] == str(project)


@pytest.mark.parametrize("field,value", [("--coder", "unknown"), ("--reviewer", "claude")])
def test_invalid_value_no_write(site, field, value):
    home, _ = site
    args = ["--apply", "--coder", "codex", "--reviewer", "none", "--trust"]
    args[args.index(field) + 1] = value
    with pytest.raises(SystemExit):
        run(*args)
    assert not (home / ".orchestrator").exists()


def test_invalid_config_left_untouched(site):
    home, _ = site
    config = home / ".orchestrator" / "config.toml"
    config.parent.mkdir()
    original = b'schema_version = 1\n[bad\n'
    config.write_bytes(original)
    assert run("--apply", "--coder", "codex", "--reviewer", "none", "--trust") != 0
    assert config.read_bytes() == original
    assert not list(config.parent.glob("*.bak-*"))


def test_existing_profile_never_overwritten(site):
    _, project = site
    profile = project / ".orchestrator" / "profile.toml"
    profile.parent.mkdir()
    profile.write_bytes(b"existing content\n")
    assert run("--apply", "--coder", "codex", "--reviewer", "none", "--scaffold-profile") == 0
    assert profile.read_bytes() == b"existing content\n"


def test_detect_without_config(site, capsys):
    assert run("--detect", "--json") == 0
    report = json.loads(capsys.readouterr().out)
    assert report["roles"] == {}
    assert report["trusted"] is False
    assert report["profile_exists"] is False
    assert "codex" in report["agents"]
    assert not purser_init.compaction_advisor.handoff_path(Path.cwd()).exists()


def test_apply_creates_handoff_and_preserves_existing_file(site, capsys):
    _, project = site
    handoff = purser_init.compaction_advisor.handoff_path(project)
    args = ("--apply", "--coder", "codex", "--reviewer", "none")
    assert run(*args) == 0
    output = capsys.readouterr().out
    assert handoff.read_text(encoding="utf-8") == purser_init.compaction_advisor.HANDOFF_TEMPLATE
    assert f"Handoff: {handoff}" in output

    handoff.write_text("user handoff; keep it\n", encoding="utf-8")
    assert run(*args) == 0
    assert handoff.read_text(encoding="utf-8") == "user handoff; keep it\n"


def test_opencode_absent_is_unavailable(site, monkeypatch, capsys):
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: None)
    assert run("--detect", "--json") == 0
    assert json.loads(capsys.readouterr().out)["agents"]["opencode"] == {
        "available": False, "path": None, "version": None, "models": []}


def test_detect_refreshes_candidates_for_installed_opencode(site, monkeypatch):
    calls = []
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    monkeypatch.setattr(purser_init, "_opencode_models", lambda _binary: ["vendor/model-free"])
    monkeypatch.setattr(purser_init, "_agent", lambda *_args: {"available": True, "path": "opencode"})
    monkeypatch.setattr(purser_init.model_catalog, "load_cached_catalog", lambda: {"providers": {"opencode": {}}})
    monkeypatch.setattr(purser_init.model_listing, "refresh_candidates", lambda providers: calls.append(providers))
    purser_init.detect(Path("missing-config"), Path.cwd())
    assert calls == [{"opencode": {}}]


def test_agy_absent_is_hidden_from_coder_options(site, monkeypatch, capsys):
    original_which = shutil.which
    monkeypatch.setattr(coder_config, "agy_binary", lambda: None)
    monkeypatch.setattr(purser_init.shutil, "which", lambda name: None if name == "agy" else original_which(name))
    assert run("--detect", "--json") == 0
    assert json.loads(capsys.readouterr().out)["agents"]["agy"]["available"] is False


def test_agy_choice_persists_gemini_routes(site, monkeypatch):
    home, project = site
    monkeypatch.setattr(coder_config, "agy_binary", lambda: "agy-native")
    monkeypatch.setattr(purser_init, "_agy_models", lambda binary: ["gemini-3.8-flash-low"])
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "gemini-3.8-flash-low", "family": "gemini", "efforts": None}]})
    assert run("--apply", "--coder", "agy", "--model", "gemini-3.8-flash-low", "--reviewer", "agy") == 0
    loaded = coder_config.load(Path(__file__).parents[1] / "config.json")
    assert loaded["coder_agent"] == "agy"
    assert set((entry["model"], entry["effort"]) for entry in loaded["routes"].values()) == {
        ("gemini-3.8-flash-low", "medium")}
    saved = tomllib.loads((home / ".orchestrator" / "config.toml").read_text(encoding="utf-8"))
    assert saved["coding"]["routes"] == loaded["routes"]
    assert saved["coding"]["family"] == "gemini"
    assert loaded["coder_family"] == "gemini"
    assert project_profile.load_profile(project).user_status == "ok"
    assert codex_exec_mcp.Server._configured_agent() == "agy"


def test_unknown_opencode_model_is_rejected_before_write(site, monkeypatch):
    home, _ = site
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode-native")
    monkeypatch.setattr(purser_init, "_opencode_models", lambda binary: ["opencode/big-pickle"])
    assert run("--apply", "--coder", "opencode", "--model", "other/model", "--reviewer", "none") == 1
    assert not (home / ".orchestrator").exists()


def test_opencode_choice_loads_same_model_on_all_routes(site, monkeypatch):
    home, _ = site
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode-native")
    monkeypatch.setattr(purser_init, "_opencode_models", lambda binary: ["opencode/big-pickle"])
    listing = {"status": "available", "models": [{"id": "opencode/big-pickle", "family": "opencode", "efforts": None}]}
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: listing)
    assert run("--apply", "--coder", "opencode", "--model", "opencode/big-pickle", "--reviewer", "none") == 0
    loaded = coder_config.load(Path(__file__).parents[1] / "config.json")
    assert loaded["coder_agent"] == "opencode"
    assert loaded["coder_family"] == "opencode"
    assert set(loaded["routes"]) == set(loaded["required_routes"])
    assert set((entry["model"], entry["effort"]) for entry in loaded["routes"].values()) == {
        ("opencode/big-pickle", "none")}
    assert codex._load_config(None)["routes"] == loaded["routes"]
    assert codex_exec_mcp.Server._configured_agent() == "opencode"
    listing["models"][0].pop("family")
    assert run("--apply", "--coder", "opencode", "--model", "opencode/big-pickle", "--reviewer", "none") == 0
    saved = tomllib.loads((home / ".orchestrator" / "config.toml").read_text(encoding="utf-8"))
    assert "family" not in saved["coding"]
    assert "coder_family" not in coder_config.load(Path(__file__).parents[1] / "config.json")


def test_switch_from_opencode_to_claude_clears_routes(site, monkeypatch):
    home, _ = site
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode-native")
    monkeypatch.setattr(purser_init, "_opencode_models", lambda binary: ["opencode/big-pickle"])
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "opencode/big-pickle", "family": "opencode", "efforts": None}]})
    assert run("--apply", "--coder", "opencode", "--model", "opencode/big-pickle", "--reviewer", "none") == 0
    assert run("--apply", "--coder", "claude", "--reviewer", "none") == 0
    saved = tomllib.loads((home / ".orchestrator" / "config.toml").read_text(encoding="utf-8"))
    assert saved["roles"]["coder"] == "claude"
    assert saved["coding"] == {"coder_agent": "codex", "routes": {}}
    assert coder_config.load(Path(__file__).parents[1] / "config.json")["routes"]["normal"]["model"] == "gpt-6-sol"


def test_balanced_persists_exact_shipped_codex_routes(site):
    home, _ = site
    assert run("--apply", "--coder", "codex", "--reviewer", "none", "--mode", "balanced") == 0
    shipped = json.loads((Path(__file__).parents[1] / "config.json").read_text(encoding="utf-8"))
    saved = tomllib.loads((home / ".orchestrator" / "config.toml").read_text(encoding="utf-8"))
    assert saved["coding"]["routes"] == shipped["routes"]
    assert coder_config.load(Path(__file__).parents[1] / "config.json")["routes"] == shipped["routes"]


def test_manual_plan_shows_resolved_route_model_effort_pairs(site, monkeypatch, capsys):
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "gpt-6-astra", "efforts": ["high"]}]})
    assert run("--plan", "--coder", "codex", "--reviewer", "none", "--mode", "manual",
               "--model", "gpt-6-astra", "--effort", "high") == 0
    output = capsys.readouterr().out
    assert "Resolved routes:\n" in output
    assert "  normal -> gpt-6-astra / high\n" in output
    assert "  plan_standard -> gpt-6-astra / high\n" in output


def test_opencode_plan_without_mode_shows_resolved_route_pairs(site, monkeypatch, capsys):
    model = "openai/gpt-6-astra"
    monkeypatch.setattr(coder_config, "opencode_binary", lambda: "opencode")
    monkeypatch.setattr(purser_init, "_opencode_models", lambda _: [model])
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": model, "efforts": None, "family": "openai"}]})
    assert run("--plan", "--coder", "opencode", "--model", model, "--reviewer", "none") == 0
    output = capsys.readouterr().out
    assert f"  normal -> {model} / none\n" in output


def test_maximum_uses_top_listed_effort_on_every_route(site, monkeypatch):
    home, _ = site
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "gpt-6-astra", "efforts": ["ultra", "high"]}]})
    assert run("--apply", "--coder", "codex", "--reviewer", "none", "--mode", "maximum") == 0
    loaded = coder_config.load(Path(__file__).parents[1] / "config.json")
    assert set(loaded["routes"]) == set(loaded["required_routes"])
    assert set((entry["model"], entry["effort"]) for entry in loaded["routes"].values()) == {
        ("gpt-6-astra", "ultra")}


def test_manual_opencode_without_listed_efforts_omits_effort(site, monkeypatch):
    home, _ = site
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: {
        "status": "available", "models": [{"id": "openai/gpt-6-astra", "efforts": None}]})
    assert run("--apply", "--coder", "opencode", "--reviewer", "none", "--mode", "manual",
               "--model", "openai/gpt-6-astra") == 0
    saved = tomllib.loads((home / ".orchestrator" / "config.toml").read_text(encoding="utf-8"))
    assert set(saved["coding"]["routes"].values()) == {"openai/gpt-6-astra"}
    assert {entry["effort"] for entry in coder_config.load(Path(__file__).parents[1] / "config.json")["routes"].values()} == {"none"}


@pytest.mark.parametrize("mode,model,effort,listing", [
    ("maximum", None, None, {"status": "unavailable", "reason": "CLI failed"}),
    ("maximum", None, None, {"status": "available", "models": [{"id": "gpt-other", "efforts": ["ultra"]}]}),
    ("manual", "gpt-6-astra", "ultra", {"status": "unavailable", "reason": "CLI failed"}),
    ("manual", "gpt-unknown", "high", {"status": "available", "models": [{"id": "gpt-6-astra", "efforts": ["high"]}]}),
    ("manual", "gpt-6-astra", "ultra", {"status": "available", "models": [{"id": "gpt-6-astra", "efforts": ["high"]}]}),
])
def test_unverified_mode_choice_rejects_before_write(site, monkeypatch, mode, model, effort, listing):
    home, _ = site
    monkeypatch.setattr(purser_init.model_catalog, "load_catalog", lambda: {"providers": {}})
    monkeypatch.setattr(purser_init.model_listing, "list_models", lambda *_: listing)
    args = ["--apply", "--coder", "codex", "--reviewer", "none", "--mode", mode]
    if model:
        args.extend(("--model", model, "--effort", effort))
    assert run(*args) == 1
    assert not (home / ".orchestrator").exists()
