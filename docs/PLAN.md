# Execution Plan — Phase 3

**Date:** 2026-07-27 · **Inputs:** `docs/ANALYSIS.md` (Phase 0), `docs/GAP.md` (Phase 2).

**Governing constraints**

- The five items in `GAP.md` §0 are **superseded direction**. No milestone implements them; where Phase 1 assumed them, the plan uses the delivered mechanism.
- **Baskets remain a deploy-time concern** (owner ruling 2026-07-27). No runtime CRUD API. Portfolio→exchange-account binding stays SOV-02's deploy-time contract, which *refuses to start on a conflict* — a runtime API would re-open that refusal.
- Scope is **OKX + Binance**, **BTC/ETH/OKB**. Phase 1.6 items are checked only for architectural compatibility, never built.
- Every milestone leaves the tree building, green, and deployable.

**A word on sizing.** Several Phase 1 milestones are largely delivered. Saying so is more useful than inventing work to fill a heading — the honest remainder is smaller than the brief implies, and concentrated in **verification and operability**, not features.

---

## M0 — Hardening (no new features)

The only milestone that is genuinely mostly-new work, and the one that unblocks trust in everything after it.

### M0.1 · CI reliability — Docker Hub is a single point of failure *(owner: P0-adjacent)*

**Files:** `.github/workflows/build.yml`, `release.yml`, `kanz-ci.yml`; all 26 Dockerfiles' `FROM` lines; new `test/arch/baseimage_registry_test.go`.

Seven `registry-1.docker.io` failures on 2026-07-27, three of them consecutive PRs each needing a manual re-run. Mitigation, in preference order: **mirror base images to `ghcr.io/eighred/base/*`** and point every `FROM` at the mirror; failing that, authenticate Docker Hub pulls for the higher rate limit.

- **Acceptance:** `grep -rE '^FROM ' kanz --include=Dockerfile* | grep -vc 'ghcr.io'` returns **0**; ten consecutive `kanz-build` runs on `main` with zero registry-timeout failures.
- **Guard:** an arch test failing the build on a `FROM` outside the mirror, with a named-exemption map.
- **Rollback:** revert the `FROM` lines; the mirror is additive.
- **Risk:** the mirror needs a refresh path or it silently ages. Ship a scheduled sync workflow with it, not after.

### M0.2 · Independent release verification *(closes GAP C6, P0)*

`release.yml`'s verify step uses `${{ github.repository }}` for `--certificate-identity-regexp`, so it **cannot disagree with what just signed**.

- **Acceptance:** on a machine that did not build the image — `cosign verify ghcr.io/eighred/oms@sha256:<digest> --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-identity-regexp '^https://github.com/eighred/kanz/.github/workflows/release.yml@refs/tags/v.*$'` exits 0; and `cosign download sbom` returns a non-empty SBOM.
- **This is one of the two standing P0 items and cannot be closed from inside CI.**

### M0.3 · DR database coverage *(closes GAP C1, P0 — first action is not code)*

- **Step 1:** `vault kv get -field=dsn kv/kanz/oms` (and per service). Resolves ONBOARD-M6.
- **Step 2:** every service with a `*-db` SecretProviderClass is either in a CNPG cluster carrying the WAL+PITR contract, or in a **named exclusion with a written reason** — the shape `market-data` and `audit` already have.
- **Step 3:** an arch guard failing the build on a service in neither.
- **Acceptance:** the guard passes; `infra/dr/postgres/README.md` maps all 13.
- **Risk:** if the OMS store proves unbacked, this becomes a data-loss finding, not a documentation one.

### M0.4 · Observability collection path *(closes GAP C2/C3, P0 — OBS-001)*

Delete or quarantine `alerts/data-quality.rules.yaml` (8/8 metrics deleted with `internal/integrity`); drop `kanz_data_staleness_lag_seconds` from the SLO set; deploy or connect a Prometheus; verify node-exporter **and** application metrics are scraped.

- **Guard:** every metric named in `alerts/` and `slo/` exists in code. **It must strip `_bucket`/`_count`/`_sum` before matching** — those are Prometheus-generated, not declared in Go, and a guard red on correct configuration gets deleted. I produced four false positives by hand doing exactly this.
- **Acceptance:** `promtool check rules` clean; a Prometheus target list showing all 26 services plus node-exporter; `kanz_venue_orderview_durable` visible.

### M0.5 · Secret-loading standardisation *(GAP C4 — SEC-CONFIG-001)*

Promote one helper distinguishing a declared-but-unreadable `*_FILE` from absent configuration; migrate all 17 call sites; **arch guard forbidding a local `secret()`**. `venue-binance`/`venue-okx` are already done and are the reference.

- **Acceptance:** `grep -rc 'func secret(' kanz --include=*.go` returns 1; guard mutation-proven.
- **Risk:** touches every composition root's startup path. Land it alone, not alongside behaviour changes.

**M0 exit:** `go build ./... && go vet ./... && go test -p 1 ./...` green; `release.yml` green; a clean-machine `cosign verify` transcript; the DR mapping complete.

---

## M1 — TUI & Identity

**Already delivered:** session persistence (`cmd/kanz/internal/tokenstore`), SSO device-flow onboarding (`pkg/deviceauth`), and the RBAC layer — a **capability** model where `authz.Mux.Handle` takes the capability as a required parameter, so a route without one does not compile (`api-gateway/internal/authz/arch_test.go:39-88`).

**Actual remainder: composition, not capability.** Four terminal binaries exist where the target wants one — `kanz` (REPL), `universe` (operator panes), `kanz-monitor` (live feeds), `kanz-halt` (brake). All four already speak to the same gateway edge.

- **Files:** `cmd/kanz/main.go` (becomes the shell), `cmd/kanz/internal/repl`, plus pane packages lifted from `cmd/universe` and `cmd/kanz-monitor`.
- **Approach:** make `kanz` the single entry with sub-screens; keep the existing binaries building during the transition so each step is revertable.
- **Acceptance:** `kanz` from `bash`, `zsh`, PowerShell and CMD opens the TUI with no flags; every pane reachable by keyboard; `cmd/universe` and `cmd/kanz-monitor` still build.
- **Rollback:** the standalone binaries remain until the shell is proven; deleting them is a separate, later commit.
- **Risk:** `kanz-monitor` is arch-guarded as a **read-only observer** (`TestReadOnlyObserversNeverJoinAQueueGroup`, `TestReadOnlyObserversCannotPublish`). Folding it into a binary that also writes **must not** let it join a durable consumer group or gain publish rights. Those guards are the acceptance criteria, not a formality.

---

## M2 — Baskets (deploy-time) & Secret Vault

**Scope reduced by owner ruling.** No runtime CRUD.

**Already delivered:** RAM-only secret handling (Vault CSI + `*_FILE`, no on-disk writes in product code — the only `WriteFile`-near-key hits in the module are two test files); `SetVenueKeys`/`ListVenueKeys`, write-only by construction; pre-write account proof against the exchange (S4b); per-exchange-account basket segregation (EXEC-M16/SOV-02).

**Remainder:**
1. **Document the deploy-time basket contract** as the supported path — `OMS_VENUE_ACCOUNTS` binding, the start-time refusal on a shared account, and where an operator edits it.
2. **Close the venue-key loop against a real exchange** — `OPERATOR_VENUE_PROOF` is off, gated on Binance testnet credentials.

- **Acceptance:** a documented deploy-time basket change reaches a running OMS and is refused when two portfolios share an account (that refusal is the feature); a venue key entered in the TUI is proved against the exchange and the adapter logs its resolved account id.
- **Risk:** the exchange-proof path needs live credentials; it cannot be closed on this box.

---

## M3 — Execution

**Already delivered:** OKX and Binance adapters; `internal/execution` idempotency where our `order_id` **is** the venue `clOrdId`; deterministic `fill_id`; the healing seam with a 1500 ms in-flight timeout; bitemporal WORM ledger.

**Remainder: proof against a real venue.** Every cluster proof to date used `SimVenue`. Nothing has executed against OKX or Binance testnet.

- **Acceptance:** a BTC/ETH/OKB order placed on **testnet** through the full chain, appearing in `ledger_entries` with a `venue_account_id`; a deliberate duplicate submit producing **one** exchange order; an ambiguous timeout recovered by `clOrdId` query rather than re-submission.
- **Rollback:** venue adapters are per-venue build-tagged; the default binary is vendor-free.
- **Risk:** this is the first time real money semantics are exercised. Testnet only until M4's gate is proven live.

---

## M4 — Risk Gate

**Already delivered, and stronger than the brief asks:** the pre-trade gate refuses to evaluate what it cannot value (`Unpriced` → `PRICE_UNAVAILABLE`, terminal); both callers routed through `Evaluate` so the second door cannot drift; position ceilings, concentration, restriction, issuer-exclusion, currency and leverage rules; a post-trade monitor.

**Remainder:** the **auto-unwind skeleton** (Phase 1.6 lists auto-deleveraging as long-term; only the skeleton is in MVP scope), and confirming COMP-M2's reference-price source so MARKET/STOP orders are admissible against governed portfolios.

- **Acceptance:** a MARKET order against a governed portfolio is admitted once a reference price exists and still refused without one; a breach triggers the unwind skeleton's decision path without executing.
- **Risk:** COMP-M1 deliberately made unpriced MARKET orders reject. Wiring a price source must not become a silent bypass — the guard is that absence still refuses.

---

## M5 — Node Management & Kill-Switch

**Already delivered and cluster-proven:** node join, cordon/uncordon, drain (both confirm branches), region relabel, `TestConnection`, and the `kanz-halt` broadcast brake on a compacted stream that a pod booting tomorrow still learns.

**Superseded:** node teardown from the TUI, and RAM/binary wiping (`GAP.md` §0).

**Remainder:**
1. **Surface halt/resume in the TUI** — the brake exists but is a separate binary.
2. **OPS-M2f-a** — replace the control-plane pin with a labelled provisioning pool. Today draining the k3s server makes Add Node and Test Connection unschedulable, and pinning *moved* the eviction deadlock rather than removing it.
3. **OPS-M2f-b runtime half** — watch a freshly joined node actually pull with the `ghcr-pull` credential. Attached at 26 ServiceAccounts and guarded; never observed.

- **Acceptance:** halt and resume driven from the TUI with the platform observed stopping and restarting; the k3s server drained while Add Node still succeeds; a node joined and a platform workload scheduled onto it with **no manual image step**.
- **Risk:** the provisioning Job mounts cluster-admission credentials. Any placement change must preserve credential confinement — that is why it is pinned today.

---

## M6 — Telemetry

**TradingView overlay is superseded** (`GAP.md` §0): no market-data redistribution API, and TV's Reuters/DJ news is licensed for display in TV's own UI only. Pine → webhook is the supported direction and **is already built**.

**The replacement, and it is not a new product:** the monitoring surface folded into the TUI (M1) plus the observability pipeline (M0.4). Between them they deliver what 1.4 actually asked for — positions, PnL and health — without a ToS breach or a parallel spine.

- **Acceptance:** open positions, PnL and risk metrics visible in the TUI from live feeds; Grafana or equivalent showing the SLO burn rates the recording rules already compute.
- **Constraint carried from Phase 1:** strategy details must never leave the estate. Only position/PnL-level telemetry is externally visible — and `tv-sync`'s outbound push (SOV-06) must carry instrument, side, size, entry and **nothing about how the decision was made**.

---

## Sequencing

**M0 first, and not partially.** Everything after it is verified by a pipeline that currently cannot complete a merge without manual intervention (M0.1), signs images nobody independently verifies (M0.2), may not back up the order store (M0.3), and cannot observe any degraded mode (M0.4).

Then **M1** (composition, low risk, unblocks M2/M5/M6 surfaces) → **M2** (thin, deploy-time) → **M3** (first live-venue proof) → **M4** (gate hardening once real orders flow) → **M5** (operability) → **M6** (telemetry, depends on M0.4 and M1).

M3 must not precede M0.3: placing real orders against a store that may not be backed up is the one ordering error with an unrecoverable failure mode.

---

## Risks and unknowns

1. **No production environment is known to exist.** Every "deploy" acceptance criterion assumes one. If the only estate is a kind rig, M0.3 and M5 change shape.
2. **Live-exchange credentials are unavailable here.** M2's proof and all of M3 are gated on them.
3. **Docker Hub.** Until M0.1, any milestone's CI can fail for reasons unrelated to its work — and a pipeline that fails randomly is one nobody reads.
4. **The 17-copy `secret()` migration touches every service's startup.** Highest blast radius in M0; land it alone.
5. **`SimVenue` has never been wrong in a way that mattered — yet.** Every proof to date is against a simulator that fills at limit. M3 is where that assumption gets tested.

---

## OPEN QUESTIONS carried forward

1. Which database does each service use in production? (blocks M0.3)
2. Does a production environment exist at all? (shapes M0.3, M5)
3. Where are the venue build tags? `KANZ_BRAIN.md` states `//go:build binance|okx`; I searched `services/venue-*/internal/*/*.go` and did not locate them. (affects M3 rollback)
4. Is there an OKX/Binance **testnet** account available? (blocks M3 entirely)
