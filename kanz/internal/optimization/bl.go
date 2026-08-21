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
	// The rank is not consulted here: BL does not report a risk number, and the
	// one matrix it inverts (M) is Σ-derived but not Σ, so a singular Σ surfaces
	// as ErrBLSingular from solveLinear below rather than as a quality flag.
	if _, err := psdRank(in.Covariance); err != nil {
		return nil, err
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
