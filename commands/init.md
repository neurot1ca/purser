---
description: Set up Purser roles and optional project trust and profile.
allowed-tools: Bash(python *)
---

1. Run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/purser_init.py" --detect --json` in the current project. Show which agents are installed and the current roles, trust, and profile state.
2. Tell the user that Claude Code is the orchestrator. Ask them to choose the coding agent, Codex or Claude, and show which of those are installed.
3. Ask whether another agent is available. If `agy` is detected, ask whether to use it as the independent reviewer. Otherwise choose `reviewer = "none"`. Explain: without an independent reviewer, every delegated call gets FULL review.
4. Ask whether to trust the current project and whether to scaffold `.orchestrator/profile.toml`.
5. Run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/purser_init.py" --plan --coder X --reviewer Y` with `--trust` and `--scaffold-profile` only if chosen. Show the entire plan. Ask for explicit yes to this exact plan. Only after that yes, run the same options with `--apply` instead of `--plan`.
6. Run `/purser:check` and show its result.

The plan shows the Python 3.11+ interpreter path that `--apply` stores in `~/.orchestrator/python` for plugin launches.

Never edit `~/.orchestrator/config.toml` or `.orchestrator/profile.toml` by hand. If detection or planning fails, show the error and stop setup.
