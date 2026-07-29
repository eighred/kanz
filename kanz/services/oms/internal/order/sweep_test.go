package order

import (
	"context"
	"strings"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// AN ORDER STRANDED BY A CRASH HAS NO REDELIVERY COMING.
//
// The redelivery path rescues an order whose command is still in the stream. It
// does nothing for one whose command was ACKED and whose process then died
// between Save(ROUTED) and the fold — nothing will ever mention that order
// again. The sweep is the only thing that finds it, and it must run before the
// consumers do, the way tv-sync rebuilds its book before it serves one.
func TestSweepResumesAnOrderStrandedAtRouted(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	venue := execution.NewSimVenue("XSIM")
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Exactly what a crashed predecessor leaves behind: admitted, routed, saved,
	// no venue acknowledgement, no fill.
	admitted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	stranded := Route(admitted, t0)
	if err := store.Create(ctx, stranded); err != nil {
		t.Fatalf("Create: %v", err)
	}

	swept, err := svc.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept %d orders, want 1", swept)
	}

	st, err := store.Load(ctx, stranded.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("stranded order status is %v, want FILLED — the venue had no record "+
			"of it and no ack was recorded, so the sweep had to work it", got)
	}
}

// A SWEEP WITH NO TENANT ON ITS CONTEXT MUST REFUSE, NOT REDRIVE BLIND.
//
// SweepInterrupted re-publishes lifecycle events for every order it resumes.
// A handler gets its tenant lifted off the inbound envelope onto ctx, but the
// sweep runs before any delivery, so nothing does that for it — the caller
// must supply one explicitly (bus.WithTenantID). If it doesn't, the sweep must
// fail before it ever reaches the venue: found by a live cluster proof where a
// tenant-less sweep reached the venue, redrove an order, and then failed
// envelope validation on the resulting publish — after already having acted.
func TestSweepRefusesAContextWithNoTenant(t *testing.T) {
	ctx := context.Background() // deliberately no bus.WithTenantID
	fb := &fakeBus{}
	venue := execution.NewSimVenue("XSIM")
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(venue), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	admitted, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	stranded := Route(admitted, t0)
	if err := store.Create(context.Background(), stranded); err != nil {
		t.Fatalf("Create: %v", err)
	}

	swept, err := svc.SweepInterrupted(ctx)
	if err == nil {
		t.Fatal("SweepInterrupted with no tenant on ctx: got nil error, want an error")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("SweepInterrupted error %q does not mention the tenant requirement", err.Error())
	}
	if swept != 0 {
		t.Fatalf("SweepInterrupted swept %d orders with no tenant on ctx, want 0", swept)
	}

	// Must not have touched the venue: the order is exactly as it was left,
	// still ROUTED, not resumed/filled/frozen.
	st, err := store.Load(context.Background(), stranded.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := st.GetStatus(); got != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("order status is %v, want ROUTED (untouched) — a tenant-less sweep must "+
			"refuse before it reaches the venue, not after", got)
	}
	if len(fb.events) != 0 {
		t.Fatalf("fakeBus recorded %d events, want 0 — a tenant-less sweep must not publish anything", len(fb.events))
	}
}

// The sweep must not touch finished orders. Loading the whole book and
// "resuming" a filled order would re-trade it.
func TestSweepIgnoresTerminalOrders(t *testing.T) {
	ctx := testCtx()
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	done, err := Accept(limitOrder(d(100, 0), d(1025, -2)), t0)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	done.Status = orderpb.OrderStatus_ORDER_STATUS_FILLED
	if err := store.Create(ctx, done); err != nil {
		t.Fatalf("Create: %v", err)
	}

	swept, err := svc.SweepInterrupted(ctx)
	if err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	if swept != 0 {
		t.Fatalf("swept %d orders, want 0 — a FILLED order has nothing to resume", swept)
	}
}

// ListByStatus must select, not scan-and-filter-in-the-caller: the sweep is on
// the startup path and the store may hold every order the fund has ever placed.
func TestListByStatusReturnsOnlyTheRequestedStatuses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	routed := &orderpb.OrderState{OrderId: "routed", Status: orderpb.OrderStatus_ORDER_STATUS_ROUTED}
	filled := &orderpb.OrderState{OrderId: "filled", Status: orderpb.OrderStatus_ORDER_STATUS_FILLED}
	pending := &orderpb.OrderState{OrderId: "pending", Status: orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW}
	for _, st := range []*orderpb.OrderState{routed, filled, pending} {
		if err := store.Create(ctx, st); err != nil {
			t.Fatalf("Create %s: %v", st.GetOrderId(), err)
		}
	}

	got, err := store.ListByStatus(ctx,
		orderpb.OrderStatus_ORDER_STATUS_ROUTED,
		orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByStatus returned %d orders, want 2", len(got))
	}
	for _, st := range got {
		if st.GetOrderId() == "filled" {
			t.Fatal("ListByStatus returned a FILLED order for a ROUTED/PENDING_NEW query")
		}
	}
}
