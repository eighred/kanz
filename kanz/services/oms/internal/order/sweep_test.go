package order

import (
	"context"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"

	"github.com/kanz-eng/kanz/internal/execution"
)

// AN ORDER STRANDED BY A CRASH HAS NO REDELIVERY COMING.
//
// The redelivery path rescues an order whose command is still in the stream. It
// does nothing for one whose command was ACKED and whose process then died
// between Save(ROUTED) and the fold — nothing will ever mention that order
// again. The sweep is the only thing that finds it, and it must run before the
// consumers do, the way tv-sync rebuilds its book before it serves one.
func TestSweepResumesAnOrderStrandedAtRouted(t *testing.T) {
	ctx := context.Background()
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

// The sweep must not touch finished orders. Loading the whole book and
// "resuming" a filled order would re-trade it.
func TestSweepIgnoresTerminalOrders(t *testing.T) {
	ctx := context.Background()
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
