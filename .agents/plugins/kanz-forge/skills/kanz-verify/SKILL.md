---
name: kanz-verify
description: Verify a Kanz change or issue acceptance without confusing local tests, CI, real dependencies, and production authority.
---

# Choose evidence that proves the claim

Read [the evidence ladder](references/evidence.md) when deciding the test level
or reporting completion. Start with the changed behavior and its discriminating
failure case; expand to affected integration and architecture gates, then CI.
Do not rerun broad suites without a remaining risk or required gate.

Build the verification set from the impact map: changed behavior → failure case
→ owning check → dependency → expected observation. Include connected consumers
and configuration outside the diff when they establish the boundary. A test
copied from the implementation is weak evidence; choose an independent oracle,
invariant or observable outcome. Fix the expected result before inspecting the
new output so an unexpected result cannot redefine success.

Capture the tested commit, exact command, package inventory, exit result,
dependency identity, and skipped tests. A green `fakeBus` test proves no
JetStream envelope acceptance. A skipped Postgres test proves no RLS, and a
superuser bypasses it. A passing guard proves only what its matcher resolves;
mutate a representative artifact when its reach matters. Windows without CGO
does not establish race freedom; inspect the Linux CI run and its skips.

Establish the expected package/job inventory before execution. Account for each
as passed, failed, skipped, not run or blocked; an exit code and a tail containing
no failures do not prove completion. If output is truncated, retrieve the full
artifact or rerun a bounded missing group. If a dependency is unavailable, use
local logic checks only for the claims they actually cover and keep the boundary
claim unverified. Never substitute a production endpoint for a missing fixture.

For deployment or external integrations, identify the exact estate, revision,
operator authority, and observed outcome. Treat code completion, live
verification, and permission to trade as separate claims. Leave an issue open
when its acceptance needs evidence that does not exist; state the missing input
precisely. Never place credentials in test files, logs, reports, or agent context.

For a Forge capability or `/doctor`, distinguish **implemented** (the
entrypoint and supporting files exist), **configured** (this host exposes its
tools and dependencies), **operational** (a representative operation succeeded
with observed evidence), **incomplete** (observed failure or missing setup), and
**unverified** (no suitable probe evidence). Report each with
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

When a check fails, classify a product defect, test defect, missing setup or
infrastructure failure from the actual logs before repairing it. A retry may
diagnose a transient failure but cannot establish that the defect was fixed.
After a repair, rerun the affected gate and required checks on the changed head.
Report the strongest supported claim, the first missing proof, and the next
concrete action; do not blend static review and execution into one green label.
