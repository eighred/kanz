package order

import (
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/execution"
)

func routedOrder(ackedAt *timestamppb.Timestamp) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:    "o-1",
		Status:     orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		VenueAckAt: ackedAt,
	}
}

// THE WHOLE POLICY, ONE ROW AT A TIME.
//
// Every row here is a decision about somebody's money. The two that matter most
// are the UNKNOWN pair: the venue saying "I have no such order" means work it if
// we never had an acknowledgement, and means FREEZE if we did — because a venue
// that confirmed an order and now denies it is not a venue we can safely act on,
// and re-driving it is how a fund trades twice.
func TestReconcilePolicy(t *testing.T) {
	acked := timestamppb.New(time.Unix(1000, 0).UTC())

	for _, tc := range []struct {
		name string
		st   *orderpb.OrderState
		view execution.OrderView
		want Action
	}{
		{
			name: "venue never saw it and we never had an ack — work it",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewUnknown},
			want: ActionRedrive,
		},
		{
			name: "venue denies an order it acknowledged — freeze",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewUnknown},
			want: ActionQuarantine,
		},
		{
			name: "venue is working it, no ack recorded — leave it, stamp the ack",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewWorking},
			want: ActionLeave,
		},
		{
			name: "venue is working it, ack recorded — leave it",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewWorking},
			want: ActionLeave,
		},
		{
			name: "venue filled it — adopt its fills",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewFilled},
			want: ActionAdopt,
		},
		{
			name: "venue partially filled it — adopt its fills",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewPartiallyFilled},
			want: ActionAdopt,
		},
		{
			name: "venue rejected it — adopt the rejection",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewRejected},
			want: ActionAdopt,
		},
		// THE FOUR WITHDRAWAL ROWS (#924). Neither verdict branches on the ack —
		// the venue has made a positive statement about an order it held, and the
		// venue is the authority on that — and neither branches on whether fills
		// came with it. An unfilled IOC the venue expired is the population this
		// row exists for and it carries none; one pulled after a partial carries
		// what traded; both ADOPT, and adopt() folds whatever arrived before it
		// writes the terminal status over the remainder.
		{
			name: "venue withdrew it, no ack recorded — adopt the withdrawal",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewCancelled},
			want: ActionAdopt,
		},
		{
			name: "venue withdrew it after a partial — adopt it with the fills",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewCancelled, Fills: []*orderpb.Fill{{FillId: "f-1"}}},
			want: ActionAdopt,
		},
		{
			name: "venue expired an unfilled IOC — adopt it, no human",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewExpired},
			want: ActionAdopt,
		},
		{
			name: "venue expired it after a partial — adopt it with the fills",
			st:   routedOrder(acked),
			view: execution.OrderView{State: execution.OrderViewExpired, Fills: []*orderpb.Fill{{FillId: "f-1"}}},
			want: ActionAdopt,
		},
		{
			name: "nothing was established — freeze",
			st:   routedOrder(nil),
			view: execution.OrderView{State: execution.OrderViewIndeterminate},
			want: ActionQuarantine,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Reconcile(tc.st, tc.view)
			if got != tc.want {
				t.Fatalf("Reconcile = %v, want %v", got, tc.want)
			}
			if got == ActionQuarantine && reason == "" {
				t.Error("quarantine with an empty reason — an operator opening this " +
					"order sees a frozen order and no account of why")
			}
		})
	}
}

// The zero Action must be the one that freezes. A switch that falls through, or
// a caller that forgets to assign, must not produce "send it to the exchange".
func TestZeroActionIsQuarantine(t *testing.T) {
	var a Action
	if a != ActionQuarantine {
		t.Fatalf("zero Action is %v, want ActionQuarantine", a)
	}
}

// A terminal order is nobody's to reconcile. Reaching this function with one is
// a caller bug, and answering "re-drive" would re-trade a filled order.
func TestReconcileRefusesTerminalOrders(t *testing.T) {
	st := &orderpb.OrderState{
		OrderId: "o-1",
		Status:  orderpb.OrderStatus_ORDER_STATUS_FILLED,
	}
	got, reason := Reconcile(st, execution.OrderView{State: execution.OrderViewUnknown})
	if got != ActionQuarantine {
		t.Fatalf("Reconcile on a FILLED order = %v, want ActionQuarantine (got reason %q)", got, reason)
	}
}
