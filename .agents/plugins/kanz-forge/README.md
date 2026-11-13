# Kanz Forge

Kanz is Eighred's institutional investment operating system. Forge is its
repository-owned engineering workflow for Codex Chat: it understands current
code and unfinished changes, selects useful work, and carries it through review
and verification using the existing component owners and host tools.

## Work from one request

| In Codex Chat | Behavior |
|---|---|
| `Kanz /work #1234` | Resolve the issue through PR delivery, merged-main verification and issue closure. |
| `Kanz /work` | Select feasible work from source, branches, PRs and Issues; prefer completing equivalent existing work. |
| `Kanz /work subagents 2` | Request two workers for independent tasks, bounded by host capacity. Issue numbers may follow. |
| `Kanz /branches` | Inspect unfinished branches and ownership; `fix` or an explicit branch starts recovery. |
| `Kanz /ship #1234` | Review and repair that PR, verify its final head, and merge with specific authorization. |
| `Kanz /audit [fix]` | Find evidenced engineering gaps; `fix` also implements scoped repairs. |
| `Kanz /secure [fix]` | Inspect security boundaries; `fix` repairs confirmed feasible defects. |
| `Kanz /evolve` | Propose justified improvements; `implement <target>` works on an accepted scope. |
| `Kanz /plan` | Order current work by impact, dependencies and validation readiness. |
| `Kanz /doctor` | Inspect installation, available host tools, and observed capability evidence. |
| `Kanz /status` | Report current work, blockers and next actions without repairs or test runs. |
| `Kanz /help all` | Show specialist commands and their scope. |

`/work` and `/ship` inspect the actual change and select skills automatically.
Identity, permissions, tenant isolation, signing, and release changes trigger
security review. Money, valuation, portfolio attribution, and backtests trigger
financial verification. Cross-component changes trigger caller and contract
tracing. The user does not need to invoke every specialist separately.

Each skill produces a decision and evidence for the next step. Financial
guidance helps implement institutional pricing, risk and execution methods;
it does not generate investment-analysis deliverables or assert undocumented
proprietary algorithms. Policy guidance follows the actual enforcement point
and audit record. Relationship tracing connects affected contracts and consumers
to their verification checks. A missing critical edge or skipped dependency
stays unresolved, even if the changed function's unit test passes.

These are instructions dispatched by the installed
[commands skill](skills/commands/SKILL.md), not registered terminal commands.
The explicit skill invocation is `$kanz-forge:commands /work #1234`. The former
`python forge.py ...` launcher is retired; use the current Chat directly.

## Architecture and coordination

[AGENTS.md](../../../AGENTS.md) owns Kanz's platform constraints. Forge's
[skills](skills/commands/SKILL.md) own reusable workflows and [commands.json](skills/commands/commands.json)
maps requests to them. The specialist profiles provide
[mapping](agents/kanz_mapper.toml), [review](agents/kanz_reviewer.toml), and
[security](agents/kanz_security.toml) instructions. The coordinator reads the appropriate
profile and passes its instructions to a native worker. Plugin profiles do not
automatically register named Codex agent types, and no `.codex/agents` copies
are required. There is one source to edit, under this plugin.

[Repository engineering](skills/commands/references/repository-engine.md) is
the shared discovery and selection workflow. It reconciles existing branches
and PRs before opening competing work, uses changed-input re-entry conditions
for known blockers, and traces path consumers for directory standardization.
New project-owned directories use snake_case where no ecosystem/discovery
contract requires another name. Existing layout migrations are reviewed and
reused rather than recreated. These instructions do not rename the repository
or qualify an untested migration by themselves.

The [parallel protocol](skills/commands/references/parallel-work.md) assigns
ownership, acceptance criteria, dependencies, and a return contract before
dispatch. Only the root dispatches; workers never spawn other workers, including
writable coding workers. The root preserves one shared limit and passes the leaf
restriction with every assignment. These instructions do not revoke host tools.
Independent edits use disjoint files in a shared checkout or separate
worktrees. Shared contracts, generated artifacts, infrastructure state, and
integration changes are sequenced. Worker failure or incomplete evidence keeps
the dependent task blocked; it does not become success by omission.

While one commit is in CI, workers can continue independent authorized work.
The coordinator associates each pipeline with its PR and exact commit, keeps
that head fixed, and revalidates integration against current main. A new push
requires new checks. No polling daemon, second task board, external scheduler,
or service credentials are introduced. Native tool availability and capacity
determine whether parallel execution is possible.

[Delivery coordination](skills/commands/references/delivery-coordination.md)
uses live evidence to separate blockers from independent work. The optional
offline reducer checks a reviewed snapshot for stale CI evidence, queue holds,
ownership conflicts and missing local builder proof. It does not orchestrate
workers or authorize delivery.

## Context and memory

Stable constraints live in AGENTS.md; decisions and blockers in Issues; review
and verification evidence in PRs; implementation history in git. Retrieval
starts with current source and follows only the relevant links. Historical
conclusions must be checked against the current revision. Forge does not copy
Codex conversation memory or maintain a separate project-state database.

[Version-aware documentation](skills/commands/references/context-and-evidence.md#check-version-specific-documentation)
starts at official releases, versioned documentation and tagged source. The bundled
[public upstream lookup](scripts/upstream.py) discovers current Go releases and
GitHub release/default-branch identities without Context7, accounts, API keys or
third-party packages. Failures remain unknown; a discovered version is not an
approved upgrade. See [dependencies](DEPENDENCIES.md#public-upstream-and-documentation-access).

The optional [receipts utility](scripts/receipts.py) preserves existing pricing
test history at `~/.kanz-forge/receipts-v1.sqlite`. It scopes records by the Git
common directory, so worktrees share identity and independent clones do not.
Its digest covers selected pricing, benchmark, term-source, and module files;
it excludes transitive dependencies, toolchains, and external inputs. An
unchanged digest is historical context, not a fresh test or authorization.
Ordinary Chat work does not use or require this database.

## Installation and requirements

Use a stable reviewed checkout as the local marketplace source. From its root:

```powershell
python -B .agents/plugins/kanz-forge/scripts/validate.py --tracked
python -B .agents/plugins/kanz-forge/scripts/provenance.py
codex plugin marketplace add .
codex plugin add kanz-forge@kanz-local
```

Start a new Chat after installation or an update, then run `Kanz /doctor`.
The current Chat's loaded skills may still belong to an earlier version.
Do not hand-edit the installed cache. Roll back by reinstalling the previous
reviewed revision from its checkout.

Codex Chat supplies execution and coordination. Git and GitHub access are
needed for repository delivery; native subagent tools are needed for parallel
workers. Python 3.9+ is a development dependency for structural/provenance
validation, installation checks, and optional receipts, with no third-party
Python packages. Node on Linux is required for the imported security finding
validator tests. Application test tools are selected from the affected
component. See [DEPENDENCIES.md](DEPENDENCIES.md) for provenance and scope.

## Capabilities and proof

The [validation readiness workflow](skills/commands/references/validation-readiness.md)
collects current main/PR checks and uses reviewed task → PR → runtime/gate
dependencies. It propagates shared blockers, distinguishes coding from delivery
readiness, prioritizes executable shared recovery, and explains state changes.
Its six delivery states require evidence; it never authorizes a merge or treats
an incomplete inventory as successful validation.

`Kanz /doctor` distinguishes **implemented** (instructions or code exist),
**configured** (installation and access are present), **operational** (a named
probe succeeded), **incomplete** (observed failure or missing setup), and
**unverified** (no suitable evidence). It reports observations in the Chat;
there is no stored status file that can silently become stale.

| Capability | Required evidence and limit |
|---|---|
| Skill installation | Matching enabled plugin version/source and fresh skill discovery. CLI discovery does not prove this Chat loaded the new version. |
| Automatic routing | A real task selects the relevant skills from its boundary and returns their evidence; catalog validation checks structure only. |
| Native workers and CI coordination | Observed worker dispatch, distinct ownership, dependency handling, exact-commit CI, and reviewed integration. Profiles alone prove none of these. |
| Code relationships | Definitions and actual callers/contracts with source locations. Lexical matches are candidates, not semantic graph edges; unresolved edges stay explicit. |
| Security audit | A scoped review of a real principal/resource boundary. Bundled validator tests prove report tooling, not the security of Kanz. Hostile-code reproduction needs isolation. |
| Financial verification | Applicable owning-service tests and independent numerical references, with complete package coverage and skips disclosed. |
| Concise context | Bounded source retrieval and concise findings without losing constraints. No measured token reduction is claimed. |
| Optional receipts | A complete unskipped pricing/benchmark run before recording; reads never count as retesting. |

Graphify and Caveman are not installed dependencies. Forge uses native source
tools and explicit tracing for relationships, and bounded retrieval and concise
communication for context. It makes no Graphify indexing or Caveman proxy claim.

The useful mechanisms are integrated directly: [Ponytail's](https://github.com/DietrichGebert/ponytail)
necessity/reuse discipline informs the existing-owner-first implementation
choice and connected-code review; [Graphify's](https://github.com/Graphify-Labs/graphify/tree/v8)
relationship approach informs the source-backed impact map; and
[Caveman's](https://github.com/JuliusBrussee/caveman) efficiency goal is implemented
as bounded reads and evidence-preserving output reduction. Forge uses normal,
precise language, retains failed/skipped results, and stops repeating sufficient
checks. No lifecycle hook, persistent graph, cloud model router, or proxy is
installed by these original workflows. See
[context and evidence handling](skills/commands/references/context-and-evidence.md).

### Why Python checks remain

| Files | Concrete purpose |
|---|---|
| `validate.py` | Catch broken packaging, skill routes and local references. |
| `provenance.py`, `test_provenance.py` | Verify imported bytes and detect tampering or broken dispatch references. |
| `skill_discovery.py`, `test_install.py` | Check real CLI discovery and installed bytes in a disposable profile. |
| `receipts.py`, `test_receipts.py` | Optional compatibility with existing local test history; unnecessary for ordinary Chat work. |
| `delivery_evidence.py`, `validation_graph.py`, `collect_validation.py`, `failure_classification.py` and their tests | Current GitHub observations, reviewed validation dependencies, recovery priorities, failure classification and ownership checks. |

These checks use the standard library. Replacing deterministic integrity and
installation checks with prose would remove their regression protection.
`forge.py` and the orchestration wrappers remain deleted.

The [Forge CI workflow](../../../.github/workflows/kanz-forge.yml) verifies
structure, imported-source integrity, Linux security validators, planner and
collector regressions, and optional receipt behavior. Run the offline suite with
`python -B -m unittest discover -s .agents/plugins/kanz-forge/scripts -p 'test_*.py' -v`.
Run `python -B .agents/plugins/kanz-forge/scripts/test_install.py` explicitly for
a disposable current-byte snapshot installation and fresh CLI skill discovery.
It includes new files, excludes deletions/ignored artifacts and rejects missing,
extra or changed installed files. Importing the test never installs anything.
None of these checks authorize a merge, infrastructure apply, or live trade.

## Controlled upgrade and rollback

Keep the live profile unchanged until explicit upgrade authorization. Record
its marketplace source, enabled version and loaded skill source with supported
CLI discovery. Review and commit the candidate in a clean current-main checkout;
run tracked validation, provenance, all offline tests, Linux validators, actual
applicable CI and the disposable installation test. Preserve existing user
configuration and optional receipt data. Do not copy the whole older dirty
checkout over current main or edit an installed cache.

The 0.13 line adds repository engineering, branch recovery, changed-input blocker
re-entry and reviewed continuation ranking. Version 0.13.1 adds direct publisher
version discovery and root-only worker dispatch. Earlier version tests are
historical, not evidence for this candidate. After approval,
register the exact reviewed
checkout through `codex plugin marketplace add <reviewed-checkout>` and reinstall
with `codex plugin add kanz-forge@kanz-local`. Verify the reported version/source,
compare every installed file against the reviewed package inventory, and use a
fresh Chat to verify loaded skills. Retain the prior reviewed checkout for
rollback through the same supported marketplace/install commands. Neither an
upgrade nor rollback changes application deployment or trading authority.
