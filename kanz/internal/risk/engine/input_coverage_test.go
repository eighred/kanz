package engine_test

import (
	"context"
	"testing"
	"time"

	risk "github.com/eighred/kanz/internal/risk"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/state"
)

// #527 AT THE SURFACE A CALLER ACTUALLY READS.
//
// A portfolio whose bond terms nobody has loaded used to come back as a
// well-formed MeasuresResponse in ModeNormal with zero quality flags and DV01 =
// 0 — the same response as a book holding no bonds. The pre-trade gate, the
// compliance monitor and the web app all read that as the fund's rate risk.
// These pin that the response now declares its own incompleteness, and that it
// does NOT declare it when the book really was measured.

// unknownTerms is the estate as it stands: a contract-terms store nothing
// writes, which can say nothing about any instrument.
type unknownTerms struct{}

func (unknownTerms) BondTerms(context.Context, string, time.Time) (compute.BondSpec, compute.TermsResolution) {
	return compute.BondSpec{}, compute.TermsUnknown
}

// knownNonBond is the same store once reference data exists and positively
// identifies the book's holdings as something other than bonds.
type knownNonBond struct{}

func (knownNonBond) BondTerms(context.Context, string, time.Time) (compute.BondSpec, compute.TermsResolution) {
	return compute.BondSpec{}, compute.TermsNotABond
}

type noCurve struct{}

func (noCurve) Curve(context.Context, string, time.Time) (*curve.Curve, bool) { return nil, false }

func newFIEngine(terms compute.BondTermsProvider) (*engine.EngineImpl, *state.Store) {
	s := state.NewStore()
	r := compute.DefaultRegistry()
	compute.RegisterFIRisk(context.Background(), r, compute.FIProviders{Terms: terms, Curve: noCurve{}})
	return engine.New(s, r, risk.NewCache(), risk.NewDetector()), s
}

func TestMeasures_UnresolvedBondTermsAreFlaggedOnTheResponse(t *testing.T) {
	e, s := newFIEngine(unknownTerms{})
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "GOVT-10Y", "USD", 98000, fresh)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if !hasFlag(resp.QualityFlags, v1.QualityFlagInputsUnresolved) {
		t.Fatalf("QualityFlags=%v, missing %s\n\n"+
			"Every FI measure on this response was computed over nothing, and DV01 came back as "+
			"zero. Without the flag that response is byte-identical to a book holding no bonds, "+
			"and the zero is an active claim of no interest-rate risk (#527).",
			resp.QualityFlags, v1.QualityFlagInputsUnresolved)
	}
	if hasFlag(resp.QualityFlags, v1.QualityFlagCurrencyExcluded) {
		t.Errorf("QualityFlags=%v — the book is single-currency; INPUTS_UNRESOLVED and "+
			"CURRENCY_EXCLUDED are different findings and only one of them is somebody's bug",
			resp.QualityFlags)
	}
	if hasFlag(resp.QualityFlags, v1.QualityFlagStale) || hasFlag(resp.QualityFlags, v1.QualityFlagDegraded) {
		t.Errorf("QualityFlags=%v — the state is fresh and the engine is healthy; this flag is "+
			"about coverage, not freshness", resp.QualityFlags)
	}

	// THE MEASURE IS STILL THERE, and it carries its own evidence. That is the
	// half of the decision omission would have thrown away: the caller can see
	// WHICH holding the engine could not price, without a second round trip.
	m, ok := resp.Set.Lookup(compute.MeasureDV01)
	if !ok {
		t.Fatal("DV01 is absent from the response — the decision was to FLAG the measure, not " +
			"to omit it; an absent measure is indistinguishable from one this engine does not " +
			"compute at all, which is the ambiguity #509 is about")
	}
	if len(m.Coverage.Exclusions) == 0 || m.Coverage.Exclusions[0].InstrumentID != "GOVT-10Y" {
		t.Errorf("DV01 coverage = %+v, want the excluded holding named", m.Coverage)
	}
}

// AND A BOOK THE ENGINE REALLY DID MEASURE IS NOT FLAGGED.
//
// Without this, the test above is satisfied by attaching the flag to every
// response — which would make it meaningless and train every reader to ignore
// it. Same positions, same measures, same zero DV01; the only difference is that
// the store can say these are not bonds.
func TestMeasures_AConfidentZeroIsNotFlagged(t *testing.T) {
	e, s := newFIEngine(knownNonBond{})
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "AAPL", "USD", 98000, fresh)

	resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1"})
	if err != nil {
		t.Fatalf("Measures: %v", err)
	}
	if hasFlag(resp.QualityFlags, v1.QualityFlagInputsUnresolved) {
		t.Errorf("QualityFlags=%v — every position was positively identified as a non-bond, so "+
			"DV01 = 0 is a measured answer. Flagging it would put the flag on every response on "+
			"the estate, and a signal that is always on is one nobody reads", resp.QualityFlags)
	}
}

// THE FLAG SURVIVES A MEASURE FILTER, but only for the measures the filter kept.
//
// Narrowing the response must not launder a partial number into a clean one.
// Equally, asking only for GrossExposure — which resolves no per-position inputs
// — must not inherit the FI family's gap, or the flag stops meaning anything
// about the numbers actually returned.
func TestMeasures_TheFlagFollowsTheFilteredSubset(t *testing.T) {
	e, s := newFIEngine(unknownTerms{})
	fresh := time.Now().Add(-1 * time.Second)
	applyPositionCcy(t, s, "PORT-1", "GOVT-10Y", "USD", 98000, fresh)

	only := func(names ...v1.MeasureName) []v1.QualityFlag {
		resp, err := e.Measures(context.Background(), v1.MeasuresRequest{PortfolioID: "PORT-1", Measures: names})
		if err != nil {
			t.Fatalf("Measures: %v", err)
		}
		return resp.QualityFlags
	}

	if f := only(compute.MeasureDV01); !hasFlag(f, v1.QualityFlagInputsUnresolved) {
		t.Errorf("asking only for DV01 returned flags=%v — narrowing the response stripped the "+
			"fact that the number is unmeasured", f)
	}
	if f := only(compute.MeasureGrossExposure); hasFlag(f, v1.QualityFlagInputsUnresolved) {
		t.Errorf("asking only for GrossExposure returned flags=%v — that measure resolves no "+
			"providers and was computed over the whole book; carrying another family's gap onto "+
			"it makes the flag say nothing about the number beside it", f)
	}
}
