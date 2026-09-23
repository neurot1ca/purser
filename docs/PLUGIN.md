# Purser Claude Code plugin (P6a)

This repository is the plugin root. The local marketplace is `purser-local` and
exposes one plugin, `purser`. The plugin registers the existing
`codex-orchestrator` skill, the `codex-main` MCP bridge, and PreToolUse,
PostToolUse, and PostToolUseFailure hooks for the plugin-scoped Codex tools.
The MCP definition lives under `plugin/`, since a root `.mcp.json` would be
loaded as project configuration before the plugin is enabled.

**WARNING:** Before enabling the plugin, remove the manual Purser hook entries
from `~/.claude/settings.json` and the manual `codex-main` MCP registration from
`~/.claude.json`. Keeping both registrations risks double hooks. That cutover is
P6d; this task does not change machine settings or install the plugin.
The current manual installer also registers `Stop` and context I/O telemetry
hooks. P6d must account for those before removing manual entries; this P6a
package registers only the three requested Codex tool events.

After cutover, in Claude Code:

```text
/plugin marketplace add <absolute path to this repository>
/plugin install purser@purser-local
/reload-plugins
```

The `python` command used by the hook and MCP launchers must resolve to Python
3.11 or newer. A cross-platform Python locator is planned for P6b.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
