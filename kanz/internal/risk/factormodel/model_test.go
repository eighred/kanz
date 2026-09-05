package factormodel

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestShockReturn: a factor shock reprices an instrument by Σ loadingₖ·shockₖ
// over the named factors; an unknown instrument reports ok=false.
func TestShockReturn(t *testing.T) {
	m := newModel(
		[]Factor{{Name: "Value", Type: FactorStyle}, {Name: "Momentum", Type: FactorStyle}},
		[]string{"A"},
		[][]float64{{2, -1}}, // A loads +2 on Value, −1 on Momentum
		[][]float64{{0.04, 0}, {0, 0.04}},
		map[string]float64{"A": 0.01},
	)
	// shock Value +5%, Momentum −10% ⇒ 2·0.05 + (−1)·(−0.10) = 0.20.
	r, ok := m.ShockReturn("A", map[string]float64{"Value": 0.05, "Momentum": -0.10})
	if !ok || math.Abs(r-0.20) > 1e-12 {
		t.Fatalf("shock return: got %.6f ok=%v want 0.20", r, ok)
	}
	if _, ok := m.ShockReturn("UNKNOWN", map[string]float64{"Value": 0.05}); ok {
		t.Fatal("unknown instrument must report ok=false")
	}
}

// TestVaR: parametric VaR is z₀.₉₉ · total risk.
func TestVaR(t *testing.T) {
	m := oneFactorModel()
	values := map[string]float64{"A": 100, "B": -50}
	total := m.Risk(values).Total
	v := m.VaR(values, 0.99)
	if math.Abs(v/total-2.3263) > 1e-3 {
		t.Fatalf("VaR/total = %.5f, want z₀.₉₉ ≈ 2.3263", v/total)
	}
}

// THE INSTANCE IDENTITY (#1039). A Model was an anonymous bag of matrices: the
// loadings and covariance behind a published FactorVaR99 lived for one call and
// nothing on the wire named them, so "reproduce last Tuesday's VaR" had no
// referent to resolve.

func TestFit_StampsTheInstanceIdentity(t *testing.T) {
	asOf := time.Date(2026, 9, 3, 14, 30, 0, 0, time.UTC)
	cfg := Config{Type: Statistical, StatFactors: 2, Window: 40}
	m, err := Fit(context.Background(), cfg, []string{"A", "B", "C"}, asOf,
		Providers{Returns: identityReturns{}})
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if m.ModelID != DefaultModelID(cfg) {
		t.Errorf("ModelID=%q want %q", m.ModelID, DefaultModelID(cfg))
	}
	if !m.AsOf.Equal(asOf) {
		t.Errorf("AsOf=%v want %v — the as-of is the point-in-time half of the key", m.AsOf, asOf)
	}
}

func TestFit_AnExplicitModelIDOverridesTheDerivedOne(t *testing.T) {
	cfg := Config{Type: Statistical, StatFactors: 2, Window: 40, ModelID: "DESK-GLOBAL-EQ"}
	m, err := Fit(context.Background(), cfg, []string{"A", "B", "C"}, time.Now().UTC(),
		Providers{Returns: identityReturns{}})
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if m.ModelID != "DESK-GLOBAL-EQ" {
		t.Errorf("ModelID=%q want the configured override", m.ModelID)
	}
}

// TestDefaultModelID_ChangesWithTheEstimationParameters is the point of putting
// them in the id at all: a deployment that widens the lookback publishes a
// MATERIALLY DIFFERENT model, and under a bare "STATISTICAL" it would publish it
// under the id yesterday's number cites. The join would still resolve and the
// numbers would no longer reconcile, with nothing reporting a change.
func TestDefaultModelID_ChangesWithTheEstimationParameters(t *testing.T) {
	base := Config{Type: Statistical, StatFactors: 3, Window: 250}
	seen := map[string]string{}
	for name, cfg := range map[string]Config{
		"base":          base,
		"wider window":  {Type: Statistical, StatFactors: 3, Window: 500},
		"more factors":  {Type: Statistical, StatFactors: 5, Window: 250},
		"another model": {Type: Blend, StatFactors: 3, Window: 250},
	} {
		id := DefaultModelID(cfg)
		if prev, clash := seen[id]; clash {
			t.Errorf("%q and %q derive the SAME model id %q — one configuration would silently "+
				"redefine the other's published instances", name, prev, id)
		}
		seen[id] = name
	}
	if got := DefaultModelID(base); got != "STATISTICAL-3F-250D" {
		t.Errorf("DefaultModelID(base)=%q want STATISTICAL-3F-250D", got)
	}
}

// identityReturns is a deterministic panel with unequal variance so the PCA is
// non-degenerate.
type identityReturns struct{}

func (identityReturns) Returns(_ context.Context, id string, _ time.Time, n int) ([]float64, error) {
	scale := map[string]float64{"A": 0.01, "B": 0.02, "C": -0.015}[id]
	out := make([]float64, n)
	for i := range out {
		out[i] = scale * math.Sin(float64(i+1))
	}
	return out, nil
}
