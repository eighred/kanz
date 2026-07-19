package order

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// unreachableVenue accepts the route and then fails to execute: a venue call
// that timed out, a 500, an adapter that lost its connection. By the time this
// fires the order has already been admitted, routed, and SAVED as ROUTED.
type unreachableVenue struct {
	execution.Venue
	mu sync.Mutex
	n  int
}

func (v *unreachableVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	return nil, errors.New("venue unreachable")
}

func (v *unreachableVenue) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// This test PINS A KNOWN GAP. It passes today because it asserts what the code
// currently does, and that is the point: it is the executable evidence for a
// decision taken elsewhere, in test/arch/bus_dlq_test.go, that bus consumers
// must NOT be wired with bus.WithRetry.
//
// The reasoning that decision rests on is: re-running handleSubmit does not
// resume the work, it ACKS. Delivery 1 creates the order and then fails at the
// venue. Any second run — a broker redelivery, or an in-handler retry attempt —
// loads the record delivery 1 left behind, takes it for a duplicate, and returns
// nil. The consumer reads that nil as success and marks the command handled.
//
// So wiring retry would be strictly worse than not wiring it: attempt 2 would
// swallow the failure IN PROCESS, and it would never reach the DLQ that the
// same change just wired. A comment asserting that would be the kind of claim
// this repository has been burned by; this is the same claim, fired.
//
// WHEN THIS TEST FAILS, THAT IS THE SIGNAL, NOT A REGRESSION. It means handlers
// have learned to resume, and the retry guard in test/arch/bus_dlq_test.go
// should be revisited rather than the assertions here relaxed. See the
// crash-mid-work row on the production readiness board: resuming safely needs a
// record of whether the venue ever saw the order, because a failed Execute does
// NOT prove it did not arrive, and re-driving it blind is a double trade.
func TestRedeliveryAfterVenueFailureIsAckedWithoutResuming(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := &unreachableVenue{Venue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted, routed, stored as ROUTED, then the venue call fails.
	// The error surfaces, which is what asks the broker for a redelivery.
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface. " +
			"If it no longer does, this test is asserting nothing and the retry guard's " +
			"rationale must be re-derived from scratch")
	}
	// NON-VACUITY: the work really was attempted. Without this, an order refused
	// at admission would produce the same acked-second-delivery below for an
	// entirely different and harmless reason.
	if got := venue.count(); got != 1 {
		t.Fatalf("venue.Execute called %d times on delivery 1, want exactly 1 — "+
			"the order never reached the venue, so this is not the failure being pinned", got)
	}

	// Delivery 2: the redelivery that error asked for — or, identically, the
	// second attempt bus.WithRetry would make in-process.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; the gap being pinned is that it returns NIL", err)
	}
	if got := venue.count(); got != 1 {
		t.Fatalf("venue.Execute called %d times after delivery 2, want still 1 — "+
			"the handler now re-drives the venue, which is the double-trade hazard, "+
			"not the fix", got)
	}

	// The command is now acked as handled, and this is what was actually left
	// behind: an order sitting at ROUTED that nothing is working and nothing
	// will mention again. It is indistinguishable, in the store and over the
	// API, from a limit order resting normally at the exchange.
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("order status is %v, want ROUTED — the gap being pinned is an order "+
			"stranded in a WORKING state after its venue call failed", got)
	}
	if IsTerminal(st) {
		t.Fatal("order is terminal — it was resolved somehow, so nothing is stranded " +
			"and the retry guard's rationale no longer holds")
	}
}
