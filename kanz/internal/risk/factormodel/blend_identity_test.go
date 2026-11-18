package factormodel_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/returns/returnstest"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/eighred/kanz/internal/risk/publish"
)

type blendFixture map[string][]float64

func (f blendFixture) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	return f[id], nil
}
func (f blendFixture) DatedReturns(_ context.Context, id string, at time.Time, _ int) (returns.Series, error) {
	return returnstest.Series(id, at, f[id]), nil
}
func (f blendFixture) Characteristics(_ context.Context, id string, at time.Time) (factormodel.Characteristics, bool) {
	weight := map[string]float64{"A": 1, "B": 2, "C": 3}[id]
	return factormodel.Characteristics{AsOf: at, SourceDigest: "fixture:" + id, Industry: "Tech", Style: map[string]float64{"SPC1": weight, "SPC2": weight}}, true
}

func TestBlendedFactorNamesSurviveAttributionAndRetention(t *testing.T) {
	fixture := blendFixture{"A": {.01, -.02, .03, -.01}, "B": {-.02, .04, .01, -.03}, "C": {.03, -.01, .02, .01}}
	cfg := factormodel.Config{Type: factormodel.Blend, StatFactors: 1, StyleFactors: []string{"SPC1"}}
	at := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	fit := func() (*factormodel.Model, error) {
		return factormodel.Fit(t.Context(), cfg, []string{"A", "B", "C"}, at, factormodel.Providers{Characteristics: fixture, Returns: fixture})
	}
	if m, err := fit(); m != nil || err == nil {
		t.Fatalf("ambiguous SPC1 accepted: %v, %v", m, err)
	}
	cfg.StyleFactors = []string{"SPC2"}
	m, err := fit()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := factormodel.FromSnapshot(publish.ToProtoFactorModelSnapshot(m))
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []*factormodel.Model{m, restored} {
		d := model.Decompose(map[string]float64{"A": 100, "B": -50, "C": 70})
		total := d.SpecificRiskContribution
		for _, name := range []string{"SPC1", "SPC2", "IND:Tech"} {
			v, ok := d.FactorRiskContribution[name]
			if !ok {
				t.Fatalf("missing %s in %v", name, d.FactorRiskContribution)
			}
			total += v
		}
		if !(d.Total > 0) || math.Abs(total-d.Total) > 1e-10 {
			t.Fatalf("named contributions %g, total %g", total, d.Total)
		}
	}
}

func TestDatedPCAIsScaleEquivalentForPortfolioVarianceAndHedges(t *testing.T) {
	at := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	ids := []string{"A", "B", "C"}
	for _, typ := range []factormodel.ModelType{factormodel.Statistical, factormodel.Blend} {
		for _, tc := range []struct {
			name    string
			k       int
			panel   blendFixture
			weights []float64
		}{
			{"full", 3, blendFixture{"A": {1, 2, -1, 0, 1}, "B": {2, -1, 1, 2, -2}, "C": {-1, 1, 3, -2, 2}}, []float64{2, -1, 3}},
			{"hedge", 1, blendFixture{"A": {1, -1, 2, -2}, "B": {1, -1, 2, -2}, "C": {-2, 2, -4, 4}}, []float64{1, -1, 0}},
		} {
			// Pairwise differences of dated portfolio P&L give unbiased sample variance.
			pnl := make([]float64, len(tc.panel["A"]))
			weights := map[string]float64{}
			for i, id := range ids {
				weights[id] = tc.weights[i]
				for j, x := range tc.panel[id] {
					pnl[j] += tc.weights[i] * x
				}
			}
			var variance float64
			for i, x := range pnl {
				for _, y := range pnl[:i] {
					variance += (x - y) * (x - y)
				}
			}
			variance /= float64(len(pnl) * (len(pnl) - 1))
			for _, amplitude := range []float64{1e-2, 1e-8, 1e-60} {
				panel := blendFixture{}
				for _, id := range ids {
					for _, x := range tc.panel[id] {
						panel[id] = append(panel[id], x*amplitude)
					}
				}
				cfg := factormodel.Config{Type: typ, StatFactors: tc.k}
				m, err := factormodel.Fit(t.Context(), cfg, ids, at, factormodel.Providers{Returns: panel, Characteristics: panel})
				if err != nil {
					t.Fatal(err)
				}
				restored, err := factormodel.FromSnapshot(publish.ToProtoFactorModelSnapshot(m))
				if err != nil {
					t.Fatal(err)
				}
				for _, model := range []*factormodel.Model{m, restored} {
					if !strings.Contains(model.ModelID, "-PCA2") || model.InputProvenance["pca_solver"] != "scale-relative-jacobi-v2" {
						t.Fatalf("missing solver identity on %v", model.ModelID)
					}
					normalized := model.Risk(weights).Total / amplitude
					got := normalized * normalized
					if math.IsNaN(got) || math.IsInf(got, 0) || math.Abs(got-variance) > 1e-12*math.Max(1, variance) {
						t.Errorf("type=%v %s amplitude=%g: normalized variance %.16g, want %.16g", typ, tc.name, amplitude, got, variance)
					}
					if tc.name == "hedge" {
						risk := model.Risk(map[string]float64{"A": 1}).Total / amplitude
						if !(math.Abs(risk*risk-10.0/3) <= 1e-12) {
							t.Fatalf("directional risk disappeared: %g", risk)
						}
					}
				}
			}
		}
	}
}

func TestStatisticalFitRefusesNonfiniteSampleCovariance(t *testing.T) {
	panel := blendFixture{"A": {1e300, -1e300, 1e300}}
	m, err := factormodel.Fit(t.Context(), factormodel.Config{Type: factormodel.Statistical}, []string{"A"}, time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC), factormodel.Providers{Returns: panel})
	if err == nil || m != nil {
		t.Fatalf("unrepresentable covariance returned %v, %v", m, err)
	}
}
