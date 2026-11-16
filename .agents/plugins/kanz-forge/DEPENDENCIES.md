# Kanz Forge dependencies

Forge supplies Eighred's engineering workflows and AI-agent coordination inside
Codex. The plugin has no production service, financial-computation runtime,
agent scheduler or project-memory database.

| Tool | Required for |
|---|---|
| Codex with repository-skill or plugin support | Discover and follow the workflows using host tools. |
| Git | Establish checkout identity, history, consumers, diffs and preservation of existing work. |
| GitHub connector or CLI | Read Issues, PRs and checks; authorized repository delivery. |
| Codex CLI | Explicit marketplace installation and disposable fresh-discovery verification. |
| Python 3.10+ standard library | Package, provenance, installation and evidence-helper checks; no third-party Python package. |
| Node.js on Linux | Tests for the bundled security report validators, which require protected-input facilities unavailable on Windows. |

Individual Kanz components supply their own build/test tools and real disposable
dependencies. A missing broker, database, browser or native toolchain limits the
claim that can be verified; Forge does not replace those dependencies.
Native worker tools are optional and used only under authorized parallel scope.
A profile in the package is not evidence that the host can dispatch that role.

## Public-source research

The [upstream helper](scripts/upstream.py) uses Python's standard library for
Go's official release feed, public GitHub release/name metadata and public HTTPS
Git ref advertisements. It does not need an account, API key, SDK or Git subprocess.
The [evidence guide](skills/commands/references/context-and-evidence.md)
resolves APIs against official versioned documentation and source.

Publisher-name matches are candidates requiring identity verification. Releases,
tags and unreleased HEAD remain distinct. Network failures and API limits remain
unknown results. The helper does not use host credentials/proxies, follow redirects,
install returned versions or run an updater. Other registries use official host
research tools. Record maintenance evidence in the existing PR/Issue.

## Imported methodology

The imported source is
[cloudflare/security-audit-skill](https://github.com/cloudflare/security-audit-skill),
pinned at [c1c8a8c1471069fb0e188eeaff69b8e8db6564a8](https://github.com/cloudflare/security-audit-skill/tree/c1c8a8c1471069fb0e188eeaff69b8e8db6564a8).
Its MIT notice remains at `skills/security-audit/LICENSE`.
[upstreams.json](upstreams.json) records upstream Git blobs, local LF-normalized
SHA-256 values and adapted files. The entrypoint's Kanz overlay and the local
boundary guide apply repository-specific rules; untouched companions retain
upstream bytes. Run `python -B .agents/plugins/kanz-forge/scripts/provenance.py`
from the repository root to verify the import.

At each Forge upgrade, and before relying on this methodology for an audit,
compare the pin with the publisher's current release/default branch. Record the
date, source URL and observed revision in the maintenance PR or owning Issue.
A local hash check establishes integrity, not upstream freshness.

When upstream changes, review its diff, license and compatibility before adoption.
Update reviewed commit/blob/hash metadata with the adapted overlay, preserve the
license, and run provenance, offline tests and Linux validator checks. Use the
supported package install path; do not edit a host cache or silently replace an
import. A new upstream revision is a review candidate, not adoption authority.

## Organization references

The [Eighred organization standard](skills/repository-organization/references/eighred-standard.md)
uses the community Go layout reference, official Go module guidance, go-github
and TypeScript for design comparison. Their code and runtimes are not imported
or required. There is no optional repository catalog or unrelated tool integration.
Financial references guide implementation verification; no external finance
library is installed by Forge.
