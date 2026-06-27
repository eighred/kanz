package compute

import (
	"context"
	"math"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"

	"github.com/kanz-eng/kanz/internal/risk/domain"
	"github.com/kanz-eng/kanz/internal/risk/pricing"
	"github.com/kanz-eng/kanz/internal/risk/pricing/curve"
)

// --- deterministic FI test providers ---------------------------------------

type staticBondTerms map[string]BondSpec

func (m staticBondTerms) BondTerms(_ context.Context, id string, _ time.Time) (BondSpec, bool) {
	s, ok := m[id]
	return s, ok
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
	if d := math.Abs(decimalToFloat(dv01.Value) - wantDV01); d > 0.5 {
		t.Fatalf("portfolio DV01: got %.4f want %.4f", decimalToFloat(dv01.Value), wantDV01)
	}

	// Single bond ⇒ MV-weighted duration is just the bond's effective duration.
	dur, _ := ms.Lookup(MeasureDuration)
	if d := math.Abs(decimalToFloat(dur.Value) - cr.EffectiveDuration); d > 1e-3 {
		t.Fatalf("portfolio Duration: got %.4f want %.4f", decimalToFloat(dur.Value), cr.EffectiveDuration)
	}
	conv, _ := ms.Lookup(MeasureConvexity)
	if decimalToFloat(conv.Value) <= 0 {
		t.Fatalf("Convexity must be positive, got %.4f", decimalToFloat(conv.Value))
	}
	// SpreadDuration equals Duration for a bullet bond.
	sd, _ := ms.Lookup(MeasureSpreadDuration)
	if d := math.Abs(decimalToFloat(sd.Value) - decimalToFloat(dur.Value)); d > 1e-9 {
		t.Fatalf("SpreadDuration should equal Duration for a bullet: %.6f vs %.6f", decimalToFloat(sd.Value), decimalToFloat(dur.Value))
	}
}

func TestRegisterFIRisk_NoBondsIsZero(t *testing.T) {
	r := DefaultRegistry()
	RegisterFIRisk(context.Background(), r, FIProviders{Terms: staticBondTerms{}, Curve: staticCurve{c: fiTestCurve(t)}})
	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: dec(1, 0), MarketValue: &commonpb.Money{Amount: dec(5000, 0), CurrencyCode: "USD"}, AsOf: time.Now()})
	ms := ComputeMeasures(p, r, nil)
	dv01, _ := ms.Lookup(MeasureDV01)
	if decimalToFloat(dv01.Value) != 0 {
		t.Fatalf("DV01 with no bonds must be 0, got %.6f", decimalToFloat(dv01.Value))
	}
}
