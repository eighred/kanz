---
name: pr-delivery
description: Bring a Kanz PR through current-main review, verification, merge, and issue closure, or record its exact blocker.
---

# Deliver a Kanz PR

Inspect every open PR in the requested scope individually. Compare its head
with current main, linked issue, changed files, reviews, checks and follow-up
commits. Identify superseded changes and real conflicts before rebasing. A
green result for an older head does not verify an updated head.

For actionable work, update the branch, fix failures at their root, and run the
relevant local and CI gates. Merge only when the PR's accepted scope and required
authority are satisfied; then verify the merge commit on main before closing
the issue. For obsolete or duplicate work, preserve unique requirements in the
authoritative issue/PR, explain the relationship, and close the redundant PR.
For a genuine external blocker, leave one authoritative draft with a concrete
missing input and no implied production approval. Do not convert a draft
capital-path PR to ready or lift the admission halt from CI alone.

Report the exact head tested, merge commit or blocker, dependency proof, skipped
tests and linked issue disposition. Keep user worktree changes out of the PR.
