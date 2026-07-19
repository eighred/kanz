package compliance

import (
	"context"
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	comp "github.com/kanz-eng/kanz/internal/compliance"
)

// stubMarks is a MarkSource returning a fixed price for one instrument.
type stubMarks struct {
	instrument string
	price      *big.Rat
}

func (s stubMarks) Mark(instrument string) *big.Rat {
	if instrument == s.instrument && s.price != nil {
		return new(big.Rat).Set(s.price)
	}
	return nil
}

// bookedTestGate is newTestGate with a NON-EMPTY book, so a successfully valued
// order has something to be a small fraction OF. Without the existing positions
// every priced order is 100% of the portfolio and breaches the concentration
// cap, which would look like a pricing failure and is not one.
func bookedTestGate(t *testing.T) *comp.PreTradeGate {
	t.Helper()
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
	return comp.NewPreTradeGate(nil, books, reg, nil, nil, nil)
}

func TestCheck_MarketOrderIsValuedFromTheMark(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a MARKET order with a fresh mark must be valued and admitted", breach)
	}
}

func TestCheck_MarketOrderWithNoMarkIsStillRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "SOMETHING-ELSE", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — an order we cannot value must still be refused. "+
			"This adds a price, not a bypass", breach)
	}
}

func TestCheck_MarketOrderWithNoSourceWiredIsRefusedAsBefore(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD") // no WithMarkSource

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — with no source wired the behaviour must be exactly COMP-M1's", breach)
	}
}

// NON-VACUITY: a LIMIT order must value from ITS OWN limit price and never
// consult the mark. Without this, an implementation that always used the mark
// would pass every test above while silently repricing every limit order.
//
// The numbers are chosen so the two paths give OPPOSITE verdicts: 10 at a limit
// of 1.00 is a notional of 10 (~0.01% of the book, admitted), while 10 at the
// stub's 1,000,000 mark is 10,000,000 (~99% of the book, a concentration
// breach). A nil breach here can only mean the limit price was used.
func TestCheck_LimitOrderIgnoresTheMark(t *testing.T) {
	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_LIMIT)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 100, Exponent: -2} // 1.00

	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(1000000, 1)}))

	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a LIMIT order must be valued at its own limit price, "+
			"not repriced at the mark", breach)
	}
}
