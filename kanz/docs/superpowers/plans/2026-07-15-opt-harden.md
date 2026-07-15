# OPT-HARDEN — Optimizer Input Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three input-validation gaps in the optimizer — a module-wide Σ-PSD precondition, an unbounded `/v1/propose` body, and a backwards zero-variance guard in the HRP inverse-variance portfolio.

**Architecture:** A new `checkPSD` helper (LDLᵀ pivot-sign: reject indefinite, allow singular) wired into `Optimize` and `BlackLitterman`; an `http.MaxBytesReader` cap in the shared `decode`; and a rewrite of `inverseVariancePortfolio` so riskless assets dominate. Three independent, small changes.

**Tech Stack:** Go 1.26, standard-library `math`/`net/http` only. No new dependencies.

## Global Constraints

- **PSD check: reject INDEFINITE, allow SINGULAR.** A real covariance may be singular (collinear assets) and the existing solvers tolerate it — the check must pass singular PSD and reject only a genuine negative eigenvalue. A relative tolerance `tol = 1e-9·(1 + maxAbsDiagonal)` so numerical round-trip noise is not rejected.
- **`ErrNotPSD` is an exported sentinel**; the service surfaces it as `400` via the existing error paths (no handler change for the PSD part).
- **The IVP rewrite must not regress the existing HRP tests:** all-positive variances ⇒ the same normalized inverse-variance; all-riskless ⇒ equal weights (unchanged); only the mixed case changes (riskless now dominates).
- **Body cap: a single module const `maxRequestBytes = 8 << 20` (8 MiB)** applied in the shared `decode` (covers `/v1/propose` and `/v1/orders`).
- Every task ends green: `GOFLAGS=-mod=mod go build ./... && go vet ./... && go test ./internal/optimization/... ./services/optimization/...`. `GOFLAGS=-mod=mod` required. `test/arch` unchanged.
- **Verified precondition:** all existing test covariances (the `diag(...)` matrices, the block-diagonal 2-cluster, and the `0.02`-cross diversification matrix — LDLᵀ pivots `0.04, 0.0076, 0.1495, 0.0303`, all positive) are PSD, so `checkPSD` does not reject them. If wiring `checkPSD` breaks an existing test, the matrix is genuinely non-PSD — fix that test's matrix, do not loosen `checkPSD`.

## File Structure

- `internal/optimization/psd.go` (new) — `ErrNotPSD`, `checkPSD`.
- `internal/optimization/psd_test.go` (new, `package optimization`) — `checkPSD` unit tests.
- `internal/optimization/optimizer.go` (modify) — call `checkPSD` in `Optimize`.
- `internal/optimization/optimizer_test.go` (modify) — `Optimize`-rejects-non-PSD test.
- `internal/optimization/bl.go` (modify) — call `checkPSD` in `BlackLitterman`.
- `internal/optimization/bl_test.go` (modify) — `BlackLitterman`-rejects-non-PSD test.
- `internal/optimization/hrp.go` (modify) — rewrite `inverseVariancePortfolio`.
- `internal/optimization/hrp_test.go` (modify) — IVP dominance tests.
- `services/optimization/internal/server/server.go` (modify) — `MaxBytesReader` in `decode`.
- `services/optimization/internal/server/server_test.go` (modify) — oversized-body test.

---

### Task 1: `checkPSD` + wiring into `Optimize` and `BlackLitterman`

**Files:**
- Create: `internal/optimization/psd.go`, `internal/optimization/psd_test.go`
- Modify: `internal/optimization/optimizer.go`, `internal/optimization/optimizer_test.go`, `internal/optimization/bl.go`, `internal/optimization/bl_test.go`

**Interfaces:**
- Produces (consumed by the wiring + Task nothing-else): `var ErrNotPSD error`; `func checkPSD(cov [][]float64) error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/optimization/psd_test.go`:

```go
package optimization

import "testing"

func TestCheckPSD(t *testing.T) {
	tests := []struct {
		name    string
		cov     [][]float64
		wantErr bool
	}{
		{"positive-definite diagonal", diag(0.04, 0.09), false},
		{"positive-definite correlated", [][]float64{{0.04, 0.036}, {0.036, 0.04}}, false},
		{"singular PSD (perfectly correlated)", [][]float64{{1, 1}, {1, 1}}, false},
		{"indefinite (positive diagonal)", [][]float64{{1, -2}, {-2, 1}}, true},
		{"asymmetric", [][]float64{{1, 0.5}, {0.4, 1}}, true},
		{"1x1 non-negative", [][]float64{{0.04}}, false},
		{"1x1 negative", [][]float64{{-0.01}}, true},
		{"numerical-noise near-PSD accepted", [][]float64{{1, 1.0000000001}, {1.0000000001, 1}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPSD(tc.cov)
			if tc.wantErr && err != ErrNotPSD {
				t.Fatalf("want ErrNotPSD, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
```

Append to `internal/optimization/optimizer_test.go`:

```go
func TestOptimize_RejectsNonPSD(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: [][]float64{{1, -2}, {-2, 1}}}
	if _, err := Optimize(in, Objective{Type: MinVariance}, nil); err != ErrNotPSD {
		t.Fatalf("indefinite Σ ⇒ ErrNotPSD, got %v", err)
	}
}
```

Append to `internal/optimization/bl_test.go`:

```go
func TestBlackLitterman_RejectsNonPSD(t *testing.T) {
	_, err := BlackLitterman(BLInput{
		Covariance:    [][]float64{{1, -2}, {-2, 1}},
		MarketWeights: []float64{0.5, 0.5},
		RiskAversion:  2.5, Tau: 0.05,
	})
	if err != ErrNotPSD {
		t.Fatalf("indefinite Σ ⇒ ErrNotPSD, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run 'TestCheckPSD|TestOptimize_RejectsNonPSD|TestBlackLitterman_RejectsNonPSD' -v`
Expected: FAIL — `undefined: checkPSD` / `undefined: ErrNotPSD` (and the Optimize/BL tests fail because the indefinite matrix is not yet rejected).

- [ ] **Step 3: Write `checkPSD`**

Create `internal/optimization/psd.go`:

```go
package optimization

import (
	"errors"
	"math"
)

// ErrNotPSD is returned when a covariance matrix is not positive-semidefinite —
// asymmetric or indefinite. A PSD-but-singular matrix (collinear assets, a zero
// eigenvalue) is accepted, because a real covariance may legitimately be singular
// and the solvers tolerate it.
var ErrNotPSD = errors.New("optimization: covariance matrix is not positive-semidefinite")

// checkPSD reports whether cov is symmetric and positive-semidefinite, via an
// LDLᵀ decomposition inspected for a negative pivot. It rejects a genuinely
// INDEFINITE matrix (a negative eigenvalue) but ACCEPTS a singular PSD one (a
// zero eigenvalue). The tolerance is scaled to the matrix magnitude so numerical
// round-trip noise is not rejected. O(n³); never panics on a square matrix.
func checkPSD(cov [][]float64) error {
	n := len(cov)
	if n == 0 {
		return nil
	}
	var maxDiag float64
	for i := 0; i < n; i++ {
		if d := math.Abs(cov[i][i]); d > maxDiag {
			maxDiag = d
		}
	}
	tol := 1e-9 * (1 + maxDiag)

	// A covariance is symmetric by construction.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if math.Abs(cov[i][j]-cov[j][i]) > tol {
				return ErrNotPSD
			}
		}
	}

	// LDLᵀ: A = L·D·Lᵀ, L unit lower-triangular. A is PSD iff every pivot D_j ≥ 0
	// and every zero pivot has a consistent (≈0) column.
	d := make([]float64, n)
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
		l[i][i] = 1
	}
	for j := 0; j < n; j++ {
		dj := cov[j][j]
		for k := 0; k < j; k++ {
			dj -= l[j][k] * l[j][k] * d[k]
		}
		if dj < -tol {
			return ErrNotPSD // negative pivot ⇒ indefinite
		}
		d[j] = dj
		for i := j + 1; i < n; i++ {
			num := cov[i][j]
			for k := 0; k < j; k++ {
				num -= l[i][k] * l[j][k] * d[k]
			}
			if math.Abs(dj) <= tol {
				// Zero pivot (singular direction): PSD requires a consistent column.
				if math.Abs(num) > tol {
					return ErrNotPSD
				}
				l[i][j] = 0
			} else {
				l[i][j] = num / dj
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Wire `checkPSD` into `Optimize`**

In `internal/optimization/optimizer.go`, immediately after the existing covariance square/dimension check, add the PSD check:

```go
	if in.Covariance != nil && (len(in.Covariance) != n || !square(in.Covariance, n)) {
		return Result{}, ErrInputsMismatch
	}
	if in.Covariance != nil {
		if err := checkPSD(in.Covariance); err != nil {
			return Result{}, err
		}
	}
```

- [ ] **Step 5: Wire `checkPSD` into `BlackLitterman`**

In `internal/optimization/bl.go`, immediately after the `square`/`MarketWeights` dimension check (before computing `Π`), add:

```go
	if err := checkPSD(in.Covariance); err != nil {
		return nil, err
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run 'TestCheckPSD|TestOptimize_RejectsNonPSD|TestBlackLitterman_RejectsNonPSD' -v`
Expected: PASS.

- [ ] **Step 7: Full package regression — the critical check that `checkPSD` rejects nothing valid**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/`
Expected: `ok` — every existing test (all using PSD covariances) still passes. If any existing test now fails with `ErrNotPSD`, its covariance is genuinely non-PSD; fix that test's matrix, do NOT loosen `checkPSD`.

- [ ] **Step 8: Commit**

```bash
cd /c/Users/root/Desktop/eighred-kanz
git add kanz/internal/optimization/psd.go kanz/internal/optimization/psd_test.go kanz/internal/optimization/optimizer.go kanz/internal/optimization/optimizer_test.go kanz/internal/optimization/bl.go kanz/internal/optimization/bl_test.go
git commit -m "feat(opt): reject non-PSD covariance at Optimize + BlackLitterman entry (OPT-HARDEN)"
```

---

### Task 2: `inverseVariancePortfolio` — riskless assets dominate

**Files:**
- Modify: `internal/optimization/hrp.go`
- Modify: `internal/optimization/hrp_test.go`

**Interfaces:**
- Consumes/rewrites: `func inverseVariancePortfolio(cov [][]float64, items []int) []float64` (behavior change for the mixed riskless/risky case; all-positive and all-riskless unchanged).

- [ ] **Step 1: Write the failing tests**

Append to `internal/optimization/hrp_test.go`:

```go
func TestInverseVariancePortfolio_RisklessDominates(t *testing.T) {
	// A riskless asset (variance 0) among risky ones takes all the weight —
	// the 1/σ²→∞ limit. Previously it was (wrongly) excluded.
	ivp := inverseVariancePortfolio(diag(0.0, 0.04, 0.04), []int{0, 1, 2})
	approx(t, "riskless dominates", ivp[0], 1.0, 1e-12)
	approx(t, "risky 1 zero", ivp[1], 0.0, 1e-12)
	approx(t, "risky 2 zero", ivp[2], 0.0, 1e-12)
}

func TestInverseVariancePortfolio_AllPositiveUnchanged(t *testing.T) {
	// σ²=[0.01,0.04] ⇒ ivp ∝ [100,25] ⇒ [0.8,0.2] — the common case, unchanged.
	ivp := inverseVariancePortfolio(diag(0.01, 0.04), []int{0, 1})
	approx(t, "w0", ivp[0], 0.8, 1e-12)
	approx(t, "w1", ivp[1], 0.2, 1e-12)
}

func TestInverseVariancePortfolio_AllRisklessEqual(t *testing.T) {
	// All-riskless ⇒ equal weights (unchanged fallback).
	ivp := inverseVariancePortfolio(diag(0.0, 0.0, 0.0, 0.0), []int{0, 1, 2, 3})
	for i := 0; i < 4; i++ {
		approx(t, "equal", ivp[i], 0.25, 1e-12)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run TestInverseVariancePortfolio -v`
Expected: FAIL — `TestInverseVariancePortfolio_RisklessDominates` fails (the current code gives the riskless asset 0, not 1). The other two pass already (unchanged behavior), which is the point.

- [ ] **Step 3: Rewrite `inverseVariancePortfolio`**

In `internal/optimization/hrp.go`, replace the whole `inverseVariancePortfolio` function with:

```go
// inverseVariancePortfolio returns the normalized inverse-variance weights over a
// cluster: ivp_i ∝ 1/Σ_ii. A riskless asset (Σ_ii ≤ 0) is the limit 1/Σ_ii → ∞:
// it DOMINATES the portfolio. If any exist, weight is split equally among the
// riskless assets and the risk-bearing assets get zero (all-riskless ⇒ equal
// weights). Otherwise the standard normalized inverse-variance.
func inverseVariancePortfolio(cov [][]float64, items []int) []float64 {
	ivp := make([]float64, len(items))
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/ -run 'TestInverseVariancePortfolio|TestHRP' -v`
Expected: PASS — the three new IVP tests AND the existing `TestHRP_*` tests (which only assert finite/long-only/sum-to-1 for the zero-variance edge, so the dominance change does not regress them).

- [ ] **Step 5: Full package regression**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./internal/optimization/`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
cd /c/Users/root/Desktop/eighred-kanz
git add kanz/internal/optimization/hrp.go kanz/internal/optimization/hrp_test.go
git commit -m "fix(opt): riskless assets dominate the inverse-variance portfolio (OPT-HARDEN)"
```

---

### Task 3: `/v1/propose` request-body cap

**Files:**
- Modify: `services/optimization/internal/server/server.go`
- Modify: `services/optimization/internal/server/server_test.go`

**Interfaces:**
- Consumes: the existing `decode` helper (adds a body cap; both `/v1/propose` and `/v1/orders` route through it).

- [ ] **Step 1: Write the failing test**

Append to `services/optimization/internal/server/server_test.go`:

```go
func TestServer_Propose_BodyTooLarge(t *testing.T) {
	// A body over the 8 MiB cap ⇒ 400 (MaxBytesReader makes the decoder error).
	big := strings.Repeat("A", (8<<20)+1024)
	body := `{"portfolio_id":"` + big + `","instruments":["A"],"covariance":[[0.04]],"objective":{"Type":1}}`
	rec := do(t, newTestServer(), http.MethodPost, "/v1/propose", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body ⇒ 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./services/optimization/internal/server/ -run TestServer_Propose_BodyTooLarge -v`
Expected: FAIL — without a cap the giant body decodes fine (or fails for an unrelated reason), so the 400 is not produced by the cap. (It may currently return 200 or a different code; either way the test is red until the cap exists.)

- [ ] **Step 3: Add the body cap**

In `services/optimization/internal/server/server.go`, add the const (near the top, after the imports) and wrap the body in `decode`:

```go
// maxRequestBytes bounds a /v1 request body. A dense covariance for a several-
// hundred-asset universe fits well under 8 MiB; the cap turns an unbounded body
// (which drives O(n³)/O(k³) solver work) into a 400.
const maxRequestBytes = 8 << 20 // 8 MiB
```

Then in `decode`:

```go
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return false
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /c/Users/root/Desktop/eighred-kanz/kanz && GOFLAGS=-mod=mod go test ./services/optimization/internal/server/ -run TestServer_Propose -v`
Expected: PASS — the oversized-body test returns 400; the existing `TestServer_Propose` and BL tests (normal small bodies) still pass unaffected.

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
git commit -m "fix(opt): cap /v1 request body at 8 MiB (OPT-HARDEN)"
```

---

## Post-implementation

After all three tasks: update `KANZ_TASKS.md` (OPT-HARDEN → DONE; the optimizer-hardening follow-ups from RISK-M2a/M2b are now closed). No `KANZ_BRAIN.md` entry is expected — these are input-validation fixes, not durable architectural decisions (the module-wide "Σ is assumed PSD" precondition is now *enforced* rather than assumed, which the DONE entry records). Board hygiene, done in the wrap-up, not a plan task.
