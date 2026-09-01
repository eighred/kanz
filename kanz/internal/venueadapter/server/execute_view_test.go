package server

// A RE-DISPATCH DOES NOT OVERWRITE THE VENUE'S OWN PARTIAL FILL (#944).
//
// #914 gave Execute a read of the view, but only in order to REFUSE a TERMINAL
// order; the write on the next line stayed Store.Record, an unconditional upsert.
// So a re-dispatch of an order the view held PARTIALLY_FILLED passed the guard
// and wrote the OMS's OrderState over the exchange's own filled_quantity and
// leaves_quantity — the same erasure #921 and #934 closed on the cancel path,
// through the one writer neither of them changed.
//
// BOUNDED THE WAY THE ISSUE BOUNDS IT, because overstating this would be its own
// defect: the status left behind was the OMS's ROUTED, which is not terminal, so
// Open kept returning the order and the healing watchdog re-queried the exchange
// and healed the quantities next pass. No fill was lost. What did not
// self-correct is venue_orders, which is never pruned — until that pass this
// adapter's answer to "what did the venue report filled" was the OMS's stale
// guess, and Seam.Lookup hands it to the user-data ingester enriching any
// execution report arriving in the window.
//
// EVERY TEST HERE ALSO ASSERTS THE ORDER WAS STILL WORKED, or the fix would be
// the worse defect: re-working a live order is how this platform recovers an
// interrupted one, and an adapter that quietly stopped placing them is a trading
// outage.

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// omsRedispatching is what the OMS sends on a second ExecuteRequest: its own
// record of the order, which has not caught up with the venue and still says
// nothing has traded.
func omsRedispatching() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	st.OrderedQuantity = qty(150_000_000, -8)
	st.FilledQuantity = qty(0, -8)
	st.LeavesQuantity = qty(150_000_000, -8)
	st.LimitPrice = qty(6_400_000_000_000, -8)
	return st
}

// venuePartiallyFilled is the entry the exchange's own execution report leaves
// behind: 0.4 of 1.5 traded, and still working.
func venuePartiallyFilled() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	st.OrderedQuantity = qty(150_000_000, -8)
	st.FilledQuantity = qty(40_000_000, -8)
	st.LeavesQuantity = qty(110_000_000, -8)
	st.AverageFillPrice = qty(6_500_000_000_000, -8)
	st.LimitPrice = qty(6_300_000_000_000, -8)
	return st
}

// THE TEST THE ISSUE NAMES.
func TestARedispatchDoesNotOverwriteTheVenuesPartialFill(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}

	if _, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()}); err != nil {
		t.Fatalf("re-dispatching an order that is still working was refused: %v", err)
	}
	if v.execCalls != 1 {
		t.Fatalf("the venue was called %d times for a live order, want 1 — re-working an "+
			"interrupted order is how this platform recovers one", v.execCalls)
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(40_000_000, -8)) {
		t.Fatalf("filled_quantity = %v after a re-dispatch, want 0.4 (coefficient 40000000, "+
			"exponent -8) — the OMS's copy, which is behind the venue by construction on this "+
			"path, overwrote the quantity the EXCHANGE reported. venue_orders is never pruned, so "+
			"until the next reconciliation pass this adapter's answer to \"what did the venue "+
			"fill\" is that stale guess, and Seam.Lookup hands it to the ingester enriching any "+
			"execution report that arrives meanwhile", st.GetFilledQuantity())
	}
	if !proto.Equal(st.GetLeavesQuantity(), qty(110_000_000, -8)) {
		t.Fatalf("leaves_quantity = %v, want 1.1 — the same stale copy, one field over",
			st.GetLeavesQuantity())
	}
	if !proto.Equal(st.GetAverageFillPrice(), qty(6_500_000_000_000, -8)) {
		t.Fatalf("average_fill_price = %v, want 65000.00 — the price the venue reported for the "+
			"part that traded is gone from this adapter's record of it", st.GetAverageFillPrice())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
		t.Fatalf("status = %v after a re-dispatch, want PARTIALLY_FILLED — writing the OMS's "+
			"ROUTED beside a filled_quantity of 0.4 leaves a record that contradicts itself",
			st.GetStatus())
	}
	// The terms the OMS re-sent still land. A merge that kept the venue's numbers
	// by declining to write would pass every assertion above and leave the adapter
	// working an order off terms the OMS has already corrected.
	if !proto.Equal(st.GetLimitPrice(), qty(6_400_000_000_000, -8)) {
		t.Fatalf("limit_price = %v, want 64000.00 — the OMS owns an order's terms and a "+
			"re-dispatch carries them; refusing the write entirely trades one defect for another",
			st.GetLimitPrice())
	}
}

// THE FIRST DISPATCH IS UNCHANGED. Nothing in the view, so the OMS's full
// OrderState seeds it, and the order lands in the set the watchdog reconciles.
func TestAFirstDispatchStillSeedsTheView(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)

	if _, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if v.execCalls != 1 {
		t.Fatalf("the venue was called %d times, want 1", v.execCalls)
	}
	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("a first dispatch recorded nothing — the adapter just worked an order it has no " +
			"record of, so its fills are unenrichable and the reconciler cannot see it")
	}
	if !proto.Equal(st, omsRedispatching()) {
		t.Fatalf("the seeded order is not the one the OMS sent:\n got %v\nwant %v", st, omsRedispatching())
	}
	open, err := view.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the view believes %d orders are open after a first dispatch, want 1", len(open))
	}
}

// A FILL LANDING INSIDE THE RE-DISPATCH'S OWN READ IS NOT OVERWRITTEN (the #934
// interleaving, on this writer).
//
// The exchange's report is written by the user-data ingester from another
// goroutine. Made deterministic here the way #934 made it deterministic for the
// cancel: a Store decorator commits the racing write inside the Get whose value
// the merge is about to be built on. -race would not find this — a lost update is
// a correct-looking interleaving, not a data race, and -race needs cgo the usual
// box does not have.
//
// on: 2 IS NOT ARBITRARY. Execute reads the view twice: refuseFinished's guard
// read is the first, and the merge's is the second. Racing the FIRST would prove
// nothing about the write, because refuseFinished throws its value away.
func TestAFillLandingInsideARedispatchsReadIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	racing := &racingStore{Store: view, on: 2, during: func() {
		// PARTIALLY_FILLED over PARTIALLY_FILLED with a larger quantity: the
		// status never moves, so a compare-and-set on the status would apply and
		// lose the fill with a green test beside it.
		if err := orderview.Progress(ctx, view, venueReport(orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 70_000_000, 80_000_000)); err != nil {
			t.Errorf("the venue's own report could not be folded in: %v", err)
		}
	}}
	s.view = racing

	if _, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(70_000_000, -8)) {
		t.Fatalf("filled_quantity = %v, want 0.7 (coefficient 70000000, exponent -8) — the venue "+
			"reported a second fill on a still-working order between this re-dispatch's read and "+
			"its write, and the write carried the 0.4 it had read", st.GetFilledQuantity())
	}
	if !proto.Equal(st.GetLeavesQuantity(), qty(80_000_000, -8)) {
		t.Fatalf("leaves_quantity = %v, want 0.8 — the same stale read, one field over",
			st.GetLeavesQuantity())
	}
	if racing.gets < 3 {
		t.Fatalf("Execute read the view %d time(s), want at least 3 (the guard's, the merge's, and "+
			"the re-read after it lost) — a conditional write that lost has to re-read and "+
			"re-decide, and no re-read means it either wrote unconditionally or gave up",
			racing.gets)
	}
	if !proto.Equal(st.GetLimitPrice(), qty(6_400_000_000_000, -8)) {
		t.Fatalf("limit_price = %v, want 64000.00 — the re-decided write still has to land the "+
			"OMS's terms; a conditional write that simply gave up would pass every assertion "+
			"above by writing nothing at all", st.GetLimitPrice())
	}
}

// A FILL THAT FINISHES THE ORDER INSIDE THAT SAME WINDOW KEEPS ITS VERDICT.
//
// This is #914's guard racing rather than this issue's write: refuseFinished has
// already waved the order through, so the placement goes out either way (#947
// carries that, and folding the two reads into one Update is the repair). What
// this pins is that the RECORD survives it — the merge carries the venue's own
// FILLED forward instead of writing the OMS's ROUTED over it, so the order does
// not come back into Open and the eviction clock is not reset.
func TestAFillThatFinishesTheOrderInsideTheRedispatchKeepsItsVerdict(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	racing := &racingStore{Store: view, on: 2, during: func() {
		if err := orderview.Progress(ctx, view, venueReport(orderpb.OrderStatus_ORDER_STATUS_FILLED, 150_000_000, 0)); err != nil {
			t.Errorf("the venue's own report could not be folded in: %v", err)
		}
	}}
	s.view = racing

	if _, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — the exchange finished the order between the guard's "+
			"read and this write, and the OMS's ROUTED reopened it: Open returns it again, the "+
			"reconciler re-queries it every pass, and the retention clock it had just started is "+
			"cleared", st.GetStatus())
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(150_000_000, -8)) {
		t.Fatalf("filled_quantity = %v, want 1.5 — the whole order traded and this adapter's "+
			"record says otherwise", st.GetFilledQuantity())
	}
	open, err := view.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the view believes %d orders are open at the venue, want 0", len(open))
	}
}

// UNDER PERMANENT CONTENTION THE ORDER IS NOT PLACED, and that asymmetry against
// CancelOrder is deliberate. A confirmed cancel has ALREADY landed at the
// exchange, so failing that RPC would make the OMS retry a withdrawal that
// worked. This runs BEFORE the exchange is touched: an adapter that has not
// recorded the order it is about to work would leave its fills unenrichable and
// invisible to the reconciler, so it refuses instead — the same direction Execute
// already took when the plain Record failed.
func TestAPermanentlyContendedRedispatchIsRefusedRatherThanPlaced(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	contended := &contendedStore{Store: view}
	s.view = contended

	_, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()})
	if status.Code(err) != codes.Internal {
		t.Fatalf("a re-dispatch whose view write was permanently contended returned %v, want "+
			"Internal — trading an order this adapter could not record leaves its fills "+
			"unenrichable and invisible to the reconciler", err)
	}
	if v.execCalls != 0 {
		t.Fatalf("the exchange was asked to place an order %d time(s) that this adapter could not "+
			"record", v.execCalls)
	}
	if contended.attempts != orderview.UpdateAttempts {
		t.Fatalf("the re-dispatch tried %d conditional writes, want UpdateAttempts (%d) — an "+
			"unbounded retry stalls the handler the OMS is waiting on, in the one process holding "+
			"the exchange session", contended.attempts, orderview.UpdateAttempts)
	}
	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view (ok=%v err=%v)", ok, err)
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(40_000_000, -8)) {
		t.Fatalf("filled_quantity = %v, want the 0.4 the venue reported — a contended write must "+
			"refuse, not force itself through", st.GetFilledQuantity())
	}
}
