---
description: Choose Reviewer-1 from the agents installed on this machine, or turn it off.
allowed-tools: Bash(*), AskUserQuestion
---

Resolve the bundled Purser binary from `${CLAUDE_PLUGIN_ROOT}/bin` as `hooks/register.ts` does and list the choices:

```bash
os="$(uname -s)"
arch="$(uname -m)"
case "$os" in MINGW*|MSYS*|CYGWIN*) os=windows ;; Darwin) os=darwin ;; Linux) os=linux ;; *) echo "Unsupported OS: $os" >&2; exit 1 ;; esac
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; amd64|arm64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1 ;; esac
suffix=""; [ "$os" = windows ] && suffix=".exe"
"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-${os}-${arch}${suffix}" reviewer list --cwd "$PWD" --json
```

Ask one AskUserQuestion in the user's language, translated from: "Which agent should review the coder's work?" Name the `current` reviewer in the question and offer every other entry of `choices` as an option labelled with its agent, model and effort (at most four options), and label `{agent: "none"}` as "Off: no independent review; Claude reviews alone." Say in the question that Other accepts an agent or model name: "Other: type an agent or model; Purser uses it if it runs through Codex, Claude, agy or OpenCode."

Run the same binary with `reviewer set --cwd "$PWD" --agent <agent> [--model <model>] [--effort <effort>]` for the chosen entry. For Other, map the typed name to one of codex, claude, agy or opencode (an OpenCode `provider/model` id when the model runs through OpenCode) and set it; if none fits, set nothing and tell the user Purser does not support it yet. Report the JSON acknowledgement in one line in the user's language. Change nothing else.
