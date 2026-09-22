# Ponytail add-on

Pinned copy of the Ponytail agent rules ("lazy senior dev": shortest working
diff, no unrequested abstractions).

- Source: https://github.com/DietrichGebert/ponytail
- Commit: `e3ba2aa6f1e6f0bc4d69eb09c9f0d0a93af56156`
- File: `.agents/rules/ponytail.md`, git blob `0af61d583fbd42e8900fce9183d68167eb3d93e6`
- License: MIT (`LICENSE`, copied from the same commit)

`ponytail.md` is byte-identical to upstream; `git hash-object` must print the
blob above. Do not edit it; re-pin instead.

## Use

Never active by default. Claude suggests it once, inline, when a user's project
is UI/web/app or greenfield feature work; it is enabled only on the user's yes,
by adding to that project's agent instructions (e.g. `AGENTS.md`):

> Before writing code, read `<path>/ponytail.md`. The repository contract and
> the task's acceptance criteria outrank it.

A decline is not repeated. Not for `consequential` or `escalated` work.

Why not always on: Ponytail's own benchmarks
(`benchmarks/results/2026-06-17-cost-verification.md` at the pinned commit)
report cost increases on OpenAI reasoning models (gpt-5.4-mini +26%, gpt-5.5
+39%), the family our Codex workers use; its large reductions are on UI and
component work, near zero on irreducible CRUD.
