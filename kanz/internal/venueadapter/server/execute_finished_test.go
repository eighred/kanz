package server

// A RE-DISPATCHED Execute IS REFUSED FOR AN ORDER THE VENUE ALREADY FINISHED (#914).
//
// Two properties, and the second is the one that keeps the first honest:
//
//  1. the exchange is never asked to place an order this adapter's view holds at
//     a terminal status, and the view is left exactly as the venue's own report
//     left it — still terminal, still out of Open;
//  2. a re-dispatch of an order that is STILL WORKING is untouched. Re-working an
//     order is legitimate, and a guard that refused those would turn a recovery
//     path into a trading outage — a worse defect than the one it closes.

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// filled is the order as the venue's own fill report leaves it in the view —
// the state orderview.Progress writes on an execution report.
func filled() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	return st
}

// redispatched is what the OMS sends: its own record of the order, which has not
// caught up with the venue and still says the order is being routed.
func redispatched() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	return st
}

func TestExecuteRefusesAnOrderTheVenueHasAlreadyFinished(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, filled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}

	_, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: redispatched()})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("re-dispatching a FILLED order returned %v, want AlreadyExists — the adapter is "+
			"the last line before the exchange, and placing an order the venue has already "+
			"finished trades the fund twice", err)
	}
	if v.execCalls != 0 {
		t.Fatalf("the exchange was asked to place the order %d times despite the refusal — the "+
			"refusal has to precede the venue call, not follow it", v.execCalls)
	}
}

// THE VIEW SURVIVES THE RE-DISPATCH, which is the half #914 was filed for: the
// order must still be terminal and must still be out of the reconciler's set.
func TestARedispatchLeavesAFilledOrderTerminalAndOutOfOpen(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, filled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}

	_, _ = s.Execute(ctx, &venuepb.ExecuteRequest{State: redispatched()})

	st, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the view has ORD-1 at %v after a re-dispatch, want FILLED — the OMS's stale "+
			"status overwrote the venue's own verdict", st.GetStatus())
	}
	open, err := view.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the view believes %d orders are still open at the venue after a filled order was "+
			"re-dispatched, want 0 — the reconciler would re-query it and re-emit StateHealed "+
			"about it on every pass, which is the leak #904 closed", len(open))
	}
}

// THE ONE THAT MUST NOT REGRESS. An order still working at the venue is
// re-dispatched by the sweep and by a redelivery, and both are legitimate: the
// record has to refresh, and the venue has to be asked to work it.
func TestExecuteStillWorksAndRefreshesAnOrderThatIsStillOpen(t *testing.T) {
	ctx := context.Background()
	working := order()
	working.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED

	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, working); err != nil {
		t.Fatalf("seed the view: %v", err)
	}

	refreshed, rok := proto.Clone(working).(*orderpb.OrderState)
	if !rok {
		t.Fatal("clone")
	}
	refreshed.VenueAccountId = "binance-main"
	if _, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: refreshed}); err != nil {
		t.Fatalf("re-dispatching an order that is still working was refused: %v — re-working an "+
			"interrupted order is how this platform recovers one, and refusing it is a trading "+
			"outage, not a safety guard", err)
	}
	if v.execCalls != 1 {
		t.Fatalf("the venue was called %d times for a live order, want 1", v.execCalls)
	}
	st, _, err := view.Get(ctx, "ORD-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.GetVenueAccountId() != "binance-main" {
		t.Fatal("the re-dispatched state did not refresh the view — Record is an upsert for a live " +
			"order, and an adapter working an order off a stale record enriches its fills wrong")
	}
}

// EVERY TERMINAL STATUS REFUSES, AND EVERY OTHER ONE DOES NOT — and the set is
// DERIVED from orderview.Terminal over the whole order.v1 enum rather than
// listed here. A status added to the schema and taught to Terminal is covered by
// this test the day it lands; a hand-written list would be a second copy of the
// set, which is how the original omission happened.
func TestOnlyATerminalViewStatusRefusesTheRedispatch(t *testing.T) {
	ctx := context.Background()
	values := orderpb.OrderStatus(0).Descriptor().Values()
	if values.Len() < 5 {
		t.Fatalf("the order.v1 status enum has %d values — this test is enumerating almost "+
			"nothing", values.Len())
	}

	terminal := 0
	for i := range values.Len() {
		st := orderpb.OrderStatus(values.Get(i).Number())
		prior := order()
		prior.Status = st

		v := &fakeVenue{}
		s, _, view := newServer(t, v)
		if err := view.Record(ctx, prior); err != nil {
			t.Fatalf("seed the view at %v: %v", st, err)
		}
		_, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: redispatched()})

		want := orderview.Terminal(st)
		if want {
			terminal++
		}
		if got := status.Code(err) == codes.AlreadyExists; got != want {
			t.Errorf("a re-dispatch over a view holding %v was refused=%v, want %v "+
				"(orderview.Terminal says %v). A terminal order re-placed is a second trade; a "+
				"live order refused is a recovery path that no longer recovers", st, got, want, want)
		}
		if v.execCalls != 0 && want {
			t.Errorf("the exchange was asked to place an order the view holds %v", st)
		}
	}
	if terminal == 0 {
		t.Fatal("orderview.Terminal called no status terminal — this test asserted nothing")
	}
}

// errStore is a view that cannot answer.
type errStore struct {
	orderview.Store
	err error
}

func (e errStore) Get(context.Context, string) (*orderpb.OrderState, bool, error) {
	return nil, false, e.err
}

// A VIEW THAT CANNOT ANSWER REFUSES. An adapter that cannot establish whether it
// has already worked an order does not get to assume it has not: that is the
// "critical unknown fails closed" rule, and the alternative is a store blip
// becoming a duplicate placement.
func TestExecuteRefusesWhenTheViewCannotBeRead(t *testing.T) {
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	s.view = errStore{Store: view, err: errors.New("view unavailable")}

	_, err := s.Execute(context.Background(), &venuepb.ExecuteRequest{State: redispatched()})
	if status.Code(err) != codes.Internal {
		t.Fatalf("a view that could not be read returned %v, want Internal", err)
	}
	if v.execCalls != 0 {
		t.Fatal("the exchange was asked to place an order this adapter could not establish it had " +
			"not already placed")
	}
}
