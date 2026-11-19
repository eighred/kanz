# Kanz Forge

Kanz Forge is Eighred's repository-owned engineering and AI-agent coordination
plugin. Kanz is the institutional investment operating system it helps build:
a shared financial model, controlled execution and auditable state across secure
interfaces. Forge helps developers and Codex investigate current source, choose
the owning component, implement changes, coordinate authorized workers, verify
results and deliver reviewed work.

## Start in this repository

Open a trusted checkout in Codex and describe the result you need. For example,
“Why did this PR fail?” asks Codex to inspect the failing job. “Fix the
failure and test the change” also asks it to repair and verify the code.
Codex selects the relevant Forge workflows as evidence changes. Use `Kanz /help`
to browse shortcuts or `Kanz /doctor` to inspect what this chat can use.
The repository skill at `.agents/skills/kanz-forge/SKILL.md` routes directly to
this package's maintained workflows. It needs no plugin-cache installation.
Repository skill discovery and installed plugin discovery are separate states;
`/doctor` reports which source this Chat actually loaded.

The repo marketplace marks Forge `INSTALLED_BY_DEFAULT`, and the project
`.codex/config.toml` enables `kanz-forge@kanz-local`. Supporting trusted clients
can install/refresh it through their marketplace lifecycle. Client versions and
administrator policy determine that behavior. A manifest cannot force an
untrusted checkout to install a plugin. The repository skill provides the
checkout workflow on clients that discover repository skills but do not
materialize repo-marketplace defaults.

For explicit installation, run from a stable reviewed checkout root:

```powershell
python -B .agents/plugins/kanz-forge/scripts/validate.py --tracked
python -B .agents/plugins/kanz-forge/scripts/provenance.py
codex plugin marketplace add .
codex plugin add kanz-forge@kanz-local
```

Start a fresh Chat and run `Kanz /doctor` after installation or an update.
Compare the loaded source with the checkout and `codex plugin list --json`.
Do not edit the installed cache. See the official
[marketplace and project settings](https://developers.openai.com/plugins/build/plugins#enable-or-disable-a-plugin-for-a-repo)
for supported discovery, trust and enablement behavior.

## Workflows

| Request in Codex Chat | Outcome |
|---|---|
| `Kanz /work #1234` | Resolve the issue through implementation, PR checks, authorized merge and merged-main acceptance. |
| `Kanz /work` | Select feasible unfinished work from current source, branches, PRs and Issues. |
| `Kanz /ship #1234` | Review and repair the PR; merge requires the applicable specific authority. |
| `Kanz /organize [scope]` | Review directory ownership, names, README accuracy and all consumers of a proposed move. |
| `Kanz /branches [fix]` | Inspect unfinished branches; an explicit recovery request continues useful work. |
| `Kanz /audit [fix]`, `Kanz /secure [fix]` | Investigate evidenced engineering or security gaps; `fix` also authorizes scoped repairs. |
| `Kanz /plan`, `Kanz /evolve` | Order evidenced work or propose justified improvements. |
| `Kanz /work subagents 2` | Request native parallel workers, subject to host capacity and independent ownership. |
| `Kanz /doctor`, `Kanz /status`, `Kanz /help all` | Inspect observed capability, current work or the full command catalog. |

These are Chat workflows, interpreted by [commands](skills/commands/SKILL.md),
not terminal executables. With the installed plugin, explicit invocation is
`$kanz-forge:commands /work #1234`; with repository discovery, use `$kanz-forge`.
Ordinary development requests select the same skills by the actual change.

The [organization skill](skills/repository-organization/SKILL.md) applies the
shared [Eighred organization standard](skills/repository-organization/references/eighred-standard.md).
Developers enter it through [CONTRIBUTING.md](../../../CONTRIBUTING.md).
The standard adapts Go's private-package and command boundaries to Kanz's
multiple runtimes, preserves deployed identities and requires complete path
consumer verification. It does not copy another project's skeleton.

## Ownership and coordination

[AGENTS.md](../../../AGENTS.md) owns platform constraints. Forge's skills own
engineering workflows; [commands.json](skills/commands/commands.json) maps the
requests. Security guidance follows identity, tenant, input, release and agent
trust boundaries. Financial verification follows pricing, money, portfolio and
strategy claims. Cross-component work resolves actual callers and typed contracts.

[Repository engineering](skills/commands/references/repository-engine.md)
reconciles existing work before starting a competing branch and uses changed-input
re-entry conditions for known blockers. [Validation readiness](skills/commands/references/validation-readiness.md)
separates code that can be written from work that can actually be delivered.
[Delivery coordination](skills/commands/references/delivery-coordination.md)
keeps initial requested outcomes separate from additional discovered repairs.

Native workers require explicit parallel-work scope and host tools. The root
coordinator alone dispatches; workers return evidence and do not re-delegate.
The [parallel protocol](skills/commands/references/parallel-work.md) assigns
owned files, acceptance and dependencies. The [mapper](agents/kanz_mapper.toml),
[reviewer](agents/kanz_reviewer.toml) and [security](agents/kanz_security.toml)
profiles supply instructions; files alone do not register or launch host agents.
No separate scheduler, agent runtime or background service is installed.

## Context and dependencies

Stable rules live in AGENTS.md, decisions and blockers in Issues, review evidence
in PRs and implementation history in git. Forge retrieves relevant current source
and revalidates historical conclusions. It maintains no conversation-memory or
project-state database. The retired pricing-receipt commands and utility are no
longer packaged; existing user-owned receipt databases are left untouched.

[Version-aware source research](skills/commands/references/context-and-evidence.md)
uses official documentation and tagged source. The standard-library
[upstream helper](scripts/upstream.py) observes public releases, publisher
candidates and HTTPS Git refs. Discovery does not install or approve an upgrade.
[DEPENDENCIES.md](DEPENDENCIES.md) documents actual tool requirements and the
licensed, pinned security-methodology import.

## Verification and maintenance

From the repository root:

```powershell
python -B .agents/plugins/kanz-forge/scripts/validate.py --tracked
python -B .agents/plugins/kanz-forge/scripts/provenance.py
python -B -m unittest discover -s .agents/plugins/kanz-forge/scripts -p 'test_*.py' -v
python -B .agents/plugins/kanz-forge/scripts/test_install.py
```

The last command explicitly tests fresh repository discovery and a separate
supported CLI installation in disposable profiles. It compares every installed
file with the candidate snapshot. Importing the test never installs anything.
On Linux, also run `node --test .agents/plugins/kanz-forge/skills/security-audit/*.test.cjs`;
the protected-input validators require Linux facilities.
[Forge CI](../../../.github/workflows/kanz-forge.yml) enforces the offline gates.

`/doctor` distinguishes implemented, configured, operational, incomplete and
unverified capabilities. Package validation proves structure; CLI discovery
proves the named CLI's discovery; a fresh Chat must establish its loaded source.
Native coordination requires observed dispatch and integration. A security or
financial claim requires evidence at that actual boundary. Skipped dependency
tests, a source digest and historical CI never establish current acceptance.

Review and commit upgrades in a clean checkout. Check imported upstream freshness
and retain its license/provenance. Run offline checks, disposable installation and
applicable CI before updating a live profile. Preserve current user configuration,
dirty work and held branches. Roll back by reinstalling the previous reviewed
checkout through the same marketplace commands, then verify a fresh Chat.
Forge installation grants no trading, deployment or production-data authority.
