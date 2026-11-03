# Kanz setup

For the application stack, start with [local development](kanz/dev/README.md).
For the repository-owned Codex engineering skills, follow the steps below.
Forge setup does not start services or require venue credentials.

## Four everyday commands

In **Codex chat**, send a message such as `Kanz /ship #1385`. In a terminal
at the repository root, use `python forge.py /ship '#1385'`. Quote issue
numbers in PowerShell. The terminal starts interactive Codex for workflows;
in an existing chat Forge performs the workflow there.

| Command | Use it for |
|---|---|
| `Kanz /work #1234` | Implement an issue. Omit the number to select actionable work. |
| `Kanz /ship #1385` | One delivery workflow: prepare the PR, diagnose/fix CI, merge when authorized, verify on main. Does not deploy to production. |
| `Kanz /gaps` | Check missing file references and required artifacts against main; file or reuse issues for confirmed gaps. |
| `Kanz /update` | Maintain Forge dependencies: identify relevant upgrades, review provenance and compatibility, then test and propose approved changes in a PR. Add an owner/repo to narrow the scope. |

`Kanz /help` shows these four. `Kanz /help all` lists specialist commands.
For a read-only missing-link scan use `python forge.py /scan-files`; for a
dependency check without preparing changes use `Kanz /upstream owner/repo`.
No slash-menu installation or PowerShell function is required.

## Install Kanz Forge

Prerequisites: Git, Python 3.9 or later, and a Codex CLI with the plugin
subcommand (`codex plugin --help`). Run these PowerShell commands from the
repository root, which contains AGENTS.md and the .agents directory.

Use the checkout containing the Forge revision you intend to install.
If you already have a checkout, inspect `git status -sb` first; do not discard
local work to install Forge. Keep the checkout available because Codex registers
its path as the marketplace root.

```powershell
python .agents/plugins/kanz-forge/scripts/validate.py --tracked
codex plugin marketplace add .
codex plugin add kanz-forge@kanz-local
codex plugin list --marketplace kanz-local --json
```

Stop if validation fails. In the final command's installed entry, check that
enabled is true, version matches the manifest in your checkout, and source.path
ends in .agents/plugins/kanz-forge. The candidate version is
0.6.0+codex.20261104; its tracked validator reports eight skills. A five-skill
result on the earlier main revision is expected.

## Use and update

Start a new Codex task in the repository. For example, ask:
“Use Kanz Forge financial-verification to review the ACT/ACT accrual test and
explain which inputs it verifies.” The financial-verification and security-audit
skills require the eight-skill candidate. Routine use needs no manual copying
into a user skill directory.

After changing the checkout revision, repeat validation, installation and the
version/source check. To roll back, run those same commands from a checkout of
the previous approved revision. Do not edit the installed cache.

## Optional security-validator tests

From the repository root on Linux with Node.js 22 available:

```sh
node --test .agents/plugins/kanz-forge/skills/security-audit/*.test.cjs
```

Node is needed for these offline validators, not to install the Markdown skills.
On Windows the findings CLI refuses inputs when the required OS file protections
are unavailable. Use Linux for the complete validator suite; keep the safeguards
enabled. Installing Forge provides no authority to probe production systems.

## Forge commands in VS Code

In a new Codex chat in this repository, type:

    $kanz-forge:commands /help
    $kanz-forge:commands /work #1234
    $kanz-forge:commands /audit settlement
    $kanz-forge:commands /verify #1317
    $kanz-forge:commands /ci #1382

Use /work without a target to select and execute the highest-priority actionable
issue. /audit proposes an implementation and waits for approval. /verify only
checks evidence. These actions belong to the skill, not the global slash menu.

Alternatively, in the integrated PowerShell terminal at the repository root:

    function kf { python .agents/plugins/kanz-forge/scripts/command.py @args }
    kf /help
    kf --preview /work '#1234'
    kf /audit settlement

Quote issue numbers in PowerShell. Preview prints the request without starting
work; normal execution launches interactive Codex under its existing permissions.
## Specialist evidence commands

From the root of a Kanz checkout, run
`python .agents/plugins/kanz-forge/scripts/command.py /help`.
Use the launcher for reproducible results. A Codex workflow exit is **not**
evidence that a task was completed. Check the linked PR, tests and revision.

| Capability | Implementation and invocation | Evidence and limits |
|---|---|---|
| Dependency exposure | /alerts | Read-only GitHub Dependabot findings by advisory and manifest. Needs repository alert access. A permission failure is not zero alerts. |
| Symbol investigation | /trace YearFraction | Searches tracked source; returns at most 100 lexical matches and total count, not a type-resolved call graph. |
| Inventory, history, dependency edges | scripts/evidence.py, /repo | Git status, three commits and Go import edges for pricing. No complete symbol/call graph, edit-impact oracle or semantic index. |
| Financial verification | /financial pricing, risk, portfolio, tca or backtest | Runs owning Go tests with -count=1 and readonly dependencies. Pricing includes the rational ACT/ACT oracle, yields and boundary cases. A passing test is not an institutional valuation or a production-data audit. Inspect skips; generate schema SDK first. |
| Security and supply chain | /security | Checks imported bytes and runs Node validator regression tests. These validate findings formats and safe input handling. They do not scan the repository for vulnerabilities. Full findings CLI requires POSIX protections. |
| Upstream intelligence | /upstream or /upstream cloudflare/security-audit-skill | Local registry inventory or read-only gh API comparison against pinned SHA. Emits JSON; redirect to a file for review. Requires GitHub access for live checks. Does not write pins, download code for execution, or establish legal compatibility. |
| Benchmarking | /bench | Five elapsed times and median for tracked Forge validation. No inference speed or runtime trading benchmark. |
| Planning, trace investigation, threat modeling, configuration review | /audit, /investigate, /work | Agent-assisted instructions with interactive Codex, not deterministic source analysis. |
| Delivery, CI diagnosis, verification | /deliver, /ci, /verify | Interactive Git/GitHub workflow. No autonomous proof of reviews, checks, merge or deployment. /deliver requires explicit merge authorization. |

Script paths in the table are relative to .agents/plugins/kanz-forge.
Registry fields identify the GitHub repository, immutable pin, observed commit/release,
license files at that pin, adoption status and review limitations. For imported files
both the upstream Git blob and the local LF-normalized SHA-256 are recorded.
Cloudflare SKILL.md has a local overlay; compare both versions during review.
Commit messages matching security keywords are triage leads only. A truncated
comparison requires manual review. For a proposed update inspect changed imports,
license notices and the overlay, then run test_evidence.py, validate.py --tracked
and the Linux Node tests before requesting adoption approval. No update scheduler
or automatic adoption is installed. Do not register a temporary worktree as a
permanent marketplace; install from the selected durable checkout after review.

### Local acceptance checks

From the candidate checkout:

    python -B .agents/plugins/kanz-forge/scripts/test_evidence.py
    python -B .agents/plugins/kanz-forge/scripts/test_install.py
    python -B .agents/plugins/kanz-forge/scripts/command.py /upstream cloudflare/security-audit-skill

The installation test uses an empty disposable CLI profile, compares tracked
plugin contents, and requests fresh skill discovery from an app-server process.
It does not change the active user marketplace. For the SDK-backed Go suites,
follow kanz-schemas/README.md before invoking /financial. GitHub metadata
requires gh access; local pricing checks do not require upstream API access.
## Forge Next: commands and dependencies

[Dependencies and upstream GitHub links](.agents/plugins/kanz-forge/DEPENDENCIES.md)
lists runtime tools separately from optional research references. The pin
registry remains the authority; changes to a pin must update its linked entry.

Run from a Kanz checkout using the candidate launcher:

    python -B .agents/plugins/kanz-forge/scripts/command.py /doctor
    python -B .agents/plugins/kanz-forge/scripts/command.py /remember pricing
    python -B .agents/plugins/kanz-forge/scripts/command.py /memory
    python -B .agents/plugins/kanz-forge/scripts/command.py /route kanz/go.mod
    python -B .agents/plugins/kanz-forge/scripts/command.py /prioritize

Remember runs both Go pricing suites and accepts only complete, skip-free
results with unchanged scoped inputs. It does not accept caller-supplied
claims, free-text notes or approval records. Recall (also /memory) reads the
last ten scoped receipts without creating an empty database. Records use a
local Git common-directory identity so worktrees share history; the source
digest is recomputed from the current worktree. It covers pricing, its
benchmarks, the terms adapter and the Go module manifests, not every transitive
dependency, compiler or environment setting. Unchanged bytes never mean fresh
verification. Independent clones have independent identities. Deleting the
index loses receipt history; evidence can be generated again by rerunning
the tests, not by reconstructing the exact historical execution.

Route suggests checks without dispatch. Prioritize screens a live issue
snapshot; it cannot establish feasibility from labels alone. /deps, /review
and /hunt are interactive agent workflows, not automated certifications.
For pricing investigations, inspect /memory first, then /trace the affected
symbol and run a fresh /financial or /remember check before accepting results.
No background scheduler, production authority or merge permission is added.
