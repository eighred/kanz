package execution

import (
	"context"
	"testing"
	"time"

	commonpb "github.com/kanz-eng/kanz-schemas-go/common/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

func simOrder(id string) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        id,
		InstrumentId:   "BTC-USD",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     &commonpb.Decimal{Coefficient: 1025, Exponent: -2},
		LeavesQuantity: &commonpb.Decimal{Coefficient: 100, Exponent: 0},
	}
}

// A venue that has never seen an order must say so AFFIRMATIVELY — that is the
// answer the reconciler needs in order to work the order safely.
func TestSimVenueQueryUnknownOrder(t *testing.T) {
	v := NewSimVenue("XSIM")
	got, err := v.QueryOrder(context.Background(), simOrder("never-sent"))
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewUnknown {
		t.Fatalf("state = %v, want OrderViewUnknown", got.State)
	}
}

// After Execute, the venue must remember the order AND report the very same
// fills — same fill_id — because position_fills dedups on fill_id. A venue that
// renames its fills on re-query double-counts the position book.
func TestSimVenueQueryReturnsTheSameFillsExecuteReported(t *testing.T) {
	ctx := context.Background()
	v := NewSimVenue("XSIM", WithClock(func() time.Time { return time.Unix(0, 0).UTC() }))
	st := simOrder("o-1")

	fills, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("Execute returned %d fills, want 1", len(fills))
	}

	got, err := v.QueryOrder(ctx, st)
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if got.State != OrderViewFilled {
		t.Fatalf("state = %v, want OrderViewFilled", got.State)
	}
	if len(got.Fills) != 1 {
		t.Fatalf("QueryOrder returned %d fills, want 1", len(got.Fills))
	}
	if got.Fills[0].GetFillId() != fills[0].GetFillId() {
		t.Fatalf("QueryOrder fill_id %q != Execute fill_id %q — the position book "+
			"dedups on fill_id, so a re-queried fill under a new id is folded twice",
			got.Fills[0].GetFillId(), fills[0].GetFillId())
	}
}

// A real exchange dedups a resubmitted clOrdId rather than opening a second
// order. The simulator must too, or a re-drive on the sim path trades twice
// while the sim path is exactly what we use to prove the real one.
func TestSimVenueExecuteIsIdempotentPerOrderID(t *testing.T) {
	ctx := context.Background()
	v := NewSimVenue("XSIM")
	st := simOrder("o-1")

	first, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute #1: %v", err)
	}
	second, err := v.Execute(ctx, st)
	if err != nil {
		t.Fatalf("Execute #2: %v", err)
	}
	if len(second) != len(first) {
		t.Fatalf("Execute #2 returned %d fills, #1 returned %d", len(second), len(first))
	}
	if second[0].GetFillId() != first[0].GetFillId() {
		t.Fatalf("Execute #2 minted a new fill_id %q (first was %q) — a resubmitted "+
			"order id must not become a second execution", second[0].GetFillId(), first[0].GetFillId())
	}
}
