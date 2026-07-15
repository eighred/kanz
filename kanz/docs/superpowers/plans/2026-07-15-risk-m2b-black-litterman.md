# RISK-M2b — Black-Litterman Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Black-Litterman posterior expected returns to the optimizer (a μ-producer feeding the existing MaxSharpe/MaxReturn objectives), wired end-to-end through `/v1/propose`.

**Architecture:** A pure `BlackLitterman(BLInput) ([]float64, error)` in `internal/optimization` (Idzorek form: equilibrium prior `δΣw_mkt` tilted by views through one k×k `solveLinear`, Σ never inverted), plus an optional `black_litterman` block on the HTTP `/v1/propose` handler that computes μ_BL and feeds it into the existing `Optimize` path.

**Tech Stack:** Go 1.26, standard-library `math` only. Reuses `solveLinear` (Gaussian elimination) and `square` from the `optimization` package. No new dependencies.

## Global Constraints

- **Idzorek form only** — one **k×k** `solveLinear` (k = number of views); Σ is **never inverted** and there is no n×n inversion.
- **Ω is a per-view diagonal** (`[]float64`, length k): views are assumed independent. A 0 entry ⇒ the He-Litterman default `(P·τΣ·Pᵀ)_ii` for that view; `nil`/empty ⇒ all-default. A full correlated k×k Ω is a non-goal.
- **k=0 (no views) ⇒ μ_BL = Π = δΣw_mkt exactly** (a valid result, not an error).
- **Fail-loud, never fabricate:** `δ ≤ 0`, `τ ≤ 0`, any dimension mismatch, or a singular `M` ⇒ a sentinel error; never a partial/NaN μ.
- **BL is a μ-producer, not an ObjectiveType** — no new objective, and **no proto change** (BL is an HTTP/JSON request shape, like the other `/v1/propose` fields).
- Every task ends green: `GOFLAGS=-mod=mod go build ./... && go vet ./... && go test ./internal/optimization/... ./services/optimization/...`. `GOFLAGS=-mod=mod` required. `test/arch` unchanged.

## File Structure

- `internal/optimization/bl.go` (new) — `BLInput`, `BlackLitterman`, error sentinels, local matrix helpers (`blMatVec`, `scaleMatrix`, `matMul`, `matMulT`).
- `internal/optimization/bl_test.go` (new, `package optimization`) — BL unit tests.
- `services/optimization/internal/server/server.go` (modify) — `blRequest`/`viewDTO`, `proposeRequest.BlackLitterman`, the pre-`Optimize` BL step in `handlePropose`, and a `blMu` helper.
- `services/optimization/internal/server/server_test.go` (modify) — the `/v1/propose` BL end-to-end test.

---

### Task 1: The `BlackLitterman` μ-producer

**Files:**
- Create: `internal/optimization/bl.go`
- Create: `internal/optimization/bl_test.go`

**Interfaces:**
- Produces (consumed by Task 2): `type BLInput struct{...}`; `func BlackLitterman(in BLInput) ([]float64, error)`; error sentinels `ErrBLDims`, `ErrBLRiskAversion`, `ErrBLTau`, `ErrBLSingular`.

- [ ] **Step 1: Write the failing tests**

Create `internal/optimization/bl_test.go`:

```go
package optimization

import (
	"math"
	"testing"
)

// No views ⇒ posterior is the equilibrium prior Π = δ·Σ·w_mkt. δ=2.5,
// Σ=diag(0.04,0.04), w=[0.6,0.4] ⇒ Π = 2.5·[0.024,0.016] = [0.06,0.04].
func TestBlackLitterman_NoViewsIsPrior(t *testing.T) {
	mu, err := BlackLitterman(BLInput{
		Covariance:    diag(0.04, 0.04),
		MarketWeights: []float64{0.6, 0.4},
		RiskAversion:  2.5,
		Tau:           0.05,
	})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "mu0", mu[0], 0.06, 1e-12)
	approx(t, "mu1", mu[1], 0.04, 1e-12)
}

// Single absolute view, n=1, hand-computed Idzorek result. Σ=[[0.04]], w=[1],
// δ=2.5 ⇒ Π=0.10. View "asset returns 0.15", τ=0.05, Ω defaulted:
// τΣ=0.002, PτΣPᵀ=0.002, Ω=0.002, M=0.004, y=(0.15−0.10)/0.004=12.5,
// μ = 0.10 + 0.002·12.5 = 0.125 (moved halfway from prior 0.10 toward view 0.15).
func TestBlackLitterman_SingleAbsoluteView(t *testing.T) {
	mu, err := BlackLitterman(BLInput{
		Covariance:    [][]float64{{0.04}},
		MarketWeights: []float64{1},
		RiskAversion:  2.5,
		Tau:           0.05,
		P:             [][]float64{{1}},
		Q:             []float64{0.15},
	})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "posterior", mu[0], 0.125, 1e-12)
}

// Confidence monotonicity: a smaller Ω (more confident view) pulls μ closer to
// the view than the default, a larger Ω pulls it less.
func TestBlackLitterman_OmegaConfidence(t *testing.T) {
	base := BLInput{
		Covariance: [][]float64{{0.04}}, MarketWeights: []float64{1},
		RiskAversion: 2.5, Tau: 0.05, P: [][]float64{{1}}, Q: []float64{0.15},
	}
	confident := base
	confident.Omega = []float64{0.001}
	def := base // Ω=nil ⇒ default 0.002
	unconfident := base
	unconfident.Omega = []float64{0.008}

	mc, _ := BlackLitterman(confident)
	md, _ := BlackLitterman(def)
	mu, _ := BlackLitterman(unconfident)
	if !(mc[0] > md[0] && md[0] > mu[0]) {
		t.Fatalf("more confidence ⇒ closer to the 0.15 view: confident=%.5f default=%.5f unconfident=%.5f", mc[0], md[0], mu[0])
	}
}

// Relative long-short view p=[1,-1]: the spread μ0−μ1 moves toward the view q.
func TestBlackLitterman_RelativeView(t *testing.T) {
	prior, _ := BlackLitterman(BLInput{
		Covariance: diag(0.04, 0.04), MarketWeights: []float64{0.5, 0.5},
		RiskAversion: 2.5, Tau: 0.05,
	})
	post, err := BlackLitterman(BLInput{
		Covariance: diag(0.04, 0.04), MarketWeights: []float64{0.5, 0.5},
		RiskAversion: 2.5, Tau: 0.05,
		P: [][]float64{{1, -1}}, Q: []float64{0.10}, // "asset0 beats asset1 by 10%"
	})
	if err != nil {
		t.Fatal(err)
	}
	priorSpread := prior[0] - prior[1] // 0 (equal prior)
	postSpread := post[0] - post[1]
	if !(postSpread > priorSpread) {
		t.Fatalf("relative view should widen the spread toward q: prior=%.5f post=%.5f", priorSpread, postSpread)
	}
}

// Two identical views with explicit zero Ω ⇒ singular M ⇒ error, not NaN.
func TestBlackLitterman_SingularErrors(t *testing.T) {
	_, err := BlackLitterman(BLInput{
		Covariance: diag(0.04, 0.04), MarketWeights: []float64{0.5, 0.5},
		RiskAversion: 2.5, Tau: 0.05,
		P:     [][]float64{{1, 0}, {1, 0}},
		Q:     []float64{0.05, 0.05},
		Omega: []float64{1e-18, 1e-18}, // ~0 ⇒ M rank-deficient
	})
	if err != ErrBLSingular {
		t.Fatalf("want ErrBLSingular, got %v", err)
	}
}

func TestBlackLitterman_Validation(t *testing.T) {
	ok := BLInput{Covariance: diag(0.04, 0.04), MarketWeights: []float64{0.5, 0.5}, RiskAversion: 2.5, Tau: 0.05}
	neg := ok
	neg.RiskAversion = 0
	if _, err := BlackLitterman(neg); err != ErrBLRiskAversion {
		t.Fatalf("δ≤0 ⇒ ErrBLRiskAversion, got %v", err)
	}
	badTau := ok
	badTau.Tau = 0
	if _, err := BlackLitterman(badTau); err != ErrBLTau {
		t.Fatalf("τ≤0 ⇒ ErrBLTau, got %v", err)
	}
	badW := ok
	badW.MarketWeights = []float64{1}
	if _, err := BlackLitterman(badW); err != ErrBLDims {
		t.Fatalf("len(w)≠n ⇒ ErrBLDims, got %v", err)
	}
	badP := ok
	badP.P = [][]float64{{1}} // row length 1 ≠ n=2
	badP.Q = []float64{0.05}
	if _, err := BlackLitterman(badP); err != ErrBLDims {
		t.Fatalf("bad P row ⇒ ErrBLDims, got %v", err)
	}
	badQ := ok
	badQ.P = [][]float64{{1, 0}}
	badQ.Q = []float64{0.05, 0.06} // len(Q)=2 ≠ k=1
	if _, err := BlackLitterman(badQ); err != ErrBLDims {
		t.Fatalf("len(Q)≠k ⇒ ErrBLDims, got %v", err)
	}
}

// End-to-end: μ_BL fed to MaxSharpe tilts the tangency portfolio toward a bullish
// view vs the no-view equilibrium. Σ=diag(0.04,0.04), w=[0.5,0.5], view "A=0.20"
// ⇒ μ_BL=[0.125,0.05] ⇒ MaxSharpe ∝ Σ⁻¹μ ⇒ w_A≈0.714 > 0.5.
func TestBlackLitterman_FeedsMaxSharpe(t *testing.T) {
	mu, err := BlackLitterman(BLInput{
		Covariance: diag(0.04, 0.04), MarketWeights: []float64{0.5, 0.5},
		RiskAversion: 2.5, Tau: 0.05,
		P: [][]float64{{1, 0}}, Q: []float64{0.20},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := MarketInputs{Instruments: []string{"A", "B"}, ExpectedReturns: mu, Covariance: diag(0.04, 0.04)}
	res, err := Optimize(in, Objective{Type: MaxSharpe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Weights["A"] <= 0.5 {
		t.Fatalf("bullish view on A should tilt MaxSharpe toward A (>0.5), got %.4f", res.Weights["A"])
	}
	if math.Abs(mu[0]-0.125) > 1e-12 {
		t.Fatalf("sanity: μ_A should be 0.125, got %v", mu[0])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run TestBlackLitterman -v`
Expected: FAIL — `undefined: BlackLitterman` / `undefined: BLInput`.

- [ ] **Step 3: Write the implementation**

Create `internal/optimization/bl.go`:

```go
package optimization

import "errors"

// Black-Litterman posterior expected returns (Idzorek / He-Litterman form). BL
// starts from the market-equilibrium prior Π = δΣw_mkt (the returns that make the
// market-cap portfolio optimal under risk-aversion δ) and tilts it by investor
// views. It is a μ-PRODUCER: the result drops into MarketInputs.ExpectedReturns
// and the existing MaxSharpe/MaxReturn objectives run unchanged.
//
// The Idzorek form inverts only the k×k view system (k = number of views), never
// Σ and never an n×n matrix:
//
//	Π    = δ·Σ·w_mkt
//	M    = P·(τΣ)·Pᵀ + diag(Ω)
//	y    = M⁻¹·(Q − P·Π)                 (one solveLinear)
//	μ_BL = Π + (τΣ)·Pᵀ·y

var (
	// ErrBLDims is returned when the BL input dimensions are inconsistent.
	ErrBLDims = errors.New("optimization: black-litterman input dimensions do not match")
	// ErrBLRiskAversion is returned when δ ≤ 0.
	ErrBLRiskAversion = errors.New("optimization: black-litterman risk aversion must be positive")
	// ErrBLTau is returned when τ ≤ 0.
	ErrBLTau = errors.New("optimization: black-litterman tau must be positive")
	// ErrBLSingular is returned when the k×k view system M is singular.
	ErrBLSingular = errors.New("optimization: black-litterman posterior system is singular")
)

// BLInput carries the Black-Litterman prior + views. Matrices are row-major and
// aligned to the same n-asset universe as Covariance.
type BLInput struct {
	Covariance    [][]float64 // Σ, n×n
	MarketWeights []float64   // w_mkt, n — the equilibrium prior (market-cap weights)
	RiskAversion  float64     // δ > 0
	Tau           float64     // τ > 0
	P             [][]float64 // k×n view-picking matrix (k may be 0)
	Q             []float64   // k view returns
	Omega         []float64   // per-view variances (diagonal Ω); nil ⇒ all He-Litterman defaults; a 0 entry ⇒ the default for that view
}

// BlackLitterman returns the posterior expected-returns vector μ_BL (length n).
// k=0 (no views) ⇒ μ_BL = Π. Never returns a partial or NaN μ.
func BlackLitterman(in BLInput) ([]float64, error) {
	n := len(in.Covariance)
	if n == 0 {
		return nil, ErrNoUniverse
	}
	if !square(in.Covariance, n) || len(in.MarketWeights) != n {
		return nil, ErrBLDims
	}
	if in.RiskAversion <= 0 {
		return nil, ErrBLRiskAversion
	}
	if in.Tau <= 0 {
		return nil, ErrBLTau
	}

	// Equilibrium prior Π = δ Σ w_mkt.
	pi := blMatVec(in.Covariance, in.MarketWeights)
	for i := range pi {
		pi[i] *= in.RiskAversion
	}

	k := len(in.P)
	if k == 0 {
		return pi, nil // no views ⇒ posterior is the prior
	}
	if len(in.Q) != k {
		return nil, ErrBLDims
	}
	for _, row := range in.P {
		if len(row) != n {
			return nil, ErrBLDims
		}
	}
	if len(in.Omega) != 0 && len(in.Omega) != k {
		return nil, ErrBLDims
	}

	tauSigma := scaleMatrix(in.Covariance, in.Tau) // τΣ  (n×n)
	tsPt := matMulT(tauSigma, in.P)                // τΣ·Pᵀ  (n×k)
	pTsPt := matMul(in.P, tsPt)                    // P·τΣ·Pᵀ  (k×k)

	// M = P·τΣ·Pᵀ + diag(Ω), with per-view He-Litterman defaults where Ω_i is 0.
	m := make([][]float64, k)
	for i := 0; i < k; i++ {
		m[i] = append([]float64(nil), pTsPt[i]...)
		om := pTsPt[i][i] // He-Litterman default for view i
		if len(in.Omega) == k && in.Omega[i] > 0 {
			om = in.Omega[i]
		}
		m[i][i] += om
	}

	// rhs = Q − P·Π
	pPi := blMatVec(in.P, pi)
	rhs := make([]float64, k)
	for i := 0; i < k; i++ {
		rhs[i] = in.Q[i] - pPi[i]
	}

	y, ok := solveLinear(m, rhs)
	if !ok {
		return nil, ErrBLSingular
	}

	// μ_BL = Π + τΣ·Pᵀ·y
	adj := blMatVec(tsPt, y)
	mu := make([]float64, n)
	for i := 0; i < n; i++ {
		mu[i] = pi[i] + adj[i]
	}
	return mu, nil
}

// blMatVec computes A·x for a possibly-rectangular A (rows = len(A), cols =
// len(x)). The package's matVec assumes a square matrix, so BL needs its own.
func blMatVec(a [][]float64, x []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		var s float64
		row := a[i]
		for j := range x {
			s += row[j] * x[j]
		}
		out[i] = s
	}
	return out
}

// scaleMatrix returns c·A as a new matrix.
func scaleMatrix(a [][]float64, c float64) [][]float64 {
	out := make([][]float64, len(a))
	for i := range a {
		out[i] = make([]float64, len(a[i]))
		for j := range a[i] {
			out[i][j] = c * a[i][j]
		}
	}
	return out
}

// matMul returns A·B for A (p×q) and B (q×r) ⇒ p×r.
func matMul(a, b [][]float64) [][]float64 {
	p := len(a)
	if p == 0 || len(b) == 0 {
		return nil
	}
	q := len(b)
	r := len(b[0])
	out := make([][]float64, p)
	for i := 0; i < p; i++ {
		out[i] = make([]float64, r)
		for j := 0; j < r; j++ {
			var s float64
			for t := 0; t < q; t++ {
				s += a[i][t] * b[t][j]
			}
			out[i][j] = s
		}
	}
	return out
}

// matMulT returns A·Bᵀ for A (p×q) and B (r×q) ⇒ p×r. Used for τΣ·Pᵀ where P is
// k×n: matMulT(τΣ, P) = τΣ·Pᵀ (n×k).
func matMulT(a, b [][]float64) [][]float64 {
	p := len(a)
	r := len(b)
	out := make([][]float64, p)
	for i := 0; i < p; i++ {
		out[i] = make([]float64, r)
		for j := 0; j < r; j++ {
			var s float64
			row := b[j]
			for t := range row {
				s += a[i][t] * row[t]
			}
			out[i][j] = s
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run TestBlackLitterman -v`
Expected: PASS (no-views prior [0.06,0.04], single-view 0.125, confidence monotonicity, relative-view spread, singular error, validation, feeds-MaxSharpe w_A≈0.714).

- [ ] **Step 5: Full package regression**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
cd /c/Users/root/Desktop/eighred-kanz
git add kanz/internal/optimization/bl.go kanz/internal/optimization/bl_test.go
git commit -m "feat(opt): Black-Litterman posterior returns (Idzorek form) (RISK-M2b)"
```

---

### Task 2: Wire Black-Litterman into `/v1/propose`

**Files:**
- Modify: `services/optimization/internal/server/server.go`
- Modify: `services/optimization/internal/server/server_test.go`

**Interfaces:**
- Consumes: `optimization.BlackLitterman`, `optimization.BLInput` (Task 1).

- [ ] **Step 1: Write the failing test**

Append to `services/optimization/internal/server/server_test.go`:

```go
func TestServer_Propose_BlackLitterman(t *testing.T) {
	// MaxSharpe (Type 2) over two equal-vol uncorrelated assets, equal market
	// weights, with a bullish absolute view on A (q=0.20). μ_BL tilts to A, so the
	// tangency target for A exceeds the 0.5 equilibrium.
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":2},
		"black_litterman":{"market_weights":[0.5,0.5],"risk_aversion":2.5,"tau":0.05,
			"views":[{"p":[1,0],"q":0.20,"omega":0}]},
		"current_weights":{"A":0.5,"B":0.5},"nav":100000,"prices":{"A":10,"B":10}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("propose+BL: got %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets map[string]float64
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Targets["A"] <= 0.5 {
		t.Fatalf("bullish BL view on A should raise A's target above 0.5, got %.4f", resp.Targets["A"])
	}
}

func TestServer_Propose_BlackLittermanInvalid(t *testing.T) {
	// A malformed BL block (risk_aversion 0) ⇒ 400 from the BL error path.
	body := `{"portfolio_id":"PF","instruments":["A","B"],
		"covariance":[[0.04,0],[0,0.04]],
		"objective":{"Type":2},
		"black_litterman":{"market_weights":[0.5,0.5],"risk_aversion":0,"tau":0.05,
			"views":[{"p":[1,0],"q":0.20,"omega":0}]},
		"current_weights":{"A":0.5,"B":0.5},"nav":100000,"prices":{"A":10,"B":10}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed BL ⇒ 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./services/optimization/internal/server/ -run TestServer_Propose_BlackLitterman -v`
Expected: FAIL — the `black_litterman` field is ignored (no such field on `proposeRequest`), so the bullish view has no effect and `Targets["A"]` stays ~0.5 (first test fails); the invalid test also fails (no BL path ⇒ MaxSharpe with no μ ⇒ its own error, not necessarily the asserted 400 for the BL reason). (Either way the tests are red until the field + handler step exist.)

- [ ] **Step 3: Add the request types and the BL step**

In `services/optimization/internal/server/server.go`, add the BL request types (near `proposeRequest`):

```go
// blRequest is the optional Black-Litterman input on /v1/propose. Present ⇒ the
// handler computes the posterior μ and uses it as ExpectedReturns before Optimize.
type blRequest struct {
	MarketWeights []float64 `json:"market_weights"`
	RiskAversion  float64   `json:"risk_aversion"`
	Tau           float64   `json:"tau"`
	Views         []viewDTO `json:"views"`
}

// viewDTO is one Black-Litterman view: a picking row p (length n), its return q,
// and its variance omega (0 ⇒ the He-Litterman default for this view).
type viewDTO struct {
	P     []float64 `json:"p"`
	Q     float64   `json:"q"`
	Omega float64   `json:"omega"`
}
```

Add the field to `proposeRequest` (after `Covariance`):

```go
	BlackLitterman  *blRequest                  `json:"black_litterman"`
```

In `handlePropose`, replace the `in := ...` / `Optimize` opening with the BL pre-step:

```go
	in := optimization.MarketInputs{Instruments: req.Instruments, ExpectedReturns: req.ExpectedReturns, Covariance: req.Covariance}
	if req.BlackLitterman != nil {
		mu, err := blMu(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		in.ExpectedReturns = mu
	}
	res, err := optimization.Optimize(in, req.Objective, req.Constraints)
```

Add the `blMu` helper (near the other helpers at the bottom of the file):

```go
// blMu assembles a BLInput from the request's black_litterman block and returns
// the posterior expected returns. A view omega of 0 stays 0 in the diagonal we
// pass, which BlackLitterman reads as "use the He-Litterman default for this view".
func blMu(req proposeRequest) ([]float64, error) {
	bl := req.BlackLitterman
	k := len(bl.Views)
	p := make([][]float64, k)
	q := make([]float64, k)
	omega := make([]float64, k)
	for i, v := range bl.Views {
		p[i] = v.P
		q[i] = v.Q
		omega[i] = v.Omega
	}
	return optimization.BlackLitterman(optimization.BLInput{
		Covariance:    req.Covariance,
		MarketWeights: bl.MarketWeights,
		RiskAversion:  bl.RiskAversion,
		Tau:           bl.Tau,
		P:             p,
		Q:             q,
		Omega:         omega,
	})
}
```

(Passing an all-zero `omega` slice is equivalent to all He-Litterman defaults — `BlackLitterman` substitutes the default wherever `Omega[i]` is not `> 0`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./services/optimization/internal/server/ -run TestServer_Propose -v`
Expected: PASS (the bullish-view test raises A's target above 0.5; the malformed-BL test returns 400; the existing `TestServer_Propose` still passes).

- [ ] **Step 5: Full verification**

Run:
```bash
cd /c/Users/root/Desktop/eighred-kanz/kanz
GOFLAGS=-mod=mod go build ./... && \
GOFLAGS=-mod=mod go vet ./internal/optimization/... ./services/optimization/... && \
GOFLAGS=-mod=mod go test ./internal/optimization/... ./services/optimization/... && \
GOFLAGS=-mod=mod go test ./test/arch/ && \
GOFLAGS=-mod=mod gofmt -l internal/optimization/ services/optimization/
```
Expected: build clean, vet clean, all tests PASS, `test/arch` `ok` (unchanged), `gofmt -l` lists nothing.

- [ ] **Step 6: Commit**

```bash
cd /c/Users/root/Desktop/eighred-kanz
git add kanz/services/optimization/internal/server/server.go kanz/services/optimization/internal/server/server_test.go
git commit -m "feat(opt): /v1/propose black_litterman block — BL-optimized proposals end-to-end (RISK-M2b)"
```

---

## Post-implementation

After both tasks: update `KANZ_TASKS.md` (RISK-M2b → DONE; RISK-M2 epic complete — HRP + BL both shipped; the module-wide Σ-PSD validation + zero-variance IVP guard remain as the carried optimizer-hardening follow-ups). No `KANZ_BRAIN.md` entry is expected unless a durable decision emerges. Board hygiene, done in the wrap-up, not a plan task.
