package varmodel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
)

// THE TAIL MEASURES HAD NO OBSERVABILITY OF ANY KIND (#527).
//
// Every other measure family in the estate has at least a skip observer behind
// its zero. This one returned a bare zero from zeroNamed on every path where the
// distribution could not be built — no counter, no observer, no flag — and it is
// REGISTERED IN PRODUCTION, overriding the placeholder VaR99 whenever a market
// data URL is configured. A VaR of zero is the most flattering number a risk
// engine can print, and it was indistinguishable from a genuinely riskless book.

// noReturns has nothing for anybody: the price store is reachable and empty.
type noReturns struct{}

func (noReturns) Returns(context.Context, string, time.Time, int) ([]float64, error) {
	return nil, errors.New("no series")
}

// oneKnownInstrument serves a return series for exactly one id and fails for the
// rest — the realistic partial state.
type oneKnownInstrument struct {
	id  string
	ret []float64
}

func (o oneKnownInstrument) Returns(_ context.Context, id string, _ time.Time, _ int) ([]float64, error) {
	if id == o.id {
		return o.ret, nil
	}
	return nil, errors.New("no series")
}

// A ZERO WITH NO DISTRIBUTION BEHIND IT IS MARKED, ACROSS ALL FOUR MEASURES.
//
// All four read the same portfolioPnL, so all four returned the same unmarked
// zero. They are asserted together because the failure was shared: fixing one
// and not the others would leave three measures claiming a flat book.
func TestTailMeasures_AnUnmeasurableBookIsNotAFlatOne(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})

	for _, tc := range []struct {
		name    v1.MeasureName
		measure compute.ReturnsMeasure
	}{
		{compute.MeasureVaR99, varmodel.Historical(varmodel.Config{})},
		{compute.MeasureES99, varmodel.ExpectedShortfall(varmodel.Config{})},
		{compute.MeasureMaxDrawdown, varmodel.MaxDrawdownFraction(varmodel.Config{})},
		{compute.MeasureMaxDrawdownAmount, varmodel.MaxDrawdownAmount(varmodel.Config{})},
		{compute.MeasureVaR99, varmodel.MonteCarlo(varmodel.Config{})},
	} {
		m := tc.measure(context.Background(), p, noReturns{})
		if got := dval(m.Value); got != 0 {
			t.Fatalf("%s = %v with no return series, want 0 — the fixture is not exercising the "+
				"unmeasurable case", tc.name, got)
		}
		if m.Coverage.ExcludedCount == 0 {
			t.Errorf("%s reported ExcludedCount=0 over a book it could build no distribution for "+
				"— that zero is byte-identical to a riskless portfolio, and this family is "+
				"registered in production", tc.name)
			continue
		}
		// The position itself is excluded (no series), AND the evaluation is
		// excluded (no distribution). Both are real and they are different
		// repairs: load that instrument's history vs. this book cannot be
		// measured at all.
		var sawWhole bool
		for _, ex := range m.Coverage.Exclusions {
			if ex.InstrumentID == "" && ex.Reason == varmodel.SkipInsufficientHistory {
				sawWhole = true
			}
		}
		if !sawWhole {
			t.Errorf("%s: exclusions = %+v, want one whole-evaluation entry with reason %s",
				tc.name, m.Coverage.Exclusions, varmodel.SkipInsufficientHistory)
		}
	}
}

// A LEG DROPPED FOR WANT OF HISTORY IS NAMED, AND THE MEASURE STILL ANSWERS.
//
// This is the case where the direction of the error is NOT downward: the dropped
// leg took its offset with it. Two positions of equal size and opposite sign
// have zero net P&L under a common return series; drop one and the remaining
// side's risk is reported in full. Serving that number without saying a leg is
// missing is worse than serving nothing.
func TestTailMeasures_ADroppedLegIsNamedAndTheNumberStillAnswers(t *testing.T) {
	p := portfolio("USD",
		domain.Position{InstrumentID: "LONG", MarketValue: money(1000, "USD")},
		domain.Position{InstrumentID: "SHORT", MarketValue: money(-1000, "USD")},
	)
	prov := oneKnownInstrument{id: "LONG", ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	m := varmodel.Historical(varmodel.Config{})(context.Background(), p, prov)
	if got := dval(m.Value); got == 0 {
		t.Fatalf("VaR99 = 0 with one priced leg — the fixture is wrong")
	}
	if m.Coverage.Contributed != 1 || m.Coverage.ExcludedCount != 1 {
		t.Fatalf("coverage = %+v, want Contributed=1 ExcludedCount=1", m.Coverage)
	}
	ex := m.Coverage.Exclusions[0]
	if ex.InstrumentID != "SHORT" || ex.Reason != varmodel.SkipNoReturns {
		t.Errorf("exclusion = %+v, want SHORT/%s — the hedge is the thing that went missing, and "+
			"a VaR that no longer knows about it reads HIGHER, not lower",
			ex, varmodel.SkipNoReturns)
	}
}

// A FULLY-PRICED BOOK IS NOT FLAGGED, or every response carries the flag and it
// stops distinguishing anything. Without this the tests above are satisfied by
// an implementation that excludes unconditionally.
func TestTailMeasures_AFullyPricedBookReportsNoExclusions(t *testing.T) {
	p := portfolio("USD", domain.Position{InstrumentID: "AAPL", MarketValue: money(1000, "USD")})
	prov := fixedProvider{ret: []float64{-0.10, -0.05, 0, 0.05, 0.10}}

	m := varmodel.Historical(varmodel.Config{})(context.Background(), p, prov)
	if m.Coverage.ExcludedCount != 0 {
		t.Errorf("a fully-priced book reported %d exclusion(s) %+v", m.Coverage.ExcludedCount, m.Coverage.Exclusions)
	}
	if m.Coverage.Contributed != 1 {
		t.Errorf("Contributed = %d, want 1", m.Coverage.Contributed)
	}
}
