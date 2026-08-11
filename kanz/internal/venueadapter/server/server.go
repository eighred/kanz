// Package server is a venue adapter's gRPC face: the venue.v1.VenueAdapterService
// the OMS calls (INFRA-M7a). It is venue-agnostic — venue-binance and venue-okx
// both serve it, fronting their own connector.
//
// It is a thin shell on purpose. All the exchange behaviour — signing, rate
// limits, partial-fill aggregation, the healing seam — already lives in the
// connector and did not change when it crossed a process boundary. This layer
// does three things: record the order in the adapter's own view, delegate to the
// connector, and preserve the two contracts that a naive RPC shell would quietly
// destroy.
package server

import (
	"context"
	"log/slog"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/execution"
	"github.com/eighred/kanz/internal/venueadapter/orderview"
)

// requireDecimalDomain refuses a request carrying an out-of-domain Decimal
// (#246), before the order is recorded or worked.
//
// Found by the widened arch guard, not by hand: the OrderState on these two
// requests carries quantity, limit_price, filled_quantity and average_fill_price
// as common.v1.Decimal, and Decimal.exponent is a plain int32 on the wire.
// internal/execution/exchange_common.go renders those with the UNBOUNDED
// dec.FromProto, which materialises 10^abs(exponent) — an order carrying
// {1, -2000000000} does not place a wrong trade, it stops the adapter answering
// while it still reports healthy.
//
// The OMS is the only caller a NetworkPolicy lets through, which bounds WHO can
// send this, not WHAT they can send: the OrderState originates in a user command
// and reaches here through the OMS unchanged. "Only an internal caller" has
// never been a domain check.
func requireDecimalDomain(req proto.Message) error {
	path, ok := dec.InDomainDeep(req)
	if ok {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"venue: %s carries a Decimal whose exponent is outside the computable domain (|exponent| > 64)", path)
}

// Server implements venue.v1.VenueAdapterService over one exchange connector.
type Server struct {
	venuepb.UnimplementedVenueAdapterServiceServer

	venue  execution.Venue
	closer execution.Closer
	view   orderview.Store
	closes execution.CloseTracker
	proof  execution.AccountProof
	logger *slog.Logger
}

// New returns a Server fronting venue. closes is the in-flight-close registry the
// connector's healing watchdog drains — it lives in THIS process now (before the
// split, the OMS wrote it and the connector read it through a shared pointer).
//
// proof is what the EXCHANGE said about this adapter's credential at startup, and
// it is a required argument rather than an option because its zero value is the
// honest one: an adapter that never proved its account reports an unverified claim,
// and the OMS can tell the difference. An optional proof would default to "trust
// me" in exactly the deployments nobody remembered to configure.
func New(venue execution.Venue, view orderview.Store, closes execution.CloseTracker, proof execution.AccountProof, logger *slog.Logger) *Server {
	s := &Server{venue: venue, view: view, closes: closes, proof: proof, logger: logger}
	// A venue that cannot cancel AT the exchange would silently downgrade every
	// cancel to a ledger-only entry while the order stays live. BinanceVenue is a
	// Closer; assert it rather than discover otherwise in production.
	if c, ok := venue.(execution.Closer); ok {
		s.closer = c
	}
	return s
}

// Execute works the order and returns its fills.
//
// The order is recorded in the adapter's own view BEFORE it is worked. That order
// matters: the fill for a marketable order can arrive on the user-data websocket
// before this RPC even returns, and the ingester enriches it by reading exactly
// this view. Record after, and the first fill of a fast order finds nothing.
func (s *Server) Execute(ctx context.Context, req *venuepb.ExecuteRequest) (*venuepb.ExecuteResponse, error) {
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	st := req.GetState()
	if st.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "venue: order_id is required")
	}
	if err := s.view.Record(ctx, st); err != nil {
		// Not a soft failure. Working an order we have no record of leaves its
		// fills unenrichable and invisible to the reconciler — better to refuse it
		// and let the OMS see the error than to trade blind.
		s.logger.Error("venue: could not record order before working it", "mic", s.venue.MIC(), "order_id", st.GetOrderId(), "err", err)
		return nil, status.Errorf(codes.Internal, "venue: record order %s: %v", st.GetOrderId(), err)
	}

	fills, err := s.venue.Execute(ctx, st)
	if err != nil {
		// Report the failure. NEVER synthesize a fill to paper over it — a
		// fabricated fill books a trade that never happened.
		return nil, status.Errorf(codes.Unavailable, "venue: execute order %s: %v", st.GetOrderId(), err)
	}
	// Zero fills is a valid answer: a resting limit order that did not trade. It
	// is NOT an error, and turning it into one would make the OMS treat a working
	// order as a failed one.
	return &venuepb.ExecuteResponse{Fills: fills}, nil
}

// CancelOrder withdraws the order at the exchange.
//
// It Tracks the close BEFORE dispatching it. That is the In-Flight Certainty seam
// (EXEC-M4c): if the cancel times out or the answer is ambiguous, the intent is
// already registered and the healing watchdog will resolve it against the
// exchange. Tracking only on success would lose exactly the cases the watchdog
// exists for — the ambiguous ones.
//
// An OK response means the venue CONFIRMED the withdrawal. Any error means the
// close is still in flight. There is no third answer, which is why
// CancelOrderResponse has no fields.
func (s *Server) CancelOrder(ctx context.Context, req *venuepb.CancelOrderRequest) (*venuepb.CancelOrderResponse, error) {
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	st := req.GetState()
	if st.GetOrderId() == "" {
		return nil, status.Error(codes.InvalidArgument, "venue: order_id is required")
	}
	if s.closer == nil {
		return nil, status.Error(codes.Unimplemented, "venue: this venue cannot withdraw an order at the exchange")
	}

	if s.closes != nil {
		s.closes.Track(execution.CloseIntent{OrderID: st.GetOrderId()})
	}
	if err := s.closer.CancelOrder(ctx, st); err != nil {
		// The close stays tracked and in flight. The watchdog owns it now. Do NOT
		// resolve it here — an error is precisely the case where we do not know.
		return nil, status.Errorf(codes.Unavailable, "venue: cancel order %s: %v", st.GetOrderId(), err)
	}
	// Confirmed at the venue.
	if s.closes != nil {
		s.closes.Resolve(st.GetOrderId())
	}
	if err := s.recordStatus(ctx, st, orderpb.OrderStatus_ORDER_STATUS_CANCELLED); err != nil {
		// The cancel landed; only our local view is stale. Log it — do not fail the
		// RPC, or the OMS would retry a cancel that already succeeded.
		s.logger.Warn("venue: cancel confirmed but order view not updated", "mic", s.venue.MIC(),
			"order_id", st.GetOrderId(), "err", err)
	}
	return &venuepb.CancelOrderResponse{}, nil
}

// Describe reports who this adapter is: the venue it trades, the exchange account
// its credential belongs to, and whether the EXCHANGE ITSELF confirmed that.
//
// The OMS calls it at dial time and refuses to start when the answer disagrees with
// its own configuration. That is the point: the account is the collateral boundary,
// and before this RPC existed the OMS could only believe a string in its own
// manifest — so a typo posted fills to one fund's ledger rows while the exchange
// debited another's, with nothing in the platform able to notice.
func (s *Server) Describe(context.Context, *venuepb.DescribeRequest) (*venuepb.DescribeResponse, error) {
	resp := &venuepb.DescribeResponse{
		Mic:               s.venue.MIC(),
		Account:           s.venue.Account(),
		AccountVerified:   s.proof.Verified,
		ExchangeAccountId: s.proof.ExchangeAccountID,
	}

	// AND WHICH ORDER TYPES IT CAN ACTUALLY PLACE (#405), for the same reason the
	// account is here: before this field the OMS could only believe order.v1's
	// enum, which declares four types while the spot connectors translate two.
	// A stop was admitted, announced and stored, and refused only inside Execute
	// — the OMS had no way to ask.
	//
	// A CONNECTOR THAT DOES NOT IMPLEMENT OrderTypeDeclarer LEAVES THIS EMPTY,
	// and empty means "did not say", never "supports nothing". The OMS names and
	// counts such an adapter at startup and refuses only under
	// OMS_REQUIRE_ORDER_TYPE_SUPPORT — because a field added to a schema must not
	// silently become a trading outage for an adapter that predates it.
	if d, ok := s.venue.(execution.OrderTypeDeclarer); ok {
		resp.SupportedOrderTypes = d.OrderTypes()
	}
	return resp, nil
}

func (s *Server) recordStatus(ctx context.Context, st *orderpb.OrderState, next orderpb.OrderStatus) error {
	cloned, ok := proto.Clone(st).(*orderpb.OrderState)
	if !ok {
		return nil
	}
	cloned.Status = next
	return s.view.Record(ctx, cloned)
}
