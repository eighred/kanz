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

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
	venuepb "github.com/kanz-eng/kanz-schemas-go/venue/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/internal/execution"
	"github.com/kanz-eng/kanz/internal/venueadapter/orderview"
)

// Server implements venue.v1.VenueAdapterService over one exchange connector.
type Server struct {
	venuepb.UnimplementedVenueAdapterServiceServer

	venue  execution.Venue
	closer execution.Closer
	view   orderview.Store
	closes execution.CloseTracker
	logger *slog.Logger
}

// New returns a Server fronting venue. closes is the in-flight-close registry the
// connector's healing watchdog drains — it lives in THIS process now (before the
// split, the OMS wrote it and the connector read it through a shared pointer).
func New(venue execution.Venue, view orderview.Store, closes execution.CloseTracker, logger *slog.Logger) *Server {
	s := &Server{venue: venue, view: view, closes: closes, logger: logger}
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

func (s *Server) recordStatus(ctx context.Context, st *orderpb.OrderState, next orderpb.OrderStatus) error {
	cloned, ok := proto.Clone(st).(*orderpb.OrderState)
	if !ok {
		return nil
	}
	cloned.Status = next
	return s.view.Record(ctx, cloned)
}
