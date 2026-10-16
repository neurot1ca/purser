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

## Shipped in M2–M4

The PreToolUse hook publishes an identity handoff and then returns unchanged
`next(e)`, or denies if publication fails. The handoff is joined to the MCP call
by `_meta["claudecode/toolUseId"]` and contains the session ID captured by the
hook, tool, caller identity, cwd, and original arguments. The MCP server claims
that exact handoff before routing. A handoff may be claimed for 10 minutes;
handoff publication sweeps artifacts older than 1 hour. Missing, expired,
mismatched, duplicate, or already-claimed handoffs fail closed with an MCP
error. The server performs routing and applies the main-Claude-only acceptance
gate. The hook does not rewrite tool input or perform routing.

## Still unverified

The M1 probe and M2–M4 implementation did not verify subagent calls, parallel
calls within one session, an in-session MCP reconnect, or `/clear`. Those
behaviors remain unverified. The `--continue` probe showed that the MCP
environment session ID can be stale; the handoff therefore uses the session ID
captured by the hook. Local validation does not establish acceptance by the
Claude plugin directory service. No Codex, OpenCode, agy, or worker calls were
made for the M1 probe.
