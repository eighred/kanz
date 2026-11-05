package compute

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/pricing"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	referencepb "github.com/eighred/kanz/kanz-schemas-go/reference/v1"
	"google.golang.org/protobuf/proto"
)

func TestFIBondShadowCannotReplaceMeasureOrMutateTerms(t *testing.T) {
	at := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	canonical := &referencepb.BondTerms{FaceValue: &commonpb.Decimal{Coefficient: 1000}}
	spec := BondSpec{Face: 1000, CouponRate: .05, Frequency: 2, Issue: at, Maturity: at.AddDate(5, 0, 0), Currency: "USD", CanonicalTerms: canonical}
	portfolio := domain.NewPortfolio("portfolio", "USD")
	portfolio.SetAggregate(domain.AggregateUpdate{AsOf: at})
	portfolio.SetPosition(domain.Position{InstrumentID: "bond", Quantity: &commonpb.Decimal{Coefficient: 2}, MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: 2000}}})
	p := FIProviders{Terms: staticBondTerms{"bond": spec}, Curve: staticCurve{fiTestCurve(t)}}
	want := fiMeasure(context.Background(), MeasureDV01, p)(portfolio)
	calls := 0
	p.OnBondRisk = func(c BondRiskCapture) {
		calls++
		if c.InstrumentID != "bond" || !c.AsOf.Equal(at) || c.Result.DirtyPrice == 0 || c.Curve == nil {
			t.Fatal("capture lost the actual valuation")
		}
		c.Terms.FaceValue.Coefficient = -1
		c.Result = pricing.CurveRisk{}
		c.Bond.Face = -1
	}
	got := fiMeasure(context.Background(), MeasureDV01, p)(portfolio)
	if calls != 1 || !proto.Equal(got.Value, want.Value) || canonical.FaceValue.Coefficient != 1000 {
		t.Fatal("shadow affected authoritative value or provider terms")
	}
	fiMeasure(context.Background(), MeasureDuration, p)(portfolio)
	fiMeasure(context.Background(), MeasureConvexity, p)(portfolio)
	fiMeasure(context.Background(), MeasureSpreadDuration, p)(portfolio)
	if calls != 1 {
		t.Fatal("one FI evaluation submitted duplicate shadow jobs")
	}
}
