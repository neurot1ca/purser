# Delegation playbook

## Delegate deliberately

Delegate bounded, verifiable work with acceptance criteria, regardless of
artifact type: implementation, repair, analysis, extraction, investigation,
schema work, migration, or scoped API probe. Turn repeated checks or reusable
evidence into written, tested instruments, not ad-hoc commands.

Keep orchestrator-only work: framing, answer criteria, authority, routing,
review, and disagreement. Do bookkeeping directly when delegation costs more.

For code, transcript, or log exploration needing 3+ reads, see
[conditional delegation](conditional_delegation.md).

Review every completion claim. Verify objections or refusals independently;
repair valid ones in the instruction. Reject fluent answers that fail review.

## Route and dispatch

Classify before dispatch. Consequential domains include concurrency, timing and
clocks, state machines, economics and execution, auth, durability and recovery,
causal evidence, fail-closed behavior, live/risk gates, and subtle cross-module
invariants. A domain label alone does not decide whether design is resolved.
An accepted map must name files, symbols, invariants, failure direction, and
behavior, leaving no semantic choice to the worker.

| Task class | Route | Model and effort |
|---|---|---|
| Mechanical, narrow, low-risk; status or counting | `mechanical` | `gpt-6-luna` + `high` |
| Narrow but semantically subtle, or fully resolved implementation | `subtle` | `gpt-6-luna` + `high` |
| Ordinary implementation, debugging, or multi-file work with design left | `normal` | `gpt-6.1-sol` + `medium` |
| Consequential and unresolved; diagnosis, design, or audit must find the rule | `consequential` | `gpt-6.1-sol` + `high` |
| Concrete escalation after a material miss, disagreement, or repeated failure | `escalated` | `gpt-6.1-sol` + `xhigh` |
| Established runbook operation: deploy, runner control, status, fetch, packaging | `operation` | `gpt-6-luna` + `high` |

With Claude as coder, the same routes run Haiku (mechanical, operation), Sonnet medium (subtle, normal), Opus high (consequential, plan) and Opus xhigh (escalated), always the newest model of each tier.

Research/study jobs (source studies, docs research, analyses) use `route=normal` with basis `Research study: <question>`.

Choose the lowest row likely to succeed in one pass; default to `subtle` when
decisions are stated. `normal`, `consequential`, and `escalated` require `basis=`
naming the remaining design choice; the hook denies omission. Avoid cheap-first
cascades: repair costs review. Effort cannot fix contradictory or unresolved
prompts. Resolved consequential implementation may use `subtle` only with the
accepted map and every invariant explicit; Claude still reviews it as
consequential. `gpt-6-luna` + `xhigh` is invalid; unresolved work uses `gpt-6.1-sol`.

### Dispatch through `purser:dispatcher`

Every implementation and operation job runs through a `purser:dispatcher`
agent: start one fresh dispatcher (a new Agent call) per job, after main
Claude fixes the design, route, scope, authority, and acceptance criteria.
Pass the exact worker prompt, including its `ORCH` line, and the exact tool
arguments. Keep dispatcher instructions outside the worker prompt. The
dispatcher runs in the background; keep talking with the user meanwhile.

Main Claude keeps routing, semantic review, disagreement resolution,
acceptance, and commits. Review from the receipt and the bundle files it
names. When `worker_validation.source` is `HOST_TEST_EVIDENCE` with status
PASS, the host saw the worker's test runs pass: do not rerun those focused
tests; rerun only when that evidence is missing, UNKNOWN or FAIL. To accept, call `codex_apply` with `jobId`, `acceptOnly: true` and
`revision` set to the receipt's `revision` (the bundle's
`changed_paths_sha256`); Purser records it only from the main thread. Then
send the dispatcher one message naming the job and revision; it applies that
revision and reports its receipt. A subagent apply without a matching
acceptance, or after the worktree drifted, is refused. A repair belongs to its
original job's dispatcher; never replace a dispatcher mid-lifecycle.

Runtime configuration owns capability; never pass `sandbox` or
`approval-policy`. Fail over on typed quota/credit exhaustion, never route
proactively by quota. After each call, report bundle model and effort plus
`codex_quota` 5h/7d used percentages and reset times when present; show the
5h reset as time remaining (e.g. "resets in 1h 20m") and the 7d reset as
time remaining (e.g. "resets in 3d 2h", or "resets in 6h 37m" when under a day).
Format the completion receipt as one line `> ⚓ **Codex done: ...**` in the
user's language; explain below in plain text.
Pass `label` on every `codex_start`, `codex`, and `codex-reply` call: a short
human description of the task in the user's language, at most 80 characters.
A reply may omit it only when the existing thread has a saved label. Purser
emits the dispatch receipt; Claude must not write one.

## Protocol line

Claude writes the first prompt line exactly in this shape:

```text
ORCH|task=<stable-id>|phase=<implementation|operation|plan>|route=<mechanical|subtle|normal|consequential|escalated|operation|plan_standard>[|reason=<new_task|continue|route_change|reanchor|repeated_failure|user_override>][|basis=<brief-basis>][|scope=<repo-relative-path[,repo-relative-path...]>][|eta=<N>m]
```

`task`, `phase`, and `route` are required; `reason` optional. `basis` is
required for `normal`, `consequential`, and `escalated`, optional otherwise.
`scope` is required for `implementation`: bounded repository-relative paths,
never the whole repository; omit for `operation`, optional for `plan`.
`phase=plan` pairs only with `route=plan_standard` (read-only; see `/purser:plan`). Claude writes the line and
route; the hook strips it, resolves model/effort, and rejects malformed or
inconsistent calls. Compatible recorded `codex-reply` may inherit.

Put estimated duration in `eta` beside the recorded actual duration.

### Async by default

Treat plan paths as estimates. Claude may start several accepted, disjoint
milestones as separate dispatchers only when the longest job plus
fan-in review is clearly shorter than serial work. Include shared generated
files and validation inputs when judging disjointness. For worktree jobs,
dispatch overlapping paths immediately: Purser queues hot-path conflicts as
`QUEUED` and auto-starts them on lease release; otherwise jobs run in parallel
worktrees and resolve overlap at apply. Wait only for a logical dependency,
such as a milestone needing the previous milestone's accepted result; a reply
on a conflicting scope is still denied. Review each result on its own delta;
otherwise run disjoint work sequentially.

Use the SessionStart duration hint for `eta` when present. The dispatcher
starts every job with `codex_start`; for continuation pass `mode=reply`,
`threadId`, the same ORCH `task`, and no `cwd`. Use `codex_cancel` only at user
request. After `codex_apply` reports a replay conflict, main Claude resolves and
commits the files by hand, then calls `codex_apply` with the same `jobId` and
`resolved: true`. This records the hand-applied job as accepted and releases
its scope lease. When main Claude applies an accepted job by hand for any other
reason (stale apply, failed finalization), it calls `codex_apply` with that
`jobId` after committing: the files already match the worktree, so the job is
recorded as applied and its lease is released. Never `codex_cancel` a job
that was applied; cancelling discards it.

When an accepted part of an approved multi-part task finishes, start the next
part in the same turn without asking. Stop only for approval-gated actions,
real decisions, failed review, or residual risk the user must weigh. Quota is
not a reason to stop unless 5h or 7d remaining is at most 10%; then warn
before the task and let the user choose.

With several jobs, keep a list of finished results the user has not yet
decided on. Present them one at a time in finish order, and do not move to the
next one or to new work until the user has answered on the current one.

## Craft the prompt

For implementation and plan calls, the pre-hook adds exactly one `CODE_MAP=<path> STATUS=<FRESH|STALE|UNAVAILABLE>` line automatically. Do not generate or add this line by hand. The bounded refresh reuses a fresh map and leaves a stale map in place when refresh is not quick; map generation failure never blocks dispatch. An async pre-hook internal failure denies the call. The map is navigation only: workers verify current source and open relevant sections on demand. Operation calls do not receive a code map.
Write worker prompts with an `Objective:` line and an `Acceptance criteria:`
list; missing labels force FULL review because the hook cannot ground the
contract. Keep the ORCH line and bounded scope.

- Give verified task context, resolved behavior, invariants, failure direction,
  and only necessary exact examples or schemas.
- Rely on the injected `AGENTS.md` for stable repo rules; state only
  task-specific boundaries and uncertain authority.
- Name the smallest authoritative source set and the affected boundary; let
  Codex inspect relevant callers, tests, and contracts.
- Omit procedural implementation or read lists unless order or exact format is
  contractual; exclude speculative redesign, cleanup, and polish.
- Give one completion condition covering deliverable, scoped verification, and
  residual gaps; keep `VALIDATION:` where required.
- Never shorten exact labels, literal output schemas, required validation, or
  task-specific safety gates.
- For execution tasks, do not request an upfront plan, preamble, or status
  narration; ask for a plan only when the plan is the deliverable.

For conflicting criteria, see [conditional delegation](conditional_delegation.md).

State the failure class as the requirement. Concrete failures are
non-exhaustive tests unless the domain genuinely requires a finite table;
then require the residual gap to be reported.

For unverified premises, see [conditional delegation](conditional_delegation.md).

For possibly missing optional lanes, see [conditional delegation](conditional_delegation.md).

For UI/web/app or greenfield feature work, see [Ponytail guidance](conditional_delegation.md).

## Thread choice and bounded completion

Use `codex-reply` for follow-up or repair while the worker, diagnosis, task
identity, route, model, and effort still hold. Start fresh for failover, stale
diagnosis, material P0/P1 error with plausible anchoring, repeated repair
failure, or justified model/effort change; never merely to simulate independence.

For authorized bounded work: initial implementation, at most two ordinary
material repairs, then at most one justified escalated repair. Stop earlier
when criteria are met, human decision or material scope expansion is needed,
authority is ambiguous, infrastructure blocks progress, or repairs cease
material progress. Do not loop for style, optional optimization, speculative
cleanup, or confidence without concrete risk.

For real residual risk at stopping, see [conditional review](conditional_review.md).
