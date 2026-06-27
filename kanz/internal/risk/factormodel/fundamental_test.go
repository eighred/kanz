package factormodel

import (
	"math"
	"testing"
)

// TestBuildLoadings_ReproduceKnownExposures: style columns are z-scored and
// industry/country memberships become 0/1 dummies in sorted order.
func TestBuildLoadings_ReproduceKnownExposures(t *testing.T) {
	instruments := []string{"A", "B", "C"}
	chars := map[string]Characteristics{
		"A": {Style: map[string]float64{"Size": 1}, Industry: "Tech"},
		"B": {Style: map[string]float64{"Size": 2}, Industry: "Tech"},
		"C": {Style: map[string]float64{"Size": 3}, Industry: "Bank"},
	}
	factors, b := buildFundamentalLoadings([]string{"Size"}, instruments, chars)

	// Factors: Size (style), then sorted industries IND:Bank, IND:Tech.
	wantNames := []string{"Size", "IND:Bank", "IND:Tech"}
	if len(factors) != 3 {
		t.Fatalf("want 3 factors, got %d", len(factors))
	}
	for i, n := range wantNames {
		if factors[i].Name != n {
			t.Fatalf("factor %d: got %q want %q", i, factors[i].Name, n)
		}
	}
	// Size z-scores: mean 2, sd 1 ⇒ [-1, 0, 1].
	wantZ := []float64{-1, 0, 1}
	for i := range instruments {
		if math.Abs(b[i][0]-wantZ[i]) > 1e-9 {
			t.Fatalf("z-score[%d]: got %.6f want %.6f", i, b[i][0], wantZ[i])
		}
	}
	// IND:Bank dummy (col 1): only C. IND:Tech (col 2): A,B.
	wantBank := []float64{0, 0, 1}
	wantTech := []float64{1, 1, 0}
	for i := range instruments {
		if b[i][1] != wantBank[i] || b[i][2] != wantTech[i] {
			t.Fatalf("dummies[%d]: got bank=%v tech=%v", i, b[i][1], b[i][2])
		}
	}
}

// TestCrossSectionalFit_RecoversFactorReturns: with returns generated exactly as
// rᵢ,ₜ = Bᵢ·fₜ (no residual), the regression recovers the factor returns, so the
// estimated factor covariance equals the sample covariance of fₜ and the specific
// variance is ~0.
func TestCrossSectionalFit_RecoversFactorReturns(t *testing.T) {
	// 3 instruments, 2 factors, full-rank design.
	b := [][]float64{{1, 0}, {0, 1}, {1, 1}}
	f := [][]float64{ // factor returns: f[k][t]
		{0.01, -0.02, 0.03, 0.00, 0.015},
		{-0.005, 0.01, 0.02, -0.01, 0.005},
	}
	l := len(f[0])
	returns := make([][]float64, 3)
	for i := range b {
		returns[i] = make([]float64, l)
		for t := 0; t < l; t++ {
			returns[i][t] = b[i][0]*f[0][t] + b[i][1]*f[1][t]
		}
	}
	factorCov, specific, residuals := crossSectionalFit(b, returns, DefaultRidge)

	wantCov := sampleCov(f)
	for i := 0; i < 2; i++ {
		for j := 0; j < 2; j++ {
			if math.Abs(factorCov[i][j]-wantCov[i][j]) > 1e-6 {
				t.Fatalf("factorCov[%d][%d]: got %.8f want %.8f", i, j, factorCov[i][j], wantCov[i][j])
			}
		}
	}
	for i, s := range specific {
		if math.Abs(s) > 1e-9 {
			t.Fatalf("specific[%d] should be ~0 (perfect fit), got %.10f", i, s)
		}
	}
	for i := range residuals {
		for tt := range residuals[i] {
			if math.Abs(residuals[i][tt]) > 1e-9 {
				t.Fatalf("residual[%d][%d] should be ~0, got %.10f", i, tt, residuals[i][tt])
			}
		}
	}
}
