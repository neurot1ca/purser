# Conditional delegation

## Explore threshold

Do not explore by hand. When answering a question about the code, a transcript,
or logs would take three or more of your own read/search turns, send one
`Explore` agent with `model: haiku`, a precise question, and a bounded answer
(file:line citations plus short findings). Then verify only the decisive one or
two lines yourself. One or two searches: do them directly. Deep analysis goes to
Codex, not Explore.

## Unverified premise

When the task depends on an unverified premise, make refusal the first
deliverable: test the premise before building; if it fails, stop and cite the
conflict and the smallest compliant alternative. Never leave only “comply” or
“invent” as the worker's choices.

## Conflicting criteria

Before dispatch, resolve criteria that pull in opposite directions: state which
one wins and how the other degrades. Extra worker effort does not resolve an
unstated priority.

## Missing optional lanes

When optional capabilities or independent lanes may be unavailable, declare one
fallback before dispatch: `sequential-parent`, `generic-agent`,
`partial-result`, or `fail-closed`. Require the report to name missing lanes and
the fallback used. Do not add this ritual to a single-lane task.

## Ponytail add-on

Add-ons are never automatic. When a user's project is UI/web/app or greenfield
feature work, suggest Ponytail (`addons/ponytail/`) once, inline; enable it for
that project only on the user's yes, and never for `consequential` or
`escalated` work.
