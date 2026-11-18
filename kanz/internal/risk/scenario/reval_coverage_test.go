package scenario_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/dec"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
)

// THE FULL-REVALUATION PATH HAD THE IDENTICAL DEFECT AND NO TEST AT ALL (#640).
//
// scenario.EvaluateReval formerly returned a zero sector adjustment both for a
// nil classifier and for an instrument the classifier did not know. The
// result made the same "sector unknown reads as not in this sector"
// inference applySectorShock made on the linear path. EvaluateReval is a tracked
// dark seam (test/arch/no_dark_measure_seam_test.go, blocked on a Revaluer), so
// no caller is served that answer today; these tests exist so the defect is not
// reintroduced on the commit that lights it up, which is the only commit where
// nobody would be looking for it.

// linearOnlyRevaluer reprices nothing, so every position takes EvaluateReval's
// linear fallback. That is enough to drive sector resolution, which carries the
// coverage — a real option pricer would only add arithmetic this is not about.
type linearOnlyRevaluer struct{}

func (linearOnlyRevaluer) RevalueOption(context.Context, string, time.Time, *commonpb.Money, compute.RevalShocks) compute.RevalResult {
	return compute.RevalResult{Status: compute.RevalKnownLinear}
}

func TestEvaluateReval_SectorShockNoClassifierIsRecorded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "TECH", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, linearOnlyRevaluer{}) // no classifier
	if cov.ExcludedCount != 1 {
		t.Fatalf("ExcludedCount=%d want 1 — the reval path must record an unappliable sector "+
			"shock exactly as the linear path does", cov.ExcludedCount)
	}
	if cov.Exclusions[0].Reason != scenario.SkipNoClassifier {
		t.Errorf("reason=%q want %q", cov.Exclusions[0].Reason, scenario.SkipNoClassifier)
	}
	if id := cov.Exclusions[0].InstrumentID; id != "" {
		t.Errorf("InstrumentID=%q want empty — one whole-evaluation gap, not one per holding "+
			"(this book has two)", id)
	}
}

func TestEvaluateReval_UnclassifiedInstrumentIsRecorded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "MYSTERY", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	c := classifierFor(map[string]string{"BANK": "40"})
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-50, -2)},
	}, nil, linearOnlyRevaluer{}, scenario.WithClassifier(c))
	if cov.ExcludedCount != 1 || cov.Contributed != 1 {
		t.Fatalf("coverage=%+v want ExcludedCount=1 (MYSTERY), Contributed=1 (BANK)", cov)
	}
	if cov.Exclusions[0].InstrumentID != "MYSTERY" ||
		cov.Exclusions[0].Reason != scenario.SkipUnclassified {
		t.Fatalf("Exclusions=%+v want MYSTERY/%s", cov.Exclusions, scenario.SkipUnclassified)
	}
}

// TestEvaluateReval_PriceOnlyScenarioNeedsNoClassifier is the false-refusal arm:
// a vol/price revaluation consults no sector, so it must come back with an empty
// coverage on a deployment with no classifier — which is every deployment.
func TestEvaluateReval_PriceOnlyScenarioNeedsNoClassifier(t *testing.T) {
	p := makePortfolio(domain.Position{InstrumentID: "BANK", MarketValue: money(1000, 0, "USD"), AsOf: baseTime})
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(-20, -2)},
		v1.VolShock{AbsBump: pct(15, -2)},
	}, nil, linearOnlyRevaluer{})
	if cov.ExcludedCount != 0 {
		t.Fatalf("coverage=%+v want empty — no shock here resolves a sector", cov)
	}
}

type partiallyAssessedRevaluer struct{}

func (partiallyAssessedRevaluer) RevalueOption(_ context.Context, id string, _ time.Time, _ *commonpb.Money, _ compute.RevalShocks) compute.RevalResult {
	if id == "MYSTERY" {
		return compute.RevalResult{Reason: compute.SkipNoTerms}
	}
	return compute.RevalResult{Status: compute.RevalKnownLinear}
}

func TestEvaluateReval_UnassessedValuationOverridesResolvedSector(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "BANK", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
		domain.Position{InstrumentID: "MYSTERY", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	for _, knownSector := range []bool{true, false} {
		sectors := map[string]string{"BANK": "40"}
		if knownSector {
			sectors["MYSTERY"] = "40"
		}
		_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-20, -2)}}, nil, partiallyAssessedRevaluer{}, scenario.WithClassifier(classifierFor(sectors)))
		if cov.ExcludedCount != 1 || cov.Contributed != 1 || len(cov.Exclusions) != 1 || cov.Exclusions[0].InstrumentID != "MYSTERY" {
			t.Fatalf("sector-known=%v: one position must remain one exclusion: %+v", knownSector, cov)
		}
		if knownSector && cov.Exclusions[0].Reason != compute.SkipNoTerms {
			t.Fatalf("sector resolution hid unavailable terms: %+v", cov)
		}
	}
}

func TestKnownLinearRevaluationCompoundsExactDecimalShocks(t *testing.T) {
	p := makePortfolio(domain.Position{InstrumentID: "A", MarketValue: money(10000000, 0, "USD"), AsOf: baseTime})
	classifier := classifierFor(map[string]string{"A": "40"})
	for _, tc := range []struct {
		name   string
		shocks []v1.ScenarioShock
		want   *big.Rat
	}{
		{"sub micro", []v1.ScenarioShock{v1.PriceShock{InstrumentID: "A", Pct: pct(1, -7)}}, big.NewRat(10000001, 1)},
		// 10,000,000 × 11/10 × 4/5 × 10000001/10000000.
		{"overlap", []v1.ScenarioShock{
			v1.ParallelShift{Pct: pct(1, -1)}, v1.PriceShock{InstrumentID: "other", Pct: pct(1, 0)},
			v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-2, -1)}, v1.SectorShock{Taxonomy: "GICS", Code: "other", Pct: pct(2, 0)},
			v1.PriceShock{InstrumentID: "A", Pct: pct(1, -7)},
		}, big.NewRat(220000022, 25)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms, cov := scenario.EvaluateReval(p, tc.shocks, nil, linearOnlyRevaluer{}, scenario.WithClassifier(classifier))
			if ms == nil || cov.ExcludedCount != 0 {
				t.Fatalf("coverage: %+v", cov)
			}
			gross, ok := ms.Lookup(compute.MeasureGrossExposure)
			if !ok || gross.Value == nil || dec.FromProto(gross.Value).Cmp(tc.want) != 0 {
				t.Fatalf("gross %v, want %s USD", gross.Value, tc.want)
			}
			original, _ := p.Position("A")
			if original.MarketValue.Amount.Coefficient != 10000000 {
				t.Fatal("changed original book")
			}
		})
	}
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{v1.ParallelShift{Pct: pct(1, -30)}}, nil, linearOnlyRevaluer{})
	if cov.ExcludedCount != 1 || cov.Exclusions[0].InstrumentID != "A" {
		t.Fatalf("unrepresentable stress: %+v", cov)
	}
}

type optionShockRecorder struct{ shocks compute.RevalShocks }

func (r *optionShockRecorder) RevalueOption(_ context.Context, _ string, _ time.Time, m *commonpb.Money, s compute.RevalShocks) compute.RevalResult {
	r.shocks = s
	return compute.RevalResult{Status: compute.RevalPriced, Value: m}
}
func TestPricedOptionsKeepTheirAdditivePricingInputs(t *testing.T) {
	p := makePortfolio(domain.Position{InstrumentID: "A", MarketValue: money(25, 0, "USD"), AsOf: baseTime})
	reval := new(optionShockRecorder)
	measures, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(25, -2)}, v1.PriceShock{InstrumentID: "A", Pct: pct(125, -3)},
		v1.SectorShock{Taxonomy: "GICS", Code: "40", Pct: pct(-5, -1)}, v1.VolShock{AbsBump: pct(125, -3)}, v1.VolShock{UnderlyingID: "A", AbsBump: pct(25, -2)},
	}, nil, reval, scenario.WithClassifier(classifierFor(map[string]string{"A": "40"})))
	if measures == nil || cov.ExcludedCount != 0 || reval.shocks.PriceFrac != -.125 || reval.shocks.GlobalVolBump != .125 || reval.shocks.VolByUnderlying["A"] != .25 {
		t.Fatalf("option inputs: %+v, coverage %+v", reval.shocks, cov)
	}
}

func TestUnassessedOptionKeepsItsExcludedAdditiveApproximation(t *testing.T) {
	p := makePortfolio(domain.Position{InstrumentID: "MYSTERY", MarketValue: money(100, 0, "USD"), AsOf: baseTime})
	ms, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.ParallelShift{Pct: pct(1, -1)},
		v1.PriceShock{InstrumentID: "MYSTERY", Pct: pct(-2, -1)},
	}, nil, partiallyAssessedRevaluer{})
	if ms == nil || cov.Contributed != 0 || cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 || cov.Exclusions[0].Reason != compute.SkipNoTerms {
		t.Fatalf("coverage: %+v", cov)
	}
	m, ok := ms.Lookup(compute.MeasureGrossExposure)
	// The legacy estimate is 100 * (1 + 0.1 - 0.2) in six-place USD.
	if !ok || m.Value.GetCoefficient() != 90000000 || m.Value.GetExponent() != -6 {
		t.Fatalf("excluded approximation: %v", m.Value)
	}
}
