package compliance

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/kanz-eng/kanz/internal/compliance"
)

// concentrationMandate is a mandate with one rule — enough to make the gate
// GOVERNED, so an unpriced order actually reaches the price check instead of
// being shadowed by Ungoverned (a mandate with zero rules also shadows it,
// which is correct and covered at the comp package level).
func concentrationMandate(maxPct int64) *compliancepb.Mandate {
	return &compliancepb.Mandate{
		MandateId: "m1", TenantId: "t1", PortfolioId: "p1", Version: 1,
		EffectiveAt: timestamppb.New(time.Unix(0, 0)),
		Rules: []*compliancepb.Rule{{
			RuleId: "c1", Type: compliancepb.RuleType_RULE_TYPE_CONCENTRATION,
			Params: &compliancepb.Rule_Concentration{Concentration: &compliancepb.ConcentrationLimit{
				Dimension: compliancepb.Dimension_DIMENSION_INSTRUMENT,
				MaxWeight: &commonpb.Decimal{Coefficient: maxPct, Exponent: -2},
			}},
		}},
	}
}

func newTestGate(t *testing.T) *comp.PreTradeGate {
	t.Helper()
	reg := comp.NewMandateRegistry()
	reg.Put(concentrationMandate(60))
	return comp.NewPreTradeGate(nil, comp.MapBookSource{}, reg, nil, nil, nil)
}

// unpricedOrder builds a SubmitOrder of the given type with no limit/stop
// price set — exactly what a MARKET or STOP order looks like on the wire.
func unpricedOrder(orderType orderpb.OrderType) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		Metadata:     &commandpb.CommandMetadata{Issuer: "trader@desk"},
		OrderId:      "o1",
		PortfolioId:  "p1",
		InstrumentId: "AAPL",
		Side:         orderpb.Side_SIDE_BUY,
		Quantity:     &commonpb.Decimal{Coefficient: 10, Exponent: 0},
		OrderType:    orderType,
	}
}

// TestCheck_UnpricedMarketOrderMapsToPriceUnavailableBreach drives an unpriced
// MARKET SubmitOrder end to end through Check (COMP-M1 Task 2): the gate's
// Unpriced decision must map to a terminal PRICE_UNAVAILABLE breach, not a
// rule code and not an error (an error would retry forever — the price will
// never appear on retry).
func TestCheck_UnpricedMarketOrderMapsToPriceUnavailableBreach(t *testing.T) {
	g := NewCOMP01Gate(newTestGate(t), "USD")
	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("an unpriced order must be a terminal Breach, not an error (a retry can never add a price): %v", err)
	}
	if breach == nil {
		t.Fatal("an unpriced MARKET order must be refused, not admitted")
	}
	if breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("want Code=PRICE_UNAVAILABLE, got %+v", breach)
	}
	if breach.Reason == "" {
		t.Fatal("the reason must name the instrument and say the order cannot be valued")
	}
}

// TestCheck_UnpricedStopOrderMapsToPriceUnavailableBreach: STOP orders have
// the same nil-limit-price hole as MARKET orders (aggregate.go only requires
// a positive stop_price, not a limit_price).
func TestCheck_UnpricedStopOrderMapsToPriceUnavailableBreach(t *testing.T) {
	g := NewCOMP01Gate(newTestGate(t), "USD")
	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_STOP))
	if err != nil {
		t.Fatal(err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("an unpriced STOP order must map to PRICE_UNAVAILABLE, got %+v", breach)
	}
}

// TestCheck_PricedLimitOrderUnaffected pins that a normal limit order still
// evaluates and is admitted exactly as before — this task removes a bypass,
// it does not retune any rule. The book already holds a large MSFT position so
// a small AAPL buy stays comfortably under the 60% concentration cap.
func TestCheck_PricedLimitOrderUnaffected(t *testing.T) {
	reg := comp.NewMandateRegistry()
	reg.Put(concentrationMandate(60))
	books := comp.MapBookSource{"p1": &comp.Book{
		PortfolioID: "p1", BaseCurrency: "USD",
		Positions: []comp.Position{
			{
				InstrumentID: "MSFT",
				Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
				MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 40000, Exponent: 0}, CurrencyCode: "USD"},
			},
			{
				InstrumentID: "GOOG",
				Quantity:     &commonpb.Decimal{Coefficient: 100, Exponent: 0},
				MarketValue:  &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: 40000, Exponent: 0}, CurrencyCode: "USD"},
			},
		},
	}}
	g := NewCOMP01Gate(comp.NewPreTradeGate(nil, books, reg, nil, nil, nil), "USD")

	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_LIMIT)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 100, Exponent: 0} // 10 units @ 100 = 1,000
	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if breach != nil {
		t.Fatalf("a small compliant limit order must be admitted, got breach %+v", breach)
	}
}
