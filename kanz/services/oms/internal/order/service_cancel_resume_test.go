package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE BUG THIS FILE PINS: handleCancel did, in order, Cancel() -> closeAtVenue()
// -> store.Save(next) -> EmitCancelled(...) -> EmitOutcome(EXECUTED). If Save
// succeeded and EmitCancelled (or the final EmitOutcome) then failed, the
// handler returned the error, the broker redelivered, the redelivery loaded
// the order already CANCELLED, Cancel()'s IsTerminal guard fired, and
// outcomeReject emitted a REJECTED outcome and acked — so the order WAS
// cancelled in the store, the ORDER_CANCELLED FACT was NEVER published (tv-sync,
// accounting, and audit never learn the cancel happened), and the caller was
// told REJECTED for a cancel that in fact succeeded.
//
// cancel_announced_at (order_events.proto:18) is the marker that lets a
// redelivery tell "my announcement was interrupted" apart from "this is a
// genuine second cancel of an already-cancelled order" — the same role
// venue_ack_at plays for routing.

// TestCancel_ResumesInterruptedAnnouncement reproduces the bug end to end: a
// cancel whose EmitCancelled fails must, on redelivery, publish the FACT and
// report EXECUTED — not silently succeed in the store while telling the
// caller REJECTED.
func TestCancel_ResumesInterruptedAnnouncement(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	// Delivery 1: the cancel reaches the venue and is saved CANCELLED, but the
	// ORDER_CANCELLED FACT fails to publish — a broker blip between the Save
	// and the emit.
	fb.failOn = EventTypeCancelled
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected EmitCancelled failure to surface " +
			"(a real bus.Publish failure must nack, not ack, so the broker redelivers)")
	}
	fb.failOn = ""

	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status after delivery 1 = %v, want CANCELLED — Save happened before the "+
			"failed EmitCancelled", st.GetStatus())
	}
	if st.GetCancelAnnouncedAt() != nil {
		t.Fatal("cancel_announced_at is set after delivery 1 — it must only be stamped once " +
			"the FACT and outcome are actually published, and EmitCancelled just failed")
	}
	if fb.last(EventTypeCancelled) != nil {
		t.Fatal("an ORDER_CANCELLED FACT was recorded despite the injected publish failure — " +
			"the fake is broken, not the handler")
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after delivery 1 = %d, want 1", len(venue.cancelled))
	}

	// Delivery 2: the redelivery. The order is already CANCELLED with no
	// cancel_announced_at — an interrupted announcement, not a duplicate. The
	// handler must complete the announcement WITHOUT calling closeAtVenue again.
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}

	if fb.last(EventTypeCancelled) == nil {
		t.Fatal("no ORDER_CANCELLED FACT was published on the redelivery — tv-sync, accounting, " +
			"and audit would never learn this order was cancelled")
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED — the cancel DID succeed and the caller must not "+
			"be told otherwise", oc.GetStatus())
	}

	st, err = svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 2: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status after delivery 2 = %v, want CANCELLED", st.GetStatus())
	}
	if st.GetCancelAnnouncedAt() == nil {
		t.Fatal("cancel_announced_at is still unset after a successful resume — a further " +
			"redelivery would re-run the resume branch forever instead of hitting the terminal guard")
	}

	// THE SECOND VENUE CALL THIS WHOLE DESIGN EXISTS TO PREVENT: the venue
	// withdrawal was already dispatched by delivery 1; the resume path must not
	// re-dispatch it.
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after resume = %d, want 1 — the resume path re-dispatched a "+
			"withdrawal the first delivery already sent to the exchange", len(venue.cancelled))
	}
}

// TestCancel_DuplicateAfterAnnouncedStaysTerminal preserves the genuine-duplicate
// case: a second cancel of an order that is CANCELLED *and* whose announcement
// already completed must still be refused ORDER_TERMINAL, not resumed.
func TestCancel_DuplicateAfterAnnouncedStaysTerminal(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	// A normal cancel: nothing fails, so the announcement completes and
	// cancel_announced_at is stamped.
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetCancelAnnouncedAt() == nil {
		t.Fatal("cancel_announced_at not stamped after a fully successful cancel")
	}
	cancelledFacts := 0
	for _, et := range fb.types() {
		if et == EventTypeCancelled {
			cancelledFacts++
		}
	}
	if cancelledFacts != 1 {
		t.Fatalf("ORDER_CANCELLED facts after first cancel = %d, want 1", cancelledFacts)
	}

	// A genuine second cancel command for the same order.
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("second cancel: %v", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		oc.GetErrorCode() != "ORDER_TERMINAL" {
		t.Fatalf("outcome on genuine duplicate = %v/%q, want REJECTED/ORDER_TERMINAL",
			oc.GetStatus(), oc.GetErrorCode())
	}
	// No second ORDER_CANCELLED FACT — an announced cancel is genuinely done.
	cancelledFacts = 0
	for _, et := range fb.types() {
		if et == EventTypeCancelled {
			cancelledFacts++
		}
	}
	if cancelledFacts != 1 {
		t.Fatalf("ORDER_CANCELLED facts after the genuine duplicate = %d, want still 1 — a "+
			"duplicate of an ANNOUNCED cancel must not re-emit the FACT", cancelledFacts)
	}
	// And the venue was never asked to withdraw the order a second time.
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after the genuine duplicate = %d, want 1", len(venue.cancelled))
	}
}

// TestCancel_ResumeNeverReDispatchesToVenue isolates the single most important
// detail of the fix, mirroring service_close_test.go's counting-Closer style:
// the resume path must complete the announcement without a second venue call.
func TestCancel_ResumeNeverReDispatchesToVenue(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"} // confirms every cancel it is asked to make
	svc, reg := restingOrderOn(t, fb, venue)

	fb.failOn = EventTypeOutcome // let EmitCancelled succeed; fail the FINAL outcome emit
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected outcome-publish failure to surface")
	}
	fb.failOn = ""

	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after delivery 1 = %d, want exactly 1", len(venue.cancelled))
	}
	if reg.Len() != 0 {
		t.Fatalf("registry holds %d closes, want 0 (the venue confirmed on delivery 1)", reg.Len())
	}
	// ORDER_CANCELLED was published on delivery 1 (only the outcome failed); the
	// resume path must recognize the order is CANCELLED-but-unannounced and
	// re-emit both without touching the venue again.
	if fb.last(EventTypeCancelled) == nil {
		t.Fatal("ORDER_CANCELLED FACT missing after delivery 1 — expected EmitCancelled to have " +
			"succeeded before the injected outcome failure")
	}

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome after resume = %v, want EXECUTED", oc.GetStatus())
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after resume = %d, want still 1 — the resume path must NEVER "+
			"re-dispatch a venue withdrawal the first delivery already sent", len(venue.cancelled))
	}
}
