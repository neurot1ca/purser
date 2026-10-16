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
calls the bundled `bin/purser-hook-<os>-<arch>` binary. The PreToolUse hook
publishes a per-call identity handoff and then forwards the original event
unchanged with `next(e)`; it denies the call if publishing fails. It does not
rewrite tool input or route Purser tools. The MCP server claims the matching
handoff, performs routing, and enforces main-Claude-only acceptance.

## Runtime

The MCP server starts with Node.js through
`bin/purser-launcher.js go-mcp`. Hooks and Purser commands run through the
bundled platform Go binary. `/purser:check`, `/purser:init`,
`/purser:optimize`, maintenance reports, and code-map generation use that
binary. The check reports `Runtime | OK | purser-hook <version> (<GOOS>/<GOARCH>)`.

The hook and MCP launcher resolve their Go executable from the installed
plugin's `bin/` directory. The plugin ships its own binaries, so both use the
same plugin version. Mixing versions is unsupported. A new MCP binary paired
with an old hook denies a call when its required handoff is missing. An old MCP
binary paired with a new hook does not consume the new handoff protocol and
does not provide the current server-side enforcement contract; keep the plugin
components from the same installation together.

## Command permissions

Slash commands grant Bash access only to the quoted plugin-root binary for the
listed subcommands, with one matching rule for each of the six shipped binary
names (`windows-amd64.exe`, `windows-arm64.exe`, `darwin-amd64`,
`darwin-arm64`, `linux-amd64`, and `linux-arm64`). Argument wildcards are only
after the listed subcommand. The grants are:

- `/purser:check`: `check`.
- `/purser:init`: `init` and `maintenance run cleanup`.
- `/purser:optimize`: `optimize`, `maintenance opt-in`,
  `maintenance run cleanup`, and `maintenance opt-out`.
- `/purser:plan`: `plan accept`, `plan status`, `plan complete-milestone`,
  `plan stop`, `plan resume`, and `wait`.
- `/purser:reviewer`: `reviewer list` and `reviewer set`.
- `/purser:auto-compact`: `auto-compact`.
- `/purser:coder-effort`: `coder-effort`.
- `/purser:gear-up`: `gear up`.
- `/purser:gear-down`: `gear down`.

The `wait` grant also has an exact PowerShell form for the two Windows
binaries. These rules do not grant arbitrary executables or other Purser
subcommands.

Plugin layout and scoped MCP tool names follow the [Claude Code plugin
reference](https://code.claude.com/docs/en/plugins-reference). Local marketplace
registration follows the [marketplace guide](https://code.claude.com/docs/en/plugin-marketplaces).
