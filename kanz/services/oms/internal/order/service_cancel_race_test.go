package order

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
)

// THE CANCEL-RESURRECTION TESTS.
//
// A cancel landing while the OMS is mid-execution on the same order used to be
// able to drive a CANCELLED order back to FILLED. The mechanism is worth stating
// once, because it is NOT a missing terminal guard:
//
//   work() captures the order state, calls venue.Execute, and folds the fills it
//   gets back into THAT CAPTURED SNAPSHOT. ApplyFill's IsTerminal check is real
//   and correct, but it is evaluated against a snapshot taken before the cancel
//   existed, so it passes. store.Save is a blind upsert with no version
//   predicate, so FILLED is written over CANCELLED — and the ORDER_CANCELLED
//   FACT has already gone out to every downstream projection.
//
// It is reachable in production, not just in principle: submit, amend and cancel
// arrive on three separate NATS durables with three cursors and three dispatch
// goroutines (pkg/bus/nats.go), so they are concurrent by construction. The
// partition_key does not serialize them — nothing in the consumer reads it for
// ordering.
//
// These tests park the venue inside Execute so the interleaving is CHOSEN rather
// than raced for. Without that, the window is a real network call wide and a
// test would pass or fail by luck.

// gatedVenue parks inside Execute until the test releases it. It also satisfies
// execution.Closer, so a cancel that WRONGLY proceeds reaches CancelOrder and is
// recorded — a withdrawal dispatched to an exchange for an order it has already
// filled is a real message to a real venue, and the tests assert it never
// happens.
type gatedVenue struct {
	mic     string
	entered chan struct{} // closed on the first Execute
	release chan struct{} // closed by the test to let Execute return
	fills   []*orderpb.Fill

	mu        sync.Mutex
	executes  int
	cancelled []string
	once      sync.Once
}

func newGatedVenue(fills ...*orderpb.Fill) *gatedVenue {
	return &gatedVenue{
		mic:     "XSIM",
		entered: make(chan struct{}),
		release: make(chan struct{}),
		fills:   fills,
	}
}

func (v *gatedVenue) MIC() string     { return v.mic }
func (v *gatedVenue) Account() string { return "acct-" + v.mic }

func (v *gatedVenue) Execute(ctx context.Context, _ *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.executes++
	v.mu.Unlock()
	v.once.Do(func() { close(v.entered) })
	select {
	case <-v.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return v.fills, nil
}

func (v *gatedVenue) CancelOrder(_ context.Context, st *orderpb.OrderState) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cancelled = append(v.cancelled, st.GetOrderId())
	return nil
}

func (v *gatedVenue) venueCancels() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.cancelled...)
}

func (v *gatedVenue) executeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.executes
}

// count returns how many events of one type were published.
func (f *fakeBus) count(eventType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

// fullFill is a fill that closes out limitOrder's 100 units. ExecutedAt is not
// optional: Emitter.EmitFill uses it as the envelope EventTime and fakeBus
// rejects a zero one, exactly as the real producer would.
func fullFill() *orderpb.Fill {
	return &orderpb.Fill{
		FillId:     "f1",
		OrderId:    "o1",
		Quantity:   d(100, 0),
		Price:      d(1025, -2),
		ExecutedAt: timestamppb.New(t0),
	}
}

// awaitInterleave blocks until the cancel goroutine has demonstrably reached the
// order — either by registering as a waiter on the per-order lock (the fixed
// code) or by publishing its ORDER_CANCELLED FACT (the broken code).
//
// THE DOUBLE CONDITION IS WHAT MAKES THIS A REAL MUTATION PROOF. A test that
// only waited for a waiter would hang forever against the broken code and fail
// on a timeout — which proves nothing about the defect, only that something
// stalled. Waiting for EITHER means the venue is released at the same logical
// point in both worlds, so the assertions below are comparing like with like.
func awaitInterleave(t *testing.T, svc *Service, fb *fakeBus, orderID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if svc.waiters(orderID) >= 2 || fb.count(EventTypeCancelled) > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the cancel never reached the order at all — it neither queued on the " +
				"per-order lock nor announced a cancellation, so this test would prove nothing")
		case <-time.After(200 * time.Microsecond):
		}
	}
}

// A cancel arriving while the venue is mid-execution must not end with the
// ledger contradicting the FACTs. The venue filled the order, so FILLED is the
// truth; the cancel must lose, and must lose LOUDLY — refused as terminal, with
// no ORDER_CANCELLED published and no withdrawal sent to the exchange.
func TestCancel_ArrivingMidExecutionCannotResurrectACancelledOrder(t *testing.T) {
	fb := &fakeBus{}
	venue := newGatedVenue(fullFill())
	reg := execution.NewCloseRegistry()
	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil, execution.NewRouter(venue), reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	var wg sync.WaitGroup
	submitErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		submitErr <- svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2))))
	}()

	<-venue.entered // the order is at the exchange and work() holds the lock

	cancelErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		cancelErr <- svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1")))
	}()

	awaitInterleave(t, svc, fb, "o1")
	close(venue.release)
	wg.Wait()

	if err := <-submitErr; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := <-cancelErr; err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// THE DISCRIMINATING ASSERTION. The stored status is FILLED under BOTH the
	// bug and the fix — which is exactly why this defect survived the existing
	// suite, and why the FACT, outcome and venue assertions are the ones that
	// carry the proof, not the status.
	if got := fb.count(EventTypeCancelled); got != 0 {
		t.Errorf("%d ORDER_CANCELLED facts were published for an order the venue FILLED, want 0 — "+
			"every downstream projection has now been told this order was withdrawn, while the "+
			"store says it traded", got)
	}
	if got := venue.venueCancels(); len(got) != 0 {
		t.Errorf("venue cancels = %v, want none — a withdrawal was dispatched to a real exchange "+
			"for an order it had already filled", got)
	}

	st, _, err := svc.store.Load(testCtx(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Errorf("stored status = %v, want FILLED — the venue traded, so the ledger's last word "+
			"must be the trade", st.GetStatus())
	}
	if st.GetQuarantine() != nil {
		t.Errorf("order was quarantined (%q) — nothing here is ambiguous", st.GetQuarantine().GetReason())
	}
	if st.GetCancelAnnouncedAt() != nil {
		t.Error("cancel_announced_at is stamped on a filled order — a cancellation was announced")
	}

	// The cancel must still be ANSWERED: refused as terminal, through the
	// existing aggregate guard, not silently dropped.
	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil {
		t.Fatal("no outcome was emitted for the cancel — it was silently dropped")
	}
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Errorf("cancel outcome = %v, want REJECTED — the order had already filled when the "+
			"cancel got the lock, and the caller must be told so", oc.GetStatus())
	}
	if oc.GetErrorCode() != "ORDER_TERMINAL" {
		t.Errorf("cancel error code = %q, want ORDER_TERMINAL", oc.GetErrorCode())
	}

	if reg.Len() != 0 {
		t.Errorf("close registry holds %d entries, want 0 — nothing was dispatched, so the "+
			"healing watchdog has nothing to own", reg.Len())
	}
	if got := venue.executeCount(); got != 1 {
		t.Errorf("venue executed %d times, want 1", got)
	}
	if got := svc.waiters("o1"); got != 0 {
		t.Errorf("lock table still holds %d refs for o1, want 0", got)
	}
}

// The same defect pointed the other way. Amend() clones the state it was handed
// and rewrites ordered/leaves quantity while carrying that snapshot's STATUS
// along — so an amend built on a pre-fill read Saves ROUTED with a positive
// leaves quantity over a FILLED order. A completed trade, un-filled in the
// ledger, with an open quantity the exchange does not have.
func TestAmend_ArrivingMidExecutionCannotUnfillAFilledOrder(t *testing.T) {
	fb := &fakeBus{}
	venue := newGatedVenue(fullFill())
	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	var wg sync.WaitGroup
	submitErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		submitErr <- svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2))))
	}()

	<-venue.entered

	amend := &orderpb.AmendOrder{
		OrderId:     "o1",
		NewQuantity: d(200, 0),
		Metadata: &commandpb.CommandMetadata{
			Issuer: "user:owner", TargetId: "o1", PrincipalPortfolios: []string{"pf1"},
		},
	}
	amendErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		amendErr <- svc.Handle(testCtx(), amendEnv(), mustMarshal(t, amend))
	}()

	// An amend publishes no FACT of its own, so the second half of the double
	// condition (see awaitInterleave) has to read the store instead: the broken
	// path Saves the rewritten quantity before work() ever finishes. Waiting for
	// EITHER signal releases the venue at the same logical point in both worlds,
	// so the assertions below are a real mutation proof rather than a timeout.
	deadline := time.After(2 * time.Second)
	for {
		if svc.waiters("o1") >= 2 {
			break
		}
		if cur, _, err := svc.store.Load(testCtx(), "o1"); err == nil &&
			dec.Cmp(cur.GetOrderedQuantity(), d(100, 0)) != 0 {
			break // the amend already applied — the defect, caught in the act
		}
		select {
		case <-deadline:
			t.Fatal("the amend never reached the order at all — it neither queued on the " +
				"per-order lock nor rewrote the stored quantity, so this test would prove nothing")
		case <-time.After(200 * time.Microsecond):
		}
	}
	close(venue.release)
	wg.Wait()

	if err := <-submitErr; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := <-amendErr; err != nil {
		t.Fatalf("amend: %v", err)
	}

	st, _, err := svc.store.Load(testCtx(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Errorf("stored status = %v, want FILLED — an amend racing the fill un-filled a "+
			"completed trade", st.GetStatus())
	}
	if dec.Cmp(st.GetOrderedQuantity(), d(100, 0)) != 0 {
		t.Errorf("ordered quantity = %v, want 100 — the amend applied to an order that had "+
			"already filled", st.GetOrderedQuantity())
	}
	if !dec.IsZero(st.GetLeavesQuantity()) {
		t.Errorf("leaves quantity = %v, want 0 — the ledger now shows an open quantity the "+
			"exchange does not have", st.GetLeavesQuantity())
	}
	if dec.Cmp(st.GetFilledQuantity(), d(100, 0)) != 0 {
		t.Errorf("filled quantity = %v, want 100", st.GetFilledQuantity())
	}

	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil {
		t.Fatal("no outcome was emitted for the amend — it was silently dropped")
	}
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Errorf("amend outcome = %v, want REJECTED", oc.GetStatus())
	}
	if oc.GetErrorCode() != "ORDER_TERMINAL" {
		t.Errorf("amend error code = %q, want ORDER_TERMINAL", oc.GetErrorCode())
	}
}

// The wait must be bounded. work() holds the lock across a real call to an
// exchange, and one bus subject is dispatched by one goroutine, so a cancel that
// waited forever on a hung venue would stall EVERY order's cancel behind it.
//
// On expiry nothing may be announced: no outcome, no FACT, no venue call. The
// handler never read the order, so it has nothing to report — a REJECTED here
// would tell the caller the ledger refused a cancel the ledger never looked at.
// The returned error nacks, and with MaxAttempts=1 and the DLQ wired the command
// parks in dlq.order.order.cancel where an operator can see and replay it.
func TestCancel_DoesNotWaitForeverOnAHungVenue(t *testing.T) {
	fb := &fakeBus{}
	venue := newGatedVenue() // never released until cleanup
	t.Cleanup(func() { close(venue.release) })

	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil,
		WithClaimWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	go func() {
		_ = svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2))))
	}()
	<-venue.entered

	before := len(fb.types())

	done := make(chan error, 1)
	go func() {
		done <- svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1")))
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned nil while the venue still held the order — it either acted " +
				"on state it could not trust, or it acked a command it never applied")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("cancel err = %v, want a wrapped context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancel never gave up — a single hung venue would stall the cancel subject's " +
			"dispatch goroutine, and with it every other order's cancel")
	}

	if got := fb.count(EventTypeCancelled); got != 0 {
		t.Errorf("%d ORDER_CANCELLED facts published after a claim timeout, want 0 — nothing "+
			"was decided, so nothing may be announced", got)
	}
	if got := len(fb.types()); got != before {
		t.Errorf("%d events published after a claim timeout, want none — the handler never read "+
			"the order, so it has nothing to report", got-before)
	}
	if got := venue.venueCancels(); len(got) != 0 {
		t.Errorf("venue cancels = %v, want none — a withdrawal was dispatched for an order whose "+
			"state the handler could not establish", got)
	}
	if got := svc.waiters("o1"); got != 1 {
		t.Errorf("refs = %d after the cancel gave up, want 1 (the work holder alone) — the "+
			"abandoned waiter leaked its reference", got)
	}
}
