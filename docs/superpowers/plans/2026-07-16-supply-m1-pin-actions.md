# SUPPLY-M1 — pin every GitHub Action to an immutable SHA (hold trivy)

## Context

`aquasecurity/trivy-action` was compromised (Dependabot #1/#2, Critical, flagged in **both**
`security.yml` and `release.yml`). The alarm is one action; **the exposure is all 14**, every one
pinned to a mutable ref that its publisher can repoint with no commit here to review.

**The blast radius is the release pipeline.** `release.yml` grants `packages: write` (push to
ghcr.io) and `id-token: write` (keyless cosign signing via GitHub OIDC). A compromised action in that
job can **push an image AND mint a genuine cosign signature for it** — one that verifies, because the
pipeline's real identity signed it. Every downstream guarantee (digest-pinned, cosign-signed,
distroless, `-trimpath`) assumes the pipeline that produced it was honest.

The repo already knows this rule and applied it one layer down. `release.yml:88` reads:

```
# Everything below pins to the immutable digest, never a tag.
- uses: aquasecurity/trivy-action@0.28.0     ← a mutable tag
```

It pins the **image it built** to a digest, then runs the scanner that checks it — and the tool that
signs it — from tags. This is `KANZ_BRAIN.md`'s own lesson one level up: *"`latest` is not a version;
it is a promise that the gate can change without a commit. Pin every tool that can fail a build, and
make moving it a reviewable change."*

**Lead decision (2026-07-16): pin the 13, HOLD trivy.** Trivy was *compromised*, so pinning it to
whatever its tag points at today could pin the compromise permanently — GHSA #1/#2's affected range
decides which SHA is safe, and that is not readable from here.

## Ground truth — resolved by the controller. USE THIS TABLE; DO NOT RE-RESOLVE.

Every SHA below was resolved with `git ls-remote`, dereferencing annotated tags via `refs/tags/<t>^{}`
so the value is the **commit**, not the tag object. Re-resolving risks a different answer (a tag can
move between now and then) and makes the diff unreviewable.

| action (current ref) | immutable commit SHA |
|---|---|
| `actions/checkout@v4` | `34e114876b0b11c390a56381ad16ebd13914f8d5` |
| `actions/github-script@v7` | `f28e40c7f34bde8b3046d885e986cb6290c5673b` |
| `actions/setup-go@v5` | `40f1582b2485089dde7abd97c1529aa768e1baff` |
| `actions/setup-node@v4` | `49933ea5288caeca8642d1e84afbd3f7d6820020` |
| `actions/setup-python@v5` | `a26af69be951a213d495a4c3e4e4022e16d87065` |
| `anchore/sbom-action@v0` | `e22c389904149dbc22b58101806040fa8d37a610` |
| `bufbuild/buf-setup-action@v1` | `a47c93e0b1648d5651a065437926377d060baa99` |
| `docker/build-push-action@v6` | `10e90e3645eae34f1e60eeb005ba3a3d33f178e8` |
| `docker/login-action@v3` | `c94ce9fb468520275223c153574b00df6fe4bcc9` |
| `docker/metadata-action@v5` | `c299e40c65443455700f0fdfc63efafe5b349051` |
| `docker/setup-buildx-action@v3` | `8d2750c68a42422c14e847fe6c8ac0403b4cbd6f` |
| `golangci/golangci-lint-action@v7` | `9fae48acfc02a90574d7c304a1758ef9895495fa` |
| `sigstore/cosign-installer@v3` | `398d4b0eeef1380460a10c8013a76f728fb906ac` |

**Two facts worth carrying:**
- **`sigstore/cosign-installer@v3` is an ANNOTATED tag.** Its unpeeled ref is the tag *object*, not a
  commit — pinning that would be subtly wrong, in the action that signs the images. The table has the
  peeled commit.
- **`bufbuild/buf-setup-action@v1` is a BRANCH (`refs/heads/v1`), not a tag at all.** A branch is
  *expected* to move; every push to it changes what CI runs. The SHA above is its current head.

**Eight workflow files carry these**, and `kanz-schemas` has its own two — do not miss them:
`.github/workflows/{build,kanz-ci,latency,preview,release,security}.yml` and
`kanz-schemas/.github/workflows/{schema-ci,schema-release}.yml`.

## Global Constraints

- **DO NOT touch `aquasecurity/trivy-action@0.28.0`.** It stays tag-pinned, deliberately, pending the
  GHSA advisory. Held — not forgotten.
- **Do not change any action's VERSION.** This is a pin, not an upgrade: each SHA above is what its
  current ref already resolves to, so CI behaviour is unchanged. Bumping versions here would make a
  security change indistinguishable from a functional one.
- Every pin carries the original ref in a trailing comment (`@<sha> # v4`) — the SHA is the contract,
  the comment is for humans and for Dependabot, which reads it to keep pinned actions updated.
- `test/arch` stays green.

## Task 1 — Pin the 13

**Files:** the eight workflow files above.

**Requirements:**
1. Replace every `uses: <repo>@<ref>` with `uses: <repo>@<sha> # <ref>`, using **the table above
   verbatim**, across all eight files (including `kanz-schemas`').
2. `aquasecurity/trivy-action@0.28.0` is **unchanged**, and gains a brief comment saying it is held
   pending GHSA #1/#2 — so the next reader knows it is a decision, not an oversight.
3. Nothing else changes: no version bumps, no `with:` changes, no reordering.

**Verification (must be EXECUTED, output pasted verbatim):**
- Every `uses:` across all eight files is either a 40-hex SHA or the held trivy line — show the full
  list: `grep -rhoE "uses: [^ ]+" --include=*.yml . | sort -u`
- Count: 13 distinct actions pinned, 1 held.
- **Each SHA matches the table** — re-grep the files and diff against it by eye; paste the result.
- YAML still parses: `python -c "import yaml,sys; [yaml.safe_load(open(f)) for f in sys.argv[1:]]" <the 8 files>` (or any equivalent you can run here).
- `go test ./test/arch/... -count=1` green.

## Task 2 — Guard it

**File:** `kanz/test/arch/supplychain_test.go` (new), package `arch`.

**Why:** a pin that nothing enforces is a pin until the next `uses:` line. The whole point is that
moving an action becomes a *reviewable commit* — which only holds if a tag-pinned action fails the
build.

**Requirements:**
1. Find every `uses: <owner>/<repo>@<ref>` across **all eight** workflow files — both `.github/` at
   the repo root and `kanz-schemas/.github/`. Note `moduleRoot(t)` is `kanz/`, so the root workflows
   are at `filepath.Dir(moduleRoot)` — `deployability_test.go` already does exactly this, follow it.
2. Assert every ref is a **40-character hex SHA**. A tag or branch fails the build, naming the file
   and the action, and saying to pin it to a SHA with the ref in a trailing comment.
3. **A reasoned exemption allowlist**, in the `unbackedByDesign` style already used by
   `archiver_topology_test.go`: `aquasecurity/trivy-action` is exempt **with a written reason** (held
   pending GHSA #1/#2 — pinning a compromised action's current tag could pin the compromise).
4. **Anti-rot, both directions:** an exempt action that IS now SHA-pinned must fail as a *stale*
   exemption; an allowlist entry for an action no longer used at all must fail as *dead*. This is the
   pattern `archiver_topology_test.go` established and it is why the allowlist there cannot decay.
5. **Non-vacuous:** zero workflow files found, or zero `uses:` lines, is a FAILURE — not a pass.
6. Normalise CRLF. Ignore local `uses: ./...` composite paths if any exist (they are in-repo, not a
   supply-chain edge) — say so in a comment if you special-case anything.

**Verification (must be EXECUTED):**
- Passes on the tree after Task 1.
- **Mutation A:** revert one pinned action to its tag → FAILS naming it. Restore (targeted edit,
  never `git checkout -- .`), confirm green.
- **Mutation B:** SHA-pin trivy → FAILS as a *stale exemption*. Restore, confirm green.
- **Mutation C:** add a bogus entry to the allowlist → FAILS as *dead*. Restore, confirm green.
- `go test ./test/arch/... -count=1` green; `gofmt -l .` clean; `git status --short` shows no modified
  tracked files.
