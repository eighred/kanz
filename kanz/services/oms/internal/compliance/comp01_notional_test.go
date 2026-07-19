package compliance

import (
	"context"
	"math/big"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// qtyOrder is unpricedOrder with an explicit quantity — the notional-overflow
// cases turn entirely on the size of the order.
func qtyOrder(orderType orderpb.OrderType, coeff int64, exp int32) *orderpb.SubmitOrder {
	cmd := unpricedOrder(orderType)
	cmd.Quantity = &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
	return cmd
}

// TestCheck_OversizedMarketOrderCannotWrapIntoAdmission is the CRITICAL repro.
//
// The mark path values a MARKET order via dec.ToProtoScaled, which emits
// exponent -8 here (well inside int64), so a $100 mark arrives with
// coefficient 1e10. The projected
// notional is quantity × price as a raw int64 multiply — at 1,844,674,407
// shares that product is ~1.8e19, which wraps int64 into a small (here
// negative) coefficient. The rules then see a tiny position and ADMIT a true
// notional of ~$184 BILLION against an $80,000 book.
//
// Before the fix this test FAILS with breach == nil (the order is admitted).
func TestCheck_OversizedMarketOrderCannotWrapIntoAdmission(t *testing.T) {
	for _, qty := range []int64{1844674407, 9223372036} {
		g := NewCOMP01Gate(bookedTestGate(t), "USD",
			WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

		breach, err := g.Check(context.Background(), qtyOrder(orderpb.OrderType_ORDER_TYPE_MARKET, qty, 0))
		if err != nil {
			t.Fatalf("qty=%d Check: %v", qty, err)
		}
		// CONCENTRATION specifically: the exact notional reached the rules and
		// the CAP is what refused it — not a valuation failure standing in for
		// a rule that never ran.
		if breach == nil || breach.Code != "CONCENTRATION" {
			t.Fatalf("qty=%d: breach = %+v, want CONCENTRATION — a BUY of %d AAPL at a $100 mark is a notional "+
				"of $%d against an $80,000 book and must NOT be admitted; the notional wrapped int64",
				qty, breach, qty, qty*100)
		}
	}
}

// TestCheck_OversizedLimitOrderCannotWrapIntoAdmission pins the same defect on
// the pre-existing LIMIT path — the fix is in the shared projection
// arithmetic, not in the mark path.
func TestCheck_OversizedLimitOrderCannotWrapIntoAdmission(t *testing.T) {
	cmd := qtyOrder(orderpb.OrderType_ORDER_TYPE_LIMIT, 184467440737095516, 0)
	cmd.LimitPrice = &commonpb.Decimal{Coefficient: 100, Exponent: 0} // $100

	g := NewCOMP01Gate(bookedTestGate(t), "USD")

	breach, err := g.Check(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach == nil || breach.Code != "CONCENTRATION" {
		t.Fatalf("breach = %+v, want CONCENTRATION — an absurdly oversized LIMIT order must not be admitted; "+
			"its notional wrapped int64", breach)
	}
}

// TestCheck_NeighbouringQuantitiesStillBreachConcentration is the NON-VACUITY
// guard: quantities either side of the wrap point must still be refused with
// CONCENTRATION specifically (the cap doing its job on an exact notional), and
// a normal small order must still be ADMITTED. A "fix" that refuses everything
// fails here.
func TestCheck_NeighbouringQuantitiesStillBreachConcentration(t *testing.T) {
	for _, qty := range []int64{1_000_000_000, 2_000_000_000, 5_000_000_000} {
		g := NewCOMP01Gate(bookedTestGate(t), "USD",
			WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))

		breach, err := g.Check(context.Background(), qtyOrder(orderpb.OrderType_ORDER_TYPE_MARKET, qty, 0))
		if err != nil {
			t.Fatalf("qty=%d Check: %v", qty, err)
		}
		if breach == nil || breach.Code != "CONCENTRATION" {
			t.Fatalf("qty=%d: breach = %+v, want CONCENTRATION — the cap must still be what refuses an "+
				"oversized-but-representable order", qty, breach)
		}
	}

	g := NewCOMP01Gate(bookedTestGate(t), "USD",
		WithMarkSource(stubMarks{instrument: "AAPL", price: big.NewRat(100, 1)}))
	breach, err := g.Check(context.Background(), qtyOrder(orderpb.OrderType_ORDER_TYPE_MARKET, 10, 0))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if breach != nil {
		t.Fatalf("breach = %+v, want nil — a normal 10-share order must still be admitted", breach)
	}
}
