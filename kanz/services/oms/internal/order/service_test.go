package order

import (
	"context"
	"sync"
	"testing"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/oms/internal/compliance"
	"github.com/kanz-eng/kanz/services/oms/internal/execution"
)

// fakeBus records published events for assertion.
type fakeBus struct {
	mu     sync.Mutex
	events []bus.Event
}

func (f *fakeBus) Publish(_ context.Context, e bus.Event) error {
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
	router := execution.NewRouter(execution.NewSimVenue("XSIM"))
	svc, err := NewService(store, NewEmitter(fb), gate, router, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

func TestService_SubmitMarketableLimit_FillsAndAcks(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, nil)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(context.Background(), submitEnv(), mustMarshal(t, cmd)); err != nil {
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
	st, err := store.Load(context.Background(), "o1")
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

	if err := svc.Handle(context.Background(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("first: %v", err)
	}
	n := len(fb.types())
	if err := svc.Handle(context.Background(), submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := len(fb.types()); got != n {
		t.Fatalf("re-submit emitted %d new events, want 0 (total stayed %d)", got-n, n)
	}
}

type denyGate struct{}

func (denyGate) Check(context.Context, *orderpb.SubmitOrder) (*compliance.Breach, error) {
	return &compliance.Breach{Code: "CONCENTRATION", Reason: "over sector cap"}, nil
}

func TestService_ComplianceBreach_RejectsBeforeAccept(t *testing.T) {
	fb := &fakeBus{}
	svc, store := newService(t, fb, denyGate{})

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(context.Background(), submitEnv(), mustMarshal(t, cmd)); err != nil {
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
	if _, err := store.Load(context.Background(), "o1"); err == nil {
		t.Fatal("order persisted despite compliance breach")
	}
}

func TestService_CancelUnknownOrder(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	env := &envelopepb.Envelope{EventType: SubjectCancel}
	if err := svc.Handle(context.Background(), env, mustMarshal(t, &orderpb.CancelOrder{OrderId: "ghost"})); err != nil {
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
