# MCP enforcement migration: M1 evidence

2026-10-17, Windows amd64, Claude Code **2.1.300**. The authenticated live
probe used the default configuration read-only with `--setting-sources ''`,
the fixture plugin, Haiku, low effort, `dontAsk`, and strict MCP configuration.
All six runs exited 0. Raw evidence remains outside the repository.

## Result

The host exposes a reliable per-call join key:

```text
MCP params._meta["claudecode/toolUseId"] == classic PreToolUse tool_use_id
```

This joined exactly in **6/6** calls: the main call, two concurrent
identical-argument sessions in the same cwd, a second session, explicit
`--resume`, and `--continue`. The MCP records also retained the original
arguments, cwd, and request metadata. The join is by Claude's tool-use ID;
JSON-RPC IDs, argument hashes, timestamps, and latest-session discovery are not
substitutes.

`$.session.id()` from the hook matched MCP `CLAUDE_CODE_SESSION_ID` in 5/6
runs. Under `--continue`, the hook session was
`0913888a…` while the MCP environment contained `f9be46bd…`. Therefore the
handoff must use the session ID captured from the hook together with the
tool-use ID; the MCP environment session value is only a consistency signal.

## Scope and open checks

The following were not exercised by these `-p` runs and remain checks for
M2/M3/M5, rather than M1 blockers: a subagent call; two parallel calls inside
one session; in-process MCP reconnect; `/clear`; and the Bash permission matrix
(exact binary/subcommand, quoted paths with spaces, unrelated binary, another
subcommand, and appended `&&`/`;`). Each `-p` run started a distinct MCP
process, so those lifecycle cases require an interactive or SDK session.

The `--continue` environment mismatch is the observed reason not to key a
handoff by environment session ID. Main/subagent caller identity and per-call
cwd/arguments still need the remaining lifecycle checks. No Codex, OpenCode,
agy, or worker calls were made.

The fixture plugin passed:

```text
claude plugin validate <fixture/plugin> --strict --json
```

The directory validator was run locally; no directory-service acceptance check
was available. Windows launchers used hidden child processes. Cross-platform
environment propagation, path rendering, file locking, and antivirus sharing
behavior remain M2 checks.

## Decision

The exact hook-to-MCP correlation contract is demonstrated. The migration may
proceed to M2. Preserve the hook session ID and the exact
`_meta["claudecode/toolUseId"]` in the durable handoff, and fail closed on a
missing, duplicate, or mismatched pair.
