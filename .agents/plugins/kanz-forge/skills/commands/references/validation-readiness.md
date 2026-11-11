# Validation-aware delivery

Run DISCOVER → PLAN → IMPLEMENT → TEST → VALIDATE → REVIEW → DELIVER → REASSESS.
Read current main, all open PR heads and the issue inventory before choosing a
new task. Use the GitHub connector first. The read-only fallback is
`python -B .agents/plugins/kanz-forge/scripts/collect_validation.py --repo eighred/kanz`.
It paginates, reconciles counts, binds checks to revisions, and refuses a moving
main or PR inventory. Failed access is unavailable evidence, never success.
Do not paste tokens, ambient configuration or raw logs into a graph.

## Review the actual dependency contract

Supply a `validation_graph` to `scripts/delivery_evidence.py`, or a graph JSON
to the collector's `--contract` option. Keep transient snapshots in chat or
external scratch. They are observations, not another project-state database.
Inspect workflow triggers, job prerequisites, changed callers, runtime pins,
branch policy and explicit holds to establish each edge. A red job alone does
not block every task. Missing scope is unresolved, not permission to infer it.

The graph has `nodes`, `edges`, optional `main_revision` and `previous_states`.
Nodes have unique `id` and `kind` (`task`, `pr`, `gate`, `runtime`). Edges use
`from`, `to`, `kind` and a source `evidence` locator. `requires` blocks delivery;
`execution-requires` also blocks coding; `recovers` connects a recovery task to
the gate/runtime it repairs. Recovery is not an exemption from that task's own
validation. Reject unresolved edges and cycles.

Tasks need `scope_evidence`, a numeric `priority` (lower first), and an evidenced
`stage` containing `state: READY` or `IN_PROGRESS`. Holds have a `reason` and
`evidence`. A gate/runtime may be `critical: true`. Failure grouping uses a
reviewed `failure` with `id`, `kind`, and `evidence`, not naming similarity.
The classifier distinguishes observed DNS failure, source-bound advisory
evidence, dependent aggregates and unknown errors; a generic exit code or a
test named `fail` does not establish an application regression.

For checked nodes, `revision` is a full SHA. `validation` contains matching
`sha`, `expected` check IDs, `observed` check records (`id`, `sha`, `status`,
`evidence`), `inventory_complete: true`, and `inventory_evidence` identifying
the reviewed applicable workflow inventory. A skipped check needs an explicit
`skip_reason` in its expected record. Duplicate, absent, stale, canceled or
unexpectedly skipped evidence cannot permit delivery. The collector binds a
node's `subject` (`main` or `pr:N`) only when its reviewed revision still matches;
it never invents the expected inventory from observed green checks.

`DELIVERABLE` also requires task/PR `review` with `passed: true`, matching `sha`
and `evidence`. A PR requires `integration` with current `base_sha`, matching
`head_sha` and evidence, against the graph's `main_revision`. This means ready
for an authorized delivery action, not merged, deployed or fully accepted.
Issue closure still needs merged-main acceptance under the delivery skill.

## Reassess instead of filling a blocked queue

Report READY, IN_PROGRESS, BLOCKED, VALIDATION_FAILED, VALIDATION_PENDING and
DELIVERABLE separately, with reasons and evidence. Propagate failed or unavailable
prerequisites only along reviewed edges. Keep `execution_only` tasks visible,
but do not select another undeliverable feature merely because its files differ.
Executable recovery of critical shared gates ranks first, then other shared
recovery, then independent work by repository priority. Apply the existing file,
contract and active-worker ownership plan before dispatch. A legacy reducer
result with `validation_aware: false` is insufficient for task selection.

After a push, completed/failed gate, changed head/base, new artifact or cleared
hold, recollect and recalculate. `previous_states` records comparison inputs;
reported transitions retain supporting check/reason evidence. An old green
observation is not recovery on a new revision. If critical recovery needs an
unavailable upstream artifact, record its exact owner and prerequisite in the
existing issue and select only work with an independent validation path.
