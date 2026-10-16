---
description: Set the Codex coder's thinking effort for new jobs (low, medium, high, xhigh), or auto to follow Purser's routes.
allowed-tools:
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" coder-effort *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" coder-effort *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" coder-effort *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" coder-effort *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" coder-effort *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" coder-effort *)'
  - AskUserQuestion
---

Choose the concrete bundled binary for the current OS and architecture and invoke its quoted path directly: `purser-hook-windows-amd64.exe`, `purser-hook-windows-arm64.exe`, `purser-hook-darwin-amd64`, `purser-hook-darwin-arm64`, `purser-hook-linux-amd64`, or `purser-hook-linux-arm64`. Run `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" coder-effort <arguments> --cwd "$PWD"`.

With a level, report the JSON acknowledgement in one line in the user's language. With no level, the command prints the current setting: ask one AskUserQuestion in the user's language, translated from "Which thinking effort should the Codex coder use for new jobs?", naming the current setting in the question, with the options auto (follow Purser's routes), medium, high and xhigh other than the current one, and saying that Other accepts low. Run the same command again with the chosen level and report its acknowledgement in one line.
