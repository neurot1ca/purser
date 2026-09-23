# Purser Claude Code plugin (P6a)

This repository is the plugin root. The local marketplace is `purser-local` and
exposes one plugin, `purser`. The plugin registers the existing
`codex-orchestrator` skill, the `codex-main` MCP bridge, and the same hooks as
the manual installer.
The MCP definition lives under `plugin/`, since a root `.mcp.json` would be
loaded as project configuration before the plugin is enabled.

| Event | Matcher | Script | Timeout |
| --- | --- | --- | ---: |
| PreToolUse | `^mcp__plugin_purser_codex-main__(codex|codex-reply)$` | `hook.py` | 60s |
| PreToolUse | `^Read$` | `context_io_hook.py` | 60s |
| PostToolUse | `^mcp__plugin_purser_codex-main__(codex|codex-reply)$` | `hook.py` | 300s |
| PostToolUseFailure | `^mcp__plugin_purser_codex-main__(codex|codex-reply)$` | `hook.py` | 60s |
| Stop | *(none)* | `hook.py` | 60s |
| Stop | *(none)* | `context_io_hook.py` | 60s |
| UserPromptSubmit | *(none)* | `context_io_hook.py` | 60s |

**WARNING:** Before enabling the plugin, remove the manual Purser hook entries
from `~/.claude/settings.json` and the manual `codex-main` MCP registration from
`~/.claude.json`. Keeping both registrations risks double hooks. That cutover is
P6d; this task does not change machine settings or install the plugin.

After cutover, in Claude Code:

```text
/plugin marketplace add <absolute path to this repository>
/plugin install purser@purser-local
/reload-plugins
```

Run `/purser:check` to read-only check the installation and see actionable next steps.
Run `/purser:init` for guided role selection, project trust, and optional profile scaffolding.

The `python` command used by the hook and MCP launchers must resolve to Python
3.11 or newer. A cross-platform Python locator is planned for P6b.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
