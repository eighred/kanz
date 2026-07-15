# RISK-M2b — Black-Litterman Posterior Returns

**Date:** 2026-07-15
**Status:** Approved design, pre-implementation
**Scope:** Add Black-Litterman as a **posterior expected-returns (μ) producer** in the optimizer, plus its service integration so a client can pass views and get a BL-optimized rebalance proposal end-to-end. BL is NOT a new objective — it produces a better `μ` that feeds the existing `MaxSharpe`/`MaxReturn` objectives. This completes RISK-M2 (RISK-M2a shipped HRP).

---

## 1. Problem & ground truth

The optimizer has a real solver (`MaxReturn`, `MinVariance`, `MaxSharpe`, `RiskParity`, and now `HRP`). Mean-variance/max-Sharpe need an expected-returns vector μ, and naïve historical μ is the least stable input in the whole pipeline — small changes swing the optimizer wildly. Black-Litterman is the standard fix: start from a **market-equilibrium prior** (the returns implied by market-cap weights) and tilt it by **investor views**, each with its own confidence. Verified absent across the whole Go tree (grep 2026-07-15): no Black-Litterman, no equilibrium reverse-optimization, no views representation.

### What already exists (reused unchanged)

- `Optimize(in MarketInputs, obj Objective, cons *ConstraintSet)` — BL feeds it: `μ_BL` goes into `MarketInputs.ExpectedReturns`, then `MaxSharpe`/`MaxReturn` runs unchanged.
- `solveLinear(A, b) ([]float64, bool)` in `solver.go` — Gaussian elimination with partial pivoting, `ok=false` on a singular matrix. This is the ONLY linear-algebra primitive BL needs (BL is a sequence of solves, not an explicit inverse).
- The HTTP `/v1/propose` handler (`services/optimization/internal/server/server.go`) decodes a `proposeRequest`, calls `Optimize`, then `Rebalance`. BL slots in as an optional pre-step that populates `ExpectedReturns` before `Optimize`.
- Note: `matVec(sigma, w)` in `solver.go` assumes a SQUARE matrix (`n := len(w)`), so BL's rectangular products (P is k×n) need small BL-local helpers — they are not a reuse candidate.

---

## 2. Design

### 2.1 The algorithm — the Idzorek / He-Litterman form

BL has two algebraically-equivalent forms. The **canonical** form inverts two n×n matrices (`(τΣ)⁻¹` and the posterior precision). The **Idzorek form** inverts only ONE **k×k** matrix (k = number of views, usually 1–5) and never inverts Σ — numerically preferable and a natural fit for `solveLinear`:

```
Π    = δ · Σ · w_mkt                                  equilibrium (reverse-optimized) returns, length n
M    = P · (τΣ) · Pᵀ + Ω                              k×k
y    = solveLinear(M, Q − P·Π)                        k-vector  (the one linear solve)
μ_BL = Π + (τΣ) · Pᵀ · y                              posterior returns, length n
```

- **Π (the prior)** = `δ Σ w_mkt`: the returns that make the market-cap portfolio `w_mkt` optimal under risk-aversion `δ` (reverse optimization). With **no views** (`k=0`), `μ_BL = Π` exactly — BL degenerates to the equilibrium prior.
- **Views**: `P` is the `k×n` picking matrix (row `i` is view `i`'s asset weights), `Q` is the `k` view returns. A row `[1,0,…]` with `q=0.05` is the absolute view "asset 0 returns 5%"; a row `[1,−1,0,…]` with `q=0.02` is the relative view "asset 0 beats asset 1 by 2%". General P-rows support both (an absolute-only friendlier DTO is a later refinement, not needed).
- **Ω (view uncertainty)**: a **per-view diagonal** — views are assumed independent (the near-universal case; a full correlated `k×k` Ω is a documented non-goal). Each view's variance is supplied, or defaulted **per view** to `(P·(τΣ)·Pᵀ)_ii` — the He-Litterman convention that makes each view's uncertainty proportional to the prior variance along that view, so the caller does not have to hand-tune Ω. `M` is then `P·τΣ·Pᵀ + diag(Ω)`.
- **τ**: the scalar weight on the prior's uncertainty (typically small, e.g. 0.025–0.05). Supplied.

Only **one k×k `solveLinear`** is performed; Σ is never inverted. A singular `M` (e.g. two identical views with zero Ω) ⇒ an error, never a fabricated μ.

### 2.2 Internal API (`internal/optimization/bl.go`)

```go
// BLInput carries the Black-Litterman prior + views. All matrices are row-major
// and aligned to the same n-asset universe as Covariance.
type BLInput struct {
	Covariance    [][]float64 // Σ, n×n (the same covariance the optimizer uses)
	MarketWeights []float64   // w_mkt, n — the equilibrium prior (market-cap weights)
	RiskAversion  float64     // δ > 0
	Tau           float64     // τ > 0
	P             [][]float64 // k×n view-picking matrix (k views, may be empty)
	Q             []float64   // k view returns
	Omega         []float64   // per-view variances (the diagonal of Ω); nil ⇒ all He-Litterman defaults; a 0 entry ⇒ the He-Litterman default for that view
}

// BlackLitterman returns the posterior expected-returns vector μ_BL (length n),
// ready to drop into MarketInputs.ExpectedReturns. k=0 (no views) ⇒ μ_BL = Π.
func BlackLitterman(in BLInput) ([]float64, error)
```

**Errors** (sentinel `var`s, matching the package's `ErrNeedCovariance` style): empty/zero universe; `δ ≤ 0` or `τ ≤ 0`; dimension mismatch (`w_mkt`/Σ, `P` columns vs n, `Q`/`P` rows, `Ω` vs k); a singular `M`. Never returns a partial or NaN μ.

**Local helpers** (small, BL-file-private; the square `matVec` cannot be reused): a general matrix×vector `A·x` (rows = len(A)), a matrix×matrix product, a transpose, and a scalar-scale. Kept in `bl.go`.

### 2.3 Service integration (`/v1/propose`)

Extend `proposeRequest` with an optional block:

```go
// blRequest is the optional Black-Litterman input on /v1/propose. Present ⇒ the
// handler computes μ_BL and uses it as ExpectedReturns before Optimize.
type blRequest struct {
	MarketWeights []float64   `json:"market_weights"`
	RiskAversion  float64     `json:"risk_aversion"`
	Tau           float64     `json:"tau"`
	Views         []viewDTO   `json:"views"`
}
type viewDTO struct {
	P     []float64 `json:"p"`     // length n — this view's asset weights
	Q     float64   `json:"q"`     // this view's return
	Omega float64   `json:"omega"` // this view's variance; 0 ⇒ He-Litterman default for this view
}
// proposeRequest gains:
//	BlackLitterman *blRequest `json:"black_litterman"`
```

Handler flow (in `handlePropose`, before building `MarketInputs`): if `req.BlackLitterman != nil`, assemble a `BLInput` (P from the view rows, Q from the view `q`s, Ω from the per-view `omega`s — a fully-zero Ω ⇒ pass `nil` so `BlackLitterman` applies the He-Litterman default; a partially-specified Ω is filled per-view: 0 entries take the default diagonal), call `BlackLitterman`, and set `in.ExpectedReturns = μ_BL`. On a BL error, return `400` with the error (same shape as the existing `Optimize` error path). Absent ⇒ the handler is byte-for-byte its current behavior. The handler sets `in.ExpectedReturns = μ_BL`, so the proposal's existing `ExpectedReturn` scalar (μᵀw) already reflects the BL-optimized portfolio — echoing the full posterior vector back is not required for this task (a client that wants it can request the objective without BL and diff, or it is a trivial later addition to the response shape).

BL is only meaningful for μ-using objectives (`MaxSharpe`/`MaxReturn`); if the request pairs BL with `MinVariance`/`RiskParity`/`HRP` (which ignore μ), `μ_BL` is computed and simply unused — harmless, documented, not an error (the client asked for it).

---

## 3. Error handling & edge cases

- `k=0` (no views): `μ_BL = Π = δΣw_mkt` (pure equilibrium) — a valid, useful result, not an error.
- Singular `M` (redundant zero-Ω views): `solveLinear` returns `ok=false` ⇒ `BlackLitterman` returns a sentinel error; the service surfaces `400`. Never a NaN μ.
- Ω default is per-view: a `viewDTO.Omega` of 0 takes `diag(P·τΣ·Pᵀ)_ii` for that view; a positive value is used as-is. If EVERY view omega is 0, `Omega` is passed `nil` and the whole default matrix is built once.
- `δ ≤ 0`, `τ ≤ 0`, dimension mismatches: sentinel errors, fail-loud (BL with a non-positive risk aversion or τ is a malformed request, not a degenerate-but-proceed case).
- Non-PSD Σ: BL inherits the module-wide PSD precondition (same as every objective; tracked as a module-wide follow-up from RISK-M2a) — a valid PSD Σ gives a well-posed `M`.

---

## 4. Testing

Deterministic unit tests in `internal/optimization` (`package optimization`, reusing `approx`/`diag`), plus a service test:

- **No-views identity**: `BlackLitterman` with empty `P` ⇒ `μ_BL = δΣw_mkt` exactly (assert against a hand-computed Π).
- **Single absolute view**: a 2-asset Σ, one view "asset 0 returns X" above its prior ⇒ `μ_BL[0]` moves toward X relative to `Π[0]` (assert direction, and the exact value against a hand-computed Idzorek result on a small matrix).
- **Relative long-short view** (`p=[1,−1]`): the spread `μ_BL[0]−μ_BL[1]` moves toward the view `q` (assert direction).
- **Ω default vs explicit**: a larger Ω (less confident view) pulls `μ_BL` LESS toward the view than a smaller Ω (assert monotonicity in confidence).
- **Singular M**: two identical zero-Ω views ⇒ error (not NaN).
- **Validation**: `δ≤0`, `τ≤0`, `len(w_mkt)≠n`, a P row of wrong length, `len(Q)≠k` ⇒ the matching sentinel errors.
- **End-to-end through `Optimize`**: `μ_BL` fed to `MaxSharpe` yields a tangency portfolio tilted toward a bullish view vs the no-view equilibrium (weights shift in the view's direction).
- **Service**: a `/v1/propose` request with a `black_litterman` block returns a proposal whose optimization used `μ_BL` (a bullish view on one asset raises its target weight vs the same request without the block); a malformed BL block ⇒ `400`.

`go build ./... && go vet ./... && go test ./internal/optimization/... ./services/optimization/...` clean; `test/arch` unchanged (no new service, no proto change — BL adds no `ObjectiveType`).

---

## 5. Scope boundary & non-goals

- **In:** the `BlackLitterman` function + helpers, its errors, and the `/v1/propose` `black_litterman` block wiring; tests at both layers.
- **Out (deliberate / YAGNI):** a full correlated `k×k` Ω (views are assumed independent — Ω is a per-view diagonal; correlated view errors are a rare, advanced case); a friendlier absolute-only views DTO (P-rows already cover absolute views); Idzorek's confidence-to-Ω calibration procedure (we accept explicit Ω or the He-Litterman proportional default — the round-trip "target this % tilt" calibration is a later refinement); any n×n matrix inversion (the Idzorek form avoids it); a new `ObjectiveType` (BL is a μ-producer, not an objective); any change to HRP or the other objectives; a proto change (BL is a service-request shape over HTTP/JSON, and — like the other propose fields — is not carried on the `optimization.v1` proto, which only models the objective enum).
- **Out (follow-up, from RISK-M2a):** the module-wide Σ-PSD validation pass and the zero-variance IVP guard — unchanged by this task, still tracked.

---

## 6. Files touched (estimate)

- `internal/optimization/bl.go` (new) — `BLInput`, `BlackLitterman`, the BL error sentinels, the local matrix helpers.
- `internal/optimization/bl_test.go` (new) — the BL unit tests.
- `services/optimization/internal/server/server.go` (modify) — the `blRequest`/`viewDTO` types, the `proposeRequest.BlackLitterman` field, and the pre-`Optimize` BL step in `handlePropose`.
- `services/optimization/internal/server/server_test.go` (modify/new) — the `/v1/propose` BL end-to-end test.

One coherent 1–3 day vertical: one posterior-returns producer + its service wiring, reusing `solveLinear` and the existing `Optimize`/`Rebalance` path — a new capability (equilibrium-anchored, view-tilted expected returns) fully usable end-to-end, with no new objective and no proto churn.
