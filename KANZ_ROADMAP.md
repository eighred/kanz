# KANZ 3-YEAR ROADMAP

> Strategic horizon — distinct from `KANZ_TASKS.md` (the focused execution board) and `KANZ_BRAIN.md` (architectural memory). This maps the path from *"runs end-to-end on synthetic data at the edges"* to *"production-grade institutional system serving live clients,"* then to sustained breadth.
>
> **Discipline (per `/forge-tasks`):** every item here is grounded in either (a) a verified in-repo gap, or (b) a boundary already documented in `KANZ_TASKS.md` PATH TO PARITY behind a delivered seam. Nothing here is a speculative feature. **Confidence decays over the horizon:** Year 1 is verifiable against the code today; Years 2–3 are boundary-gated themes — each must be re-scanned against ground truth and split into 1–3 day tasks *at the point its enabling boundary lands*, not before. The codebase is always the ground truth; when a roadmap line and the code disagree, the code wins and the line is discarded.
>
> Last reconciled 2026-07-04.

---

## Where the system actually is (baseline)

A real workflow executes start-to-finish today on in-repo infra: `./start.sh` boots the NATS/Kafka/Postgres spine + risk-engine + api-gateway, seeds a portfolio through the production bus, and serves live exposure/measures/scenario queries through the gateway. Accounting NAV/cash, regulatory signing, FX folding, and the copilot governed-read path are genuinely wired through composition roots. **The architecture, seams, and composition-root wiring are done and internally consistent.**

Three classes of work remain: (1) a small set of **verified non-gated in-repo gaps** — chiefly a running engine serving a placeholder VaR; (2) **binding the credential/infra/human boundaries** already seamed in PATH TO PARITY; (3) **breadth and operational maturity** once the platform is live.

---

## Year 1 — Close to production parity (highest confidence)

*Goal: one real client, in production, on live data, with zero placeholder analytics on the serving path and the load-bearing dev `replace` stripped.*

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

## Year 2 — Scale, resilience & operational maturity (boundary-gated themes)

*Goal: multi-client at institutional scale with proven HA/DR and closed latency SLOs. Each theme sits behind a PARITY-05 / PARITY-04 carried-forward seam; verify the seam against code before splitting into tasks.*

- **Live horizontal scale.** Wire the consistent-hash shard-ring member lists (PARITY-05 carried-forward) for real multi-instance risk compute; partitioned market-data hot path under production feed volume.
- **Shared-state cutover.** Promote the `-tags redis` PendingStore path from build-tag to the default serving path once live Redis is proven; validate the degrade-not-fabricate fallback under real outage.
- **Active DR, not just warm.** Promote the CloudNativePG warm standby toward active/active where the domain allows; run the quarterly DR drill as an operational cadence, not a one-off.
- **Close LATENCY-01** (the open `KANZ_BRAIN.md` question): pin the <50ms interactive-inference target to p50 vs p99 with measured evidence, then hold it as an SLO with error budgets.
- **Real DecisionRecorder + model coordination.** Wire the bus-backed `DecisionRecorder` producer at copilot/gateway and the `platform.model` bus binding + real `ModelLoader` / model-registry coordination (PARITY-04 carried-forward).
- **Complete the calibration schedule surface.** Vol reuses the generic scheduler once its quote source is wired; add the missing credit `Refresh`/`QuoteSource` seam (WIRE-01 note) so curve/vol/credit calibration all run live on schedule.

**Year 2 exit criteria:** N-client tenancy at target throughput; a real (not simulated) DR failover exercised; interactive latency SLO measured and met; no analytic on the serving path depends on a build-tag default.

---

## Year 3 — Breadth & institutional depth (directional — re-verify before committing)

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
