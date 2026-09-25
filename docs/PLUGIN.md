# Purser Claude Code plugin (P6a)

This repository is the plugin source. The local `purser-local` marketplace is
generated outside the repository from the release commit and exposes one
plugin, `purser`. Local releases therefore ship only committed files. The plugin registers the existing
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

## Cutover

Run the standard-library cutover script once to inspect, apply, or roll back the
move from manual registration:

```powershell
python .claude/hooks/orchestrator/purser_cutover.py --plan
python .claude/hooks/orchestrator/purser_cutover.py --apply
```

Apply saves timestamped backups of `~/.claude/settings.json` and
`~/.claude.json`. To restore the newest backup pair:

```powershell
python .claude/hooks/orchestrator/purser_cutover.py --rollback
```

## Local release marketplace

Switch the existing local marketplace registration once. Replace `<dir>` with
the generated marketplace path (by default `~/.claude/purser/marketplace/`):

```powershell
claude plugin marketplace remove purser-local
claude plugin marketplace add <dir>
claude plugin install purser@purser-local
```

`scripts/release.py` refreshes this marketplace from the release commit before
running the plugin update commands. To refresh it without a version bump, run
`python scripts/release.py --export-only`; use `--marketplace-dir <dir>` to
choose another local path.

Restart Claude Code after switching the local marketplace, then run
`/purser:check`. The check should report plugin-only registration.

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
