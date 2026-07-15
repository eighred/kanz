package optimization

import (
	"math"
	"testing"
)

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.6f want %.6f (Δ %.2e)", name, got, want, math.Abs(got-want))
	}
}

func sumWeights(w map[string]float64) float64 {
	var s float64
	for _, v := range w {
		s += v
	}
	return s
}

// diag builds a diagonal covariance from per-asset variances.
func diag(vars ...float64) [][]float64 {
	n := len(vars)
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, n)
		m[i][i] = vars[i]
	}
	return m
}

func TestMinVariance_KnownFrontierPoint(t *testing.T) {
	// Two uncorrelated assets, σ₁=0.1 (var 0.01), σ₂=0.2 (var 0.04). The
	// long-only min-variance weights are ∝ 1/varᵢ: w₁ = 0.04/0.05 = 0.8,
	// w₂ = 0.2 (the closed-form min-variance portfolio).
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.01, 0.04)}
	res, err := Optimize(in, Objective{Type: MinVariance}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "w_A", res.Weights["A"], 0.8, 1e-3)
	approx(t, "w_B", res.Weights["B"], 0.2, 1e-3)
	approx(t, "sum", sumWeights(res.Weights), 1, 1e-9)
}

func TestMinVariance_EqualVolZeroCorr(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.04, 0.04)}
	res, _ := Optimize(in, Objective{Type: MinVariance}, nil)
	approx(t, "equal weights", res.Weights["A"], 0.5, 1e-3)
}

func TestMaxReturn_GoesToHighestSubjectToCap(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, ExpectedReturns: []float64{0.05, 0.10}}
	// Unconstrained long-only ⇒ all into the higher-return asset B.
	res, _ := Optimize(in, Objective{Type: MaxReturn}, nil)
	approx(t, "all in B", res.Weights["B"], 1.0, 1e-6)

	// With a 0.6 cap on B, the remainder spills to A.
	cons := &ConstraintSet{LongOnly: true, MaxWeight: 1, Bounds: map[string]WeightBound{"B": {Min: 0, Max: 0.6}}}
	res2, _ := Optimize(in, Objective{Type: MaxReturn}, cons)
	approx(t, "B capped", res2.Weights["B"], 0.6, 1e-6)
	approx(t, "A spill", res2.Weights["A"], 0.4, 1e-6)
}

func TestMaxSharpe_TangencyPortfolio(t *testing.T) {
	// Diagonal Σ, μ=[0.05,0.10], rf=0 ⇒ tangency w ∝ Σ⁻¹μ = [0.05/0.01, 0.10/0.04]
	// = [5, 2.5] ⇒ normalized [0.667, 0.333].
	in := MarketInputs{Instruments: []string{"A", "B"}, ExpectedReturns: []float64{0.05, 0.10}, Covariance: diag(0.01, 0.04)}
	res, _ := Optimize(in, Objective{Type: MaxSharpe}, nil)
	approx(t, "w_A", res.Weights["A"], 2.0/3.0, 2e-3)
	approx(t, "w_B", res.Weights["B"], 1.0/3.0, 2e-3)
}

func TestRiskParity_EqualRiskContribution(t *testing.T) {
	// Uncorrelated σ₁=0.1, σ₂=0.2 ⇒ ERC weights ∝ 1/σᵢ = [10,5] ⇒ [0.667,0.333],
	// and the two risk contributions are equal.
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: diag(0.01, 0.04)}
	res, _ := Optimize(in, Objective{Type: RiskParity}, nil)
	approx(t, "w_A", res.Weights["A"], 2.0/3.0, 2e-3)
	rc := RiskContributions(res.Weights, in)
	approx(t, "equal risk contribution", rc["A"], rc["B"], 1e-3)
	approx(t, "RC sum to 1", rc["A"]+rc["B"], 1, 1e-9)
}

func TestConstraintsBind_MaxWeightRespected(t *testing.T) {
	// A 3-asset min-variance where the lowest-vol asset would dominate; a 0.5 cap
	// must bind.
	in := MarketInputs{Instruments: []string{"A", "B", "C"}, Covariance: diag(0.01, 0.04, 0.04)}
	cons := &ConstraintSet{LongOnly: true, MaxWeight: 0.5}
	res, _ := Optimize(in, Objective{Type: MinVariance}, cons)
	if res.Weights["A"] > 0.5+1e-6 {
		t.Fatalf("max-weight cap violated: w_A=%.4f", res.Weights["A"])
	}
	approx(t, "sum", sumWeights(res.Weights), 1, 1e-6)
}

func TestSampleCovariance(t *testing.T) {
	// Perfectly anti-correlated → off-diagonal negative, diagonals equal.
	a := []float64{0.01, -0.01, 0.02, -0.02}
	b := []float64{-0.01, 0.01, -0.02, 0.02}
	cov := SampleCovariance([][]float64{a, b})
	if cov[0][1] >= 0 {
		t.Fatalf("anti-correlated assets must have negative covariance, got %.6f", cov[0][1])
	}
	approx(t, "symmetric", cov[0][1], cov[1][0], 0)
}

func TestOptimize_InputValidation(t *testing.T) {
	if _, err := Optimize(MarketInputs{}, Objective{Type: MinVariance}, nil); err != ErrNoUniverse {
		t.Fatalf("want ErrNoUniverse, got %v", err)
	}
	if _, err := Optimize(MarketInputs{Instruments: []string{"A"}}, Objective{Type: MaxReturn}, nil); err != ErrNeedReturns {
		t.Fatalf("want ErrNeedReturns, got %v", err)
	}
	if _, err := Optimize(MarketInputs{Instruments: []string{"A"}}, Objective{Type: MinVariance}, nil); err != ErrNeedCovariance {
		t.Fatalf("want ErrNeedCovariance, got %v", err)
	}
}

func TestOptimize_HRP(t *testing.T) {
	// Same two-cluster cov as the hrp unit test ⇒ [0.4,0.4,0.1,0.1], keyed by id.
	cov := [][]float64{
		{0.04, 0.036, 0, 0},
		{0.036, 0.04, 0, 0},
		{0, 0, 0.16, 0.144},
		{0, 0, 0.144, 0.16},
	}
	in := MarketInputs{Instruments: []string{"A", "B", "C", "D"}, Covariance: cov}
	res, err := Optimize(in, Objective{Type: HRP}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "w_A", res.Weights["A"], 0.4, 1e-9)
	approx(t, "w_D", res.Weights["D"], 0.1, 1e-9)
	approx(t, "sum", sumWeights(res.Weights), 1, 1e-9)
	// ExpectedRisk = √(wᵀΣw); ExpectedReturn = 0 with no μ supplied.
	if res.ExpectedRisk <= 0 {
		t.Fatalf("HRP result should carry a positive ExpectedRisk, got %v", res.ExpectedRisk)
	}
	approx(t, "no μ ⇒ zero expected return", res.ExpectedReturn, 0, 1e-12)
}

func TestOptimize_HRP_NeedsCovariance(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}}
	if _, err := Optimize(in, Objective{Type: HRP}, nil); err != ErrNeedCovariance {
		t.Fatalf("HRP with no covariance ⇒ ErrNeedCovariance, got %v", err)
	}
}

// HRP ignores box bounds (the approved decision): the result equals the
// unconstrained HRP weights even when a non-default ConstraintSet is supplied.
func TestOptimize_HRP_IgnoresBoxBounds(t *testing.T) {
	cov := [][]float64{
		{0.04, 0.036, 0, 0},
		{0.036, 0.04, 0, 0},
		{0, 0, 0.16, 0.144},
		{0, 0, 0.144, 0.16},
	}
	in := MarketInputs{Instruments: []string{"A", "B", "C", "D"}, Covariance: cov}
	// A 0.3 cap on A would bind for a bounds-respecting objective; HRP must ignore it.
	cons := &ConstraintSet{LongOnly: true, MaxWeight: 0.3}
	res, err := Optimize(in, Objective{Type: HRP}, cons)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "w_A unclamped", res.Weights["A"], 0.4, 1e-9)
}

func TestOptimize_RejectsNonPSD(t *testing.T) {
	in := MarketInputs{Instruments: []string{"A", "B"}, Covariance: [][]float64{{1, -2}, {-2, 1}}}
	if _, err := Optimize(in, Objective{Type: MinVariance}, nil); err != ErrNotPSD {
		t.Fatalf("indefinite Σ ⇒ ErrNotPSD, got %v", err)
	}
}
