package order

import (
	"context"
	"testing"
	"time"

	commandpb "github.com/kanz-eng/kanz-schemas-go/command/v1"
	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kanz-eng/kanz/internal/execution"
)

// EXEC-M8 — what the OMS does when a venue adapter is not there.
//
// The venue adapters' publish-health control (bus.HealthPublisher → /readyz 503)
// exists to pull an adapter out of its Service when it can no longer report what
// it did with an order. That is only a capital control if the OTHER END behaves:
// the OMS must refuse to trade, and it must never book a fill nobody made.
//
// venue-binance's own composition root states the claim outright: "publish health
// feeds /readyz, the pod drops out of its Service, and the OMS's router
// hard-errors on this MIC." These are the tests of that sentence.

// deadVenue is a GRPCVenue pointed at an address nothing is listening on — an
// adapter that has been evicted from its Service, which is exactly what the
// readiness control does to an adapter that cannot publish its fills.
func deadVenue(t *testing.T, mic string) execution.Venue {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return execution.NewGRPCVenue(mic, "acct-test", conn, "acme-capital")
}

// An evicted adapter must stop the order, not lose it.
//
// The command FAILS (so the bus redelivers it rather than acking a trade that
// never happened), the order never reaches FILLED, and — the part that matters —
// no Fill FACT is emitted. An OMS that booked a fill here would be inventing an
// execution that no exchange ever performed.
func TestVenueUnreachable_RefusesToTradeAndFabricatesNothing(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(deadVenue(t, "BINANCE")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(testCtx(), 3*time.Second)
	defer cancel()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "BINANCE"
	err = svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd))

	if err == nil {
		t.Fatal("the OMS ACKED an order it could not execute: the adapter is gone, the command is acked, and the order is never worked again")
	}
	for _, ev := range fb.types() {
		if ev == EventTypeFilled {
			t.Fatal("the OMS emitted a Fill for an order no exchange ever saw")
		}
	}
	st, lerr := store.Load(ctx, cmd.GetOrderId())
	if lerr != nil {
		t.Fatalf("the order vanished: %v", lerr)
	}
	if st.GetStatus() == orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("order status = FILLED against a venue that never answered")
	}
	if oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome); oc != nil &&
		oc.GetStatus() == commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_EXECUTED {
		t.Fatal("the OMS reported the command EXECUTED against an unreachable venue")
	}
}

// TestOrderNamingAnUnconfiguredVenue_IsRejected is the other half of the claim.
//
// An order that names a venue the OMS has no adapter for can NEVER be executed —
// not now, not on a retry, not ever. Admitting it and leaving it to "rest" tells
// the strategy its order is working while nothing on this platform will ever send
// it anywhere. The order sits ACCEPTED forever, the command is acked, and the only
// way anyone finds out is by noticing the fill never came.
//
// This is not the same as an OMS with NO venues configured (a paper/observation
// deployment, where resting is deliberate). This is an order that asked for a
// specific venue by name and did not get it. That is a rejection.
func TestOrderNamingAnUnconfiguredVenue_IsRejected(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	// The OMS is wired for XSIM only. The order asks for BINANCE.
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(execution.NewSimVenue("XSIM")), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	cmd.Venue = "BINANCE"
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// It is refused at ADMISSION — the same treatment a compliance breach or a
	// malformed order gets, and for the same reason: this OMS cannot work it. So
	// there is no admitted order, exactly as there is none for any other rejection.
	if _, lerr := store.Load(ctx, cmd.GetOrderId()); lerr == nil {
		t.Fatal("the order was ADMITTED against a venue this OMS has no adapter for.\n" +
			"It can never be executed — not now, not on a retry, not ever — but it rests as if it were working, " +
			"the command is acked, and the only way anyone finds out is by noticing the fill never came.")
	}

	oc, _ := fb.last(EventTypeOutcome).(*commandpb.CommandOutcome)
	if oc == nil || oc.GetStatus() != commandpb.CommandOutcomeStatus_COMMAND_OUTCOME_STATUS_REJECTED {
		t.Fatalf("command outcome = %v, want REJECTED — the strategy must be TOLD its venue does not exist", oc.GetStatus())
	}
	if oc.GetErrorCode() != "VENUE_NOT_CONFIGURED" {
		t.Errorf("rejection code = %q, want VENUE_NOT_CONFIGURED", oc.GetErrorCode())
	}
	// And it must not have been routed or filled on the way out.
	for _, ev := range fb.types() {
		if ev == EventTypeRouted || ev == EventTypeFilled {
			t.Fatalf("the order was %s despite naming a venue that is not configured", ev)
		}
	}
}

// An OMS with NO venues at all still rests its orders. That is a deliberate,
// different case (a paper deployment, or execution not yet wired) and this test
// pins that the rejection above did not break it.
func TestOrderWithNoVenuesConfigured_StillRests(t *testing.T) {
	fb := &fakeBus{}
	store := NewMemoryStore()
	svc, err := NewService(store, NewEmitter(fb), nil, execution.NewRouter(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx()

	cmd := limitOrder(d(100, 0), d(1025, -2))
	if err := svc.Handle(ctx, submitEnv(), mustMarshal(t, cmd)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	st, lerr := store.Load(ctx, cmd.GetOrderId())
	if lerr != nil || st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_PENDING_NEW {
		t.Fatalf("status = %v (err=%v), want PENDING_NEW — an OMS with no execution wired rests its orders", st.GetStatus(), lerr)
	}
}
