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
// on: 1 IS NOT ARBITRARY, and it used to be 2. Execute read the view twice —
// refuseFinished's guard read and then the merge's — and racing the first proved
// nothing about the write because that guard threw its value away. #947 folded
// the guard into the merge's own decide, so there is exactly ONE read on this
// path now and it is the one the write is conditional on.
func TestAFillLandingInsideARedispatchsReadIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	racing := &racingStore{Store: view, on: 1, during: func() {
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
	if racing.gets < 2 {
		t.Fatalf("Execute read the view %d time(s), want at least 2 (the merge's, and the re-read "+
			"after it lost) — a conditional write that lost has to re-read and re-decide, and no "+
			"re-read means it either wrote unconditionally or gave up", racing.gets)
	}
	// AND NO MORE THAN THE RETRY NEEDED. Two reads is one read plus one re-read;
	// a third would mean the check-then-act #947 removed had come back as a
	// separate Get on this path.
	if racing.gets > 2 {
		t.Fatalf("Execute read the view %d times for one uncontended re-dispatch that lost a "+
			"single race, want 2 — the terminal check and the write are supposed to be ONE "+
			"operation, and a second read of the same entry is the check-then-act again",
			racing.gets)
	}
	if !proto.Equal(st.GetLimitPrice(), qty(6_400_000_000_000, -8)) {
		t.Fatalf("limit_price = %v, want 64000.00 — the re-decided write still has to land the "+
			"OMS's terms; a conditional write that simply gave up would pass every assertion "+
			"above by writing nothing at all", st.GetLimitPrice())
	}
}

// A FILL THAT FINISHES THE ORDER INSIDE THAT SAME WINDOW REFUSES THE PLACEMENT
// (#947) — THE TEST THIS ISSUE NAMES.
//
// This used to be #914's guard racing rather than the write: refuseFinished had
// already waved the order through on a read of its own, so the exchange was asked
// to place an order the venue had just finished, and all this test could pin was
// that the RECORD survived. With the terminal check inside the merge's decide
// there is nothing left to race: the first decide sees a still-working order and
// does not refuse, the conditional write loses because the value changed
// underneath, and Update re-reads and RE-DECIDES against the FILLED entry — which
// refuses. execCalls is the assertion that matters; the view's state is the
// bookkeeping behind it.
func TestAFillThatFinishesTheOrderInsideTheRedispatchRefusesThePlacement(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, venuePartiallyFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	racing := &racingStore{Store: view, on: 1, during: func() {
		if err := orderview.Progress(ctx, view, venueReport(orderpb.OrderStatus_ORDER_STATUS_FILLED, 150_000_000, 0)); err != nil {
			t.Errorf("the venue's own report could not be folded in: %v", err)
		}
	}}
	s.view = racing

	_, err := s.Execute(ctx, &venuepb.ExecuteRequest{State: omsRedispatching()})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a re-dispatch that lost to the fill finishing the order returned %v, want "+
			"AlreadyExists — the venue finished the order between the read and the placement, "+
			"and the adapter is the last line before the exchange", err)
	}
	if v.execCalls != 0 {
		t.Fatalf("the exchange was asked to place the order %d time(s) after the venue reported "+
			"it FILLED inside this re-dispatch's own read — both connectors stamp order_id as "+
			"the client order id, and the exchange dedups a resubmission only WHILE the original "+
			"is open. Once it has filled that id is free again, so this is a second real trade "+
			"with the fund's money", v.execCalls)
	}
	if racing.gets < 2 {
		t.Fatalf("Execute read the view %d time(s), want at least 2 — the refusal has to come "+
			"from a decision RE-MADE against the value the write is conditional on, not from a "+
			"second guard read bolted back on", racing.gets)
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — the refusal wrote the OMS's ROUTED anyway and "+
			"reopened the order: Open returns it again, the reconciler re-queries it every pass, "+
			"and the retention clock it had just started is cleared", st.GetStatus())
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
