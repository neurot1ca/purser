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

Replies: at most 5 lines unless asked for detail; replies remain in context.
Exceed the limit only to state a risk, a failure or a decision the user must
make; never drop one of these to fit. Each finished job also gets one result
line (what changed and its measured effect) outside the limit.
Follow ASD-STE100 rules, applied to the user's language: give the result first.
Write one topic per sentence, with at most 20 words in an instruction and 25 in
a description. Use the active voice, simple tenses and the imperative for
instructions. Use one term for one thing, with no idioms or metaphors. Write
procedures as numbered steps with one action per step. Keep paragraphs to 6
sentences or fewer.

## Project gear

If the user asks in chat to gear up or down, run the command block from the plugin's
`commands/gear-up.md` or `gear-down.md` directly; this is not a coding job.
If the user only asks for faster work without requesting a switch, offer
`/purser:gear-up` in one line and wait. Gear applies to the current project
until gear-down. Existing replies keep their thread's model; new jobs use the
geared route.

## Capability cards

Load each source when relevant. Runtime configuration and bundles own current models, coders, reviewer state, and review mode. Command paths are relative to the plugin root.

| Card | Use when | Skip when | Load |
|---|---|---|---|
| `delegate` | Preparing bounded delegated work | No delegation | [delegation](references/delegation.md) |
| `review` | Result arrives; repair finding, acceptance, or apply conflict | No result to assess | [review](references/review.md) |
| `conditional` | Unverified premise, conflicting criteria, optional lanes, substantial exploration, UI add-on question | None | [conditional delegation](references/conditional_delegation.md) |
| `causal-review` | Claimed causal effect or residual risk at stopping | No claim or risk | [conditional review](references/conditional_review.md) |
| `receipt` | Dispatch/completion receipt or formatting uncertainty | Clear format | [receipt examples](references/receipt_examples.md) |
| `roles` | User requests coder/reviewer setup or changes | No change | `commands/init.md` or `commands/reviewer.md` |
| `plan` | Explicit planning request or accepted-plan continuation | Neither | `commands/plan.md` |
| `gear` | Explicit gear change; speed request needing its existing suggestion | Neither | `commands/gear-up.md` or `gear-down.md` |
| `maintenance` | Token audit, cleanup report, maintenance consent or opt-out | None | `commands/optimize.md` |
| `check` / `compact` | Installation diagnosis or explicit auto-compact configuration | Neither | `commands/check.md` / `auto-compact.md` |

The ordinary delegation activates `delegate`; its completion activates `review`. Their combined cost must be measured over the whole lifecycle. Deferring review until completion does not by itself save tokens.

Keep brief language and authority pointers in this catalog. Preserve exact contractual schemas in the relevant playbook. Do not copy cards or playbooks into dispatcher or worker prompts.
