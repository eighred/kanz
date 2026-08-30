package order

import (
	"context"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// THE DELIVERY BUDGET AGAINST THE STORE THAT ACTUALLY HONOURS IT (#801).
//
// # Why this file exists at all
//
// Every other test of the budget runs on MemoryStore, and MemoryStore IGNORES
// the context: its Save commits whether or not the deadline has passed. pgx does
// not. So the in-memory suite cannot see the one interleaving that matters most
// here — the delivery budget expiring BETWEEN a child's venue withdrawal and the
// write that records it.
//
// If that window is reachable, the fix has a hole exactly where the issue says
// it must not: the exchange has been told to cancel a slice, the ledger has no
// record of it, the parent nacks, and the replay finds the child still live and
// dispatches a SECOND CancelOrder — the duplicate venue traffic the budget was
// added to remove, arriving through the door the budget opened.
//
// This is therefore a MEASUREMENT, not a formality. It asserts the property on
// the store the OMS actually runs against.
//
// Gated on TEST_POSTGRES_URL, like every other Postgres test in this package.
func TestPostgres_AnAbandonedFanOutWithdrawsEachChildExactlyOnce(t *testing.T) {
	pool := newPool(t)

	at := schedEnd
	fb := &fakeBus{}
	store := NewPostgres(pool)
	venue := &restingCloser{delay: 120 * time.Millisecond}
	closes := execution.NewCloseRegistry()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), closes, nil,
		WithHaltGate(halt.OpenGate(nil)),
		WithClaimWait(50*time.Millisecond),
		WithDeliveryBudget(600*time.Millisecond))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return at }

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, scheduledOrder("p1", 12, nil))); err != nil {
		t.Fatalf("admit the parent: %v", err)
	}
	if _, err := svc.DriveSchedules(driveCtx()); err != nil {
		t.Fatalf("DriveSchedules: %v", err)
	}
	children, err := store.ListByParent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListByParent: %v", err)
	}
	var live []string
	for _, c := range children {
		if !IsTerminal(c) {
			live = append(live, c.GetOrderId())
		}
	}
	if len(live) != 12 {
		t.Fatalf("precondition: %d live children, want 12", len(live))
	}

	// FIRST DELIVERY: it must run out of budget partway through the fan-out.
	if err := cancelParent(t, svc); err == nil {
		t.Fatal("the first delivery withdrew all twelve children inside its budget — there is " +
			"nothing abandoned to replay, so this test measures nothing")
	}
	firstPass := terminalPGChildren(t, store, live)
	if firstPass == 0 || firstPass == len(live) {
		t.Fatalf("the first delivery withdrew %d of %d children; this test needs a partial "+
			"withdrawal", firstPass, len(live))
	}

	// REPLAY with a budget that can finish the job, exactly as an operator
	// replaying from dlq.order.order.cancel would.
	svc.deliveryBudget = 10 * time.Second
	if err := cancelParent(t, svc); err != nil {
		t.Fatalf("the replay of an abandoned cancel failed: %v", err)
	}
	if got := terminalPGChildren(t, store, live); got != len(live) {
		t.Fatalf("%d of %d children are terminal after the replay", got, len(live))
	}

	// THE MEASUREMENT. One venue withdrawal per child across both deliveries.
	//
	// A count above len(live) means a child was dispatched to the exchange by the
	// abandoned delivery and dispatched AGAIN by the replay — which is only
	// possible if the first delivery told the venue something the ledger never
	// recorded. That is the duplicate cancel-replace traffic #801 is about, and
	// the whole argument for abandoning being safe rests on it not happening.
	if got, uniq := venue.dispatchCount(), venue.uniqueDispatches(); got != len(live) || uniq != got {
		t.Fatalf("%d venue withdrawals across %d distinct children for %d live children over an "+
			"abandoned delivery and its replay — a slice was cancelled at the exchange twice, "+
			"and closeAtVenue is not idempotent from the exchange's point of view",
			got, uniq, len(live))
	}

	// AND NOTHING IS LEFT DANGLING. A withdrawal the deadline cut off mid-flight
	// is unconfirmed but TRACKED, so the healing watchdog resolves it against
	// venue truth; one that is neither confirmed nor tracked is a cancel nobody
	// will ever reconcile.
	if unconfirmed := venue.dispatchCount() - venue.confirmedCount(); unconfirmed > closes.Len() {
		t.Fatalf("%d withdrawals went unconfirmed but only %d are tracked for healing",
			unconfirmed, closes.Len())
	}
}

func terminalPGChildren(t *testing.T, store *Postgres, ids []string) int {
	t.Helper()
	n := 0
	for _, id := range ids {
		c, _, err := store.Load(context.Background(), id)
		if err != nil {
			t.Fatalf("Load child %s: %v", id, err)
		}
		if IsTerminal(c) {
			n++
		}
	}
	return n
}
