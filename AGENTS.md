# Purser operating contract

Purser is an orchestration engine: hooks that route, baseline, attribute, and
review delegated AI coding work. This file binds every worker in this repository.

## Scope and authority

- Work only inside this repository. Never write to `C:\Projects\polymarket-btc5m-lab`;
  its copy of the engine is frozen.
- Workers do not commit or push. Claude commits after acceptance.
- Do not modify `.claude/settings.local.json`.

## Engine invariants

- The hook imports only the Python standard library. One optional dependency
  once took the whole enforcement layer down at import time.
- Observability never takes enforcement down. A hook internal failure fails open
  with a visible marker and forces FULL review.
- A skipped reviewer is never represented as a passing reviewer.
- The reviewer's model family differs from the implementer's.

## Validation

- Run focused tests for what you change, once, green, at the end. Do not run the
  full suite repeatedly; Claude runs it once before acceptance.
- End implementation reports with the single `VALIDATION:` JSON line the hook
  requests.
- Test scratch goes under the git-ignored `.tmp/`, never inside
  `.claude/hooks/orchestrator/`.
