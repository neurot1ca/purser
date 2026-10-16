# Purser Claude Code plugin

## Install

Requirements: Claude Code, Node.js, and Git. For delegated work, install and
authenticate at least one coder CLI such as Codex. Purser can also run with
Claude alone.

Run these commands inside Claude Code:

```text
/plugin marketplace add neurot1ca/purser-claude-orchestrator
/plugin install purser@purser-claude-orchestrator
```

Restart Claude Code after installation, then run `/purser:check`.
Run `/purser:init` to configure Purser. Update with
`/plugin marketplace update purser-claude-orchestrator`.

## Release catalog

A release tag starts the release workflow. The workflow builds and verifies
the archive, publishes the GitHub Release, and emits a marketplace catalog
entry containing the archive URL and SHA-256 digest. Copy that entry into
`.claude-plugin/marketplace.json` and commit the catalog update to the default
branch to publish the new marketplace pin.

To roll back, replace the plugin entry in `.claude-plugin/marketplace.json`
with the verified archive entry from an earlier release, then commit that
change. Users can update the marketplace to install the pinned release.

## Local development

The release tool's export-only mode refreshes the local development marketplace
from the committed tree at `HEAD`:

```text
go run ./go/cmd/purser-release --export-only
```

It writes `~/.claude/purser/marketplace/` with a local catalog that points to
`./purser`. Use `--marketplace-dir <dir>` to choose another export directory.

## Plugin structure

This repository is the plugin source. The plugin registers the
`codex-orchestrator` skill, the `codex-main` MCP bridge, and the hooks. The MCP
definition lives under `plugin/`, since a root `.mcp.json` would be loaded as
project configuration before the plugin is enabled.

Hooks are registered through `hooks/hooks.json` and `hooks/register.ts`, which
calls the bundled `bin/purser-hook-<os>-<arch>` binary.

## Runtime

The MCP server starts with Node.js through
`bin/purser-launcher.js go-mcp`. Hooks and Purser commands run through the
bundled platform Go binary. `/purser:check`, `/purser:init`,
`/purser:optimize`, maintenance reports, and code-map generation use that
binary. The check reports `Runtime | OK | purser-hook <version> (<GOOS>/<GOARCH>)`.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
