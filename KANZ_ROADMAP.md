# KANZ 3-YEAR ROADMAP

> Strategic horizon — distinct from `KANZ_TASKS.md` (the focused execution board) and `KANZ_BRAIN.md` (architectural memory). This maps the path from *"runs end-to-end on synthetic data at the edges"* to *"production-grade institutional system serving live clients,"* then to sustained breadth.
>
> **Discipline (per `/forge-tasks`):** every item here is grounded in either (a) a verified in-repo gap, or (b) a boundary already documented in `KANZ_TASKS.md` PATH TO PARITY behind a delivered seam. Nothing here is a speculative feature. **Confidence decays over the horizon:** Year 1 is verifiable against the code today; Years 2–3 are boundary-gated themes — each must be re-scanned against ground truth and split into 1–3 day tasks *at the point its enabling boundary lands*, not before. The codebase is always the ground truth; when a roadmap line and the code disagree, the code wins and the line is discarded.
>
> Last reconciled 2026-07-11.

---

## Strategic repositioning (2026-07)

Kanz was repositioned from an analytics/risk platform into an **Automated Fund-Management & Multi-Exchange Execution System** (see `KANZ_BRAIN.md` → "Strategic direction"). The delivered analytics/Aladdin-class platform is retained as the **valuation/risk substrate**; the client surface is now the automated trading loop: TradingView Pine strategies → HMAC webhook → advisory signal → venue-allocated order fan-out → simultaneous Binance/OKX execution → bitemporal projection → TradingView Broker-API feedback.

This roadmap now has **two tracks**. The **execution track** (below, Year 1) is the active direction. The **substrate track** (the original analytics Year 1–3, further down) is retained but re-prioritized *behind* the execution track — its credential/infra/human boundaries still gate it, and it continues to underpin pre-trade risk and post-trade valuation for the trading loop.

**Delivered on the execution track (as of 2026-07-11):** the signal contract + `webhook-ingest` (M1); `tv-sync` bitemporal projection + Broker-API (M2); the `execution.Venue` seam, Binance connector, and multi-venue allocation matrix (M3–M4); OKX operational parity — user-data WS + REST reconciliation + testnet gate (M4b); the In-Flight Certainty healing seam + production IP/anti-decompilation hardening (M4c); and the native-alpha edge foundation — `market-ingest` in-memory L2 book + bounded snapshots + signal provenance (M5a). See `KANZ_TASKS.md` DONE → Phase 11.

---

## Where the substrate is (baseline)

A real analytics workflow executes start-to-finish today on in-repo infra: `./start.sh` boots the NATS/Kafka/Postgres spine + risk-engine + api-gateway, seeds a portfolio through the production bus, and serves live exposure/measures/scenario queries through the gateway. Accounting NAV/cash, regulatory signing, FX folding, and the copilot governed-read path are genuinely wired through composition roots. **The architecture, seams, and composition-root wiring are done and internally consistent** — and now double as the pre-trade risk / post-trade valuation substrate under the execution loop.

Three classes of substrate work remain: (1) a small set of **verified non-gated in-repo gaps** — chiefly a running engine serving a placeholder VaR; (2) **binding the credential/infra/human boundaries** already seamed in PATH TO PARITY; (3) **breadth and operational maturity** once the platform is live.

---

## Year 1 (execution track — active direction) — Complete the loop to live trading

*Goal: the automated loop runs one fund's capital on live venue connections — signals executing across Binance + OKX with reconciled, never-frozen ledgers, native alpha feeding the same command path, under the production security perimeter.*

### H1 — Operational parity + live venue bring-up

- **Binance healing-seam parity.** Port the In-Flight Certainty watchdog (M4c) to the Binance reconciler via the shared, untagged `CloseIntent`/`PendingCloses` seam — a trivial follow-on, no new seam. Then wire the OMS venue-close dispatch path to `Track` closes so the registry has a live writer end to end.
- **Live venue depth `DepthSource`s (EXEC-M5c).** Binance/OKX L2-depth websockets behind the existing per-venue build tags, replacing the deterministic sim on the live path; the fold/snapshot pipeline is unchanged. *(exchange creds-gated; testnet first)*
- **Testnet → production key rotation.** Exercise `TEST_BINANCE_TESTNET` / `TEST_OKX_TESTNET` signed round-trips, then bind production keys via mock-Vault `*_FILE` mounts locked to the Tokyo/London egress IPs (the `ErrEgressDenied` boundary is already in place). *(creds/infra-gated)*

### H2 — Native alpha + capital lifecycle

- **Native-alpha emit port (EXEC-M5b).** The `market-ingest` book read-seam + native-signal emit port (`source = NATIVE_ENGINE` onto `strategy.signal.received`). **The OBI/arbitrage math itself stays in the separate restricted alpha layer** (house rule) — build only the boundary here.
- **Broker-API feedback hardening.** Drive `tv-sync`'s Broker-API (positions/orders/executions + streaming) against the real TradingView broker integration once the account is provisioned. *(vendor-gated)*
- **Leverage/derivatives readiness.** The `MarginMode`/leverage fields are schema-present but Phase-1 spot-only; promote to margin execution only after spot is proven live and the liquidation path has its own gate.

**Year 1 (execution) exit criteria:** one fund trading live across two venues; every fill reconciled from venue truth; no ledger freeze under a stuck close (healing verified); native signals flowing through the unchanged OMS path; production keys memory-only and egress-locked; the shipped binary stripped.

---

## Year 1 (substrate track) — Close analytics to production parity (highest confidence)

*Retained but re-prioritized behind the execution track. Goal: zero placeholder analytics on the serving path and the load-bearing dev `replace` stripped — the substrate that underpins the trading loop's pre-trade risk and post-trade valuation.*

### H1 — Retire the verified in-repo gaps, then bind the first boundaries

- **Verified buildable tranche (no credentials) — do first.** `RISK-12` (register real historical VaR at the composition root; retire the 1%×gross placeholder), `REG-02` (durable `LinkSink` for the regulatory ChainSigner), `WIRE-03` (surface the durable-log fold position into `query.v1 source_position`). All three sit behind delivered, tested seams — see `KANZ_TASKS.md` TODO. **This is the only tranche fully executable on this box today.**
- **Monte-Carlo VaR alongside historical.** `MODEL-01e` is anticipated in `var/historical.go`; once `RISK-12` lands the provider seam, add the Monte-Carlo model over the same `compute.BindReturns` path (deterministic given seed + inputs, for replay).

### H2 — Bind the credential/infra/human boundaries (PATH TO PARITY M2–M6)

Sequenced by when each boundary typically becomes available, not by engineering size. Each is *bind the concrete adapter at the composition root* — **the seam already exists; do not rebuild it.**

- **Vendor `Source` / external-SDK bindings** as creds land: Bloomberg/Refinitiv/ICE market `Source`; anthropic (`-tags anthropic`) for the copilot LLM; go-redis (`-tags redis`); quickfix `FIXSession`; `settlement.v1` `FailEncoder`. *(credential-gated)*
- **Licensed dataset load + reconciliation:** ISDA SIMM / BCBS FRTB / NGFS parameter tables + live-FX feed loaded into the delivered `*Inputs` seams; reconcile against regulator worked examples. *(license-gated)*
- **Real-infra bring-up:** live Redis, a real DR region, migrations applied against `kanz-books`, PITR/DR drills run for real. *(infra-gated)*
- **SDK publish + strip the `replace`:** `PARITY-07b–d` — publish target, release pipeline, pin versions and delete the `replace github.com/kanz-eng/kanz-schemas-go` directive (arms the dormant no-`replace` CI guard). *(infra-gated)*
- **First-client + certification human loops:** third-party SOC 2 Type II engagement + first-client UAT execution. *(ops/human-gated)*

**Year 1 exit criteria:** live vendor feed replaces `SimAdapter`; VaR served is data-driven, not placeholder; SDKs published + `replace` stripped; one tenant live in production under a signed SOC 2 engagement.

---

## Year 2 (substrate track) — Scale, resilience & operational maturity (boundary-gated themes)

*Goal: multi-client at institutional scale with proven HA/DR and closed latency SLOs. Each theme sits behind a PARITY-05 / PARITY-04 carried-forward seam; verify the seam against code before splitting into tasks.*

- **Live horizontal scale.** Wire the consistent-hash shard-ring member lists (PARITY-05 carried-forward) for real multi-instance risk compute; partitioned market-data hot path under production feed volume.
- **Shared-state cutover.** Promote the `-tags redis` PendingStore path from build-tag to the default serving path once live Redis is proven; validate the degrade-not-fabricate fallback under real outage.
- **Active DR, not just warm.** Promote the CloudNativePG warm standby toward active/active where the domain allows; run the quarterly DR drill as an operational cadence, not a one-off.
- **Close LATENCY-01** (the open `KANZ_BRAIN.md` question): pin the <50ms interactive-inference target to p50 vs p99 with measured evidence, then hold it as an SLO with error budgets.
- **Real DecisionRecorder + model coordination.** Wire the bus-backed `DecisionRecorder` producer at copilot/gateway and the `platform.model` bus binding + real `ModelLoader` / model-registry coordination (PARITY-04 carried-forward).
- **Complete the calibration schedule surface.** Vol reuses the generic scheduler once its quote source is wired; add the missing credit `Refresh`/`QuoteSource` seam (WIRE-01 note) so curve/vol/credit calibration all run live on schedule.

**Year 2 exit criteria:** N-client tenancy at target throughput; a real (not simulated) DR failover exercised; interactive latency SLO measured and met; no analytic on the serving path depends on a build-tag default.

---

## Year 3 (substrate track) — Breadth & institutional depth (directional — re-verify before committing)

*Goal: sustained expansion of asset-class and regulatory coverage on the proven core, with self-service onboarding and continuous model governance. Lowest confidence — these become concrete only after Year 2 ground-truth re-scan; list them as direction, not commitment.*

- **First-class schemas for the reused domains.** Nine proto packages (alternatives, collateral, factor, master, regulatory, settlement, sustainability, wealth, xva) currently reuse the generic envelope/command/domain types rather than same-named schemas. As each domain's requirements mature, promote it to a first-class `vN` schema via the dual-write payload-migration path — **only where a concrete requirement justifies the break**, never speculatively.
- **Self-serve tenant onboarding at scale.** Extend the existing onboarding automation (PARITY-06) from assisted to self-service, holding the deny-by-default multi-tenancy isolation contract.
- **Continuous model governance.** Champion/challenger automation over the SR 11-7 validation gate: shadow/canary already exist (PRED); add scheduled re-validation, drift-triggered re-calibration, and automated attribution/backtesting reporting.
- **Additional regulatory regimes** as client jurisdictions demand, reusing the signed-filing + `*Inputs` seam (add regimes as data + a filing type, not new infrastructure).
- **Cost & efficiency hardening** once scale is real: capacity model refresh, tiered-retention tuning, and inference cost controls — driven by measured production signal, not anticipated.

**Year 3 exit criteria:** new asset classes and regulatory regimes ship as data + schema extensions against the unchanged core; onboarding is self-service; model re-validation is continuous and automated.

---

## Operating notes

- **Every entry retires before it expands.** A carried-forward seam is bound at the composition root; the corresponding DONE-epic carried-forward note is retired. No parallel systems, no rebuilt seams.
- **Boundary-gated ≠ scheduled.** Years 2–3 dates are dependency positions, not calendar commitments — they move with when creds/infra/clients actually land.
- **Promotion rule.** A roadmap line becomes a `KANZ_TASKS.md` task only after a ground-truth verification confirms the gap still exists and is now buildable. If verification disproves it, discard it.
