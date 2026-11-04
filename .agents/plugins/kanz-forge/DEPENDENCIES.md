# Kanz Forge dependencies

Forge's development workflow runs in the current Codex Chat. It needs no Python
launcher, agent scheduler, graph database, compression proxy or additional model
account. These are development tools, not Kanz production dependencies.

| Tool | Actual dependency |
|---|---|
| Codex host tools | Source inspection, edits, native workers and task coordination; available tools determine what this chat can do |
| Git | Revision, diff and tracked-file identity |
| GitHub connector or CLI | Current Issues, PRs and CI evidence; use the available authenticated interface |
| Codex CLI | Plugin installation and the optional CLI discovery test; not required to dispatch a Chat command |
| Python standard library | Package/provenance validation and install tests; optional run receipts. No Python dependency for ordinary Chat routing |
| Go and the component's real test dependencies | Existing Kanz financial, benchmark and domain suites; not a second calculation engine in Forge |
| Node.js | Offline security finding/coverage validators; their protected-input CLI requires Linux support |

## Vendored upstream

The only imported external project is
[cloudflare/security-audit-skill](https://github.com/cloudflare/security-audit-skill),
pinned at
[`c1c8a8c1471069fb0e188eeaff69b8e8db6564a8`](https://github.com/cloudflare/security-audit-skill/tree/c1c8a8c1471069fb0e188eeaff69b8e8db6564a8).
Its MIT license is retained in `skills/security-audit/LICENSE`. The imported file
list, upstream Git blob IDs and local LF-normalized SHA-256 hashes are recorded
in [upstreams.json](upstreams.json). `skills/security-audit/SKILL.md` has a Kanz
overlay; review that difference when updating. The validators check report
structure and evidence contracts, not whether an alleged vulnerability is real.

The entrypoint selects focused guidance or a full audit. `ATTACK-CLASSES.md`
selects domain companions; `RECONNAISSANCE.md`, `HUNTING.md` and
`VALIDATION-AND-REPORTING.md` implement full-audit coverage and independent
verification. Domain companions are loaded for observed boundaries, not all at
once. Their isolation requirements remain binding; these tools do not authorize
production probes. Run `python .agents/plugins/kanz-forge/scripts/provenance.py`
from the repository root to verify the pinned local imports.

## Evaluated alternatives, not installed dependencies

[Graphify](https://github.com/Graphify-Labs/graphify) builds a persisted,
queryable graph with local AST parsing and explicit extracted/inferred edges.
Its document/media semantic pass can use a model backend. It needs Python and
additional packages. Forge instead traverses current source, typed contracts,
resolved references and package metadata on demand. This covers scoped impact
analysis without claiming a complete indexed graph. A future index must justify
its maintenance, language coverage, freshness and source-handling costs. No
Graphify package, cloud service or source upload is configured by Forge.

[Caveman](https://github.com/JuliusBrussee/caveman) separates concise replies
from a runtime that compresses tool input and can route models. Its current
runtime documents telemetry and optional cloud request sharing. These add data
handling and configuration beyond concise writing. Forge uses short, precise
instructions and bounded reads directly, preserving evidence and exact payloads.
The advertised 65% is not a measured Kanz saving:
[JetBrains' paired coding-task experiment](https://blog.jetbrains.com/ai/2026/07/speak-to-ai-agents-like-cavemen-tosave-tokens/)
reported 8.5% fewer output tokens in its tested setup, without detectable quality
degradation. Neither result predicts this host's savings. No Caveman code,
proxy, telemetry or model-routing service is installed by Forge.

Research references do not become dependencies or update targets. Financial
methods require an attributable primary specification and independent numerical
checks scoped to the Kanz decision; reference-only finance repositories are not
part of Forge.
