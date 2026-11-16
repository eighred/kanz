---
name: repository-organization
description: Review Eighred repository ownership, directory naming, component documentation and path migrations against actual build and runtime consumers.
---

# Organize an Eighred repository

Read the repository's AGENTS.md and [Eighred organization standard](references/eighred-standard.md).
The standard guides placement and naming; AGENTS.md owns Kanz's platform boundaries.
Use this skill for a layout review, component README reconciliation or directory
migration. An ordinary implementation stays with its existing owner.

Start with the tracked tree, applicable instructions and current branch/main.
Reconcile existing layout branches and PRs using
[repository engineering](../commands/references/repository-engine.md).
Account for every README in the requested scope, including retained documents;
check purpose, ownership, real entrypoints, configuration, dependencies and limits
against the component's source. Keep the inventory in the chat or owning PR.

For each proposed move, identify the responsibility it clarifies and trace both
literal and constructed consumers: imports, module replacements, generated SDKs,
workspace members, binary output names, build contexts, CI paths and filters,
deployment references, scaffolds, guards and local links. Distinguish filesystem
names from package, wire, image, service and stored-data identities. Inspect
Windows case collisions and preserve dirty files and held worktrees.

Apply moves and their consumers together. Keep path changes reviewable apart
from behavior changes. Do not add empty layers, copy a template tree, collapse
independent deployment owners or infer that every logical capability needs a
service. Use [stack routing](../stack-routing/SKILL.md) for disputed ownership.

Run the repository's naming and documentation checks, then the affected real
generation/build/test entrypoints. In Kanz these include the directory guard in
`kanz/test/arch/repository_directory_names_test.go` and `tools/check_readmes.py`.
Exercise a representative forbidden path and a broken documentation link.
Use [verification](../kanz-verify/SKILL.md) to distinguish local source checks,
complete CI and deployment acceptance. Record unresolved consumers and holds in
the existing Issue/PR; a clean search is not proof of runtime compatibility.
