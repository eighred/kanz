---
name: kanz-verify
description: Verify a Kanz change or issue acceptance without confusing local tests, CI, real dependencies, and production authority.
---

# Choose evidence that proves the claim

Read [the evidence ladder](references/evidence.md) when deciding the test level
or reporting completion. Start with the changed behavior and its discriminating
failure case; expand to affected integration and architecture gates, then CI.
Do not rerun broad suites without a remaining risk or required gate.

Capture the tested commit, exact command, package inventory, exit result,
dependency identity, and skipped tests. A green `fakeBus` test proves no
JetStream envelope acceptance. A skipped Postgres test proves no RLS, and a
superuser bypasses it. A passing guard proves only what its matcher resolves;
mutate a representative artifact when its reach matters. Windows without CGO
does not establish race freedom; inspect the Linux CI run and its skips.

For deployment or external integrations, identify the exact estate, revision,
operator authority, and observed outcome. Treat code completion, live
verification, and permission to trade as separate claims. Leave an issue open
when its acceptance needs evidence that does not exist; state the missing input
precisely. Never place credentials in test files, logs, reports, or agent context.

For a Forge capability or `/doctor`, separate four claims: **implemented** (the
entrypoint and supporting files exist), **configured** (this host exposes its
tools and dependencies), **operational** (a representative operation succeeded
with observed evidence), and **incomplete** (the missing step). Report each with
its scope and evidence; a manifest check proves implementation structure, not
Chat routing, native delegation, live access or completion of an audit. Inspect
tools before attempting a probe and avoid mutations for a status check.

For instruction changes, exercise representative tasks through the actual
workflow: skill selection, source traversal, output boundary and missing-tool
behavior. A scenario checked by reading instructions is static validation;
claim live operation only when the host actually ran it. For parallel work,
worker completion and conflict/failure recovery are different claims. Installation
discovery, CLI discovery and the current Chat's loaded skill version are also
separate. Recheck changed dependencies or revisions before reusing prior evidence.
