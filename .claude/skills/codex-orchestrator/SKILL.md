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

Before each call, give one brief user receipt: route, model, effort, continuity
(`reply`, `fresh`, or `failover-fresh`), reason, expected duration, and stall
threshold. Runtime configuration owns capability; never pass `sandbox` or
`approval-policy`. Fail over on typed quota/credit exhaustion, never route
proactively by quota. After each call, report bundle model and effort plus
`codex_quota` 5h/7d used percentages and reset times when present.

Format each as one bold line between `---` rules in the user's language, with
a blank line before and after the bold line (otherwise Markdown turns it into a
heading); explain below in plain text. Put the dispatch receipt in the reply
that ends the turn after `codex_start`, not in text before the tool call (the
desktop app can hide that). If needed, see [examples](references/receipt_examples.md).

## Protocol line

Claude writes the first prompt line exactly in this shape:

```text
ORCH|task=<stable-id>|phase=<implementation|operation>|route=<mechanical|subtle|normal|consequential|escalated|operation>[|reason=<new_task|continue|route_change|reanchor|repeated_failure|user_override>][|basis=<brief-basis>][|scope=<repo-relative-path[,repo-relative-path...]>][|eta=<N>m]
```

`task`, `phase`, and `route` are required; `reason` optional. `basis` is
required for `normal`, `consequential`, and `escalated`, optional otherwise.
`scope` is required for `implementation`: bounded repository-relative paths,
never the whole repository; omit for `operation`. Claude writes the line and
route; the hook strips it, resolves model/effort, and rejects malformed or
inconsistent calls. Compatible recorded `codex-reply` may inherit.

Put estimated duration in `eta` beside the recorded actual duration.

### Async by default

Use the SessionStart duration hint for `eta` when present. Dispatch every call
with `codex_start`; for continuation use `mode=reply`, `threadId`, the same ORCH
`task`, and no `cwd`. Use `codex`/`codex-reply` only if `codex_start` is
unavailable. Run the exact command returned by start with the shell background
option, end the turn, and keep talking with the user. Once the waiter finishes,
call `codex_result` once with `jobId` and review as synchronous output. Never
call it before the waiter finishes; use `codex_cancel` only at user request.

## Craft the prompt

Label a precise objective and observable criteria `Objective:` and `Acceptance
criteria:` (or corresponding `#` headings); missing labels force FULL review
because the hook cannot ground the contract. Include verified context,
constraints, and only files, symbols, contracts, or evidence that narrow the
task. Prefer behavior and invariants to unverified implementation. Exclude
speculative redesign, cleanup, refactors, and polish outside the criteria.

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
