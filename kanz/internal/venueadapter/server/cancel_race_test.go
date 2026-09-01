package server

// A FILL LANDING BETWEEN THE CANCEL'S READ AND ITS WRITE IS NOT OVERWRITTEN (#934).
//
// #921 stopped a venue-confirmed cancel replacing the venue's own verdict with
// the OMS's copy of the order — but it did it with a Get followed by a Record,
// and those are two operations. The user-data ingester's orderview.Progress
// writes the exchange's execution report into the same entry from another
// goroutine, so a fill landing in the gap was still overwritten by a merge built
// on the pre-fill read. Both statuses are terminal, so Open and the eviction
// clock never noticed; what was lost is the RECORD — permanently, in
// venue_orders, which is never pruned — and with it the platform's answer to
// "what did the venue return, and what filled".
//
// -race would not have found this. A lost update is a correct-looking
// interleaving and not a data race, and -race needs cgo, which the usual box does
// not have. So the interleaving is made DETERMINISTIC instead: a Store decorator
// runs the racing write inside recordStatus's own Get, in one goroutine, at
// exactly the instant that used to lose the fill.
//
// THE SECOND TEST IS THE ONE THAT MATTERS MOST, and it is here because the
// obvious repair passes the first. A compare-and-set on the order's STATUS catches
// a racing writer that moved the status — but the ordinary interleaving does not
// move it: a partially filling order takes another fill, and the venue writes
// PARTIALLY_FILLED over PARTIALLY_FILLED with a larger filled_quantity. A status
// predicate holds, the merge is still built on the pre-fill read, and the quantity
// is lost with a green test beside it.

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// racingStore commits a write inside the Nth Get, after the read has happened and
// before its value reaches the caller — so the caller decides against a value the
// store no longer holds. Deterministic, single-goroutine, and repeatable.
type racingStore struct {
	orderview.Store
	gets int
	on   int
	// during is the venue's own report landing in the gap.
	during func()
}

func (r *racingStore) Get(ctx context.Context, orderID string) (*orderpb.OrderState, orderview.Revision, bool, error) {
	st, rev, ok, err := r.Store.Get(ctx, orderID)
	r.gets++
	if r.gets == r.on && r.during != nil {
		r.during()
	}
	return st, rev, ok, err
}

// venuePartial is the order as the exchange's own report leaves it: still
// working, and 0.4 of it traded.
func venuePartial(filled, leaves int64) *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	st.OrderedQuantity = qty(150_000_000, -8)
	st.FilledQuantity = qty(filled, -8)
	st.LeavesQuantity = qty(leaves, -8)
	return st
}

// venueReport is what the user-data ingester hands orderview.Progress: the
// venue's status and the quantities it observed, and NOT the order's terms.
func venueReport(status orderpb.OrderStatus, filled, leaves int64) *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        "ORD-1",
		Status:         status,
		FilledQuantity: qty(filled, -8),
		LeavesQuantity: qty(leaves, -8),
	}
}

// racingCancel seeds the view at prior, drives a confirmed cancel with the venue
// report landing inside recordStatus's Get, and returns the view's final answer.
func racingCancel(t *testing.T, prior, report *orderpb.OrderState) (*orderpb.OrderState, *racingStore) {
	t.Helper()
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, prior); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	racing := &racingStore{Store: view, on: 1, during: func() {
		if err := orderview.Progress(ctx, view, report); err != nil {
			t.Errorf("the venue's own report could not be folded in: %v", err)
		}
	}}
	s.view = racing

	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	return st, racing
}

// THE FILL WINS, AND THE CANCEL WRITES NOTHING. Re-deciding against a view that
// has gone terminal since the read is #921's own rule reached a second time: a
// confirmed cancel is the venue saying the order is no longer WORKING, never that
// it did not TRADE.
func TestAFillLandingInsideTheCancelsReadKeepsTheVenuesVerdict(t *testing.T) {
	st, racing := racingCancel(t,
		venuePartial(40_000_000, 110_000_000),
		venueReport(orderpb.OrderStatus_ORDER_STATUS_FILLED, 150_000_000, 0))

	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the view has ORD-1 at %v, want FILLED — the exchange's execution report landed "+
			"between this cancel's read and its write, and the cancel wrote CANCELLED over it from "+
			"a pre-fill read", st.GetStatus())
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(150_000_000, -8)) {
		t.Fatalf("filled_quantity = %v, want 1.5 (coefficient 150000000, exponent -8) — the "+
			"quantity the exchange reported was replaced by the one this cancel had read before "+
			"the fill", st.GetFilledQuantity())
	}
	if !proto.Equal(st.GetLeavesQuantity(), qty(0, -8)) {
		t.Fatalf("leaves_quantity = %v, want 0 — the view still says 1.1 is working at an exchange "+
			"that has finished the order", st.GetLeavesQuantity())
	}
	if racing.gets < 2 {
		t.Fatalf("the cancel read the view %d time(s), want at least 2 — a conditional write that "+
			"lost has to re-read and re-decide, and one read means it either wrote unconditionally "+
			"or gave up", racing.gets)
	}
}

// THE CASE A STATUS COMPARE-AND-SET PASSES. Nothing about the status changed —
// the venue reported another fill on the same working order — so a status
// predicate applies, and the cancel's merge carries the stale 0.4 to the store.
// The cancel itself must still land: the order really was withdrawn.
func TestAFillThatOnlyMovesTheQuantityIsNotOverwrittenByTheCancel(t *testing.T) {
	st, _ := racingCancel(t,
		venuePartial(40_000_000, 110_000_000),
		venueReport(orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 70_000_000, 80_000_000))

	if !proto.Equal(st.GetFilledQuantity(), qty(70_000_000, -8)) {
		t.Fatalf("filled_quantity = %v, want 0.7 (coefficient 70000000, exponent -8). The venue "+
			"reported a second fill on a STILL WORKING order — PARTIALLY_FILLED over "+
			"PARTIALLY_FILLED — so the status never moved and a compare-and-set on the status "+
			"cannot see it. The write that followed carried the 0.4 this cancel had read, and "+
			"this adapter can no longer say what the venue filled", st.GetFilledQuantity())
	}
	if !proto.Equal(st.GetLeavesQuantity(), qty(80_000_000, -8)) {
		t.Fatalf("leaves_quantity = %v, want 0.8 — the same stale read, one field over",
			st.GetLeavesQuantity())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — the order was withdrawn at the venue and is not "+
			"terminal in the view, so the reconciler will chase it forever", st.GetStatus())
	}
}

// AND IT CONVERGES. A conditional write that fails and gives up would pass the
// test above's quantity assertion — the venue's numbers survive precisely because
// nothing was written — while silently dropping a cancel the exchange confirmed.
// So this asserts the cancel LANDED, and that the writer actually went round
// again to land it.
func TestALosingConditionalWriteRetriesRatherThanGivingUp(t *testing.T) {
	ctx := context.Background()
	st, racing := racingCancel(t,
		venuePartial(40_000_000, 110_000_000),
		venueReport(orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED, 70_000_000, 80_000_000))

	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v after a cancel whose first conditional write lost, want CANCELLED — "+
			"a write that fails and gives up loses a withdrawal the exchange confirmed, and the "+
			"healing watchdog re-queries the order forever", st.GetStatus())
	}
	if racing.gets < 2 {
		t.Fatalf("the cancel read the view %d time(s), want at least 2 — the losing write has to "+
			"re-read before it can re-decide", racing.gets)
	}
	open, err := racing.Store.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the view believes %d order(s) are still open after a confirmed cancel, want 0",
			len(open))
	}
}

// contendedStore refuses every conditional write, which is what permanent
// contention looks like from the caller's side.
type contendedStore struct {
	orderview.Store
	attempts int
}

func (c *contendedStore) RecordIf(context.Context, *orderpb.OrderState, orderview.Revision) (bool, error) {
	c.attempts++
	return false, nil
}

// UNDER PERMANENT CONTENTION THE RPC STILL SUCCEEDS AND THE VIEW IS LEFT ALONE.
// Same asymmetry #921 states: the withdrawal LANDED at the exchange, so failing
// the RPC would tell the OMS to retry a cancel that already worked. The order
// stays non-terminal here, which means the healing watchdog re-reads venue truth
// on its next pass — a cost that self-corrects, unlike an overwrite of the
// exchange's own report in a table nothing prunes.
func TestAPermanentlyContendedCancelSucceedsWithoutOverwritingTheView(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, closes, view := newServer(t, v)
	if err := view.Record(ctx, venuePartial(40_000_000, 110_000_000)); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	contended := &contendedStore{Store: view}
	s.view = contended

	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("a confirmed cancel was reported to the OMS as a failure because the LOCAL VIEW "+
			"was contended: %v — the OMS would retry a withdrawal the venue already confirmed", err)
	}
	if closes.Len() != 0 {
		t.Fatalf("registry still holds %d closes after a confirmed cancel, want 0", closes.Len())
	}
	if contended.attempts != orderview.UpdateAttempts {
		t.Fatalf("the cancel tried %d conditional writes, want UpdateAttempts (%d) — an unbounded "+
			"retry stalls the handler the OMS is waiting on", contended.attempts, orderview.UpdateAttempts)
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
