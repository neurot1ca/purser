---
description: Set the Codex coder's thinking effort for new jobs (low, medium, high, xhigh), or auto to follow Purser's routes.
allowed-tools: Bash(*), AskUserQuestion
---

Resolve the bundled Purser binary from `${CLAUDE_PLUGIN_ROOT}/bin` as `hooks/register.ts` does. Use `uname -s -m`; map `MINGW*`, `MSYS*`, or `CYGWIN*` to `windows`, `Darwin` to `darwin`, and `Linux` to `linux`; map `x86_64` to `amd64` and `aarch64` to `arm64`; add `.exe` on Windows. Run:

```bash
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in MINGW*|MSYS*|CYGWIN*) os=windows ;; Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
suffix=""; [ "$os" = windows ] && suffix=".exe"
"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" coder-effort $ARGUMENTS --cwd "$PWD"
```

With a level, report the JSON acknowledgement in one line in the user's language. With no level, the command prints the current setting: ask one AskUserQuestion in the user's language, translated from "Which thinking effort should the Codex coder use for new jobs?", naming the current setting in the question, with the options auto (follow Purser's routes), medium, high and xhigh other than the current one, and saying that Other accepts low. Run the same command again with the chosen level and report its acknowledgement in one line.
