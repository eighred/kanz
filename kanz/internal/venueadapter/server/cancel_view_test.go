package server

// A VENUE-CONFIRMED CANCEL DOES NOT ERASE WHAT THE VENUE ITSELF REPORTED (#921).
//
// Server.recordStatus used to clone the OrderState the OMS sent on the cancel
// request, stamp CANCELLED on it and upsert that — so the OMS's record, which is
// behind the venue by construction on this path, replaced the verdict and the
// quantities the exchange's own execution report had established through
// orderview.Progress.
//
// It is reachable on purpose: BinanceVenue.CancelOrder maps Binance's -2011
// "Unknown order sent" to a CONFIRMED withdrawal because the order may have
// "already filled, expired, or withdrawn by an earlier attempt". A cancel that
// races a fill returns success, and the next line wrote CANCELLED over FILLED.
//
// Four properties, and the last two are what keep the first two honest:
//
//  1. a terminal verdict already in the view survives the cancel, with the
//     venue-observed quantities intact;
//  2. an order that is STILL WORKING is still closed — and closed by merging
//     onto what the view holds, so a partial fill the venue reported is not
//     erased by the OMS's copy either;
//  3. an order this adapter has no record of is still seeded, or a cancel after
//     a restart would leave nothing for a late execution report to enrich from;
//  4. a view that cannot be read writes NOTHING, and the RPC still succeeds —
//     the cancel landed at the exchange and must not be retried.

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// qty builds a common.v1.Decimal. Quantities on this path are base-10 Decimal
// and never float, so every assertion below compares the exact wire value.
//
// It is not named dec: server.go imports internal/dec under that name, and a
// package-level identifier colliding with another FILE-scoped import breaks that
// file rather than this one, which is a confusing way to find out.
func qty(coefficient int64, exponent int32) *commonpb.Decimal {
	return &commonpb.Decimal{Coefficient: coefficient, Exponent: exponent}
}

// venueFilled is the order as the EXCHANGE's own execution report leaves it in
// the view: orderview.Progress folded FILLED plus the traded quantities in.
func venueFilled() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	st.OrderedQuantity = qty(150_000_000, -8) // 1.5
	st.FilledQuantity = qty(150_000_000, -8)  // 1.5 — the venue traded all of it
	st.LeavesQuantity = qty(0, -8)            // nothing left working
	st.AverageFillPrice = qty(6_500_000_000_000, -8)
	return st
}

// omsCancelling is what the OMS sends on CancelOrderRequest: its own record,
// which has not caught up with the venue. It still believes the order is working
// and that nothing has traded.
func omsCancelling() *orderpb.OrderState {
	st := order()
	st.Status = orderpb.OrderStatus_ORDER_STATUS_ROUTED
	st.OrderedQuantity = qty(150_000_000, -8)
	st.FilledQuantity = qty(0, -8)
	st.LeavesQuantity = qty(150_000_000, -8)
	return st
}

func TestAConfirmedCancelKeepsTheVenuesFilledVerdictAndItsQuantities(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, closes, view := newServer(t, v)
	if err := view.Record(ctx, venueFilled()); err != nil {
		t.Fatalf("seed the view: %v", err)
	}

	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The withdrawal itself is untouched by this change: it was dispatched and
	// confirmed, and the in-flight close was resolved. A guard that fixed the
	// bookkeeping by not cancelling would be a far worse defect.
	if closes.Len() != 0 {
		t.Fatalf("registry still holds %d closes after a confirmed cancel, want 0", closes.Len())
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("the view has ORD-1 at %v after a confirmed cancel, want FILLED — a cancel the "+
			"venue confirms says the order is no longer WORKING, not that it did not TRADE, and "+
			"Binance returns exactly that answer (-2011) for an order it already filled",
			st.GetStatus())
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(150_000_000, -8)) {
		t.Fatalf("filled_quantity is %v after a confirmed cancel, want 1.5 (coefficient 150000000, "+
			"exponent -8) — the OMS's copy overwrote the quantity the exchange reported, and this "+
			"adapter can no longer say what the venue filled", st.GetFilledQuantity())
	}
	if !proto.Equal(st.GetLeavesQuantity(), qty(0, -8)) {
		t.Fatalf("leaves_quantity is %v after a confirmed cancel, want 0 — the OMS's copy said the "+
			"whole order was still working", st.GetLeavesQuantity())
	}
	if !proto.Equal(st.GetAverageFillPrice(), qty(6_500_000_000_000, -8)) {
		t.Fatalf("average_fill_price is %v after a confirmed cancel, want 65000.00 — the price the "+
			"venue reported for the trade is gone from this adapter's record of it",
			st.GetAverageFillPrice())
	}
}

// EVERY TERMINAL STATUS SURVIVES AND EVERY OTHER ONE IS CLOSED — and the set is
// DERIVED from orderview.Terminal over the whole order.v1 enum rather than listed
// here, the same way TestOnlyATerminalViewStatusRefusesTheRedispatch derives it.
// A status added to the schema and taught to Terminal is covered by this test the
// day it lands; a hand-written list here would be a second copy of the set, which
// is the shape that produced the defect in the first place.
func TestOnlyATerminalViewStatusSurvivesAConfirmedCancel(t *testing.T) {
	ctx := context.Background()
	values := orderpb.OrderStatus(0).Descriptor().Values()
	if values.Len() < 5 {
		t.Fatalf("the order.v1 status enum has %d values — this test is enumerating almost nothing",
			values.Len())
	}

	terminal, closed := 0, 0
	for i := range values.Len() {
		prior := orderpb.OrderStatus(values.Get(i).Number())
		seed := order()
		seed.Status = prior

		v := &fakeVenue{}
		s, _, view := newServer(t, v)
		if err := view.Record(ctx, seed); err != nil {
			t.Fatalf("seed the view at %v: %v", prior, err)
		}
		if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
			t.Fatalf("cancel over a view holding %v: %v", prior, err)
		}

		want := prior
		if !orderview.Terminal(prior) {
			want = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
			closed++
		} else {
			terminal++
		}
		st, _, ok, err := view.Get(ctx, "ORD-1")
		if err != nil || !ok {
			t.Fatalf("order ORD-1 left the view entirely for prior %v (ok=%v err=%v)", prior, ok, err)
		}
		if st.GetStatus() != want {
			t.Errorf("a confirmed cancel over a view holding %v left it at %v, want %v "+
				"(orderview.Terminal says %v). A terminal verdict overwritten is the venue's own "+
				"report replaced by the OMS's guess; a live order left un-closed is an order the "+
				"reconciler chases forever", prior, st.GetStatus(), want, orderview.Terminal(prior))
		}
	}
	if terminal == 0 || closed == 0 {
		t.Fatalf("the enum walk saw %d terminal and %d non-terminal statuses — one of the two "+
			"branches asserted nothing", terminal, closed)
	}
}

// THE MERGE IS NOT ONLY ABOUT TERMINAL ORDERS. A PARTIALLY_FILLED order IS closed
// by the cancel, and the venue's partial-fill quantities have to come through it:
// the OMS's copy on the request is the same stale record, and recording it
// wholesale erases a fill that DID happen.
func TestAConfirmedCancelOfAWorkingOrderKeepsTheVenuesPartialFill(t *testing.T) {
	ctx := context.Background()
	partial := order()
	partial.Status = orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED
	partial.OrderedQuantity = qty(150_000_000, -8)
	partial.FilledQuantity = qty(40_000_000, -8) // 0.4 traded at the venue
	partial.LeavesQuantity = qty(110_000_000, -8)

	v := &fakeVenue{}
	s, _, view := newServer(t, v)
	if err := view.Record(ctx, partial); err != nil {
		t.Fatalf("seed the view: %v", err)
	}
	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil || !ok {
		t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("a working order is %v after a confirmed cancel, want CANCELLED — the close has to "+
			"take it out of Open, or the reconciler re-queries it forever", st.GetStatus())
	}
	if !proto.Equal(st.GetFilledQuantity(), qty(40_000_000, -8)) {
		t.Fatalf("filled_quantity is %v after the cancel, want 0.4 (coefficient 40000000, exponent "+
			"-8) — the OMS's copy said nothing had traded and overwrote a partial fill the venue "+
			"reported", st.GetFilledQuantity())
	}
	// Out of Open is the other half of closing it, and it is the half an operator
	// feels: an order left there is one the healing watchdog keeps reconciling.
	open, err := view.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the view believes %d orders are still open after a confirmed cancel, want 0", len(open))
	}
}

// AN ORDER THIS ADAPTER HAS NO RECORD OF IS STILL SEEDED. Memory loses the whole
// view on restart and forgets a terminal order after DefaultTerminalRetention, so
// a cancel arriving for an order with no entry is ordinary. The request carries
// the FULL OrderState the OMS holds — the order's terms, not the partial
// reconstruction orderview.Progress refuses with ErrNotInView — so it is safe to
// seed, and dropping the write would leave a late execution report nothing to
// enrich from.
func TestAConfirmedCancelSeedsAnOrderTheAdapterHasNoRecordOf(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, _, view := newServer(t, v)

	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st, _, ok, err := view.Get(ctx, "ORD-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("a confirmed cancel for an order with no view entry recorded nothing — after a " +
			"restart that is every order, and Seam.Lookup then has nothing to enrich a late " +
			"execution report with")
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("the seeded entry is %v, want CANCELLED", st.GetStatus())
	}
	if st.GetInstrumentId() != "BTC-USD" {
		t.Fatalf("the seeded entry lost the order's terms (instrument_id=%q) — it must carry the "+
			"full OrderState the OMS sent", st.GetInstrumentId())
	}
}

// blindStore cannot answer Get and counts every write it is asked to make —
// BOTH kinds, because the cancel path writes through RecordIf now and a counter
// that only watched Record would report zero writes while the view was being
// overwritten. Counting the writes is the whole assertion: "nothing was written"
// is not observable from a store that also cannot be read.
type blindStore struct {
	orderview.Store
	err     error
	records int
}

func (b *blindStore) Get(context.Context, string) (*orderpb.OrderState, orderview.Revision, bool, error) {
	return nil, orderview.Revision{}, false, b.err
}

func (b *blindStore) Record(ctx context.Context, st *orderpb.OrderState) error {
	b.records++
	return b.Store.Record(ctx, st)
}

func (b *blindStore) RecordIf(ctx context.Context, st *orderpb.OrderState, at orderview.Revision) (bool, error) {
	b.records++
	return b.Store.RecordIf(ctx, st, at)
}

// A VIEW THAT CANNOT BE READ WRITES NOTHING, AND THE RPC STILL SUCCEEDS.
//
// Two halves, and both are load-bearing. An adapter that cannot establish what
// the venue already reported does not get to assume it reported nothing — that is
// the critical-unknown-fails-closed rule, and the alternative is a store blip
// permanently replacing the exchange's verdict with the OMS's in a table that is
// never pruned. But the cancel itself LANDED, so failing the RPC would tell the
// OMS to retry a withdrawal that already succeeded.
func TestAConfirmedCancelWritesNothingWhenTheViewCannotBeRead(t *testing.T) {
	ctx := context.Background()
	v := &fakeVenue{}
	s, closes, view := newServer(t, v)
	blind := &blindStore{Store: view, err: errors.New("view unavailable")}
	s.view = blind

	if _, err := s.CancelOrder(ctx, &venuepb.CancelOrderRequest{State: omsCancelling()}); err != nil {
		t.Fatalf("a confirmed cancel was reported to the OMS as a failure because the LOCAL VIEW "+
			"could not be read: %v — the OMS would retry a withdrawal the venue already "+
			"confirmed", err)
	}
	if closes.Len() != 0 {
		t.Fatalf("registry still holds %d closes after a confirmed cancel, want 0", closes.Len())
	}
	if blind.records != 0 {
		t.Fatalf("the adapter wrote to the order view %d times without being able to read it "+
			"first, want 0 — an unreadable view is an unknown, and an unknown on this path must "+
			"not overwrite the venue's own report", blind.records)
	}
}
