# Review playbook

## Review and acceptance

Implementation and review may use the same model.

For mutations, review the delta, then changed behavior and invariants, then
material callers, callees, and boundaries. For read-only work, start with cited
sources, method, and result. Expand evidence only for concrete
uncertainty, failed checks, disagreement, dependency, or risk.

For a worker's causal claim, see [attribution verification](conditional_review.md).

Reviewer-1 is independent input, not acceptance. Claude fully reviews all
consequential and escalated work. Request repair only for a material defect,
failed criterion, contract violation, or required validation gap. After repair,
review the new delta, blocking finding, and newly affected boundaries.

When relevant, Claude runs `go test ./...` and `go vet ./...` from `go/` once
before acceptance. Workers run focused validation, never repeated full suites.
Repeat an expensive successful check only if repair could invalidate it.

### One-turn AUDIT

On `claude_review_mode: AUDIT`, use one Claude turn to check the bundle's task
contract, reviewer result or eligible skip, worker validation, delta summary,
and safety fields. Accept and commit only with sufficient evidence and commit
authority; otherwise decline with the specific gap. Reopen raw diffs or shell
review only for concrete inconsistency. Send Reviewer-1's required changes
through the existing thread; do not edit worker code yourself.

Close every review with exactly one typed verdict and name the evidence that
decides it:

- `VERIFIED` — the claimed effect was observed, not merely reported.
- `NOT VERIFIED` — the claim failed or the evidence contradicts it.
- `INCONCLUSIVE` — the available check could not decide; this is not acceptance.

Proceeding after `INCONCLUSIVE` is a user decision. Never round it up to success.
