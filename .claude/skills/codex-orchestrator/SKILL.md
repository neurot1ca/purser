---
name: codex-orchestrator
description: Orchestrate bounded Codex work by deciding what to delegate, selecting a task route, crafting a decisive prompt, choosing continuation or a fresh thread, and independently reviewing the result. Use whenever Claude considers or makes a Codex delegation.
---

# Codex Orchestrator

`AGENTS.md`, `state/current.yaml`, and the active spec define authority. This
skill supplies orchestration judgment. `.claude/hooks/orchestrator/` supplies
deterministic mechanics. Delegation can use existing authority; it cannot create
authority.

Rare transport, recovery, and hook-diagnostic procedures are in
[`docs/ORCHESTRATOR_OPERATIONS.md`](../../../docs/ORCHESTRATOR_OPERATIONS.md), a
non-authoritative reference that is not loaded automatically.

## Delegate deliberately

Delegate work that is bounded, verifiable, and expressible as acceptance
criteria. The artifact type is not the boundary: implementation, repair,
analysis, extraction, investigation, schema work, migration, and a scoped API
probe can all qualify. A repeated check or reusable evidence should become a
written, tested instrument rather than a chain of ad-hoc commands.

Keep the work that only the orchestrator can do: frame the problem, decide what
would count as an answer, resolve authority, choose the route, review the result,
and adjudicate disagreement. Also keep bookkeeping whose delegation would cost
more than doing it directly.

Delegation is not handoff. A completion report is a claim, not a result. Review
every return. Treat review as a negotiation: independently verify a worker's
objection or refusal; if it stands, repair the instruction instead of restating
it more forcefully. Reject fluent answers that do not survive the same check.

## Route and dispatch

Classify the task before dispatch. Consequential domains include concurrency,
timing and clocks, state machines, economics and execution semantics, auth,
durability and recovery, causal evidence, fail-closed behavior, live/risk gates,
and subtle cross-module invariants. Residual design, not the domain label alone,
decides whether an implementation is resolved: a task is resolved only when an
accepted map already names the files, symbols, invariants, failure direction,
and behavior without leaving a semantic choice to the worker.

| Task class | Route | Model and effort |
|---|---|---|
| Mechanical, narrow, low-risk; status or counting | `mechanical` | `gpt-6-luna` + `high` |
| Narrow but semantically subtle, or fully resolved implementation | `subtle` | `gpt-6-luna` + `high` |
| Ordinary implementation, debugging, or multi-file work with design left | `normal` | `gpt-6-sol` + `medium` |
| Consequential and unresolved; diagnosis, design, or audit must find the rule | `consequential` | `gpt-6-sol` + `high` |
| Concrete escalation after a material miss, disagreement, or repeated failure | `escalated` | `gpt-6-sol` + `xhigh` |
| Established runbook operation: deploy, runner control, status, fetch, packaging | `operation` | `gpt-6-luna` + `high` |

Use the lowest row expected to finish correctly in one pass. Default to
`subtle` when the prompt states the decisions; `normal`, `consequential`, and
`escalated` need `basis=` naming the design choice left to the worker, and the
hook denies them without it. Do not use a
cheap-first cascade: an expected repair cycle also spends review. Do not raise
effort to compensate for a contradictory or unresolved prompt. A resolved
consequential implementation may use `subtle`, but its prompt must state the
accepted map and every invariant explicitly, and Claude still reviews it as
consequential work. `gpt-6-luna` + `xhigh` is not a route; move unresolved work
to `gpt-6-sol` instead.

Before each call, give the user one brief dispatch receipt: route, model, effort,
continuity (`reply`, `fresh`, or `failover-fresh`), one clause explaining the
choice, and the expected duration with what would count as a stall. Never pass
`sandbox` or `approval-policy`; runtime configuration owns capability. On a typed
quota or credit exhaustion response, fail over; do not perform proactive quota
routing.
After each Codex call, tell the user the bundle's `codex_quota` 5h and 7d used percentages and reset times when present.

## Protocol line

Claude writes the first prompt line exactly in this shape:

```text
ORCH|task=<stable-id>|phase=<implementation|operation>|route=<mechanical|subtle|normal|consequential|escalated|operation>[|reason=<new_task|continue|route_change|reanchor|repeated_failure|user_override>][|basis=<brief-basis>][|scope=<repo-relative-path[,repo-relative-path...]>]
```

Fields are `task`, `phase`, `route`, optional `reason`, `basis` required for
`normal`, `consequential`, and `escalated` and optional otherwise, and
`scope`. `task`, `phase`, and `route` are always required. `scope` is required
for `implementation`, must be bounded repository-relative paths, and must not be
the whole repository; omit it for `operation`. Claude chooses the route and
writes this line. The hook strips it, resolves model and effort from the route
table, and rejects malformed or inconsistent invocations. A recorded compatible
`codex-reply` may inherit the line.

## Craft the prompt

State a precise objective and observable acceptance criteria, labelled
`Objective:` and `Acceptance criteria:` (or `# Objective` / `# Acceptance
criteria` headings); without both labels the hook cannot ground the task
contract and forces FULL review. Include only
verified problem context, explicit constraints, and the files, symbols,
contracts, or evidence that materially narrow the task. Prefer behavior and
invariants over an unverified implementation. Exclude speculative redesign,
cleanup, refactors, and polish outside the criteria.

Before dispatch, resolve criteria that pull in opposite directions: state which
one wins and how the other degrades. Extra worker effort does not resolve an
unstated priority.

State the failure class as the requirement. Concrete failures are
non-exhaustive tests unless the domain genuinely requires a finite table; in
that case require the residual gap to be reported.

When the task depends on an unverified premise, make refusal the first
deliverable: test the premise before building; if it fails, stop and cite the
conflict and the smallest compliant alternative. Never leave only “comply” or
“invent” as the worker's choices.

When optional capabilities or independent lanes may be unavailable, declare one
fallback before dispatch: `sequential-parent`, `generic-agent`,
`partial-result`, or `fail-closed`. Require the report to name missing lanes and
the fallback used. Do not add this ritual to a single-lane task.

Add-ons are never automatic. When a user's project is UI/web/app or greenfield
feature work, suggest Ponytail (`addons/ponytail/`) once, inline; enable it for
that project only on the user's yes, and never for `consequential` or
`escalated` work.

## Review and acceptance

For mutations, review diff-first: inspect the actual delegated delta, then the
changed behavior and affected invariants, then only material callers, callees,
and boundaries. For read-only work, inspect the cited sources, method, and
result first. Expand from the smallest decisive evidence set only for a concrete
uncertainty, failed check, disagreement, dependency, or risk.

Verify attribution as well as outcome. When a worker claims its change caused an
effect, establish proportionally that the effect disappears without the new
code. A passing suite is consistent with redundant coverage; reapplying the
defect and observing the relevant test fail is the decisive regression check.

Reviewer-1 evidence is independent input, not acceptance. Consequential and
escalated work always receives Claude's full review. Request repair only for a
material defect, failed criterion, contract violation, or required validation
gap. After repair, review the new delta, the blocking finding, and newly affected
boundaries instead of restarting the whole review.

When a full suite is relevant, Claude runs it exactly once before acceptance.
Workers run focused validation and do not repeatedly run the full suite. Do not
repeat an expensive successful check unless the repair could invalidate it.

### One-turn AUDIT

When the hook emits `claude_review_mode: AUDIT`, use the consolidated bundle in
one Claude turn: check the task contract, reviewer result or eligible skip,
worker validation, delta summary, and safety fields; then accept and commit if
the evidence is sufficient and commit authority exists, or decline with the
specific gap. Do not reopen raw diffs or exploratory shell review without a
concrete inconsistency. If Reviewer-1 requires changes, send its exact findings
back through the existing thread; do not edit the worker's code yourself.

Close every review with exactly one typed verdict and name the evidence that
decides it:

- `VERIFIED` — the claimed effect was observed, not merely reported.
- `NOT VERIFIED` — the claim failed or the evidence contradicts it.
- `INCONCLUSIVE` — the available check could not decide; this is not acceptance.

Proceeding after `INCONCLUSIVE` is a user decision. Never round it up to success.

## Thread choice and bounded completion

Continue with `codex-reply` for an ordinary follow-up or repair while the worker
is available and the diagnosis, task identity, route, model, and effort still
hold. Start a fresh thread for failover, a stale diagnosis, a material P0/P1
error where anchoring is plausible, repeated repair failure, or a justified
model or effort change. Do not open a fresh thread merely to simulate
independence.

For an autonomously authorized bounded objective: initial implementation, at
most two ordinary material repairs, then at most one justified escalated repair.
Stop earlier when the criteria are met, a human decision or material scope
expansion is required, authority is ambiguous, infrastructure blocks progress,
or repairs stop making material progress. Do not loop for style, optional
optimization, speculative cleanup, or confidence without a concrete risk.

If stopping leaves a real residual risk, tell the user what it is, why it fell
outside the loop, and what addressing it would cost. Silent-failure risks must
never be deferred without the user's knowledge.
