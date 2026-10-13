---
description: Ask the configured coder for a read-only, evidence-based implementation plan.
allowed-tools: Bash(node *)
---

Use the user's request below as the planning goal. Plan only; do not edit code or start implementation.

Dispatch one asynchronous call through the configured coder. Use `codex_start` once, run the exact waiter command it returns in the background, end the turn, and if it prints `STILL_WAITING`, run it again; otherwise call `codex_result` with this jobId. Do not start a second planning call for this request. Use a stable task ID for the whole call. The first line of the worker prompt must be exactly:

```text
ORCH|task=<stable-id>|phase=plan|route=plan_standard|reason=new_task[|scope=<repo-relative-path[,repo-relative-path...]>]
```

Include `scope=` only for verified, bounded repository-relative paths that are relevant; omit it when unsure. Replace the bracketed optional portion with the actual scope field or omit it entirely. Keep the prompt in English and give the coder a compact brief containing:

- **Goal:** the user's requested outcome.
- **Verified context:** only observed repository facts, with relevant paths and symbols; label assumptions explicitly. The pre-hook adds exactly one `CODE_MAP=<path> STATUS=<FRESH|STALE|UNAVAILABLE>` line automatically. Do not run the generator or add the line by hand. The map is navigation only, so open relevant sections on demand and verify current source; a stale or unavailable map does not block planning.
- **Constraints and exclusions:** applicable repository instructions, requested boundaries, and explicit exclusions.
- Purser appends the required plan shape automatically.

Require an evidence-based plan: no code edits, invented paths or symbols, or generic checklists. The worker must inspect the repository and distinguish observed facts from assumptions. Do not turn plan output into implementation work in this call.

Use the normal one-line dispatch receipt in the user's language: route `plan_standard`, the plan_standard route's resolved model and effort, fresh continuity, reason `new_task`, expected duration from the available duration hint (or a clearly marked estimate). Format it as the skill's bold receipt line and put it in the reply that ends the turn after `codex_start`; never show the waiter command or job ID to the user. After the result arrives, read the complete plan artifact at `plan_path` before review or acceptance, then review it and the bundle, including that the plan is read-only and the required sections and bounds are satisfied. If the inline `plan_summary` is absent or says the complete summary is available in the artifact, use the full artifact for the user-facing summary. Never present an abbreviated preview as the complete plan. Treat the expected `PHASE_PLAN` reviewer skip as a skip, not as a passing review; Claude performs FULL review. Purser shows the completion receipt; do not repeat it. Present the plan summary to the user in the user's language, and wait for the user's decision; do not begin implementation on your own.

When the user accepts the plan, implement all milestones returned as ready. For each, read only that milestone's section from the returned `plan_path`, turn it into a normal, bounded implementation task with its scope and acceptance criteria, and follow the ordinary implementation routing and review process. Use task ID `<plan-task-id>-m<N>of<T>` for every milestone job, where `T` is the number of accepted milestones. The existing job records supply the compact handoff with each milestone's job ID, apply state, and next step. After a milestone is accepted, launch newly ready milestones without asking. Stop only for a real decision, a failed review, an approval-gated action, or user-visible text, which still needs the user's approval. If the user accepts only some milestones, implement only those.

Below, `purser-hook` means `"${CLAUDE_PLUGIN_ROOT}/bin/purser-hook-<os>-<arch>"` (`windows`/`linux`/`darwin`, `amd64`/`arm64`, `.exe` on Windows).
When the user explicitly accepts milestones, call `purser-hook plan accept` with the repository `--cwd`, plan `--task-id`, returned `--artifact` path, and comma-separated accepted `--milestones`.
When a milestone is fully accepted, run `purser-hook plan status --cwd <repository> --task-id <accepted-plan-task-id>` and use that plan's returned revision with the same `--task-id` in `purser-hook plan complete-milestone --milestone <the accepted milestone number>`. Always name the milestone Claude accepted; if apply already completed it, this is an idempotent no-op. Research/no-delta milestones can be completed without an applied pass.
For dependency scheduling, the artifact declares `### M<N>` sections with `**Dependency:** none` or comma-separated `M<N>` IDs and a `**Write paths:**` (or `**Touched paths:**`) bullet list. Each section is bounded to 32 KiB.
For an accepted plan, dispatch every entry in ready_milestones in the same turn, using one background purser:dispatcher Agent call per pass. Submit all calls before waiting for any result. Each entry carries its milestone ID and artifact reference; running independent work does not suppress other ready milestones. Legacy plans retain serial continuation.
Call `purser-hook plan stop` or `resume` only on an explicit user request. First run `purser-hook plan status --cwd <repository> --task-id <accepted-plan-task-id>`, then pass that same `--task-id` and its returned `--revision` to `stop` or `resume`; never infer either action from a topic change, failure, or review rejection.

## User request

$ARGUMENTS
