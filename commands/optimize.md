---
description: Find unused plugins, skills and MCP servers and other token waste in your recent sessions.
allowed-tools: Bash(python *)
---

Run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/purser_optimize.py"` and translate its English output into the user's language. Preserve the Markdown structure and table, numbers, names, ⚓, and bold formatting exactly. Then give at most one recommendation in the user's language. Never change settings, files, or plugins without the user's explicit yes.

## Maintenance report

When the user asks to run the project cleanup report, make sure the project is opted in first. If it is not, ask for consent before enabling it; an API-billed scan requires the user's explicit yes. After consent, run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/maintenance_reports.py" opt-in --repo <repository-root>` if needed, then run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/maintenance_reports.py" run cleanup --repo <repository-root>` and show the resulting report. If the user asks to turn the module off, run `python "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/maintenance_reports.py" opt-out --repo <repository-root>`.
