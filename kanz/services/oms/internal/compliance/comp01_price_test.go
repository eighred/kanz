package compliance

import (
	"context"
	"math/big"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	marketpb "github.com/kanz-eng/kanz-schemas-go/market/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	comp "github.com/kanz-eng/kanz/internal/compliance"
	"github.com/kanz-eng/kanz/internal/marketdata/mark"
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

	// NON-VACUITY (finding 6): prove the mark's MAGNITUDE reaches the rules, not
	// just that pricing happened at all. 10 units at the stub's 1,000,000 mark is
	// a notional of 10,000,000 against an 80,000 book (~99%) — an implementation
	// off by 1000x (or any other wrong magnitude) would still admit the small-mark
	// case above and pass silently.
	gBig := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(1000000, 1)}))
	breach, err = gBig.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "CONCENTRATION" {
		t.Fatalf("breach = %+v, want CONCENTRATION — a MARKET order valued at a large mark must breach "+
			"the concentration cap, proving the magnitude actually reached the rules", breach)
	}
}

// TestCheck_MarketOrderWithNonRepresentableMarkIsRefused (finding 1, CRITICAL):
// 184467440738 scaled by dec.ToProto's fixed ×10^8 lands just past 2^64, which
// wraps in big.Int.Int64() to a coefficient of 90448384 at exponent -8 — i.e.
// $0.90. Before the fix this order is ADMITTED at that fabricated $0.90 price
// (true notional ~1.8e12, never actually evaluated against the mandate).
// Refusal is correct: we would rather refuse a real order than admit one at a
// wrong price.
func TestCheck_MarketOrderWithNonRepresentableMarkIsRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(184467440738, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — a mark that cannot be represented as a Decimal "+
			"must refuse, not silently wrap to a fabricated price", breach)
	}
}

// TestCheck_MarketOrderWithZeroMarkIsRefused and
// TestCheck_MarketOrderWithNegativeMarkIsRefused (finding 3) pin, in THIS
// package, that a present-but-non-positive mark refuses. That safety today
// comes entirely from decutil.IsPositive in a different package (gate.go:245);
// without these, a future change to Evaluate's guard could flip this gate to
// admitting zero- or negative-priced orders with every existing test here
// still green.
func TestCheck_MarketOrderWithZeroMarkIsRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(0, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — a zero mark must refuse, not admit at zero value", breach)
	}
}

func TestCheck_MarketOrderWithNegativeMarkIsRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(-100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — a negative mark must refuse, not admit at a negative value", breach)
	}
}

// TestCheck_StopOrderIsValuedFromTheMark and
// TestCheck_StopLimitOrderIgnoresTheMark (finding 4): the contract names four
// order types; only MARKET and LIMIT were previously exercised.
func TestCheck_StopOrderIsValuedFromTheMark(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_STOP))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a STOP order with a fresh mark must be valued and admitted", breach)
	}
}

// NON-VACUITY: mirrors TestCheck_LimitOrderIgnoresTheMark. The numbers are
// chosen so the two paths give OPPOSITE verdicts: 10 at a limit of 1.00 is a
// notional of 10 (admitted), while 10 at the stub's 1,000,000 mark is a
// concentration breach. A nil breach here can only mean the limit price was
// used, never the mark.
func TestCheck_StopLimitOrderIgnoresTheMark(t *testing.T) {
	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_STOP_LIMIT)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 100, Exponent: -2} // 1.00

	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(1000000, 1)}))

	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a STOP_LIMIT order must be valued at its own limit price, "+
			"not repriced at the mark", breach)
	}
}

// TestCheck_UnspecifiedOrderTypeIsRefused (finding 5): the gate runs BEFORE
// order validation (services/oms/internal/order/service.go), so an
// ORDER_TYPE_UNSPECIFIED order must not fall through to mark pricing and be
// admitted here — recorded as a compliance PASS in the audit log — only to be
// rejected later by Accept.
func TestCheck_UnspecifiedOrderTypeIsRefused(t *testing.T) {
	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

	breach, err := g.Check(context.Background(), unpricedOrder(orderpb.OrderType_ORDER_TYPE_UNSPECIFIED))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — an unrecognised order type must be refused, "+
			"not mark-priced and admitted ahead of order-type validation", breach)
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

// TestCheck_ExpiredRealMarkIsRefused (finding 3) is the only test that plugs the
// REAL mark.Source into this gate. Every other case here uses stubMarks, which
// has no expiry at all — so the seam between the two packages was untested:
// nothing proved COMP01Gate consults Mark (expiry-aware) rather than Lookup
// (not), the substitution mark.go's own comment warns is "one `if` away", and
// the spec's required "with an expired mark, also refused" case lived only
// inside the mark package and never crossed the seam.
//
// One order, one source, one clock: fresh mark admits it, and the SAME order is
// refused PRICE_UNAVAILABLE once maxAge of local time passes with no new tick.
func TestCheck_ExpiredRealMarkIsRefused(t *testing.T) {
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	now := base
	const maxAge = 30 * time.Second

	src := mark.New(func() time.Time { return now }, maxAge)

	payload, err := proto.Marshal(&marketpb.MarketDataEvent{
		InstrumentId: "AAPL",
		EventTime:    timestamppb.New(base),
		Data: &marketpb.MarketDataEvent_Trade{Trade: &marketpb.Trade{
			Price: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	envelope := &envelopepb.Envelope{EventTime: timestamppb.New(base)}
	if err := src.Handle(context.Background(), envelope, payload); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	g := NewCOMP01Gate(bookedTestGate(t), "USD", WithMarkSource(src))
	cmd := unpricedOrder(orderpb.OrderType_ORDER_TYPE_MARKET)

	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a MARKET order against a FRESH real mark must be valued and admitted", breach)
	}

	// No new tick; only local time moves.
	now = base.Add(maxAge + time.Second)

	breach, err = g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "PRICE_UNAVAILABLE" {
		t.Fatalf("breach = %+v, want PRICE_UNAVAILABLE — the mark is past maxAge with no new tick, so the SAME "+
			"order must now be refused. A gate reading Lookup instead of Mark would still admit it", breach)
	}
}
