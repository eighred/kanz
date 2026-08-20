package order

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// THE BUG THIS FILE PINS: a transition that persisted a TERMINAL state and then
// failed to announce it. On redelivery, resume()'s old
// `if IsTerminal(st) { return nil }` acked that as a duplicate — the order sits
// FILLED or REJECTED in the store, its FACT was NEVER published, and nothing
// ever recovers it: SweepInterrupted skips terminal orders, and a DLQ replay
// re-enters the same IsTerminal trap. A completed trade that tv-sync,
// accounting, and audit never learn about.
//
// outcome_announced_at (order_events.proto) is the one marker covering all
// of those transitions — the same role venue_ack_at plays for routing and
// cancel_announced_at plays for cancellation.
//
// # WHAT #292 CHANGED HERE, AND WHAT IT DID NOT
//
// The fill fold no longer publishes outside its transaction: work() and adopt()
// hand the ORDER_FILLED / ORDER_PARTIALLY_FILLED record to store.Save, so the
// FACT survives a broker refusal and resume()'s flush publishes the ORIGINAL.
// The first test below therefore asserts the fill FACT DOES arrive on resume,
// which is the reverse of what it asserted before, and the reverse of what
// completeTerminalOutcome's comment says is possible for a fill folded before
// migration 0006.
//
// BOTH REJECT PATHS ARE NOW CONVERTED TOO. The ErrUnpriced reject and adopt()'s
// VENUE_REJECTED reject each hand ORDER_REJECTED and the outcome to the same
// store.Save that writes the terminal state, and stamp outcome_announced_at in
// it. The reason and error_code still exist only in a local variable — that is
// the point, and it is exactly why they had to stop being a second write:
// completeTerminalOutcome cannot rebuild either FACT and says so.
//
// WHAT STAYS UNCONVERTED, and what these tests still pin as an honest limit: the
// pre-outbox fill case. A fill folded before migration 0006 has no record behind
// it, so completeTerminalOutcome re-publishes a generic outcome and logs at
// ERROR that the ORDER_FILLED FACT is gone. That is a property of old DATA, not
// of the code, and no conversion can retire it.

// TestSubmit_ResumesInterruptedFillAnnouncement_FillFactFails is the SAME
// interruption as before — a fill that FILLS the order and whose ORDER_FILLED
// FACT the broker refuses — and it now asserts the OPPOSITE of what it used to.
//
// # WHAT THIS TEST USED TO SAY, AND WHY IT NO LONGER SAYS IT
//
// It used to end by asserting that no ORDER_FILLED FACT is published on resume,
// with the reasoning that the individual Fill (fill_id, price,
// venue_execution_id) lived only in work()'s loop variable and was gone the
// moment the interrupted call returned. That reasoning was correct, and
// completeTerminalOutcome still carries it for the ORDER_REJECTED case. It
// stopped being true for fills when the fold started committing its FACT to the
// outbox in the same transaction as the state (#292): the Fill is now IN THE
// TABLE, byte for byte, and the relay publishes the original.
//
// So the assertion inverts. The fill FACT MUST appear on resume, it must appear
// BEFORE the CommandOutcome that follows it, and it must be the real one rather
// than a reconstruction — which is checkable, because a fabricated fill is
// exactly what the old code could not produce and the new code never has to.
//
// This is the assertion that distinguishes #292's fill conversion from a
// latency change. It is the one FACT in this service that no marker and no
// compensator could ever rebuild, and it is now recoverable.
func TestSubmit_ResumesInterruptedFillAnnouncement_FillFactFails(t *testing.T) {
	fb := &fakeBus{}
	// A capturing logger, because the SIGNAL is part of the behaviour here.
	// completeTerminalOutcome logs at ERROR that the fill is unrecoverable, and
	// that line is what an operator would page on. Emitting it on a successful
	// recovery is CLAUDE.md's "nothing configured and checked-and-fine must never
	// look the same", pointed the other way: an alert that fires on every healthy
	// recovery stops being read.
	logs := &captureHandler{}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, slog.New(logs),
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cmd := limitOrder(d(100, 0), d(1025, -2)) // marketable ⇒ fills in full, one fill

	// Delivery 1: the fill folds and Saves FILLED, but the ORDER_FILLED FACT
	// fails to publish — a broker blip between the commit and the flush.
	fb.failOn = EventTypeFilled
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected fill-publish failure to surface " +
			"(a real bus.Publish failure must nack, not ack, so the broker redelivers)")
	}
	fb.failOn = ""

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status after delivery 1 = %v, want FILLED — the fold committed before the failed publish", st.GetStatus())
	}
	if st.GetOutcomeAnnouncedAt() != nil {
		t.Fatal("outcome_announced_at is set after delivery 1 — it must only be stamped once the " +
			"FACT/outcome are actually published, and the fill publish just failed")
	}
	if fb.last(EventTypeFilled) != nil {
		t.Fatal("an ORDER_FILLED FACT was recorded despite the injected publish failure — the fake is broken, not the handler")
	}
	if fb.last(EventTypeOutcome) != nil {
		t.Fatal("a CommandOutcome was recorded on delivery 1 — handleSubmit must not reach the trailing " +
			"EmitOutcome once work() has already failed")
	}
	// AND THE FACT IS NOT LOST — it is committed, waiting. This is the assertion
	// that separates "the publish failed" from "the fill is gone", and before
	// #292's fill conversion there was nothing here to assert.
	if got := store.outbox.PendingCount(); got != 1 {
		t.Fatalf("outbox holds %d unpublished records after the fill publish failed, want 1. The fill "+
			"FACT carries the only copy of fill_id, price and venue_execution_id this platform has; "+
			"if it is not in the table it does not exist anywhere (#292)", got)
	}

	// Delivery 2: the redelivery. The order is already FILLED with no
	// outcome_announced_at — an interrupted announcement, not a duplicate.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("delivery 2 (resume) returned %v, want nil", err)
	}

	// THE FILL FACT ITSELF ARRIVES, AND AHEAD OF THE OUTCOME. Order matters for
	// the same reason it does at admission: a consumer folding the command
	// outcome for an execution it has not been told about is folding this order's
	// history out of sequence.
	types := fb.types()
	filledAt := indexOf(types, EventTypeFilled)
	if filledAt == -1 {
		t.Fatalf("no ORDER_FILLED FACT after the resume: %v.\n\n"+
			"The fill was committed to the outbox with the state that recorded it, so the resume's "+
			"flush must publish the ORIGINAL — this is the FACT completeTerminalOutcome could never "+
			"rebuild, and the whole point of putting the fold in the transaction (#292)", types)
	}
	if outcomeAt := indexOf(types, EventTypeOutcome); outcomeAt != -1 && outcomeAt < filledAt {
		t.Fatalf("the CommandOutcome preceded the ORDER_FILLED FACT it reports: %v", types)
	}
	// IT IS THE REAL FILL, NOT A RECONSTRUCTION. A fabricated one is what the old
	// code could not honestly produce; this asserts the platform did not start.
	filled, ok := fb.last(EventTypeFilled).(*orderpb.OrderFilled)
	if !ok {
		t.Fatalf("the published fill FACT is %T, want *orderpb.OrderFilled", fb.last(EventTypeFilled))
	}
	if filled.GetFill().GetFillId() == "" {
		t.Fatal("the recovered ORDER_FILLED FACT carries no fill_id — a fill FACT without one is not " +
			"the venue's execution, it is an invention, and the position book claims fill_id to avoid " +
			"double-counting")
	}
	if got := store.outbox.PendingCount(); got != 0 {
		t.Fatalf("%d records still unpublished after the resume, want 0", got)
	}
	// AND NOTHING REPORTED IT AS LOST. This is the difference between recovering
	// a fill and merely publishing one: the operator-facing signal has to agree
	// with what happened.
	if line, found := logs.find(slog.LevelError, "WITHOUT its ORDER_FILLED FACT"); found {
		t.Fatalf("an ERROR says the fill was unrecoverable on a resume that just published it from "+
			"the outbox: %q.\n\nThat line is what an operator pages on. Firing it on a healthy "+
			"recovery is how a real lost fill stops being noticed (#292).", line)
	}

	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED — the order DID fill and the caller must not be left "+
			"with no outcome at all", oc.GetStatus())
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

// THE PRE-OUTBOX POPULATION STILL HAS THE OLD LIMIT, AND IT MUST STAY PROVEN.
//
// An order FILLED before migration 0006 has its terminal state committed and
// NOTHING in the outbox behind it, because there was no outbox. For those,
// completeTerminalOutcome still cannot rebuild the ORDER_FILLED FACT and must
// not try — a fabricated fill on the record is worse than a missing one. This
// seeds exactly that row rather than driving the live path, which can no longer
// produce it, for the same reason TestAcceptedOrderWithoutFactIsReconciled
// seeds its own.
//
// Deleting this alongside the inversion above would have retired the honest
// limit on the strength of a replacement that does not cover this population.
func TestResumeDoesNotFabricateAFillForAPreOutboxOrder(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	logs := &captureHandler{}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")}), nil, slog.New(logs),
		WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// A FILLED order with outcome_announced_at unset and no outbox record: what
	// an interrupted delivery left behind before the fold rode a transaction.
	admitted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := store.Create(ctx, admitted, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	loaded, ver, err := store.Load(ctx, admitted.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	terminal := cloneState(loaded)
	terminal.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	if err := store.Save(ctx, terminal, ver, nil); err != nil {
		t.Fatalf("Save terminal: %v", err)
	}
	if got := store.outbox.PendingCount(); got != 0 {
		t.Fatalf("outbox holds %d records for a seeded pre-outbox order, want 0 — this test would "+
			"otherwise prove the recovery path, not the limit", got)
	}

	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("redelivery of a terminal-but-unannounced order: %v", err)
	}

	if fb.last(EventTypeFilled) != nil {
		t.Fatal("an ORDER_FILLED FACT was published for an order with NOTHING in the outbox. There is " +
			"no stored copy of the fill_id, price or venue_execution_id, so this FACT can only have " +
			"been invented — invented data on the trade record is worse than a missing FACT")
	}
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok {
		t.Fatal("no CommandOutcome for a terminal-but-unannounced order — the caller must still be told")
	}
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
	// AND IT IS SAID OUT LOUD. This is the residual gap #292 does not close for
	// the pre-outbox population, and it must stay loud — a lost fill nobody is
	// told about is the failure this whole path exists to end. It is also the
	// non-vacuity twin of the assertion in the test above that this same line
	// does NOT fire on a recovery: one of the two proves the matcher matches.
	if _, found := logs.find(slog.LevelError, "WITHOUT its ORDER_FILLED FACT"); !found {
		t.Fatalf("no ERROR reported the unrecoverable fill. Logged instead: %v.\n\n"+
			"For this population there IS no copy of the fill anywhere, so the only thing the "+
			"platform can do is say so", logs.messages())
	}
}

// captureHandler records log records so a test can assert on the SIGNAL, not
// just the state. It exists because two of the tests in this file turn on
// whether an ERROR fires: one says a recovered fill must not be reported lost,
// the other says a genuinely lost one must be. Asserting only the state would
// let a change silently swap those.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// find returns the first message at level containing substr.
func (h *captureHandler) find(level slog.Level, substr string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return r.Message, true
		}
	}
	return "", false
}

func (h *captureHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, r := range h.records {
		out = append(out, r.Level.String()+": "+r.Message)
	}
	return out
}

// TestSubmit_InterruptedTrailingOutcomeIsDeferredNotLost covers the other half
// of the same bug shape: the ORDER_FILLED FACT succeeds and the trailing
// CommandOutcome fails.
//
// THIS PAIR WAS THE LAST ONE #292 NAMED, and it was a decision rather than a
// mechanical conversion — the outcome reports state work() committed in
// transactions that had already closed, so there was no pending write to hand it
// to. The answer: on the FILLED branch it rides the write this path was already
// doing, the outcome_announced_at stamp. Its predecessor asserted the old limit
// honestly — that the marker must stay UNSET after a failed publish, because it
// was the only evidence resume() had that the announcement was owed. The outbox
// row is that evidence now, so the marker commits with it and a redelivery is
// the genuine duplicate it always was.
func TestSubmit_InterruptedTrailingOutcomeIsDeferredNotLost(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)
	cmd := limitOrder(d(100, 0), d(1025, -2))

	fb.failOn = EventTypeOutcome
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("delivery 1 returned nil, want the injected trailing outcome failure to surface")
	}
	fb.failOn = ""

	// The fill's own FACT flushed inside work(), ahead of the outcome.
	if fb.last(EventTypeFilled) == nil {
		t.Fatal("ORDER_FILLED FACT missing after delivery 1 — the fill is flushed in work(), before " +
			"the trailing outcome this test fails")
	}
	filledFactsBefore := 0
	for _, et := range fb.types() {
		if et == EventTypeFilled {
			filledFactsBefore++
		}
	}

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after the failed publish: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at is unset.\n\n" +
			"It commits WITH the record now. Leaving it unset would send a redelivery into " +
			"completeTerminalOutcome to re-derive a GENERIC outcome for an order whose real one " +
			"is already queued.")
	}

	// NOTHING IS LOST: the outcome is queued, and the relay alone delivers it.
	pending, perr := store.Outbox().Pending(context.Background(), "o1", 10)
	if perr != nil {
		t.Fatalf("outbox pending: %v", perr)
	}
	if len(pending) != 1 {
		t.Fatalf("outbox holds %d records for o1, want 1 (the trailing outcome).\n\n"+
			"Everything before it — ACCEPTED, ROUTED, the fill — flushed on its own transition.",
			len(pending))
	}
	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome after the relay ran = %v, want EXECUTED — and EXECUTED because the order "+
			"FILLED, not the generic outcome completeTerminalOutcome would have re-derived",
			oc.GetStatus())
	}

	// A REDELIVERY IS A DUPLICATE, AND SAYS NOTHING. The marker was true before
	// the relay ran, so this holds whether or not the flush above happened.
	before := len(fb.types())
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("redelivery returned %v, want nil", err)
	}
	if got := len(fb.types()); got != before {
		t.Fatalf("the redelivery emitted %d new events, want 0", got-before)
	}
	filledFactsAfter := 0
	for _, et := range fb.types() {
		if et == EventTypeFilled {
			filledFactsAfter++
		}
	}
	if filledFactsAfter != filledFactsBefore {
		t.Fatalf("ORDER_FILLED facts = %d before the redelivery, %d after — nothing may re-emit a "+
			"FACT that already succeeded", filledFactsBefore, filledFactsAfter)
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

// THE UNPRICED REJECT IS TRANSACTIONAL (#292).
//
// This pair used to be the worst of the remaining ones. The order was Saved
// REJECTED, then refuse() published ORDER_REJECTED and the outcome. A failure
// between them left the order durably rejected with its FACT gone — and
// unrecoverably so: completeTerminalOutcome rebuilds the CommandOutcome from
// stored state but cannot rebuild ORDER_REJECTED, because the PRICE_UNAVAILABLE
// reason and code live only in handleSubmit's local error value and have no
// OrderState counterpart. The predecessors of these tests asserted exactly that
// limit, as the honest description of what recovery could not do.
//
// Both records now commit with the rejection, so the limit is gone. What these
// tests assert is the stronger property: a publish failure loses NOTHING, and
// the FACT arrives on the relay's next pass — no redelivery, no sweep, no
// restart.
func TestSubmit_UnpricedRejectSurvivesAFailedPublish(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)
	cmd := marketOrder() // the sim venue has no PriceFunc ⇒ ErrUnpriced

	// The ORDER_REJECTED publish fails. Under the old shape this was the FACT
	// nothing could ever rebuild.
	fb.failOn = EventTypeRejected
	err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd))
	fb.failOn = ""

	// THE STATE CHANGE COMMITTED. A failed announcement must not roll back a
	// rejection the venue has already made permanent.
	st, _, lerr := svc.store.Load(context.Background(), "o1")
	if lerr != nil {
		t.Fatalf("load after the failed publish: %v (handle err: %v)", lerr, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatalf("status = %v, want REJECTED — the order is unpriceable, and leaving it working "+
			"would have a later cancel act on a live-looking order", st.GetStatus())
	}

	// AND NOTHING IS LOST: both records are queued, holding the original reason.
	pending, perr := svc.store.(*MemoryStore).Outbox().Pending(context.Background(), "o1", 10)
	if perr != nil {
		t.Fatalf("outbox pending: %v", perr)
	}
	if len(pending) != 2 {
		t.Fatalf("outbox holds %d records for o1, want 2 (ORDER_REJECTED + the outcome).\n\n"+
			"This is the whole of #292 for this path: the FACT is committed with the state change, "+
			"so a publish failure defers it instead of destroying it.", len(pending))
	}

	// The relay drains it — no redelivery of the command, no sweep, no restart.
	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	rej := fb.last(EventTypeRejected)
	if rej == nil {
		t.Fatal("ORDER_REJECTED never reached the bus after the relay ran — the record was queued " +
			"and then not published, which is a worse failure than the one this replaced")
	}
	if got := rej.(*orderpb.OrderRejected).GetErrorCode(); got != "PRICE_UNAVAILABLE" {
		t.Errorf("error_code = %q, want PRICE_UNAVAILABLE.\n\n"+
			"The ORIGINAL reason survived the failure. Recovery from stored state could never "+
			"produce this — it is not on OrderState — which is why the pre-outbox test asserted "+
			"the FACT must NOT be republished at all.", got)
	}
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome after the relay ran = %v, want REJECTED — the caller must still be told", oc.GetStatus())
	}
}

// A REDELIVERY AFTER THE FAILURE IS A DUPLICATE, NOT AN INTERRUPTED REJECTION.
//
// outcome_announced_at is stamped in the same write as the records, so it is
// true the moment it is durable. That is what lets resume() treat a redelivery
// as the genuine duplicate it now is: the announcement is guaranteed by the
// outbox, whether or not it has flushed yet. Under the old shape the marker was
// a THIRD write after two publishes, and a redelivery arriving in between
// re-entered the reject path.
func TestSubmit_UnpricedRejectRedeliveryDoesNotReAnnounce(t *testing.T) {
	fb := &fakeBus{}
	svc := simService(t, fb)
	cmd := marketOrder()

	fb.failOn = EventTypeRejected
	_ = svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd))
	fb.failOn = ""

	st, _, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at is unset.\n\n" +
			"It commits WITH the records now. Leaving it unset would send a redelivery back into " +
			"the reject path to announce a rejection the outbox already holds — a duplicate FACT " +
			"for an order that was only ever rejected once.")
	}

	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	before := len(fb.types())

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("redelivery returned %v, want nil — a duplicate SubmitOrder is acked", err)
	}
	if got := len(fb.types()); got != before {
		t.Fatalf("the redelivery emitted %d new events, want 0 — the rejection was already "+
			"announced, so re-announcing it duplicates a terminal FACT", got-before)
	}
}

// rejectingVenue acknowledges an execute without recording anything (the same
// shape as amnesiacVenue and twoFillVenue, so the order reaches ROUTED with
// venue_ack_at set) and then, when queried, reports that it REJECTED the order.
// It is the venue answer that drives adopt()'s VENUE_REJECTED branch — the
// recovery path's own terminal transition.
type rejectingVenue struct {
	*execution.SimVenue
	reason string
}

func (v *rejectingVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil
}

func (v *rejectingVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewRejected, Reason: v.reason}, nil
}

// adoptedRejectionService wires a Service onto a venue that will report the
// order rejected when asked, and drives it to ROUTED + acknowledged so the next
// delivery reconciles.
func adoptedRejectionService(t *testing.T, fb *fakeBus, reason string) (*Service, *MemoryStore, []byte) {
	t.Helper()
	venue := &rejectingVenue{SimVenue: execution.NewSimVenue("XSIM"), reason: reason}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	body := mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))
	if err := svc.Handle(testCtx(), submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load after delivery 1: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after delivery 1 — the adopt arm below needs the order " +
			"routed and acknowledged before the redelivery queries the venue")
	}
	return svc, store, body
}

// THE VENUE'S REJECTION IS TRANSACTIONAL (#292), AND IT MATTERS MOST ON THIS
// PATH.
//
// adopt() runs during RECOVERY: the process reaching it is one that already
// crashed once, and the venue's answer is the whole reason the reconciliation
// exists. Under the old shape it was Save(REJECTED) -> refuse()'s two publishes
// -> a third Save stamping outcome_announced_at, and a failure between them left
// the order durably REJECTED with its ORDER_REJECTED FACT gone —
// completeTerminalOutcome rebuilds the CommandOutcome from stored state and says
// in its own comment that it cannot rebuild the rejection, because the venue's
// reason has no OrderState counterpart.
//
// So this asserts the same property the ErrUnpriced pair does: a publish failure
// loses NOTHING, and the FACT arrives on the relay's next pass carrying the
// VENUE'S OWN reason — no redelivery, no sweep, no restart.
func TestAdopt_VenueRejectionSurvivesAFailedPublish(t *testing.T) {
	fb := &fakeBus{}
	const reason = "insufficient margin at the exchange"
	svc, store, body := adoptedRejectionService(t, fb, reason)

	// The redelivery reconciles, the venue says REJECTED, and the ORDER_REJECTED
	// publish fails. Under the old shape this was the FACT nothing could rebuild.
	fb.failOn = EventTypeRejected
	err := svc.Handle(testCtx(), submitEnv(), body)
	fb.failOn = ""

	// THE STATE CHANGE COMMITTED. A failed announcement must not roll back a
	// rejection the venue has already made permanent — leaving the order ROUTED
	// would have a later cancel act on an order the exchange has thrown away.
	st, _, lerr := store.Load(context.Background(), "o1")
	if lerr != nil {
		t.Fatalf("load after the failed publish: %v (handle err: %v)", lerr, err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatalf("status = %v, want REJECTED — adopt takes the venue's truth as ours", st.GetStatus())
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at is unset.\n\n" +
			"It commits WITH the records now. Leaving it unset would send a redelivery back into " +
			"the reconcile path to re-announce a rejection the outbox already holds.")
	}

	// AND NOTHING IS LOST: both records are queued, holding the venue's reason.
	pending, perr := store.Outbox().Pending(context.Background(), "o1", 10)
	if perr != nil {
		t.Fatalf("outbox pending: %v", perr)
	}
	if len(pending) != 2 {
		t.Fatalf("outbox holds %d records for o1, want 2 (ORDER_REJECTED + the outcome).\n\n"+
			"This is the whole of #292 for this path: the FACTs commit with the state change, so a "+
			"publish failure defers them instead of destroying them.", len(pending))
	}

	// The relay drains it — no redelivery of the command, no sweep, no restart.
	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	rej, ok := fb.last(EventTypeRejected).(*orderpb.OrderRejected)
	if !ok {
		t.Fatal("ORDER_REJECTED never reached the bus after the relay ran — the record was queued " +
			"and then not published, which is worse than the failure this replaced")
	}
	if rej.GetErrorCode() != "VENUE_REJECTED" {
		t.Errorf("error_code = %q, want VENUE_REJECTED", rej.GetErrorCode())
	}
	if rej.GetReason() != reason {
		t.Errorf("reason = %q, want %q.\n\n"+
			"THE VENUE'S OWN ACCOUNT SURVIVED THE FAILURE. Recovery from stored state could never "+
			"produce this — it is not on OrderState — which is why completeTerminalOutcome refuses "+
			"to re-emit an ORDER_REJECTED at all.", rej.GetReason(), reason)
	}
	oc, ok := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if !ok || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome after the relay ran = %v, want REJECTED — the caller must still be told", oc.GetStatus())
	}
	if oc.GetErrorCode() != "VENUE_REJECTED" {
		t.Errorf("outcome error_code = %q, want VENUE_REJECTED", oc.GetErrorCode())
	}
}

// A REDELIVERY AFTER THE FAILURE IS A DUPLICATE, NOT AN INTERRUPTED REJECTION —
// the adopt-path twin of TestSubmit_UnpricedRejectRedeliveryDoesNotReAnnounce.
// outcome_announced_at commits with the records, so resume() recognises the
// order as terminal-and-announced whether or not the relay has flushed yet.
func TestAdopt_VenueRejectionRedeliveryDoesNotReAnnounce(t *testing.T) {
	fb := &fakeBus{}
	svc, _, body := adoptedRejectionService(t, fb, "insufficient margin at the exchange")

	fb.failOn = EventTypeRejected
	_ = svc.Handle(testCtx(), submitEnv(), body)
	fb.failOn = ""

	if _, ferr := svc.relay.Flush(context.Background(), "o1"); ferr != nil {
		t.Fatalf("relay flush: %v", ferr)
	}
	before := len(fb.types())

	if err := svc.Handle(testCtx(), submitEnv(), body); err != nil {
		t.Fatalf("redelivery returned %v, want nil — a duplicate SubmitOrder is acked", err)
	}
	if got := len(fb.types()); got != before {
		t.Fatalf("the redelivery emitted %d new events, want 0 — the rejection was already "+
			"announced, so re-announcing it duplicates a terminal FACT", got-before)
	}
}
