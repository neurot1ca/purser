# Purser

A frugal orchestrator for AI coding work. Claude frames and reviews; Codex and
Gemini do the work. Every delegation is baselined, attributed to its worker, and
independently reviewed by a model of a different family — and the orchestrator
counts every token it spends, because saving Claude's context and time is the
reason it exists.

The user runtime requires Node.js for the MCP launcher and uses the bundled Go
binary for hooks and Purser commands. Python is development-only.

## Status

Extracted on 2026-09-21 from `polymarket-btc5m-lab` (branch
`phase-2-live-strategy-spec` at `d3ef816`), with its history: 42 commits
stitched from the founding branch `claude/orchestrator-hook-layer` and the
branch that carried the layer afterwards. [docs/PROVENANCE.md](docs/PROVENANCE.md)
maps every commit here to its source commit.

The layout is transitional. Files keep their original paths
(`.claude/hooks/orchestrator/`, `.claude/skills/codex-orchestrator/`) so that
behaviour is byte-identical to the source; the full test suite (314 tests)
passes standalone.

## Known coupling — the first work here

- The engine derives the project root from its own file location
  (`parents[3]` / `parents[2]` in `context_io_hook.py`, `obsidian_projection.py`,
  `install_claude_task_telemetry.py`, `reviewer.py`, `telemetry.py`). It must use
  the session's git root before Purser can serve any other repository.
- Protected paths, authority files and the vault project are still the source
  project's. They move into a per-project profile (plan phases P2-P3).

## Documents

- [Roadmap](docs/ROADMAP.md) — every phase of both tracks with its status; current task in [NEXT.md](NEXT.md)
- [Token reform spec](docs/ORCHESTRATION_REFORM_SPEC.md) — the original saving plan and its review rounds
- [Profile plan v2](docs/ORCHESTRATOR_PROFILE_PLAN.md) — design decisions and sequence
- [Operations reference](docs/ORCHESTRATOR_OPERATIONS.md) — transport, recovery, and what moved out of the skill
- [Research](docs/research/) — surveys of open-source orchestrators and a Codex review of the plan
- `scripts/orchestration_token_audit_0.py` — measures tokens and the tool-batching ratio from transcripts

## Commands

- `/purser:gear-up` and `/purser:gear-down` set the current project's Codex gear for new jobs.
