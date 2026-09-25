# Changelog

Every plugin release (`.claude-plugin/plugin.json` version) gets an entry here
before it is published.

## 0.1.26 — 2026-09-25

- Skill: every Codex call runs async; dispatch and result receipts are one bold line with model, effort and quota.
- /purser:optimize: ranked, plain-words report of actionable drains per 500 messages, friendly names, hidden sessions in absolute terms, learning note.
- Init: stores the base Python instead of a venv one, offers Ponytail for UI projects, mentions what Purser learns.
- Reply deny messages name the tool, the disallowed fields, and the recorded task/phase/route mismatch (`a91fc7a`).

## 0.1.25 — 2026-09-25

- **Fix:** codex_start no longer cancels a live job when Claude Code passes the MCP result as plain text (`59da6bc`).

## 0.1.24 — 2026-09-25

- **New:** long Codex work runs async (codex_start / codex_result / codex_cancel + background waiter), so Claude keeps talking to the user while it runs; same enforcement and review as sync.
- **New:** optional ORCH eta=<N>m is recorded beside the actual duration; SessionStart shows this project's per-route median/p90 once 10 samples exist.
- **New:** every prompt carries a one-line reply-language reminder (recorded handoff language; none for English) to stop drift into English (`e38d962`).

## 0.1.23 — 2026-09-25

- **Telemetry:** Codex call events record why the call was made (reason, explicit or inherited) and route changes on a thread (`2ee5fcd`).

## 0.1.22 — 2026-09-25

- **Fix:** runtime state moves from %LOCALAPPDATA%\claude-agent-orchestrator (virtualized per app under the Store Claude app, invisible to other processes) to `PURSER_STATE_DIR` > `CLAUDE_CONFIG_DIR/purser` > `~/.claude/purser`; `purser_migrate_state.py` copies old state once (3b48442) (`3b48442`).

## 0.1.21 — 2026-09-25

- **Fix:** the handoff is no longer injected on `resume` (e.g. after an app restart), where the conversation is still in context; `compact`, `clear` and fresh `startup` unchanged (`3e4a274`).

## 0.1.20 — 2026-09-25

- **Fix:** the handoff records `User language:` and the post-compaction restore tells Claude to reply in it, since the English summary and handoff made Claude switch to English (`01faa15`).

## 0.1.19 — 2026-09-25

- **Fix:** protected paths match case-insensitively on Windows and macOS, and hook checks report edits to already-dirty or untracked files (`b4c1e81`); tests pass in a fresh checkout and no longer leave pytest cache dirs (`1e4d622`, `ccef73e`) (`ccef73e`).

## 0.1.18 — 2026-09-25

- **New:** after each Codex call a systemMessage shows the remaining quota, e.g. '⚓ **Purser:** Codex quota left: 5h 99% (resets 11:57) · 7d 54% (resets Sep 28)' (`310ef85`).

## 0.1.17 — 2026-09-25

- **Fix:** `/purser:optimize` builds the current setup by replaying the session's attachments, so an MCP server the user disabled (removed in `mcp_instructions_delta` / `deferred_tools_delta`) is no longer reported, and a non-initial skill listing adds skills instead of replacing the list (`1997c36`).

## 0.1.16 — 2026-09-24

- **New:** `/purser:optimize` reads the last 30 days of local transcripts (read-only, on demand, ~2 s) and reports: skills, plugins and MCP servers loaded on every request but never used; hidden non-interactive sessions by weighted-token share; duplicate skills; median reply length with a short-reply rule suggestion; repeated shell-command sequences worth one script; and the compaction advice status. `/purser:init` offers it at the end (`b355d5f`).

## 0.1.15 — 2026-09-24

- **Change:** the compaction notice must be the very last text of the turn, after every tool call, so a later tool call or reply cannot bury it (`1c46872`).

## 0.1.14 — 2026-09-24

- **New:** a routed Codex call shows a one-line systemMessage such as 'Purser: route=mechanical -> gpt-6-luna / high', because the call UI shows Claude's input, not the hook's routed model. Desktop only; mobile does not show systemMessage (`898c421`).

## 0.1.13 — 2026-09-24

- **Fix:** the handoff's Purser state block and the task-end trigger take a codex-reply's task from its result bundle when the prompt has no ORCH line, instead of recording task=unknown (`f7004a1`).

## 0.1.12 — 2026-09-24

- **Change:** the `codex-orchestrator` skill caps replies to the user at 5
  plain lines unless they ask for detail (result, one recommendation, at most
  one question; no tables or retelling), because every reply is re-read on
  each later request (`0a9f749`).

## 0.1.11 — 2026-09-24

- **Change:** the compaction notice adds "Nothing is lost." after the handoff
  line (`de0533a`).

## 0.1.10 — 2026-09-24

- **Change:** the compaction suggestion is now a separate closing block after
  a `---` line: context size, the estimated saving per later message
  (`1 - 75K / context`), a `/compact` call to action and the handoff path.
  A question Claude asks in the same reply is also written under Next in the
  handoff so it survives compaction (`33e090a`).

## 0.1.9 — 2026-09-24

- **Change:** the restored handoff tells Claude to skip listed files that are
  already in context, since Claude Code re-attaches recently read files after
  `/compact` (`5010083`).

## 0.1.8 — 2026-09-24

- **New:** compaction advisor step 2. After `/compact` or `/clear`, a
  SessionStart hook injects `purser-handoff.md` (historical reference, capped
  at 6,000 characters; on startup/resume only if written in the last 12
  hours). `/purser:init` creates the handoff template and prints its path
  (`d74e5e8`).

## 0.1.7 — 2026-09-24

- **New:** compaction advisor (step 1 of 2). On a user prompt, when the
  session context is >= 250K tokens and a phase boundary was crossed (>= 20
  min idle, a successful `git commit`, or a reviewed Codex result with a new
  `task=`), the UserPromptSubmit hook asks Claude to fill `purser-handoff.md`
  in Purser's per-project state directory (outside git) and to suggest
  `/compact` to the user. The hook writes a bounded "Purser state" block
  (recent Codex calls, commits, uncommitted count) with no model call, and
  does not repeat until the context grows 60K more. Settings: `[compaction]`
  in `~/.orchestrator/config.toml` (`enabled`, `threshold_tokens`,
  `idle_minutes`, `regrow_tokens`) (`03bc330`). A transcript replay of 65 desktop sessions
  estimated up to ~37% billable-weight saving at 250K.

## 0.1.6 — 2026-09-24

- **Change:** the `codex-orchestrator` skill sends exploration that would take
  Claude three or more read/search turns to one `Explore` agent on Haiku with a
  bounded answer; Claude verifies only the decisive lines. Each Claude turn
  re-reads the whole context, so exploration in a small side context is cheaper.

## 0.1.5 — 2026-09-24

- **Change:** the `codex-orchestrator` skill now sets the language rule: only
  replies to the user use the user's language; worker prompts, required
  reports, commits, state and repository documents are English, because
  non-English text costs more tokens and returns in every bundle.

## 0.1.4 — 2026-09-24

- **Fix:** hooks now read and write their JSON as UTF-8 on Windows instead of
  the console code page. Before, a bundle containing a character outside that
  code page (a failing Vitest tail's `❯`) crashed the PostToolUse hook, and
  Claude silently received the raw Codex result without the
  `HOOK_CHECK_FAILED` bundle; non-ASCII prompt text was also garbled, and could
  crash the Codex bridge with a lone surrogate. The Python locator now sets
  `PYTHONUTF8=1` for every hook and the bridge (`f1575e6`).

## 0.1.3 — 2026-09-24

- **Fix:** a `codex-reply` now resolves to the project its thread was created
  in. Before, it used the Claude session's current directory, so after the
  session moved into another git repository (for example a nested clone) the
  reply was denied `NO_LIVE_THREAD`. A recorded project that is no longer a git
  work tree is denied instead of falling back (`688f680`).
- **Change:** `route=normal`, `consequential` and `escalated` (gpt-6-sol) now
  require `basis=` naming the design choice left to the worker; without it the
  call is denied with `SOL_ROUTE_NEEDS_BASIS` and pointed to `route=subtle`
  (`363b1f5`).

## 0.1.2 — 2026-09-24

- **New:** low-quota warning. A fresh `codex` call is denied once with
  `LOW_CODEX_QUOTA` when a live 5h or 7d Codex window is at >= 90% used; resend
  with `reason=user_override` to proceed. Replies are never checked and a
  missing reading never blocks (`5adb4df`).

## 0.1.1 — 2026-09-24

- **Fix:** Reviewer-1 failures are now diagnosable: telemetry carries
  `reviewer1_error_code` and elapsed time, and agy's stderr tail is saved and
  linked from the finding. A non-zero agy exit under 60 s is retried once
  within the same timeout (`409fcaf`).

## 0.1.0 — 2026-09-23

- First plugin release (P6a-P6d): hooks, skill, Codex exec bridge,
  `/purser:check`, `/purser:init`, Python locator, cutover script.
