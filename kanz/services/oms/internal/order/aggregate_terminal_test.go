package order

import (
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// THE AGGREGATE REFUSES EVERY TRANSITION OUT OF A TERMINAL STATE (#840).
//
// # What was asymmetric
//
// aggregate.go is the component whose job is to decide which order transitions
// are legal. Four of its six enforced that themselves — ApplyFill, Cancel,
// Expire and Amend each open with IsTerminal(st). Route and Reject did not: both
// cloned the state and wrote ROUTED / REJECTED unconditionally, so handed a
// CANCELLED order either returned a state saying the order was working at a
// venue, or terminally rejected, over a terminal one.
//
// # Why it was not reachable, and why that is the argument FOR the guard
//
// Three mechanisms closed it, none of them in the aggregate: resume() checks
// IsTerminal(fresh) before calling work(); Reconcile() refuses a terminal order
// outright and calls reaching it "a caller defect"; and the per-order claim plus
// Store.Save's version predicate stop two goroutines or two pods interleaving.
//
// So the invariant lived in a caller-side check in one function plus a lock and
// a CAS in two other files, while a reader of aggregate.go saw four transitions
// refusing a terminal state and reasonably concluded the aggregate enforced it.
// A new caller of work(), or a reshuffle of resume()'s guards, reintroduced it
// silently — and the failure is not a crash. It is an order the ledger calls
// ROUTED after it was CANCELLED, which resume() and the sweep then treat as
// live: a withdrawn order re-driven to an exchange.
//
// These tests pin the aggregate's own answer, so the callers' guards become
// defence in depth rather than the only defence.

// terminalStates are every state IsTerminal accepts, built through the real
// transitions rather than by assigning Status, so a state that stops being
// terminal is caught here too.
func terminalStates(t *testing.T) map[string]*orderpb.OrderState {
	t.Helper()
	out := map[string]*orderpb.OrderState{}

	cancelled, _, err := Cancel(freshRoutedOrder(t), t0)
	if err != nil {
		t.Fatalf("build CANCELLED: %v", err)
	}
	out["CANCELLED"] = cancelled

	expired, _, err := Expire(freshRoutedOrder(t), t0)
	if err != nil {
		t.Fatalf("build EXPIRED: %v", err)
	}
	out["EXPIRED"] = expired

	filled, err := ApplyFill(freshRoutedOrder(t), &orderpb.Fill{
		Quantity: d(100, 0), Price: d(1000, -2), ExecutedAt: timestamppb.New(t0),
	}, t0)
	if err != nil {
		t.Fatalf("build FILLED: %v", err)
	}
	out["FILLED"] = filled

	// NON-VACUITY FOR THE FIXTURE ITSELF. A state this map calls terminal that
	// IsTerminal disagrees with would make every assertion below meaningless.
	for name, st := range out {
		if !IsTerminal(st) {
			t.Fatalf("fixture %s is %s, which IsTerminal does not consider terminal — these "+
				"tests would assert nothing", name, st.GetStatus())
		}
	}
	return out
}

func freshRoutedOrder(t *testing.T) *orderpb.OrderState {
	t.Helper()
	st, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	routed, err := Route(st, t0)
	if err != nil {
		t.Fatalf("Route a fresh order: %v", err)
	}
	return routed
}

// ROUTING A TERMINAL ORDER IS REFUSED.
//
// This is the one that costs money. work() calls Route and has no IsTerminal
// check of its own — its only status test is WORKING_SCHEDULED — so a terminal
// order arriving there was rewritten to ROUTED and saved. resume() and the sweep
// treat ROUTED as live, so the order is then re-driven to a venue: the fund
// trades an order an operator already withdrew.
func TestRoute_RefusesATerminalOrder(t *testing.T) {
	for name, st := range terminalStates(t) {
		t.Run(name, func(t *testing.T) {
			next, err := Route(st, t0)
			if next != nil {
				t.Errorf("Route returned a %s state for a %s order — the ledger would call a "+
					"finished order working at a venue", next.GetStatus(), name)
			}
			assertTerminalRefusal(t, err, "route")
		})
	}
}

// REJECTING A TERMINAL ORDER IS REFUSED.
//
// adopt() takes the venue's truth as ours on the recovery path. Rejecting an
// order that is already FILLED would overwrite a completed trade with REJECTED —
// the ledger's last word contradicting a fill the fund actually got. Reconcile()
// refuses a terminal order before adopt() is reached, and this is the aggregate
// saying the same thing where the transition happens.
func TestReject_RefusesATerminalOrder(t *testing.T) {
	for name, st := range terminalStates(t) {
		t.Run(name, func(t *testing.T) {
			next, err := Reject(st, t0)
			if next != nil {
				t.Errorf("Reject returned a %s state for a %s order", next.GetStatus(), name)
			}
			assertTerminalRefusal(t, err, "reject")
		})
	}
}

// EVERY TRANSITION REFUSES UNDER ONE CODE, so a caller needs no new error
// handling and an operator reading an audit trail sees one name for one failure.
func assertTerminalRefusal(t *testing.T, err error, what string) {
	t.Helper()
	re, ok := err.(*RejectError)
	if !ok {
		t.Fatalf("%s of a terminal order: err = %v, want *RejectError", what, err)
	}
	if re.Code != "ORDER_TERMINAL" {
		t.Fatalf("%s of a terminal order: code = %q, want ORDER_TERMINAL — the four transitions "+
			"that already guarded use that code, and a second name for one failure is a second "+
			"thing for a caller to handle", what, re.Code)
	}
}

// NON-VACUITY, AND THE BOUNDARY THAT KEEPS THE FIX HONEST. A guard that refused
// everything would pass both tests above and stop the OMS routing anything at
// all. Every LIVE state must still transition exactly as before.
func TestRouteAndReject_StillTransitionALiveOrder(t *testing.T) {
	accepted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	routed, err := Route(accepted, t0)
	if err != nil {
		t.Fatalf("Route a PENDING_NEW order: %v — the guard is refusing ordinary work", err)
	}
	if routed.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("status = %v, want ROUTED", routed.GetStatus())
	}

	rejected, err := Reject(routed, t0)
	if err != nil {
		t.Fatalf("Reject a ROUTED order: %v", err)
	}
	if rejected.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatalf("status = %v, want REJECTED", rejected.GetStatus())
	}

	// A PARTIALLY_FILLED order is live, not terminal, and must still route and
	// reject — the state most likely to be wrongly caught by a broad guard.
	partial, err := ApplyFill(freshRoutedOrder(t), &orderpb.Fill{
		Quantity: d(40, 0), Price: d(1000, -2), ExecutedAt: timestamppb.New(t0),
	}, t0)
	if err != nil {
		t.Fatalf("partial fill: %v", err)
	}
	if IsTerminal(partial) {
		t.Fatal("a PARTIALLY_FILLED order is terminal — this test is asserting nothing")
	}
	if _, err := Reject(partial, t0); err != nil {
		t.Fatalf("Reject a PARTIALLY_FILLED order: %v — a partially filled order that meets a "+
			"permanent venue failure must still be able to die", err)
	}
}
