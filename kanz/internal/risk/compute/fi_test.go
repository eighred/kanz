package compute

import (
	"context"
	decutil "github.com/eighred/kanz/internal/dec"
	"math"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// --- deterministic FI test providers ---------------------------------------

// staticBondTerms answers TermsResolved for the ids it holds and TermsOtherVariant
// for everything else — a store that KNOWS the rest of the book is not bonds.
// The unknown-instrument case (a store that holds no record) has its own fixture
// below, because after #527 those two are the answers this seam exists to keep
// apart.
type staticBondTerms map[string]BondSpec

func (m staticBondTerms) BondTerms(_ context.Context, id string, _ time.Time) (BondSpec, TermsResolution) {
	s, ok := m[id]
	if !ok {
		return BondSpec{}, TermsOtherVariant
	}
	return s, TermsResolved
}

// emptyTermsStore is the estate as it actually stands (#527): a contract-terms
// store with no production writer, which holds no record for ANY instrument and
// can therefore certify nothing about any of them.
type emptyTermsStore struct{}

func (emptyTermsStore) BondTerms(context.Context, string, time.Time) (BondSpec, TermsResolution) {
	return BondSpec{}, TermsUnknown
}

// unusableTermsStore holds a bond record for every id that cannot be priced from
// — the TermsUnusable arm.
type unusableTermsStore struct{}

func (unusableTermsStore) BondTerms(context.Context, string, time.Time) (BondSpec, TermsResolution) {
	return BondSpec{}, TermsUnusable
}

type staticCurve struct{ c *curve.Curve }

func (s staticCurve) Curve(_ context.Context, _ string, _ time.Time) (*curve.Curve, bool) {
	return s.c, s.c != nil
}

func fiTestCurve(t *testing.T) *curve.Curve {
	t.Helper()
	c, err := curve.NewZeroCurve([]float64{1, 2, 3, 5, 10}, []float64{0.03, 0.033, 0.036, 0.04, 0.045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegisterFIRisk_PortfolioAggregation(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	spec := BondSpec{
		Face: 100, CouponRate: 0.05, Frequency: 2,
		Issue: asOf, Maturity: asOf.AddDate(7, 0, 0),
		DayCount: pricing.Thirty360, Currency: "USD",
	}
	c := fiTestCurve(t)
	providers := FIProviders{Terms: staticBondTerms{"BOND_A": spec}, Curve: staticCurve{c: c}}

	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, providers)

	p := domain.NewPortfolio("p1", "USD")
	const qty = 1000.0
	p.SetPosition(domain.Position{InstrumentID: "BOND_A", Quantity: dec(int64(qty), 0), MarketValue: &commonpb.Money{Amount: dec(98000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	// A non-bond position contributes nothing to the FI measures.
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: dec(10, 0), MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: asOf})

	ms := ComputeMeasures(p, r, nil)

	bond := pricing.Bond{Face: spec.Face, CouponRate: spec.CouponRate, Frequency: spec.Frequency, Issue: spec.Issue, Maturity: spec.Maturity, DayCount: spec.DayCount}
	cr := bond.CurveRisk(asOf, c)

	dv01, ok := ms.Lookup(MeasureDV01)
	if !ok {
		t.Fatal("DV01 measure missing")
	}
	wantDV01 := cr.DV01 * qty
	if d := math.Abs(decutil.Float64Or(dv01.Value, 0) - wantDV01); d > 0.5 {
		t.Fatalf("portfolio DV01: got %.4f want %.4f", decutil.Float64Or(dv01.Value, 0), wantDV01)
	}

	// Single bond ⇒ MV-weighted duration is just the bond's effective duration.
	dur, _ := ms.Lookup(MeasureDuration)
	if d := math.Abs(decutil.Float64Or(dur.Value, 0) - cr.EffectiveDuration); d > 1e-3 {
		t.Fatalf("portfolio Duration: got %.4f want %.4f", decutil.Float64Or(dur.Value, 0), cr.EffectiveDuration)
	}
	conv, _ := ms.Lookup(MeasureConvexity)
	if decutil.Float64Or(conv.Value, 0) <= 0 {
		t.Fatalf("Convexity must be positive, got %.4f", decutil.Float64Or(conv.Value, 0))
	}
	// SpreadDuration equals Duration for a bullet bond.
	sd, _ := ms.Lookup(MeasureSpreadDuration)
	if d := math.Abs(decutil.Float64Or(sd.Value, 0) - decutil.Float64Or(dur.Value, 0)); d > 1e-9 {
		t.Fatalf("SpreadDuration should equal Duration for a bullet: %.6f vs %.6f", decutil.Float64Or(sd.Value, 0), decutil.Float64Or(dur.Value, 0))
	}
}

func TestRegisterFIRisk_NoBondsIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, FIProviders{Terms: staticBondTerms{}, Curve: staticCurve{c: fiTestCurve(t)}})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: dec(1, 0), MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: time.Now()})
	ms := ComputeMeasures(p, r, nil)
	dv01, _ := ms.Lookup(MeasureDV01)
	if decutil.Float64Or(dv01.Value, 0) != 0 {
		t.Fatalf("DV01 with no bonds must be 0, got %.6f", decutil.Float64Or(dv01.Value, 0))
	}
}

// A BOND DROPPED FOR WANT OF A CURVE IS REPORTED, NOT SILENT (#509).
//
// This is the failure the observer exists for. The bond is known — its terms
// resolve — and no discount curve exists for its currency, so it contributes 0
// to DV01 and nothing to the duration average. The book's measured rate risk
// SHRINKS, and a DV01 of zero is indistinguishable from a portfolio holding no
// bonds at all.
func TestRegisterFIRisk_ABondWithNoCurveIsReported(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	spec := BondSpec{
		Face: 100, CouponRate: 0.05, Frequency: 2,
		Issue: asOf, Maturity: asOf.AddDate(7, 0, 0),
		DayCount: pricing.Thirty360, Currency: "USD",
	}
	var skipped []string
	providers := FIProviders{
		Terms: staticBondTerms{"BOND_A": spec},
		Curve: staticCurve{c: nil}, // calibration never ran for USD
		OnSkip: func(id, reason string) {
			skipped = append(skipped, id+":"+reason)
		},
	}
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, providers)

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "BOND_A", Quantity: dec(1000, 0),
		MarketValue: &commonpb.Money{Amount: dec(98000, 0), CurrencyCode: "USD"}, AsOf: asOf})

	ms := ComputeMeasures(p, r, nil)
	dv01, _ := ms.Lookup(MeasureDV01)
	if got := decutil.Float64Or(dv01.Value, 0); got != 0 {
		t.Fatalf("DV01 = %v with no curve, want 0 — the fixture is not exercising the skip", got)
	}
	if len(skipped) == 0 {
		t.Fatal("a bond was excluded from every FI measure and nothing was reported — the " +
			"portfolio's DV01 fell to zero, which reads exactly like a book holding no bonds")
	}
	for _, s := range skipped {
		if s != "BOND_A:"+SkipNoCurve {
			t.Errorf("reported %q, want BOND_A:%s", s, SkipNoCurve)
		}
	}
}

// A NON-BOND IS NOT REPORTED, or the signal drowns in every equity on the book.
//
// Without this the test above is satisfied by an observer that fires for every
// position, which would make the counter useless and the alert unbuildable.
func TestRegisterFIRisk_ANonBondIsNotReportedAsSkipped(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	var skipped []string
	providers := FIProviders{
		Terms:  staticBondTerms{}, // resolves nothing: every position is a non-bond
		Curve:  staticCurve{c: fiTestCurve(t)},
		OnSkip: func(id, reason string) { skipped = append(skipped, id+":"+reason) },
	}
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, providers)

	p := domain.NewPortfolio("p1", "USD")
	for _, id := range []string{"EQ_A", "EQ_B", "EQ_C"} {
		p.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), Quantity: dec(10, 0),
			MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: asOf})
	}
	ComputeMeasures(p, r, nil)

	if len(skipped) != 0 {
		t.Errorf("a share book reported %d FI skips: %v — a non-bond is correctly absent from a "+
			"bond measure, and counting it would bury the bonds that really were dropped",
			len(skipped), skipped)
	}
}
