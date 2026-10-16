# Purser

Purser coordinates delegated coding work for Claude Code.

Purser keeps the workflow simple and focuses on saving Claude context and time.

![Windows](https://img.shields.io/badge/Windows-supported-0078D6?logo=windows) ![Linux](https://img.shields.io/badge/Linux-supported-FCC624?logo=linux&logoColor=black) ![macOS](https://img.shields.io/badge/macOS-supported-000000?logo=apple)

Purser is a parallel coding agent orchestrator for Claude Code: it delegates coding tasks to background workers that run side by side. It is made for developers on smaller Claude plans who want their usage to last through large projects. Claude stays in charge of planning and reviews, while configured coding agents write code in isolated Git worktrees.

## How Purser works

You ask Claude for a feature or a fix. Main Claude chooses a route. For a new implementation on a consequential or escalated route without a matching plan, Purser asks for `/purser:plan` first.

It finds which steps can run at the same time and starts each one in its own worktree (a private copy of the project), so your working folder stays untouched. You keep chatting with Claude while background workers edit files and run tests. Jobs that would change the same files wait in a queue until the earlier task finishes. Each job's start, wait and result handling runs in a small side session on a low-cost Haiku model, so Claude's main conversation is not interrupted.

Because jobs are small, lightweight models that use very few tokens, such as gpt-6-luna, can do the work well. They need only the files for that step, without loading your full conversation history.

Claude reviews each finished job. Work that is not accepted can go back to the recorded job for repair. Accepted work is applied to your project, and host validation runs on the applied revision. Finished worktrees are scheduled for cleanup when eligible. Claude stays the planner and Reviewer, while the token-heavy coding happens elsewhere. Purser records local telemetry from each run and uses it to improve timing estimates.

## Measured savings

These figures come from repository measurements and estimates with their basis shown:

- About 78% smaller first-request context for the Purser dispatcher than a general-purpose subagent, about 12K versus 55K tokens, measured over 27 runs.

- Estimated 62% and 84% lower Claude token use for two delegated jobs, based on subagent transcript call counts and a 176K-token main-thread lower-bound estimate. The repository records these as estimates from two delegated jobs; no matched main-conversation runs were used.

- Tiny job receipts (at most 250 tokens), returning only the essential facts.

Results vary by task.

## Telemetry and estimates

Purser measures local job details such as route, model, duration, tokens, reviews, send-backs, queue waits, and stalls. The records stay on your machine.

Purser uses these records to update job time estimates. Route selection is rule-based; ETA estimates and free-model rankings learn from past runs.

For more detail, Purser can write telemetry notes to an Obsidian vault when `PURSER_OBSIDIAN_VAULT` or `vault_root` is set to an absolute path. You can then inspect which jobs ran, how they were reviewed, and where time and tokens went.

## Quickstart

### Prerequisites

The released plugin needs Claude Code, Node.js, Git, and at least one supported coding CLI installed and authenticated. Purser ships prebuilt binaries for Windows, macOS, and Linux on amd64 and arm64. Node.js launches the MCP server; bundled Go binaries run the hooks and commands. Python is not required.

- Claude Code
- Node.js
- Git
- At least one installed and authenticated coding CLI, such as OpenAI Codex
- Optional OpenCode or Antigravity (agy) CLI when you want those coder lanes

Install and authenticate `codex`, `agy`, or `opencode` separately. Purser resolves the selected CLI from `PATH`.

### Installation

<!-- TODO install command -->

Restart Claude Code, then initialize your settings:

```text
/purser:init
```

The setup marks an option recommended only when Purser has a reason. It can write global defaults to `~/.orchestrator/config.toml` and project settings to `.orchestrator/profile.toml`. It asks whether to trust the current project; project profile commands run only after that trust is accepted. If something looks off, run `/purser:check` to inspect your runtime binaries and CLI status. Purser stores runtime state under `~/.claude/purser` by default.

## 1. Keep Claude responsive while agents code in the background

Purser moves execution to a background dispatcher agent. Main Claude defines the task contract, file boundaries, and acceptance criteria, then starts the dispatcher. The dispatcher launches the external worker and waits for it to finish. While the worker writes code and runs tests in the background, you and main Claude continue planning or discussing architecture.

When the worker finishes, the dispatcher verifies the contract and returns a JSON receipt of at most 250 tokens. It reports the task, job, thread, revision, state, scope check, validation, Reviewer status, review mode, apply outcome, evidence reference, and next blocker.

A small Haiku dispatcher runs each job in four steps:

1. It starts the worker with the brief main Claude wrote.
2. It waits in the background while the worker edits files and runs tests.
3. It checks scope, test results and the independent review, and sends the work back once if something is missing.
4. It returns a receipt with `apply_outcome: awaiting_acceptance`. Main Claude inspects `evidence_ref` and calls `codex_apply(jobId, acceptOnly: true, revision)`. The dispatcher then runs `codex_apply(jobId)` and returns the final receipt.

## 2. Handoff and auto-compact

Purser keeps a short handoff note: goal, state, decisions, next step, open risks, files to load, and user language. Before Claude compacts, Purser updates it with recent commits, jobs, and the active plan step. After compact or `/clear`, the note returns at session start so work can pick up right where it stopped.

Auto-compact is optional; `/purser:init` asks whether to turn it on. When the conversation grows large and idle, Purser can compact it and continue with the note, reporting context before and after.

## 3. Catch blind spots with an independent reviewer before you accept

Purser routes code reviews to Reviewer, an automated review check from a different model family. Purser recommends a Reviewer independent of both the coder and Claude, but the choice is yours. You can assign an installed agent, including Codex, Claude, Antigravity (agy), or OpenCode, or turn Reviewer off. For Codex, the current choices are the newest sol model at low effort and the newest luna model at medium effort. An agy Reviewer uses the newest listed Gemini model.

The reviewer receives an isolated packet with the diff, the task criteria, test results, and relevant context. It returns no material finding, changes required, uncertain, or reviewer unavailable. Before Reviewer runs, Purser can skip plan and operation phases, no-delta jobs, small diffs of 50 lines or fewer across two files or fewer, or a disabled or unavailable reviewer. Skips and unavailable reviewers are recorded explicitly and never converted into automatic approvals.

Main Claude evaluates the findings. Purser applies changes when Claude approves the reviewed change. Applying code updates your working tree; creating a Git commit remains an explicit action for you and Claude.

## 4. Match model power to task difficulty

Purser matches task difficulty to a lighter or stronger model. Routine chores route to fast, economical tiers, while subtle fixes, features, and complex architectural decisions route to deeper reasoning tiers.

You can adjust project capacity on the fly without editing configuration files:

- Run `/purser:gear-up` to temporarily elevate eligible Codex jobs to a configured higher gear.
- Run `/purser:gear-down` to return Codex jobs to baseline settings.

A first review failure gets one same-thread send-back. If a luna implementation attempt is rejected after that send-back, Purser routes the next attempt to normal and records a short basis note.

Purser also monitors rate limits. If a provider reaches quota limits or fails to launch, Purser presents diagnostic details and asks for your consent before switching to a configured fallback coder. If you decline and wait, Purser records the reset time and wakes the session with "⚓ Codex quota renewed ✓" once it passes, so the held work resumes without a message from you.

## 5. Measure free OpenCode models for mechanical work

For mechanical chores, Purser can use a measured OpenCode free-model race when you turn the lane on.

OpenCode publishes free models. When enabled, Purser uses free OpenCode models for eligible mechanical chores. New models first run a built-in task bracket in the background. The bracket winner then races the current leader on shadow runs. Purser ranks the result with the smoothed score `(successes+1)/(samples+2)`; a newcomer can win after three samples, and the leader changes only on a race win.

For live jobs:

- Live jobs run in their own worktree and get the normal scope check and review rules. Nothing lands until Claude accepts it.
- Shadow runs use scratch files. The benchmark lets the model edit those files and scores the result.
- When enabled, only mechanical chores go to free models. Anything that needs judgment stays with your main coder.

## 6. Structure large work into durable plans that survive restarts

For a new implementation on a consequential or escalated route without a matching plan artifact, Purser redirects the call to `/purser:plan`. The planner inspects the repository and produces a structured plan with ordered milestones. You review and approve the milestones before implementation begins. Once approved, each milestone starts as soon as the milestones it depends on are done, so independent steps run side by side.

Purser saves the plan to disk and records progress as milestones finish. Because the plan is stored durably, your work survives session compactions or terminal restarts. When you open Claude Code again, Purser can restore your active milestone so you can pick up where you left off.

## 7. Find relevant code with a lightweight code map

Purser builds a lightweight map of the repository's files and symbols across 17 languages, including Go, Python, JavaScript/TypeScript, Rust, Java, C/C++, Swift, and Kotlin.

Workers get a map to find the right files and open them as needed, without loading the whole repository into their prompt.

## 8. A background cleaner works while you rest

Purser can use quiet time for upkeep. When the project has been idle for five minutes and no job of yours is running, a background cleaner scans the code and writes two short reports: cleanup candidates (unused code) and missing tests, each with its evidence and a path to the full report.

- Findings come sorted from most to least important, each with a severity, so the first row is the one worth your time.
- It never edits your files. You decide what, if anything, becomes a job.
- It is opt-in per project, runs at most once every 56 hours, and steps aside the moment you start a job.
- Turn it off with `purser-hook maintenance opt-out --repo <repository-root>`.

## Also included

- Quota and usage monitoring: see active model limits, reset countdowns, and reported session costs in your terminal.
- Background job board: track each job's stage (coding, in review, accepted, done), its model, and a time estimate learned from this project's own past jobs.
- Token and configuration optimizer: run `/purser:optimize` to find unused plugins and lighten prompt loads.
- Multiple coding CLIs: run OpenAI Codex, Claude, OpenCode, or Antigravity (agy) with detected Gemini models under one workflow.

## Commands

- `/purser:auto-compact`: Turn Purser's automatic `/compact` on or off for all projects.
- `/purser:check`: Check whether Purser is installed and usable.
- `/purser:coder-effort`: Set the Codex coder's thinking effort for new jobs (low, medium, high, xhigh), or auto to follow Purser's routes.
- `/purser:gear-down`: Return this project to its normal Codex gear.
- `/purser:gear-up`: Use the faster project gear for new Codex jobs.
- `/purser:init`: Set up Purser roles and optional project trust and profile.
- `/purser:optimize`: Find unused plugins, skills and MCP servers and other token waste in your recent sessions.
- `/purser:plan`: Ask the configured coder for a read-only, evidence-based implementation plan.
- `/purser:reviewer`: Choose Reviewer from the agents installed on this machine, or turn it off.

## How Purser compares

Purser coordinates coding work from inside Claude Code:

- Tools like Aider commit every change automatically. Purser isolates work in Git worktrees and leaves commit decisions to you and main Claude.
- Tools like Cline and Roo Code run inside the IDE. Purser runs detached workers in the background so tasks can run concurrently.
- Tools like Worktrunk manage Git worktrees as a standalone command-line tool. Purser integrates directly into Claude Code to coordinate background workers, reviews, and apply.

## Under the hood

These mechanisms support the orchestration engine:

- Code map: native tree-sitter parsers index symbols across many languages, giving workers targeted excerpts without scanning the entire project.
- Contracts and change attribution: every job starts with declared path boundaries; Purser snapshots Git state beforehand and attributes changes afterward, flagging any edit to protected paths like `.git` or environment files.
- Honest test results: Purser records every test run a worker makes (command, exit code, output tail) and decides PASS or FAIL from that record. Earlier failing runs remain context; runs after the last edit decide. Claude does not rerun tests that already passed.
- Fair queuing: jobs that touch the same files wait in order instead of colliding, and the board names the job each one is waiting on.
- Automatic cleanup: Purser removes finished worktrees and temporary patch files while preserving active, unapplied, or conflicted work.

## Frequently asked questions

### Does Purser provide its own AI models?
No. Purser is an orchestration plugin. It coordinates Claude Code with the coding CLIs installed on your machine, including OpenAI Codex and Claude Code.

### Do I need anything besides Claude Code?
You need Node.js, Git, and at least one coding CLI such as OpenAI Codex installed separately and available on `PATH`. Purser ships native binaries for Windows, Linux, and macOS on amd64 and arm64.

### Is it worth it on a Pro plan?
Yes, especially if you hit Claude usage limits on longer tasks. Claude only plans and reviews while Codex handles the token-heavy coding, helping your plan go further.

### Can background workers edit files outside their declared scope?
Tasks define explicit path boundaries, and Purser flags any out-of-scope or protected file. These boundaries are contract checks; worker CLIs run with your standard user permissions.

### Is an independent review the same as accepting code?
No. Reviewer provides an advisory second opinion from another model family. Main Claude evaluates the findings, and the changes are applied when Claude approves the reviewed change.

### What happens if my terminal closes during a task?
Purser records job state and process identifiers on disk. If your terminal closes or your system restarts, startup checks inspect active processes so you can adopt completed work or resume.

## Documentation

- [Plugin setup](docs/PLUGIN.md)
