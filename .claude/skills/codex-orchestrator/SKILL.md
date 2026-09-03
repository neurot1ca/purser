---
name: codex-orchestrator
description: Delegate bounded, verifiable work to codex-main or codex-b - implementation, repair, analysis, extraction, investigation - choose Codex model and reasoning effort, handle worker failover, reconcile the worktree, and independently review and negotiate the result. Use whenever Claude hands a task to Codex, and to decide whether a task should be handed over at all.
---

# Codex Orchestrator

`AGENTS.md` is the repository operating contract. This skill governs only
Claude-to-Codex orchestration and can neither expand nor replace repository
authority.

## One copy

This file is the only copy. After any edit, reload it in-session; a loaded skill
does not update automatically.

## Role & Tripartite Architecture (Sol / Sentinel / Luna)

- **Sol (Claude)**: The reasoning master, task framer, orchestrator, and final adjudicator. Sol receives compact 1-3K task packets, never executes direct code edits, and adjudicates verification receipts.
- **Sentinel (Antigravity / Gemini 3.7 Flash High)**: The scout and quality gatekeeper. Performs pre-task AST/context reconnaissance and post-task adversarial review.
- **Luna (Codex / GPT-5.6 Sol/Luna)**: The execution worker. Writes code, executes test suites, and resolves mechanical errors.

Unless the user explicitly asks Claude itself to edit code, delegate all repository code changes to `codex-main`.

## Token Economy & Tool Batching Mandate (Priority 0 & 2)

Because context re-read cost is approximately linear in assistant turns (~300K tokens per request):

1. **Tool Batching (Priority 0 - Master Lever):**
   - Whenever planning independent inspections, reads, or status checks, Claude **MUST batch 2+ tool calls in a single turn** (e.g. `[Read(fileA), Read(fileB)]` or `[Grep(pattern), Bash(git status)]`).
   - Never issue single, sequential read/status calls when they can be dispatched concurrently.

2. **Targeted Read Discipline (Priority 2):**
   - Never `cat` or read whole multi-hundred-line files through Bash or Read.
   - Prefer single-turn `grep -n -C <N>` (or `rg -n -C 5`) which returns surrounding lines and context in the **same call** without needing a second turn.
   - Reserve serial `Grep` -> `Read(offset, limit)` strictly for wide multi-page windows, and batch the locate step with other independent work.

3. **Post-Task Compaction:**
   - Once a task is accepted and committed, issue `/compact` before starting the next objective to clear expired execution context.

## The hook layer

Since 2026-08-29 part of this skill is enforced by code rather than by memory.
`.claude/hooks/orchestrator/` runs on `PreToolUse`, `PostToolUse` and
`PostToolUseFailure` for the Codex MCP tools. It parses a protocol line, sets
model and effort from the route table, guards thread reuse, captures the
worktree baseline, computes the delegated delta, runs Reviewer-1 when the gate
opens, and replaces the tool result with a compact bundle.

Three layers, and nothing is duplicated between them:

```
AGENTS / state / spec   authority
SKILL.md                judgment
hooks/orchestrator      deterministic mechanics
```

Every fresh Codex prompt begins with one line, which the hook strips before Codex
sees it; a `codex-reply` may inherit the line's metadata from its recorded
thread:

```
ORCH|task=<stable-id>|phase=<implementation|operation>|route=<route>
```

`reason` and `basis` are optional metadata. When supplied, `reason` is
`new_task | continue | route_change | reanchor | repeated_failure | user_override`,
and both fields are recorded. `scope` is required when the phase is
`implementation` and names the paths the delegation may touch; add it to the
line as `|scope=<path[,path...]>`. It bounds the baseline manifest, and a scope
resolving to the whole repository is refused — repository-wide enumeration here
is 361,754 paths and does not finish inside a hook timeout.

A missing or malformed line is denied for a fresh `codex` call, never defaulted.
`codex-reply` may omit the line because it inherits task, phase, route, and scope
from its recorded thread; a reply without a line and without a recoverable
thread record is denied. What the hook enforces you no longer have to remember;
what it cannot judge — which route a task belongs to, whether a finding is real
— is still entirely yours.

### Task Contract & Validation Evidence Protocol

To ensure causal verification by Reviewer-1 and enable AUDIT mode, delegations should supply an explicit grounded task contract:

```
TASK_CONTRACT: {"objective": "<precise objective>", "criteria": ["<criterion 1>", "<criterion 2>"]}
```
Alternatively, multiline headers may be used:
```
Task Objective:
<precise objective>
Acceptance Criteria:
- <criterion 1>
- <criterion 2>
```
If neither is supplied, the hook uses bounded fallback values and reports `task_contract_grounded: false`. Continuations (`codex-reply`) automatically inherit the grounded contract established on the initial call.

Workers report execution validation strictly through a single structured JSON record:
```
VALIDATION: {"source": "WORKER_REPORTED", "status": "PASS"|"FAIL"|"UNKNOWN", "summary": "...", "commands": ["<test command>"], "exit_codes": [0], "artifacts": ["..."]}
```
- Free text never produces `PASS` or `FAIL` status; it is preserved purely as narrative `summary` with `status: "UNKNOWN"`.
- Strict invariants: `commands: list[str]`, `exit_codes: list[int]` (no bools), `len(commands) == len(exit_codes)`.
  - `status: "PASS"` requires `len(commands) > 0` and all `exit_codes == 0`.
  - `status: "FAIL"` requires `len(commands) > 0` and at least one non-zero `exit_code`.
  - Multiple `VALIDATION:` records, forged sources, or malformed types fall back to `status: "UNKNOWN"`.
- `claude_review_mode: AUDIT` requires all of: `task_contract_grounded is True`, `worker_validation.status == "PASS"`, Reviewer-1 result `NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE` (or AUDIT-eligible skip), clean delta, `PROCEED` concurrent-writer status, and `force_full_review is False`.
- Any worker validation failure (`status: FAIL`) mechanically forces `FULL` review with finding `WORKER_VALIDATION_FAILED`.
- Any unverified worker validation (`status: UNKNOWN`) forces `FULL` review with finding `WORKER_VALIDATION_UNVERIFIED`.
- Any ungrounded task contract forces `FULL` review with finding `TASK_CONTRACT_NOT_GROUNDED`.

`phase=operation` is what keeps ordinary operational work out of the review
pipeline: a deploy, a runner start, a status fetch, a read-only investigation.
It never triggers Reviewer-1. It does require that any changed artifact it acts
on was already accepted.

A run whose result is not the bundle never reached the hook at all. The fail-open
path below covers an internal ERROR; it cannot cover a hook that never runs,
which produces no bundle, no marker, no pending record and no diagnostic. That
has happened here: on 2026-08-30 a delegation executed with no hook, with the
registration untouched and the hook answering a synthetic event correctly, and
nothing announced it. So check the SHAPE of what comes back. A raw Codex object
instead of the compact bundle means routing, baseline, delta, Reviewer-1 and
telemetry did not run. Treat that run as UNROUTED: review it fully, and
reconcile the worktree against a baseline you captured yourself before doing
anything else with it.

When the hook fails internally it ALLOWS the call through unmodified rather than
denying it, because a denial on internal error leaves no way to proceed at all.
It then marks the call, and the bundle says `routing_enforced: false` with the
reason and forces `claude_review_mode: FULL`. If you see that, the model and
effort you asked for were never applied and no baseline was taken — treat the
result as unrouted and review it fully.

## What to delegate

Scope delegation by the shape of the task, not by the kind of artifact it
produces. "Repository code changes" is the common case, not the boundary.
Anything bounded, verifiable, and stateable as acceptance criteria is
delegable — an analysis, a data extraction, a schema derivation, an API probe,
a migration, a document repair, an investigation with a decidable question at
the end. Restricting delegation to files that happen to be source code is the
most common way an orchestration session quietly becomes a hand-work session.

Keep for yourself only what genuinely cannot be handed over: framing the task,
deciding what would count as an answer, reviewing the result, adjudicating
disagreement, and any operation whose authority is yours and not the worker's.
Also keep small orchestration bookkeeping whose delegation would cost more than
the work. Those are the orchestrator's actual job. Everything else is a
candidate.

The reliable early warning is repetition. The first ad-hoc one-liner against a
live system is legitimate reconnaissance. The second reading of the same source
is a signal you are building an instrument by accident, one throwaway command at
a time — and doing it in the least reviewable, least reproducible form
available. When a check will run more than once, or when its output will be
retained or reused beyond the current decision as evidence, it belongs in a
written instrument with tests, and writing it is delegable work. The cheapest
decisive check is right for a one-off review; reusable evidence needs an
instrument.

A synchronous call with no visible progress creates its own pull toward
duplicating the delegated investigation. Finish orchestrator-only preparation
before dispatch rather than starting ad-hoc hand-work in anticipation of the
wait; once the call starts, accept that the turn is blocked.

## Delegation is not handoff

Delegated work is reviewed after the worker returns, without exception and
regardless of how confident the completion report sounds. A worker reporting
success is a claim, not a result. This holds for every task shape above, not
only for code: an analysis whose method you never inspected is not evidence, and
a document you never read is not a record.

Review is a negotiation, not a verdict. A worker that pushes back, cites a
contract line against your instruction, reports that your premise does not hold,
or refuses the task is doing the job you asked for. Verify the objection
independently, and when it stands, change the instruction — do not restate it
more forcefully. Expect your own prompts to be wrong sometimes; when the defect
traces back to what you asked for rather than to what was built, say so plainly
and repair the prompt rather than patching around it in the code. The same
openness runs the other way: a fluent, well-structured answer that does not
survive an independent check is still wrong, and deference to it is not review.

## Authority

Delegation never expands authority. Codex capability does not imply authority. A
worker may perform only what the current user instruction and the authority
chain in `AGENTS.md`, `state/current.yaml`, and the active spec already allow.

Whatever a worker's runtime configuration happens to permit, an unauthorized
deploy, runner start, commit, push, live or order action, credential or secret
access, or scope expansion stays prohibited. Conversely, when the current task
and repository authority do open an operation — deploy, runner control, SSH or
API interaction, commit, push — do not impose an artificial orchestration-layer
restriction on it.

Do not pass `sandbox` or `approval-policy`. Workers inherit their own runtime
configuration and orchestration does not override it. Capability is set by
runtime config; authority is set by the user, `AGENTS.md`, current state, and the
active spec. This skill enforces authority; it does not narrow capability.

Never infer authority from available credentials, worker capability, a previous
task, or a previous review.

### Protected paths

Authority above is defined by ACTION. Some damage needs no prohibited action at
all — an ordinary edit to the wrong file breaks a hash chain or rewrites the
authority a later thread will read, with no deploy, commit, or credential access
anywhere in it. That is not hypothetical: a checkout-time rewrite of 26
sha-pinned inputs was filed as a cosmetic local hazard and then blocked a deploy
outright.

Check these paths in the post-call delta after every write-capable delegation.
When the task is not about one, name it in the prompt only if the delegated
scope or tooling could plausibly touch it:

- `state/current.yaml` and `AGENTS.md` — the authority chain itself
- `docs/EVIDENCE_RULES.md` and the active frozen spec
- `inputs/**` — sha-pinned envelopes, contracts, manifests, and fixtures whose
  declared hashes are load-bearing
- `review_artifacts/**` and any collected tape — evidence is written once and
  amended by appending a new artifact, never edited in place
- `.gitattributes` — it is what keeps the pinned bytes intact on checkout

This is a list, not a gate: there is no deterministic apply engine here and
reproducing one as prose would only create a rule nothing enforces. If one of
these appears in the delta and the task was not about it, stop and reconcile
before review, exactly as with an unknown concurrent writer. Do not silently
keep the edit because it looks harmless — the CRLF rewrite looked harmless.

## Consequential work

Treat as consequential unless the actual edit is demonstrably mechanical:
concurrency and async ordering; timing, clocks, deadlines, monotonic versus
wall-clock semantics; state machines and lifecycle transitions; economics,
prices, fees, fills, execution semantics; auth boundaries; durability,
persistence, recovery, reconciliation; causal or point-in-time evidence
integrity; fail-closed behavior; live, order-path, risk, and capability gating;
subtle cross-module invariants.

## New threads

Set explicitly on every new implementation or repair thread: `model`,
`config.model_reasoning_effort`, `cwd`, `developer-instructions`, and a bounded
`prompt`. Never rely on a worker's default model or effort — `codex-main` and
`codex-b` carry different local defaults, so relying on them silently changes
capability across a failover.

A configured `model` and effort are a **request, not runtime evidence**. Report
the served pair only when the runtime actually exposes it; otherwise label it
`unverified`. Never infer model-family diversity — the thing that makes a second
opinion independent — from configuration alone: "a different profile was
requested" and "a different model answered" are separate claims, and only the
second one makes independent review real.

`developer-instructions` carries:

> Codex must not blindly accept Claude's proposed solution. It should independently verify the relevant repository state and project rules, prefer the smallest correct implementation, and explicitly flag any instruction that would introduce a regression, unnecessary complexity, scope expansion, or conflict with existing contracts.

Codex verification is proportional: check the material assumptions behind the
requested change, not the whole repository. On a material conflict, report the
conflicting assumption, the governing rule, and the smallest compliant
alternative before editing. Otherwise proceed rather than manufacture
objections. When Codex raises a substantive objection, verify it independently
before repeating or overriding the instruction.

Do not set `base-instructions` in routine orchestration — it replaces Codex's
default instructions entirely. Do not use `compact-prompt`.

```json
{
  "prompt": "<bounded task>",
  "model": "gpt-5.6-sol",
  "config": {"model_reasoning_effort": "medium"},
  "cwd": "C:\\Projects\\polymarket-btc5m-lab",
  "developer-instructions": "<independence instruction>"
}
```

If a worker rejects a selected model or effort, treat that as a live availability
or configuration result: choose a routing tier deliberately, never silently
substitute another effort value.

## Worker placement and the quota ledger

The automatic chain is `codex-main` → agy → DeepSeek.

`codex-b` is NOT ACTIVE (user, 2026-08-29). Its account was a one-off. Do not
place work there, do not read its quota, and do not name it in a dispatch
receipt as a lane you weighed — treat the file under `~/.codex-account-b` as
history. If it is ever reactivated that is a user decision, and this paragraph
is what has to change first.

The three-lane rule below therefore reads as codex-main, agy and DeepSeek. The
quota ledger applies to `codex-main` alone among the Codex homes; agy and
DeepSeek are ranked on availability and task fit.

Worker identity is a placement decision with its own preflight, resolved BEFORE
model and effort. Run it in this order:

1. **Eligible continuation.** Apply the canonical `Continuation` rule below. If
   it permits a reply, placement stops here; context reuse beats quota routing.
2. **Observe quota.** For a FRESH thread, read the latest recorded quota signal
   and reset time for both workers, unless one is `bounded-exhausted` or
   `session-exhausted` and the other is not.
3. **Stale-lane probe.** Before burn pressure, apply the stale-unknown probe rule
   below when its old boundary passed or the best observed lane is under real
   pressure; never probe during consequential work.
4. **Burn pressure.** Exclude `bounded-exhausted`, `session-exhausted`, or
   otherwise known-insufficient lanes; among the rest choose the highest
   `remaining percent / time until reset`.
5. **Model and effort.** Apply `Model routing` after placement.

### The ledger

Keep a session-local table: worker, remaining percent, reset time, availability
by quota axis (`live-observed` / `recent-observed` / `stale-unknown` /
`bounded-exhausted` / `session-exhausted`), live thread id, that thread's model
and effort, and what diagnosis it holds. Session-local memory only - do not
write a persistent quota file.

`stale-unknown` stays `stale-unknown` on that axis. Never record it as zero or as
either observed state; outside the explicit stale-lane probe, a lane with a
stale-unknown relevant axis ranks after one whose relevant axes are observed.

The non-observed states must stay distinct; collapsing them either probes a lane
before reset or locks it out after reset:

- **`bounded-exhausted`** — a recorded limit WITH a reset time that has not yet
  passed. Genuinely unavailable. Do not place; do not re-read before the
  boundary.
- **`stale-unknown`** - no recent record, a record whose reset boundary has
  already passed, an old signal, an untyped signal carrying NO reset time, or
  no signal at all. Re-reading the same stale transcript cannot promote it: the
  transcript only updates when that worker is CALLED, so re-reading returns the same old
  record forever. If stale-unknown merely ranks last, the lane is never chosen,
  never called, and never observed.
- **`session-exhausted`** — a typed hard exhaustion carrying NO reset time,
  including credits. Do not place unless new live or user evidence establishes
  replenishment.

The DeepSeek lane sits on this last axis by construction. It is metered credit,
not a subscription window, so it has NO reset time to wait for and burn pressure
does not apply to it: the ratio that ranks Codex lanes is meaningless against a
balance. Rank it on availability and task fit alone, and treat exhaustion as
`session-exhausted` with `reset_time: none`. Its cost is real money per call
rather than a renewing allowance, so a wasted call there is not recovered by
waiting — which is an argument for giving it well-specified work, not cheap work.

A reading the USER supplies is a valid and preferred ledger source: it comes
from a live surface rather than a transcript artifact, so it outranks any file
read. A non-exhaustion reading can promote the windowed quota axis from
`stale-unknown` to `live-observed` without spending a call. It does not establish
overall lane availability: a percentage figure says nothing about a separate
credits limit. Record each axis separately.

Resolve a stale-unknown lane by PLACING a task on it - deliberately, and
preferring a low-risk bounded one whose failure costs a failover and nothing
else. A typed quota or credit refusal is a clean availability result; another
kind of refusal is not quota evidence and follows `Failover`. Do this when the
best observed lane is under real pressure or when the stale lane's last recorded
boundary has passed; do not do it in the middle of consequential work.

Refresh the ledger at exactly three decision points: before fresh-thread
placement, after a typed limit signal from a worker, and at the next placement
after a recorded reset boundary has passed. Not on a timer, and not before a
continuation.

### Reading quota

The two accounts have separate Codex homes: `codex-main` at `~/.codex`,
`codex-b` at `~/.codex-account-b`. The newest available recorded `rate_limits`
signal among recent session transcripts carries `used_percent`,
`window_minutes`, `resets_at` (epoch seconds), and
`rate_limit_reached_type`:

```bash
for h in ~/.codex ~/.codex-account-b; do
  find "$h/sessions" -name '*.jsonl' -printf '%T@\t%p\n' | sort -rn | head -8 |
  while IFS=$'\t' read -r ts f; do r=$(grep -o '"rate_limits":{.*' "$f" | tail -1 | cut -c1-200); [ -n "$r" ] && { echo "$h: transcript_mtime=$ts file=$f $r"; break; }; done
done
```

This costs one tool call and is worth it only when it can change the lane — that
is, when both workers are plausibly usable. If one is `bounded-exhausted` or
`session-exhausted` and the other is not, skip the read and place on the survivor.

Label the read with its transcript time and age. It is a transcript artifact,
not a live endpoint, and may predate the current session by days. Treat its
`used_percent` as a lower bound on consumption, never as current headroom.
Classify a non-exhausted signal as `recent-observed` only while its transcript
age is no greater than its reported `window_minutes` and its reset boundary has
not passed. Otherwise the axis is `stale-unknown`; retain stale values only as
labeled history, never as availability.

Burn pressure is close to meaningless when the two ratios are within a few
points of each other, so do not let it manufacture precision. Break a near-tie in
this order: on the same axis, `live-observed` outranks `recent-observed`; a lane
whose relevant axes are observed beats one with a `stale-unknown` axis; if both
lanes are otherwise equally observed, the EARLIER reset wins, because that
allowance is the one about to expire.

Burn pressure is a heuristic, not a scheduler. The next task's quota cost is
unknown, so it does not claim optimal allocation - it only spends an allowance
that expires on a clock at roughly the rate needed to avoid losing it. It never
overrides `Model routing`.

## Reviewer-1

An independent reviewer runs between the implementer and you. It is a different
model family from the implementer, which is the only thing in this arrangement
that offsets Claude both framing and reviewing the work.

The gate is mechanical and lives in the hook, not in your judgement:

| condition | outcome |
|---|---|
| `phase=operation` | skipped, `PHASE_OPERATION` |
| empty delta | skipped, `NO_DELTA` |
| ≤50 changed lines in ≤2 files | skipped, `BELOW_THRESHOLD` |
| transport unavailable | skipped, `REVIEWER_UNAVAILABLE` → review mode FULL |
| otherwise | runs exactly once |

The threshold exists because Reviewer-1 spends GEMINI quota to save CLAUDE
context, and those are separate budgets. A measured ~18k input tokens of
preamble rides on every agy call, so on a ten-line diff the packet round trip
costs more than reading the diff yourself. Above the threshold it pays.

A skipped reviewer is never representable as a passing reviewer — the bundle
carries `reviewer1_skipped_reason`, and skip, failure and success are three
different shapes. `NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE` means no material P0
or P1 in the bounded evidence it saw. It is not acceptance; you still audit the
diff, the acceptance criteria, the validation, and whether the patch could
invalidate an invariant outside the changed lines. Consequential and escalated
routes always get your full review regardless of what it said.

Transport is Antigravity, at the absolute path `C:\Users\ugur\scripts\agy.exe`,
model `gemini-3.8-flash-high` — model and thinking level are fused in that id.
Never a bare `gemini`: on this machine that name resolves to a different program
depending on which shell runs it, and Google's own Gemini CLI is dead for this
account (`IneligibleTierError`, individual Code Assist withdrawn).

Two properties are declared gaps rather than checks, because a check that always
passes is worse than a named gap:

- **Served-model attestation is UNAVAILABLE.** The agy envelope carries no
  served-model field, so the model id is pinned on the command line and
  recorded, and nothing claims it was verified.
- **Read-only is containment, not a guarantee.** `--mode plan` is an agent mode,
  not a sandbox: under `--mode plan --sandbox` with a directory supplied through
  `--add-dir`, the reviewer created a file there. So the reviewer runs in a
  throwaway directory outside the repository and is never handed a repository
  path; both are refused in code. Without `--add-dir` it did not reach outside
  its own working directory, and that is the whole basis for the containment.

## The DeepSeek lane

There is a third worker and it is not a Codex. It is the `claude` CLI itself,
pointed at DeepSeek's Anthropic-compatible endpoint. It has no MCP server, so it
is driven as a shell command rather than through `codex`/`codex-reply`:

```bash
CLAUDE_CONFIG_DIR="C:\Users\ugur\.claude-deepseek" \
ANTHROPIC_BASE_URL="https://api.deepseek.com/anthropic" \
ANTHROPIC_AUTH_TOKEN="$(cat /c/Users/ugur/Desktop/deepseek-api-key.txt)" \
ANTHROPIC_MODEL="deepseek-v4-pro[1m]" CLAUDE_CODE_EFFORT_LEVEL="high" \
claude -p "$(cat prompt.txt)" --permission-mode acceptEdits
```

`C:\Users\ugur\scripts\deepseek.cmd` wraps the same thing for interactive use.
Pass long prompts from a FILE; quoting a multi-paragraph prompt through cmd is
fragile and silently mangles it.

Four things differ from a Codex lane and each one will bite if forgotten:

- **`CLAUDE_CONFIG_DIR` is load-bearing, not tidiness.** Claude Code prefers a
  stored Anthropic OAuth login over `ANTHROPIC_AUTH_TOKEN`. With the default
  config directory the lane silently ships the logged-in account's ROTATING
  access token to DeepSeek and returns 401 with a DIFFERENT masked key tail on
  every attempt. That reads exactly like a bad API key and is not one. Confirm a
  suspected key problem by calling `/v1/messages` directly before touching the
  key; if the raw call returns 200, the key is fine and the config directory is
  the fault.
- **The worker is Claude Code, so it reads this repository's `CLAUDE.md`** — the
  file that tells Claude to delegate to Codex. Say in the prompt that it is the
  WORKER for this task and must not delegate or spawn subagents.
- **`developer-instructions` has no equivalent flag.** Use
  `--append-system-prompt`, and remember `--permission-mode` decides whether the
  lane can act at all; without it a tool-using task hangs non-interactively.
- **Its quota is metered credits, not a subscription window.** See the ledger
  rule below.

Architecturally it is a different model family from Codex, which makes it the
most valuable lane for an INDEPENDENT check of Codex's work, and vice versa. The
weakest point in this whole arrangement is that Claude both frames and reviews;
a second Codex does less to fix that than this lane does.

## Model routing

- mechanical, narrow, low risk → `gpt-5.6-luna` + `high`
- narrow but semantically subtle, where a miss would cost a repair cycle →
  `gpt-5.6-luna` + `xhigh`
- normal implementation, debugging, multi-file work → `gpt-5.6-sol` + `medium`
- consequential or semantically difficult → `gpt-5.6-sol` + `high`
- `gpt-5.6-sol` + `xhigh` → only on concrete escalation grounds: demonstrated
  complexity, material disagreement, or a specific lower-effort failure
- DeepSeek `deepseek-v4-pro[1m]` + `CLAUDE_CODE_EFFORT_LEVEL=high` →
  PROVISIONALLY the same row as `luna` + `xhigh`: narrow but semantically
  subtle. ONE observed task supports this and one task is not a placement.
  On its first outing it tested the premise before editing, wrote the failure
  CLASS rather than the reported strings, kept the fail-closed direction, added
  a deliberate negative test, ran its own reversion checks, and named what it
  could not decide. That is the evidence; it does not yet extend to
  consequential work, and the row stays marked provisional until several
  bounded tasks land. `deepseek-v4-flash` is the cheap tier and is untested
  here.

Note that `CLAUDE_CODE_EFFORT_LEVEL` is a Claude Code variable; whether the
DeepSeek endpoint honours it is UNVERIFIED. Do not report the lane as running
at a given effort as though that were established.

The left column is the task CLASS, and naming it is the first step, not a
free-form difficulty estimate. The classes are observable: mechanical/narrow;
ordinary implementation; semantically subtle; consequential by the list above;
or escalated on a concrete unexpected failure. Consequential is fixed at
classification time, before dispatch — adequate capability is never earned by
failing first. Within the class, pick the lowest tier expected to finish in ONE
pass.

Optimize total task cost, not per-call tokens: a configuration that predictably
triggers a review-repair loop is not cheap. Equally, do not escalate merely
because a stronger model exists, and never downgrade consequential work to
conserve quota. If a model or effort is unavailable on the selected worker,
prefer the other worker over silently reducing capability.

This rules out a deliberate cheap-first cascade. A weak first pass does not just
risk a retry: it spends a full independent review to discover the miss, and
reviewer cost dominates worker cost here. Escalation is recovery from an
UNEXPECTED failure, never a planned price-discovery step.

Effort is not a substitute for a resolved prompt. Raising it on an ambiguous
task buys deeper reasoning about the wrong question — the worker spends the
extra budget reconciling your contradiction rather than solving the problem.
Choose effort from the task, then re-read the prompt for conflicts; if you find
yourself reaching for `high` because you feel uneasy rather than because the
task is hard, the unease is usually about the prompt, and that is what to fix.

## Dispatch is a decision, not a default

Use this section as the visible-receipt checklist after applying `Continuation`,
worker placement, and `Model routing`; re-reading the whole skill each time is
not the intent.

Before every call, write the dispatch choice in your own visible text: worker,
model, effort, continuity, and one clause of why. Not in your reasoning — in the
output the user reads.

Stamp continuity with one of three typed values, never a loose phrase: `reply`,
`fresh`, or `failover-fresh`. Typed receipts make the stated cold-thread choices
countable and keep failover visibly distinct from a chosen cold start; they do
not prove that the alternatives were considered.

Name what you RULED OUT, not only what you chose. An option you did not name is
not auditable from the receipt:

- `not reply because …` - use `reply` only while the live diagnosis still holds,
  the worker is not known unavailable, and no anchoring, repeated-failure,
  model-or-effort-change, or failover ground requires a fresh thread.
- `not luna because …` — narrow or mechanical work defaults to `luna`; `sol` is
  the escalation, not the floor.
- `not medium because …` — `high` needs a named difficulty, never unease.
- `not the other worker because …` — a fresh thread names the placement ground:
  the other lane is `bounded-exhausted` or `session-exhausted`, has a
  `stale-unknown` relevant axis, or lost on burn pressure. "It is the one I used
  last time" is not a ground. With three lanes this line must account for BOTH
  the ones not chosen, including `not deepseek because …`; a lane that is never
  named in a receipt is a lane that has quietly stopped being considered.

Run the checklist BEFORE the choice, not after it. This is a self-instruction,
not something the visible receipt can verify. Retrospective, it only rationalizes
a decision already made; prospective, `not reply because …` prompts the search
for a live thread before a cold one is justified.

Writing the line is not the check. If the line reads the same whatever the task
is, the ritual has replaced the decision. Treat this as a soft control of
unmeasured effect - one observed dispatch changed because of it, which refutes
decoration without establishing reliability.

This exists because dispatch rules are the ones that get silently dropped.
Prompt-quality failures often surface during patch review, though conflicting
criteria can survive a shallow review. Dispatch drift is different: it can
return a correct result with no implementation failure at all.
Choosing the heavier model returns a correct result that merely cost more.
Opening a fresh thread for a one-line follow-up returns a correct result that
merely took a cold start. Nothing pushes back, so the default silently wins
every time and the drift is invisible from inside the session — in one observed
case, twenty consecutive calls at the same model with the routing table never
consulted, and no worker `codex-reply` at all despite several textbook
continuation cases.

## Make progress observable before a long call

A worker call is synchronous and emits nothing until it returns. On the user's
screen a normal six-minute task and a hung one are identical, and the user
carries the whole cost of that ambiguity: you know the call will return, they
do not. Left unaddressed they will interrupt working tasks — including, once, a
task that had already finished correctly.

Two cheap practices reduce that uncertainty and, for local mutations, improve
post-call reconstruction.

State the expected duration and the stall signal before the call, in one line:
roughly how long, and what would count as too long.

For a long local-mutation call where file timing would materially help after an
interruption, optionally start a trivial watcher first. It cannot show live
progress or attribute a write; it records only when matching filesystem
activity occurred. Never use its silence as a stall signal, and do not run it
for read-only or investigation-heavy work.

Tell the user they may interrupt the wait, but never imply that interruption
cancels the worker. Treat the outcome as ambiguous and reconcile before retrying.
A local-only edit may be recoverable from the baseline and inspected delta;
external or non-idempotent work needs an explicit status or idempotency
reconciliation path before dispatch, and external state must be reconciled
before any retry.

Splitting a task and announcing its duration are not substitutes for each other.
Investigation-heavy work writes nothing for a long time even when small, so lead
with a read-only diagnosis that returns fast and gives a visible pulse, then
implement against the answer.

Cut at an evidence boundary, not at an arbitrary midpoint. The first call must
return something the human can read and act on - a finding, a measurement, a
decidable answer - because that is what makes the pause worth a second call and
limits sunk cost if the user stops before the second call. A split that returns
half an implementation gives a pulse and nothing else, and costs a review either
way.

## Continuation

`codex-reply` accepts only `threadId` and `prompt`. Model, effort, and
`developer-instructions` cannot change on a reply — if any of them must change,
start a new thread.

Continue the same thread for an ordinary follow-up repair or task while the
worker is not known unavailable and the diagnosis still holds. Start a fresh
thread for worker failover, a stale diagnosis, a material P0/P1 error where
anchoring is plausible, repeated repair failure, or a justified model or effort
change. Do not open fresh threads for trivial follow-ups to simulate
independence.

## Baseline and worktree

Before every write-capable delegation, record `HEAD`, the `git status
--porcelain` path and state set, and a fingerprint of the staged and unstaged
diffs. Also record a path-and-hash manifest of every untracked or ignored file in
the delegated scope or in any protected path above, then rebuild it after the
call so additions, deletions, and changes in ignored paths are visible.

The baseline detects tracked changes and manifest changes in those scoped files;
it does not attribute them. After a timeout, connection loss, or any ambiguous
failure, do not immediately hand the same task to another writer: compare
against the baseline, inspect the delta, and reconcile before any further write.

Concurrent mutation from another session is normal here: Claude controls only the
workers it launches and cannot assume exclusive ownership of the worktree. Never
attribute such a change to Codex and never revert it.

Stop as `UNKNOWN_CONCURRENT_WRITER` — halt the autonomous write loop, reconcile
nothing blindly, and report — when the concurrent change overlaps the delegated
edit scope, touches a material dependency, contract, test, or verification
boundary, makes the diff or attribution ambiguous, could invalidate the review or
the reconciliation, or cannot be safely separated from the delegated task.

When the change is clearly disjoint from the delegated scope, preserve it,
exclude it from the delegated review, and continue the bounded task.

Never run `codex-main` and `codex-b` as concurrent writers in one working tree.
If parallel work is explicitly required, give each an isolated worktree through
`cwd` first. Preserve unrelated existing changes; a failover must never revert
work it did not create.

## Failover

Hard account limit - quota exhausted, credits insufficient, explicit capacity
error: no retry on that worker. A timed exhaustion is unavailable until its
recorded reset; a typed hard exhaustion with no reset, including credits, is
session-exhausted with `reset_time: none` unless new live or user evidence
establishes replenishment.
Open a new thread on the other worker carrying the same objective, constraints,
and acceptance criteria; apply `Model routing`; report the failover briefly.
Quota exhaustion is availability information, not an implementation-quality
failure.

Clearly transient failure where no work was performed: retry the same worker
once, then fail over.

Auth, config, or schema failure: fail over only if it is worker-specific; stop
and report the blocker if it is shared infrastructure.

## State currency

`state/current.yaml` is the Claude-to-Codex handoff surface. Prompt every new
thread to read it cold and, when state identifiers are material, require the
completion report to cite the active decision and spec identifiers it used. A
model or effort change forces a new thread, so stale state actively misleads a
worker instructed to verify repository state independently.

Before opening a new thread, check that the active lane block still describes
what actually happened. If it drifted and the current task authorizes state
maintenance, correct only the drifted fields. Otherwise do not edit state or
open a worker against a misleading handoff; report the discrepancy and request
the needed authority or direction.

Write state at a stable material checkpoint or lifecycle transition, never after
every tool call. Normally that is once per implementation and review cycle, after
the verdict: `updated_at`, the lane `status` or `disposition`, and the relevant
head. A long-running operation may justify its own checkpoint at a material
transition such as DEPLOYED, RUNNING, COMPLETED, or FAILED. Claude makes these
edits directly; delegating a few-line YAML transcription costs a whole worker
thread and a review pass.

Factual implementation and result fields may be updated from verified evidence.
Never infer new authority because runtime events occurred.

Never edit the active spec - it is frozen and sha-pinned in state; verify that
sha during review. Never change capability fields - `order_transmission`,
`current_start_authority`, `live_run_envelope`, `agent_authority`, caps,
deadlines — inside an autonomous loop; those transcribe a human decision only.

`state/current.yaml` is a current decision index, not a history log, and it is
read cold and repeatedly. Do not evaluate compaction after every delegation
cycle. Consider it when a lane closes or a phase is superseded, when completed or
superseded task narrative accumulates, or when the file stops helping a fresh
worker orient; line count is a heuristic prompt to look, never a trigger to act.
When warranted, follow the existing state and archive governance rather than
creating a second current surface.

## Delegation prompts

Write Codex prompts in short, technical English regardless of conversation
language.

Include only the objective, verified problem context, acceptance criteria,
explicit user constraints, and the files, symbols, contracts, or evidence that
materially narrow the task — plus the short authority slice when ambiguity is
otherwise possible. Do not repeat transport metadata such as `cwd` that the MCP
call already carries. Do not paste repository governance or task history — Codex
reads authoritative files itself. Prefer required behavior and invariants over
prescribing an unverified solution. Exclude speculative redesign, opportunistic
cleanup, unsolicited refactors, and polish outside the acceptance criteria.

The authority slice carries only what is task-specific and not derivable - the
active lane, current decision id and disposition, active spec path and sha,
which named run, and what is CLOSED for THIS task. It is not a place to restate
`AGENTS.md`. Codex merges `AGENTS.md` root-to-leaf on its own, and duplicating
standing rules per task both wastes the budget and creates a second, drifting
copy of the contract.

### Declare the fallback before dispatch

Only when a task has optional capabilities or several independent lanes — a
check that may be unavailable, a second reviewer that may not answer, a data
source that may be missing. Name one fallback in the prompt before sending:

- `sequential-parent` — the parent finishes the missing part itself;
- `generic-agent` — another worker covers it;
- `partial-result` — the task returns what it has, with the gap named;
- `fail-closed` — a missing lane fails the whole task.

Then require the report to state which fallback was taken and which lanes were
missing. Without this, a lane that quietly never ran is indistinguishable from a
lane that passed, and the result reads as complete when it is not. Skip it for
single-lane tasks; it is not a per-prompt ritual.

### Verify the evidence baseline

An audit, review, report, or mutation run measures one commit; it is not a
standing fact. Before citing one as problem context, identify its baseline
commit and compare it with HEAD. Tell the worker which findings are still
verified at HEAD and which must be re-checked rather than assumed.

> Worked example, from a mutation report produced against a detached commit.
> The prompt presented four surviving mutants as current findings, but two had
> already been closed by commits between that baseline and HEAD. The worker's
> report that all four were closed was literally true, but counted existing
> coverage as new progress because the evidence baseline had decayed.

### Resolve conflicts before sending

A GPT-class worker given contradictory instructions does not stop and ask. It
spends reasoning on reconciling them and then ships whichever reading it landed
on. So the failure arrives as a plausible patch that satisfies your words and
defeats your intent, which is expensive precisely because it survives a shallow
review.

Before sending, read the acceptance criteria as an adversary and find the pair
that pulls in opposite directions. Criteria of the form "always do X" beside
"bound X so it cannot run away" are the common shape. When two criteria can
conflict, say which one wins and what the loser degrades to.

> Worked example, from a real repair that came back broken. The criteria were
> "detect sustained blindness and resubscribe so recovery never depends on
> chance" and "attempts must be bounded, no resubscribe storm". Nothing said
> which governed. The worker bounded the lifetime total, latched off after three
> attempts, and reproduced the exact terminal deafness the task existed to
> remove. The fix in the prompt, not the code, was one sentence: bound the RATE,
> never the lifetime; after the burst, fall back to a slow cadence forever.

### Examples become the specification

Concrete failing cases are the fastest way to convey a defect and the fastest
way to get a fix that generalizes to nothing but those cases. A list of four
broken inputs reliably produces a four-entry lookup table.

State the failure CLASS as the requirement, then offer the instances as tests
that must pass, explicitly labelled as non-exhaustive. When the honest answer is
that only a finite table can work — exonyms, transliterations, anything not
derivable by rule — say so, and require that the residual gap be reported rather
than hidden.

### Make refusal a first-class deliverable

When a task rests on an assumption you have not verified, make testing it the
first deliverable and name the stop condition: answer this before building
anything, and if it fails, stop and report what rejects it and what the minimal
compliant alternative would be. Never leave a worker with only two options,
comply or invent.

This is the single highest-yield pattern available here. It converts a wasted
build into a cheap, cited "no" — and a worker that cites the three contract
lines rejecting your plan has told you more than one that silently works around
them.

Ask for a concise completion report: files changed, behavioral change,
validation performed and its result, and any unresolved concern or assumption.
For analytical work also require sources, commands or method summary, and exact
evidence citations sufficient to reproduce the result. Deep internal reasoning
is fine; long final narration is not.

An MCP call is synchronous and its progress is not visible, so keep each
delegated task small enough to finish in one pass. That is what keeps the
ambiguous-outcome path rare rather than routine.

### Repository reconnaissance and impact mapping

Delegate broad repository reconnaissance when it would otherwise require large
orchestrator reads. Have the worker locate callers, tests, schemas, and boundary
files and return exact paths and symbols, not file contents. Treat the map as an
index, never evidence: the same worker that missed a caller can omit it from the
map. Run ONE independent completeness check, then review the named material
boundaries. A cheap shell operation can produce an expensive context result, and
successful discovery logs are the largest avoidable payload.

### Broad validation execution

Delegate the expensive build, suite, replay, or static search. Have the worker
return a compact result plus artifact paths and hashes, not the logs; successful
logs are the largest avoidable payload from an otherwise cheap shell operation.
Run the cheapest decisive independent check that task risk requires. This does
not move attribution review: a reported passing suite is still a claim, and the
orchestrator must establish that a claimed effect disappears without the new
code.

## Review

For mutations, review diff-first: the actual diff, then the changed behavior and
affected invariants, then only the material callers, callees, and boundaries,
then the cheapest decisive check that `AGENTS.md` proportional validation
requires. For read-only work, inspect the cited sources, method, and result
first.

Depth follows risk, never token budget: normal for a bounded ordinary change;
deeper for the consequential work above, covering every material boundary but no
unrelated code; escalated only on a concrete failed verification, unresolved red
flag, unexplained runtime behavior, material disagreement, or repeated repair
failure.

Start from the smallest evidence set that decides the question and expand only
for a concrete uncertainty, dependency, failed check, disagreement, or risk. Do
not reload authoritative files already in context unless they may have changed.
After a repair, review the delta since the last reviewed state, the prior
blocking finding, and any newly affected boundary — not the whole repository —
and do not repeat an expensive successful check that the repair could not
invalidate.

Request another repair only for a concrete material defect, failed acceptance
criterion, repository-contract violation, or required validation gap. Do not
manufacture findings to justify another cycle.

### Sequencing around a verdict

Human decision, 2026-08-29: the commit comes AFTER acceptance.

```
implementation -> focused validation -> Reviewer-1 -> Claude ACCEPT -> commit -> push
```

Rejected work does not enter the history. Push, deploy, runner start, and any remote
mutation based on changed code all wait for ACCEPT as well.

### One-Turn Consolidated Audit Protocol (Priority 4)

When Reviewer-1 (Sentinel) passes and the bundle emits `claude_review_mode: AUDIT`:
- The bundle delivers a **1-turn Consolidated Audit Receipt** containing:
  1. Reviewer-1 verdict (`NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE`)
  2. A $\le 10$-line diffstat
  3. Focused validation summary (e.g. `focused pytest = PASS, 20 passed`)
  4. Prepared atomic commit command
- Claude **MUST review evidence and execute commit in ONE SINGLE TURN** without reading raw diffs or issuing exploratory Bash commands.
- If Reviewer-1 returns `CHANGES_REQUIRED`, the bundle sets `claude_review_mode: AUDIT_REPLY`. Claude forwards the exact findings to Codex via `codex-reply` in a single turn without manual code editing.

### One typed verdict

Close every review with exactly one verdict, and name the artifact or check that
decides it:

- `VERIFIED` — the claimed effect was observed, not merely reported.
- `NOT VERIFIED` — the claim failed, or the evidence contradicts it.
- `INCONCLUSIVE` — the check could not decide. **This is not acceptance.**

A green suite, a clean build, or a worker's own success report is evidence for
the verdict, never the verdict itself. When the delegated change was supposed to
alter behavior, `VERIFIED` requires evidence that the behavior actually changed —
the attribution rule above still applies. Reaching `INCONCLUSIVE` and proceeding
anyway is a decision the user makes, so surface it rather than rounding up.

When a worker claims its change produces an effect, verify the attribution as
well as the outcome: establish that the effect disappears without the new code.
Apply this proportionally to claims that something is now protected, especially
that new coverage catches a defect; a passing suite is equally consistent with
the coverage being redundant. Re-applying the defect and observing which test
fails is the decisive check.

## Bounded loop

When the user authorizes autonomous completion of a bounded objective: initial
implementation, then at most two ordinary material repair iterations, then at
most one escalated repair — fresh thread, other worker, or stronger model — when
justified. Then ACCEPT, or stop with the unresolved blockers named.

Stop earlier when acceptance criteria are met, a new human decision is required,
scope would materially expand, repository authority is ambiguous, both workers
are unavailable, a shared MCP or config problem blocks both, or repairs stop
producing material progress.

Do not loop for P2 polish, style preference, optional optimization, speculative
cleanup, or confidence without a concrete unresolved risk. When the scoped
acceptance criteria and repository requirements are met, stop.

When stopping leaves a real residual risk — work that is out of scope or below
the repair bar, but that a reasonable person would still want addressed — say so
to the user and let them decide whether to continue. Name the risk, why it fell
outside the loop, and what fixing it would cost. Do not silently file it into a
background task and move on; a deferral the user never saw is a decision made on
their behalf. Filing the task is the right way to record it, but only alongside
telling them.

This matters most where the residual risk is a SILENT failure — a check that can
lose coverage without announcing it. Those cannot be caught later by noticing
something broke, so the decision to defer them is the user's, not yours.
