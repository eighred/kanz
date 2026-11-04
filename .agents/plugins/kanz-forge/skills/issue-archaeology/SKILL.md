---
name: issue-archaeology
description: Reconcile a Kanz issue or PR against current main, linked history, guards, acceptance, and superseding work.
---

# Reconcile engineering state

Use this for a disputed closure, stale scope, duplicate, or backlog audit. Read
the current issue body, labels, decisive comments, linked PRs and merge commits;
then inspect the affected current-main files and guard exemptions. Use
path-scoped history to explain a migration or changed decision. Do not treat an
old title, green CI, or code presence as proof of original acceptance.

Turn each acceptance clause into a claim with an owner, required observation,
revision/estate, available evidence and remaining gap. Separate a code change
from installation, rollout, historical-data repair or an operational policy
decision; these may have different completion dates. Follow linked evidence to
its actual artifact instead of counting a repeated assertion as corroboration.

Classify the issue as open, partially implemented, externally blocked,
decision-needed, verified, superseded, or incorrectly closed. For a duplicate,
carry its unique acceptance into the authoritative issue and cross-link both.
For a mixed issue, close only the satisfied scope and link a concrete successor
for remaining work when tracker changes are authorized. If the user prohibits
new Issues, preserve unsatisfied criteria in the existing issue rather than
closing the whole claim or creating a successor anyway. Reopen a false closure
when current acceptance demonstrably
fails. If evidence is unavailable, say `unverified` and keep the claim narrow.

Resolve disagreement by evidence type: current code defines what is implemented;
accepted requirements define what is needed; observed tests/deployment define
what ran; explicit approval defines authority. None silently substitutes for
another. When a requirement changed, cite the decision and preserve the old
claim's disposition so future readers can distinguish supersession from repair.

Read [reconciliation criteria](references/reconciliation.md) for issue-body and
guard-owner checks when editing the tracker. Make individual, evidenced edits;
do not mechanically rewrite hundreds of issues. The output is an issue whose
title, labels, acceptance, blocker and source match current reality.
