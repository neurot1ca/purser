---
description: Ask Codex for a read-only, evidence-based implementation plan.
allowed-tools: Bash(node *)
---

Use the user's request below as the planning goal. Plan only; do not edit code or start implementation.

Dispatch one asynchronous Codex call. Use `codex_start` once, run the exact waiter command it returns in the background, end the turn, and call `codex_result` exactly once after that waiter finishes. Do not start a second planning call for this request. Use a stable task ID for the whole call. The first line of the worker prompt must be exactly:

```text
ORCH|task=<stable-id>|phase=plan|route=plan_standard|reason=new_task[|scope=<repo-relative-path[,repo-relative-path...]>]
```

Include `scope=` only for verified, bounded repository-relative paths that are relevant; omit it when unsure. Replace the bracketed optional portion with the actual scope field or omit it entirely. Keep the prompt in English and give Codex a compact brief containing:

- **Goal:** the user's requested outcome.
- **Verified context:** only observed repository facts, with relevant paths and symbols; label assumptions explicitly. Before dispatch, run `node "${CLAUDE_PLUGIN_ROOT}/bin/python-locator.js" mcp "${CLAUDE_PLUGIN_ROOT}/.claude/hooks/orchestrator/code_map.py" --repo <project root>` (substitute the actual project root) and append its single output line `CODE_MAP=<path> STATUS=<FRESH|STALE|UNAVAILABLE>` here only when the planning goal lacks reliable file or symbol locations; otherwise omit it and give the known paths. The map is navigation only, so open relevant sections on demand and verify current source; if the command fails or prints nothing, omit the line and say planning proceeds without it.
- **Constraints and exclusions:** applicable repository instructions, requested boundaries, and explicit exclusions.
- **Required plan shape:** Markdown with these sections and limits:
  - A leading `Summary:` section of at most 4000 characters.
  - `Goal and boundaries`.
  - `Current evidence` with paths and symbols, clearly separating observed facts from assumptions.
  - `Decision` with one recommended approach and brief alternatives.
  - `Milestones`: no more than five; each must state bounded scope, observable acceptance, validation, dependency, proposed touched files/dirs, and a parallel group (milestones in the same group touch disjoint paths and may run together).
  - `Risks/open decisions`: no more than five, each with an owner or a question that stops progress.
  - `Implementation handoff`: the first task's bounded repository paths and acceptance criteria.

Require an evidence-based plan: no code edits, invented paths or symbols, or generic checklists. The worker must inspect the repository and distinguish observed facts from assumptions. Do not turn plan output into implementation work in this call.

Use the normal one-line dispatch receipt in the user's language: route `plan_standard`, model `gpt-6-sol/high`, fresh continuity, reason `new_task`, expected duration from the available duration hint (or a clearly marked estimate), and stall threshold. Format it as the skill's bold receipt line and put it in the reply that ends the turn after `codex_start`; never show the waiter command or job ID to the user. After the result arrives, review the full plan at `plan_path` and the bundle, including that the plan is read-only and the required sections and bounds are satisfied. Treat the expected `PHASE_PLAN` reviewer skip as a skip, not as a passing review; Claude performs FULL review. Report the normal completion receipt using returned model, effort, duration, and quota details when present. Present the returned `plan_summary` to the user in the user's language, and wait for the user's decision; do not begin implementation on your own.

When the user accepts the plan, implement its milestones in order. For each, read only that milestone's section from the returned `plan_path`, turn it into a normal, bounded implementation task with its scope and acceptance criteria, and follow the ordinary implementation routing and review process. After a milestone is accepted, start the next one without asking. Stop only for a real decision, a failed review, an approval-gated action, or user-visible text, which still needs the user's approval. If the user accepts only some milestones, implement only those.

## User request

$ARGUMENTS
