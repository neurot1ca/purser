# Changelog

Every plugin release (`.claude-plugin/plugin.json` version) gets an entry here
before it is published.

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
