package order

import (
	"context"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// THE BUG THIS FILE PINS: work()'s fill loop does, per fill: store.Save(next)
// then emitter.EmitFill(...). When a fill makes the order FILLED, Save
// persists a TERMINAL state; if EmitFill (or handleSubmit's trailing
// EmitOutcome) then fails, the handler returns the error and the broker
// NACKs. On redelivery, resume()'s old `if IsTerminal(st) { return nil }`
// acked the redelivery as a duplicate — the order sits FILLED in the store,
// the ORDER_FILLED FACT was NEVER published, and nothing ever recovers it:
// SweepInterrupted skips terminal orders, and a DLQ replay re-enters the same
// IsTerminal trap. A completed trade that tv-sync, accounting, and audit
// never learn about.
//
// The same hole exists on the terminal-reject paths: the ErrUnpriced reject
// (Save(Reject(...)) then refuse(...)) and adopt()'s VENUE_REJECTED reject
// (Save(Reject(...)) then refuse(...)).
//
// outcome_announced_at (order_events.proto:19) is the one marker covering all
// three transitions — the same role venue_ack_at plays for routing and
// cancel_announced_at plays for cancellation.

// TestSubmit_ResumesInterruptedFillAnnouncement_FillFactFails reproduces the
// core bug: a fill that FILLS the order but whose ORDER_FILLED FACT fails to
// publish must, on redelivery, complete the CommandOutcome — not silently ack
// a duplicate of an order the world was never told about.
//
// It also pins the honest limit of the fix: the stored OrderState carries no
// field for the individual Fill (fill_id, price, venue_execution_id) that
// produced the terminal state — only the cumulative filled_quantity/
// leaves_quantity/average_fill_price (order_events.proto:78-85). That Fill
// lived only in work()'s loop-local `fill` variable and is gone the moment
// the interrupted call returns, so the ORDER_FILLED FACT itself can never be
// reconstructed here. Only the CommandOutcome is recoverable, and that is all
// this fix re-publishes.
func TestSubmit_ResumesInterruptedFillAnnouncement_FillFactFails(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)
	cmd := limitOrder(d(100, 0), d(1025, -2)) // marketable ⇒ fills in full, one fill

	// Delivery 1: the fill folds and Saves FILLED, but the ORDER_FILLED FACT
	// fails to publish — a broker blip between the Save and the emit.
	fb.failOn = EventTypeFilled
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected EmitFill failure to surface " +
			"(a real bus.Publish failure must nack, not ack, so the broker redelivers)")
	}
	fb.failOn = ""

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status after delivery 1 = %v, want FILLED — Save happened before the failed EmitFill", st.GetStatus())
	}
	if st.GetOutcomeAnnouncedAt() != nil {
		t.Fatal("outcome_announced_at is set after delivery 1 — it must only be stamped once the " +
			"FACT/outcome are actually published, and EmitFill just failed")
	}
	if fb.last(EventTypeFilled) != nil {
		t.Fatal("an ORDER_FILLED FACT was recorded despite the injected publish failure — the fake is broken, not the handler")
	}
	if fb.last(EventTypeOutcome) != nil {
		t.Fatal("a CommandOutcome was recorded on delivery 1 — handleSubmit must not reach the trailing " +
			"EmitOutcome once work() has already failed")
	}

	// Delivery 2: the redelivery. The order is already FILLED with no
	// outcome_announced_at — an interrupted announcement, not a duplicate.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED — the order DID fill and the caller must not be left "+
			"with no outcome at all", oc.GetStatus())
	}

	// THE HONEST LIMIT: the ORDER_FILLED FACT is still never published. The
	// original Fill was never recoverable from stored state, so re-emitting it
	// would mean fabricating one — this fix does not do that.
	if fb.last(EventTypeFilled) != nil {
		t.Fatal("an ORDER_FILLED FACT was published on resume — the original Fill is not recoverable " +
			"from stored OrderState, so this must NOT happen; a passing assertion here would mean a " +
			"fabricated fill was invented")
	}

	st, _, err = store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 2: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status after delivery 2 = %v, want FILLED", st.GetStatus())
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at is still unset after a successful resume — a further redelivery " +
			"would re-run this same path forever instead of hitting the terminal-duplicate guard")
	}
}

// TestSubmit_ResumesInterruptedFillAnnouncement_OutcomeFails covers the other
// half of the same bug shape: the ORDER_FILLED FACT succeeds but the trailing
// CommandOutcome fails. Resume must complete the outcome WITHOUT re-emitting
// the FACT a second time.
func TestSubmit_ResumesInterruptedFillAnnouncement_OutcomeFails(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)
	cmd := limitOrder(d(100, 0), d(1025, -2))

	fb.failOn = EventTypeOutcome
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected trailing EmitOutcome failure to surface")
	}
	fb.failOn = ""

	if fb.last(EventTypeFilled) == nil {
		t.Fatal("ORDER_FILLED FACT missing after delivery 1 — expected EmitFill to have succeeded " +
			"before the injected outcome failure")
	}
	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() != nil {
		t.Fatal("outcome_announced_at is set after delivery 1 — the trailing EmitOutcome just failed")
	}

	filledFactsBefore := 0
	for _, et := range fb.types() {
		if et == EventTypeFilled {
			filledFactsBefore++
		}
	}

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome after resume = %v, want EXECUTED", oc.GetStatus())
	}
	filledFactsAfter := 0
	for _, et := range fb.types() {
		if et == EventTypeFilled {
			filledFactsAfter++
		}
	}
	if filledFactsAfter != filledFactsBefore {
		t.Fatalf("ORDER_FILLED facts = %d before resume, %d after — resume must NEVER re-emit a FACT "+
			"that already succeeded", filledFactsBefore, filledFactsAfter)
	}

	st, _, err = store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 2: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at still unset after a successful resume")
	}
}

// TestSubmit_DuplicateAfterFillAnnouncedStaysTerminal preserves the genuine-
// duplicate case: a redelivery of a SubmitOrder whose fill was fully and
// successfully announced must still be a silent no-op, not a re-drive or a
// re-announcement.
func TestSubmit_DuplicateAfterFillAnnouncedStaysTerminal(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)
	cmd := limitOrder(d(100, 0), d(1025, -2))

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at not stamped after a fully successful fill")
	}
	before := len(fb.types())

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(fb.types()); got != before {
		t.Fatalf("redelivery of an already-announced FILLED order emitted %d new events, want 0", got-before)
	}
}

// TestSubmit_ResumesInterruptedRejectAnnouncement pins the ErrUnpriced reject
// path: a market order the sim venue cannot price is Saved REJECTED before
// its announcement, and if that announcement (refuse's EmitRejected or the
// outcome it emits) then fails, the redelivery must complete the rejection
// rather than silently acking a "duplicate" the caller was never told about.
func TestSubmit_ResumesInterruptedRejectAnnouncement(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)
	cmd := marketOrder() // the sim venue has no PriceFunc ⇒ ErrUnpriced

	// Delivery 1: Reject() is Saved, but the ORDER_REJECTED FACT fails to publish.
	fb.failOn = EventTypeRejected
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected EmitRejected failure to surface")
	}
	fb.failOn = ""

	st, _, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatalf("status after delivery 1 = %v, want REJECTED — Save happened before the failed EmitRejected", st.GetStatus())
	}
	if st.GetOutcomeAnnouncedAt() != nil {
		t.Fatal("outcome_announced_at is set after delivery 1 — EmitRejected just failed")
	}
	if fb.last(EventTypeOutcome) != nil {
		t.Fatal("a CommandOutcome was recorded on delivery 1 — refuse() must not reach EmitOutcome " +
			"once EmitRejected has already failed")
	}

	// Delivery 2: the redelivery. REJECTED with no outcome_announced_at — an
	// interrupted rejection, not a duplicate.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome after resume = %v, want REJECTED — the order WAS rejected and the caller "+
			"must not be left with no outcome at all", oc.GetStatus())
	}

	// THE HONEST LIMIT: the original PRICE_UNAVAILABLE reason/code lived only in
	// handleSubmit's local error value and is not stored on OrderState
	// (order_events.proto's OrderRejected.reason/error_code have no OrderState
	// counterpart), so the redelivery's outcome is necessarily generic, not a
	// reconstruction of the original PRICE_UNAVAILABLE rejection.
	if fb.last(EventTypeRejected) != nil {
		t.Fatal("an ORDER_REJECTED FACT was published on resume — the original reason/code is not " +
			"recoverable from stored OrderState, so this must NOT happen")
	}

	st, _, err = svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 2: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at still unset after a successful resume")
	}
}

// TestSubmit_ResumesInterruptedRejectAnnouncement_OutcomeFails covers the
// ErrUnpriced path's other failure point: EmitRejected succeeds but the
// trailing EmitOutcome fails.
func TestSubmit_ResumesInterruptedRejectAnnouncement_OutcomeFails(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)
	cmd := marketOrder()

	fb.failOn = EventTypeOutcome
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected trailing EmitOutcome failure to surface")
	}
	fb.failOn = ""

	if fb.last(EventTypeRejected) == nil {
		t.Fatal("ORDER_REJECTED FACT missing after delivery 1 — expected EmitRejected to have succeeded " +
			"before the injected outcome failure")
	}
	st, _, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() != nil {
		t.Fatal("outcome_announced_at is set after delivery 1 — the trailing EmitOutcome just failed")
	}

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome after resume = %v, want REJECTED", oc.GetStatus())
	}

	st, _, err = svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 2: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at still unset after a successful resume")
	}
}
