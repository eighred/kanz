# Kanz Forge dependencies

Forge runs inside Codex Chat. Its skills use the host's source, editing, worker
and verification tools; no separate agent scheduler, proxy or finance library is
required. A repository link is not an installed capability.

| Requirement | Why it remains |
|---|---|
| Codex host tools | Execute the workflows and delegate when native workers are available. |
| Git | Resolve source identity, callers/history and reviewed diffs. |
| GitHub connector or CLI | Retrieve live Issues, PRs, checks and delivery evidence. |
| Codex CLI | Install the plugin and verify fresh CLI skill discovery; Chat dispatch does not launch it. |
| Python 3.9+ standard library | Deterministic package/provenance and installation checks; no third-party Python package. Optional historical receipts are separate from ordinary work. |
| Node.js on Linux | Test the bundled security report validators, whose protected-input CLI needs Linux facilities. |

The affected Kanz component supplies its own build and test toolchain. For
example, Go is needed to verify a Go implementation; it is not a Forge agent
runtime. Financial methodology is information for designing and testing Kanz's
pricing, risk and execution behavior, not a finance-package dependency or a
financial-analysis product.

## Public upstream and documentation access

Forge's [native upstream lookup](scripts/upstream.py) reads Go's official release
feed, public GitHub publisher releases and name-search candidates, and any public
HTTPS Git server's refs with Python's standard library. The Git reader uses the
standard ref advertisement protocol, without a checkout, Git subprocess or SDK.
The [evidence guide](skills/commands/references/context-and-evidence.md#check-version-specific-documentation)
then resolves API questions against official versioned documentation and source.
No Context7 package, server, API key, account, subscription or imported code is needed.
Names are candidates requiring official publisher identity verification. Registries
and projects outside GitHub's name index use official sources through host tools,
then the same generic URL lookup. This is independent of the target repository's language.

This is metadata discovery, not an automatic updater, hosted documentation index
or replacement crawler. Upstream services must be reachable; failure is reported
as unknown, never evidence that no new release exists. GitHub public API limits
apply. The helper does not use host credentials/proxies or follow redirects, and
it never installs, executes or adopts the returned version. Record dated results
in the existing maintenance PR/issue, not a second memory database.

## Imported source

The only imported external project is
[cloudflare/security-audit-skill](https://github.com/cloudflare/security-audit-skill),
pinned at
[`c1c8a8c1471069fb0e188eeaff69b8e8db6564a8`](https://github.com/cloudflare/security-audit-skill/tree/c1c8a8c1471069fb0e188eeaff69b8e8db6564a8).
Its MIT license is retained in `skills/security-audit/LICENSE`.
[upstreams.json](upstreams.json) records each upstream Git blob and local
LF-normalized SHA-256. The security entrypoint has a local Kanz overlay;
`references/kanz-boundaries.md` is original local guidance. Imported companions
retain their pinned bytes. Verify them from the repository root:

```powershell
python -B .agents/plugins/kanz-forge/scripts/provenance.py
```

The report validators check structure and evidence contracts; they do not prove
that Kanz is secure. Full-audit isolation requirements remain binding. Hostile
code and production probes are not authorized by installation.

## Review upstream changes during Forge maintenance

At each Forge upgrade, and before an audit that depends on the imported
methodology, compare `upstreams.json` with the publisher's current release or
default-branch commit. Record the checked date, source URL and observed revision
in the existing maintenance PR or owning issue. If unchanged, retain the pin
and imported bytes; a successful local provenance check alone does not check
upstream freshness.

When upstream has changed, review its diff, license and compatibility with the
Kanz boundaries before adoption. Update the reviewed commit, blob identifiers,
local hashes and any adapted overlay together. Retain the license notice, run
provenance and offline tests, and verify the protected-input validators on Linux.
Deliver through the normal PR and supported plugin installation workflow. A
new upstream revision is a review candidate, not permission to replace installed
files or clear an existing blocker.

Use the native lookup's `github OWNER/REPO --default-branch` observation for
branch-pinned imports, or its release lookup for published tools. A lookup does
not refresh pins or imported files. This maintenance instruction adds no
background updater, recurring job or service dependency.

## Optional history

`scripts/receipts.py` preserves the existing local pricing-test receipt format.
It is not required for `/work`, `/ship`, skill selection, graph tracing or worker
coordination. Its source digest does not cover every execution input, so reads
never count as fresh proof. The tool remains for existing history compatibility,
not as a project memory service. No SQL server or SQLite package is installed.

Graphify, Caveman and Ponytail runtimes are not dependencies. The
[Forge guide](README.md) explains the concrete native methods used for their
quality and efficiency goals. Reference-only finance repository catalogs are
not maintained or installed by Forge.
