package orderview

// THE OMS'S COPY OF AN ORDER DOES NOT OVERWRITE WHAT THE VENUE OBSERVED (#944).
//
// Dispatch is the write Server.Execute makes before it works an order, and it
// used to be Store.Record — an unconditional upsert. A re-dispatch of an order
// the view held PARTIALLY_FILLED therefore wrote the OMS's OrderState straight
// over the exchange's own filled_quantity and leaves_quantity, and the OMS's copy
// is behind the venue BY CONSTRUCTION on this path: the adapter hears about a
// fill on the exchange's websocket and the OMS hears about it from the adapter.
//
// THE PROPERTY IS A PARTITION, NOT A PRECEDENCE. The OMS owns the order's TERMS
// and the exchange owns what HAPPENED to it, so a re-dispatch has to refresh the
// first without touching the second. Both halves are asserted in every case
// below: a merge that kept the venue's numbers and dropped the refresh would be
// a different defect (an adapter working an order off terms it has been
// corrected on), not a fix.
//
// BOTH BACKENDS, because Memory establishes "nothing changed underneath" with a
// counter under a mutex and Postgres with an equality on the stored bytes inside
// an UPDATE, and neither is evidence for the other — #905's own history is a
// green in-memory suite hiding a duplicate venue cancel that the first
// Postgres-gated run caught.

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// omsRedispatch is what arrives on a venue.v1.ExecuteRequest: the OMS's own
// record of the order, which still believes nothing has traded.
func omsRedispatch(id string) *orderpb.OrderState {
	st := routed(id)
	st.OrderedQuantity = dec(150_000_000, -8) // 1.5
	st.FilledQuantity = dec(0, -8)
	st.LeavesQuantity = dec(150_000_000, -8)
	// NOT FROZEN, STATED RATHER THAN INHERITED. routed() is the round-trip
	// fixture — every field of OrderState populated, so the stores are proven to
	// carry all of them — and one of those fields is the quarantine. An order
	// this adapter has frozen is one Dispatch refuses outright
	// (ErrQuarantinedNotRedispatched, #1045), so leaving it set here would make
	// every case in this file a test of the freeze instead of a test of the
	// terms/observations partition it is about.
	st.Quarantine = nil
	return st
}

// venuePartial is the entry the exchange's own execution report leaves behind —
// what orderview.Progress writes, quantities and price included.
func venuePartial(id string, filled, leaves int64) *orderpb.OrderState {
	st := partiallyFilled(id, filled, leaves)
	st.Quarantine = nil // see omsRedispatch: not frozen, and this file is not about the freeze
	st.AverageFillPrice = dec(6_500_000_000_000, -8)
	st.LimitPrice = dec(6400000, -2)
	st.VenueAccountId = "binance-alpha"
	return st
}

// THE CASE THE ISSUE WAS FILED FOR. The view holds the venue's partial fill; the
// OMS re-dispatches saying nothing has traded; the exchange's numbers have to
// still be readable afterwards.
func TestDispatchKeepsTheVenuesObservationsOverTheOMSsCopy(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.Record(ctx, venuePartial("ORD-1", 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed the view: %v", err)
		}

		if err := Dispatch(ctx, s, omsRedispatch("ORD-1")); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		got, _, ok, err := s.Get(ctx, "ORD-1")
		if err != nil || !ok {
			t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
		}
		if !proto.Equal(got.GetFilledQuantity(), dec(40_000_000, -8)) {
			t.Fatalf("filled_quantity = %v, want 0.4 (coefficient 40000000, exponent -8) — the "+
				"OMS's copy of the order overwrote the quantity the EXCHANGE reported, and "+
				"venue_orders is never pruned, so this adapter's answer to \"what did the venue "+
				"fill\" is now the OMS's stale guess", got.GetFilledQuantity())
		}
		if !proto.Equal(got.GetLeavesQuantity(), dec(110_000_000, -8)) {
			t.Fatalf("leaves_quantity = %v, want 1.1 — the same stale copy, one field over",
				got.GetLeavesQuantity())
		}
		if !proto.Equal(got.GetAverageFillPrice(), dec(6_500_000_000_000, -8)) {
			t.Fatalf("average_fill_price = %v, want 65000.00 — the price the venue reported for "+
				"the part that traded is gone from this adapter's record of it",
				got.GetAverageFillPrice())
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PARTIALLY_FILLED {
			t.Fatalf("status = %v, want PARTIALLY_FILLED — writing the OMS's ROUTED beside a "+
				"filled_quantity of 0.4 leaves a record that contradicts itself, which is harder "+
				"to act on than either honest answer", got.GetStatus())
		}
	})
}

// THE HALF THAT KEEPS THE OTHER HALF HONEST. A re-dispatch writes at all because
// it carries the order's terms as the OMS holds them NOW. Declining to write when
// an entry exists would pass the test above and leave the adapter working an
// order off terms it has since been corrected on — the fills would be enriched
// with the wrong limit price, account and expiry.
func TestDispatchStillRefreshesTheTermsTheOMSOwns(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.Record(ctx, venuePartial("ORD-1", 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed the view: %v", err)
		}

		amended := omsRedispatch("ORD-1")
		amended.LimitPrice = dec(6600000, -2)
		amended.VenueAccountId = "binance-hedge"
		amended.Leverage = dec(5, 0)
		if err := Dispatch(ctx, s, amended); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		got, _, ok, err := s.Get(ctx, "ORD-1")
		if err != nil || !ok {
			t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
		}
		if !proto.Equal(got.GetLimitPrice(), dec(6600000, -2)) {
			t.Fatalf("limit_price = %v, want 66000.00 — the OMS is the authority on an order's "+
				"terms, and a re-dispatch that does not refresh them leaves this adapter working "+
				"and enriching against a record the OMS has already corrected", got.GetLimitPrice())
		}
		if got.GetVenueAccountId() != "binance-hedge" {
			t.Fatalf("venue_account_id = %q, want binance-hedge — same rule, and this one decides "+
				"which account a fill is attributed to", got.GetVenueAccountId())
		}
		if !proto.Equal(got.GetLeverage(), dec(5, 0)) {
			t.Fatalf("leverage = %v, want 5", got.GetLeverage())
		}
		if !proto.Equal(got.GetFilledQuantity(), dec(40_000_000, -8)) {
			t.Fatalf("filled_quantity = %v, want 0.4 — the refresh reached a field the exchange "+
				"is the authority on", got.GetFilledQuantity())
		}
	})
}

// AN ORDER THE ADAPTER HAS NEVER SEEN IS SEEDED, which is the ordinary case and
// the one every placement takes. The seed is the FULL OrderState the OMS sent —
// not the partial reconstruction Progress refuses with ErrNotInView — and the
// order has to land in Open, or the healing watchdog never reconciles it.
func TestDispatchSeedsAnOrderTheViewHasNeverSeen(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := Dispatch(ctx, s, omsRedispatch("ORD-1")); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		got, _, ok, err := s.Get(ctx, "ORD-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !ok {
			t.Fatal("a first dispatch recorded nothing — the adapter would work an order it has " +
				"no record of, so its fills are unenrichable and the reconciler cannot see it")
		}
		if !proto.Equal(got, omsRedispatch("ORD-1")) {
			t.Fatalf("the seeded order is not the one the OMS sent:\n got %v\nwant %v", got, omsRedispatch("ORD-1"))
		}
		open, err := s.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if len(open) != 1 {
			t.Fatalf("the view believes %d orders are open at the venue after a first dispatch, "+
				"want 1 — the healing watchdog reconciles exactly this set", len(open))
		}
	})
}

// A TERMINAL ENTRY REFUSES THE DISPATCH OUTRIGHT (#947), which is a stronger
// answer than the merge that used to keep the verdict here (#944) and it is the
// one the caller acts on: Server.Execute turns this sentinel into
// codes.AlreadyExists and never calls the exchange. The refusal is HERE and not
// in the caller because in here it is the same operation as the write — the
// caller's own Get-then-check was a check-then-act, and a fill landing in its
// window got the order placed a second time at a real exchange.
//
// EVERY TERMINAL STATUS, DERIVED FROM Terminal RATHER THAN LISTED. A status added
// to the schema and taught to Terminal is covered the day it lands; a
// hand-written list here would be a second copy of the set, which is how the
// original omission happened.
func TestDispatchRefusesAnOrderTheVenueHasAlreadyFinished(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		values := orderpb.OrderStatus(0).Descriptor().Values()
		terminal := 0
		for i := range values.Len() {
			st := orderpb.OrderStatus(values.Get(i).Number())
			if !Terminal(st) {
				continue
			}
			terminal++
			id := "ORD-" + st.String()
			done := venuePartial(id, 150_000_000, 0)
			done.Status = st
			if err := s.Record(ctx, done); err != nil {
				t.Fatalf("seed the view at %v: %v", st, err)
			}

			err := Dispatch(ctx, s, omsRedispatch(id))
			if !errors.Is(err, ErrTerminalNotRedispatched) {
				t.Fatalf("a dispatch over a view holding %v returned %v, want "+
					"ErrTerminalNotRedispatched — the caller places the order at the exchange "+
					"unless this refuses, and an order the venue already finished placed again "+
					"is a second trade with the fund's money", st, err)
			}

			// AND THE ENTRY IS UNTOUCHED. A refusal that still wrote would put the
			// OMS's ROUTED over the venue's verdict: Open returns the order again,
			// the reconciler re-queries it every pass and re-emits StateHealed
			// about it, and the eviction clock it had started is cleared — the leak
			// #904 closed, reopened through this writer.
			got, _, ok, err := s.Get(ctx, id)
			if err != nil || !ok {
				t.Fatalf("order %s left the view entirely (ok=%v err=%v)", id, ok, err)
			}
			if got.GetStatus() != st {
				t.Fatalf("status = %v after a refused dispatch, want %v — the refusal wrote "+
					"anyway, and the OMS's stale copy replaced the venue's own verdict",
					got.GetStatus(), st)
			}
			if !proto.Equal(got.GetFilledQuantity(), dec(150_000_000, -8)) {
				t.Fatalf("filled_quantity = %v, want 1.5 — the refused write reached the "+
					"quantities the EXCHANGE reported", got.GetFilledQuantity())
			}
		}
		if terminal == 0 {
			t.Fatal("Terminal called no status terminal — this test asserted nothing")
		}
		open, err := s.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if len(open) != 0 {
			t.Fatalf("the view believes %d orders are open after re-dispatches over finished "+
				"orders, want 0", len(open))
		}
	})
}

// A FILL THAT FINISHES THE ORDER INSIDE THE DISPATCH'S OWN READ STILL REFUSES,
// and this is the interleaving #947 exists for. The decorator commits the venue's
// FILLED report inside the Get whose value the merge is about to be built on, so
// the first decide sees a still-working order and does NOT refuse. The
// conditional write then loses — the value changed underneath — and Update
// re-reads and RE-DECIDES against the FILLED entry, which refuses. A retry that
// re-applied the first decision would write the OMS's ROUTED over the verdict and
// return success, and the caller would place the order at the exchange.
func TestADispatchLosingToAFinishingFillRefusesOnTheRedecide(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.Record(ctx, venuePartial("ORD-1", 40_000_000, 110_000_000)); err != nil {
			t.Fatalf("seed the view: %v", err)
		}
		done := venuePartial("ORD-1", 150_000_000, 0)
		done.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
		racing := &racingView{Store: s, on: 1, during: func() {
			if err := Progress(ctx, s, done); err != nil {
				t.Errorf("the venue's own report could not be folded in: %v", err)
			}
		}}

		err := Dispatch(ctx, racing, omsRedispatch("ORD-1"))
		if !errors.Is(err, ErrTerminalNotRedispatched) {
			t.Fatalf("a dispatch that lost to the fill that finished the order returned %v, want "+
				"ErrTerminalNotRedispatched — the decision has to be re-made against the value "+
				"the write is conditional on, or the terminal check is a check-then-act again "+
				"and the exchange is asked to place a finished order", err)
		}
		if racing.gets < 2 {
			t.Fatalf("Dispatch read the view %d time(s), want at least 2 (its own, and the "+
				"re-read after the write lost) — no re-read means it either wrote "+
				"unconditionally or gave up", racing.gets)
		}
		got, _, ok, err := s.Get(ctx, "ORD-1")
		if err != nil || !ok {
			t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
			t.Fatalf("status = %v, want FILLED — the venue's verdict was overwritten by a merge "+
				"built on the pre-fill read", got.GetStatus())
		}
		if !proto.Equal(got.GetFilledQuantity(), dec(150_000_000, -8)) {
			t.Fatalf("filled_quantity = %v, want 1.5 — the whole order traded and this adapter's "+
				"record says otherwise", got.GetFilledQuantity())
		}
	})
}

// racingView commits a write inside the Nth Get, after the read has happened and
// before its value reaches the caller — so the caller decides against a value the
// store no longer holds. Deterministic, single-goroutine, repeatable; the same
// decorator internal/venueadapter/server uses on the RPC, kept here so this
// package's own interleavings are provable against BOTH backends.
type racingView struct {
	Store
	gets   int
	on     int
	during func()
}

func (r *racingView) Get(ctx context.Context, orderID string) (*orderpb.OrderState, Revision, bool, error) {
	st, rev, ok, err := r.Store.Get(ctx, orderID)
	r.gets++
	if r.gets == r.on && r.during != nil {
		r.during()
	}
	return st, rev, ok, err
}

// AN UNSPECIFIED STORED STATUS IS NOT CARRIED FORWARD. UNSPECIFIED is "the venue
// said something we cannot read", never "no status" (see ErrUnmappedStatus), so
// keeping it over a status the OMS CAN state would replace what this adapter
// knows with what it does not — the inverse of the field-blanking Progress
// guards against, and reachable through the same store: Record is unconditional
// and takes whatever it is given.
func TestDispatchTakesTheOMSsStatusOverAnUnreadableStoredOne(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		unreadable := venuePartial("ORD-1", 40_000_000, 110_000_000)
		unreadable.Status = orderpb.OrderStatus_ORDER_STATUS_UNSPECIFIED
		if err := s.Record(ctx, unreadable); err != nil {
			t.Fatalf("seed the view: %v", err)
		}

		if err := Dispatch(ctx, s, omsRedispatch("ORD-1")); err != nil {
			t.Fatalf("Dispatch: %v", err)
		}

		got, _, ok, err := s.Get(ctx, "ORD-1")
		if err != nil || !ok {
			t.Fatalf("order ORD-1 left the view entirely (ok=%v err=%v)", ok, err)
		}
		if got.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
			t.Fatalf("status = %v, want ROUTED — an unreadable stored status was carried forward "+
				"over one the OMS could state, and an order stuck at UNSPECIFIED is one Open "+
				"returns forever", got.GetStatus())
		}
		if !proto.Equal(got.GetFilledQuantity(), dec(40_000_000, -8)) {
			t.Fatalf("filled_quantity = %v, want 0.4 — the quantities are guarded per field, so "+
				"an unreadable status must not take them with it", got.GetFilledQuantity())
		}
	})
}

// A DISPATCH WITH NO ORDER ID IS REFUSED RATHER THAN WRITTEN SOMEWHERE. Update
// would otherwise read and write the empty key.
func TestDispatchRefusesAnOrderWithNoID(t *testing.T) {
	eachBackend(t, func(t *testing.T, s Store) {
		if err := Dispatch(context.Background(), s, &orderpb.OrderState{}); err == nil {
			t.Fatal("Dispatch accepted an order with no order_id")
		}
	})
}
