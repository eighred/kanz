---
name: kanz-triage
description: Select or prioritize the next worthwhile task in eighred/kanz from code and GitHub evidence; use for backlog triage or "what next" requests, not implementation.
---

# Kanz triage

1. Inspect current git state, open PRs, open Issues and main's latest commit.
2. Verify candidate gaps against the minimum necessary code. Discard claims the
   repository disproves.
3. Keep only work that removes a production blocker or materially improves
   correctness, durability, security, recoverability, observability, scale or a
   complete operational workflow.
4. Prefer foundations over dependent features and vertical completion over
   several partial systems.
5. Search Issues before proposing new work. Reuse an existing Issue when its
   root cause and verification boundary match.

Return the surviving candidates in priority order, disproved candidates, and
one recommended next task with evidence. Stop before implementation unless the
user also requested implementation.
