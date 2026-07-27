# Gap Analysis — Phase 2

**Date:** 2026-07-27 · **Baseline:** `docs/ANALYSIS.md` (Phase 0) vs. the Phase 1 target.

**Method.** Every row carries a `file:line` or a quoted command result. Where I could not find something I name the paths searched. No claim rests on a document alone — this repository's own board records nine cases of an artifact asserting a control that did not operate.

---

## §0 Superseded direction — NOT gaps

**Owner ruling, 2026-07-27:** the following five Phase 1 items were each decided against previously. They are recorded here as **superseded direction**, not as missing work, and must not be planned against.

| Phase 1 item | Superseded by | Where recorded |
|---|---|---|
| "Remotely tear down nodes completely" from the TUI | Node **decommission** is a provisioning/administrative operation outside operator-runtime scope. `operator-node-writer` grants `nodes:[patch]` + `pods/eviction:[create]` only; `operator_rbac_test.go:96` forbids `create`/`delete`/`deletecollection`/`update`/`*`. Granting the long-running operator standing node-delete is explicitly rejected. | `KANZ_TASKS.md` → NODE-LIFECYCLE-BOUNDARY (2026-07-27) |
| Kill-switch "wiping binary payloads, RAM contents, DB connection states" | The node-wipe concept was a **host-vector** design that did not survive the 2026-07-16 Kubernetes decision (SSH-push cancelled). On k8s, revocation = stop the tenant's workloads, purge its Vault-CSI secrets, scale to zero, with `kanz-halt` as the in-band brake that stops trading first. | `KANZ_BRAIN.md` → SOV-04 |
| `root@company` (Tenant Admin) tier | SOV-01 closed as delivered. The `tenant` claim **is** the hierarchy (binds sub-user → company → `app.tenant_id` → RLS); `Read`/`Trade` is the RW/RO split. A `root@company` tier was deliberately not built: sub-user creation lives in Eighred SSO, basket allocation is deploy-time, exchange keys are Vault CSI. **Accepted consequence, recorded:** a tenant master and a trader are indistinguishable on `/v1`. | `KANZ_TASKS.md` → SOV-01 (closed 2026-07-16) |
| TradingView overlay / market-data ingest | TV has **no market-data redistribution API**, and its Reuters/Dow Jones news is licensed for display in TV's own UI only — consuming either breaches ToS and the underlying exchange licensing. Pine → webhook is the one supported direction and **is already built** (`services/webhook-ingest`). `tv-sync` is the OUTBOUND Broker-API projection, not an ingest node. | `KANZ_TASKS.md` (2026-07-14) |
| SSH-fingerprint device registration for the TUI | **TWO PLANES** decision: the HTTP/API plane stays OIDC-at-the-gateway; SSH-key auth belongs to the operator plane *because it is an SSH transport with a handshake to validate one in*. A thin TUI over gateway mTLS has no such handshake. | `KANZ_BRAIN.md` → SOV-01 (2026-07-14) |

**Consequence for Phase 3:** no milestone should implement any of the five. Where Phase 1 assumed them, the plan substitutes the delivered mechanism.

---

## §1 Requirement-by-requirement

Status: **EXISTS** / **PARTIAL** / **MISSING** / **FLAWED** · Effort: S/M/L/XL · Priority: P0 (MVP blocker) / P1 / P2

| # | Requirement | Status | Evidence | Missing | Effort | Pri |
|---|---|---|---|---|---|---|
| 1 | `kanz` launches a TUI from any shell | **PARTIAL** | `cmd/kanz/main.go:1-6` is a REPL client; `cmd/universe/main.go:1-8` is the operator TUI. Two binaries. | One entry point. The features of §1.2 sit in `universe`, not `kanz` | M | P0 |
| 2 | First-run onboarding / device registration | **PARTIAL** | `cmd/kanz` uses SSO **device flow** (`pkg/deviceauth`), token persisted via `cmd/kanz/internal/tokenstore` | SSH-fingerprint variant is **superseded** (§0). Device-flow onboarding exists | S | P1 |
| 3 | Session persistence across launches | **EXISTS** | `cmd/kanz/internal/tokenstore` | — | — | — |
| 4 | Claude-Code-like single-window UX | **PARTIAL** | `cmd/kanz/internal/repl` (REPL); `cmd/universe` is Bubble Tea panes | The two are different UX models; unifying is the §1 gap | M | P1 |
| 5 | Portfolio / basket CRUD in the TUI | **MISSING** | I searched `cmd/universe/` and `cmd/kanz/internal/repl/` for basket/portfolio create paths and found none. `operator/v1` has 11 RPCs, none for baskets | Basket CRUD surface end-to-end | L | P0 |
| 6 | API-key management, RAM-only | **EXISTS** | `SetVenueKeys`/`ListVenueKeys` in `operator/v1`; `operator/internal/secrets/kube.go:40`; write-only by construction (no value-returning method). Only `WriteFile`-near-key hits in the module are two **test** files | Nothing for MVP | — | — |
| 7 | Node deploy from the TUI | **EXISTS** | `AddNode`/`ListProvisions`/`TestConnection`; proven live on a two-node k3s estate (OPS-M2e) | — | — | — |
| 8 | Node removal from the TUI | **SUPERSEDED** | §0 | — | — | — |
| 9 | Monitoring: positions, PnL, order book, risk | **PARTIAL** | `cmd/kanz-monitor` streams `order.>` and `risk.position.changed.>` and polls OMS counters — but it is a **separate binary**, and the Book pane shows last-known only because risk-engine is off the rig | Fold into one TUI; no order-book depth pane | M | P1 |
| 10 | Kill-switch (halt) | **EXISTS (halt)** / **SUPERSEDED (wipe)** | `cmd/kanz-halt`, broadcast on a compacted stream, deny-by-default gate. Wipe semantics: §0 | Halt is not surfaced *in* the TUI | S | P1 |
| 11 | OKX + Binance only | **EXISTS** | `services/venue-binance/`, `services/venue-okx/` — no other venue adapters | — | — | — |
| 12 | BTC / ETH / OKB | **PARTIAL** | Symbol mapping is config-driven (`parseSymbolMap`, `venue-binance/cmd/.../main.go`); no hardcoded asset restriction found | Basket definition surface (row 5) is what binds assets | S | P1 |
| 13 | Signal → Risk Gate → Order (never direct) | **EXISTS** | `internal/signal/translate` is the single translator; publishing `StrategySignal` **executes nothing — nothing subscribes to it**; the OMS pre-trade gate is the choke point with both callers routed through `Evaluate` | — | — | — |
| 14 | Strict order idempotency | **EXISTS** | `internal/execution/{venue,closes,query,grpc_venue,fixvenue}.go` — our `order_id` **is** the venue `clOrdId`/`newClientOrderId`; `fill_id` deterministic `{venueSymbol}-{tradeId}`. **Note:** a grep scoped to `services/venue-*/` returns zero — the mechanism is centralised, not per-adapter | — | — | — |
| 15 | TradingView overlay | **SUPERSEDED** | §0 — Pine → webhook already built | — | — | — |
| 16 | Global Sovereign (`root@universe`) | **EXISTS** | Operator plane, SPIFFE SVID `ns/kanz-operator/sa/kanz-halt` (SEC-M3c) | — | — | — |
| 17 | Tenant Admin (`root@company`) | **SUPERSEDED** | §0 | — | — | — |
| 18 | Sub-user Write / Read-only | **EXISTS** | Capability model: `authz.Mux.Handle` takes the capability as a **required parameter** (a route without one does not compile); `Read`/`Trade` split; `api-gateway/internal/authz/arch_test.go:39-88` fails the build on an undeclared or under-privileged capital route | — | — | — |
| 19 | Zero floating-point on money | **EXISTS** | `internal/dec/dec.go:4` — float64 banned on money/prices/qty/L2; exact `*big.Rat` | — | — | — |
| 20 | Bitemporal ledger | **EXISTS** | `accounting/migrations/0001_ledger.sql` — `effective_time` + `knowledge_time`; `0004_ledger_worm.sql` adds an engine-level append-only trigger | — | — | — |
| 21 | Basket isolation (margin) | **EXISTS (per-account)** | EXEC-M16/SOV-02: `venue_account_id` on every order/fill/ledger entry; OMS refuses to start on an account shared by two portfolios; the router cannot reach another portfolio's credential | Isolation is per **exchange account**, which is the only real boundary an exchange enforces | — | — |
| 22 | Secrets never on disk | **EXISTS** | Vault CSI + `<KEY>_FILE`; no `.env` in repo; the only `WriteFile`-near-key hits are `cmd/kanz-provisioner/join_test.go:68` and `cmd/universe/model_validation_test.go:275` — both tests | — | — | — |

---

## §2 Critical defects (monetary or security consequence)

| ID | Defect | Evidence | Consequence | Pri |
|---|---|---|---|---|
| **C1** | **DR does not cover the order store — and it is unknown, not merely absent** | `infra/dr/postgres/cluster.yaml` defines 3 CNPG clusters; the README maps 6 services and **excludes 2 with a written reason**. Five services with `*-db` SecretProviderClasses are named nowhere: `oms`, `tv-sync`, `venue-binance`, `venue-okx`, `regulatory` | After a failover the OMS starts against an empty order store; `SweepInterrupted` walks zero rows and logs `count=0` — **the same line a healthy clean start produces**. In-flight orders vanish with no signal. ONBOARD-M6 means nobody can say which cluster it is in | **P0** |
| **C2** | **No Prometheus. Every degraded mode is unobservable** | `infra/observability/` ships `alerts/`, `dashboards/`, `slo/` and a `node-exporter.yaml` declaring only a Namespace + DaemonSet. No `kind: Prometheus`, no `prometheus.io/scrape`, no `prom/prometheus` image anywhere in `infra/`. The only `rule_files:` is `slo/slo_test.yaml`, a promtool **test** harness | Alert rules, SLOs and dashboards exist as artifacts and **none of them runs** | **P0** |
| **C3** | **The only alert rules fire on metrics that no longer exist** | `alerts/data-quality.rules.yaml` — all 8 metrics absent from Go, verified three ways (literal, component-fragment, and `internal/integrity` deleted). `DataQualityMetricsMissing` is `absent(kanz_data_quality_events_total)`, so the one rule that *can* fire would fire **forever** | False assurance; the path to a permanently silenced rule | **P0** |
| **C4** | **17 composition roots swallow the secret read error** | Every service + `cmd/kanz-migrate` has its own `func secret(k string)`; all 17 discard `os.ReadFile`'s error. **Fixed in `venue-binance`/`venue-okx` only** | A mounted-but-unreadable Vault CSI secret is indistinguishable from one never configured. **Calibration:** the two highest-risk fail-open paths are safe — `api-gateway/internal/config/config.go:149-158` refuses to start with no auth, and `OIDCIssuer` is read via plain `os.Getenv` (`:113`), **not** `secret()` | P1 |
| **C5** | **Docker Hub is a live single point of failure for the build pipeline** | Six incidents on 2026-07-27 alone: four logged earlier, plus `image (inference)` and `image (datamaster)` failing with `registry-1.docker.io` timeouts on PRs #46 and #48 | Any release or merge can fail on an unrelated third party. Known mitigation (base-image mirroring to ghcr, registry auth) is unimplemented | P1 |
| **C6** | **Release self-verification cannot fail** | `release.yml`'s verify step uses `${{ github.repository }}` for `--certificate-identity-regexp`, so it can never disagree with what just signed | Cosign signatures are produced but **never independently verified**. This is one of the two open P0 criteria | **P0** |

**Explicitly checked and NOT found** (recording negatives so they are not re-raised): secrets written to disk in product code; float arithmetic on money; non-idempotent order submission; unauthenticated capital routes.

---

## §3 Architectural debt

| Item | Assessment | Cost |
|---|---|---|
| **Four terminal binaries** (`kanz`, `universe`, `kanz-monitor`, `kanz-halt`) where the target wants one | **Refactor, do not rebuild.** All four already speak to the same gateway edge; the work is a shell + pane composition, not new capability | M |
| **`secret()` duplicated 17×** | **Refactor.** Promote one helper + an arch guard forbidding a local redefinition. Precedent: `internal/dec` consolidated 5 copies for the same reason | M |
| **Observability declared but not deployed** | **Rebuild the collection path**, keep the definitions. Instrumentation is sound — 5 of 6 SLO metrics exist | M |
| **`replace` → `kanz-schemas-go`** | **Keep.** Load-bearing and correct until a publish target exists (PARITY-07) | — |
| **`web-bff` parked** | **Keep frozen.** Cancelled 2026-07-13, costs nothing while unbuilt | — |

---

## §4 Over-engineering relative to MVP

Judged strictly against *"OKX + Binance, BTC/ETH/OKB, signal → risk gate → order"*. **Recommendation for all: freeze, do not delete** — each is delivered, tested and carries no MVP maintenance cost, and deletion would be the larger change.

`services/{alternatives, wealth, datamaster, regulatory, optimization, performance, copilot, lineage, autopilot, market-data}` — private-markets, wealth, MDM, filings, portfolio optimisation, AI copilot and data-lineage surfaces, none on the MVP execution path. `services/alternatives` and `services/wealth` additionally have **no automated producer** — their only publishers are operator CLIs.

---

## OPEN QUESTIONS

1. **Which database does each service use in production?** ONBOARD-M6 — six conflicting in-repo answers; resolvable only by `vault kv get`.
2. **Are `oms`/`tv-sync`/`venue-*`/`regulatory` in any DR cluster?** Blocks C1.
3. **Where are the venue build tags?** `KANZ_BRAIN.md` states `//go:build binance|okx`; I searched `services/venue-*/internal/*/*.go` and did not locate them.
4. **Does a production environment exist at all?** The only reachable context in prior sessions was a kind rig; `kanz-data`, `spire-system`, `vault` namespaces absent.
5. **Basket definition — where should it live?** Row 5 is a P0 gap with no existing surface. Deploy-time binding (SOV-02) currently owns portfolio→account mapping and *refuses to start on a conflict*; a runtime CRUD API re-opens that. This is a design decision, not a coding task.
