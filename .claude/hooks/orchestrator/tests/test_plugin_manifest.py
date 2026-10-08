import json
import io
from pathlib import Path
import re
import sys

import pytest

ROOT = Path(__file__).parents[4]
ORCHESTRATOR_ROOT = Path(__file__).resolve().parents[1]
if str(ORCHESTRATOR_ROOT) not in sys.path:
    sys.path.insert(0, str(ORCHESTRATOR_ROOT))

import install_claude_task_telemetry as installer
import session_start_maintenance
import context_io_hook
import model_version_notice


PLUGIN_TOOLS = {
    "mcp__plugin_purser_codex-main__codex",
    "mcp__plugin_purser_codex-main__codex-reply",
    "mcp__plugin_purser_codex-main__codex_start",
    "mcp__plugin_purser_codex-main__codex_result",
    "mcp__plugin_purser_codex-main__codex_cancel",
}
PLUGIN_APPLY_TOOL = "mcp__plugin_purser_codex-main__codex_apply"


def test_session_start_maintenance_keeps_python_cleanup(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(session_start_maintenance.sys, "stdin", io.StringIO(json.dumps({"hook_event_name": "SessionStart"})))
    monkeypatch.setattr(session_start_maintenance.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(session_start_maintenance.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(session_start_maintenance.maintenance_reports, "wake_background_check", lambda repo: calls.append(("wake", repo)))
    monkeypatch.setattr(session_start_maintenance.async_session_maintenance, "cleanup_jobs", lambda root: calls.append(("cleanup", root)))
    monkeypatch.setattr(context_io_hook, "discover_model_version", lambda _repo: None)
    assert session_start_maintenance.main() == 0
    assert calls == [("wake", tmp_path), ("cleanup", tmp_path / "state")]


def test_session_start_queues_newer_model_version_without_printing(monkeypatch, tmp_path):
    state_root = tmp_path / "state"
    monkeypatch.setattr(model_version_notice, "state_root", session_start_maintenance.state_root)
    monkeypatch.setattr(session_start_maintenance.state_root, "default_state_root", lambda: state_root)
    monkeypatch.setattr(session_start_maintenance.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(session_start_maintenance.maintenance_reports, "wake_background_check", lambda _repo: None)
    monkeypatch.setattr(session_start_maintenance.async_session_maintenance, "cleanup_jobs", lambda _root: None)
    monkeypatch.setattr(session_start_maintenance.sys, "stdin", io.StringIO(json.dumps({"hook_event_name": "SessionStart"})))
    output = io.StringIO()
    monkeypatch.setattr(session_start_maintenance.sys, "stdout", output)
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda _path: {
        "coder_agent": "codex", "routes": {"normal": "gpt-6-luna", "mechanical": {"model": "gpt-6-luna"}},
        "reviewer1_model_requested": "gemini-3.8-flash-high",
    })
    monkeypatch.setattr(context_io_hook.model_catalog, "load_cached_catalog", lambda: {"providers": {}})
    listing_calls = []
    def list_models(agent, _providers, *, timeout_seconds):
        listing_calls.append((agent, timeout_seconds))
        models = ["gpt-6.1-luna"] if agent == "codex" else ["gemini-3.9-flash-high"]
        return {"status": "available", "models": [{"id": model} for model in models]}
    monkeypatch.setattr(context_io_hook.model_listing, "list_models", list_models)

    assert session_start_maintenance.main() == 0
    state = model_version_notice.read_state()
    assert state["pending"] == {"current": "gpt-6-luna", "new": "gpt-6.1-luna",
                                "blocked": False, "delivered": False}
    assert state["checked_on"] is not None
    assert listing_calls == [("codex", 2)]
    assert output.getvalue() == ""


def test_session_start_model_version_discovery_does_nothing_when_not_due(monkeypatch, tmp_path):
    monkeypatch.setattr(model_version_notice, "state_root", session_start_maintenance.state_root)
    monkeypatch.setattr(session_start_maintenance.state_root, "default_state_root", lambda: tmp_path / "state")
    monkeypatch.setattr(session_start_maintenance.project_root, "resolve_project_root", lambda _event: (tmp_path, "test"))
    monkeypatch.setattr(session_start_maintenance.maintenance_reports, "wake_background_check", lambda _repo: None)
    monkeypatch.setattr(session_start_maintenance.async_session_maintenance, "cleanup_jobs", lambda _root: None)
    monkeypatch.setattr(session_start_maintenance.sys, "stdin", io.StringIO(json.dumps({"hook_event_name": "SessionStart"})))
    monkeypatch.setattr(context_io_hook.model_version_notice, "due_today", lambda: False)
    monkeypatch.setattr(context_io_hook.coder_config, "load", lambda _path: pytest.fail("configuration loaded when not due"))

    assert session_start_maintenance.main() == 0
    assert model_version_notice.read_state() == {"checked_on": None, "asked": [], "declined": [], "pending": None}


def test_plugin_hooks_match_manual_installer():
    installed, _ = installer.merge_settings(
        {}, repo_root=ROOT, python_executable=Path(sys.executable)
    )
    assert {"PostToolUse", "PostToolUseFailure", "Stop"} <= set(installed["hooks"])
    expected = set()
    for event, entries in installed["hooks"].items():
        for entry in entries:
            manual_command = entry["hooks"][0]["command"]
            script = Path(manual_command.split('" "')[-1].strip('"')).name
            matcher = entry.get("matcher")
            # The plugin Read registration moved to hooks/register.ts; keep
            # the manual installer Read hook available for release rollback.
            if event == "PreToolUse" and matcher == r"^Read$":
                continue
            if script == "context_io_hook.py" and event in {"Stop", "UserPromptSubmit", "SessionStart", "PreCompact"}:
                continue
            if event == "Stop":
                continue
            if matcher and "mcp__codex-main__" in matcher:
                matcher = matcher.replace(
                    "mcp__codex-main__", "mcp__plugin_purser_codex-main__"
                )
            if event == "PreToolUse" and matcher and "mcp__plugin_purser_codex-main__" in matcher:
                continue
            expected.add((event, matcher, script, entry["hooks"][0]["timeout"]))

    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8")).get("hooks", {})
    # HOOK_SPECS describes the manual installer. Compare only its events that
    # remain registered directly by the plugin manifest.
    plugin_events = set(hooks) & {event for event, _matcher, _timeout in installer.HOOK_SPECS}
    actual = set()
    for event, entries in hooks.items():
        for entry in entries:
            command = entry["hooks"][0]
            script = Path(command["args"][-1]).name
            actual.add((event, entry.get("matcher"), script, command["timeout"]))

    assert {entry for entry in actual if entry[0] in plugin_events} == {
        entry for entry in expected if entry[0] in plugin_events
    }


def test_plugin_manifest_paths_and_registration():
    manifest = json.loads((ROOT / ".claude-plugin/plugin.json").read_text(encoding="utf-8"))
    marketplace = json.loads((ROOT / ".claude-plugin/marketplace.json").read_text(encoding="utf-8"))
    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8"))
    assert hooks["modules"] == ["./register.ts"]
    assert "hooks" not in hooks
    assert not any(
        entry.get("matcher") == r"^Read$"
        for entries in hooks.get("hooks", {}).values()
        for entry in entries
    )
    assert (ROOT / "hooks/register.ts").is_file()
    assert not (ROOT / ".mcp.json").exists()
    mcp = json.loads((ROOT / manifest["mcpServers"]).read_text(encoding="utf-8"))
    assert manifest["name"] == "purser"
    assert len(marketplace["plugins"]) == 1
    assert marketplace["plugins"][0]["source"] == "./"
    assert marketplace["plugins"][0]["name"] == "purser"
    assert (ROOT / manifest["skills"]).is_dir()
    assert (ROOT / manifest["skills"] / "codex-orchestrator/SKILL.md").is_file()
    assert (ROOT / manifest["hooks"]).is_file()
    assert (ROOT / manifest["mcpServers"]).is_file()
    # Plugin commands are auto-discovered from the conventional commands/ directory.
    assert (ROOT / "commands/check.md").is_file()
    register_source = (ROOT / "hooks/register.ts").read_text(encoding="utf-8")
    assert re.search(r"^  registerEnforcement\(on\)$", register_source, re.M)
    assert "on('classic.Stop', ($, e, next) => lifecycle($, e, next, 'Stop'))" in register_source
    enforcement_source = (ROOT / "hooks/mod/enforcement.ts").read_text(encoding="utf-8")
    assert 'on("classic.PostToolUse", ($, e, next) => handlePostToolUse(\'PostToolUse\', $, e, next))' in enforcement_source
    assert 'on("classic.PostToolUseFailure", ($, e, next) => handlePostToolUse(\'PostToolUseFailure\', $, e, next))' in enforcement_source
    post_tools = re.search(r"const postToolUseTools = new Set\(\[([\s\S]*?)\]\)", enforcement_source)
    failure_tools = re.search(r"const postToolUseFailureTools = new Set\(\[([\s\S]*?)\]\)", enforcement_source)
    assert post_tools and failure_tools
    assert set(re.findall(r"'([^']+)'", post_tools.group(1))) == PLUGIN_TOOLS | {PLUGIN_APPLY_TOOL}
    assert set(re.findall(r"'([^']+)'", failure_tools.group(1))) == PLUGIN_TOOLS
    assert not any(
        re.fullmatch(entry.get("matcher", ""), tool)
        for entry in hooks.get("hooks", {}).get("PreToolUse", [])
        for tool in PLUGIN_TOOLS
    )
    adapter_sets = re.findall(r"const (?:syncTools|startTools|followupTools) = new Set\(\[([\s\S]*?)\]\)", enforcement_source)
    adapter_tools = {tool for body in adapter_sets for tool in re.findall(r"'([^']+)'", body)}
    codex_pre_tools = {f"mcp__{identity}__{tool}" for identity in ("codex-main", "plugin_purser_codex-main") for tool in ("codex", "codex-reply", "codex_start", "codex_result", "codex_cancel")}
    assert adapter_tools == codex_pre_tools
    assert PLUGIN_APPLY_TOOL not in adapter_tools
    assert "PURSER_MOD_ASYNC_START" not in enforcement_source
    bridge = mcp["mcpServers"]["codex-main"]
    assert bridge["command"] == "node"
    assert bridge["args"] == ["${CLAUDE_PLUGIN_ROOT}/bin/purser-launcher.js", "go-mcp"]
    for value in [*bridge["args"], *(arg for entries in hooks.get("hooks", {}).values() for entry in entries for arg in entry["hooks"][0]["args"] )]:
        for relative in re.findall(r"\$\{CLAUDE_PLUGIN_ROOT\}/([^\s\"']+)", value):
            assert (ROOT / relative).is_file()
