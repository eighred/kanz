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

Classify the issue as open, partially implemented, externally blocked,
decision-needed, verified, superseded, or incorrectly closed. For a duplicate,
carry its unique acceptance into the authoritative issue and cross-link both.
For a mixed issue, close only the satisfied scope and link a concrete successor
for remaining work. Reopen a false closure when current acceptance demonstrably
fails. If evidence is unavailable, say `unverified` and keep the claim narrow.

Read [reconciliation criteria](references/reconciliation.md) for issue-body and
guard-owner checks when editing the tracker. Make individual, evidenced edits;
do not mechanically rewrite hundreds of issues. The output is an issue whose
title, labels, acceptance, blocker and source match current reality.
