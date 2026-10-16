---
description: Find unused plugins, skills and MCP servers and other token waste in your recent sessions.
allowed-tools:
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" maintenance opt-out *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" maintenance opt-out *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" maintenance opt-out *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" maintenance opt-out *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" maintenance opt-out *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" optimize)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" maintenance opt-in *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" maintenance run cleanup *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" maintenance opt-out *)'
---

Choose the concrete bundled binary for the current OS and architecture and invoke its quoted path directly: `purser-hook-windows-amd64.exe`, `purser-hook-windows-arm64.exe`, `purser-hook-darwin-amd64`, `purser-hook-darwin-arm64`, `purser-hook-linux-amd64`, or `purser-hook-linux-arm64`. Run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" optimize`.

Translate its English output into the user's language. Preserve the Markdown structure and table, numbers, names, ⚓, and bold formatting exactly. Then give at most one recommendation in the user's language. The report is read-only; never change settings, files, or plugins without the user's explicit yes.

## Maintenance report

When the user asks to run the project cleanup report, make sure the project is opted in first. If it is not, ask for consent before enabling it; an API-billed scan requires the user's explicit yes. After consent, run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" maintenance opt-in --repo <repository-root>` if needed, then run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" maintenance run cleanup --repo <repository-root>` and show the resulting report. If the user asks to turn the module off, run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" maintenance opt-out --repo <repository-root>`.
