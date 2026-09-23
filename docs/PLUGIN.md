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

## Python locator

Plugin hooks and the MCP server use `bin/python-locator.js` to run a verified
Python 3.11+ interpreter. It tries `PURSER_PYTHON`, then the path saved by
`/purser:init` in `~/.orchestrator/python`, then `py -3` on Windows, `python3`,
and `python`. Set `PURSER_PYTHON` to a specific interpreter to override the
saved path. Node.js must be available for the launcher; `/purser:check` reports
what it resolves.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
