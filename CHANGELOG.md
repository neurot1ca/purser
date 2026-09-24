# Changelog

Every plugin release (`.claude-plugin/plugin.json` version) gets an entry here
before it is published.

## 0.1.3 — 2026-09-24

- **Fix:** a `codex-reply` now resolves to the project its thread was created
  in. Before, it used the Claude session's current directory, so after the
  session moved into another git repository (for example a nested clone) the
  reply was denied `NO_LIVE_THREAD`. A recorded project that is no longer a git
  work tree is denied instead of falling back (`fd8e1d1`).
- **Change:** `route=normal`, `consequential` and `escalated` (gpt-6-sol) now
  require `basis=` naming the design choice left to the worker; without it the
  call is denied with `SOL_ROUTE_NEEDS_BASIS` and pointed to `route=subtle`
  (`d5c6a21`).
- **Docs:** source-verified ECC study, `docs/research/ECC_SOURCE_STUDY_20260924.md`
  (`281d2b7`).

## 0.1.2 — 2026-09-24

- **New:** low-quota warning. A fresh `codex` call is denied once with
  `LOW_CODEX_QUOTA` when a live 5h or 7d Codex window is at >= 90% used; resend
  with `reason=user_override` to proceed. Replies are never checked and a
  missing reading never blocks (`6c8ed86`).

## 0.1.1 — 2026-09-24

- **Fix:** Reviewer-1 failures are now diagnosable: telemetry carries
  `reviewer1_error_code` and elapsed time, and agy's stderr tail is saved and
  linked from the finding. A non-zero agy exit under 60 s is retried once
  within the same timeout (`6261d62`).

## 0.1.0 — 2026-09-23

- First plugin release (P6a-P6d): hooks, skill, Codex exec bridge,
  `/purser:check`, `/purser:init`, Python locator, cutover script.
