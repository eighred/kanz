# RISK-M1 — Tail & Concentration Risk Measures

**Date:** 2026-07-15
**Status:** Approved design, pre-implementation
**Scope:** Add the tail and concentration risk measures a desk asks for immediately after VaR — **CVaR/Expected Shortfall, Max Drawdown, and concentration (HHI)** — to the risk engine. Black-Litterman and HRP are portfolio *construction*, a different production boundary (`internal/optimization`), and are explicitly **deferred to RISK-M2**.

---

## 1. Problem & ground truth

The risk engine ships real, calibrated VaR — historical simulation + Monte-Carlo, with a Kupiec/Christoffersen backtest, liquidity-adjusted VaR, factor models and XVA. What is verifiably **absent from the entire Go tree** (confirmed by grep 2026-07-15; the only hits are incidental — `specVar` = specific variance, "drawdown" in scenario comments):

- **No CVaR / Expected Shortfall** — the mean loss in the tail beyond VaR.
- **No drawdown** — peak-to-trough decline.
- **No concentration measure** — no Herfindahl-Hirschman Index (HHI).

These are the first questions asked after "what is my VaR?". No AI, no external vendor, no new infrastructure is required — the data and the seams already exist.

### What already exists (and that we reuse unchanged)

- **The RISK-07 measure Registry** (`internal/risk/compute/measures.go`): a measure is a pure `MeasureFunc func(*domain.Portfolio) v1.Measure`, keyed by `v1.MeasureName`. Adding a measure is one `Register` call. `MeasureName` is a plain `string` type (`internal/risk/api/v1/engine.go`), **not a proto enum** — so a new measure needs no `.proto` change and no api/v1 contract change.
- **The returns injection seam** (`internal/risk/compute/marketdata.go`): `ReturnsMeasure func(ctx, *domain.Portfolio, ReturnsProvider) v1.Measure` is a provider-aware measure; `BindReturns(ctx, provider, ReturnsMeasure) MeasureFunc` closes it over the provider so the Registry stores a plain `MeasureFunc`. `ReturnsProvider.Returns(ctx, instrumentID, asOf, window)` reads the price history point-in-time-correct as of `p.AsOf()` (no future leakage).
- **The historical-sim VaR** (`internal/risk/compute/var` — package `varmodel`): `Historical` pulls each position's return series, forms per-scenario `P&L_t = Σ_i value_i × return_i`, sorts, and reports `−quantile(pnl, 1−α)` floored at zero. This is the **deployed** VaR: `risk-engine/main.go:161` calls `varmodel.Register`, which registers `Historical`, whenever a market-data price store is configured; otherwise VaR stays the RISK-07 `1%×gross` placeholder.
- **The shared registry**: `risk-engine/main.go:140-161` builds one registry, shared by both the async recomputer (publish path) and the query `EngineImpl`. Registering a measure once surfaces it on **both** paths with zero caller changes.

---

## 2. Design

Four measures, split by data dependency, which decides where each registers.

### 2.1 Measure set

| Name (`MeasureName`) | Definition | Units | Data | Answers |
|---|---|---|---|---|
| `ES99` | `−mean(P&L at or beyond the 99% VaR quantile)` on the empirical P&L distribution | money, base-currency cents (like `VaR99`) | returns provider | tail severity *beyond* VaR |
| `MaxDrawdown` | worst peak-to-trough decline of the cumulative P&L path over the window, ÷ peak value | fraction 0–1 | returns provider | **relative** severity / benchmark comparison |
| `MaxDrawdownAmount` | same path, absolute peak-to-trough loss | money, base-currency cents | returns provider | **capital / margin** reality |
| `HHI` | `Σ wᵢ²` where `wᵢ` = position's share of gross exposure | dimensionless 0–1 | positions only | concentration |

**Confidence:** ES is at **99%**, pairing with the deployed `VaR99` on the same tail — `ES99 ≥ VaR99` by construction, and the two read as a directly-comparable pair on a dashboard. (Basel FRTB's 97.5% ES was considered and rejected for this task: it sits at a different confidence than VaR99, so the two would not be a same-tail pair. FRTB-aligned ES is a later, separate addition if regulatory capital reporting needs it.)

**Why two drawdown measures, not one.** A drawdown is read at two layers that answer different questions, and collapsing them loses information:

- **Percentage** (`MaxDrawdown`) is the *performance metric* — how severe the decline was relative to history and peers. A 20% market drop against a 12% portfolio drop is a strategy that mitigated relative risk. Scale-free, comparable across portfolios of any size.
- **Money** (`MaxDrawdownAmount`) is the *budget reality* — the absolute base-currency loss that drives margin-call math and capital allocation. Size-dependent, and the number a risk officer acts on.

(A third "points/pips" layer — the raw structural move of an underlying index — is an *instrument/index* metric, not a portfolio-level measure; it is out of scope for this task and would be a separate per-instrument analytic.)

Both drawdown measures fall out of **one walk** of the cumulative path, so shipping both costs one extra scan, not a second data pull.

### 2.2 The consistency refactor (the one non-trivial internal change)

`varmodel.Historical` currently builds the per-scenario P&L distribution **inline**. ES and drawdown need that same construction, and ES in particular must be computed from the **identical** P&L distribution as VaR — otherwise VaR and ES can silently disagree (VaR reports a loss the tail-mean ES then contradicts, which is impossible and signals a bug).

Extract one private helper in `varmodel`:

```
// portfolioPnL builds the time-ordered per-scenario P&L series for the
// portfolio's base-currency positions over the window, tail-aligned to the
// shortest available series. Returns (nil, false) on insufficient data.
func portfolioPnL(ctx, p *domain.Portfolio, rp ReturnsProvider, window int) (pnl []float64, ok bool)
```

- It returns the P&L series **in time order** (the `pnl[t] = Σ_i value_i × return_i` step, before any sort).
- `Historical` (VaR) sorts a copy and takes the quantile — unchanged behaviour, now reading from the helper.
- `ExpectedShortfall` sorts a copy ascending and averages the worst tail: `ES₉₉ = −mean(sorted[:k])` with `k = ⌈n·(1−α)⌉` (the empirical ES estimator — the mean of the `⌈n·(1−α)⌉` worst scenarios; e.g. `n=250, α=0.99 ⇒ k=3`). `k` is floored at 1 so a small window still yields the single worst loss rather than an empty mean.
- `MaxDrawdown` / `MaxDrawdownAmount` walk the series **in time order**: `V₀ = Σ_i value_i` (current base-currency portfolio value), `V_t = V₀ + Σ_{s≤t} pnl_s`; track the running peak and, at each step, update `maxAmount = max(maxAmount, peak − V_t)` and `maxFraction = max(maxFraction, (peak − V_t)/peak)`. One pass yields both. **The two maxima are computed independently and may fall at different points** when the path has multiple peaks of different heights — this is correct, not a bug: the worst *percentage* decline (from a lower peak) and the worst *dollar* decline (from a higher peak) are distinct questions and can have distinct answers. There is therefore **no fixed `Amount = Fraction × peak` relationship** across the two in general.

Each measure remains an independent `ReturnsMeasure` (honours the Registry contract); they merely share the construction so they cannot drift. Cost per measure is one provider pull + one O(n log n) sort (VaR/ES) or O(n) walk (drawdown).

### 2.3 Registration & wiring

- **`HHI`** needs no provider, so it is a pure `MeasureFunc` in `internal/risk/compute` (a new `concentration.go`, or added to `measures.go`), registered **unconditionally** in `compute.DefaultRegistry()` alongside `GrossExposure`. It is therefore available even in the no-market-data fallback — concentration is computable from positions alone.
- **`ES99`, `MaxDrawdown`, `MaxDrawdownAmount`** need the returns provider, so they register at the **same gated site** as historical VaR. Extend `varmodel.Register` (`risk-engine/main.go:161`) so the single call that registers historical VaR also registers the three provider-backed tail measures over the same provider, ctx and config. No new composition-root branch — they light up exactly when real VaR does, and stay dark (not placeholdered) when there is no price store.
- No change to `services/risk-engine/internal/app/features.go` (`modelInputs = {GrossExposure, VaR99}`): the AI feature vector uses a fixed measure subset; new measures are additive and do not perturb it. Wiring a tail measure into the feature vector is a separate, deliberate decision, not part of this task.

### 2.4 HHI definition detail

`wᵢ = |MarketValueᵢ| / GrossExposure`, over positions in the portfolio base currency (the same same-currency convention every baseline measure uses; other-currency positions are skipped, an FX layer is out of scope). `HHI = Σ wᵢ²`. Bounds: `1/n ≤ HHI ≤ 1`. `HHI = 1` ⇒ a single-position book (maximally concentrated); `HHI → 1/n` ⇒ n equal positions (maximally diversified). Empty / zero-gross portfolio ⇒ zero-value measure (the "never error" contract).

---

## 3. Error handling & edge cases

- **Insufficient history / empty portfolio**: every measure returns the zero-value `Measure`, never an error — the `MeasureFunc` contract. The response layer marks it degraded (RISK-11), exactly as historical VaR already does.
- **No market-data store**: `ES99` and the drawdowns are simply not registered (not placeholdered) — an honest absence, consistent with how VaR degrades to the `1%×gross` placeholder. `HHI` is still served (positions-only).
- **Non-loss tail** (a window with no losing scenarios): `ES99` and drawdowns floor at zero, matching `Historical`'s floor.
- **Names are a one-way door**: `ES99`, `MaxDrawdown`, `MaxDrawdownAmount`, `HHI` are chosen once. A later rename breaks every dashboard/alert bound to them — same discipline as `event_type` names.

---

## 4. Testing

Table-driven unit tests, deterministic (no real broker/DB — a fake `ReturnsProvider`, exactly like the existing `varmodel`/`compute` tests):

- **`ES99`**: against a hand-computed distribution, assert `ES99 = −mean(tail)` and the invariant **`ES99 ≥ VaR99`** on the *same* sample (the core consistency property). A degenerate all-gains window ⇒ 0.
- **`MaxDrawdown` / `MaxDrawdownAmount`**: a hand-constructed return path with a known single peak-to-trough — assert both the fraction and the money value against hand-computed numbers. A monotonically rising path ⇒ both 0. Plus a **two-peak path** (a larger % decline from a low peak, a larger $ decline from a higher peak) asserting the two maxima **diverge** — pinning the independent-maxima design decision so a later "simplification" that couples them fails the build.
- **`HHI`**: single position ⇒ `1`; `n` equal positions ⇒ `1/n`; a known mixed book ⇒ the hand-computed `Σ wᵢ²`. Empty ⇒ 0.
- **Consistency test**: `ES99 ≥ VaR99` driven from the *shared* `portfolioPnL` output, proving the two cannot drift.
- **Regression**: the existing `Historical` / VaR tests (including `TestHistorical_DecouplesFromGrossPlaceholder` and the RISK-12 property tests) stay green through the refactor — the extraction must be behaviour-preserving for VaR.

`go build ./... && go vet ./... && go test ./internal/risk/... ./services/risk-engine/...` clean; the `test/arch` suite unchanged (no new service, no manifest, no contract change).

---

## 5. Scope boundary & non-goals

- **In:** `ES99`, `MaxDrawdown`, `MaxDrawdownAmount`, `HHI`; the `portfolioPnL` extraction in `varmodel`; registration wiring; tests.
- **Out (RISK-M2, separate boundary):** Black-Litterman and HRP — these produce portfolio *weights* in `internal/optimization` / `services/optimization`, not risk *measures*, and flow through a different service and data path.
- **Out (separate/YAGNI):** an MC variant of ES/drawdown (only add if MC VaR is ever the deployed model — the deployed VaR is historical-sim); FRTB 97.5% ES; FX/cross-currency aggregation; feeding tail measures into the AI feature vector; a per-instrument "points/pips" structural metric.

---

## 6. Files touched (estimate)

- `internal/risk/compute/var/*.go` — extract `portfolioPnL`; add `ExpectedShortfall`, `MaxDrawdown`/`MaxDrawdownAmount` `ReturnsMeasure`s; extend `Register`; new measure-name consts.
- `internal/risk/compute/measures.go` (or new `concentration.go`) — `HHI` `MeasureFunc` + name const; register in `DefaultRegistry`.
- `services/risk-engine/cmd/risk-engine/main.go` — no new branch expected (the extended `varmodel.Register` carries it); confirm the log line names the newly-registered measures.
- Test files alongside each.

One coherent 1–3 day vertical: four measures, one behaviour-preserving internal refactor, reusing every existing seam and contract.
