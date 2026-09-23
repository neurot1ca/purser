import json
from pathlib import Path
import re


ROOT = Path(__file__).parents[4]
PLUGIN_TOOLS = {
    "mcp__plugin_purser_codex-main__codex",
    "mcp__plugin_purser_codex-main__codex-reply",
}


def test_plugin_manifest_paths_and_registration():
    manifest = json.loads((ROOT / ".claude-plugin/plugin.json").read_text(encoding="utf-8"))
    marketplace = json.loads((ROOT / ".claude-plugin/marketplace.json").read_text(encoding="utf-8"))
    hooks = json.loads((ROOT / "hooks/hooks.json").read_text(encoding="utf-8"))
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
    assert set(hooks["hooks"]) == {"PreToolUse", "PostToolUse", "PostToolUseFailure"}
    for name, timeout in (("PreToolUse", 60), ("PostToolUse", 300), ("PostToolUseFailure", 60)):
        entry, = hooks["hooks"][name]
        matched = {tool for tool in PLUGIN_TOOLS if re.fullmatch(entry["matcher"], tool)}
        assert matched == PLUGIN_TOOLS
        assert not re.fullmatch(entry["matcher"], "mcp__codex-main__codex")
        command, = entry["hooks"]
        assert command["timeout"] == timeout
        assert command["type"] == "command"
        assert "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/hook.py" in command["command"]
    bridge = mcp["mcpServers"]["codex-main"]
    assert bridge["command"] == "python"
    assert bridge["args"] == ["${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/codex_exec_mcp.py"]
    for value in [*bridge["args"], *(entry["hooks"][0]["command"] for entries in hooks["hooks"].values() for entry in entries)]:
        for relative in re.findall(r"\$\{CLAUDE_PLUGIN_ROOT\}/([^\s\"']+)", value):
            assert (ROOT / relative).is_file()
