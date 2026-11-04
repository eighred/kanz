# Kanz and Kanz Forge

## Main repository overview

Kanz is Eighred's institutional investment operating system. The Go services,
schemas, web client, inference service, and deployment configuration live in
this repository. Start application development with
[the developer guide](kanz/dev/README.md). Forge does not start those services
or grant authority to trade or deploy.

## What is Kanz Forge?

Kanz Forge is the repository-owned Codex engineering plugin in
[.agents/plugins/kanz-forge](.agents/plugins/kanz-forge). It helps investigate an
issue, find its owning component, implement a scoped change, verify the actual
risk boundary, and deliver a PR. The marketplace entry is
[.agents/plugins/marketplace.json](.agents/plugins/marketplace.json).

Forge uses Codex's native skills and, when the host provides them, subagents.
It does not run an agent service or a background task scheduler.

## Kanz Forge instructions

[AGENTS.md](AGENTS.md) owns the platform's stable rules. Forge's
[skills](.agents/plugins/kanz-forge/skills) provide focused workflows; the
[command catalog](.agents/plugins/kanz-forge/skills/commands/commands.json)
routes requested actions. The three tracked specialist profiles live in
[the plugin](.agents/plugins/kanz-forge/agents). Optional `/agents setup`
creates ignored `.codex/agents` compatibility copies for project discovery.
Edit the plugin profiles, not those copies or the installed cache.

Durable decisions and deferred work belong in GitHub Issues and PRs, alongside
code and git history. `/remember pricing` stores only scoped test receipts in
`~/.kanz-forge/receipts-v1.sqlite`; `/memory` reports whether their source
bytes changed. Receipts do not replace fresh verification or store project
decisions. Independent clones have separate receipt identities.

## Core capabilities

Forge routes issue work, scoped reviews, verification, PR delivery, missing-file
reconciliation, and review of its imported source. Its evidence commands are
deterministic checks with stated limits; they do not certify production state.

## Usage in Codex Chat

In a Codex Chat opened on this repository, use:

| Request | Result |
|---|---|
| `Kanz /work #1234` | Reconcile and implement one issue, then verify and prepare delivery. |
| `Kanz /work` | Inspect live Issues, PRs, and current code before selecting actionable work. |
| `Kanz /work subagents 2` | Request two native workers for independent work. Specific issues may follow the count. |
| `Kanz /ship #1234` | Review a PR, repair failures, and merge only with approval for that PR. |
| `Kanz /verify #1234` | Check the acceptance evidence without treating skips as passes. |
| `Kanz /gaps` | Reconcile missing-file candidates with main and existing Issues. |
| `Kanz /update` | Review Forge's registered external source and prepare an approved update. |

`Kanz /help` shows everyday commands; `Kanz /help all` shows specialist
commands. You can also invoke the installed skill explicitly as
`$kanz-forge:commands /work #1234`.

For parallel work, the coordinating agent checks dependencies and file overlap,
assigns distinct owners and worktrees, and keeps each CI result tied to the
commit it tested. Workers continue independent tasks while CI runs; dependent
tasks wait for their required result. The coordinator reviews worker results,
handles failures, and integrates only after applicable checks and approvals.
The requested count is limited by host capacity; Forge reports the effective
count. If native subagents are unavailable, it reports that limit instead of
claiming parallel execution. An instruction alone is not a running worker.

## Configuration and requirements

For Codex Chat, install the plugin from the checkout you intend to use. Python
3.9 or later is needed for the repository validator and optional evidence CLI;
ordinary Chat skill workflows use Codex directly. Git and GitHub access are
needed for live issue and PR work. The host must expose native subagents for
parallel work.

```powershell
python -B .agents/plugins/kanz-forge/scripts/validate.py --tracked
codex plugin marketplace add .
codex plugin add kanz-forge@kanz-local
python -B forge.py /doctor
```

The validator checks tracked plugin structure. `/doctor` checks the current
checkout, installed version and source, and CLI skill discovery when available.
After changing the registered checkout revision, validate and install again,
then start a new Codex task. Do not register a disposable worktree as a
permanent marketplace or edit the installed cache. For rollback, install the
previous reviewed checkout revision.

`forge.py` is the root terminal entry point. `python -B forge.py /help`
lists commands, `python -B forge.py --preview /work '#1234'` prints the
request, and normal terminal workflow commands launch interactive Codex.
Quote issue numbers in PowerShell. From an existing Codex Chat, invoke the
skill directly so the workflow stays in that chat. Deterministic evidence
commands such as `/doctor`, `/memory`, and `/upstream` run locally.
See [dependencies](.agents/plugins/kanz-forge/DEPENDENCIES.md) for the small
set of development tools and imported source.

## Feature status

`python -B forge.py /doctor` reports **implemented** when checkout structure
passes validation, **configured** when an enabled installed plugin matches
that checkout, and **operational** when fresh CLI discovery finds its skills.
It reports **incomplete** for an observed failure and **unverified** when a
probe cannot run. This is an observed local status, not certification of every
workflow or of the Codex Chat host. Its feature map covers command preview,
upstream inventory, financial verification, installed skills, receipt memory,
security validators, repository metadata access, native delegation, and CI
coordination. Each result is limited to the probe named in its entry.

| Feature | Evidence needed beyond configuration |
|---|---|
| Parallel work and CI-aware coordination | A live Chat run with native workers, separate ownership, exact-head CI, and a coordinated result. |
| Financial receipt memory | Fresh, complete Go pricing suites for `/remember pricing`; `/memory` only reports receipt freshness. |
| Security audit | Bundled offline validator tests on Linux, then a scoped review of the actual trust boundary. |
| PR delivery | Required checks and reviews on the exact head, specific merge approval, and merged-main verification. |

The CI workflow in [.github/workflows/kanz-forge.yml](.github/workflows/kanz-forge.yml)
checks plugin structure and offline command contracts. It does not prove live
delegation, a production dependency, or authorization to merge or deploy.
