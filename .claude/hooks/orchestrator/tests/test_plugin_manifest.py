import json
from pathlib import Path
import re
import sys


ROOT = Path(__file__).parents[4]
ORCHESTRATOR_ROOT = Path(__file__).resolve().parents[1]
if str(ORCHESTRATOR_ROOT) not in sys.path:
    sys.path.insert(0, str(ORCHESTRATOR_ROOT))

import install_claude_task_telemetry as installer


PLUGIN_TOOLS = {
    "mcp__plugin_purser_codex-main__codex",
    "mcp__plugin_purser_codex-main__codex-reply",
    "mcp__plugin_purser_codex-main__codex_start",
    "mcp__plugin_purser_codex-main__codex_result",
    "mcp__plugin_purser_codex-main__codex_cancel",
}
PLUGIN_APPLY_TOOL = "mcp__plugin_purser_codex-main__codex_apply"


def test_plugin_hooks_match_manual_installer():
    installed, _ = installer.merge_settings(
        {}, repo_root=ROOT, python_executable=Path(sys.executable)
    )
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
            if matcher and "mcp__codex-main__" in matcher:
                matcher = matcher.replace(
                    "mcp__codex-main__", "mcp__plugin_purser_codex-main__"
                )
            expected.add((event, matcher, script, entry["hooks"][0]["timeout"]))

    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8"))["hooks"]
    actual = set()
    for event, entries in hooks.items():
        for entry in entries:
            command = entry["hooks"][0]
            script = Path(command["args"][-1]).name
            actual.add((event, entry.get("matcher"), script, command["timeout"]))

    assert {entry for entry in actual if entry[0] != "SessionStart"} == expected


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
    assert set(hooks["hooks"]) == {
        "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop", "UserPromptSubmit", "SessionStart", "PreCompact"
    }
    session_start, = hooks["hooks"]["SessionStart"]
    assert "matcher" not in session_start
    assert session_start["hooks"][0] == {
        "type": "command",
        "command": "node",
        "timeout": 60,
        "args": [
            "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js",
            "hook",
            "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/context_io_hook.py",
        ],
    }
    for name, timeout in (
        ("PreToolUse", 60),
        ("PostToolUse", 300),
        ("PostToolUseFailure", 60),
        ("Stop", 60),
    ):
        entry, = [
            item
            for item in hooks["hooks"][name]
            if item["hooks"][0]["args"][-1].endswith("/hook.py")
        ]
        if name != "Stop":
            matched = {
                tool for tool in PLUGIN_TOOLS if re.fullmatch(entry["matcher"], tool)
            }
            assert matched == PLUGIN_TOOLS
            if name == "PostToolUse":
                assert re.fullmatch(entry["matcher"], PLUGIN_APPLY_TOOL)
            else:
                assert not re.fullmatch(entry["matcher"], PLUGIN_APPLY_TOOL)
            assert not re.fullmatch(entry["matcher"], "mcp__codex-main__codex")
        command, = entry["hooks"]
        assert command["timeout"] == timeout
        assert command["type"] == "command"
        assert command["command"] == "node"
        assert command["args"] == ["${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js", "pre-codex" if name == "PreToolUse" else "hook", "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/hook.py"]
    bridge = mcp["mcpServers"]["codex-main"]
    assert bridge["command"] == "node"
    assert bridge["args"] == ["${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js", "mcp", "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/codex_exec_mcp.py"]
    for value in [*bridge["args"], *(arg for entries in hooks["hooks"].values() for entry in entries for arg in entry["hooks"][0]["args"])]:
        for relative in re.findall(r"\$\{CLAUDE_PLUGIN_ROOT\}/([^\s\"']+)", value):
            assert (ROOT / relative).is_file()
