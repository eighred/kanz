# Native parallel work

Use this protocol only for a user-supplied `/work subagents N [targets]`. Parse `N` as a positive integer. The number is the requested concurrent worker count; use at most the host's available worker slots and state the effective count. If native subagents are unavailable, report that limitation and continue only with work the user has authorized for one agent. Do not launch a Python agent scheduler, another Codex session, or a background polling service.

## Select and assign

Read current AGENTS.md, Git status, main, open PRs and Issues, then reconcile each candidate with its acceptance and actual owner. Explicit targets still require this check; without targets rank actionable issues by the repository's capital, security and integrity priorities. Build a small task map in the chat: issue, dependency, owning files/contracts, worker, branch/worktree, tested SHA, CI state and next gate. Put durable findings and blockers in the matching Issue or PR, never a new status file.

Only dispatch tasks whose required inputs and decisions are available. Two tasks are independent only when their file ownership, typed contracts, infrastructure state and test assumptions do not conflict. Assign each coding worker a distinct issue, explicit file or module ownership, separate branch/worktree, base revision, acceptance and verification goal. Tell every worker that others are editing the repository, to preserve their changes, and to report overlaps before editing. Use `kanz_mapper`, `kanz_reviewer` and `kanz_security` for their bounded read-only roles; coding workers need a writable host worker. The parent owns cross-task decisions.

## Work while CI runs

Treat a CI result as evidence for its exact head SHA. Once a PR head enters validation, keep that branch fixed while independent workers continue in their own worktrees. Queue tasks that need its result, same files, a shared contract, or a merged revision. Observe required contexts and the full applicable check set on the exact head; a check queued or skipped unexpectedly is not green. If a worker changes the head or repairs a failure, its earlier CI evidence is stale and the affected checks must run on the new SHA. A failed worker leaves its scoped branch and Issue/PR evidence for repair; it does not block unrelated workers.

## Consolidate

Collect each worker's diff, tested SHA, commands, dependencies, skips and remaining risks. Compare every branch with current main and resolve overlaps in dependency order. The parent reviews and coordinates integration; workers do not merge each other's PRs. Require specific authorization for every merge and external-code adoption. Merge only after the exact head has the applicable required checks and reviews, then verify the merged main revision and the issue's remaining acceptance. Keep partial or externally blocked work in an Issue and draft PR. Report completed and unverified work separately.
