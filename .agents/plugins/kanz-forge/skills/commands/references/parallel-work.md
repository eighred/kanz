# Native parallel work

Use this protocol for `/work subagents N [targets]` or an explicit request for parallel agents. Parse `N` as a positive integer. The number is the requested concurrent worker count; use at most the host's available worker slots and state the effective count. For a natural-language request without a count, choose only enough workers for independent available tasks. If native subagents are unavailable, report that limitation and continue with authorized single-agent work. Do not launch a Python agent scheduler, another Codex session, or a background polling service.

## Select and assign

Read current AGENTS.md, Git status, main, open PRs and Issues, then reconcile each candidate with its acceptance and actual owner. Explicit targets still require this check; without targets rank actionable issues by the repository's capital, security and integrity priorities. Build a small task map in the chat: issue, dependency, owning files/contracts, worker, branch/worktree, tested SHA, CI state and next gate. Put durable findings and blockers in the matching Issue or PR, never a new status file.

Only dispatch tasks whose required inputs and decisions are available. Two tasks are independent only when their file ownership, typed contracts, infrastructure state and test assumptions do not conflict. Assign each coding worker a bounded task, explicit file or module ownership, base revision, acceptance and verification goal. Independent issues use separate branches/worktrees; subtasks for one change may share a checkout when ownership is disjoint and the parent owns integration. Shared manifests, generated files and contracts belong to one owner; queue their consumers until that interface is settled. Tell every worker that others are editing the repository, to preserve their changes, and to report overlaps before editing.

Read the relevant plugin `kanz_mapper`, `kanz_reviewer` or `kanz_security` profile and pass it to a native general/explorer worker for bounded read-only questions. Use named roles only if the host already exposes them; plugin profile files alone do not register roles. Coding workers need a writable host worker. Give workers only the necessary acceptance, paths, evidence and constraints; the parent owns cross-task decisions and integration.

## Work while CI runs

Treat a CI result as evidence for its exact head SHA. Once a PR head enters validation, keep that branch fixed while independent workers continue in their own worktrees. Queue tasks that need its result, same files, a shared contract, or a merged revision. Observe required contexts and the full applicable check set on the exact head; a check queued or skipped unexpectedly is not green. If a worker changes the head or repairs a failure, its earlier CI evidence is stale and the affected checks must run on the new SHA. A failed worker leaves its scoped branch and Issue/PR evidence for repair; it does not block unrelated workers.

Track each task in the current chat as ready, working, awaiting-CI, blocked,
ready-to-integrate or complete, with the dependency that permits its next move.
These are coordination states, not a new persisted task board. Observe CI with
available events or bounded waits and report meaningful changes only. A worker
waiting for CI may take another independent scope only after handing off its
fixed SHA and pending gate. If no independent task remains, waiting is correct.

When a worker fails, preserve its diff and inspect the failure before retrying.
Reassign only after ownership is released; never start two repair writers on
the same files. Resolve an overlap by choosing one owner or sequencing changes,
not by accepting whichever write finishes last. A parent interruption leaves a
recoverable summary of branch, revision, ownership, unfinished checks and next
step in the chat or existing PR.

## Consolidate

Collect each worker's diff, tested SHA, commands, dependencies, skips and remaining risks. Compare every branch with current main and resolve overlaps in dependency order. The parent reviews and coordinates integration; workers do not merge each other's PRs. Apply the current permission rules and carry existing specific authorization forward. Merge only after the exact head has the applicable required checks and reviews, then verify the merged main revision and the issue's remaining acceptance. Keep partial or externally blocked work in the existing Issue and draft PR. Report completed and unverified work separately.
