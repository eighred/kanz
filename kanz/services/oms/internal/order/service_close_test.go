package order

import (
	"context"
	"errors"
	"testing"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// The venue-close dispatch path (EXEC-M4d): a cancel must reach the EXCHANGE,
// not just the ledger, and the close must be tracked in the in-flight registry
// before it races the venue so the healing watchdog owns the ack window.

// closerVenue is a Venue that also satisfies execution.Closer. Execute returns no
// fills, so an admitted order rests — the state a cancel actually applies to.
type closerVenue struct {
	mic       string
	cancelled []string
	err       error
}

func (v *closerVenue) MIC() string     { return v.mic }
func (v *closerVenue) Account() string { return "acct-" + v.mic }

func (v *closerVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil // rests
}

func (v *closerVenue) CancelOrder(_ context.Context, st *orderpb.OrderState) error {
	v.cancelled = append(v.cancelled, st.GetOrderId())
	return v.err
}

func cancelEnv() *envelopepb.Envelope { return &envelopepb.Envelope{EventType: SubjectCancel} }

// restingOrderOn admits a resting limit order on venue and returns the wired
// service + the shared close registry.
func restingOrderOn(t *testing.T, fb *fakeBus, venue execution.Venue) (*Service, *execution.CloseRegistry) {
	t.Helper()
	reg := execution.NewCloseRegistry()
	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil, execution.NewRouter(venue), reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A limit order the venue does not fill ⇒ it rests and is cancellable.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, limitOrder(d(100, 0), d(1025, -2)))); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return svc, reg
}

// A cancel is dispatched to the venue holding the order, and a venue that
// confirms the withdrawal leaves nothing for the watchdog to heal.
func TestCancel_DispatchesToVenueAndResolves(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, reg := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(venue.cancelled) != 1 || venue.cancelled[0] != "o1" {
		t.Fatalf("venue cancels = %v, want [o1] — a cancel MUST reach the exchange, not just the ledger", venue.cancelled)
	}
	if reg.Len() != 0 {
		t.Fatalf("registry holds %d closes, want 0 (the venue confirmed — nothing to heal)", reg.Len())
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
}

// A venue that will not confirm the cancel leaves the close TRACKED for the
// healing watchdog — and the ledger still progresses. The ledger must never
// freeze on an exchange that does not answer.
func TestCancel_UnconfirmedVenueLeavesCloseTracked(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE", err: errors.New("timeout")}
	svc, reg := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel returned %v — an unconfirmed venue cancel must not nack the command", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("registry holds %d closes, want 1 (an unconfirmed close is the watchdog's to resolve)", reg.Len())
	}
	// The ledger did not freeze: the cancellation FACT + outcome still went out.
	if fb.last(EventTypeCancelled) == nil {
		t.Fatal("no OrderCancelled emitted — the ledger froze on an unresponsive venue")
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome = %v, want EXECUTED", oc.GetStatus())
	}
}

// The tracked close carries NO sweep: a resting order that never traded opens no
// exposure when withdrawn, so sweeping it would fabricate a position.
func TestCancel_TrackedCloseCarriesNoSweep(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE", err: errors.New("timeout")}
	svc, reg := restingOrderOn(t, fb, venue)

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	due := reg.DueCloses(time.Now(), 0) // timeout 0 ⇒ every tracked close is due
	if len(due) != 1 {
		t.Fatalf("due = %d, want 1", len(due))
	}
	if due[0].SweepSide != orderpb.Side_SIDE_UNSPECIFIED {
		t.Fatalf("SweepSide = %v, want UNSPECIFIED — cancelling a resting order opens no exposure to sweep",
			due[0].SweepSide)
	}
	if due[0].Leaves != nil && due[0].Leaves.Sign() > 0 {
		t.Fatalf("Leaves = %s, want none — a sweep here would open a position out of thin air",
			due[0].Leaves.RatString())
	}
}

// A venue with nothing resting externally (SimVenue does not implement Closer)
// cancels ledger-only: no dispatch, no tracked close, no panic.
func TestCancel_NonCloserVenueIsLedgerOnly(t *testing.T) {
	fb := &fakeBus{}
	reg := execution.NewCloseRegistry()
	svc, err := NewService(NewMemoryStore(), NewEmitter(fb), nil,
		execution.NewRouter(execution.NewSimVenue("XSIM")), reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// A market order with no price resolver does not fill on SimVenue ⇒ it rests.
	if err := svc.Handle(testCtx(), submitEnv(), mustMarshal(t, marketOrder())); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("registry holds %d closes, want 0 (nothing rests at an exchange to withdraw)", reg.Len())
	}
}

// A QUARANTINED ORDER MUST REFUSE A CANCEL, and — the point of this test — the
// refusal must land BEFORE closeAtVenue, not after. quarantine() exists because
// the platform could not establish what the venue did with this order; a
// cancel that still reaches the exchange (or still saves CANCELLED, which is
// terminal and makes the order invisible to resume/SweepInterrupted forever)
// silently converts that unresolved unknown into a confident, wrong answer —
// exactly the freeze quarantine was supposed to force a human to resolve.
func TestCancel_RefusesQuarantinedOrder(t *testing.T) {
	fb := &fakeBus{}
	venue := &closerVenue{mic: "BINANCE"}
	svc, _ := restingOrderOn(t, fb, venue)

	st, err := svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := svc.quarantine(context.Background(), st, "test: venue truth could not be established"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	if err := svc.Handle(testCtx(), cancelEnv(), mustMarshal(t, cancelAs("pf1"))); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if len(venue.cancelled) != 0 {
		t.Fatalf("venue cancels = %v, want none — a quarantined order must not reach the exchange", venue.cancelled)
	}
	st, err = svc.store.Load(context.Background(), "o1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatal("quarantined order was CANCELLED — the freeze was silently converted into a " +
			"confident terminal answer, and CANCELLED is terminal, so resume/SweepInterrupted " +
			"will never look at this order again")
	}
	if st.GetQuarantine() == nil {
		t.Fatal("quarantine was cleared by the cancel attempt — it must survive a refused command")
	}
	oc := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED || oc.GetErrorCode() != "ORDER_QUARANTINED" {
		t.Fatalf("outcome = %v/%q, want REJECTED/ORDER_QUARANTINED", oc.GetStatus(), oc.GetErrorCode())
	}
}

func marketOrder() *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		OrderId: "o1", PortfolioId: "pf1", InstrumentId: "AAPL",
		Side: orderpb.Side_SIDE_BUY, Quantity: d(100, 0),
		OrderType:   orderpb.OrderType_ORDER_TYPE_MARKET,
		TimeInForce: orderpb.TimeInForce_TIME_IN_FORCE_DAY,
	}
}
