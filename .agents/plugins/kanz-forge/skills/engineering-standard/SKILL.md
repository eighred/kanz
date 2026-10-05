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

Draw the state transition and failure boundary before editing: command versus
FACT, transaction/outbox, redelivery, restart, tenant scope, and unknown
financial state. For capital-path changes, read
[capital state](references/capital-state.md). Use the canonical Decimal and
existing typed contracts. Make the smallest change that completes the accepted
workflow; a speculative abstraction or unrelated cleanup needs its own issue.

Place a lasting invariant in the closest meaningful test or architecture guard
when the old failure could recur. A guard must fail for a representative
violation and must not silently bless a dead exemption. Ask `kanz-verify` to
choose evidence for the actual risk boundary. Record decisions and deferred work
in Issues, not a second architecture or progress document.
