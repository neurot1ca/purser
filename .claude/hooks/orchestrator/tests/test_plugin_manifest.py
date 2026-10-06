import json
import io
from pathlib import Path
import re
import sys


ROOT = Path(__file__).parents[4]
ORCHESTRATOR_ROOT = Path(__file__).resolve().parents[1]
if str(ORCHESTRATOR_ROOT) not in sys.path:
    sys.path.insert(0, str(ORCHESTRATOR_ROOT))

import install_claude_task_telemetry as installer
import session_start_maintenance


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
    assert session_start_maintenance.main() == 0
    assert calls == [("wake", tmp_path), ("cleanup", tmp_path / "state")]


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

    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8"))["hooks"]
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
    assert not any(
        entry.get("matcher") == r"^Read$"
        for entries in hooks["hooks"].values()
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
    assert set(hooks["hooks"]) == {"SessionStart"}
    assert "Stop" not in hooks["hooks"]
    session_start, = hooks["hooks"]["SessionStart"]
    assert "matcher" not in session_start
    assert session_start["hooks"][0] == {
        "type": "command",
        "command": "node",
        "timeout": 60,
        "args": [
            "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js",
            "hook",
            "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/session_start_maintenance.py",
        ],
    }
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
        for entry in hooks["hooks"].get("PreToolUse", [])
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
    assert bridge["args"] == ["${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js", "go-mcp", "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/codex_exec_mcp.py"]
    for value in [*bridge["args"], *(arg for entries in hooks["hooks"].values() for entry in entries for arg in entry["hooks"][0]["args"])]:
        for relative in re.findall(r"\$\{CLAUDE_PLUGIN_ROOT\}/([^\s\"']+)", value):
            assert (ROOT / relative).is_file()
