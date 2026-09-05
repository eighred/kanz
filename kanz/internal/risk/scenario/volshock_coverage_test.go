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

// A VOL SHOCK THE LINEAR PATH CANNOT EXPRESS MUST BE RECORDED, NOT NO-OPPED
// (#1035).
//
// The shock taxonomy has two classes and the code modelled one. PriceShock,
// ParallelShift and SectorShock are MarketValue arithmetic; a VolShock is not —
// it moves an option's price through vega and has no linear expression at all.
// applyShock's VolShock arm was therefore a comment and nothing else, which made
// it indistinguishable on the wire from "vol stress applied, this book is
// convexity-neutral". The coverage record is the channel that already exists for
// exactly that distinction (#640), and this arm was the one shock kind that
// wrote nothing to it.

// vegaRevaluer stands in for the option pricer this estate does not have: it
// scales a position by the global vol bump so a wired revaluer produces a
// VISIBLY different answer from the unshocked book. The arithmetic is not the
// point — that a Revaluer being present changes the outcome is.
type vegaRevaluer struct{}

func (vegaRevaluer) RevalueOption(_ context.Context, _ string, _ time.Time, baseMV *commonpb.Money, shocks compute.RevalShocks) (*commonpb.Money, bool) {
	if baseMV == nil || shocks.GlobalVolBump == 0 {
		return nil, false
	}
	return compute.ShockMoney(baseMV, pct(int64(shocks.GlobalVolBump*100), -2)), true
}

// TestEvaluate_VolShockWithNoRevaluerIsExcluded is the assertion that makes the
// omission visible. It also pins the SHAPE of the record: one whole-evaluation
// exclusion with an empty instrument id, because the absent revaluer is a
// property of the deployment and not of any holding — the same distinction
// noClassifierWired draws, and the one an operator acts on differently.
func TestEvaluate_VolShockWithNoRevaluerIsExcluded(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "SPX-C-5000", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.VolShock{AbsBump: pct(15, -2)},
	}, nil) // no revaluer, which is every deployment

	if cov.ExcludedCount != 1 || len(cov.Exclusions) != 1 ||
		cov.Exclusions[0].Reason != scenario.SkipNoRevaluer {
		t.Fatalf("coverage=%+v want one %q exclusion — otherwise the caller is handed a clean "+
			"coverage record, which is affirmative evidence the stress ran",
			cov, scenario.SkipNoRevaluer)
	}
	if id := cov.Exclusions[0].InstrumentID; id != "" {
		t.Errorf("InstrumentID=%q want empty — no revaluer is a composition-root gap, and "+
			"recording it per holding would make it look like a reference-data hole", id)
	}
	// The measures are the unshocked book. Asserted rather than assumed, because
	// it is the reason the exclusion has to exist: a clean coverage beside this
	// number is the lie.
	m, _ := got.Lookup(compute.MeasureGrossExposure)
	if v := decToFloat(m.Value); v != 1000 {
		t.Errorf("GrossExposure=%v want 1000 — the linear path cannot reprice on vol, so the "+
			"projection IS the book and only the coverage can say so", v)
	}
}

// TestEvaluate_RepeatedVolShocksRecordOneExclusion pins the dedup. A per-
// underlying vol surface stress is one shock per underlying, and counting each
// would make "0 of N resolved" a multiple of the request rather than a fact
// about the book — the same inflation shockCoverage.seen exists to prevent.
func TestEvaluate_RepeatedVolShocksRecordOneExclusion(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "SPX-C-5000", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.VolShock{UnderlyingID: "SPX", AbsBump: pct(15, -2)},
		v1.VolShock{UnderlyingID: "NDX", AbsBump: pct(12, -2)},
		v1.VolShock{AbsBump: pct(5, -2)},
	}, nil)
	if cov.ExcludedCount != 1 {
		t.Fatalf("ExcludedCount=%d want 1 — three vol shocks are one missing revaluer, not three",
			cov.ExcludedCount)
	}
}

// TestEvaluate_VolShockWithRevaluerIsApplied is the FALSE-REFUSAL arm and the
// half that makes the exclusion conditional rather than a blanket ban on the
// shock kind. WithRevaluer is the seam the eventual RegisterGreeks wiring lands
// on: present ⇒ the vol shock is real and the coverage is clean; absent ⇒ the
// exclusion above. Without this test the fix is satisfied by refusing every vol
// stress forever, which would make the shock kind permanently unaskable.
func TestEvaluate_VolShockWithRevaluerIsApplied(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "SPX-C-5000", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	got, cov := scenario.Evaluate(p, []v1.ScenarioShock{
		v1.VolShock{AbsBump: pct(15, -2)},
	}, nil, scenario.WithRevaluer(vegaRevaluer{}))

	if cov.ExcludedCount != 0 {
		t.Fatalf("coverage=%+v want empty — a wired revaluer prices the vol bump, so there is "+
			"nothing missing", cov)
	}
	m, _ := got.Lookup(compute.MeasureGrossExposure)
	if v := decToFloat(m.Value); v == 1000 {
		t.Errorf("GrossExposure=%v — unchanged, so the revaluer was not consulted and the "+
			"coverage is clean for the wrong reason", v)
	}
}

// TestEvaluateReval_VolShockNeedsNoExclusion holds the other entry point to the
// same rule: EvaluateReval carries a Revaluer by construction, so it must not
// inherit the linear path's exclusion. It is a tracked dark seam, and this is
// the commit the defect would otherwise be reintroduced on.
func TestEvaluateReval_VolShockNeedsNoExclusion(t *testing.T) {
	p := makePortfolio(
		domain.Position{InstrumentID: "SPX-C-5000", MarketValue: money(1000, 0, "USD"), AsOf: baseTime},
	)
	_, cov := scenario.EvaluateReval(p, []v1.ScenarioShock{
		v1.VolShock{AbsBump: pct(15, -2)},
	}, nil, vegaRevaluer{})
	if cov.ExcludedCount != 0 {
		t.Fatalf("coverage=%+v want empty — the reval path is the one that CAN price a vol "+
			"bump", cov)
	}
}
