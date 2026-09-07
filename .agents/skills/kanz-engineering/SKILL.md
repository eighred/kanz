---
name: kanz-engineering
description: Apply Kanz's institutional architecture and production-readiness standard when designing, implementing, or reviewing changes in eighred/kanz.
---

# Kanz engineering

Make the smallest change that completes a real production workflow while
preserving the boundaries and invariants in the repository `AGENTS.md`.

## Decision order

1. Establish ground truth from code, tests, git state and the relevant Issue.
2. Reuse an existing typed contract and ownership boundary.
3. Prefer deletion or extension over a parallel abstraction.
4. Keep execution deterministic and independent of intelligence interfaces.
5. Make failure, unknown state, tenant scope and operational consequence
   explicit.
6. Verify the behavior that changes, proportionate to its risk.

Do not create plan files, architecture documents, task boards or persistent
session notes. Put durable decisions and deferred work in the relevant GitHub
Issue; put implementation evidence in the PR.

For cross-plane, event-topology, persistence, scale-out or operator-interface
work, read [references/architecture.md](references/architecture.md). Ordinary
localized changes do not need that reference.
