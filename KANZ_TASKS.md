# KANZ TASK BOARD

Execution board — not history. Task IDs are module-prefixed and scoped to 1–3 engineer-days; epics split into lettered subtasks. Active work lives in **TODO**; the open roadmap lives in **PATH TO PARITY** (dependency-ordered); completed epics collapse to a one-line record in **DONE**. Architectural decisions live in `KANZ_BRAIN.md`. The codebase is the ground truth — verify a gap before creating a task.

---

## TODO

_The credential-/infra-/human-gated half of PATH TO PARITY still waits on an external boundary landing. **The `kanz` CLI vertical is complete** (CLI-01 device-flow RP + CLI-02 terminal binary), and **WIRE-03 has landed** — the copilot citation seed now carries a real, verifiable durable-log position end to end. One buildable, non-gated backend gap remains (REG-02); the rest of the product surface is dependency-ordered in PATH TO PARITY. The 3-year horizon lives in `KANZ_ROADMAP.md`._

### Buildable now — carried-over backend gaps

- [ ] **REG-02** Wire a durable `LinkSink` for the regulatory `ChainSigner`. `cmd/regulatory` `buildSigner` keeps hash-chain links in-memory only, so filings are not tamper-evident across restarts. Persist the link append behind the in-memory-default/Postgres `Store` seam (the PARITY-02 pattern). · buildable
- [ ] **DEBT-ARCH-01** Restore the RISK-02 import boundary. `test/arch` `TestRiskBoundary_OutsidersUseAPIOnly` fails on three pre-existing violations — `internal/lake/dataset`→`risk/compute`, `services/oms/internal/position`→`risk/ingest`, `test/contract/tenancy`→`risk/state/persist` — outsiders reaching past `risk/api/*`. Route each through the `api/v1` surface (extend it only where a genuine capability is missing), re-greening the arch guard. · buildable

**Recommended next: REG-02** — the last buildable, non-gated backend gap: persist the regulatory hash-chain links so signed filings stay tamper-evident across restarts, closing a real durability hole behind the delivered `ChainSigner`/`Store` seam. Beyond it, **DEBT-ARCH-01** (a newly surfaced, pre-existing gap — three outsider packages import risk internals, failing the RISK-02 boundary arch test) is a buildable correctness/architecture task; **PS-02** (web workspace over the now-proven edge) is the next product-surface step.

---

## PATH TO PARITY

> The DONE board delivered the analytic surface of an Aladdin-class platform. What remains between that and production is the carried-forward integration work, split into a **buildable half** (WIRE-01/02 — composition-root wiring of delivered seams, no credentials) and a **credential-gated half** (external vendor SDKs, licensed datasets, real infra, human audit/UAT loops). Each subtask is 1–3 engineer-days; "done" includes tests, `go build/vet/gofmt` clean, arch test unchanged, and the carried-forward note on the relevant DONE epic retired.

### Sequencing — 6-month milestone map

_Dependency-ordered. The buildable tranches (M0–M2) need no credentials and come first; the rest gates on an external boundary landing, so it is sequenced by when that boundary typically becomes available, not by engineering size._

- **M0–M1 · WIRE-01 (live wiring). ✅ DONE.** The delivered feed/consumer/calibrator/filing seams now run in composition roots (a–f complete; see DONE board).
- **M1–M2 · WIRE-02 (query.v1 ownership extension). ✅ DONE.** `query.v1` carries the owning-tenant + source-log-position surface; the `governed.GRPCClient` reads it and retired `StubClient` at `cmd/copilot`, closing the last PARITY-04b half.
- **M2–M3 · Vendor `Source` / external-SDK bindings.** Bind the delivered seams to real SDKs as creds land: Bloomberg/Refinitiv/ICE `Source`, anthropic (`-tags anthropic`), go-redis (`-tags redis`), quickfix `FIXSession`, `settlement.v1` `FailEncoder`. Credential-gated.
- **M3–M4 · Licensed dataset load + reconciliation.** Load ISDA SIMM / BCBS FRTB / NGFS + the live-FX feed into the delivered `*Inputs`/parameter seams and reconcile against worked examples. License-gated.
- **M4–M5 · Real-infra bring-up.** Stand up a live Redis + a real DR region, apply the migrations against `kanz-books`, and run the PITR/DR drills for real. Infra-gated.
- **M5–M6 · SDK publish + human loops.** PARITY-07b–d (publish + strip the `replace`), the third-party SOC 2 audit engagement, and first-client UAT execution. Ops/human-gated.

### Product surface — end-user client & marketplace (opened 2026-07-08)

_New direction: the delivered backend needs a front-of-house. Each item wires to an existing edge seam — **no parallel backend, no new analytics.** Dependency-ordered; the CLI vertical (CLI-01/02) is **delivered** (see DONE). Every line below promotes to a 1–3 day TODO task only after a ground-truth verification when its turn comes — listed here as committed direction, not decomposed work._

- **PS-01 · Consumer SSO portal → moved out to the standalone Eighred SSO project (2026-07-08).** Identity, `login.eighred.com`, SAML, MFA, and the device authorization server are owned by **Eighred SSO** (`../eighred-SSO`), not kanz. kanz integrates only as an OIDC **relying party**: the delivered AUTH-01a authenticator validates Eighred-SSO-issued JWTs via JWKS — nothing to build here beyond CLI-01's RP integration. No portal, SAML, or MFA work happens in this repo.
- **PS-02 · Web workspace (BFF + dark terminal UI).** The Bloomberg/Linear-style shell: left nav + workspace + AI panel + market widgets, over the same `/v1` edge the CLI uses. Needs a BFF/session layer in front of the gateway; reuses `/v1/ask` + risk endpoints. Follows the CLI so the edge contract is proven by two clients before the UI surface grows.
- **PS-03 · Fund discovery marketplace.** Filterable catalog (risk/volatility/drawdown/asset-class/region/sector/ESG/liquidity/fees) + fund pages (overview, 1D–5Y performance, VaR/Sharpe/Sortino/vol/drawdown, benchmark) with AI "explain this fund" over the copilot. `datamaster` is golden-source data, **not** a discovery surface — this needs a new query/index seam over it. Verify the datamaster read model before decomposing.
- **PS-04 · Investment onboarding + funding flow.** Fund selection → strategy/risk/fee review → bull/base/bear simulation → funding (regulated custody / connect brokerage / external broker). Backend seams exist (`oms`, `accounting`, settlement); this is the UX flow + funding-option integrations (custody/broker connectors credential-gated). Keeps analytics / recommendation / **execution** strictly separated per the brief.
- **PS-05 · TradingView visualization + webhook→event bridge.** Embedded charts, watchlists, alerts; TradingView webhook → gateway → the existing event spine → copilot analysis → user notification. Buildable bridge; charting is an embed (licence-gated).
- **PS-06 · Subscription / SaaS billing.** Free / Professional / Premium / Enterprise plan gating, enforced at the edge over the delivered per-tenant quota + RBAC seam (`middleware.quota`, MT-01). New billing-provider integration (credential-gated); plan→entitlement mapping is buildable.
- **PS-07 · Customer management system (CMS).** At scale the operations surface is a CMS, not an admin panel: customer/account operations, subscriptions, API connections, audit-log access, and security policies over the delivered `compliance` / `audit` / PARITY-06 backends. Customer *identity* operations (account lifecycle, least-privilege support-agent access) belong to Eighred SSO's CMS (`../eighred-SSO`, D-007); kanz's CMS covers the investment-product surface only and federates identity to SSO — no duplicate customer store.

### PARITY-07 — SDK distribution (credential-gated)

_Retire the load-bearing `replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go` with published, versioned SDKs. The workflow + CI guard already exist (PARITY-07a); only the publish target needs infra credentials this box lacks. Not a run blocker — the `replace` builds and runs today._

- [ ] **PARITY-07b** Stand up the publish target: tagged internal `kanz-schemas-go` companion repo + a private GOPROXY, and a private index / GitHub Packages for the Py/TS packages · infra
- [ ] **PARITY-07c** Run the release pipeline → tagged `kanz-schemas-go` + Python/TS SDK artifacts · `kanz-schemas` CI
- [ ] **PARITY-07d** Pin the published versions, delete the `replace` directives in `kanz/go.mod` + the kanz-py/ts equivalents, and arm the dormant `KANZ_SDK_PUBLISHED` no-`replace` CI guard · `kanz/go.mod`, `kanz-py/pyproject.toml`

### Credential-/schema-gated carried-forward (not in an active tranche)

_Each sits behind a delivered seam; only the external boundary is missing._

- [ ] **Composition-root vendor `Source`/binding wiring** where the SDK/creds exist (CI/prod): Bloomberg BLPAPI / Refinitiv RTSDK / ICE (`Source`); anthropic-sdk-go (`-tags anthropic`); go-redis (`-tags redis`, `redisadapter`); quickfix (`FIXSession`); the `settlement.v1` `FailEncoder` + DTCC/SWIFT `SettlementVenue`; the `platform.model` bus binding + real `ModelLoader`; the bus-backed `DecisionRecorder` producer at copilot/gateway.
- [ ] **Licensed datasets** for calibration + filing reconciliation: ISDA SIMM / BCBS FRTB / NGFS climate parameter tables, the live-FX feed, and the regulator worked-example datasets — loaded at the composition root against the delivered `*Inputs` structs.
- [ ] **Real infra + human loops**: a live Redis, a real DR region, applying the migrations against `kanz-books` + driving the PITR drill; the third-party SOC 2 audit engagement + first-client UAT execution.

---

## IN PROGRESS

_Nothing in flight._

---

## DONE

_Historical record — one line per completed epic. Full implementation detail is in git history; durable decisions are in `KANZ_BRAIN.md`._

### Phase 10 — Product surface (end-user client)

- **CLI-02** — the `kanz` terminal binary (`cmd/kanz`): a Claude-Code-style REPL that completes the CLI-01 device flow, persists the token (0600, atomic, under `os.UserConfigDir`), renders the KANZ TERMINAL header, and drives turns over the delivered `/v1` edge — natural-language questions through `POST /v1/ask` (answer + citations + refusal/injection/ungrounded warnings) and slash commands (`/exposure`, `/measures`, `/scenario`) through the risk endpoints, plus `/login`/`/logout`/`/whoami`. Auto-runs the device flow on the first turn when no valid token is cached; the bearer is read through a closure so a mid-session `/login` is picked up live. Pure client — talks only to the running gateway (which validates the SSO token via the delivered OIDCAuthenticator); no backend, no mocks. Split into testable `config`/`tokenstore`/`gateway`/`repl` packages; the copilot `/v1/ask` is a single-JSON response (not a stream — the board's "streaming" phrasing was aspirational; ground truth won). Completes the CLI vertical.
- **CLI-01** — the kanz CLI is now an Eighred SSO relying party for the RFC 8628 device-authorization grant: new `pkg/deviceauth` (client-side counterpart to `pkg/auth`'s server-side JWKS validator) resolves the device + token endpoints from the issuer's OIDC discovery document, begins authorization, shows the user_code/verification URI via a caller callback, and polls the token endpoint honoring the server interval + `slow_down` back-off and the `device_code` expiry — returning the issued token for a caller to bearer against the gateway. Adds NO auth endpoints and holds NO keys; identity stays owned by Eighred SSO. Injectable clock/wait keep the poll-cadence + expiry tests deterministic and fast. Unblocks CLI-02.

### Phase 9 — Live wiring

- **WIRE-03** — surfaced the risk-engine durable-log fold position into `query.v1` `source_position`, completing the copilot citation seed end to end. The `api/v1.Engine` `ExposureResponse`/`MeasuresResponse` now carry a `SourcePosition *common.v1.LogPosition`; `EngineImpl` populates it from the served portfolio's applied snapshot `LogPosition` (RISK-05 `SetSnapshot`) on the live path and leaves it nil on the degraded cache read or before any positioned snapshot is applied (an absent, not a zero, coordinate); `grpcsrv` stamps it onto the proto response. The copilot `governed.GRPCClient` already read `GetSourcePosition()` into its `citation()` (WIRE-02b), so a real position now flows risk-engine → gateway → copilot `/v1/ask` citations → CLI "Sources:" (e.g. `risk.state@3:4242`). The Kafka-offset-backed `LogPositionSource` (PERS-01d) remains future work; this surfaces the honest last-anchored coordinate the engine already holds.
- **RISK-12** — retired the RISK-07 1%×gross VaR99 placeholder at the risk-engine composition root: when `RISK_ENGINE_MARKETDATA_DATABASE_URL` is set, `cmd/risk-engine` opens the shared MODEL-01b price-history store (read-only; prices are universal market fact, not tenant-scoped, so no RLS/GUC) and registers `varmodel.Historical` over a `compute.StoreReturnsProvider` via `varmodel.Register`, so the shared registry the recomputer + query `EngineImpl` both use serves data-driven historical-simulation VaR read point-in-time-correct. Absent the DSN the placeholder stands — the honest no-market-data fallback. The model + end-to-end override test (`TestRegister_OverridesPlaceholderEndToEnd`) already existed; this was the composition-root wire + provider binding.
- **WIRE-02** (a–b) — closed the last carried-forward PARITY-04b half: extended `query.v1` `ExposureResponse`/`MeasuresResponse` with `owner_tenant` (the deny-by-default authz-gate input) + `source_position` (`common.v1.LogPosition`, the citation seed); the risk-engine gRPC server stamps `owner_tenant` from its single-tenant `cfg.Tenant`; the copilot `governed.GRPCClient` reads the extended surface over mTLS query.v1 and retired `StubClient` at `cmd/copilot` (`COPILOT_RISK_QUERY_ADDR`), feeding the owning tenant to the authz gate + the citation seed to answers. An empty `owner_tenant` fails the gate CLOSED; `source_position` is populated once the engine surfaces its durable-log fold position.
- **WIRE-01** (a–f) — wired the delivered PARITY seams into running composition roots: `feed.BusSink` + `SyntheticSession` publish normalized market events onto the spine (`cmd/market-data`, `MARKET_DATA_FEED`); `cmd/accounting` folds live `order.v1` fill FACTs + emits/folds `accounting.v1` subscription/redemption/fee cash FACTs (`cashmove.Publisher` + the cash-movement endpoint) into the shared journal, and values multi-currency NAV off a live `fxfeed.LiveFX` cache with no `fx` in the request; `cmd/risk-engine` runs the `schedule.Scheduler` over `curve.Calibrator`, fed by a `livequote.LiveQuotes` cache off the market spine (deny-on-garbage preserved); a new `regulatory` service serves signed, completeness-gated FRTB/FormPF/AIFMD/TCFD/SFDR filings from request-supplied `*Inputs` via the promoted `signer.ChainSigner`. Each consumer folds the spine into its own last-value cache (a service cannot import another's `internal/`); `chain`/`signer` promoted to `internal/audit/` on the second-consumer trigger; `accounting.v1` generated into the local SDK. Vol reuses the generic scheduler once wired; credit lacks a `Refresh`/`QuoteSource` seam.

### Phase 8 — PATH TO PARITY (Aladdin-class floor)

- **SVCWIRE-01** (a–f) — operationalized the Phase-7 wealth/datamaster/copilot/alternatives services: GitOps deploy manifests, gateway routes behind edge auth + mTLS mesh backend, KEDA/PDB scaling, end-to-end contract test, proto CODEOWNERS.
- **PARITY-01** (a–i) — live market/reference/ESG data plane: vendor-agnostic feed adapter + validating normalizer, Bloomberg/Refinitiv/ICE + reference + ESG adapters, live DQ gate (drop-vs-report), curve/vol/credit snapshot surface, soak/chaos harness. _Carried-forward: live vendor `Source` bindings (bus-producer `Sink` wired by WIRE-01a)._
- **PARITY-02** (a–h) — durable, replayable state: Postgres behind every Store (IBOR/fund/book/datamaster/risk), bus consumers folding live FACTs, migrations + PITR, replay-determinism contract. _Carried-forward: live broker subjects + proto Decoder bindings (cash-movement producer + fold wired by WIRE-01b/f)._
- **PARITY-03** (a–i) — model calibration + independent validation: curve/vol(SVI)/credit(CDS) calibration, live factor model, alternatives/climate/regulatory calibration, deny-by-default validation gate (SR 11-7), backtesting/attribution. _Carried-forward: live quote/universe bindings + licensed parameter datasets (curve Refresh schedule wired by WIRE-01c; vol/credit reuse the generic scheduler once wired)._
- **PARITY-04** (a–i, both 04b halves) — real external adapters behind delivered seams: Claude client (build-tag), FIX venue, settlement calendar, hash-chain Signer, bus DecisionRecorder, Redis PendingStore, model registry coordination, LineageCatalog, BusFailSink, governed.Client (query.v1 ownership surface — WIRE-02). _Carried-forward: external SDK bindings (creds-gated)._
- **PARITY-05** (a–f) — scale/HA/latency: consistent-hash shard ring, partitioned market-data hot path, shared-state cutover (redis build tag), load/capacity model, DR drill, multi-currency NAV. _Carried-forward: composition-root bindings (shard member lists, live Redis, DR region — live FX feed wired by WIRE-01d)._
- **PARITY-06** (a–f) — regulatory certification + first-client onboarding: FRTB/FormPF/AIFMD/TCFD/SFDR signed filings, SOC 2 Type II evidence pipeline, tenant onboarding automation, audit-engagement + findings lifecycle. _Carried-forward: licensed datasets + human audit/UAT loop (live signed-filing service wired by WIRE-01e)._
- **PARITY-07a** — SDK-release workflow + dormant DEBT-01d no-`replace` CI guard (armed by PARITY-07d).

### Phases 1–7 — Foundation, hardening & analytic surface

- **EVT-01…21** — event platform: envelope/payload proto design, `common.v1`, schema registry (storage/ingest/resolve), Go/Python/TS bus clients (stamping/lineage/dedup/retry/DLQ), Kafka log + NATS spine provisioning, replay tooling, cross-language + compatibility contract tests.
- **RISK-01…11** — risk engine domain: state ingestion/apply, exposure computation, measure registry, scenario engine, uncertainty propagation, degraded mode, output publish, correctness + integration tests, import-boundary arch test.
- **PRED-01…14** — Go↔Python prediction layer: inference proto schemas, streaming + interactive paths, feature store, model registry, shadow/canary executor, degraded-mode + backpressure/lag-scaling tests.
- **DATA-01…11** — data integrity layer: gap/staleness/watermark-late/drift detectors, NATS↔Kafka reconciliation, `quality_flags` emission, DataQualityEvent publishing, observability dashboards + alerting.
- **ORCH-01 / PERS-01** — risk-engine runtime service + durable state (Postgres StateStore, periodic snapshotter, restart-recovery bootstrap).
- **SEC-01 / AUTH-01 / MT-01** — zero-trust (SPIFFE/SPIRE + mTLS + Vault/CSI secrets), OIDC/JWKS authN + deny-by-default authZ + forged-issuer guard + decision logging, multi-tenancy (envelope tenant_id, broker isolation, Postgres RLS, per-tenant quotas, lifecycle automation, isolation contract tree).
- **API-01** — api-gateway BFF: risk query gRPC proto + server + edge chain.
- **CICD-01 / OBS-01** — Go/Python/TS CI + buf schema gate + supply-chain gate + distroless builds; telemetry (metrics/traces/dashboards/alerts).
- **INFRA-01 / DR-01 / SRE-01 / AUDIT-01 / MLOPS-01** — cloud foundation (Terraform + Argo GitOps + KEDA autoscaling + multi-AZ/PDB/quotas); disaster recovery (Kafka MirrorMaker2 + Postgres PITR + NATS rebuild + failover automation + quarterly drill); reliability (SLOs, error budgets, chaos); auditability + WORM regulatory reporting; model validation/explainability/governance.
- **Real-analytics & market-data plane** — reference/market-history schemas, point-in-time store, historical + Monte-Carlo VaR, factor models, fixed-income/curve analytics, performance attribution, portfolio optimization, liquidity risk, XVA/SA-CCR, collateral/margin/SIMM, structured products, regulatory capital, IBOR, post-trade lifecycle, private markets/alternatives, climate/sustainability, goals-based wealth, golden-source data master, AI analytics copilot.
