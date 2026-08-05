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
)

// THE BUG THIS FILE PINS: a transition that persisted a TERMINAL state and then
// failed to announce it. On redelivery, resume()'s old
// `if IsTerminal(st) { return nil }` acked that as a duplicate — the order sits
// FILLED or REJECTED in the store, its FACT was NEVER published, and nothing
// ever recovers it: SweepInterrupted skips terminal orders, and a DLQ replay
// re-enters the same IsTerminal trap. A completed trade that tv-sync,
// accounting, and audit never learn about.
//
// outcome_announced_at (order_events.proto:19) is the one marker covering all
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
// The reject paths are UNCONVERTED. The ErrUnpriced reject and adopt()'s
// VENUE_REJECTED reject still Save and then refuse(), and the reason/error_code
// still exist only in a local variable — so those tests still pin the old,
// honest limit, and so does the pre-outbox fill case.

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
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, slog.New(logs))
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
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, slog.New(logs))
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
