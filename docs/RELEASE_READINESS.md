# Release Readiness Report

**Audited:** 2026-07-27 · **Updated after recovery Phases 1-2** · **Verdict: NO-GO (both P0s addressed; a third defect is now unmasked)**

> **Recovery status.** REL-P0b is **fixed and CI-verified**: the race detector reported two `WARNING: DATA RACE` on `main` and **zero** after the fix. REL-P0a is **implemented** (owner decision: adopt `eighred`) and awaiting its first green `kanz-build` on a push to `main` — the only evidence that counts, and one merging alone can produce.
>
> **A third defect surfaced, previously masked.** With the provisioner race gone, `kanz-ci`'s race step still fails — on `TestNATSMTLS_SPIFFEClientConnectsPublishesConsumes` (`pkg/bus`): *"publish over mTLS: nats publish: context deadline exceeded"*. It failed on `main` too, hidden behind the race. **This resolves board item OPS-M2f-c**, which was carried as an unverified ~2-minute check: CI runs it, and it fails. Tracked below as P1-4.

Two P0 defects block release. Both are proven by CI evidence, not inferred. This report is the source of truth for finishing the system; where it contradicts `KANZ_TASKS.md`, this report is newer and the board rows it names are being corrected.

**The single most important finding: the board's premise is stale.** `KANZ_TASKS.md` rows 39/40 assert that CI is dead from a GitHub **billing halt**, that every run "fails in 3–9 seconds with all `steps=0`", and that the correct response is to escalate billing rather than debug. **That has not been true for at least two days.** Runs now execute for **5–10 minutes**, `security` and `Dependency Graph` **pass**, and `kanz-build` **succeeded on `main`** as recently as commit `e54331f`. Every current failure is a real code or configuration defect. Work planned on the "we are blocked on billing" premise should be re-planned.

---

## A. Current System Status

| Item | State | Evidence |
|---|---|---|
| `main` HEAD | `e0cddf4` | `git rev-parse HEAD` |
| Local build (CI-equivalent, default `-mod=readonly`) | **PASS** | `go build ./...`, `go vet ./...` exit 0 |
| Local tests, all packages except `cmd/kanz-provisioner` | **PASS** | `go test ./...` exit 0 |
| Local tests, `cmd/kanz-provisioner` | **FAIL ~75%** | 6/8 failures pre-#15, 4/5 now |
| GitHub `kanz-ci` | **FAILING** | run `30220630656`, step "Test (race)" |
| GitHub `kanz-build` | **FAILING** | run `30220630691`, step "Build and push", all 25 images |
| GitHub `security` | **PASSING** | run `30220630657` |
| GitHub `latency` | failing | last run 2026-07-26 |
| GitHub `preview` | skipped (label-gated) | — |
| GitHub `release` | **never completed a job** | 1 run ever, 2026-07-18, 5s |
| Git tags | **ZERO** | `git tag` empty |
| GitHub releases | **ZERO** | `gh release list` empty |
| Open issues | **ZERO** | `gh issue list` empty |
| Open PRs | 1 (#10, held) | — |
| Branch protection on `main` | **NONE** | API returns "Branch not protected" |
| Dependabot alerts | **0 open** | — |
| Secret scanning | **ENABLED** (owner, 2026-07-27) | API `security_and_analysis` |
| Secret-scanning push protection | **ENABLED** 2026-07-27 | API `security_and_analysis` |
| Dependabot security updates | **ENABLED** 2026-07-27 | API `automated-security-fixes` |
| Repository visibility | **PUBLIC — temporary billing workaround** | anonymous API GET = 200; see P2-0 |
| Code scanning (CodeQL) | **RUNNING, green** (owner added `8879586`; pinned to SHAs) | workflow "CodeQL Advanced" |
| Board validator | PASS, 62 rows | exit 0 |

**Release status: nothing has ever been released.** `release.yml` triggers on `push: tags: ['v*']` and no tag has ever existed, so the tag path has never fired once. The single historical run was a manual `workflow_dispatch` that died in 5 seconds.

---

## B. Critical Issues

### P0-1 — `main` cannot publish any image. Every push fails.

`kanz-build`'s "Build and push" step fails for **all 25 images** on every push to `main`.

**Causation is proven, not inferred:**

| commit | run | result |
|---|---|---|
| `4b75471` (#28) | 2026-07-26 20:18 | **success** |
| `e54331f` (#29) | 2026-07-26 20:18 | **success** |
| `3f6772c` (#30, canonical-org change) | 2026-07-26 20:19 | **failure** |
| every run since | — | failure |

The change made both workflows publish to `ghcr.io/kanz-eng/*`. They authenticate with `secrets.GITHUB_TOKEN`, which is scoped to `eighred/kanz` and **cannot push into another organization's namespace**. This was disclosed as a deliberate trade when the change was merged — fail loudly rather than keep publishing where nothing pulls — but it is now an active, ongoing breakage and it is the top blocker. `kanz-build` passes on `pull_request` (which builds without pushing), which is why the PR was green.

### P0-2 — Data race in `sshexec.go`, in the node-provisioning path

CI's race detector reports two `WARNING: DATA RACE` stacks, both reaching `bytes.Buffer` through `x/crypto/ssh/session.go:527` and `:514` — the **stdout and stderr copy goroutines** — from `sshexec.go:65`.

`sshexec.go:57-58` assigns **the same `bytes.Buffer`** to `sess.Stdout` and `sess.Stderr`. `io.Copy` takes the `Buffer.ReadFrom` path, which grows and mutates the buffer **regardless of how many bytes it reads**, so both goroutines mutate it concurrently.

This is **product code**, not test-only. Consequence: the provisioner can lose or corrupt the output of a remote command, so a node-provisioning failure can be reported with empty or garbage detail. It is also the root cause of the `TestSSHRunExecutesCommand` flake (empty output, nil error, occasionally 16 NUL bytes), which fails ~75% of whole-package runs and predates every recent merge.

### P1-1 — The repository-location decision is now on the critical path

Three consequences of `eighred/kanz` vs `kanz-eng` remain, and **P0-1 cannot be closed without deciding this**:

1. **Push credential.** Publishing to `kanz-eng` needs a `write:packages` credential `GITHUB_TOKEN` cannot supply.
2. **Keyless signing identity.** `cluster-image-policy.yaml:36` pins `subjectRegExp` to `github.com/kanz-eng/kanz/...`; a workflow in `eighred/kanz` presents `github.com/eighred/kanz/...`. The policy is `mode: enforce`, so it would reject every signature. Latent only because policy-controller has no install path in-repo.
3. **Self-verify step.** `release.yml`'s `--certificate-identity-regexp` uses `${{ github.repository }}`, so it can never disagree with what just signed.

Two ways out, and they cost very differently:
- **Move the repository to a `kanz-eng` org** — makes 747 hardcoded references, the Go module path, and the admission policy correct as written; needs org admin.
- **Adopt `eighred` as canonical** — revert two workflow lines and rewrite 39 manifests, `preview.yml`, and the policy. **No new credential needed, and CI goes green immediately.**

### P1-4 — NATS mTLS publish times out in CI

`TestNATSMTLS_SPIFFEClientConnectsPublishesConsumes` fails with *"publish over mTLS: nats publish: context deadline exceeded"* (`nats_mtls_integration_test.go:151`). Present on `main` and on every branch checked; it was hidden behind the provisioner data race in the same test step. The client reaches the publish call, so this is not a connect or certificate rejection — it is a publish that never completes.

**ROOT CAUSE, proven from the config (2026-07-27).** The harness client cert carries `spiffe://kanz.internal/ns/kanz-services/sa/risk-engine` (`test/mtls/up.sh:48`), and `verify_and_map: true` maps that SVID to the `risk-engine` NATS user. That user's `publish.allow` (`infra/nats/tenancy.yaml:230-237`) is exactly:

`risk.portfolio.exposure_recomputed`, `risk.portfolio.measures_computed`, four `dlq.*` subjects, `$JS.API.>`, `$JS.ACK.>`.

The test publishes to `test.mtls.<timestamp>` — **not in that list**. `$JS.API.>` is, which is precisely why `CreateStream` succeeds and `Publish` does not: a NATS permission denial on a request subject yields **no reply**, so a JetStream publish awaiting its `PubAck` presents as `context deadline exceeded` rather than as an authorization error.

**The test cannot be fixed by changing its subject alone.** `risk-engine`'s `publish.allow` and `subscribe.allow` (`:238-244`) have an **empty intersection** — by design, since a service emits its outputs and consumes its inputs and never round-trips its own traffic. A single-identity publish-then-consume round trip is therefore impossible under the production permission model the test exists to assert. It has never passed; the billing halt merely meant nobody was told.

**Three ways out, and one is a trap:**

1. **Two identities** — publish as `risk-engine` on `risk.portfolio.measures_computed`, consume as an identity permitted to subscribe it. Keeps the round-trip guarantee and stays inside the production model. Most work.
2. **Narrow the assertion** — prove mTLS connect, SVID-to-account mapping, and an accepted publish on a permitted subject; drop the consume half. Honest and small, but the test stops proving delivery.
3. **Grant `test.mtls.>` in `tenancy.conf` — do not do this.** It weakens the deny-by-default permission model in production config so a test can pass, which inverts what the test is for.

This is the same surface board item **OPS-M2f-c** flagged as unverified (the `nats.conf` `pid_file` addition, extracted verbatim by `test/mtls/up.sh`). That item can stop being a "2-minute check somebody should run" — CI runs it every time, and it fails every time.

### P1-2 — No versioning exists

Zero tags, zero releases. `release.yml`'s only trigger besides manual dispatch is `push: tags: ['v*']`. "Prove `release.yml` once" (OPS-M1) cannot happen until someone cuts a tag, and nothing documents a versioning scheme.

### P1-3 — `release.yml` covers 2 of 25 services

Its matrix is `[risk-engine, schema-registry]`; `build.yml` builds 25. Even a green release run leaves 23 services with no signed image, no SBOM, and no digest to pin.

### P2-0 — Repository is PUBLIC: a deliberate, temporary billing workaround (owner-confirmed 2026-07-27)

`eighred/kanz` is **public** — an anonymous `GET` on the repository API returns 200. Container packages remain **private** (anonymous ghcr probe returns 401).

**This is intentional and temporary.** The owner switched visibility to public to work around the billing-related CI interruption; it was an operational decision, not a misconfiguration. Recorded here because the board still describes this as "this private repo", and because two consequences follow that are easy to miss:

- **It explains the "billing halt ended".** Public repositories get free GitHub Actions minutes. CI did not get fixed — the repository changed visibility, and the runners came back with it.
- **Therefore restoring private visibility WILL re-break CI** unless the billing/account issue is settled first. Sequence matters: resolve billing, then flip visibility, then confirm a green run. Flipping first reproduces the original outage.

| aspect | state |
|---|---|
| Public visibility | Temporary workaround |
| Security risk | **Accepted temporarily**, by owner decision |
| Intended long-term state | Repository private; packages private; release and deployment access via authenticated workflows |
| Follow-up | Restore private visibility once billing is resolved and the workaround is no longer needed — **after** confirming CI can run on a private repo |

While public, treat every secret ever committed as disclosed and rely on push protection (now enabled) to stop new ones.

### P2-1 — Repository security settings are off

Secret scanning **disabled**; code scanning has **never run**; `main` has **no branch protection**, so anything can be pushed or force-pushed directly. `security.yml` runs gitleaks in-workflow, which covers commits it sees — it does not cover pushes that bypass CI.

---

## C. Technical Debt

| Item | Finding |
|---|---|
| Stale board rows | `KANZ_TASKS.md` rows 39/40 assert a billing halt that ended ≥2 days ago; OPS-M1's guidance ("escalate billing, do not route around it") no longer applies |
| Merged branches | 6 remote branches fully merged into `main` and deletable |
| Orphaned branch | `docs/board-ops-m2-verification` — its PR (#26) was closed as superseded |
| Held PR | #10 — 8 GitHub Actions major-version bumps; previously broke `TestAllDockerfilesPinTheSameGolangVersion` (board row 46) |
| `go.mod`/`go.sum` CRLF | `.gitattributes` pins `*.yaml`, `*.go`, `*.sh`, `*.proto` to LF but not `go.mod`/`go.sum`; both show CRLF warnings |
| Verification-mode hazard | `GOFLAGS=-mod=mod` silently rewrites `go.mod`/`go.sum`; it hid the k8s 0.36 breakage until after merge |
| No tracking issues | Zero GitHub issues; findings live only in markdown |

---

## D. Remaining Work

| Item | Status | Priority | Required action |
|---|---|---|---|
| `main` cannot push images | Open, active breakage | **P0** | Decide repo location, then either add a `write:packages` credential or adopt `eighred` |
| `sshexec.go` data race | Open, proven by CI | **P0** | Give stdout and stderr separate buffers; combine after `Run` returns |
| Repository location | Undecided | **P1** | Lead decision; unblocks P0-1 and two signing defects |
| No version tags | Open | **P1** | Define scheme, cut first tag after P0-1 |
| `release.yml` covers 2/25 | Open | **P1** | Bring matrix to parity with `build.yml` |
| PR #10 (8 major action bumps) | Held | **P1** | Merge only after CI is green, so a failure is attributable |
| Branch protection absent | Open | **P2** | Require PR + passing checks on `main` |
| Secret + code scanning off | Open | **P2** | Enable both |
| 6 merged branches | Cleanup | **P2** | Delete |
| `go.mod`/`go.sum` EOL | Open | **P2** | Extend `.gitattributes` |

---

## E. Execution Roadmap

1. **Decide the repository location.** Everything below P0-1 depends on it. Fastest path to green is adopting `eighred`; the more correct long-term shape is moving to `kanz-eng`.
2. **Restore image publishing.** Implement the decision; confirm by a green `kanz-build` on a push to `main`, not by inspection.
3. **Fix the data race.** Separate buffers in `sshexec.go`; confirm with a green `kanz-ci` "Test (race)" step — the only environment where `-race` runs, since this workstation has no C compiler.
4. **Then, and only then, cut `v0.1.0`** and let `release.yml` execute for the first time. Verify `cosign verify` from a clean machine and a non-empty SBOM.
5. **Bring the release matrix to 25 services**, then merge #10 and let CI validate it.
6. **Harden the repository:** branch protection, secret scanning, code scanning. Delete merged branches.

**Do not treat OPS-M1 as billing-blocked.** It is blocked on P0-1 and P1-2, both of which are engineering work.
