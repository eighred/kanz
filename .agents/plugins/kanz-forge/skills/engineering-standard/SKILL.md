---
name: engineering-standard
description: Implement a Kanz issue or code change within its owning domain, with targeted scope and durable state boundaries.
---

# Implement the real workflow

Start from the current `AGENTS.md`, issue acceptance, affected code, migration,
and architecture guard. Read `git log -- <path>` or `git log -S <symbol> -- <path>`
when an odd branch, exemption, or compatibility path has a reason the current
code does not show. Search live callers before deleting or promoting code.
Avoid a repo-wide tour when the owner is already clear.

Separate the requested behavior from the proposed mechanism. Before adding code,
check whether existing behavior, configuration, or a smaller repair satisfies
the request. State the observable before/after result and the failure that would
make the change unsafe. Do not turn an example implementation into a requirement.

Draw the state transition and failure boundary before editing: command versus
FACT, transaction/outbox, redelivery, restart, tenant scope, and unknown
financial state. For capital-path changes, read
[capital state](references/capital-state.md). Use the canonical Decimal and
existing typed contracts. Make the smallest change that completes the accepted
workflow; a speculative abstraction or unrelated cleanup needs its own issue.

Before adding a helper, search the owner and its actual consumers for an existing
implementation. Prefer, in order, the existing owner/helper, standard-library or
native-host behavior, an already approved dependency, then new code. Evaluate
semantic fit at each step; fewer lines do not justify hidden behavior changes.
Keep a helper local until a second consumer establishes shared
ownership. Preserve public behavior during refactors; separate intended behavior
changes from moves so a reviewer can see both. Prefer an explicit branch or typed
contract over an abstraction whose only consumer is the new code. Check error
propagation, resource cleanup, cancellation and retry boundaries where relevant.

For directory naming or layout work, use the
[structure contract](../commands/references/repository-engine.md#repository-structure-is-a-contract).
Reconcile existing migration branches first, follow the owning ecosystem's names,
and verify every path consumer before treating a move as complete.

Use [change review](references/change-review.md) for a new boundary, persistence
change, operational control or removal. Select only the applicable rows. It maps
failure modes to implementation decisions and discriminating checks, including
mixed-version rollout, ambiguous external results and policy enforcement.

For compliance-related work, trace requirement or approved policy → versioned
decision → enforcement before side effects → attributable audit fact. A report
or dashboard is not admission control. Distinguish enforcing an approved rule
from interpreting law or inventing policy; an unresolved definition blocks that
decision, not independent mechanical work. Use current primary sources when a
legal claim matters, and retain the source/version without copying licensed data.

Place a lasting invariant in the closest meaningful test or architecture guard
when the old failure could recur. A guard must fail for a representative
violation and must not silently bless a dead exemption. Ask `kanz-verify` to
choose evidence for the actual risk boundary. Record decisions and deferred work
in Issues, not a second architecture or progress document.

## Turn an ordinary request into an implementation brief

Start with the behavior the human needs. For a local code change inspect the
definition, callers, types and nearest tests (use gopls when available). Expand
to Git history or repository-wide search only to answer a specific uncertainty.
Do not load financial references for unrelated UI, build or tooling changes.

For financial changes, use [the design evidence guide](references/design-evidence.md)
before choosing an algorithm. Return the smallest decision-useful brief:
what Kanz must do, what it already does, the specific gap, and the next change
with its proof. State material unknowns. Skip this formal brief for trivial edits.

Use the [commands routing table](../commands/SKILL.md) to add focused security,
financial or delivery work when the change reaches those boundaries. Finish with
a diff review against acceptance: no unrelated churn, obsolete compatibility
paths, duplicate implementation, undocumented behavior change or unexplained
generated output. A useful review explains why the chosen change is sufficient;
line count and stylistic uniformity alone do not establish maintainability.

Before handing off, identify the operational effect of a failure after each
side effect and how the system detects and recovers from it. State any migration
or rollout ordering needed by current consumers. Remove temporary probes from
the diff, preserve unrelated edits, and give verification the behavior to
disprove rather than a request to confirm that the implementation works.
