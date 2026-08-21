package order

import (
	"context"
	"strings"
	"testing"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// AN OPERATOR'S HALT MUST STOP THE HUMAN ORDER PATH (#635).
//
// Before this, kanz-halt's ModeChanged had exactly one subscriber in the estate
// and it was not on this path: an operator halting on a risk breach stopped
// TradingView signals while POST /v1/orders → api-gateway → OMS → venue kept
// trading. These tests hold the repair to the standard the issue set — not "a
// gate object reports halted", but an order submitted through the OMS's own bus
// handler being REFUSED, proven by re-reading the store rather than by the
// handler's return value.
//
// A nil RETURN FROM Handle IS NOT SUCCESS HERE. The OMS acks a refusal: it
// publishes ORDER_REJECTED and returns nil, exactly as it does for a successful
// admission. So every assertion below reads the resulting STATE.

// haltedService wires an OMS whose gate has been closed by a real
// lifecycle.v1.ModeChanged FACT — decoded through Gate.Handle, the same path the
// bus subscription uses — rather than by calling Trip directly. The FACT is the
// thing an operator actually sends, and a test that closed the gate by hand
// would not notice a component filter or a decode that stopped working.
func haltedService(t *testing.T, store Store, gate *halt.Gate) (*Service, *fakeBus, *closerVenue) {
	t.Helper()
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(gate))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, fb, venue
}

// operatorHalt is the FACT cmd/kanz-halt publishes on platform.mode.changed.
func operatorHalt(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:akif",
		Reason:    "risk breach on fund-alpha",
	})
	if err != nil {
		t.Fatalf("marshal ModeChanged: %v", err)
	}
	return b
}

// THE ISSUE'S OWN CLOSING CONDITION: "a halt observably stops the human order
// path — the OMS refuses admission while halted".
func TestSubmit_RefusedWhileThePlatformIsHalted(t *testing.T) {
	gate := halt.OpenGate(nil) // trading, then an operator stops it
	store := &countingStore{Store: NewMemoryStore()}
	svc, fb, venue := haltedService(t, store, gate)

	if err := gate.Handle(context.Background(), nil, operatorHalt(t)); err != nil {
		t.Fatalf("Gate.Handle: %v", err)
	}

	// The command is otherwise perfect: right tenant, entitled principal, a
	// routable venue. The ONLY thing refusing it is the halt.
	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf1"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// THE SIDE EFFECT, ASSERTED AS AN ABSENCE — the same standard the entitlement
	// refusals hold. An order that reached store.Create exists, is swept on
	// restart, and can be routed to a venue later; "refused" would be a label on
	// a live order.
	if store.creates != 0 {
		t.Fatalf("store.Create called %d times during a declared platform halt — the order was ADMITTED", store.creates)
	}
	if _, _, err := store.Load(context.Background(), "o1"); err == nil {
		t.Fatal("the order was admitted despite the platform halt")
	}
	if len(venue.cancelled) != 0 {
		t.Fatalf("the venue was touched during a halt: %v", venue.cancelled)
	}

	// AND THE REFUSAL IS ANNOUNCED. A silent drop would leave the caller waiting
	// forever for an order the platform decided not to take.
	want := []string{EventTypeRejected, EventTypeOutcome}
	if got := fb.types(); !equal(got, want) {
		t.Fatalf("emitted = %v, want %v", got, want)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("outcome = %v, want REJECTED", oc.GetStatus())
	}
	if oc.GetErrorCode() != ReasonPlatformHalted {
		t.Fatalf("error_code = %q, want %q — a halt refusal must be distinguishable from a "+
			"compliance rejection, because only one of them is answered by retrying", oc.GetErrorCode(), ReasonPlatformHalted)
	}
	// The operator's own words reach the client. "Refused" without the reason
	// sends someone to read logs during an incident.
	if !strings.Contains(oc.GetReason(), "risk breach on fund-alpha") {
		t.Fatalf("outcome reason = %q, want it to carry the operator's reason", oc.GetReason())
	}
}

// DENY-BY-DEFAULT REACHES THE OMS TOO. A gate that has never been told anything
// — a pod that started and never heard a ModeChanged — refuses, and this is the
// case that matters most: it is the state every OMS boots in.
func TestSubmit_RefusedWhenTheOMSHasNeverLearnedThePlatformMode(t *testing.T) {
	store := &countingStore{Store: NewMemoryStore()}
	svc, fb, _ := haltedService(t, store, halt.NewGate(nil))

	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf1"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.creates != 0 {
		t.Fatalf("store.Create called %d times on an OMS that has never learned the platform mode", store.creates)
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetErrorCode() != ReasonPlatformHalted {
		t.Fatalf("error_code = %q, want %q", oc.GetErrorCode(), ReasonPlatformHalted)
	}
}

// AN OPERATOR RESUME REOPENS THE PATH, and this is the other half of the
// control: a brake that cannot be released is an outage. The same order, the
// same service, refused and then admitted.
func TestSubmit_AdmittedAgainAfterAnOperatorResume(t *testing.T) {
	gate := halt.OpenGate(nil)
	store := &countingStore{Store: NewMemoryStore()}
	svc, _, _ := haltedService(t, store, gate)

	if err := gate.Handle(context.Background(), nil, operatorHalt(t)); err != nil {
		t.Fatalf("Gate.Handle(halt): %v", err)
	}
	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf1"))); err != nil {
		t.Fatalf("Handle(halted): %v", err)
	}
	if store.creates != 0 {
		t.Fatal("admitted while halted")
	}

	resume, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_NORMAL,
		ChangedBy: "operator:akif",
		Reason:    "breach cleared",
	})
	if err != nil {
		t.Fatalf("marshal resume: %v", err)
	}
	if err := gate.Handle(context.Background(), nil, resume); err != nil {
		t.Fatalf("Gate.Handle(resume): %v", err)
	}

	if err := svc.Handle(testCtx(), submitEnvFor(testTenant), mustMarshal(t, submitAs("pf1"))); err != nil {
		t.Fatalf("Handle(resumed): %v", err)
	}
	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("the order was NOT admitted after an operator resume: %v", err)
	}
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_REJECTED {
		t.Fatal("the order was admitted as REJECTED after a resume")
	}
}

// A HALT DOES NOT JAM THE EXITS (#635). The operator who halts on a risk breach
// is usually the one who then needs to flatten the book, so cancels are
// deliberately outside the gate — at this layer and at the venue adapter.
//
// Asserted by re-reading the STORED order, not by the handler's nil.
func TestCancel_StillWorksWhileThePlatformIsHalted(t *testing.T) {
	gate := halt.OpenGate(nil)
	store := NewMemoryStore()
	venue := &closerVenue{mic: "BINANCE"}
	fb := &fakeBus{}
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), execution.NewCloseRegistry(), nil, WithHaltGate(gate))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A resting limit order, admitted BEFORE the halt.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if err := gate.Handle(context.Background(), nil, operatorHalt(t)); err != nil {
		t.Fatalf("Gate.Handle: %v", err)
	}

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel during halt: %v", err)
	}

	st, _, err := store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — a halted platform must still let an operator "+
			"get out of the book", st.GetStatus())
	}
	if len(venue.cancelled) != 1 {
		t.Fatalf("venue cancels = %v, want the order withdrawn at the exchange", venue.cancelled)
	}
}

// AN OMS WITHOUT A BRAKE MUST NOT EXIST. A nil *halt.Gate answers halted, so
// such a service would refuse every order — safe, and indistinguishable at 3am
// from a real declared halt. NewService keeps the two apart by refusing.
func TestNewService_RefusesWithoutAHaltGate(t *testing.T) {
	_, err := NewService(testTenant, NewMemoryStore(), NewEmitter(&fakeBus{}), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("NewService built an OMS with no halt gate — an optional brake is an absent brake")
	}
	if !strings.Contains(err.Error(), "halt gate is required") {
		t.Fatalf("error = %q, want it to name the missing halt gate", err.Error())
	}
}
