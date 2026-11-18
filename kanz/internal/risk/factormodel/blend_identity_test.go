package factormodel_test

import (
	"context"
	"math"
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
