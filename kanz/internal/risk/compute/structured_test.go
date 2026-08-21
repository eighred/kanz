package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
)

// staticStructured answers TermsResolved for the ids it holds and TermsUnknown
// for everything else — a store that holds NO record for the rest of the book,
// which is the estate as it stands (no production writer for contract_terms).
// The "a record exists and is not a securitization" answer has its own fixture
// below, because after #572 widened this seam those two are exactly what it
// exists to keep apart.
type staticStructured map[string]StructuredSpec

func (m staticStructured) Structured(_ context.Context, id string, _ time.Time) (StructuredSpec, TermsResolution) {
	s, ok := m[id]
	if !ok {
		return StructuredSpec{}, TermsUnknown
	}
	return s, TermsResolved
}

// certifyingStore holds a record for every instrument and says none of them is a
// securitization — the TermsOtherVariant arm, which #572 made reachable and
// which is the ONLY answer that licenses a silent absence.
type certifyingStore struct{}

func (certifyingStore) Structured(context.Context, string, time.Time) (StructuredSpec, TermsResolution) {
	return StructuredSpec{}, TermsOtherVariant
}

// unusableDealStore holds a STRUCTURED record for every id that cannot be turned
// into a spec — a held_tranche naming no tranche, a BEHAVIORAL model whose
// parameters the schema does not carry, an absent quoted OAS.
type unusableDealStore struct{}

func (unusableDealStore) Structured(context.Context, string, time.Time) (StructuredSpec, TermsResolution) {
	return StructuredSpec{}, TermsUnusable
}

// structProviders bundles a terms fixture with the test curve. Written once so
// no test can accidentally register with a nil Curve and attribute the resulting
// exclusions to the terms half.
func structProviders(t *testing.T, terms StructuredProvider) StructuredProviders {
	t.Helper()
	return StructuredProviders{Terms: terms, Curve: staticCurve{c: structTestCurve(t)}}
}

func structTestCurve(t *testing.T) *curve.Curve {
	t.Helper()
	c, err := curve.NewZeroCurve([]float64{1, 5, 10, 30}, []float64{0.03, 0.035, 0.04, 0.045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func structTestSpec(t *testing.T) StructuredSpec {
	t.Helper()
	return StructuredSpec{
		Deal: structured.Deal{
			Pool:     structured.Pool{Balance: 1_000_000, GrossCoupon: 0.06, ServicingFee: 0.005, TermMonths: 360},
			Tranches: []structured.Tranche{{Name: "A", Balance: 800_000, Coupon: 0.04}, {Name: "B", Balance: 200_000, Coupon: 0.06}},
		},
		Prepay:       structured.Behavioral{Base: 0.05, Max: 0.45, Steepness: 40, CDR: 0.01, Sev: 0.35},
		Currency:     "USD",
		TrancheIndex: 0,
		OAS:          0.01,
	}
}

func TestRegisterStructuredRisk(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, structProviders(t, staticStructured{"MBS_A": structTestSpec(t)}))

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MBS_A", MarketValue: &commonpb.Money{Amount: dec(950_000, 0), CurrencyCode: "USD"}})
	p.SetPosition(domain.Position{InstrumentID: "EQ", MarketValue: &commonpb.Money{Amount: dec(50_000, 0), CurrencyCode: "USD"}}) // non-structured

	ms := ComputeMeasures(p, r, nil)
	dur, ok := ms.Lookup(MeasureStructDuration)
	if !ok || decutil.Float64Or(dur.Value, 0) <= 0 {
		t.Fatalf("StructDuration must be positive, got %.4f ok=%v", decutil.Float64Or(dur.Value, 0), ok)
	}
	wal, _ := ms.Lookup(MeasureStructWAL)
	if decutil.Float64Or(wal.Value, 0) <= 0 {
		t.Fatalf("StructWAL must be positive, got %.4f", decutil.Float64Or(wal.Value, 0))
	}
	// THE COVERAGE OF A GOOD ANSWER MATTERS TOO (#572): the priced tranche is
	// counted, and the equity beside it is reported as UNASSESSED rather than
	// silently dropped — because THIS fixture's store holds no record for it, so
	// nothing has certified it as a non-securitization. A store that DOES hold a
	// record for it reports it silently instead
	// (TestStructMeasures_ACertifiedNonSecuritizationIsASilentAbsence), and the
	// difference between the two is the whole reason this seam was widened.
	if dur.Coverage.Contributed != 1 {
		t.Errorf("Contributed=%d, want 1 (MBS_A priced)", dur.Coverage.Contributed)
	}
	if dur.Coverage.ExcludedCount != 1 || len(dur.Coverage.Exclusions) != 1 ||
		dur.Coverage.Exclusions[0].InstrumentID != "EQ" {
		t.Errorf("coverage = %+v, want EQ excluded", dur.Coverage)
	}
}

// A BOOK THE PROVIDER DECLINES ENTIRELY MUST NOT LOOK LIKE A BOOK WITH NO
// STRUCTURED RISK (#572, the #527 property for this family).
//
// The value is still zero — see QualityFlagInputsUnresolved on why the measure
// is annotated rather than omitted — so the coverage is the ONLY thing that
// tells the two apart, and before this change there was no coverage at all.
//
// Renamed from TestRegisterStructuredRisk_NonStructuredIsZero, whose name
// asserted the defect: with no schema, no store and no terms.Kind able to
// describe a structured product, a provider answering false has not established
// that the position is non-structured. Nothing on this estate can.
func TestRegisterStructuredRisk_ADeclinedBookIsNotAnEmptyBook(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, structProviders(t, staticStructured{}))
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", MarketValue: &commonpb.Money{Amount: dec(50_000, 0), CurrencyCode: "USD"}})
	ms := ComputeMeasures(p, r, nil)

	for _, name := range []v1.MeasureName{MeasureStructDuration, MeasureStructConvexity, MeasureStructWAL} {
		m, ok := ms.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the set", name)
		}
		// THE FIXTURE HAS TO STILL PRODUCE THE ZERO or this test proves nothing:
		// the coverage must be what separates the two responses.
		if got := decutil.Float64Or(m.Value, 0); got != 0 {
			t.Fatalf("%s = %.4f, want 0 — the fixture is not exercising the case this test is about", name, got)
		}
		if m.Coverage.Contributed != 0 {
			t.Errorf("%s: Contributed=%d, want 0 — nothing was priced", name, m.Coverage.Contributed)
		}
		if m.Coverage.ExcludedCount != 1 {
			t.Errorf("%s: ExcludedCount=%d, want 1 — a position the provider could not resolve is "+
				"an exclusion, because nothing in this estate can certify it as non-structured (#572)",
				name, m.Coverage.ExcludedCount)
		}
		if len(m.Coverage.Exclusions) != 1 ||
			m.Coverage.Exclusions[0].InstrumentID != "EQ" ||
			m.Coverage.Exclusions[0].Reason != SkipUnknownInstrument {
			t.Errorf("%s: exclusions = %+v, want one EQ:%s", name, m.Coverage.Exclusions, SkipUnknownInstrument)
		}
	}
}

// AN UNPRICEABLE POSITION MUST LEAVE THE AVERAGE, NOT DRAG IT DOWN (#572).
//
// This is the sharpest form of the hazard and the reason the pricing layer
// returns ErrUnpriceable instead of zero. Two structured positions of EQUAL
// market value: one resolves and prices, the other resolves and carries a rate
// environment with no discount curve. Folding the second in at zero — which is
// exactly what `val, _ := EffectiveRisk(...)` did, since the p0==0 guard
// returned (0, 0) with no error — HALVES the reported duration of the book, and
// the response says nothing about it.
//
// The assertion is on the number, not only on the coverage, because a coverage
// annotation on a halved average is still a halved average.
func TestStructMeasures_AnUnpriceableSpecIsExcludedRatherThanFoldedInAtZero(t *testing.T) {
	good := structTestSpec(t)
	// A TRANCHE THE DEAL DOES NOT CONTAIN. The spec resolves, the curve resolves,
	// and the pricer still refuses — structured.ErrUnpriceable out of
	// Projection.Tranche. It stands in for every way a resolved spec fails to
	// price, and it is the arm that used to answer (0, 0) with no error.
	unpriceable := structTestSpec(t)
	unpriceable.TrancheIndex = 9

	book := func(specs staticStructured, ids ...string) v1.Measure {
		r := DefaultRegistry()
		RegisterStructuredRisk(context.Background(), r, structProviders(t, specs))
		p := domain.NewPortfolio("p1", "USD")
		for _, id := range ids {
			p.SetPosition(domain.Position{
				InstrumentID: domain.InstrumentID(id),
				MarketValue:  &commonpb.Money{Amount: dec(1_000_000, 0), CurrencyCode: "USD"},
			})
		}
		ms := ComputeMeasures(p, r, nil)
		dur, ok := ms.Lookup(MeasureStructDuration)
		if !ok {
			t.Fatalf("StructDuration missing")
		}
		return dur
	}

	alone := book(staticStructured{"MBS_A": good}, "MBS_A")
	mixed := book(staticStructured{"MBS_A": good, "MBS_B": unpriceable}, "MBS_A", "MBS_B")

	want := decutil.Float64Or(alone.Value, 0)
	if want <= 0 {
		t.Fatalf("the priceable fixture must have a positive duration, got %.4f", want)
	}
	if got := decutil.Float64Or(mixed.Value, 0); got != want {
		t.Errorf("StructDuration over one priceable and one unpriceable tranche = %.4f, want %.4f "+
			"(the priceable one alone). A tranche the pricer refused was folded into the "+
			"|MV|-weighted average at zero, which halves the book's measured rate risk and reports "+
			"it with a 200 (#572).", got, want)
	}
	if mixed.Coverage.Contributed != 1 {
		t.Errorf("Contributed=%d, want 1", mixed.Coverage.Contributed)
	}
	if mixed.Coverage.ExcludedCount != 1 {
		t.Errorf("ExcludedCount=%d, want 1 — the refused tranche must be named on the response",
			mixed.Coverage.ExcludedCount)
	}
	if len(mixed.Coverage.Exclusions) != 1 ||
		mixed.Coverage.Exclusions[0].InstrumentID != "MBS_B" ||
		mixed.Coverage.Exclusions[0].Reason != SkipUnpriceableTranche {
		t.Errorf("exclusions = %+v, want one MBS_B:%s", mixed.Coverage.Exclusions, SkipUnpriceableTranche)
	}
}

// ONE MALFORMED SPEC USED TO PRODUCE TWO DIFFERENT FAILURES (#572): StructWAL
// indexed Projection.Tranches directly and panicked, taking the whole
// ComputeMeasures call — every measure, not just this family — with it, while
// StructDuration returned a silent 0.0 for the same spec. Now both refuse and
// both say so.
func TestStructMeasures_ATrancheTheDealDoesNotHaveIsRefusedByEveryMeasure(t *testing.T) {
	spec := structTestSpec(t)
	spec.TrancheIndex = 7 // the deal has two

	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, structProviders(t, staticStructured{"MBS_A": spec}))
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "MBS_A", MarketValue: &commonpb.Money{Amount: dec(950_000, 0), CurrencyCode: "USD"}})

	ms := ComputeMeasures(p, r, nil) // must not panic
	for _, name := range []v1.MeasureName{MeasureStructDuration, MeasureStructConvexity, MeasureStructWAL} {
		m, ok := ms.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the set", name)
		}
		if m.Coverage.ExcludedCount != 1 || m.Coverage.Contributed != 0 {
			t.Errorf("%s: coverage = %+v, want one exclusion and nothing contributed", name, m.Coverage)
		}
		if len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipUnpriceableTranche {
			t.Errorf("%s: exclusions = %+v, want %s", name, m.Coverage.Exclusions, SkipUnpriceableTranche)
		}
	}
}

// A REGISTRATION WITH NO PROVIDER REPORTS THE WHOLE BOOK UNASSESSED (#572), the
// stance positionBondRisk takes for the same case — and it no longer panics on
// the first position, which is what a nil interface used to do.
func TestStructMeasures_NoProviderExcludesEveryPosition(t *testing.T) {
	r := DefaultRegistry()
	RegisterStructuredRisk(context.Background(), r, StructuredProviders{})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "A", MarketValue: &commonpb.Money{Amount: dec(1000, 0), CurrencyCode: "USD"}})
	p.SetPosition(domain.Position{InstrumentID: "B", MarketValue: &commonpb.Money{Amount: dec(2000, 0), CurrencyCode: "USD"}})

	ms := ComputeMeasures(p, r, nil)
	dur, _ := ms.Lookup(MeasureStructDuration)
	if dur.Coverage.ExcludedCount != 2 {
		t.Errorf("ExcludedCount=%d, want 2 — with no provider the engine has certified nothing",
			dur.Coverage.ExcludedCount)
	}
}
