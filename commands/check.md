---
description: Check whether Purser is installed and usable.
allowed-tools:
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" check)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" check)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" check)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" check)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" check)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" check)'
---

Run the bundled Purser binary and show its output as-is:

Choose the concrete bundled binary for the current OS and architecture and invoke its quoted path directly: `purser-hook-windows-amd64.exe`, `purser-hook-windows-arm64.exe`, `purser-hook-darwin-amd64`, `purser-hook-darwin-arm64`, `purser-hook-linux-amd64`, or `purser-hook-linux-arm64`. Run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" check`.

Do not change settings, files, or installations, and do not fix anything automatically.
