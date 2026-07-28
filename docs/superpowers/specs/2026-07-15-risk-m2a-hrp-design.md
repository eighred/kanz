# RISK-M2a — Hierarchical Risk Parity (HRP) Allocator

**Date:** 2026-07-15
**Status:** Approved design, pre-implementation
**Scope:** Add Hierarchical Risk Parity (HRP, López de Prado 2016) as a new objective in the portfolio optimizer — a standalone allocator that produces long-only weights from the covariance matrix alone, with no expected-return estimates. **Black-Litterman is explicitly deferred to RISK-M2b** (it is a different production boundary — a μ-producer that feeds the existing QP solver, not an allocator).

---

## 1. Problem & ground truth

The optimizer (`internal/optimization`) ships a genuine solver — `MaxReturn` (LP), `MinVariance` / `MaxReturn`-with-λ (QP), `MaxSharpe` (tangency), `RiskParity` (equal-risk-contribution). Verified absent across the whole Go tree (grep 2026-07-15): **Black-Litterman and HRP**. HRP is the allocator a desk reaches for when it distrusts return estimates and wants diversification that is robust to an ill-conditioned covariance matrix — Markowitz's mean-variance solution famously concentrates and inverts unstable covariances; HRP does neither because it never inverts Σ.

### What already exists (reused unchanged)

- `Optimize(in MarketInputs, obj Objective, cons *ConstraintSet) (Result, error)` in `internal/optimization/optimizer.go` — the single entry point; a `switch obj.Type` dispatches to a solver, then packages `Result{Weights, ExpectedReturn, ExpectedRisk}`.
- `MarketInputs{Instruments []string, ExpectedReturns []float64, Covariance [][]float64}` — HRP uses **only `Covariance`**.
- `ObjectiveType` (`int`, iota): `MaxReturn=0, MinVariance=1, MaxSharpe=2, RiskParity=3`. Documented to "Mirror `optimization.v1.ObjectiveType`".
- The solver helpers in `solver.go`: `matVec`, `quadForm`, `dot`, `equalSeed`, `maxAbsDiff`. `riskParity(sigma, lo, hi)` is HRP's closest sibling and the code pattern to mirror (a pure `[][]float64 → []float64` function).
- The HTTP service (`services/optimization/internal/server/server.go`) decodes the **internal** `optimization.Objective` from JSON (`{"objective":{"type":N}}`) — it does NOT route through the proto enum. So a new internal objective is reachable through `/v1/propose` with **no service change**.

### Two facts that scope the proto work down

- **Nothing in Go consumes `optimization.v1.ObjectiveType`** (grep: the only `ObjectiveType` referenced in non-generated Go is the internal type in `optimizer.go`). The proto enum is a documentation mirror, not a consumed contract.
- **No test guards the proto↔internal mirror.** So adding HRP is functionally an internal-only change; the proto enum value is added purely to keep the documented mirror honest (repo culture values cross-language consistency), at the cost of a one-line `.proto` edit and nothing else.

---

## 2. Design

### 2.1 The HRP algorithm (López de Prado)

A pure function `hrp(cov [][]float64) []float64` returning long-only weights that sum to 1, using only Σ:

1. **Correlation & distance.** From Σ, `ρ_ij = Σ_ij / √(Σ_ii · Σ_jj)` (guard `Σ_ii ≤ 0` → treat as isolated/zero-correlation). Distance `d_ij = √(½ · (1 − ρ_ij))`, clamped to `[0, 1]` (ρ∈[-1,1] ⇒ d∈[0,1]).
2. **Hierarchical clustering.** Agglomerative clustering on `d` with **single linkage** (nearest-point), implemented in Go (no scipy) — repeatedly merge the two closest clusters, recording the merge tree. Single linkage is López de Prado's default and is the simplest correct choice; the linkage tree is what drives the ordering.
3. **Quasi-diagonalization.** Walk the merge tree to produce a leaf ordering that places correlated assets adjacent (recursively replace each merged node by its two children until only original assets remain). This is the "quasi-diagonal" reordering.
4. **Recursive bisection.** Starting from the full ordered list, split it in half; for each half compute its **cluster variance** the López de Prado way — form the inverse-variance portfolio over the sub-cluster's diagonal, `ivp_i = (1/Σ_ii) / Σ_j(1/Σ_jj)` for `i,j` in the cluster, then take the **full quadratic form over the sub-covariance**, `V_cluster = ivpᵀ · Σ_sub · ivp` (Σ_sub is the covariance restricted to the cluster's assets — NOT just the diagonal; the off-diagonal terms are what make the split account for intra-cluster correlation). Allocate the split factor `α = 1 − V_left/(V_left+V_right)` to the left sub-list and `1−α` to the right (lower-variance side gets more); recurse into each half. The leaf weights, reassembled in the original instrument order, are the HRP weights.

Properties that hold by construction: **all weights ≥ 0** (products of convex splits of inverse variances) and **Σw = 1**. Σ is **never inverted** (the numerical robustness that is HRP's reason to exist).

**Edge cases (all yield a valid weight vector, never NaN/panic — the optimizer's honest-degradation stance):**
- `n == 1` ⇒ weight `{1.0}`.
- A zero/non-positive diagonal variance `Σ_ii`: guard the `1/Σ_ii` in the inverse-variance step (treat `1/Σ_ii` as 0 when `Σ_ii ≤ 0`) so there is never a division by zero or NaN. A zero-variance singleton's cluster variance is then 0, which the bisection *favors* (α→1 on its split — a riskless asset attracts weight); the guarantee here is finiteness and a valid long-only, sum-to-1 vector, NOT a prescribed weight direction.
- If every variance in a cluster is non-positive, the inverse-variance portfolio for that cluster falls back to equal weights.
- A fully degenerate Σ (all variances 0): every cluster variance is 0, so every split is 50/50 (α=0.5); the weights then follow the bisection tree — equal for a balanced/power-of-two universe, and always finite, long-only, and summing to 1.

### 2.2 Integration into `Optimize`

- Add `HRP` to the `ObjectiveType` iota (value `4`, after `RiskParity`).
- In `Optimize`'s `switch`, add `case HRP: w = hrp(in.Covariance)`.
- Validation: HRP needs Σ, not μ. Extend `needsCov` so it is true for `HRP` (it already is — `needsCov := obj.Type != MaxReturn || obj.RiskAversion > 0` is true for HRP since HRP ≠ MaxReturn), and `needsReturns` stays false for HRP. So an HRP call with no covariance returns `ErrNeedCovariance`; with no μ it proceeds. `Result.ExpectedReturn = μᵀw` only when μ is supplied (else 0); `Result.ExpectedRisk = √(wᵀΣw)` as for every Σ-bearing objective.
- **Constraints (the approved decision): box bounds are NOT applied to HRP.** Unlike `riskParity`, which ends with `projectBudgetBox(w, lo, hi)`, `hrp` returns its natural allocation directly — HRP is long-only and fully-invested by construction, and projecting onto `[lo,hi]` would silently distort the risk allocation the caller asked for (a clamped-and-renormalized vector is no longer HRP). The `case HRP` therefore does **not** compute or pass `lo/hi`. This is a deliberate asymmetry with `RiskParity`, documented in the `hrp` doc comment and the `ObjectiveType` const comment: **HRP does not honor per-asset box/group bounds; mandate compliance is still validated downstream by the existing `CheckMandate`** (a book you cannot hold is rejected there, not silently reshaped). A caller needing box-bounded risk allocation uses `RiskParity`.

### 2.3 Proto mirror

Add to `kanz-schemas/proto/optimization/v1/optimization.proto`, after `OBJECTIVE_TYPE_RISK_PARITY = 4`:

```proto
  // HRP: hierarchical risk parity — clustering-based allocation from the
  // covariance alone (no expected returns), robust to an ill-conditioned Σ
  // because it never inverts it. Long-only, fully-invested; does not honor
  // per-asset box bounds (mandate compliance is validated downstream).
  OBJECTIVE_TYPE_HRP = 5;
```

The generated SDK (`kanz-schemas/gen/go`) is generated-not-committed (EVT-15a) and nothing in Go consumes this enum, so no regeneration or downstream change is required for the build — the edit exists to keep the internal↔proto mirror honest. Note the numbering offset (internal iota `HRP=4` ↔ proto `OBJECTIVE_TYPE_HRP=5`, because the proto reserves 0 for `UNSPECIFIED`), matching the existing `RiskParity=3 ↔ RISK_PARITY=4` pattern.

---

## 3. Error handling & edge cases

- Empty universe ⇒ `ErrNoUniverse` (existing). Missing Σ ⇒ `ErrNeedCovariance` (existing, now covers HRP). Dimension mismatch ⇒ `ErrInputsMismatch` (existing).
- All numeric edge cases in §2.1 return a valid long-only weight vector; HRP never returns an error of its own (the objective-level validation in `Optimize` is the only error path), consistent with the other solvers being pure `→ []float64`.
- No panic on a singular/degenerate Σ (HRP never inverts Σ, so there is no singular-matrix path to begin with — the robustness that motivates it).

---

## 4. Testing

Deterministic unit tests in `internal/optimization` (package-internal where the unexported `hrp` is tested directly; `_test` package for the `Optimize` integration), mirroring the existing solver tests:

- **`hrp` reference case**: a small hand-traceable universe (e.g. 4 assets in two correlated pairs) where the clustering, ordering, and bisection can be computed by hand or against a published HRP worked example — assert the weight vector to a tolerance.
- **Invariants** (property-style over a few covariance matrices): all weights ≥ 0, Σw = 1 within 1e-9.
- **Diversification property**: on a covariance where one asset has much lower variance, min-variance concentrates heavily in it while HRP spreads materially more — assert `max(hrp weight) < max(min-variance weight)` on that input (the concrete statement of "HRP diversifies where mean-variance concentrates").
- **Edge cases**: `n==1 ⇒ {1.0}`; one zero-variance asset ⇒ finite weights, no NaN, still long-only and summing to 1; all-zero-variance Σ on a 4-asset universe ⇒ equal `0.25` weights (balanced tree, every split 50/50); a 2-asset case ⇒ the closed-form inverse-variance split (assert exact — e.g. Σ=diag(0.01,0.04) ⇒ weights [0.8, 0.2]).
- **`Optimize` integration**: `case HRP` returns weights keyed by instrument summing to 1, `ExpectedRisk=√(wᵀΣw)`, `ExpectedReturn=0` when μ nil and `μᵀw` when μ present; `HRP` with nil covariance ⇒ `ErrNeedCovariance`; a supplied `ConstraintSet` with non-default box bounds is **ignored** by HRP (assert the weights equal the unconstrained HRP weights, pinning the approved "bounds not applied" decision).
- Existing solver/`Optimize` tests stay green (HRP is additive; no existing path changes).

`go build ./... && go vet ./... && go test ./internal/optimization/... ./services/optimization/...` clean; `test/arch` unchanged (no new service, no manifest; the proto edit adds no consumed Go symbol).

---

## 5. Scope boundary & non-goals

- **In:** `hrp` allocator + helpers, `HRP` `ObjectiveType` + `case HRP`, the `OBJECTIVE_TYPE_HRP` proto value, tests.
- **Out (RISK-M2b, separate boundary):** Black-Litterman — a μ-producer with a views API (P/Q/Ω/τ) and an equilibrium prior; it feeds the existing QP objectives, it is not an allocator.
- **Out (deliberate / YAGNI):** box- or group-constrained HRP (the approved decision is pure HRP; constrained HRP is a research topic, not this task); alternative linkages (single linkage is the default — average/ward are a later refinement only if a desk asks); any change to the HTTP service, the bridge, or the rebalance/orders path (HRP flows through all of them unchanged because it is just another `ObjectiveType`); regenerating or committing the proto SDK (generated-not-committed, unconsumed).

---

## 6. Files touched (estimate)

- `internal/optimization/hrp.go` (new) — `hrp` + its clustering/quasi-diagonal/bisection helpers.
- `internal/optimization/hrp_test.go` (new) — the `hrp` unit tests.
- `internal/optimization/optimizer.go` — add `HRP` to the `ObjectiveType` const block (with the bounds-not-applied comment) + `case HRP` in `Optimize`.
- `internal/optimization/optimizer_test.go` (or the existing optimize test file) — the `Optimize`/`case HRP` integration tests.
- `kanz-schemas/proto/optimization/v1/optimization.proto` — the `OBJECTIVE_TYPE_HRP = 5` value.

One coherent 1–3 day vertical: one new allocator, one objective case, one proto value, tests — reusing every existing seam, service, and the rebalance/orders path unchanged.
