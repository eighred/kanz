package execution

import (
	"context"
	"testing"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/dec"
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

func TestSimVenue_MarketWithoutPriceRests(t *testing.T) {
	v := NewSimVenue("XSIM") // no price func
	st := &orderpb.OrderState{
		OrderId:        "o2",
		InstrumentId:   "AAPL",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_MARKET,
		LeavesQuantity: d(100, 0),
	}
	fills, err := v.Execute(context.Background(), st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 0 {
		t.Fatalf("fills = %d, want 0 (no price ⇒ rests)", len(fills))
	}
}

func TestRouter_NoVenue(t *testing.T) {
	r := NewRouter()
	if _, err := r.Route(&orderpb.OrderState{}); err != ErrNoVenue {
		t.Fatalf("err = %v, want ErrNoVenue", err)
	}
}
