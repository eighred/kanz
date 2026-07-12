package execution

// GRPCVenue tests (INFRA-M7a). These run over a REAL gRPC connection on a
// bufconn listener — not a mocked client — so the proto round-trip (OrderState
// out, Fill back) is exercised, including the common.v1.Decimal encoding of price
// and quantity. A fake that skipped the wire would prove nothing about the thing
// this change exists to introduce: the wire.

import (
	"context"
	"errors"
	"math/big"
	"net"
	"testing"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kanz-eng/kanz/internal/dec"
)

// stubAdapter is a venue adapter process, stood up over real gRPC.
type stubAdapter struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	fills     []*orderpb.Fill
	execErr   error
	cancelErr error

	gotTenant string
	gotOrder  string
}

func (s *stubAdapter) Execute(_ context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	s.gotTenant = req.GetTenantId()
	s.gotOrder = req.GetState().GetOrderId()
	if s.execErr != nil {
		return nil, s.execErr
	}
	return &venuepb.ExecuteResponse{Fills: s.fills}, nil
}

func (s *stubAdapter) CancelOrder(_ context.Context, req *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	s.gotOrder = req.GetState().GetOrderId()
	if s.cancelErr != nil {
		return nil, s.cancelErr
	}
	return &venuepb.CancelOrderResponse{}, nil
}

func dialStub(t *testing.T, srv *stubAdapter) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	venuepb.RegisterVenueAdapterServiceServer(gs, srv)
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
	return conn
}

func testOrder() *orderpb.OrderState {
	return &orderpb.OrderState{
		OrderId:        "ORD-1",
		InstrumentId:   "BTC-USD",
		Side:           orderpb.Side_SIDE_BUY,
		OrderType:      orderpb.OrderType_ORDER_TYPE_LIMIT,
		LimitPrice:     dec.ToProto(big.NewRat(1234567, 100)), // 12345.67, exact
		LeavesQuantity: dec.ToProto(big.NewRat(1, 2)),         // 0.5, exact
	}
}

func TestGRPCVenueExecuteRoundTripsFillsExactly(t *testing.T) {
	// The fill the adapter reports must survive the wire with its decimals
	// intact. A price that arrives as 12345.669999 has moved capital wrongly.
	price := dec.ToProto(big.NewRat(1234567, 100))
	qty := dec.ToProto(big.NewRat(1, 2))
	srv := &stubAdapter{fills: []*orderpb.Fill{{
		FillId:   "F-1",
		OrderId:  "ORD-1",
		Quantity: qty,
		Price:    price,
		Venue:    "XBIN",
	}}}
	v := NewGRPCVenue("XBIN", dialStub(t, srv), "acme")

	fills, err := v.Execute(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("got %d fills, want 1", len(fills))
	}
	if got := dec.Str(dec.FromProto(fills[0].GetPrice())); got != "12345.67" {
		t.Fatalf("price round-tripped as %q, want exactly 12345.67", got)
	}
	if got := dec.Str(dec.FromProto(fills[0].GetQuantity())); got != "0.5" {
		t.Fatalf("quantity round-tripped as %q, want exactly 0.5", got)
	}
	// The adapter must receive the tenant and the order it was asked to work.
	if srv.gotTenant != "acme" || srv.gotOrder != "ORD-1" {
		t.Fatalf("adapter saw tenant=%q order=%q", srv.gotTenant, srv.gotOrder)
	}
	if v.MIC() != "XBIN" {
		t.Fatalf("MIC = %q", v.MIC())
	}
}

func TestGRPCVenueNoFillsIsNotAnError(t *testing.T) {
	// A resting limit order that did not trade. Turning this into an error would
	// make the OMS treat a working order as a failed one.
	v := NewGRPCVenue("XBIN", dialStub(t, &stubAdapter{fills: nil}), "acme")
	fills, err := v.Execute(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("no-fill execute returned error: %v", err)
	}
	if len(fills) != 0 {
		t.Fatalf("got %d fills, want 0", len(fills))
	}
}

func TestGRPCVenueExecuteErrorDoesNotInventFills(t *testing.T) {
	// The adapter is down / rejected the order. The OMS must hear an error and
	// must NOT receive fills — fabricating a fill here books a trade that never
	// happened.
	srv := &stubAdapter{execErr: status.Error(codes.Unavailable, "exchange unreachable")}
	v := NewGRPCVenue("XBIN", dialStub(t, srv), "acme")

	fills, err := v.Execute(context.Background(), testOrder())
	if err == nil {
		t.Fatal("execute: want error when the adapter fails, got nil")
	}
	if len(fills) != 0 {
		t.Fatalf("fabricated %d fills on a failed execute", len(fills))
	}
}

func TestGRPCVenueCancelConfirmsOnlyOnOK(t *testing.T) {
	// nil error ⇒ the venue CONFIRMED the withdrawal.
	v := NewGRPCVenue("XBIN", dialStub(t, &stubAdapter{}), "acme")
	if err := v.CancelOrder(context.Background(), testOrder()); err != nil {
		t.Fatalf("cancel: want nil (confirmed), got %v", err)
	}

	// Any error — including an ambiguous timeout — leaves the close IN FLIGHT for
	// the healing watchdog. It must never be swallowed into a nil.
	srv := &stubAdapter{cancelErr: status.Error(codes.DeadlineExceeded, "timeout")}
	v2 := NewGRPCVenue("XBIN", dialStub(t, srv), "acme")
	err := v2.CancelOrder(context.Background(), testOrder())
	if err == nil {
		t.Fatal("cancel: an ambiguous timeout was reported as a confirmed cancel")
	}
}

// GRPCVenue must satisfy both interfaces, or the OMS will silently downgrade a
// real venue cancel to a ledger-only one.
func TestGRPCVenueImplementsVenueAndCloser(t *testing.T) {
	var v any = NewGRPCVenue("XBIN", dialStub(t, &stubAdapter{}), "acme")
	if _, ok := v.(Venue); !ok {
		t.Fatal("GRPCVenue does not implement Venue")
	}
	if _, ok := v.(Closer); !ok {
		t.Fatal("GRPCVenue does not implement Closer — cancels would never reach the exchange")
	}
}

func TestRouterRejectsUnroutableOrderRatherThanSimulating(t *testing.T) {
	// The mandate's loud-demarcation rule, at the router. An order whose venue has
	// no adapter must FAIL, not quietly fall through to a simulator.
	r := NewRouter() // no venues registered at all
	_, err := r.Route(testOrder())
	if err == nil {
		t.Fatal("router matched an order with no registered venue — a silent simulator fallback")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

// TestGRPCVenueIsSelfHealing guards the cross-venue healing bug the process split
// introduced. An out-of-process adapter tracks and heals its own in-flight closes.
// If the OMS ALSO tracked them, its entry would never be resolved by anyone — and
// the in-process OKX reconciler drains that registry indiscriminately, so for an
// instrument listed on both venues (BTC-USD is) OKX would query itself for a
// Binance order id, not find it, and "heal" an order that was never its own.
func TestGRPCVenueIsSelfHealing(t *testing.T) {
	var v any = NewGRPCVenue("XBIN", dialStub(t, &stubAdapter{}), "acme")
	if _, ok := v.(SelfHealing); !ok {
		t.Fatal("GRPCVenue is not SelfHealing — the OMS would double-track its closes and OKX could heal a Binance order")
	}
	// The in-process simulator is NOT self-healing: the OMS must keep tracking it.
	var sim any = NewSimVenue("XSIM")
	if _, ok := sim.(SelfHealing); ok {
		t.Fatal("SimVenue claims to heal its own closes; the OMS would stop tracking them and nothing would")
	}
}
