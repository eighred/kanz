package execution

import (
	"context"
	"errors"
	"testing"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
)

func d(coeff int64, exp int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coeff, Exponent: exp}
}

func TestSimVenue_FillsLimitInFull(t *testing.T) {
	v := NewSimVenue("XSIM")
	st := &orderpb.OrderState{
		OrderId:        "o1",
		InstrumentId:   "AAPL",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     d(1025, -2),
		LeavesQuantity: d(100, 0),
	}
	fills, err := v.Execute(context.Background(), st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("fills = %d, want 1", len(fills))
	}
	if dec.Cmp(fills[0].GetQuantity(), d(100, 0)) != 0 {
		t.Fatalf("fill qty = %v, want 100", fills[0].GetQuantity())
	}
	if dec.Cmp(fills[0].GetPrice(), d(1025, -2)) != 0 {
		t.Fatalf("fill price = %v, want 10.25", fills[0].GetPrice())
	}
	if fills[0].GetVenue() != "XSIM" {
		t.Fatalf("venue = %q, want XSIM", fills[0].GetVenue())
	}
}

// CONTRACT CHANGE: this used to be TestSimVenue_MarketWithoutPriceRests, and it
// asserted that an unpriceable market order returned (nil, nil) — "no fill" —
// which the OMS could only read as a working order. The order then rested
// FOREVER, indistinguishable from a legitimately working limit order, on the
// capital path, with no per-order signal of any kind.
//
// The condition is PERMANENT: this venue has no price source for this order type
// and will not acquire one at runtime. So it is now an error, and a typed one —
// ErrUnpriced — so the caller can tell "never" from "not yet" and refuse the
// order terminally instead of re-queuing it forever.
func TestSimVenue_MarketWithoutPriceIsRefusedNotRested(t *testing.T) {
	v := NewSimVenue("XSIM") // no price func — exactly how production builds it
	st := &orderpb.OrderState{
		OrderId:        "o2",
		InstrumentId:   "AAPL",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_MARKET,
		LeavesQuantity: d(100, 0),
	}
	fills, err := v.Execute(context.Background(), st)
	if err == nil {
		t.Fatal("Execute returned no error — an order that can NEVER fill must not look like one that is working")
	}
	if !errors.Is(err, ErrUnpriced) {
		t.Fatalf("err = %v, want ErrUnpriced — the caller distinguishes PERMANENT from transient on this sentinel, "+
			"and a plain error would be retried forever", err)
	}
	if len(fills) != 0 {
		t.Fatalf("fills = %d, want 0 alongside the refusal", len(fills))
	}
}

func TestRouter_NoVenue(t *testing.T) {
	r := NewRouter(nil)
	if _, err := r.Route(&orderpb.OrderState{}); err != ErrNoVenue {
		t.Fatalf("err = %v, want ErrNoVenue", err)
	}
}
