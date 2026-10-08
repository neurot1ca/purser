---
description: Find unused plugins, skills and MCP servers and other token waste in your recent sessions.
allowed-tools: Bash(*)
---

Resolve and run the bundled Purser binary as in `commands/gear-up.md`, using the `optimize` subcommand:

```bash
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in MINGW*|MSYS*|CYGWIN*) os=windows ;; Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
suffix=""; [ "$os" = windows ] && suffix=".exe"
"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" optimize
```

Translate its English output into the user's language. Preserve the Markdown structure and table, numbers, names, ⚓, and bold formatting exactly. Then give at most one recommendation in the user's language. The report is read-only; never change settings, files, or plugins without the user's explicit yes.

## Maintenance report

When the user asks to run the project cleanup report, make sure the project is opted in first. If it is not, ask for consent before enabling it; an API-billed scan requires the user's explicit yes. After consent, run `"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" maintenance opt-in --repo <repository-root>` if needed, then run `"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" maintenance run cleanup --repo <repository-root>` and show the resulting report. If the user asks to turn the module off, run `"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" maintenance opt-out --repo <repository-root>`.
