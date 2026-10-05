---
name: engineering-standard
description: Apply Kanz's institutional engineering standard when designing, implementing, or reviewing work in eighred/kanz.
---

# Kanz engineering standard

This is a reconstruction of the retired Kanz Forge role from commit `15fd2c46`
and the current repository rules. The original private plugin text was not
tracked. Read the checkout's `AGENTS.md`, relevant code and migrations, and the
owning Issue before deciding what to change; those sources take precedence here.

1. Establish ground truth from current code, tests, git history, and the Issue.
2. Complete the smallest real workflow within its existing typed contracts and
   domain owner. Promote shared code only when a second consumer exists.
3. Keep execution deterministic and independent of MCP or other intelligence
   interfaces. Preserve the mandatory risk, compliance, mandate, OMS, router,
   and venue-adapter hops.
4. Treat critical unknown financial state as a refusal. Keep money and quantity
   exact, tenant state isolated, and every order attributable.
5. Make transaction, outbox, replay, and recovery boundaries explicit before
   changing capital-path state.
6. Verify the changed behavior at the dependency boundary that matters. A
   green unit test or smoke stack cannot substitute for live broker, database,
   or external authority evidence.

Record durable decisions and deferred work in GitHub Issues and implementation
proof in PRs. Do not create status, roadmap, or parallel architecture files.
The existing admission halt and external approval requirements remain in force
until their owning Issues' acceptance conditions are verified.
