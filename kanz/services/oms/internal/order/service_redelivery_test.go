package order

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// unreachableVenue fails its FIRST execute and then recovers — a venue call
// that timed out, a 500, an adapter that had lost its connection and got it
// back. By the time the first failure fires the order has already been admitted,
// routed, and SAVED as ROUTED.
//
// The recovery is load-bearing in this test, not incidental. A venue that failed
// forever could never demonstrate a successful resume; it would only ever prove
// that the redelivery errored again. The behaviour under test is that delivery 2
// ASKS and then WORKS the order, so the venue has to be able to work it.
//
// QueryOrder is delegated to the embedded SimVenue, which is the honest model:
// the EXECUTE call failed and recorded nothing, so the venue truthfully answers
// that it has no such order.
type unreachableVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *unreachableVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	first := v.n == 1
	v.mu.Unlock()
	if first {
		return nil, errors.New("venue unreachable")
	}
	return v.SimVenue.Execute(ctx, st)
}

func (v *unreachableVenue) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// THIS TEST REPLACES TestRedeliveryAfterVenueFailureIsAckedWithoutResuming.
//
// That test pinned the gap: delivery 1 created the order and failed at the
// venue; delivery 2 found the record, took it for a duplicate, returned nil, and
// left the order at ROUTED with nothing working it and nothing anywhere saying
// so. Its own doc said "WHEN THIS TEST FAILS, THAT IS THE SIGNAL, NOT A
// REGRESSION. It means handlers have learned to resume."
//
// This is that signal, inverted into an assertion. The redelivery must now ASK
// THE VENUE and act on the answer. The venue never received this order — its
// Execute failed before recording anything — so it answers UNKNOWN, our record
// carries no venue_ack_at, and the only safe action is to work it. It fills.
func TestRedeliveryAfterVenueFailureResumesAgainstVenueTruth(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := &unreachableVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted, routed, stored as ROUTED, then the venue call fails.
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	if got := venue.count(); got != 1 {
		t.Fatalf("venue.Execute called %d times on delivery 1, want exactly 1", got)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 1: %v", err)
	}
	if st.GetVenueAckAt() != nil {
		t.Fatal("venue_ack_at is set after a FAILED Execute — the ack must be stamped " +
			"only when the venue actually confirmed it holds the order, or the " +
			"quarantine arm fires on healthy orders")
	}

	// Delivery 2: the redelivery that error asked for. The handler must now ask
	// the venue rather than ack on sight.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v, want nil after a successful resume", err)
	}

	st, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("order status is %v, want FILLED — the venue had no record of this "+
			"order and no ack was ever recorded, so the redelivery had to work it", got)
	}
	if st.GetQuarantine() != nil {
		t.Fatalf("order was quarantined (%q) — the venue affirmatively said UNKNOWN "+
			"and we held no ack, which is the one combination that is safe to re-drive",
			st.GetQuarantine().GetReason())
	}
}

// THE DOUBLE-TRADE ARM. This is the assertion the whole design exists to make.
//
// The venue confirmed it held this order, and now denies it. Exactly one of the
// two records is wrong and nothing here can tell which. Re-driving trades the
// fund twice if ours is right; abandoning strands a live exchange order if the
// venue is right. The order freezes, and it must NOT reach the venue again.
func TestVenueDenyingAnAcknowledgedOrderQuarantinesAndDoesNotRedrive(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	// amnesiacVenue acknowledges an execute (no error) but then has no record of
	// the order — a venue that lost its order book, or answered from a replica
	// that never saw the write.
	venue := &amnesiacVenue{SimVenue: execution.NewSimVenue("XSIM")}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 1: %v", err)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetVenueAckAt() == nil {
		t.Fatal("venue_ack_at not stamped after a SUCCESSFUL Execute — without it the " +
			"quarantine arm below can never fire, and this venue's contradiction " +
			"would be re-driven as if it were a fresh order")
	}
	before := venue.executes()

	// The redelivery. The venue now denies the order it acknowledged.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}

	st, err = store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load after delivery 2: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("order was NOT quarantined. The venue acknowledged it and now reports " +
			"UNKNOWN — that is two authorities contradicting each other, and acting " +
			"on either one is a coin flip with the fund's money")
	}
	if st.GetQuarantine().GetReason() == "" {
		t.Error("quarantine carries no reason — an operator sees a frozen order and no account of why")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times — the quarantined order was "+
			"RE-DRIVEN, which is the double trade this whole design exists to prevent",
			got-before)
	}
}

// amnesiacVenue executes successfully but never remembers: every QueryOrder
// answers UNKNOWN. It models a venue whose order book was lost or whose read
// replica never saw the write.
type amnesiacVenue struct {
	*execution.SimVenue
	mu sync.Mutex
	n  int
}

func (v *amnesiacVenue) Execute(ctx context.Context, st *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	// Deliberately does NOT delegate to SimVenue.Execute: this venue must
	// acknowledge without recording, so the query below can contradict it.
	return nil, nil
}

func (v *amnesiacVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	return execution.OrderView{State: execution.OrderViewUnknown}, nil
}

func (v *amnesiacVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}

// A venue that cannot be asked is not a venue we may guess about. Every real
// out-of-process adapter is in this state today, because venue.v1's wire
// contract has no query RPC.
func TestVenueWithoutQuerierQuarantinesRatherThanGuessing(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBus{}
	venue := &muteVenue{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the venue failure to surface")
	}
	before := venue.executes()

	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack", err)
	}
	st, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("an order at a venue that implements no Querier was not quarantined — " +
			"nothing can establish what that venue did, and the alternative to " +
			"freezing is guessing")
	}
	if got := venue.executes(); got != before {
		t.Fatalf("venue.Execute called %d more times on a venue nobody can query", got-before)
	}
}

// muteVenue implements Venue and nothing else — no Querier. This is every
// out-of-process GRPCVenue today.
type muteVenue struct {
	mu sync.Mutex
	n  int
}

func (v *muteVenue) MIC() string     { return "XMUTE" }
func (v *muteVenue) Account() string { return "acct-mute" }
func (v *muteVenue) Execute(context.Context, *orderpb.OrderState) ([]*orderpb.Fill, error) {
	v.mu.Lock()
	v.n++
	v.mu.Unlock()
	return nil, errors.New("venue unreachable")
}
func (v *muteVenue) executes() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.n
}
