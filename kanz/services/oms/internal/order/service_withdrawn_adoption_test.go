package order

// ADOPTING A VENUE'S OWN WITHDRAWAL (#924).
//
// Before these, execution.OrderView had five verdicts and no answer for "the
// venue withdrew or expired this order", so both live connectors reported
// INDETERMINATE and order.Reconcile quarantined. The population that mattered
// was ordinary rather than exceptional: Binance calls an unfilled IOC or FOK
// order EXPIRED, both time-in-force values are declared supported, and every one
// interrupted between Save(ROUTED) and its venue ack froze for a human on an
// answer the exchange had given in full.
//
// # THE ONE THESE TESTS EXIST TO CATCH
//
// A withdrawn order MAY HAVE PARTIALLY TRADED BEFORE IT WAS PULLED. A verdict
// that dropped those fills would trade a visible quarantine for an invisible
// lost fill — the fund holds a position the platform has no record of — which is
// far worse than the freeze it removes. So the fills are folded FIRST and the
// terminal status is written over what remains.

import (
	"context"
	"sync"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// withdrawnVenue acks an execute without recording anything — the same shape
// twoFillVenue uses, so the order reaches ROUTED with venue_ack_at set and NO
// fills folded — and then answers a WITHDRAWN verdict when queried.
type withdrawnVenue struct {
	*execution.SimVenue
	mu      sync.Mutex
	view    execution.OrderView
	queries int
}

func (v *withdrawnVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	return nil, nil
}

func (v *withdrawnVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.queries++
	return v.view, nil
}

// serviceOverWithdrawnVenue wires an OMS over one, and returns the store and bus
// so a test can read what was written and what was announced.
func serviceOverWithdrawnVenue(t *testing.T, view execution.OrderView) (*Service, *MemoryStore, *fakeBus, *withdrawnVenue) {
	t.Helper()
	fb := &fakeBus{}
	venue := &withdrawnVenue{SimVenue: execution.NewSimVenue("XSIM"), view: view}
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(fb), nil,
		execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store, fb, venue
}

// AN INTERRUPTED IOC THE VENUE EXPIRED GOES TERMINAL WITHOUT A HUMAN.
//
// This is #924's own "Verified when", end to end through the real resume() and
// the real Reconcile(): delivery one routes the order and the venue acks without
// recording anything, delivery two is the redelivery that queries and gets
// EXPIRED with zero fills.
//
// It must not quarantine, and the estate must hear about it: an order the store
// calls EXPIRED with nothing published is a live-looking order to every consumer
// that folds the FACTs.
func TestAnInterruptedExpiredOrderGoesTerminalWithoutAHuman(t *testing.T) {
	ctx := testCtx()
	svc, store, fb, venue := serviceOverWithdrawnVenue(t, execution.OrderView{
		State: execution.OrderViewExpired,
	})

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 (the redelivery): %v", err)
	}
	if venue.queries == 0 {
		t.Fatal("the venue was never queried — this test is not exercising resume()")
	}

	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("the order was QUARANTINED (%q). Binance's EXPIRED is the ORDINARY terminal "+
			"state of an unfilled IOC, and freezing it for a human is exactly what #924 removed",
			q.GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_EXPIRED {
		t.Fatalf("status = %v, want EXPIRED — the venue said its time in force elapsed, and the "+
			"book of record has a status for that", st.GetStatus())
	}
	if !dec.IsZero(st.GetFilledQuantity()) {
		t.Fatalf("filled_quantity = %v, want 0 — the venue reported no fill", st.GetFilledQuantity())
	}
	if st.GetOutcomeAnnouncedAt() == nil {
		t.Fatal("outcome_announced_at unset on a terminal order — a later redelivery would " +
			"re-announce a generic outcome for an order whose real one already went out")
	}

	// THE ESTATE HEARS IT. An ORDER_EXPIRED FACT, carrying the unfilled quantity,
	// and the command outcome the caller of the submit is waiting on.
	exp, _ := fb.last(EventTypeExpired).(*orderpb.OrderExpired)
	if exp == nil {
		t.Fatalf("no ORDER_EXPIRED FACT published; events = %v", fb.types())
	}
	if got := dec.Str(dec.FromProto(exp.GetUnfilledQuantity())); got != "100" {
		t.Fatalf("unfilled_quantity = %s, want 100 — nothing traded", got)
	}
	out, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if out == nil {
		t.Fatalf("no CommandOutcome published; events = %v", fb.types())
	}
	if out.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatalf("outcome status = %v, want EXECUTED — the order reached the exchange and the "+
			"exchange worked it to a conclusion; FAILED would report a fault for the ordinary "+
			"outcome of an IOC", out.GetStatus())
	}
	if out.GetReason() == "" {
		t.Fatal("the outcome carries no reason — the caller learns the command ended and not why")
	}
}

// A WITHDRAWAL THAT PARTIALLY TRADED CARRIES ITS FILLS INTO THE RECORD.
//
// THIS IS THE TEST THAT PROVES THE CHANGE DID NOT TRADE A QUARANTINE FOR A LOST
// FILL. The venue pulled an order for 100 that had already traded 40, and both
// halves must land: the 40 folded as a real fill with a real FACT, and the
// remaining 60 recorded as the cancelled quantity. Dropping the fill would leave
// the fund holding 40 units the platform has no record of, silently — which is
// worse than the freeze this replaces, because a freeze is visible.
func TestAdoptingAWithdrawalFoldsWhatTradedBeforeItWasPulled(t *testing.T) {
	ctx := testCtx()
	cmd := limitOrder(d(100, 0), d(1025, -2))
	svc, store, fb, _ := serviceOverWithdrawnVenue(t, execution.OrderView{
		State: execution.OrderViewCancelled,
		Fills: []*orderpb.Fill{{
			FillId: "e1", OrderId: cmd.GetOrderId(), InstrumentId: "AAPL",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(40, 0), Price: d(1020, -2), Venue: "XSIM",
		}},
	})

	body := mustMarshal(t, cmd)
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 (the redelivery): %v", err)
	}

	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("the order was QUARANTINED (%q)", q.GetReason())
	}
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "40" {
		t.Fatalf("filled_quantity = %s, want 40. THE VENUE'S FILL WAS DROPPED: the fund holds 40 "+
			"units this platform has no record of, and nothing downstream can notice", got)
	}
	if got := dec.Str(dec.FromProto(st.GetAverageFillPrice())); got != "10.2" {
		t.Fatalf("average_fill_price = %s, want 10.2", got)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v, want CANCELLED — the fill is folded first and the withdrawal is "+
			"written over what remains", st.GetStatus())
	}
	if st.GetCancelAnnouncedAt() == nil {
		t.Fatal("cancel_announced_at unset — completeCancelAnnouncement would republish an " +
			"ORDER_CANCELLED FACT for a withdrawal this platform never issued")
	}

	// THE FILL'S OWN FACT WENT OUT. It is the one FACT nothing can rebuild from
	// stored state: the aggregate keeps only the cumulative result.
	pf, _ := fb.last(EventTypePartiallyFilled).(*orderpb.OrderPartiallyFilled)
	if pf == nil {
		t.Fatalf("no ORDER_PARTIALLY_FILLED FACT for the 40 that traded; events = %v", fb.types())
	}
	if pf.GetFill().GetFillId() != "e1" {
		t.Fatalf("fill_id = %q, want e1 — the venue's OWN identity, which is what the order "+
			"aggregate and the position book dedup on", pf.GetFill().GetFillId())
	}
	// AND THE WITHDRAWAL'S, carrying what did NOT trade.
	can, _ := fb.last(EventTypeCancelled).(*orderpb.OrderCancelled)
	if can == nil {
		t.Fatalf("no ORDER_CANCELLED FACT published; events = %v", fb.types())
	}
	if got := dec.Str(dec.FromProto(can.GetCancelledQuantity())); got != "60" {
		t.Fatalf("cancelled_quantity = %s, want 60 — 100 ordered less the 40 that traded", got)
	}
}

// A WITHDRAWAL WHOSE FILLS COMPLETE THE ORDER STAYS FILLED.
//
// The venue's fills are the harder evidence, and writing a withdrawal over them
// is exactly the erasure that stopped #920 from spelling this verdict REJECTED.
// adopt()'s fold loop breaks on a terminal state and the withdrawal is never
// written.
func TestAWithdrawalWhoseFillsCompleteTheOrderStaysFilled(t *testing.T) {
	ctx := testCtx()
	cmd := limitOrder(d(100, 0), d(1025, -2))
	svc, store, _, _ := serviceOverWithdrawnVenue(t, execution.OrderView{
		State: execution.OrderViewCancelled,
		Fills: []*orderpb.Fill{{
			FillId: "e1", OrderId: cmd.GetOrderId(), InstrumentId: "AAPL",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(100, 0), Price: d(1020, -2), Venue: "XSIM",
		}},
	})

	body := mustMarshal(t, cmd)
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 (the redelivery): %v", err)
	}

	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — the venue's own fills complete the order, and "+
			"writing CANCELLED over them erases a trade the fund actually received", st.GetStatus())
	}
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "100" {
		t.Fatalf("filled_quantity = %s, want 100", got)
	}
}

// RE-ADOPTING A WITHDRAWAL CHANGES NOTHING. The order is terminal and its
// outcome is announced, so a third delivery returns before the policy is
// consulted at all — and the aggregate must not move by one bit.
func TestReadoptingAWithdrawalIsANoOp(t *testing.T) {
	ctx := testCtx()
	cmd := limitOrder(d(100, 0), d(1025, -2))
	svc, store, _, _ := serviceOverWithdrawnVenue(t, execution.OrderView{
		State: execution.OrderViewCancelled,
		Fills: []*orderpb.Fill{{
			FillId: "e1", OrderId: cmd.GetOrderId(), InstrumentId: "AAPL",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(40, 0), Price: d(1020, -2), Venue: "XSIM",
		}},
	})

	body := mustMarshal(t, cmd)
	for i := 1; i <= 2; i++ {
		if err := svc.Handle(ctx, submitEnv(), body); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	before := financialBytes(t, st)

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 3 (re-adoption): %v", err)
	}
	after, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 3: %v", err)
	}
	if string(before) != string(financialBytes(t, after)) {
		t.Fatalf("re-adopting the same withdrawal CHANGED the aggregate: filled was %s, now %s",
			dec.Str(dec.FromProto(st.GetFilledQuantity())),
			dec.Str(dec.FromProto(after.GetFilledQuantity())))
	}
	if after.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_CANCELLED {
		t.Fatalf("status = %v after re-adoption, want CANCELLED", after.GetStatus())
	}
}
