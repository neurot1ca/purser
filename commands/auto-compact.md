---
description: Turn Purser's automatic /compact on or off for all projects.
allowed-tools:
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" auto-compact *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" auto-compact *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" auto-compact *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" auto-compact *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" auto-compact *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" auto-compact *)'
---

Choose the concrete bundled binary for the current OS and architecture and invoke its quoted path directly: `purser-hook-windows-amd64.exe`, `purser-hook-windows-arm64.exe`, `purser-hook-darwin-amd64`, `purser-hook-darwin-arm64`, `purser-hook-linux-amd64`, or `purser-hook-linux-arm64`. Run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" auto-compact <arguments>`, using `status` when there is no argument.

Report the JSON acknowledgement in one line in the user's language.
