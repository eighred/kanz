package replay

import (
	"context"
	"math/big"
	"testing"
	"time"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	"github.com/eighred/kanz/internal/risk/compute/factor"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/liquidity"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/pricing/structured"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
)

type operationSource struct{ known bool }

func (s *operationSource) Liquidity(context.Context, string, time.Time) (liquidity.LiquiditySpec, bool) {
	return liquidity.LiquiditySpec{ADV: 1000}, s.known
}
func (s *operationSource) AccountMargins(context.Context, v1.PortfolioID) ([]compute.AccountMargin, bool) {
	return []compute.AccountMargin{{Venue: "test", Account: "account", Read: true, CoverageReported: true, Positions: []compute.LiquidationRef{{InstrumentID: "A", Liquidation: big.NewRat(80, 1), Mark: big.NewRat(100, 1)}}}}, s.known
}
func (s *operationSource) Classify(context.Context, string, time.Time) (factor.Classification, bool) {
	return factor.Classification{Sector: factor.Sector{Taxonomy: "TEST", Code: "FIXED_INCOME"}}, s.known
}
func (s *operationSource) Structured(context.Context, string, time.Time) (compute.StructuredSpec, compute.TermsResolution) {
	if !s.known {
		return compute.StructuredSpec{}, compute.TermsUnknown
	}
	return compute.StructuredSpec{
		Deal:   structured.Deal{Pool: structured.Pool{Balance: 1000000, GrossCoupon: .06, ServicingFee: .005, TermMonths: 360}, Tranches: []structured.Tranche{{Name: "A", Balance: 800000, Coupon: .04}, {Name: "B", Balance: 200000, Coupon: .06}}},
		Prepay: structured.Behavioral{Base: .05, Max: .45, Steepness: 40, CDR: .01, Sev: .35}, Currency: "USD", TrancheIndex: 0, OAS: .01,
	}, compute.TermsResolved
}

func TestStructuredLiquidityMarginAndClassificationReplay(t *testing.T) {
	ctx := context.Background()
	source := &operationSource{known: true}
	r := compute.DefaultRegistry()
	c, err := curve.NewZeroCurve([]float64{1, 5, 10, 30}, []float64{.03, .035, .04, .045}, curve.Continuous, curve.LinearZero)
	if err != nil {
		t.Fatal(err)
	}
	compute.RegisterStructuredRisk(ctx, r, compute.StructuredProviders{Terms: Structures{Source: source}, Curve: Curves{Source: &curveSource{value: c}}})
	compute.RegisterLiquidityRisk(ctx, r, Liquidity{Source: source}, liquidity.Model{ParticipationRate: .13, ImpactCoeff: .23}, nil)
	compute.RegisterMarginRisk(ctx, r, Margins{Source: source})
	p := testPortfolio()
	p.SetPosition(domain.Position{InstrumentID: "A", Quantity: &commonpb.Decimal{Coefficient: 100}, MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 950000}}})
	sink := new(recordSink)
	e, err := New(r, sink, "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Compute(ctx, p, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []v1.MeasureName{compute.MeasureStructWAL, compute.MeasureLiquidationHorizon, compute.MeasureLiquidationProximity} {
		m, ok := result.Measures.Lookup(name)
		if !ok || m.Value.GetCoefficient() <= 0 {
			t.Fatalf("fixture failed to price %s", name)
		}
	}
	source.known = false
	if _, err := reconstruct(ctx, sink.record); err != nil {
		t.Fatal(err)
	}
	// Explicit unknowns also survive replay; they may not turn into confident zeros.
	unknown, err := e.Compute(ctx, p, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Measures.UnresolvedMeasures()) == 0 {
		t.Fatal("unknown inputs lost coverage")
	}
	source.known = true
	if _, err := reconstruct(ctx, sink.record); err != nil {
		t.Fatal(err)
	}
}
