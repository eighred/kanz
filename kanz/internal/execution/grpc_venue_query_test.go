package execution

// GRPCVenue's Querier half (#920), over a REAL gRPC connection on the same
// bufconn listener the Execute tests use — so the proto round-trip and, more
// importantly, the ERROR MAPPING are exercised against the transport rather than
// against a mock that cannot produce a status code.
//
// EVERY TEST IN THIS FILE IS ABOUT ONE VALUE. OrderViewUnknown is what
// order.Reconcile turns into ActionRedrive, which places an order at a real
// exchange. A transport failure, a deadline, an exhausted rate-limit budget and
// an adapter that does not serve the RPC must each produce something else. That
// is the whole file.

import (
	"context"
	"math/big"
	"testing"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/eighred/kanz/internal/dec"
)

// queryStub is a venue adapter that answers QueryOrder with a scripted verdict
// or a scripted gRPC error.
type queryStub struct {
	venuepb.UnimplementedVenueAdapterServiceServer
	resp *venuepb.QueryOrderResponse
	err  error

	gotTenant string
	gotOrder  string
	calls     int
}

func (s *queryStub) QueryOrder(_ context.Context, req *venuepb.QueryOrderRequest) (*venuepb.QueryOrderResponse, error) {
	s.calls++
	s.gotTenant = req.GetTenantId()
	s.gotOrder = req.GetState().GetOrderId()
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

// The happy path: a verdict and its fills survive the wire, decimals intact, and
// the adapter is asked about the right order on behalf of the right tenant.
func TestGRPCVenueQueryOrderRoundTripsAVerdictAndItsFills(t *testing.T) {
	srv := &queryStub{resp: &venuepb.QueryOrderResponse{
		State: venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED,
		Fills: []*orderpb.Fill{{
			FillId:   "BTCUSDT-77",
			OrderId:  "ORD-1",
			Quantity: dec.ToProto(big.NewRat(1, 2)),
			Price:    dec.ToProto(big.NewRat(1234567, 100)),
			Venue:    "XBIN",
		}},
	}}
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, srv), "acme")

	view, err := v.QueryOrder(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if view.State != OrderViewFilled {
		t.Fatalf("state = %v, want FILLED", view.State)
	}
	if len(view.Fills) != 1 || view.Fills[0].GetFillId() != "BTCUSDT-77" {
		t.Fatalf("fills = %+v, want the venue's own fill identity", view.Fills)
	}
	// The fill identity is what the order aggregate and the position book dedup
	// on, and the price is what moves capital. Both must cross exactly.
	if got := dec.Str(dec.FromProto(view.Fills[0].GetPrice())); got != "12345.67" {
		t.Fatalf("price round-tripped as %q, want exactly 12345.67", got)
	}
	if srv.gotTenant != "acme" || srv.gotOrder != "ORD-1" {
		t.Fatalf("adapter saw tenant=%q order=%q", srv.gotTenant, srv.gotOrder)
	}
}

// A TRANSPORT FAILURE IS NOT AN ANSWER, AND MUST NEVER BE OrderViewUnknown.
//
// This is the duplicate-order test. Unavailable means the question could not be
// asked; UNKNOWN means the venue positively stated it has no such order, and
// Reconcile re-drives on it. If this test fails because the state came back
// UNKNOWN, the platform will place an order at a real exchange every time a
// venue adapter blips.
func TestGRPCVenueQueryTransportFailureIsAnErrorNeverUnknown(t *testing.T) {
	srv := &queryStub{err: status.Error(codes.Unavailable, "venue: could not ask XBIN about order ORD-1")}
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, srv), "acme")

	view, err := v.QueryOrder(context.Background(), testOrder())
	if err == nil {
		t.Fatal("a transport failure returned no error — the OMS would read the verdict as an answer")
	}
	if view.State == OrderViewUnknown {
		t.Fatal("a transport failure was reported as OrderViewUnknown, which order.Reconcile " +
			"turns into a RE-DRIVE: this places a second order at a real exchange for every network blip")
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want the zero value INDETERMINATE alongside the error", view.State)
	}
}

// A RATE-LIMIT REFUSAL IS NOT AN ANSWER EITHER, and it is the one most likely to
// be mistaken for one: the adapter DID reply, promptly, with a well-formed
// status. It still never asked the exchange anything.
func TestGRPCVenueQueryRateLimitRefusalIsAnErrorNeverUnknown(t *testing.T) {
	srv := &queryStub{err: status.Error(codes.ResourceExhausted, "venue: weight budget exhausted")}
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, srv), "acme")

	view, err := v.QueryOrder(context.Background(), testOrder())
	if err == nil {
		t.Fatal("an exhausted rate-limit budget returned no error")
	}
	if view.State == OrderViewUnknown {
		t.Fatal("an exhausted rate-limit budget was reported as OrderViewUnknown — the reconciler " +
			"would read our own back-pressure as the venue denying the order")
	}
}

// A CANCELLED CONTEXT is the deadline case, produced by the transport rather
// than scripted: same rule, and it is worth pinning separately because it is the
// failure a slow exchange actually produces.
func TestGRPCVenueQueryDeadlineIsAnErrorNeverUnknown(t *testing.T) {
	srv := &queryStub{resp: &venuepb.QueryOrderResponse{State: venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING}}
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, srv), "acme")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view, err := v.QueryOrder(ctx, testOrder())
	if err == nil {
		t.Fatal("a cancelled context returned no error")
	}
	if view.State == OrderViewUnknown {
		t.Fatal("a cancelled request was reported as OrderViewUnknown")
	}
}

// AN ADAPTER THAT DOES NOT SERVE THE RPC QUARANTINES, IT DOES NOT RETRY FOREVER.
//
// This is the upgrade-order case: the OMS gains QueryOrder before its adapters
// do. Retrying cannot fix an unimplemented method, so returning an error would
// replace the quarantine the OMS used to perform with an endless redelivery loop
// ending in a DLQ. INDETERMINATE reproduces the old behaviour exactly.
func TestGRPCVenueQueryUnimplementedIsIndeterminateNotAnError(t *testing.T) {
	// The stub embeds UnimplementedVenueAdapterServiceServer and this test uses
	// the ORIGINAL stubAdapter, which does not override QueryOrder — so the
	// Unimplemented status comes from the generated server, not from a script.
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, &stubAdapter{}), "acme")

	view, err := v.QueryOrder(context.Background(), testOrder())
	if err != nil {
		t.Fatalf("an adapter serving no QueryOrder returned an error (%v); the OMS would nack "+
			"forever instead of quarantining", err)
	}
	if view.State != OrderViewIndeterminate {
		t.Fatalf("state = %v, want INDETERMINATE so the order quarantines", view.State)
	}
	if view.Reason == "" {
		t.Fatal("no reason given — an operator reading the quarantine record learns nothing " +
			"about why the order froze")
	}
}

// A nil state is a caller defect and must not reach the wire.
func TestGRPCVenueQueryRefusesANilState(t *testing.T) {
	srv := &queryStub{}
	v := NewGRPCVenue("XBIN", "binance-main", dialStub(t, srv), "acme")
	if _, err := v.QueryOrder(context.Background(), nil); err == nil {
		t.Fatal("QueryOrder(nil) returned no error")
	}
	if srv.calls != 0 {
		t.Fatalf("the adapter was called %d times for a nil order state", srv.calls)
	}
}

// THE WIRE→VERDICT MAPPING IS EXHAUSTIVE AND FAILS CLOSED.
//
// It is a switch rather than an integer conversion so that a value this build
// has not been taught freezes the order instead of taking the meaning of
// whatever Go constant shares its number. The last row is the one that matters:
// it is a value venue.v1 does not define today.
func TestOrderViewStateMappingFailsClosedOnAnUnknownWireValue(t *testing.T) {
	cases := []struct {
		wire venuepb.OrderViewState
		want OrderViewState
	}{
		{venuepb.OrderViewState_ORDER_VIEW_STATE_UNSPECIFIED, OrderViewIndeterminate},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_INDETERMINATE, OrderViewIndeterminate},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_UNKNOWN, OrderViewUnknown},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING, OrderViewWorking},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_PARTIALLY_FILLED, OrderViewPartiallyFilled},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED, OrderViewFilled},
		{venuepb.OrderViewState_ORDER_VIEW_STATE_REJECTED, OrderViewRejected},
		// Not a value venue.v1 declares. A future adapter built against a newer
		// schema can send it, and it must not become an authoritative verdict.
		{venuepb.OrderViewState(99), OrderViewIndeterminate},
	}
	for _, tc := range cases {
		if got := orderViewState(tc.wire); got != tc.want {
			t.Fatalf("orderViewState(%v) = %v, want %v", tc.wire, got, tc.want)
		}
	}
}

// THE SAME MAPPING THE OTHER WAY, which the adapter serves through. It must
// round-trip every verdict a connector can produce, or a venue's answer changes
// meaning in transit.
func TestOrderViewStateProtoRoundTripsEveryVerdict(t *testing.T) {
	for _, s := range []OrderViewState{
		OrderViewIndeterminate, OrderViewUnknown, OrderViewWorking,
		OrderViewPartiallyFilled, OrderViewFilled, OrderViewRejected,
	} {
		if got := orderViewState(OrderViewStateProto(s)); got != s {
			t.Fatalf("%v round-tripped as %v", s, got)
		}
	}
	// An out-of-range verdict must NOT become UNSPECIFIED (which reads as "the
	// adapter said nothing") and must NOT become UNKNOWN.
	if got := OrderViewStateProto(OrderViewState(42)); got != venuepb.OrderViewState_ORDER_VIEW_STATE_INDETERMINATE {
		t.Fatalf("an unrecognised verdict mapped to %v, want INDETERMINATE", got)
	}
}

// GRPCVenue must satisfy Querier at runtime as well as at compile time: the OMS
// reaches it through a type assertion on an interface value, and a nil-typed
// assertion failure there is exactly the state this whole issue was about.
func TestGRPCVenueSatisfiesQuerierThroughTheVenueInterface(t *testing.T) {
	var venue Venue = NewGRPCVenue("XBIN", "binance-main", dialStub(t, &queryStub{}), "acme")
	if _, ok := venue.(Querier); !ok {
		t.Fatal("a GRPCVenue held as a Venue does not assert to Querier — Service.resume would " +
			"quarantine every interrupted ROUTED order, which is the whole of #920")
	}
}
