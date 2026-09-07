package scenario_test

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/scenario"
)

// THE FULL-REVALUATION PATH HAD THE IDENTICAL DEFECT AND NO TEST AT ALL (#640).
//
// scenario.EvaluateReval resolves each position's sector through sectorFrac,
// which returned 0 for a nil classifier and 0 for an instrument the classifier
// does not know — the same "sector unknown reads as not in this sector"
// inference applySectorShock made on the linear path. EvaluateReval is a tracked
// dark seam (test/arch/no_dark_measure_seam_test.go, blocked on a Revaluer), so
// no caller is served that answer today; these tests exist so the defect is not
// reintroduced on the commit that lights it up, which is the only commit where
// nobody would be looking for it.

// linearOnlyRevaluer reprices nothing, so every position takes EvaluateReval's
// linear fallback. That is enough to drive sectorFrac, which is what carries the
// coverage — a real option pricer would only add arithmetic this is not about.
type linearOnlyRevaluer struct{}

func (linearOnlyRevaluer) RevalueOption(context.Context, string, time.Time, *commonpb.Money, compute.RevalShocks) (*commonpb.Money, bool) {
	return nil, false
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

func TestEvaluateReval_UnknownShockTypeIsRecorded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "A", MarketValue: money(100, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{unknownShock{}}, nil, linearOnlyRevaluer{})
	if cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 ||
		cov.Exclusions[0].Reason != "unknown_shock_type" {
		t.Fatalf("coverage=%+v want one unknown_shock_type exclusion", cov)
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
