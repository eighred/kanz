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
// cancel_announced_at (order_events.proto) was the marker that let a
// redelivery tell "my announcement was interrupted" apart from "this is a
// genuine second cancel of an already-cancelled order" — the same role
// venue_ack_at plays for routing.
//
// # WHAT #292 CHANGED, AND WHY THESE TESTS ASSERT SOMETHING STRONGER NOW
//
// The cancellation and both its FACTs are ONE write: CancelledFact +
// OutcomeFact ride the same store.Save that writes the CANCELLED state, and
// cancel_announced_at is stamped in it. So the interrupted state these tests
// used to construct — CANCELLED in the store, announcement gone — is no longer
// reachable from the live path. A publish failure now DEFERS the announcement to
// the relay instead of destroying it.
//
// The predecessors of the two rewritten tests below asserted the old shape
// honestly: that the marker must stay UNSET after a failed publish, because it
// was the only evidence a compensator had. That evidence is now the outbox row
// itself, so the marker commits with it and means something stronger — "the
// announcement is committed for delivery" rather than "a publish call returned
// nil a moment ago". What these assert is the property #292's Verified-when
// actually asks for: the state change committed, the records are queued, and the
// FACTs arrive once the relay runs — with no redelivery, no sweep and no
// restart.
//
// completeCancelAnnouncement stays, and TestCancel_ResumesInterruptedAnnouncement
// below still drives it, because rows CANCELLED by the PREVIOUS code exist and
// have no outbox record behind them. It is a compensator for a closed
// population now, not a step on the live path.

// TestCancel_InterruptedAnnouncementIsDeferredNotLost is the live path under a
// failing publisher. Nothing is lost and nothing needs to redeliver: the relay
// alone completes the announcement.
func TestCancel_InterruptedAnnouncementIsDeferredNotLost(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	// The cancel reaches the venue and is saved CANCELLED, but the
	// ORDER_CANCELLED FACT fails to publish — a broker blip where the emit used
	// to be.
	fb.failOn = EventTypeCancelled
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected publish failure to surface " +
			"(a real bus.Publish failure must nack, not ack)")
	}
	fb.failOn = ""

	// THE STATE CHANGE COMMITTED. A failed announcement must not roll back a
	// withdrawal the exchange has already been told about.
	st, _, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after the failed publish: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — the Save happens before the announcement", st.GetStatus())
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels = %d, want 1", len(venue.cancelled))
	}
	if fb.last(EventTypeCancelled) != nil {
		t.Fatal("an ORDER_CANCELLED FACT reached the bus despite the injected failure — " +
			"the fake is broken, not the handler")
	}

	// THE MARKER COMMITTED WITH THE RECORDS, which is the inversion this change
	// makes. It no longer records that a publish returned nil; it records that
	// the announcement is durable and owed.
	if st.GetCancelAnnouncedAt() == nil {
		t.Fatal("cancel_announced_at is unset.\n\n" +
			"It commits WITH the records now. Leaving it unset would send a redelivery into " +
			"completeCancelAnnouncement to republish an announcement the outbox already holds — " +
			"a duplicate ORDER_CANCELLED for an order cancelled exactly once.")
	}

	// AND NOTHING IS LOST: both records are queued.
	pending, perr := svc.store.(*MemoryStore).Outbox().Pending(context.Background(), "o1", 10)
	if perr != nil {
		t.Fatalf("outbox pending: %v", perr)
	}
	if len(pending) != 2 {
		t.Fatalf("outbox holds %d records for o1, want 2 (ORDER_CANCELLED + the outcome).\n\n"+
			"This is the whole of #292 for this path: the FACTs commit with the state change, so a "+
			"publish failure defers them instead of destroying them.", len(pending))
	}

	// The relay drains it — no redelivery of the command, no sweep, no restart.
	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	cancelled, ok := fb.last(EventTypeCancelled).(*orderpb.OrderCancelled)
	if !ok {
		t.Fatal("ORDER_CANCELLED never reached the bus after the relay ran — the record was queued " +
			"and then not published, which is worse than the failure this replaced")
	}
	if cancelled.GetCancelledQuantity() == nil {
		t.Error("the recovered ORDER_CANCELLED carries no cancelled quantity — it must be the FACT " +
			"the live path built, not a reconstruction")
	}
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome after the relay ran = %v, want EXECUTED — the cancel DID succeed and the "+
			"operator must be told so", oc.GetStatus())
	}

	// AND THE VENUE IS NEVER ASKED TWICE. Recovery through the relay touches no
	// exchange at all, which is the strongest form of the property the old resume
	// path had to be careful to preserve.
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after recovery = %d, want 1", len(venue.cancelled))
	}
}

// TestCancel_ResumesInterruptedAnnouncement keeps the COMPENSATOR honest. Rows
// saved CANCELLED by the pre-outbox code carry cancel_announced_at unset with
// nothing queued behind them, and completeCancelAnnouncement is the only thing
// that can announce them. The state is constructed directly because the live
// path can no longer produce it — which is the point, and is why this test
// cannot be written by injecting a publish failure any more.
func TestCancel_ResumesInterruptedAnnouncement(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)
	store := svc.store.(*MemoryStore)

	// A pre-outbox row: CANCELLED, announcement never made, nothing in the queue.
	st, ver, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	stale := cloneState(st)
	stale.Status = orderpb.OrderStatus_ORDER_STATUS_CANCELLED
	stale.CancelAnnouncedAt = nil
	if err := store.Save(context.Background(), stale, ver, nil); err != nil {
		t.Fatalf("seed the pre-outbox row: %v", err)
	}
	pending, err := store.Outbox().Pending(context.Background(), "o1", 10)
	if err != nil {
		t.Fatalf("outbox pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the seeded row has %d outbox records, want 0 — this test is meant to exercise the "+
			"compensator, and a queued record would let the relay do the work instead", len(pending))
	}
	venue.cancelled = nil

	// The cancel command arrives for it. This is the interrupted-announcement
	// branch: complete the announcement, and do NOT dispatch to the venue again.
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("delivery returned %v, want nil", err)
	}

	if fb.last(EventTypeCancelled) == nil {
		t.Fatal("no ORDER_CANCELLED FACT was published — tv-sync, accounting and audit would never " +
			"learn this order was cancelled, and nothing is queued to tell them later")
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED — the cancel DID succeed and the caller must not "+
			"be told otherwise", oc.GetStatus())
	}

	st, _, err = store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after the repair: %v", err)
	}
	if st.GetCancelAnnouncedAt() == nil {
		t.Fatal("cancel_announced_at is still unset after a successful repair — a further " +
			"redelivery would re-run the compensator forever instead of hitting the terminal guard")
	}

	// THE SECOND VENUE CALL THIS WHOLE BRANCH EXISTS TO PREVENT: the withdrawal
	// was dispatched by the delivery that got the order to CANCELLED; the repair
	// must not re-dispatch it.
	if len(venue.cancelled) != 0 {
		t.Fatalf("venue cancels during the repair = %d, want 0 — completing an announcement "+
			"re-sent a withdrawal to the exchange", len(venue.cancelled))
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
	st, _, err := svc.store.Load(context.Background(), "o1")
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

// TestCancel_RecoveryNeverReDispatchesToVenue isolates the single most important
// detail of the fix, mirroring service_close_test.go's counting-Closer style: a
// cancellation whose announcement was interrupted is completed without a second
// venue call — and now without a second DELIVERY either.
//
// IT ALSO PINS THE ONE BEHAVIOUR #292 CHANGED FOR AN OPERATOR, so that the next
// reader does not take it for a regression. Delivery 1 nacks to the DLQ; by the
// time anyone redrives it, the marker is set, so the redrive is indistinguishable
// from a genuine second cancel of a terminal order and is answered
// REJECTED/ORDER_TERMINAL — exactly as TestCancel_DuplicateAfterAnnouncedStaysTerminal
// pins for a cancel that succeeded outright. The EXECUTED answer the caller
// actually needs came from the outbox and carries the original command's
// lineage. Telling the two apart would need a fourth marker recording which
// DELIVERY announced a transition, which is the instalment pattern #292 exists
// to stop.
func TestCancel_RecoveryNeverReDispatchesToVenue(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"} // confirms every cancel it is asked to make
	svc, reg := restingOrderOn(t, fb, venue)

	fb.failOn = EventTypeOutcome // let ORDER_CANCELLED go out; fail the outcome behind it
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
	// ORDER_CANCELLED flushed ahead of the outcome, so only the outcome is owed.
	if fb.last(EventTypeCancelled) == nil {
		t.Fatal("ORDER_CANCELLED FACT missing after delivery 1 — the relay publishes records in " +
			"order, so the cancellation should have gone out before the injected failure")
	}

	// THE RELAY ALONE FINISHES IT. No redelivery, and therefore no possibility of
	// a second venue call at all.
	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome after the relay ran = %v, want EXECUTED", oc.GetStatus())
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after recovery = %d, want still 1", len(venue.cancelled))
	}

	// AND A REDRIVE OF THE PARKED COMMAND STILL NEVER REACHES THE EXCHANGE. It is
	// answered as the duplicate it now is (see the doc above), which is a refusal
	// on the ledger — closeAtVenue sits below Cancel()'s terminal guard and is
	// never reached.
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("the redrive returned %v, want nil — a duplicate cancel is acked", err)
	}
	oc = fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED ||
		oc.GetErrorCode() != "ORDER_TERMINAL" {
		t.Fatalf("outcome on the redrive = %v/%q, want REJECTED/ORDER_TERMINAL", oc.GetStatus(), oc.GetErrorCode())
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels after the redrive = %d, want still 1 — no path may ever "+
			"re-dispatch a withdrawal the first delivery already sent", len(venue.cancelled))
	}
}
