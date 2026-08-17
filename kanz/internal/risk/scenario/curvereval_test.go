package scenario

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"

	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/scenario/library"
)

type bondTerms map[string]compute.BondSpec

func (m bondTerms) BondTerms(_ context.Context, id string, _ time.Time) (compute.BondSpec, compute.TermsResolution) {
	s, ok := m[id]
	if !ok {
		return s, compute.TermsNotABond
	}
	return s, compute.TermsResolved
}

type oneCurve struct{ c *curve.Curve }

func (o oneCurve) Curve(_ context.Context, _ string, _ time.Time) (*curve.Curve, bool) {
	return o.c, true
}

func money(amt int64, ccy string) *commonpb.Money {
	return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: amt, Exponent: 0}, CurrencyCode: ccy}
}

func TestEvaluateCurveShift_RepricesBondsRatesUpIsLoss(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	spec := compute.BondSpec{
		Face: 100, CouponRate: 0.04, Frequency: 2,
		Issue: asOf, Maturity: asOf.AddDate(10, 0, 0),
		DayCount: pricing.Thirty360, Currency: "USD",
	}
	c, err := curve.NewZeroCurve([]float64{1, 5, 10}, []float64{0.04, 0.04, 0.04}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	reval := compute.NewBondRevaluer(compute.FIProviders{Terms: bondTerms{"B": spec}, Curve: oneCurve{c: c}})

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "B", Quantity: &commonpb.Decimal{Coefficient: 1000}, MarketValue: money(100000, "USD"), AsOf: asOf})

	base := compute.ComputeMeasures(cloneCarry(p), nil, nil)
	baseNet, _ := base.Lookup(compute.MeasureNetExposure)

	// Rates +100bp ⇒ a 10y bond's MarketValue falls.
	up := library.CurveParallel(100)
	shocked := EvaluateCurveShift(p, up, nil, reval)
	upNet, _ := shocked.Lookup(compute.MeasureNetExposure)

	if !(decFloat(upNet.Value) < decFloat(baseNet.Value)) {
		t.Fatalf("rates-up must lower bond MV: base %.2f shocked %.2f", decFloat(baseNet.Value), decFloat(upNet.Value))
	}

	// Rates −100bp ⇒ MarketValue rises (symmetry of the curve move).
	down := EvaluateCurveShift(p, library.CurveParallel(-100), nil, reval)
	downNet, _ := down.Lookup(compute.MeasureNetExposure)
	if !(decFloat(downNet.Value) > decFloat(baseNet.Value)) {
		t.Fatalf("rates-down must raise bond MV: base %.2f shocked %.2f", decFloat(baseNet.Value), decFloat(downNet.Value))
	}
}

func TestEvaluateCurveShift_NonBondUntouched(t *testing.T) {
	asOf := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	c, _ := curve.NewZeroCurve([]float64{1, 10}, []float64{0.04, 0.04}, curve.Continuous, curve.LinearZero)
	reval := compute.NewBondRevaluer(compute.FIProviders{Terms: bondTerms{}, Curve: oneCurve{c: c}})

	p := domain.NewPortfolio("p1", "USD")
	p.SetPosition(domain.Position{InstrumentID: "EQ", Quantity: &commonpb.Decimal{Coefficient: 10}, MarketValue: money(5000, "USD"), AsOf: asOf})

	base := compute.ComputeMeasures(cloneCarry(p), nil, nil)
	baseNet, _ := base.Lookup(compute.MeasureNetExposure)
	shocked := EvaluateCurveShift(p, library.CurveParallel(100), nil, reval)
	shockedNet, _ := shocked.Lookup(compute.MeasureNetExposure)
	if decFloat(baseNet.Value) != decFloat(shockedNet.Value) {
		t.Fatalf("equity must be untouched by a curve shift: %.2f vs %.2f", decFloat(baseNet.Value), decFloat(shockedNet.Value))
	}
}

func TestNamedCurveShift_Catalog(t *testing.T) {
	for _, name := range library.CurveShiftNames() {
		if _, ok := library.NamedCurveShift(name); !ok {
			t.Fatalf("catalog name %q not resolvable", name)
		}
	}
	if _, ok := library.NamedCurveShift("NOPE"); ok {
		t.Fatal("unknown scenario must not resolve")
	}
}
