---
description: Turn Purser's automatic /compact on or off for all projects.
allowed-tools: Bash(*)
---

Resolve the bundled Purser binary from `${CLAUDE_PLUGIN_ROOT}/bin` as `hooks/register.ts` does. Use `uname -s -m`; map `MINGW*`, `MSYS*`, or `CYGWIN*` to `windows`, `Darwin` to `darwin`, and `Linux` to `linux`; map `x86_64` to `amd64` and `aarch64` to `arm64`; add `.exe` on Windows. Run `auto-compact $ARGUMENTS`, using `status` when there is no argument:

```bash
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in MINGW*|MSYS*|CYGWIN*) os=windows ;; Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
suffix=""; [ "$os" = windows ] && suffix=".exe"
args="${ARGUMENTS:-status}"
"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" auto-compact $args
```

Report the JSON acknowledgement in one line in the user's language.
