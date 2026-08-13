package order

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/internal/compliance"
)

// testTenant is the tenant these OMS instances serve. Handle refuses an envelope
// from any other tenant (#223), so the fixtures below publish as this one.
const testTenant = "__system__"

// testCtx stands in for what bus.Consumer would have stashed onto a handler's
// ctx from the inbound envelope (pkg/bus/context.go's WithTenantID) before the
// handler ever runs. A direct svc.Handle(ctx, ...) call in these tests bypasses
// the Consumer, so the test must supply the tenant a real delivery would have
// carried — exactly as cmd/oms/main.go does for the real startup sweep.
func testCtx() context.Context {
	return bus.WithTenantID(context.Background(), "test-tenant")
}

// fakeBus records published events for assertion. Publish enforces exactly the
// caller-facing preconditions the real bus.Producer enforces (pkg/bus/producer.go
// publish/stamp; pkg/bus/validate.go Validate) — the fields a handler, not the
// producer, is responsible for getting right. It deliberately does NOT run the
// full bus.Validate: most envelope fields (event_id, schema_version,
// publish_time, payload_schema_ref, producer_sequence) are stamped by
// Producer.stamp deterministically and cannot be wrong from a handler's side,
// so asserting them here would validate nothing a handler could ever break.
//
// This exists because a lenient fake once let a Critical bug reach a live
// cluster with the entire Go suite green: the OMS startup sweep published with
// no tenant on the context, the real broker rejected it with "envelope
// validation: tenant_id required", and because a sweep failure is fatal at
// startup, the OMS crash-looped forever on exactly the orders the sweep exists
// to rescue. A double that accepts what the real broker rejects certifies
// nothing.
type fakeBus struct {
	mu     sync.Mutex
	events []bus.Event
	// tenant, if set, stands in for ProducerConfig.Tenant — the producer's
	// last-resort tenant fallback (stamp's precedence: Event.TenantID > ctx >
	// ProducerConfig.Tenant).
	//
	// THE ZERO VALUE IS DELIBERATELY STRICTER THAN THE REAL PRODUCER, and that
	// divergence is the point. The OMS's producer does set ProducerConfig.Tenant
	// (cmd/oms/main.go), so in production a publish that reaches it with no tenant
	// no longer fails — it is silently stamped with the deployment's own tenant.
	// That converts a loud crash into a quiet MISLABEL, which is the worse failure
	// of the two: a FACT attributed to the wrong tenant is wrong forever, and
	// nothing downstream can tell. Leaving the fallback unset here keeps that
	// mislabel visible in tests, where it is still cheap to fix.
	//
	// So the fallback exists in production as a safety net and is withheld here as
	// a detector. Only set this in a test that deliberately exercises the fallback
	// itself.
	tenant string

	// failOn, if set, is the EventType whose Publish fails — modeling a broker
	// outage that interrupts one specific FACT/outcome mid-handler, so a test can
	// pin exactly which publish a handler's error-handling must survive (e.g. the
	// ORDER_CANCELLED FACT succeeding but the CommandOutcome after it failing).
	// The event is NOT recorded when it fails, matching a real Publish that never
	// reached the broker.
	failOn  string
	failErr error
}

func (f *fakeBus) Publish(ctx context.Context, e bus.Event) error {
	f.mu.Lock()
	failOn, failErr := f.failOn, f.failErr
	f.mu.Unlock()
	if failOn != "" && e.EventType == failOn {
		if failErr != nil {
			return failErr
		}
		return errors.New("fakeBus: injected publish failure for " + failOn)
	}
	// Rule 1 — producer.go:121-123.
	if e.Payload == nil {
		return errors.New("bus: Event.Payload required")
	}
	// Rule 2 — stamp, producer.go:170-172.
	if e.EventTime.IsZero() {
		return errors.New("Event.EventTime required")
	}
	// Rule 3 — stamp, producer.go:210-213.
	if e.EventClass == envelopepb.EventClass_EVENT_CLASS_COMMAND && e.IdempotencyKey == "" {
		return errors.New("idempotency_key required for COMMAND events")
	}
	// Rule 4 — tenant precedence mirrors stamp exactly (producer.go:225-231),
	// then Validate's live-path rejection of an empty tenant (validate.go:35-37,
	// wrapped by publish() at producer.go:128-130).
	tenant := e.TenantID
	if tenant == "" {
		tenant = bus.TenantIDFromContext(ctx)
	}
	if tenant == "" {
		tenant = f.tenant
	}
	if tenant == "" {
		return errors.New("envelope validation: tenant_id required")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeBus) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	for i, e := range f.events {
		out[i] = e.EventType
	}
	return out
}

func (f *fakeBus) last(eventType string) proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		if f.events[i].EventType == eventType {
			return f.events[i].Payload
		}
	}
	return nil
}

// Direct tests of fakeBus's own enforcement — one per rule, pinning the
// double's contract so it cannot silently regress back to accepting anything.

func TestFakeBus_RejectsNilPayload(t *testing.T) {
	fb := &fakeBus{}
	err := fb.Publish(testCtx(), bus.Event{EventTime: time.Now(), Payload: nil})
	if err == nil {
		t.Fatal("Publish with nil Payload: got nil error, want an error")
	}
}

func TestFakeBus_RejectsZeroEventTime(t *testing.T) {
	fb := &fakeBus{}
	err := fb.Publish(testCtx(), bus.Event{Payload: &orderpb.OrderAccepted{}})
	if err == nil {
		t.Fatal("Publish with zero EventTime: got nil error, want an error")
	}
}

func TestFakeBus_RejectsCommandWithoutIdempotencyKey(t *testing.T) {
	fb := &fakeBus{}
	err := fb.Publish(testCtx(), bus.Event{
		EventTime:  time.Now(),
		EventClass: envelopepb.EventClass_EVENT_CLASS_COMMAND,
		Payload:    &orderpb.OrderAccepted{},
	})
	if err == nil {
		t.Fatal("Publish of a COMMAND with no IdempotencyKey: got nil error, want an error")
	}
}

func TestFakeBus_RejectsMissingTenant(t *testing.T) {
	fb := &fakeBus{}
	// No Event.TenantID, no ctx tenant, no fb.tenant fallback: the same
	// combination that reached the live broker and produced "envelope
	// validation: tenant_id required".
	err := fb.Publish(context.Background(), bus.Event{
		EventTime: time.Now(),
		Payload:   &orderpb.OrderAccepted{},
	})
	if err == nil {
		t.Fatal("Publish with no tenant anywhere: got nil error, want an error")
	}
	if !strings.Contains(err.Error(), "tenant_id") {
		t.Fatalf("error = %q, want it to mention tenant_id (matching the real broker's rejection text)", err.Error())
	}
}

func submitEnv() *envelopepb.Envelope { return &envelopepb.Envelope{EventType: SubjectSubmit} }

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newService(t *testing.T, fb *fakeBus, gate compliance.Gate) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	router := execution.NewRouter([]execution.Venue{execution.NewSimVenue("XSIM")})
	svc, err := NewService(testTenant, store, NewEmitter(fb), gate, router, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

func TestService_SubmitMarketableLimit_FillsAndAcks(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	want := []string{EventTypeAccepted, EventTypeRouted, EventTypeFilled, EventTypeOutcome}
	if got := fb.types(); !equal(got, want) {
		t.Fatalf("emitted = %v, want %v", got, want)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("stored status = %v, want FILLED", st.GetStatus())
	}
}

func TestService_IdempotentResubmit(t *testing.T) {
	fb := &fakeBus{}
	svc, _ := newService(t, fb, nil)
	cmd := limitOrder(d(100, 0), d(1025, -2))

	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("first: %v", err)
	}
	n := len(fb.types())
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := len(fb.types()); got != n {
		t.Fatalf("re-submit emitted %d new events, want 0 (total stayed %d)", got-n, n)
	}
}

type denyGate struct{}

func (denyGate) Check(context.Context, string, *orderpb.SubmitOrder) (*compliance.Breach, error) {
	return &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}, nil
}

func TestService_ComplianceBreach_RejectsBeforeAccept(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, denyGate{})

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := []string{EventTypeRejected, EventTypeOutcome}
	if got := fb.types(); !equal(got, want) {
		t.Fatalf("emitted = %v, want %v", got, want)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc.GetStatus())
	}
	if oc.GetErrorCode() != "COMPLIANCE_CONCENTRATION" {
		t.Fatalf("error_code = %q, want COMPLIANCE_CONCENTRATION", oc.GetErrorCode())
	}
	if _, _, err := store.Load(context.Background(), "o1"); err == nil {
		t.Fatal("order persisted despite compliance breach")
	}
}

func TestService_CancelUnknownOrder(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	env := &envelopepb.Envelope{EventType: SubjectCancel}
	if err := svc.Handle(testCtx(), env, mustMarshal(t, &orderpb.CancelOrder{OrderId: "ghost"})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED || oc.GetErrorCode() != "UNKNOWN_ORDER" {
		t.Fatalf("outcome = %v/%q, want REJECTED/UNKNOWN_ORDER", oc.GetStatus(), oc.GetErrorCode())
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countingVenue wraps a Venue and counts how many orders it was actually asked to
// execute. That count is the only thing that matters here: it is the number of
// orders that reached the exchange.
type countingVenue struct {
	execution.Venue
	mu sync.Mutex
	n  int
}

func (v *countingVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	return v.Venue.Execute(ctx, st)
}

func (v *countingVenue) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// THE double-trade test.
//
// TestService_IdempotentResubmit covers the SEQUENTIAL re-submit, which is why it
// passed while the bug was live: admission was Load()-then-Save(), and the two
// calls raced only when they overlapped. Concurrently, both deliveries of one
// SubmitOrder saw ErrNotFound, both admitted, and both routed to the venue — the
// stored state converged (Save is an upsert, so it LOOKED idempotent) while the
// fund traded twice.
//
// store.Create is now the atomic admission gate, so exactly one delivery can reach
// the venue. Run with -race.
func TestService_ConcurrentResubmit_RoutesToVenueExactlyOnce(t *testing.T) {
	const deliveries = 16

	fb := &fakeBus{}
	venue := &countingVenue{Venue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil, execution.NewRouter([]execution.Venue{venue}), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// One order, delivered many times at once — a redelivery storm, or several OMS
	// replicas handed the same command.
	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	var wg sync.WaitGroup
	errs := make(chan error, deliveries)
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Handle(testCtx(), submitEnv(), body); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Handle: %v", err)
	}

	if got := venue.count(); got != 1 {
		t.Fatalf("the same order reached the venue %d times, want exactly 1 — every extra one is a real trade against the fund's capital", got)
	}

	// And the order was admitted once: no duplicate ACCEPTED FACT on the bus.
	accepted := 0
	for _, et := range fb.types() {
		if et == EventTypeAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("emitted %d ACCEPTED facts, want 1", accepted)
	}
}
