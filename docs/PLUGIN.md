# Purser Claude Code plugin (P6a)

This repository is the plugin source. The local `purser-local` marketplace is
generated outside the repository from the release commit and exposes one
plugin, `purser`. Local releases therefore ship only committed files. The plugin registers the existing
`codex-orchestrator` skill, the `codex-main` MCP bridge, and the same hooks as
the manual installer.
The MCP definition lives under `plugin/`, since a root `.mcp.json` would be
loaded as project configuration before the plugin is enabled.

Hooks are registered through the mod in `hooks/hooks.json` and
`hooks/register.ts`, which calls the bundled
`bin/purser-hook-<os>-<arch>` binary.

## Local release marketplace

Switch the existing local marketplace registration once. Replace `<dir>` with
the generated marketplace path (by default `~/.claude/purser/marketplace/`):

```powershell
claude plugin marketplace remove purser-local
claude plugin marketplace add <dir>
claude plugin install purser@purser-local
```

For development, the Go release tool refreshes this marketplace from the
release commit before running the plugin update commands. To refresh it
without a version bump, run:

```powershell
go run ./go/cmd/purser-release --export-only
```

Use `--marketplace-dir <dir>` to choose another local path.

Restart Claude Code after switching the local marketplace, then run
`/purser:check`. The check should report plugin-only registration.

Run `/purser:check` to read-only check the installation and see actionable next steps.
Run `/purser:init` for guided role selection, project trust, and optional profile scaffolding.

## Runtime

The MCP server starts with Node.js through
`bin/purser-launcher.js go-mcp`. Hooks and Purser commands run through the
bundled platform Go binary. `/purser:check`, `/purser:init`,
`/purser:optimize`, maintenance reports, and code-map generation use that binary.
The check reports `Runtime | OK | purser-hook <version> (<GOOS>/<GOARCH>)`.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
