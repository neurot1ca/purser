---
name: codex-orchestrator
description: Orchestrate bounded Codex work by deciding what to delegate, selecting a task route, crafting a decisive prompt, choosing continuation or a fresh thread, and independently reviewing the result. Use whenever Claude considers or makes a Codex delegation.
---

# Codex Orchestrator

`AGENTS.md`, `state/current.yaml`, and the active spec define authority. This
skill supplies judgment; `.claude/hooks/orchestrator/` supplies mechanics.
Delegation cannot create authority.

For transport, recovery, or hook diagnosis, see non-authoritative
[`operations guide`](../../../docs/ORCHESTRATOR_OPERATIONS.md).

Language: user's language only in user replies; English in worker prompts and
reports, commits, state, and repository documents. Other languages cost more
tokens in every bundle.

Replies: at most 5 lines unless asked for detail. Plain friendly sentences:
result, one recommendation, at most one question; no tables or headings unless
asked, no retelling. Replies remain in context.

## Delegate deliberately

Delegate bounded, verifiable work with acceptance criteria, regardless of
artifact type: implementation, repair, analysis, extraction, investigation,
schema work, migration, or scoped API probe. Turn repeated checks or reusable
evidence into written, tested instruments, not ad-hoc commands.

Keep orchestrator-only work: framing, answer criteria, authority, routing,
review, and disagreement. Do bookkeeping directly when delegation costs more.

For code, transcript, or log exploration needing 3+ reads, see
[conditional delegation](references/conditional_delegation.md).

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
| Ordinary implementation, debugging, or multi-file work with design left | `normal` | `gpt-6-sol` + `medium` |
| Consequential and unresolved; diagnosis, design, or audit must find the rule | `consequential` | `gpt-6-sol` + `high` |
| Concrete escalation after a material miss, disagreement, or repeated failure | `escalated` | `gpt-6-sol` + `xhigh` |
| Established runbook operation: deploy, runner control, status, fetch, packaging | `operation` | `gpt-6-luna` + `high` |

Choose the lowest row likely to succeed in one pass; default to `subtle` when
decisions are stated. `normal`, `consequential`, and `escalated` require `basis=`
naming the remaining design choice; the hook denies omission. Avoid cheap-first
cascades: repair costs review. Effort cannot fix contradictory or unresolved
prompts. Resolved consequential implementation may use `subtle` only with the
accepted map and every invariant explicit; Claude still reviews it as
consequential. `gpt-6-luna` + `xhigh` is invalid; unresolved work uses `gpt-6-sol`.

Runtime configuration owns capability; never pass `sandbox` or
`approval-policy`. Fail over on typed quota/credit exhaustion, never route
proactively by quota. After each call, report bundle model and effort plus
`codex_quota` 5h/7d used percentages and reset times when present; show the
5h reset as time remaining (e.g. "resets in 1h 20m").
Format the completion receipt as one bold line between `---` rules, with blank
lines around it, in the user's language; explain below in plain text.
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
milestones as separate async `codex_start` calls only when the longest job plus
fan-in review is clearly shorter than serial work. Include shared generated
files and validation inputs when judging disjointness. Overlapping milestones
may also be started; the conflict queue serializes them, and a reply on a
conflicting scope is still denied. Review each result on its own delta;
otherwise run disjoint work sequentially.

Use the SessionStart duration hint for `eta` when present. Dispatch every call
with `codex_start`; for continuation use `mode=reply`, `threadId`, the same ORCH
`task`, and no `cwd`. Use `codex`/`codex-reply` only if `codex_start` is
unavailable. Run the exact command returned by start with the shell background
option (use the PowerShell tool on Windows), end the turn, and keep talking with the user. Once the waiter finishes,
call `codex_result` once with `jobId` and review as synchronous output. Never
call it before the waiter finishes; use `codex_cancel` only at user request. After
resolving conflicts by hand and committing, call `codex_apply` with `resolved: true`.

When an accepted part of an approved multi-part task finishes, start the next
part in the same turn without asking. Stop only for approval-gated actions,
real decisions, failed review, or residual risk the user must weigh. Quota is
not a reason to stop unless 5h or 7d remaining is at most 10%; then warn
before the task and let the user choose.

With several jobs, keep a list of finished results the user has not yet
decided on. Present them one at a time in finish order, and do not move to the
next one or to new work until the user has answered on the current one.

## Craft the prompt

For implementation, plan, and investigation calls, run `node "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js" mcp "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/code_map.py" --repo <project root>` outside the hook before delegation and paste its one `CODE_MAP=... STATUS=...` line into the worker prompt.
Treat the map as navigation only; workers verify current source before acting.

Label a precise objective and observable criteria `Objective:` and `Acceptance
criteria:` (or corresponding `#` headings); missing labels force FULL review
because the hook cannot ground the contract. Keep the ORCH line and bounded scope.

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

For conflicting criteria, see [conditional delegation](references/conditional_delegation.md).

State the failure class as the requirement. Concrete failures are
non-exhaustive tests unless the domain genuinely requires a finite table;
then require the residual gap to be reported.

For unverified premises, see [conditional delegation](references/conditional_delegation.md).

For possibly missing optional lanes, see [conditional delegation](references/conditional_delegation.md).

For UI/web/app or greenfield feature work, see [Ponytail guidance](references/conditional_delegation.md).

## Review and acceptance

For mutations, review the delta, then changed behavior and invariants, then
material callers, callees, and boundaries. For read-only work, start with cited
sources, method, and result. Expand evidence only for concrete
uncertainty, failed checks, disagreement, dependency, or risk.

For a worker's causal claim, see [attribution verification](references/conditional_review.md).

Reviewer-1 is independent input, not acceptance. Claude fully reviews all
consequential and escalated work. Request repair only for a material defect,
failed criterion, contract violation, or required validation gap. After repair,
review the new delta, blocking finding, and newly affected boundaries.

When relevant, Claude runs the full suite once before acceptance. Workers run
focused validation, never repeated full suites. Repeat an expensive successful
check only if repair could invalidate it.

### One-turn AUDIT

On `claude_review_mode: AUDIT`, use one Claude turn to check the bundle's task
contract, reviewer result or eligible skip, worker validation, delta summary,
and safety fields. Accept and commit only with sufficient evidence and commit
authority; otherwise decline with the specific gap. Reopen raw diffs or shell
review only for concrete inconsistency. Send Reviewer-1's required changes
through the existing thread; do not edit worker code yourself.

Close every review with exactly one typed verdict and name the evidence that
decides it:

- `VERIFIED` — the claimed effect was observed, not merely reported.
- `NOT VERIFIED` — the claim failed or the evidence contradicts it.
- `INCONCLUSIVE` — the available check could not decide; this is not acceptance.

Proceeding after `INCONCLUSIVE` is a user decision. Never round it up to success.

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

For real residual risk at stopping, see [conditional review](references/conditional_review.md).
