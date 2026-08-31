package order

// resume() ACROSS THE WIRE, over a real execution.GRPCVenue (#920).
//
// # Why this test is not a duplicate of the ones beside it
//
// Every other reconciliation test in this package drives an IN-PROCESS venue —
// SimVenue, or a fake embedding it. That is exactly the gap #920 was filed for:
// venue.v1 carried no query RPC, so `GRPCVenue` implemented no
// execution.Querier, `Service.resume`'s type assertion failed for EVERY real
// deployment, and every interrupted ROUTED order quarantined. The policy table
// in order.Reconcile — LEAVE, ADOPT, REDRIVE — was reachable only against a
// simulator, so a green suite said nothing about production.
//
// So these run over a bufconn gRPC connection to a stub adapter, through the
// same NewGRPCVenue the OMS composition root builds. UNWIRE THE RPC — delete
// GRPCVenue.QueryOrder, or its Querier assertion, or stop serving the method —
// AND THESE FAIL, because the OMS falls back to quarantining.

import (
	"context"
	"errors"
	"net"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
)

// remoteAdapter is a venue adapter in another process: it fails the first
// Execute (the interruption), then answers QueryOrder with a scripted verdict.
type remoteAdapter struct {
	venuepb.UnimplementedVenueAdapterServiceServer

	view       *venuepb.QueryOrderResponse
	queryErr   error
	execs      int
	queryCalls int
}

func (a *remoteAdapter) Execute(_ context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	a.execs++
	if a.execs == 1 {
		// The interruption: the order is already SAVED as ROUTED by the time this
		// fails, which is the state resume() has to recover from.
		return nil, errors.New("venue adapter unreachable")
	}
	return &venuepb.ExecuteResponse{Fills: []*orderpb.Fill{{
		FillId: "REDRIVEN-1", OrderId: req.GetState().GetOrderId(), InstrumentId: "AAPL",
		Side: orderpb.Side_SIDE_BUY, Quantity: d(100, 0), Price: d(1025, -2), Venue: "XREMOTE",
	}}}, nil
}

func (a *remoteAdapter) QueryOrder(context.Context, *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	a.queryCalls++
	if a.queryErr != nil {
		return nil, a.queryErr
	}
	return a.view, nil
}

// omsOverGRPCVenue stands the adapter up on a bufconn listener and returns an
// OMS routing to it through the real GRPCVenue client.
func omsOverGRPCVenue(t *testing.T, adapter venuepb.VenueAdapterServiceServer) (*Service, *MemoryStore) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	venuepb.RegisterVenueAdapterServiceServer(gs, adapter)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	venue := execution.NewGRPCVenue("XREMOTE", "remote-main", conn, testTenant)
	store := NewMemoryStore()
	svc, err := NewService(testTenant, store, NewEmitter(&fakeBus{}), nil,
		execution.NewRouter([]execution.Venue{venue}), nil, nil, WithHaltGate(halt.OpenGate(nil)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store
}

// AN INTERRUPTED ORDER THE REMOTE VENUE HOLDS FILLED IS ADOPTED, NOT FROZEN.
//
// This is #920's own "Verified when", and it is the case with the most capital
// behind it: the fund's trade happened, and before this RPC the platform's only
// answer was to freeze the order and have a human reconcile it against the
// exchange's order history by hand.
func TestResumeOverAGRPCVenueAdoptsAFilledOrderInsteadOfQuarantining(t *testing.T) {
	ctx := testCtx()
	adapter := &remoteAdapter{view: &venuepb.QueryOrderResponse{
		State: venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED,
		Fills: []*orderpb.Fill{{
			FillId: "AAPL-42", OrderId: "o1", InstrumentId: "AAPL",
			Side: orderpb.Side_SIDE_BUY, Quantity: d(100, 0), Price: d(1025, -2), Venue: "XREMOTE",
		}},
	}}
	svc, store := omsOverGRPCVenue(t, adapter)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	// Delivery 1: admitted, routed, SAVED as ROUTED — and then the adapter call
	// fails. The order is now interrupted.
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the adapter failure to surface")
	}

	// Delivery 2: the redelivery. It must ASK the adapter and adopt its truth.
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	if adapter.queryCalls == 0 {
		t.Fatal("the OMS never asked the remote adapter what became of the order. That is #920 " +
			"exactly: GRPCVenue implements no Querier, so resume() quarantines instead of asking")
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("an order the remote venue reported FILLED was QUARANTINED (%q). Every "+
			"interrupted order at every real venue used to land here", q.GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — the venue's truth is ours to adopt", st.GetStatus())
	}
	if got := dec.Str(dec.FromProto(st.GetFilledQuantity())); got != "100" {
		t.Fatalf("filled_quantity = %s, want 100", got)
	}
	if adapter.execs != 1 {
		t.Fatalf("the adapter was asked to Execute %d times; an order it reported FILLED must "+
			"never be placed again", adapter.execs)
	}
}

// AN ORDER THE REMOTE VENUE NEVER RECEIVED IS RE-DRIVEN, NOT FROZEN.
//
// The other end of the same policy: our record carries no venue_ack_at and the
// adapter says the exchange has no such order, so both authorities agree it
// never arrived. Working it is the entire point of resuming.
func TestResumeOverAGRPCVenueRedrivesAnOrderTheVenueNeverReceived(t *testing.T) {
	ctx := testCtx()
	adapter := &remoteAdapter{view: &venuepb.QueryOrderResponse{
		State: venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN,
	}}
	svc, store := omsOverGRPCVenue(t, adapter)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the adapter failure to surface")
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if q := st.GetQuarantine(); q != nil {
		t.Fatalf("an order the venue affirmatively never received was QUARANTINED (%q)", q.GetReason())
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_FILLED {
		t.Fatalf("status = %v, want FILLED — the re-drive placed it and it filled", st.GetStatus())
	}
	if adapter.execs != 2 {
		t.Fatalf("the adapter saw %d Executes, want 2 — the interrupted one and the re-drive", adapter.execs)
	}
}

// A REMOTE VENUE THAT COULD NOT BE ASKED NEVER RE-DRIVES THE ORDER.
//
// THIS IS THE DUPLICATE-ORDER TEST AT THE OMS. The adapter answered with a gRPC
// error, which means the question could not be asked. If that ever reached
// Reconcile as OrderViewUnknown, the order would be placed at the exchange a
// second time — for a network blip. The delivery must NACK instead, leaving the
// order exactly as it was.
func TestResumeOverAGRPCVenueNacksWhenTheAdapterCannotBeAsked(t *testing.T) {
	ctx := testCtx()
	adapter := &remoteAdapter{queryErr: errors.New("adapter unreachable")}
	svc, store := omsOverGRPCVenue(t, adapter)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the adapter failure to surface")
	}
	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("a query that could not be asked was ACKED. The delivery must nack so the broker " +
			"asks again; acking abandons the order with nothing left to resume it")
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetStatus() != orderpb.OrderStatus_ORDER_STATUS_ROUTED {
		t.Fatalf("status = %v, want ROUTED — an unanswerable query must change nothing", st.GetStatus())
	}
	if adapter.execs != 1 {
		t.Fatalf("the adapter saw %d Executes. A failed QUERY was read as the venue denying the "+
			"order, and the platform placed it again at a real exchange", adapter.execs)
	}
}

// AN ADAPTER THAT SERVES NO QueryOrder QUARANTINES, AND DOES NOT LOOP.
//
// The upgrade-order case: the OMS gains the RPC before its adapters do. The
// behaviour must be exactly what it was before #920 — a terminal, visible
// quarantine — rather than an endless redelivery that ends in a DLQ.
func TestResumeOverAnAdapterWithoutQueryOrderQuarantines(t *testing.T) {
	ctx := testCtx()
	// olderAdapter embeds the generated Unimplemented server and overrides only
	// Execute, so QueryOrder answers codes.Unimplemented from the transport.
	adapter := &olderAdapter{}
	svc, store := omsOverGRPCVenue(t, adapter)

	cmd := limitOrder(d(100, 0), d(1025, -2))
	body := mustMarshal(t, cmd)

	if err := svc.Handle(ctx, submitEnv(), body); err == nil {
		t.Fatal("delivery 1 returned nil — expected the adapter failure to surface")
	}
	if err := svc.Handle(ctx, submitEnv(), body); err != nil {
		t.Fatalf("delivery 2 returned %v; a quarantine is terminal and must ack, or the OMS "+
			"nacks forever against an adapter that will never answer", err)
	}
	st, _, err := store.Load(ctx, cmd.GetOrderId())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.GetQuarantine() == nil {
		t.Fatal("an order at an adapter that serves no QueryOrder was not quarantined — nothing " +
			"can establish what that venue did, and the alternative to freezing is guessing")
	}
	if adapter.execs != 1 {
		t.Fatalf("the adapter saw %d Executes; an unanswerable venue must not be re-driven", adapter.execs)
	}
}

// olderAdapter is a venue adapter built before venue.v1 carried QueryOrder.
type olderAdapter struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	execs int
}

func (a *olderAdapter) Execute(context.Context, *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	a.execs++
	return nil, errors.New("venue adapter unreachable")
}
