# Repository engineering

Use this shared workflow for work selection, branch recovery, discovery and
planning. The repository's purpose and current implementation define the work;
Issues, branches, PRs and checks are evidence and delivery mechanisms. Apply
[validation readiness](validation-readiness.md) to the resulting candidates.
This reference does not grant merge, installation or deployment authority.

## Discover before starting another change

Read the current checkout identity and dirty paths, current main, open PRs and
relevant Issues. Inspect `git worktree list --porcelain` and local/remote refs
with `git for-each-ref` before creating a branch. Fetch when freshness matters;
do not prune or delete work as part of discovery. A local main or installed
plugin can lag current remote main. Compare their actual revisions and scoped
diffs before claiming a capability is missing or porting an older implementation.

For each candidate, resolve the observable gap, owning component, current source
evidence, acceptance, existing branch/PR/Issue, dependencies and discriminating
check. Keep this brief in chat; retrieve durable decisions from Issues and PRs.
An issue-free defect found in source is eligible work after confirming its
reachable behavior and searching for an existing owner. Record it under the
repository's issue rules before delivery. A TODO, spelling preference, failed
check name or old branch alone does not establish a defect.

Reconcile duplicates and partial implementations before ranking. Prefer a
deliverable continuation over starting equivalent work: at comparable priority
and impact, repair an existing PR, recover useful branch work, then start a new
change. Executable critical-gate recovery retains its precedence. Do not choose
a low-value cleanup because it is easy, or create competing branches for the
same acceptance. Respect explicit targets and holds; an unrelated queue does
not enlarge a narrowly scoped request.

The existing validation graph accepts optional task `continuation` metadata:
`{"kind": "pr", "evidence": "PR URL, reviewed head SHA and remaining acceptance"}`.
Use `kind: "branch"` for branch recovery. Supply this only after reviewing the
actual diff and current ownership. It breaks otherwise equal selection ranks;
it cannot clear a blocker, authorize delivery or replace current-head checks.

## Stop repeating unchanged failures

Read the owning issue's latest substantive evidence before reproducing a known
failure. Separate a repairable regression from a missing upstream artifact,
operator decision, access, platform support or merely pending validation.
Retain the cause, affected input identity, observed revision, owner, next action
and **re-entry condition** in the existing Issue/PR. A re-entry condition names
what must change: for example an available reviewed runtime artifact with its
checksum, restored runner access, or an explicit policy decision.

Revalidate that condition using the smallest relevant read-only observation.
If it is unchanged, keep the affected scope blocked and choose independent
work with a real validation path. Do not restart the full qualification,
try alternate installation routes, or reopen the same diagnosis on each
`/work`. A new date, chat, branch name or downstream failure is not changed
input. A bounded retry needs evidence of a transient condition and a stated
observation it will resolve. Never weaken the refusing gate.

Represent unavailable prerequisites as evidenced runtime/gate holds and attach
`execution-requires` to a recovery task that cannot begin without them. Use
`requires` for delivery-only dependencies. Inspect these edges before marking
a recovery task READY; calling a task "repair" does not make its input available.
After a changed artifact, check, head/base or decision, recollect and reassess.

## Recover branches and PRs

For `/branches`, inventory first; for an explicit branch or `/branches fix`,
recover the best feasible in-scope candidate through the shared workflow.
Match refs to current PR heads and owning worktrees. Inspect merge-base,
unique commits and actual diffs against current main; commits already applied
under another SHA may leave no remaining behavior. Age, a deleted upstream or
a missing PR is a review signal, never deletion authority.

Classify each relevant branch as active, recoverable, blocked, superseded or
unresolved. Preserve unique work and dirty/staged files. Reuse its current PR
where possible; sequence overlapping changes and generated inputs. Create an
isolated checkout when existing worktrees cannot be used without contaminating
someone else's change. Resolve conflicts by intended behavior, then run the
affected checks on the integrated revision. Do not force-push, discard edits,
delete branches or close competing work merely to tidy the inventory.

For untargeted `/ship`, continue the active change when identifiable; otherwise
review the open PR queue and choose a feasible recovery under the same ranking.
Follow [PR delivery](../../pr-delivery/SKILL.md) for authority and acceptance.
Report historical red checks separately from current unrun or blocked checks.

## Audit, plan and evolve

`/audit` and `/secure` discover source-backed gaps; `fix` additionally authorizes
scoped repairs. `/secure` uses the security skill's modes and isolation rules.
`/evolve` proposes improvements from Kanz's purpose, existing capabilities and
observed user/operational needs. Separate a demonstrated gap from a hypothesis;
do not invent product requirements or adopt dependencies through a proposal.
`/evolve implement <target>` follows the accepted target through `/work`.

`/plan` orders evidenced work by impact, dependencies and validation readiness.
Return the next executable outcome, existing owners and unresolved decisions in
chat or existing Issues. It neither dispatches work nor writes a repository
roadmap/status file. Discovery modes do not silently implement proposals.
An approved task uses the same owner, implementation, verification and delivery
skills as `/work`; these commands do not introduce separate engines or runtimes.

## Repository structure is a contract

For naming work, inspect existing layout branches/PRs before moving anything.
Follow AGENTS.md's directory naming rule. Preserve established skill/plugin identifiers,
tool discovery directories, generated/vendor paths, package identities, wire
names and deployed resource names. Follow the nearest established owner rather
than creating generic managers, utility layers or one-file directory trees.

A directory move requires an old-to-new consumer map: imports/module replaces,
code generation, build contexts, workflow paths/filters, scripts, deployment,
architecture guards and documentation. Preserve sibling build relationships
until all their consumers can change together. Separate moves from semantic
edits, detect collisions on Windows, and exercise the guard with a representative
violation. Reuse an existing migration instead of producing a second rename.
An unqualified large rename remains pending; naming consistency is not proof
of build, runtime or operational compatibility.
