---
description: Choose Reviewer from the agents installed on this machine, or turn it off.
allowed-tools:
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-amd64.exe" reviewer set *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-windows-arm64.exe" reviewer set *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-amd64" reviewer set *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-darwin-arm64" reviewer set *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-amd64" reviewer set *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" reviewer list *)'
  - 'Bash("${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-linux-arm64" reviewer set *)'
  - AskUserQuestion
---

Choose the concrete bundled binary for the current OS and architecture and invoke its quoted path directly: `purser-hook-windows-amd64.exe`, `purser-hook-windows-arm64.exe`, `purser-hook-darwin-amd64`, `purser-hook-darwin-arm64`, `purser-hook-linux-amd64`, or `purser-hook-linux-arm64`. List the choices with `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" reviewer list --cwd "$PWD" --json`.

Ask one AskUserQuestion in the user's language, translated from: "Which agent should review the coder's work?" Name the `current` reviewer in the question and offer every other entry of `choices` as an option labelled with its agent, model and effort (at most four options), and label `{agent: "none"}` as "Off: no independent review; Claude reviews alone." Say in the question that Other accepts an agent or model name: "Other: type an agent or model; Purser uses it if it runs through Codex, Claude, agy or OpenCode."

Run the same binary with `"${CLAUDE_PLUGIN_ROOT}/bin/<selected-binary>" reviewer set --cwd "$PWD" --agent <agent> [--model <model>] [--effort <effort>]` for the chosen entry. For Other, map the typed name to one of codex, claude, agy or opencode (an OpenCode `provider/model` id when the model runs through OpenCode) and set it; if none fits, set nothing and tell the user Purser does not support it yet. Report the JSON acknowledgement in one line in the user's language, appending this italic text on that same line when it has `"scope": "project"`: EN *Reviewer saved for this project only.* / TR *Reviewer yalnızca bu proje için kaydedildi.* Change nothing else.
