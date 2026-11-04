# Kanz Forge dependencies

Kanz Forge runs inside the repository's Codex chat or through the documented
terminal launcher. These are development tools, not production dependencies.

| Tool | Use |
|---|---|
| Python | Repository validator, evidence commands, and offline tests; standard library only |
| Git | Revision and tracked-file identity |
| GitHub CLI | Terminal evidence commands that query repository metadata |
| Codex CLI | Optional terminal launcher, plugin installation, and CLI discovery probe |
| Go | Existing Kanz financial verification suites |
| Node.js | Offline security-audit validator tests |

## Vendored upstream

The only imported external project is
[cloudflare/security-audit-skill](https://github.com/cloudflare/security-audit-skill),
pinned at
[`c1c8a8c1471069fb0e188eeaff69b8e8db6564a8`](https://github.com/cloudflare/security-audit-skill/tree/c1c8a8c1471069fb0e188eeaff69b8e8db6564a8).
Its [MIT license](https://github.com/cloudflare/security-audit-skill/blob/c1c8a8c1471069fb0e188eeaff69b8e8db6564a8/LICENSE)
is retained in `skills/security-audit/LICENSE`. The imported file list, upstream
Git blob IDs, and local LF-normalized SHA-256 hashes are recorded in
[upstreams.json](upstreams.json). `skills/security-audit/SKILL.md` has a Kanz
overlay; review that difference when proposing an update. The validators are
development-time tooling and do not authorize a production probe.

Other research repositories are not Forge dependencies or update targets.
For a financial method, use an attributable primary specification and an
independent numerical check scoped to the actual Kanz decision.
