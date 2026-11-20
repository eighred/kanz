package factormodel

import (
	"math"
	"testing"
)

// oneFactorModel: 2 instruments A,B both loading 1 on a single factor with
// variance 0.04; specific variance 0.01 each. Hand-checkable.
func oneFactorModel() *Model {
	return newModel(
		[]Factor{{Name: "MKT", Type: FactorStyle}},
		[]string{"A", "B"},
		[][]float64{{1}, {1}},
		[][]float64{{0.04}},
		map[string]float64{"A": 0.01, "B": 0.01},
	)
}

func TestFactorExposuresRetainSignedHedgeResidualAcrossUniverseOrders(t *testing.T) {
	for _, ids := range [][]string{{"large", "small", "hedge"}, {"small", "large", "hedge"}, {"hedge", "large", "small"}} {
		m := newModel([]Factor{{Name: "F1"}, {Name: "F2"}}, ids, [][]float64{{1, -1}, {1, -1}, {1, -1}}, nil, nil)
		for _, small := range []float64{-1, 0, 1} {
			// All operands are exact powers of two or signed units.
			got := m.FactorExposures(map[string]float64{"large": 1 << 55, "hedge": -(1 << 55), "small": small})
			if got[0] != small || got[1] != -small {
				t.Errorf("universe %v, residual %g: got %v", ids, small, got)
			}
		}
	}
}

// TestRisk_FactorPlusSpecificEqualsTotal: Total² = Systematic² + Specific².
func TestRisk_FactorPlusSpecificEqualsTotal(t *testing.T) {
	m := oneFactorModel()
	values := map[string]float64{"A": 100, "B": -50}
	rb := m.Risk(values)

	// e = 100+(-50) = 50; sysVar = 50·0.04·50 = 100 ⇒ systematic 10.
	if math.Abs(rb.Systematic-10) > 1e-9 {
		t.Fatalf("systematic: got %.6f want 10", rb.Systematic)
	}
	// specVar = 100²·0.01 + 50²·0.01 = 125 ⇒ specific √125.
	if math.Abs(rb.Specific-math.Sqrt(125)) > 1e-9 {
		t.Fatalf("specific: got %.6f want %.6f", rb.Specific, math.Sqrt(125))
	}
	if d := math.Abs(rb.Total*rb.Total - (rb.Systematic*rb.Systematic + rb.Specific*rb.Specific)); d > 1e-6 {
		t.Fatalf("Total² must equal Systematic²+Specific², residual %.8f", d)
	}
}

// TestDecompose_ContributionsSumToTotal: the per-instrument component
// contributions sum to total risk, and the per-factor contributions plus the
// specific contribution also sum to total risk (Euler decomposition).
func TestDecompose_ContributionsSumToTotal(t *testing.T) {
	m := oneFactorModel()
	values := map[string]float64{"A": 100, "B": -50}
	d := m.Decompose(values)

	var compSum float64
	for _, c := range d.ComponentRisk {
		compSum += c
	}
	if math.Abs(compSum-d.Total) > 1e-6 {
		t.Fatalf("component contributions %.6f must sum to total %.6f", compSum, d.Total)
	}

	var factorSum float64
	for _, c := range d.FactorRiskContribution {
		factorSum += c
	}
	if math.Abs(factorSum+d.SpecificRiskContribution-d.Total) > 1e-6 {
		t.Fatalf("factor (%.6f) + specific (%.6f) must sum to total %.6f", factorSum, d.SpecificRiskContribution, d.Total)
	}
}

// TestTrackingError: identical portfolio and benchmark ⇒ TE 0; a deviation ⇒
// TE > 0.
func TestTrackingError(t *testing.T) {
	m := oneFactorModel()
	port := map[string]float64{"A": 100, "B": 0}
	if te := m.TrackingError(port, port); te != 0 {
		t.Fatalf("TE vs self must be 0, got %.6f", te)
	}
	bench := map[string]float64{"A": 50, "B": 50}
	if te := m.TrackingError(port, bench); te <= 0 {
		t.Fatalf("TE vs a different benchmark must be positive, got %.6f", te)
	}
}

func TestDecomposeMarginalsAgainstIndependentFiniteDifference(t *testing.T) {
	d := oneFactorModel().Decompose(map[string]float64{"A": 100, "B": -50})
	// Direct instrument-space covariance, independently expanded from BFB^T+D.
	// This oracle does not call Model.Risk or any production covariance helper.
	risk := func(a, b float64) float64 { return math.Sqrt(.05*a*a + .08*a*b + .05*b*b) }
	const h = .001
	for _, check := range []struct {
		id   string
		want float64
	}{
		{"A", (risk(100+h, -50) - risk(100-h, -50)) / (2 * h)},
		{"B", (risk(100, -50+h) - risk(100, -50-h)) / (2 * h)},
	} {
		if math.Abs(d.MarginalRisk[check.id]-check.want) > 1e-9 {
			t.Fatalf("%s marginal %.17g vs independent finite difference %.17g", check.id, d.MarginalRisk[check.id], check.want)
		}
	}
}
