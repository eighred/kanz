package orderview_test

import (
	"context"
	"errors"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// THE ADAPTER-SIDE FREEZE (#1045).
//
// A venue execution report the ingester refuses has to leave something durable
// behind, or the refusal is a silent drop — the fund holding a position with no
// FACT, which is worse than the wrong number it replaced. This is what it leaves:
// the same order.v1.OrderQuarantine record the OMS writes, on this adapter's own
// view, which Dispatch then refuses to work over.
func TestQuarantineFreezesTheOrderAndDispatchRefusesIt(t *testing.T) {
	ctx := context.Background()
	store := orderview.NewMemory()
	if err := store.Record(ctx, &orderpb.OrderState{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	const reason = "binance reports a cumulative filled quantity greater than the ordered quantity"
	if err := orderview.Quarantine(ctx, store, "o1", reason, at); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	st, _, ok, err := store.Get(ctx, "o1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	q := st.GetQuarantine()
	if q == nil {
		t.Fatal("no quarantine recorded — the refusal left nothing an operator can read")
	}
	if q.GetReason() != reason {
		t.Errorf("reason = %q, want the venue disagreement verbatim", q.GetReason())
	}
	if !q.GetAt().AsTime().Equal(at) {
		t.Errorf("at = %v, want %v — a freeze that cannot be dated cannot be triaged", q.GetAt().AsTime(), at)
	}

	// THE FREEZE IS LOAD-BEARING, not a note. Working the order again would put
	// the fund's money behind a size the venue and the platform disagree about.
	err = orderview.Dispatch(ctx, store, &orderpb.OrderState{
		OrderId: "o1", PortfolioId: "fund-alpha", InstrumentId: "BTC-USD",
		Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED,
	})
	if !errors.Is(err, orderview.ErrQuarantinedNotRedispatched) {
		t.Fatalf("Dispatch over a quarantined order returned %v, want ErrQuarantinedNotRedispatched — "+
			"a freeze nothing refuses is a record nobody reads", err)
	}
}

// THE FIRST CONTRADICTION IS THE ONE AN OPERATOR NEEDS. A repeat report must not
// refresh the timestamp, or a freeze from an hour ago reads as one from now and
// the triage question "how long has this been true" has no answer.
func TestQuarantineKeepsTheFirstContradiction(t *testing.T) {
	ctx := context.Background()
	store := orderview.NewMemory()
	if err := store.Record(ctx, &orderpb.OrderState{OrderId: "o1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
	if err := orderview.Quarantine(ctx, store, "o1", "first", first); err != nil {
		t.Fatalf("first Quarantine: %v", err)
	}
	if err := orderview.Quarantine(ctx, store, "o1", "second", first.Add(time.Hour)); err != nil {
		t.Fatalf("second Quarantine: %v", err)
	}
	st, _, _, err := store.Get(ctx, "o1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := st.GetQuarantine().GetReason(); got != "first" {
		t.Errorf("reason = %q, want the FIRST contradiction", got)
	}
	if !st.GetQuarantine().GetAt().AsTime().Equal(first) {
		t.Errorf("at = %v, want the first freeze's instant", st.GetQuarantine().GetAt().AsTime())
	}
}

// AN ORDER THIS ADAPTER NEVER HELD IS NOT FROZEN SILENTLY: quarantining it would
// be a claim about somebody else's order, and the caller must hear that its
// freeze did not land.
func TestQuarantineRefusesAnOrderNotInTheView(t *testing.T) {
	err := orderview.Quarantine(context.Background(), orderview.NewMemory(), "ghost", "why", time.Now())
	if !errors.Is(err, orderview.ErrNotInView) {
		t.Fatalf("Quarantine of an unknown order returned %v, want ErrNotInView", err)
	}
}

// A FREEZE WITHOUT A REASON IS REFUSED. The reason is what an operator resolves
// the contradiction against; an empty one turns a freeze into an unexplained stop.
func TestQuarantineRefusesAnEmptyReason(t *testing.T) {
	ctx := context.Background()
	store := orderview.NewMemory()
	if err := store.Record(ctx, &orderpb.OrderState{OrderId: "o1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := orderview.Quarantine(ctx, store, "o1", "", time.Now()); err == nil {
		t.Fatal("Quarantine accepted an empty reason")
	}
}

// Seam.Quarantined is the non-failing face the websocket read loop calls. A
// store refusal must reach onErr rather than vanish.
func TestSeamQuarantinedReportsARefusal(t *testing.T) {
	var errs []error
	seam := orderview.NewSeam(orderview.NewMemory(), func(err error) { errs = append(errs, err) })
	seam.Quarantined(&orderpb.OrderState{OrderId: "ghost"}, "venue over-filled")
	if len(errs) != 1 || !errors.Is(errs[0], orderview.ErrNotInView) {
		t.Fatalf("Seam.Quarantined reported %v, want one ErrNotInView — a silent refusal here "+
			"leaves the ingester believing the order was frozen when it was not", errs)
	}
}
