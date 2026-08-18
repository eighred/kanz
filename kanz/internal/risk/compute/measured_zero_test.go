package compute

import (
	"context"
	"strconv"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/xva"
)

// A MEASURED ZERO AND A CONFIDENT ZERO MUST NOT BE THE SAME RESPONSE (#527).
//
// Every measure family in this package resolves per-position or per-snapshot
// inputs through a provider, skips whatever does not resolve, and then returns
// its accumulator regardless. When nothing resolved, that accumulator is zero —
// and zero is the flattering answer for every one of them: no rate risk, no
// counterparty credit risk, no factor risk, no VaR. These tests pin the one
// property that separates the two, family by family.

// --- fixtures -------------------------------------------------------------
//
// staticBondTerms, emptyTermsStore and unusableTermsStore live in fi_test.go
// beside the measures they were written for.

// partialTermsStore knows some instruments and has no record of the rest — the
// realistic mid-migration state, and the one where a measure is genuinely
// partial rather than wholly unmeasured.
type partialTermsStore struct{ known map[string]BondSpec }

func (s partialTermsStore) BondTerms(_ context.Context, id string, _ time.Time) (BondSpec, TermsResolution) {
	if spec, ok := s.known[id]; ok {
		return spec, TermsResolved
	}
	return BondSpec{}, TermsUnknown
}

func zeroCoverageAsOf() time.Time { return time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC) }

func aBondPosition(id domain.InstrumentID, asOf time.Time) domain.Position {
	return domain.Position{
		InstrumentID: id,
		Quantity:     dec(1000, 0),
		MarketValue:  &commonpb.Money{Amount: dec(98000, 0), CurrencyCode: "USD"},
		AsOf:         asOf,
	}
}

func aBondSpec(asOf time.Time) BondSpec {
	return BondSpec{
		Face: 100, CouponRate: 0.05, Frequency: 2,
		Issue: asOf, Maturity: asOf.AddDate(7, 0, 0),
		DayCount: pricing.Thirty360, Currency: "USD",
	}
}

// --- the test this issue exists for --------------------------------------

// A book holding a bond nobody has loaded terms for must not produce the same
// response as a book holding no bonds. Before this, both produced DV01 = 0 with
// nothing else on the response — and since the contract-terms store has no
// production writer, the FIRST of those two is what every portfolio on this
// estate actually gets. A DV01 of zero is an active claim that the book carries
// no interest-rate risk, and it is served with a 200.
//
// The two fixtures differ in exactly ONE thing: whether the terms store can say
// what the instrument is. The position, the quantity, the market value and the
// curve are identical, so any difference in the response comes from the
// coverage and from nothing else.
func TestFIMeasures_AnUnresolvedBondIsNotAnEmptyBook(t *testing.T) {
	asOf := zeroCoverageAsOf()
	book := func(terms BondTermsProvider) *domain.MeasureSet {
		r := DefaultRegistry()
		RegisterFIRisk(context.Background(), r, FIProviders{Terms: terms, Curve: staticCurve{c: fiTestCurve(t)}})
		p := domain.NewPortfolio("p1", "USD")
		p.SetPosition(aBondPosition("BOND_A", asOf))
		return ComputeMeasures(p, r, nil)
	}

	unresolved := book(emptyTermsStore{}) // nothing writes the store (#527)
	notBonds := book(staticBondTerms{})   // a store that positively says "not a bond"

	for _, name := range []v1.MeasureName{MeasureDV01, MeasureDuration, MeasureConvexity, MeasureSpreadDuration} {
		u, ok := unresolved.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the unresolved-terms set", name)
		}
		n, ok := notBonds.Lookup(name)
		if !ok {
			t.Fatalf("%s missing from the no-bonds set", name)
		}

		// THE FIXTURE HAS TO BE THE #527 ONE or this test proves nothing: both
		// values must still be the flattering zero, so that the coverage is the
		// only thing telling them apart.
		if got := decimalToFloat(u.Value); got != 0 {
			t.Fatalf("%s over unresolved terms = %v, want 0 — the fixture is not exercising the "+
				"case this test is about", name, got)
		}
		if got := decimalToFloat(n.Value); got != 0 {
			t.Fatalf("%s over a book of non-bonds = %v, want 0", name, got)
		}

		if u.Coverage.ExcludedCount == 0 {
			t.Errorf("%s: a book whose only bond resolved NO terms reports ExcludedCount=0 — the "+
				"response is indistinguishable from a book holding no bonds, and its zero reads "+
				"as a measured claim of no rate risk (#527)", name)
		}
		if u.Coverage.Contributed != 0 {
			t.Errorf("%s: Contributed=%d, want 0 — nothing priced", name, u.Coverage.Contributed)
		}
		if len(u.Coverage.Exclusions) != 1 ||
			u.Coverage.Exclusions[0].InstrumentID != "BOND_A" ||
			u.Coverage.Exclusions[0].Reason != SkipUnknownInstrument {
			t.Errorf("%s: exclusions = %+v, want exactly BOND_A/%s — a flag with no evidence "+
				"cannot tell an operator which holding to go and look at",
				name, u.Coverage.Exclusions, SkipUnknownInstrument)
		}

		// AND THE CONFIDENT ZERO STAYS CONFIDENT. A store that knows the book holds
		// no bonds must not be flagged, or every response on the estate carries the
		// flag and it stops meaning anything.
		if n.Coverage.ExcludedCount != 0 {
			t.Errorf("%s: a store that positively identifies every position as a non-bond "+
				"reported %d exclusion(s) %+v — a share is correctly absent from a bond measure, "+
				"and flagging it would bury the bonds that really were dropped",
				name, n.Coverage.ExcludedCount, n.Coverage.Exclusions)
		}
	}
}

// A BOND WHOSE TERMS ARE PRESENT AND UNUSABLE IS ITS OWN REASON, and it is the
// one that reaches OnSkip.
//
// SkipNoTerms used to mean "never loaded OR unusable" and was never emitted by
// anything — nothing in fi.go passed it to the observer, so the series sat at
// zero while every bond on the estate fell out. The two are different repairs
// (load the data vs fix the record), so they are different labels.
func TestFIMeasures_UnusableTermsAreReportedSeparatelyFromUnknownOnes(t *testing.T) {
	asOf := zeroCoverageAsOf()
	run := func(terms BondTermsProvider) (v1.Measure, []string) {
		var skipped []string
		r := DefaultRegistry()
		RegisterFIRisk(context.Background(), r, FIProviders{
			Terms:  terms,
			Curve:  staticCurve{c: fiTestCurve(t)},
			OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
		})
		p := domain.NewPortfolio("p1", "USD")
		p.SetPosition(aBondPosition("BOND_A", asOf))
		m, _ := ComputeMeasures(p, r, []v1.MeasureName{MeasureDV01}).Lookup(MeasureDV01)
		return m, skipped
	}

	m, skipped := run(unusableTermsStore{})
	if len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipNoTerms {
		t.Errorf("unusable terms recorded %+v, want reason %s", m.Coverage.Exclusions, SkipNoTerms)
	}
	if len(skipped) != 1 || skipped[0] != "BOND_A:"+SkipNoTerms {
		t.Errorf("OnSkip saw %v, want [BOND_A:%s] — the constant was declared in #509 and never "+
			"once emitted, so the counter read zero while bonds fell out", skipped, SkipNoTerms)
	}

	m, skipped = run(emptyTermsStore{})
	if len(m.Coverage.Exclusions) != 1 || m.Coverage.Exclusions[0].Reason != SkipUnknownInstrument {
		t.Errorf("an unknown instrument recorded %+v, want reason %s",
			m.Coverage.Exclusions, SkipUnknownInstrument)
	}
	if len(skipped) != 0 {
		t.Errorf("OnSkip fired %v for an instrument with no terms record — that case is already "+
			"counted by the terms provider's own missing-terms observer, and counting it twice "+
			"puts one fact in two metrics that can then disagree", skipped)
	}
}

// A PARTIALLY-PRICED BOOK KEEPS ITS NUMBER AND SAYS SO — the case omitting the
// measure could not have expressed.
//
// Two bonds, one priceable. DV01 is a real, useful figure computed over half the
// book's rate risk. Dropping the measure would throw that away; serving it
// unmarked is #527. It is served, and it is marked.
func TestFIMeasures_APartiallyPricedBookKeepsItsNumberAndSaysSo(t *testing.T) {
	asOf := zeroCoverageAsOf()
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, FIProviders{
		Terms: partialTermsStore{known: map[string]BondSpec{"BOND_A": aBondSpec(asOf)}},
		Curve: staticCurve{c: fiTestCurve(t)},
	})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(aBondPosition("BOND_A", asOf))
	p.SetPosition(aBondPosition("BOND_B", asOf))

	dv01, _ := ComputeMeasures(p, r, nil).Lookup(MeasureDV01)
	if decimalToFloat(dv01.Value) == 0 {
		t.Fatal("DV01 = 0 over a book with one priceable bond — the fixture is wrong")
	}
	if dv01.Coverage.Contributed != 1 || dv01.Coverage.ExcludedCount != 1 {
		t.Errorf("coverage = %+v, want Contributed=1 ExcludedCount=1 — a half-measured book must "+
			"report BOTH halves, or 'we measured nothing' and 'we measured most of it' collapse "+
			"into one signal", dv01.Coverage)
	}
}

// THE EVIDENCE IS BOUNDED AND THE COUNT IS NOT, or a book whose reference data
// was never loaded puts every one of its positions into every measure on every
// response, and into the degraded cache behind it.
func TestInputCoverage_TheExclusionSampleIsCappedButTheCountIsNot(t *testing.T) {
	asOf := zeroCoverageAsOf()
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, FIProviders{Terms: emptyTermsStore{}, Curve: staticCurve{c: fiTestCurve(t)}})

	p := domain.NewPortfolio("p1", "USD")
	const n = v1.MaxInputExclusions * 3
	for i := 0; i < n; i++ {
		p.SetPosition(aBondPosition(domain.InstrumentID("I"+strconv.Itoa(i)), asOf))
	}
	dv01, _ := ComputeMeasures(p, r, nil).Lookup(MeasureDV01)

	if dv01.Coverage.ExcludedCount != n {
		t.Errorf("ExcludedCount = %d, want %d — the COUNT must be exact; it is the magnitude an "+
			"operator reads", dv01.Coverage.ExcludedCount, n)
	}
	if len(dv01.Coverage.Exclusions) != v1.MaxInputExclusions {
		t.Errorf("sampled %d exclusions, want the cap of %d — an unbounded sample puts the whole "+
			"book in every response and in the degraded cache",
			len(dv01.Coverage.Exclusions), v1.MaxInputExclusions)
	}
}

// --- the same property, the other families -------------------------------

// A FACTOR VaR OF ZERO WITH NO MODEL IS NOT A FLAT BOOK.
//
// The whole book's factor risk vanishes at once here, so the exclusion belongs
// to the evaluation rather than to any holding — one entry, empty instrument id.
func TestFactorMeasures_NoModelIsAWholeBookExclusion(t *testing.T) {
	r := DefaultRegistry()
	RegisterFactorRisk(context.Background(), r, FactorProviders{Model: nil})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(aBondPosition("EQ", zeroCoverageAsOf()))

	m, ok := ComputeMeasures(p, r, nil).Lookup(MeasureFactorVaR99)
	if !ok {
		t.Fatal("FactorVaR99 missing")
	}
	if decimalToFloat(m.Value) != 0 {
		t.Fatalf("FactorVaR99 = %v with no model, want 0 — fixture wrong", decimalToFloat(m.Value))
	}
	if m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 {
		t.Fatalf("coverage = %+v, want exactly one whole-book exclusion — inflating it to the "+
			"position count would make an outage look like a per-instrument data gap", m.Coverage)
	}
	ex := m.Coverage.Exclusions[0]
	if ex.InstrumentID != "" || ex.Reason != SkipNoModel {
		t.Errorf("exclusion = %+v, want an empty instrument id with reason %s", ex, SkipNoModel)
	}
}

// LIQUIDITY IS THE ONE THAT IS LIVE (#509).
//
// The other three families this file pins were unregistered when #527 landed, so
// their confident zeros were a schedule rather than an exposure. LiquidationHorizon
// is REGISTERED IN PRODUCTION — risk-engine wires it whenever a liquidity venue and
// a bar store are configured — and it returned a bare zero with no coverage at all.
//
// The counter was there and was not enough, which is the exact argument fi.go
// already makes about its own seam: "OnSkip is a counter an operator watches
// across all portfolios and only sees if they are looking; the InputCoverage this
// measure attaches to its own value travels WITH the number, to the one caller
// acting on that one portfolio, at the moment they act."
//
// A HORIZON OF ZERO DAYS IS THE FLATTERING DIRECTION. It says the book unwinds
// instantly. Served over a book where nothing resolved, it is indistinguishable
// from a flat book — and it is a number somebody sizes a position against.
func TestLiquidityMeasures_NothingResolvedIsAMarkedZero(t *testing.T) {
	p, _ := liqTestBook(t)

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, staticLiquidity{}, liquidity.DefaultModel(), nil)
	h, ok := ComputeMeasures(p, r, []v1.MeasureName{MeasureLiquidationHorizon}).Lookup(MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon missing")
	}
	// THE FIXTURE HAS TO KEEP SERVING THE ZERO, exactly as the FI case does, so
	// that the coverage is the only thing distinguishing it from a flat book.
	if got := decimalToFloat(h.Value); got != 0 {
		t.Fatalf("horizon = %v, want 0 — the premise of this test is that the zero is served", got)
	}
	if h.Coverage.ExcludedCount == 0 {
		t.Errorf("a book where NOTHING resolved reports ExcludedCount=0 — the response is " +
			"indistinguishable from a book that genuinely unwinds instantly, and this measure is " +
			"registered in production today (#509/#527)")
	}
	if len(h.Coverage.Exclusions) != 1 {
		t.Fatalf("exclusions = %+v, want exactly one whole-book entry — one per position would "+
			"make a total outage look like a scatter of per-instrument gaps", h.Coverage.Exclusions)
	}
	if ex := h.Coverage.Exclusions[0]; ex.InstrumentID != "" || ex.Reason != SkipNoLiquidHorizon {
		t.Errorf("exclusion = %+v, want an empty instrument id with reason %s", ex, SkipNoLiquidHorizon)
	}
}

// AND A FLAT BOOK'S ZERO STAYS CONFIDENT. A book with no positions really does
// unwind instantly; flagging that would train an operator to scroll past the
// flag that matters.
func TestLiquidityMeasures_AnEmptyBookIsAConfidentZero(t *testing.T) {
	p := domain.NewPortfolio("empty", "USD")

	r := DefaultRegistry()
	RegisterLiquidityRisk(context.Background(), r, staticLiquidity{}, liquidity.DefaultModel(), nil)
	h, ok := ComputeMeasures(p, r, []v1.MeasureName{MeasureLiquidationHorizon}).Lookup(MeasureLiquidationHorizon)
	if !ok {
		t.Fatal("LiquidationHorizon missing")
	}
	if h.Coverage.ExcludedCount != 0 {
		t.Errorf("an empty book reports ExcludedCount=%d, want 0 — crying wolf here drowns the "+
			"signal the case above depends on", h.Coverage.ExcludedCount)
	}
}

// A CVA OF ZERO IS THE MOST FLATTERING NUMBER THIS ENGINE CAN PRINT.
//
// XVA is unregistered today, which is a schedule rather than a safeguard: the
// shape #527 documents for FI was already sitting in xva.go waiting for the
// wiring. A nil provider is covered by the same assertion because that path used
// to PANIC — this seam was the only one of the four that dereferenced without a
// check.
func TestXVAMeasures_NoExposuresIsAMarkedZero(t *testing.T) {
	for name, provider := range map[string]XVAProvider{
		"nil provider":      nil,
		"provider declines": decliningXVA{},
	} {
		r := DefaultRegistry()
		RegisterXVA(context.Background(), r, provider)
		p := domain.NewPortfolio("p1", "USD")
		p.SetPosition(aBondPosition("SWAP", zeroCoverageAsOf()))

		m, ok := ComputeMeasures(p, r, nil).Lookup(MeasureCVA)
		if !ok {
			t.Fatalf("%s: CVA missing", name)
		}
		if decimalToFloat(m.Value) != 0 {
			t.Fatalf("%s: CVA = %v, want 0 — fixture wrong", name, decimalToFloat(m.Value))
		}
		if m.Coverage.ExcludedCount != 1 || len(m.Coverage.Exclusions) != 1 ||
			m.Coverage.Exclusions[0].Reason != SkipNoExposures {
			t.Errorf("%s: coverage = %+v, want one whole-evaluation exclusion with reason %s — a "+
				"CVA of zero says the book carries no counterparty default risk at all",
				name, m.Coverage, SkipNoExposures)
		}
	}
}

type decliningXVA struct{}

func (decliningXVA) Exposures(context.Context, time.Time) ([]xva.Adjustments, bool) {
	return nil, false
}
