package factormodel

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// --- deterministic test providers -----------------------------------------

type matrixReturns map[string][]float64

func (m matrixReturns) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return m[id], nil
}

type staticChars map[string]Characteristics

func (c staticChars) Characteristics(_ context.Context, id string, asOf time.Time) (Characteristics, bool) {
	v, ok := c[id]
	v.AsOf = asOf
	v.SourceDigest = "fixture:" + id
	return v, ok
}

var testReturns = matrixReturns{
	"A": {0.010, -0.020, 0.015, 0.000, -0.010, 0.020},
	"B": {0.012, -0.018, 0.013, 0.002, -0.011, 0.019},
	"C": {-0.005, 0.010, -0.008, 0.004, 0.012, -0.015},
}

// TestFitStatistical_ReconstructsCovariance: a full-rank (k=N) statistical model
// reconstructs the instrument covariance, so the model's total variance equals
// the quadratic form vᵀSv of the sample covariance.
func TestFitStatistical_ReconstructsCovariance(t *testing.T) {
	ids := []string{"A", "B", "C"}
	returns := [][]float64{testReturns["A"], testReturns["B"], testReturns["C"]}
	m, err := fitStatistical(ids, returns, 3, "PC")
	if err != nil {
		t.Fatal(err)
	}

	s := sampleCov(returns)
	values := map[string]float64{"A": 100, "B": -50, "C": 30}
	v := []float64{values["A"], values["B"], values["C"]}
	want := quadForm(s, v)
	got := m.Risk(values).Total * m.Risk(values).Total
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("full-rank statistical variance %.10f must equal vᵀSv %.10f", got, want)
	}
}

// TestFit_Dispatch: Statistical builds PCs; Blend stacks the fundamental factors
// with statistical residual factors (Kf+Ks).
func TestFit_Dispatch(t *testing.T) {
	ctx := context.Background()
	asOf := time.Now()
	ids := []string{"A", "B", "C"}
	chars := staticChars{
		"A": {Style: map[string]float64{"Size": 1}, Industry: "Tech"},
		"B": {Style: map[string]float64{"Size": 2}, Industry: "Tech"},
		"C": {Style: map[string]float64{"Size": 3}, Industry: "Bank"},
	}
	ids = append(ids, "D")
	chars["D"] = Characteristics{Style: map[string]float64{"Size": 4}, Industry: "Bank"}
	rets := matrixReturns{"A": testReturns["A"], "B": testReturns["B"], "C": testReturns["C"], "D": {.03, -.01, .01, .02, -.03, .01}}
	p := Providers{Characteristics: chars, Returns: rets}

	stat, err := Fit(ctx, Config{Type: Statistical, StatFactors: 2}, ids, asOf, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(stat.Factors) != 2 {
		t.Fatalf("statistical model should have 2 PCs, got %d", len(stat.Factors))
	}

	fund, err := Fit(ctx, Config{Type: Fundamental, StyleFactors: []string{"Size"}}, ids, asOf, p)
	if err != nil {
		t.Fatal(err)
	}
	kf := len(fund.Factors) // Size + IND:Bank + IND:Tech = 3

	bl, err := Fit(ctx, Config{Type: Blend, StyleFactors: []string{"Size"}, StatFactors: 2}, ids, asOf, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl.Factors) != kf+2 {
		t.Fatalf("blend should stack %d fundamental + 2 statistical factors, got %d", kf, len(bl.Factors))
	}
	if !strings.HasSuffix(bl.ModelID, "-JOINT1") || bl.InputProvenance["blend_covariance"] != "aligned-joint-v1" {
		t.Fatalf("blend estimator identity missing: %s, %v", bl.ModelID, bl.InputProvenance)
	}
}

// TestFit_RequiresReturns: a missing ReturnsProvider is an error, not a panic.
func TestFit_RequiresReturns(t *testing.T) {
	if _, err := Fit(context.Background(), Config{Type: Statistical}, []string{"A"}, time.Now(), Providers{}); err == nil {
		t.Fatal("Fit without a ReturnsProvider must error")
	}
}

// Full retention reconstructs every observed instrument return as fitted
// fundamental return plus residual. Portfolio P&L variance can therefore be
// calculated directly from the dated returns without any factor operations.
func TestBlendReconstructsObservedPortfolioVarianceWithRidge(t *testing.T) {
	at := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	ids := []string{"A", "B", "C"}
	chars := staticChars{"A": {Industry: "IND"}, "B": {Industry: "IND"}, "C": {Industry: "IND"}}
	m, err := Fit(context.Background(), Config{Type: Blend, StatFactors: 3, Ridge: 3}, ids, at, Providers{Characteristics: chars, Returns: testReturns})
	if err != nil {
		t.Fatal(err)
	}
	if !retainedCovarianceValid(m.FactorCov) {
		t.Fatalf("joint covariance is not positive semidefinite: %v", m.FactorCov)
	}
	for _, weights := range [][]float64{{100, 100, 100}, {100, -50, 30}, {0, 0, 100}} {
		var values = map[string]float64{}
		profit := make([]float64, len(testReturns["A"]))
		for i, id := range ids {
			values[id] = weights[i]
			for j, r := range testReturns[id] {
				profit[j] += weights[i] * r
			}
		}
		// Pairwise differences give unbiased sample variance directly.
		var sum float64
		for i, x := range profit {
			for _, y := range profit[:i] {
				sum += (x - y) * (x - y)
			}
		}
		want := sum / float64(len(profit)*(len(profit)-1))
		risk := m.Risk(values).Total
		if got := risk * risk; !finite(got) || math.Abs(got-want) > 1e-10 {
			t.Errorf("portfolio %v: variance %.12g, want %.12g in squared currency units", weights, got, want)
		}
	}
}
