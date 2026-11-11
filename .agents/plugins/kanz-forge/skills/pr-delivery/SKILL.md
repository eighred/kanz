---
name: pr-delivery
description: Bring a Kanz PR through current-main review, verification, merge, and issue closure, or record its exact blocker.
---

# Deliver a Kanz PR

Inspect every open PR in the requested scope individually. Compare its head
with current main, linked issue, changed files, reviews, checks and follow-up
commits. Identify superseded changes and real conflicts before rebasing. A
green result for an older head does not verify an updated head.

Resolve the delivery identity explicitly: repository, base branch, PR head,
merge base and local checkout. A similarly named branch, stale installed plugin
or another worktree is not the proposed change. Inspect all changed files and
the connected callers, configuration and guards needed to assess their effect.
Keep the PR description about its final behavior and current evidence; remove
abandoned scope instead of appending an unexplained history of plans.

For actionable work, update the branch, fix failures at their root, and run the
relevant local and CI gates. Merge only when the PR's accepted scope and required
authority are satisfied; then verify the merge commit on main before closing
the issue. For obsolete or duplicate work, preserve unique requirements in the
authoritative issue/PR, explain the relationship, and close the redundant PR.
For a genuine external blocker, leave one authoritative draft with a concrete
missing input and no implied production approval. Do not convert a draft
capital-path PR to ready or lift the admission halt from CI alone.

While one exact head is in CI, independent issues may proceed in separate
worktrees. Keep that PR head fixed until its checks finish; a new push needs
new evidence. Sequence work that shares files, contracts or live state.

Use the [routing table](../commands/SKILL.md) on the final diff, including
security review for changed trust boundaries and financial verification for
changed calculations. A review of an earlier scope is insufficient after a
new contract or permission is added. Compare the full applicable check set
with the expected workflow, including unexpected skips and missing jobs.
Use an event or bounded wait when available; while CI runs, continue authorized
independent work instead of repeatedly polling unchanged state. Carry already
granted merge permission for this specific PR forward, within its stated scope.
When reviewing a queue, use [delivery coordination](../commands/references/delivery-coordination.md)
to rank independent candidates. A green required-context list cannot override
an applicable security failure or an explicit hold.

Treat gates according to observed state:

- Pending means wait or continue independent authorized work; it is not approval.
- Missing, canceled or unexpectedly skipped means inspect trigger, path filter,
  permissions and job condition before deciding whether evidence is applicable.
- Failed means diagnose the cause and repair the responsible layer; do not
  disable the check or accept a retry without explaining what the retry proved.
- Green means the check verified its recorded revision and scope, not later
  edits, a different base or an untested deployment.

Before merging, re-read current head/base, applicable checks, review decisions
and unresolved threads. Use an expected-head condition when the merge API offers
one. If main advanced or conflict resolution changed the result, obtain fresh
evidence required by the repository's branch policy. Do not bypass protections.

After merging, identify the merge SHA and verify the relevant main jobs and
acceptance there. If deployment or installation is part of the task, verify the
artifact/version actually loaded, not merely the source file. A post-merge
failure keeps acceptance open; diagnose it, preserve the evidence and propose
the smallest authorized repair or rollback. Never relabel failed delivery as
complete because the merge itself succeeded.

Report the exact head tested, merge commit or blocker, dependency proof, skipped
tests and linked issue disposition. Keep user worktree changes out of the PR.
