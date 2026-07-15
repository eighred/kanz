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
