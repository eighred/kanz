# KANZ TASK BOARD

Execution board — not history. Task IDs are module-prefixed and scoped to 1–3 engineer-days; epics split into lettered subtasks. Active work lives in **TODO**; the open roadmap lives in **PATH TO PARITY** (dependency-ordered); completed epics collapse to a one-line record in **DONE**. Architectural decisions live in `KANZ_BRAIN.md`. The codebase is the ground truth — verify a gap before creating a task.

---

## TODO

_Active tranche: **WIRE-02 (query.v1 ownership extension)**. WIRE-01 (a–f, live wiring) is complete: the delivered feed/consumer/calibrator/filing seams now run in composition roots (see DONE). PARITY-01…06 is complete to its in-repo ceiling. WIRE-02 closes the last carried-forward PARITY-04b half — the `query.v1` risk-state responses surface neither the owning tenant nor a source-log-position, so a query.v1-only governed client would make the cross-tenant authz gate always pass. Buildable on this box (schema change + buf regen), no credentials._

**Recommended next: WIRE-02a** (extend the `query.v1` risk-state schema with an owning-tenant field + a source-log-position, then regenerate the SDK). This is the dependency root: WIRE-02b's `governed.Client` needs the extended surface before it can feed the copilot authz gate + citation seed and retire `StubClient`.

---

## PATH TO PARITY

> The DONE board delivered the analytic surface of an Aladdin-class platform. What remains between that and production is the carried-forward integration work, split into a **buildable half** (WIRE-01/02 — composition-root wiring of delivered seams, no credentials) and a **credential-gated half** (external vendor SDKs, licensed datasets, real infra, human audit/UAT loops). Each subtask is 1–3 engineer-days; "done" includes tests, `go build/vet/gofmt` clean, arch test unchanged, and the carried-forward note on the relevant DONE epic retired.

### Sequencing — 6-month milestone map

_Dependency-ordered. The buildable tranches (M0–M2) need no credentials and come first; the rest gates on an external boundary landing, so it is sequenced by when that boundary typically becomes available, not by engineering size._

- **M0–M1 · WIRE-01 (live wiring). ✅ DONE.** The delivered feed/consumer/calibrator/filing seams now run in composition roots (a–f complete; see DONE board).
- **M1–M2 · WIRE-02 (query.v1 ownership extension).** Extend `query.v1` with the owning-tenant + source-log-position surface, regenerate, then wire the governed.Client and retire `StubClient` — unblocks the last PARITY-04b half. Buildable today (schema + regen), no creds.
- **M2–M3 · Vendor `Source` / external-SDK bindings.** Bind the delivered seams to real SDKs as creds land: Bloomberg/Refinitiv/ICE `Source`, anthropic (`-tags anthropic`), go-redis (`-tags redis`), quickfix `FIXSession`, `settlement.v1` `FailEncoder`. Credential-gated.
- **M3–M4 · Licensed dataset load + reconciliation.** Load ISDA SIMM / BCBS FRTB / NGFS + the live-FX feed into the delivered `*Inputs`/parameter seams and reconcile against worked examples. License-gated.
- **M4–M5 · Real-infra bring-up.** Stand up a live Redis + a real DR region, apply the migrations against `kanz-books`, and run the PITR/DR drills for real. Infra-gated.
- **M5–M6 · SDK publish + human loops.** PARITY-07b–d (publish + strip the `replace`), the third-party SOC 2 audit engagement, and first-client UAT execution. Ops/human-gated.

### WIRE-02 — query.v1 ownership extension (buildable; M1–M2)

_Closes the last carried-forward PARITY-04b half. Verified gap: `query.v1` RiskQueryService responses surface neither the portfolio's owning tenant (the `OwnerTenant` authz-gate input) nor a source-event-id (the citation seed), so a query.v1-only governed client would make the cross-tenant authz gate always pass. Buildable on this box (schema change + buf regen), no credentials._

- [ ] **WIRE-02a** Extend the `query.v1` risk-state schema with an owning-tenant field + a source-log-position (the LogPosition the state was folded from); regenerate the SDK · `kanz-schemas/proto/query/`, `kanz/gen`
- [ ] **WIRE-02b** Implement `governed.Client` over the extended `query.v1`, feeding the copilot deny-by-default authz gate (`OwnerTenant`) + the citation seed; retire `StubClient` at `cmd/copilot` behind the existing seam · `kanz/services/copilot/`

### PARITY-07 — SDK distribution (credential-gated)

_Retire the load-bearing `replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go` with published, versioned SDKs. The workflow + CI guard already exist (PARITY-07a); only the publish target needs infra credentials this box lacks. Not a run blocker — the `replace` builds and runs today._

- [ ] **PARITY-07b** Stand up the publish target: tagged internal `kanz-schemas-go` companion repo + a private GOPROXY, and a private index / GitHub Packages for the Py/TS packages · infra
- [ ] **PARITY-07c** Run the release pipeline → tagged `kanz-schemas-go` + Python/TS SDK artifacts · `kanz-schemas` CI
- [ ] **PARITY-07d** Pin the published versions, delete the `replace` directives in `kanz/go.mod` + the kanz-py/ts equivalents, and arm the dormant `KANZ_SDK_PUBLISHED` no-`replace` CI guard · `kanz/go.mod`, `kanz-py/pyproject.toml`

### Credential-/schema-gated carried-forward (not in an active tranche)

_Each sits behind a delivered seam; only the external boundary is missing._

- [ ] **PARITY-04b (governed.Client half)** — blocked on a real security gap, not skipped: `query.v1` surfaces neither the portfolio's owning tenant (the `OwnerTenant` authz-gate input) nor a source-event-id (the citation seed), so a query.v1-only client would make the cross-tenant authz gate always pass. Needs a query.v1 schema extension (ownership surface + source-log-position) before `StubClient` can be retired. (The LIN-01 lineage half is DONE.)
- [ ] **Composition-root vendor `Source`/binding wiring** where the SDK/creds exist (CI/prod): Bloomberg BLPAPI / Refinitiv RTSDK / ICE (`Source`); anthropic-sdk-go (`-tags anthropic`); go-redis (`-tags redis`, `redisadapter`); quickfix (`FIXSession`); the `settlement.v1` `FailEncoder` + DTCC/SWIFT `SettlementVenue`; the `platform.model` bus binding + real `ModelLoader`; the bus-backed `DecisionRecorder` producer at copilot/gateway.
- [ ] **Licensed datasets** for calibration + filing reconciliation: ISDA SIMM / BCBS FRTB / NGFS climate parameter tables, the live-FX feed, and the regulator worked-example datasets — loaded at the composition root against the delivered `*Inputs` structs.
- [ ] **Real infra + human loops**: a live Redis, a real DR region, applying the migrations against `kanz-books` + driving the PITR drill; the third-party SOC 2 audit engagement + first-client UAT execution.

---

## IN PROGRESS

_Nothing in flight._

---

## DONE

_Historical record — one line per completed epic. Full implementation detail is in git history; durable decisions are in `KANZ_BRAIN.md`._

### Phase 9 — Live wiring

- **WIRE-01** (a–f) — wired the delivered PARITY seams into running composition roots: `feed.BusSink` + `SyntheticSession` publish normalized market events onto the spine (`cmd/market-data`, `MARKET_DATA_FEED`); `cmd/accounting` folds live `order.v1` fill FACTs + emits/folds `accounting.v1` subscription/redemption/fee cash FACTs (`cashmove.Publisher` + the cash-movement endpoint) into the shared journal, and values multi-currency NAV off a live `fxfeed.LiveFX` cache with no `fx` in the request; `cmd/risk-engine` runs the `schedule.Scheduler` over `curve.Calibrator`, fed by a `livequote.LiveQuotes` cache off the market spine (deny-on-garbage preserved); a new `regulatory` service serves signed, completeness-gated FRTB/FormPF/AIFMD/TCFD/SFDR filings from request-supplied `*Inputs` via the promoted `signer.ChainSigner`. Each consumer folds the spine into its own last-value cache (a service cannot import another's `internal/`); `chain`/`signer` promoted to `internal/audit/` on the second-consumer trigger; `accounting.v1` generated into the local SDK. Vol reuses the generic scheduler once wired; credit lacks a `Refresh`/`QuoteSource` seam.

### Phase 8 — PATH TO PARITY (Aladdin-class floor)

- **SVCWIRE-01** (a–f) — operationalized the Phase-7 wealth/datamaster/copilot/alternatives services: GitOps deploy manifests, gateway routes behind edge auth + mTLS mesh backend, KEDA/PDB scaling, end-to-end contract test, proto CODEOWNERS.
- **PARITY-01** (a–i) — live market/reference/ESG data plane: vendor-agnostic feed adapter + validating normalizer, Bloomberg/Refinitiv/ICE + reference + ESG adapters, live DQ gate (drop-vs-report), curve/vol/credit snapshot surface, soak/chaos harness. _Carried-forward: live vendor `Source` bindings (bus-producer `Sink` wired by WIRE-01a)._
- **PARITY-02** (a–h) — durable, replayable state: Postgres behind every Store (IBOR/fund/book/datamaster/risk), bus consumers folding live FACTs, migrations + PITR, replay-determinism contract. _Carried-forward: live broker subjects + proto Decoder bindings (cash-movement producer + fold wired by WIRE-01b/f)._
- **PARITY-03** (a–i) — model calibration + independent validation: curve/vol(SVI)/credit(CDS) calibration, live factor model, alternatives/climate/regulatory calibration, deny-by-default validation gate (SR 11-7), backtesting/attribution. _Carried-forward: live quote/universe bindings + licensed parameter datasets (curve Refresh schedule wired by WIRE-01c; vol/credit reuse the generic scheduler once wired)._
- **PARITY-04** (a, c–i + 04b-lineage) — real external adapters behind delivered seams: Claude client (build-tag), FIX venue, settlement calendar, hash-chain Signer, bus DecisionRecorder, Redis PendingStore, model registry coordination, LineageCatalog, BusFailSink. _Carried-forward: 04b governed.Client (query.v1 schema change — WIRE-02) + external SDK bindings._
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
