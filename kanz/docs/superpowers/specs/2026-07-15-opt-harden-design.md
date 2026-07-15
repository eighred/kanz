# OPT-HARDEN — Optimizer Input Validation

**Date:** 2026-07-15
**Status:** Approved design, pre-implementation
**Scope:** Close the three input-validation gaps the HRP (RISK-M2a) and Black-Litterman (RISK-M2b) whole-branch reviews located precisely — a module-wide Σ-PSD precondition, an unbounded request body on the optimization service, and a backwards zero-variance guard in the HRP inverse-variance portfolio. A small, cohesive hardening pass on a capital-path service. No new capability, no proto change.

---

## 1. Problem & ground truth

The optimizer trusts its numeric inputs. Three reviews said so, each with a concrete failing input (verified against the code 2026-07-15):

1. **No Σ-PSD precondition, module-wide.** Every objective assumes the covariance is positive-semidefinite but none checks it. A non-PSD Σ silently produces garbage on a capital path: in HRP, `clusterVar = ivpᵀΣ_sub ivp` can go negative, making a bisection `α` exceed `[0,1]` and breaking the long-only guarantee; in Black-Litterman, the He-Litterman default `Ω_i = (PτΣPᵀ)_ii` can go negative, yielding an indefinite `M` that `solveLinear` "solves" into a finite-but-garbage μ. The concrete failing sub-matrix is `[[1,-2],[-2,1]]` — positive diagonal, symmetric, but indefinite (eigenvalues `3, -1`). Verified: `Optimize` (optimizer.go) validates only `square`; `BlackLitterman` (bl.go) validates only dimensions; there is **no PSD/Cholesky helper anywhere in `internal/optimization`**.
2. **`/v1/propose` has no request-body limit.** `decode` (`services/optimization/internal/server/server.go:216`) does `json.NewDecoder(r.Body).Decode(v)` with no bound, so attacker-controlled `k`/`n` in the JSON drive `O(n³)`/`O(k³)` solver work with the body itself unbounded. It is the single decode helper for BOTH `/v1/propose` and `/v1/orders`, so one fix covers both.
3. **The zero-variance guard in `inverseVariancePortfolio` (hrp.go:162) is directionally backwards.** A non-positive `Σ_ii` (a riskless asset) is the limit `1/Σ_ii → ∞`, so it should *dominate* the inverse-variance portfolio; the current code sets its contribution to `0` — *excluding* the safest asset. It bites a 3+-asset cluster containing an exactly-zero-variance member; latent (no NaN, no failing test), but wrong.

None is AI- or infra-gated. Together ≈ 1 engineer-day.

---

## 2. Design

### 2.1 Σ-PSD precondition — reject indefinite, allow singular

A new package-private `checkPSD(cov [][]float64) error` in `internal/optimization` and an exported sentinel `ErrNotPSD`. Called at the entry of `Optimize` (when a covariance is present) and `BlackLitterman` (Σ is always required there).

**The decision (approved): reject INDEFINITE, allow SINGULAR.** A covariance from real returns is PSD but may be *singular* (collinear assets — e.g. two perfectly correlated instruments give a zero eigenvalue), and the existing solvers already tolerate that. A strict positive-definite check (plain Cholesky) would reject those currently-working inputs — a regression. A "symmetric + non-negative diagonal" check is too weak — it passes the indefinite `[[1,-2],[-2,1]]`, the exact bug. So the check must reject only genuine **indefiniteness** (a negative eigenvalue) while passing singular PSD.

**Method: LDLᵀ pivot-sign inspection** (no `sqrt`, no eigensolver). For a symmetric matrix `A = LDLᵀ` (L unit-lower-triangular, D diagonal), `A` is PSD iff every pivot `D_j ≥ 0` AND every zero pivot is *consistent* (its column's off-diagonal numerators are also ≈ 0). The algorithm, with a relative tolerance `tol` scaled to the matrix (e.g. `tol = 1e-9 · (1 + maxAbsDiagonal)`):

1. **Symmetry:** require `|A_ij − A_ji| ≤ tol` for all `i<j` — a covariance is symmetric by construction; reject an asymmetric matrix (`ErrNotPSD`). (Tolerance, not exact equality, so a float round-trip is not rejected.)
2. **Pivots:** compute `D_j = A_jj − Σ_{k<j} L_jk² D_k`.
   - `D_j < −tol` ⇒ a negative eigenvalue ⇒ **indefinite** ⇒ `ErrNotPSD`.
   - `|D_j| ≤ tol` ⇒ a zero pivot (singular direction). For every `i>j`, the numerator `A_ij − Σ_{k<j} L_ik L_jk D_k` must be `≤ tol` in magnitude (a consistent PSD column); if not, the matrix is indefinite ⇒ `ErrNotPSD`. Set `L_ij = 0`.
   - else `L_ij = (A_ij − Σ_{k<j} L_ik L_jk D_k) / D_j`.
3. All pivots non-negative and consistent ⇒ PSD, return `nil`.

`O(n³)`, dominated anyway by the solve that follows. `checkPSD` never panics (all indexing is within the square matrix the caller already validated).

**Wiring:**
- `Optimize`: after the existing `square`/dimension checks, if `in.Covariance != nil`, `if err := checkPSD(in.Covariance); err != nil { return Result{}, err }`. This covers every objective that carries a Σ (MinVariance/MaxSharpe/RiskParity/HRP and MaxReturn-with-λ). MaxReturn without λ (no Σ) is unaffected.
- `BlackLitterman`: after the dimension checks, `if err := checkPSD(in.Covariance); err != nil { return nil, err }` — before any use of Σ.
- The service surfaces `ErrNotPSD` as a `400` via the existing `Optimize`/BL error paths (no handler change).

### 2.2 `/v1/propose` request-body limit

In `decode` (server.go), wrap the body before decoding:

```go
const maxRequestBytes = 8 << 20 // 8 MiB — generous for a several-hundred-asset covariance, bounds the O(n³)/O(k³) work an unbounded body could drive.

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}
```

`MaxBytesReader` makes the decoder error once the body exceeds the cap, so an oversized request is rejected with the existing `400` shape. Fixes both endpoints (both call `decode`). 8 MiB caps a dense covariance at roughly a few hundred assets in JSON — comfortably above any realistic institutional portfolio, well below an abuse vector. The limit is a named const, trivially tunable.

### 2.3 Zero-variance IVP guard — riskless assets dominate

Rewrite `inverseVariancePortfolio` so a non-positive-variance asset takes the `1/Σ_ii → ∞` limit (dominates) instead of being excluded:

```go
func inverseVariancePortfolio(cov [][]float64, items []int) []float64 {
	ivp := make([]float64, len(items))
	// A riskless asset (Σ_ii ≤ 0) is the limit 1/Σ_ii → ∞: it DOMINATES the
	// inverse-variance portfolio. If any exist, split weight equally among them and
	// give the risk-bearing assets zero. (All-riskless ⇒ equal weights, unchanged.)
	var riskless int
	for _, i := range items {
		if cov[i][i] <= 0 {
			riskless++
		}
	}
	if riskless > 0 {
		w := 1 / float64(riskless)
		for k, i := range items {
			if cov[i][i] <= 0 {
				ivp[k] = w
			}
		}
		return ivp
	}
	var sum float64
	for k, i := range items {
		ivp[k] = 1 / cov[i][i]
		sum += ivp[k]
	}
	for k := range ivp {
		ivp[k] /= sum
	}
	return ivp
}
```

- All-positive variances ⇒ the same normalized inverse-variance as before (no behavior change for the common case).
- Some riskless ⇒ they now dominate (equal split among them), instead of being zeroed.
- All riskless ⇒ equal weights (`riskless == len(items)` ⇒ `1/n` each) — identical to the old fallback, so the existing "all-zero Σ ⇒ equal weights" HRP test still holds.

The existing HRP zero-variance edge test only asserts finite/long-only/sum-to-1 (not exact weights), so this change does not regress it; a new test pins the "dominates" behavior.

---

## 3. Error handling & edge cases

- `checkPSD` on an empty matrix (`n=0`) ⇒ `nil` (vacuously PSD; the caller's `n==0` guard fires first anyway). On a 1×1 `[[v]]`: PSD iff `v ≥ −tol`.
- The PSD check runs only when a covariance is present; objectives/paths with no Σ are unaffected.
- `MaxBytesReader` over-limit ⇒ the decoder returns an error ⇒ the existing `400 "invalid request body"`. (A more specific "request too large" message is a nicety, out of scope — the 400 is correct.)
- The IVP change cannot produce a non-long-only or non-normalized vector: the riskless branch sums to 1 by construction; the positive branch is normalized as before.

---

## 4. Testing

- **`checkPSD`** (`internal/optimization`, package-internal): a PD matrix ⇒ `nil`; a singular-but-PSD matrix (two perfectly correlated assets, e.g. `[[1,1],[1,1]]`) ⇒ `nil` (the key non-regression); the indefinite `[[1,-2],[-2,1]]` ⇒ `ErrNotPSD`; an asymmetric matrix ⇒ `ErrNotPSD`; a matrix with a tiny negative eigenvalue from numerical noise (within `tol`) ⇒ `nil` (does not reject shrinkage-estimator round-trip noise); a clearly-indefinite diagonal-plus-large-off-diagonal ⇒ `ErrNotPSD`.
- **`Optimize` / `BlackLitterman` reject a non-PSD Σ** ⇒ `ErrNotPSD`, and still accept a valid PSD Σ (the existing tests stay green — they all use PD covariances).
- **Body limit** (`services/optimization/internal/server`): a `> 8 MiB` body ⇒ `400`; a normal request ⇒ unaffected (the existing propose/BL tests stay green).
- **IVP**: a cluster `{riskless, risky}` ⇒ the riskless asset gets ~all the weight (new assertion via a small HRP case or a direct `inverseVariancePortfolio` unit test); all-positive ⇒ unchanged normalized inverse-variance; all-riskless ⇒ equal weights.
- Regression: the full `internal/optimization` + `services/optimization` suites stay green; `test/arch` unchanged.

`go build ./... && go vet ./... && go test ./internal/optimization/... ./services/optimization/...` clean.

---

## 5. Scope boundary & non-goals

- **In:** `checkPSD` + `ErrNotPSD` and its wiring into `Optimize`/`BlackLitterman`; the `MaxBytesReader` body cap in `decode`; the `inverseVariancePortfolio` rewrite; tests for all three.
- **Out (deliberate / YAGNI):** an eigensolver or a "nearest-PSD" projection (we reject a bad Σ, we do not repair it — the caller owns producing a valid covariance); a per-endpoint or configurable body limit (one module const suffices); any change to the solvers themselves or to the objective set; a proto change; a "request too large" distinct error message.

---

## 6. Files touched (estimate)

- `internal/optimization/psd.go` (new) — `checkPSD` + `ErrNotPSD`.
- `internal/optimization/psd_test.go` (new) — `checkPSD` unit tests.
- `internal/optimization/optimizer.go` — call `checkPSD` in `Optimize`.
- `internal/optimization/bl.go` — call `checkPSD` in `BlackLitterman`.
- `internal/optimization/hrp.go` — rewrite `inverseVariancePortfolio`.
- `internal/optimization/optimizer_test.go` / `hrp_test.go` — the `Optimize`-rejects-non-PSD and IVP-dominance assertions.
- `services/optimization/internal/server/server.go` — the `MaxBytesReader` cap in `decode`.
- `services/optimization/internal/server/server_test.go` — the oversized-body test.

One coherent ≈1-day hardening pass: one validation helper wired at two entry points, one one-line body cap, one guard rewrite — all closing precisely-located, review-verified gaps on a capital-path service.
