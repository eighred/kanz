// A MARKET order the sim venue cannot price must be REJECTED, not left working.
//
// The production fallback builds `execution.NewSimVenue(mic)` with no options,
// so no PriceFunc is wired and `WithPrice` has no call site outside the venue's
// own tests. SimVenue.executionPrice then returns nil for MARKET/STOP, Execute
// returned (nil, nil), and the order rested — FOREVER, and INDISTINGUISHABLY
// from a legitimately working limit order. No per-order log, no metric, no
// terminal state. There is a loud one-time startup WARN that no real venues are
// configured, but it does not say that market orders specifically can never fill.
//
// A permanent condition must be TERMINAL. The order is refused with
// PRICE_UNAVAILABLE — deliberately the same code COMP-M1's compliance gate
// already uses for an order it cannot value, because it is the same failure
// discovered at a different point, and an operator should not have to learn two
// names for it.
package order

import (
	"context"
	"testing"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// simService wires a Service onto a real SimVenue built exactly as production
// builds it: no PriceFunc.
func simService(t *testing.T, fb *fakeBus) *Service {
	t.Helper()
	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("SIM")), execution.NewCloseRegistry(), nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestSubmit_MarketOrderTheSimVenueCannotPriceIsRejected(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, marketOrder())); err != nil {
		t.Fatalf("submit returned %v — an unpriceable order must be REFUSED and acked, never nacked: "+
			"the condition is permanent, so a retry loops forever", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED — a market order that can NEVER fill must not be "+
			"reported as accepted and left working", oc.GetStatus())
	}
	if oc.GetErrorCode() != "PRICE_UNAVAILABLE" {
		t.Fatalf("reason code = %q, want PRICE_UNAVAILABLE (the code COMP-M1 already uses)", oc.GetErrorCode())
	}
	// And it must be terminal in the store, not resting.
	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatalf("status = %v, want REJECTED — ROUTED here means the order rests forever and will "+
			"never fill, which is the silent no-op this change exists to remove", st.GetStatus())
	}
}

// The FACT must be emitted too, not just the command outcome: a rejected order
// is part of the fund's history, and a consumer folding only FACTs would
// otherwise never learn this order died.
func TestSubmit_UnpriceableMarketOrderEmitsTheRejectedFact(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, marketOrder())); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if fb.last(EventTypeRejected) == nil {
		t.Fatal("no OrderRejected FACT emitted — the rejection exists only as a command outcome")
	}
}

// NON-VACUITY: a LIMIT order carries its own price, so the sim venue can still
// fill it. A change that rejected every order at this venue would pass the tests
// above while disabling the paper-trading path entirely.
func TestSubmit_LimitOrderStillFillsAtTheSimVenue(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)

	if err := svc.Handle(testCtx(), submitEnv(),
		mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() == commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("a LIMIT order was rejected — the sim venue prices it from its own limit price, "+
			"and the paper path must keep working (reason: %q)", oc.GetErrorCode())
	}
	if fb.last(EventTypeFilled) == nil {
		t.Fatal("no fill emitted for a priceable LIMIT order")
	}
}
