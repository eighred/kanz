package order

// THE ADMISSION-SIDE DIVERGENCE (#238): store.Create and EmitAccepted are two
// independent writes with no outbox between them, so a failed publish leaves a
// durable order that no downstream service has heard of. These tests pin the
// three things that make the compensator honest — it repairs the order WITHOUT a
// restart, it does NOT repair the same order twice, and it does not touch an
// order young enough that a live delivery might still be admitting it.

import (
	"context"
	"testing"
	"time"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/eighred/kanz/internal/execution"
)

// indexOf returns the position of the first event of eventType, or -1.
func indexOf(types []string, eventType string) int {
	for i, ty := range types {
		if ty == eventType {
			return i
		}
	}
	return -1
}

// TestAcceptedOrderWithoutFactIsReconciled is the issue's own verification, run
// against the DURABLE store rather than the in-memory one.
//
//	TEST_POSTGRES_URL=… go test -p 1 -run TestAcceptedOrderWithoutFactIsReconciled ./services/oms/...
//
// It runs against Postgres deliberately, even though the logic under test is
// store-agnostic. The whole defect is that the ROW SURVIVES the publish failure:
// asserting that against a map proves the handler's control flow and nothing
// about the thing that actually strands orders in production, which is a
// committed transaction. It also exercises accepted_announced_at across a real
// marshal/unmarshal of the state proto — an additive field that read back wrong
// would silently make every recovered order look already-announced.
//
// The sequence is the failure exactly as reported: submit with the publisher
// refusing ORDER_ACCEPTED, observe the committed row and the missing FACT, then
// — with no restart and no second SubmitOrder — let the periodic sweep run and
// observe the FACT arrive.
func TestAcceptedOrderWithoutFactIsReconciled(t *testing.T) {
	pool := newPool(t) // skips unless TEST_POSTGRES_URL is set
	ctx := testCtx()

	fb := &fakeBus{failOn: EventTypeAccepted}
	store := NewPostgres(pool)
	reannounced := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_accepted_reannounced_total"})
	// A fixed clock the test advances by hand: SweepOlderThan compares the
	// order's as_of against now-minAge, and a test that slept for two real
	// minutes to clear the floor would be a test nobody runs.
	clock := t0
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil,
		WithAcceptedReannounceCounter(reannounced))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return clock }

	// 1. The submit. EmitAccepted fails, so the handler returns an error — which
	//    is what parks the command in the DLQ and acks it (MaxAttempts=1 +
	//    WithDLQ, estate-wide).
	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err == nil {
		t.Fatal("Handle returned nil after the ORDER_ACCEPTED publish failed; " +
			"a lost FACT must nack, not ack")
	}

	// 2. THE DEFECT, ASSERTED. The row is committed and durable; the FACT is not
	//    on the bus. This is the state the fund is in for as long as it lasts.
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after the failed publish: %v — the whole issue is that this row EXISTS", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("committed status = %v, want PENDING_NEW", got)
	}
	if st.GetAcceptedAnnouncedAt() != nil {
		t.Fatal("accepted_announced_at is set on an order whose ORDER_ACCEPTED publish FAILED — " +
			"the marker is what tells a compensator this order is unannounced, and it just lied")
	}
	if idx := indexOf(fb.types(), EventTypeAccepted); idx != -1 {
		t.Fatalf("an ORDER_ACCEPTED FACT was published despite the injected failure: %v", fb.types())
	}

	// 3. The broker comes back. Nothing restarts, nothing resubmits.
	fb.mu.Lock()
	fb.failOn = ""
	fb.mu.Unlock()

	// 4. The order ages past the sweep's floor, and the periodic sweep runs — the
	//    same call cmd/oms/main.go's ticker makes, in a process that never stopped.
	clock = t0.Add(10 * time.Minute)
	swept, err := svc.SweepOlderThan(ctx, 2*time.Minute)
	if err != nil {
		t.Fatalf("SweepOlderThan: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d orders, want 1", swept)
	}

	// 5. THE FACT APPEARS, and it appears BEFORE the routed FACT that follows it.
	//    Order matters downstream: tv-sync's transition() ignores an ORDER_ROUTED
	//    for an order its projection never admitted, so a routed-then-accepted
	//    sequence would leave the projection blind to a live order.
	types := fb.types()
	acceptedAt := indexOf(types, EventTypeAccepted)
	if acceptedAt == -1 {
		t.Fatalf("no ORDER_ACCEPTED FACT after the sweep: %v — the order is still one "+
			"nothing downstream has heard of", types)
	}
	if routedAt := indexOf(types, EventTypeRouted); routedAt != -1 && routedAt < acceptedAt {
		t.Fatalf("ORDER_ROUTED was published before ORDER_ACCEPTED (%v); a consumer that "+
			"never admitted the order drops the routed FACT", types)
	}
	if got := testutil.ToFloat64(reannounced); got != 1 {
		t.Fatalf("reannounce counter = %v, want 1 — a repair nobody counts is a "+
			"publish failure nobody learns about", got)
	}

	// 6. AND IT IS RECORDED, so the next pass does not say it all again.
	st, _, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after the sweep: %v", err)
	}
	if st.GetAcceptedAnnouncedAt() == nil {
		t.Fatal("accepted_announced_at is still unset after the FACT was published — " +
			"every later sweep would re-announce this order forever")
	}
}

// A SECOND PASS MUST SAY NOTHING. Without the marker the compensator cannot tell
// an unannounced order from one resting normally at PENDING_NEW — which is the
// steady state of every order in a deployment with no venue wired — so it would
// republish the entire resting book on every tick, forever. This is the test
// that fails if the marker is ever dropped or stops being persisted.
func TestSweepDoesNotReannounceAnOrderItAlreadyAnnounced(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	reannounced := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_no_reannounce_total"})
	clock := t0
	store := NewMemoryStore()
	// No router: a paper deployment, where an admitted order RESTS at PENDING_NEW
	// indefinitely. This is the deployment a naive periodic sweep floods.
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil,
		WithAcceptedReannounceCounter(reannounced))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return clock }

	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	beforeSweeps := len(fb.types())

	for i := 0; i < 3; i++ {
		clock = clock.Add(10 * time.Minute)
		if _, err := svc.SweepOlderThan(ctx, 2*time.Minute); err != nil {
			t.Fatalf("SweepOlderThan pass %d: %v", i, err)
		}
	}

	if got := len(fb.types()); got != beforeSweeps {
		t.Fatalf("three sweeps over one RESTING order published %d extra events (%v); a "+
			"resting order is not an interrupted one and must produce nothing",
			got-beforeSweeps, fb.types())
	}
	if got := testutil.ToFloat64(reannounced); got != 0 {
		t.Fatalf("reannounce counter = %v, want 0", got)
	}
}

// TOO YOUNG TO BE INTERRUPTED. handleSubmit holds NO per-order claim between
// store.Create and the claim it takes after EmitAccepted, and that window spans a
// network publish. A sweep with no age floor can walk into it, take the claim
// first, and announce an order a live delivery is still admitting — after which
// the live delivery's own ACCEPTED lands second and walks a consumer's view of
// the order backwards. The floor removes the case instead of racing it.
func TestPeriodicSweepSkipsAnOrderYoungerThanMinAge(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	clock := t0
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return clock }

	// An order admitted 30 seconds ago, stranded at PENDING_NEW with its FACT
	// unannounced — exactly what the sweep is for, but not yet.
	admitted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), clock)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := store.Create(ctx, admitted); err != nil {
		t.Fatalf("Create: %v", err)
	}
	clock = clock.Add(30 * time.Second)

	swept, err := svc.SweepOlderThan(ctx, 2*time.Minute)
	if err != nil {
		t.Fatalf("SweepOlderThan: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept %d orders, want 0 — a 30s-old order is inside the window a live "+
			"delivery may still be holding", swept)
	}
	if got := fb.types(); len(got) != 0 {
		t.Fatalf("published %v for an order too young to sweep, want nothing", got)
	}

	// Past the floor it is swept, so the skip above is a deferral and not a
	// permanent exclusion — a test that only proved "nothing happened" would pass
	// just as well against a sweep that was broken outright.
	clock = clock.Add(5 * time.Minute)
	swept, err = svc.SweepOlderThan(ctx, 2*time.Minute)
	if err != nil {
		t.Fatalf("SweepOlderThan after ageing: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d orders after the floor elapsed, want 1", swept)
	}
	if indexOf(fb.types(), EventTypeAccepted) == -1 {
		t.Fatalf("no ORDER_ACCEPTED after the order aged past the floor: %v", fb.types())
	}
}

// THE VALUE THAT DISABLES THE PRECAUTION MUST NOT BE THE DEFAULT ONE. A zero
// minAge is what a caller gets from an unset config field, and treating it as
// "no filter" would silently turn the ticker into the racing version.
func TestSweepOlderThanRefusesANonPositiveMinAge(t *testing.T) {
	svc, _ := newService(t, &fakeBus{}, nil)
	for _, minAge := range []time.Duration{0, -time.Second} {
		if _, err := svc.SweepOlderThan(testCtx(), minAge); err == nil {
			t.Fatalf("SweepOlderThan(%v) returned nil, want a refusal", minAge)
		}
	}
}

// A REDRIVEN COMMAND MUST REPAIR THE SAME GAP THE SWEEP DOES.
//
// kanz-redrive (#220) republishes a parked SubmitOrder byte for byte, so it
// reaches handleSubmit with its original bytes, finds the row store.Create
// already committed, and lands in resume(). Before this fix that path re-drove
// the order to the venue and never re-announced its admission — so an operator
// draining the DLQ got a filled order the projection had still never heard of.
// The repair lives in resume() precisely so both callers get it.
func TestRedeliveredSubmitReannouncesTheAcceptedFact(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{failOn: EventTypeAccepted}
	reannounced := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_redrive_reannounce_total"})
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil,
		WithAcceptedReannounceCounter(reannounced))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	payload := mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))
	if err := svc.Handle(ctx, submitEnv(), payload); err == nil {
		t.Fatal("Handle returned nil after the ORDER_ACCEPTED publish failed")
	}

	// The broker recovers; the operator drains the DLQ. Same bytes, same subject.
	fb.mu.Lock()
	fb.failOn = ""
	fb.mu.Unlock()
	if err := svc.Handle(ctx, submitEnv(), payload); err != nil {
		t.Fatalf("redriven Handle: %v", err)
	}

	types := fb.types()
	acceptedAt := indexOf(types, EventTypeAccepted)
	if acceptedAt == -1 {
		t.Fatalf("a redriven SubmitOrder did not re-announce the order: %v", types)
	}
	if routedAt := indexOf(types, EventTypeRouted); routedAt != -1 && routedAt < acceptedAt {
		t.Fatalf("ORDER_ROUTED preceded ORDER_ACCEPTED on the redrive path: %v", types)
	}
	if got := testutil.ToFloat64(reannounced); got != 1 {
		t.Fatalf("reannounce counter = %v, want 1", got)
	}
}

// poisonLoadStore fails Load for exactly one order id. resume() re-reads under
// the claim, so this makes that one order permanently unreconcilable while every
// other order in the book is fine.
type poisonLoadStore struct {
	Store
	poison string
}

func (p *poisonLoadStore) Load(ctx context.Context, orderID string) (*orderpb.OrderState, int64, error) {
	if orderID == p.poison {
		return nil, 0, context.DeadlineExceeded
	}
	return p.Store.Load(ctx, orderID)
}

// ONE STUCK ORDER MUST NOT SHADOW THE WHOLE BOOK, TICK AFTER TICK.
//
// The startup sweep aborts on the first order it cannot reconcile, and that is
// right there: a pod must not begin trading having skipped an order it cannot
// account for. On a ticker the same stance is self-defeating. ListByStatus
// returns ORDER BY order_id, so an order that fails every time aborts the pass
// at the same point forever and every order sorting after it is never examined
// again — while the pod stays up and the sweep appears to be running.
func TestPeriodicSweepContinuesPastAnOrderItCannotReconcile(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	clock := t0
	mem := NewMemoryStore()
	// "a-poison" sorts before "b-good", so under abort-on-first-failure the good
	// order is never reached.
	store := &poisonLoadStore{Store: mem, poison: "a-poison"}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.now = func() time.Time { return clock }

	for _, id := range []string{"a-poison", "b-good"} {
		cmd := limitOrder(d(100, 0), d(1025, -2))
		cmd.OrderId = id
		st, aerr := Accept(cmd, clock)
		if aerr != nil {
			t.Fatalf("Accept %s: %v", id, aerr)
		}
		if cerr := mem.Create(ctx, st); cerr != nil {
			t.Fatalf("Create %s: %v", id, cerr)
		}
	}
	clock = clock.Add(10 * time.Minute)

	swept, err := svc.SweepOlderThan(ctx, 2*time.Minute)
	if err == nil {
		t.Fatal("SweepOlderThan returned nil despite an order it could not reconcile — " +
			"a sweep that fails silently is the state this whole path exists to end")
	}
	if swept != 1 {
		t.Fatalf("swept %d orders, want 1 — the good order sorts AFTER the poison one and "+
			"must still have been reconciled", swept)
	}
	good, _, lerr := mem.Load(ctx, "b-good")
	if lerr != nil {
		t.Fatalf("Load b-good: %v", lerr)
	}
	if good.GetAcceptedAnnouncedAt() == nil {
		t.Fatal("the order after the poison one was never announced; one stuck order " +
			"shadowed the rest of the book")
	}

	// The startup sweep keeps the opposite, fatal stance — asserted here so the
	// two cannot silently converge on whichever one somebody edits next.
	fresh := &fakeBus{}
	svc2, err := NewService(testTenant, store, NewEmitter(fresh), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc2.now = func() time.Time { return clock }
	if swept, err := svc2.SweepInterrupted(ctx); err == nil || swept != 0 {
		t.Fatalf("SweepInterrupted = (%d, %v), want (0, an error) — a starting pod must "+
			"refuse rather than skip an order it cannot account for", swept, err)
	}
}

// A HEALTHY ADMISSION MUST STAMP THE MARKER ITSELF. If it did not, every order
// the OMS ever admitted would look unannounced to the next sweep, and the
// compensator would republish the whole open book on its first tick — the exact
// failure SweepInterrupted's status list already documents for FILLED/REJECTED.
func TestHealthySubmitStampsAcceptedAnnouncedAt(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, _, err := store.Load(ctx, "o1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetAcceptedAnnouncedAt() == nil {
		t.Fatal("accepted_announced_at is unset after a clean admission whose ORDER_ACCEPTED " +
			"was published; the next sweep would re-announce this order")
	}
}
