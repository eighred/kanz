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
