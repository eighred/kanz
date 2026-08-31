package server

// The gRPC shell's QueryOrder handler (#920).
//
// It carries the same contract the RPC's doc states, and every test here is
// about the boundary between "the venue answered" and "the venue could not be
// asked". A verdict travels ONLY on the OK path; a failure travels ONLY as a
// status code. Collapsing those is a duplicate order at a real exchange, because
// ORDER_VIEW_STATE_UNKNOWN is what order.Reconcile turns into a re-drive.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lifecyclepb "github.com/eighred/kanz/kanz-schemas-go/lifecycle/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// queryableVenue is a connector that can be asked about an order — the shape
// both real connectors now have.
type queryableVenue struct {
	fakeVenue
	view       execution.OrderView
	queryErr   error
	queryCalls int
}

func (q *queryableVenue) QueryOrder(context.Context, *orderpb.OrderState) (execution.OrderView, error) {
	q.queryCalls++
	if q.queryErr != nil {
		return execution.OrderView{}, q.queryErr
	}
	return q.view, nil
}

func newQueryServer(t *testing.T, v *queryableVenue) *Server {
	t.Helper()
	closes := execution.NewCloseRegistry()
	v.closes = closes
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(v, orderview.NewMemory(), closes, execution.AccountProof{Verified: true, ExchangeAccountID: "12345678"}, halt.OpenGate(nil), logger)
}

// The verdict and its fills reach the OMS as the connector stated them.
func TestQueryOrderServesTheConnectorsVerdict(t *testing.T) {
	v := &queryableVenue{view: execution.OrderView{
		State: execution.OrderViewFilled,
		Fills: []*orderpb.Fill{{FillId: "BTCUSDT-77", OrderId: "ORD-1"}},
	}}
	s := newQueryServer(t, v)

	resp, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: order()})
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if resp.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_FILLED {
		t.Fatalf("state = %v, want FILLED", resp.GetState())
	}
	if len(resp.GetFills()) != 1 || resp.GetFills()[0].GetFillId() != "BTCUSDT-77" {
		t.Fatalf("fills = %+v, want the venue's own fill identity", resp.GetFills())
	}
}

// AN EXHAUSTED WEIGHT BUDGET IS NOT AN ANSWER. It is this platform's own
// back-pressure — the connector declined to fire a request, so the exchange said
// nothing at all. Returning an OK response with any verdict would let the OMS act
// on a question nobody asked, and UNKNOWN would re-place the order.
func TestQueryOrderRateLimitRefusalIsAStatusNeverAVerdict(t *testing.T) {
	v := &queryableVenue{queryErr: execution.ErrRateLimited}
	s := newQueryServer(t, v)

	resp, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: order()})
	if err == nil {
		t.Fatalf("an exhausted rate-limit budget returned an OK response (%v) — the reconciler "+
			"would read our own back-pressure as an answer about the order", resp)
	}
	if resp != nil {
		t.Fatalf("a refused query returned a response body (%v) alongside its error", resp)
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted so the OMS can tell back-pressure from an outage", got)
	}
}

// A TRANSPORT FAULT AT THE EXCHANGE IS NOT AN ANSWER EITHER.
func TestQueryOrderExchangeFailureIsAStatusNeverAVerdict(t *testing.T) {
	v := &queryableVenue{queryErr: errors.New("binance: dial tcp: i/o timeout")}
	s := newQueryServer(t, v)

	resp, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: order()})
	if err == nil {
		t.Fatalf("an unreachable exchange returned an OK response (%v)", resp)
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
}

// A CONNECTOR THAT CANNOT ASK ITS EXCHANGE ANSWERS Unimplemented, which
// GRPCVenue reads as INDETERMINATE — the quarantine the OMS performed before
// this RPC existed. It must NOT be Unavailable: that would be retried forever.
func TestQueryOrderOnANonQuerierConnectorIsUnimplemented(t *testing.T) {
	v := &fakeVenue{}
	s, _, _ := newServer(t, v)

	_, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: order()})
	if err == nil {
		t.Fatal("a connector that is not a Querier answered a query")
	}
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented — anything retryable turns a quarantine into a "+
			"redelivery loop that ends in a DLQ", got)
	}
}

// An order id is required: without one there is nothing to ask about, and the
// connector would address the exchange with an empty client order id.
func TestQueryOrderRequiresAnOrderID(t *testing.T) {
	v := &queryableVenue{view: execution.OrderView{State: execution.OrderViewUnknown}}
	s := newQueryServer(t, v)

	_, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: &orderpb.OrderState{}})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
	if v.queryCalls != 0 {
		t.Fatalf("the exchange was asked %d times about an order with no id", v.queryCalls)
	}
}

// QUERYORDER IS NOT HALT-GATED, AND THAT IS DELIBERATE.
//
// Execute is gated because it moves capital. This is a read, and it is the read
// an operator most needs WHILE the platform is stopped: a halt does not undo the
// orders already at the exchange, and freezing the only way to find out what
// became of them would make the kill-switch itself a source of unresolvable
// quarantines. Same boundary CancelOrder sits on.
func TestQueryOrderAnswersWhileThePlatformIsHalted(t *testing.T) {
	g := halt.OpenGate(nil)
	payload, err := proto.Marshal(&lifecyclepb.ModeChanged{
		Component: halt.ComponentSystem,
		NewMode:   lifecyclepb.OperatingMode_OPERATING_MODE_HALTED,
		ChangedBy: "operator:akif",
		Reason:    "risk breach on fund-alpha",
	})
	if err != nil {
		t.Fatalf("marshal ModeChanged: %v", err)
	}
	if err := g.Handle(context.Background(), nil, payload); err != nil {
		t.Fatalf("Gate.Handle: %v", err)
	}
	v := &queryableVenue{view: execution.OrderView{State: execution.OrderViewWorking}}
	closes := execution.NewCloseRegistry()
	v.closes = closes
	s := New(v, orderview.NewMemory(), closes, execution.AccountProof{Verified: true}, g, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{State: order()})
	if err != nil {
		t.Fatalf("a halted adapter refused to say what the exchange holds: %v", err)
	}
	if resp.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING {
		t.Fatalf("state = %v, want WORKING", resp.GetState())
	}
}

// THE LOCAL ORDER VIEW IS NOT AN ANSWER ABOUT THE EXCHANGE.
//
// s.view is this adapter's own record, written by Execute. A MISS in it means
// this process never wrote one — not that the exchange has no such order — and
// answering UNKNOWN from it would re-place an order the exchange may well hold.
// So the connector is asked for every query, including one for an order the view
// has never seen.
func TestQueryOrderAsksTheExchangeEvenForAnOrderTheViewNeverSaw(t *testing.T) {
	v := &queryableVenue{view: execution.OrderView{State: execution.OrderViewWorking}}
	s := newQueryServer(t, v)

	resp, err := s.QueryOrder(context.Background(), &venuepb.QueryOrderRequest{
		State: &orderpb.OrderState{OrderId: "NEVER-RECORDED", InstrumentId: "BTC-USD"},
	})
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if v.queryCalls != 1 {
		t.Fatalf("the connector was asked %d times; the adapter answered from its own view, "+
			"which cannot speak for the exchange", v.queryCalls)
	}
	if resp.GetState() != venuepb.OrderViewState_ORDER_VIEW_STATE_WORKING {
		t.Fatalf("state = %v, want the exchange's answer", resp.GetState())
	}
}
