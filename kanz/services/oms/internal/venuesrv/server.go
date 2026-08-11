// Package venuesrv serves the venue.v1 catalogue read surface (#406): what this
// deployment can actually trade.
//
// IT ANSWERS FROM CONFIGURATION THE OMS ALREADY ASKED FOR, and holds nothing of
// its own. The catalogue is built once, at dial time, from each adapter's own
// symbol map — the same act that asks the adapter which account it holds. There
// is no cache to invalidate and no second source of truth, because the set only
// changes when an adapter is redeployed, and an adapter restart already makes the
// OMS ask again.
//
// IT REPORTS WHAT CAN BE ROUTED, NEVER WHAT AN EXCHANGE OFFERS. A pair with no
// symbol mapping cannot be routed — the router refuses it and admission rejects
// the order. Listing one here would put a pair in front of a user that the
// platform then refuses, and an operator reads that refusal as a platform fault
// rather than as missing configuration.
package venuesrv

import (
	"context"

	"google.golang.org/grpc"

	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"

	"github.com/eighred/kanz/internal/execution"
)

// Server implements venue.v1.VenueQueryService over the catalogue the
// composition root built while dialling the adapters.
type Server struct {
	venuepb.UnimplementedVenueQueryServiceServer

	// catalogue is immutable after construction. It is read on every request and
	// written by nobody: the composition root builds it before this server is
	// registered, so there is no window in which a reader sees a partial one and
	// no lock to get wrong.
	catalogue []*venuepb.TradeableInstrument

	// ownerTenant is the tenant that owns this deployment (OMS_TENANT), stamped
	// on the reply as the deny-by-default authz input. Empty fails CLOSED.
	ownerTenant string
}

// New returns a Server over a catalogue.
//
// The proto messages are built ONCE, here, rather than per request: the
// catalogue never changes, and rebuilding it on every call would allocate a full
// copy of the tradeable universe for a screen that shows a list of names.
func New(catalogue []execution.VenueInstrument, ownerTenant string) *Server {
	out := make([]*venuepb.TradeableInstrument, 0, len(catalogue))
	for _, in := range catalogue {
		out = append(out, &venuepb.TradeableInstrument{
			InstrumentId:  in.InstrumentID,
			VenueSymbol:   in.VenueSymbol,
			Mic:           in.MIC,
			BaseAsset:     in.Pair.Base,
			QuoteAsset:    in.Pair.Quote,
			QuoteMismatch: in.QuoteMismatch,
		})
	}
	return &Server{catalogue: out, ownerTenant: ownerTenant}
}

// Register attaches the service to a gRPC server.
func (s *Server) Register(g *grpc.Server) { venuepb.RegisterVenueQueryServiceServer(g, s) }

// ListTradeableInstruments returns every pair this deployment can route.
//
// IT CANNOT FAIL, AND THAT IS NOT A SHORTCUT. There is no store to be
// unavailable and no upstream to time out — the answer was settled at startup,
// and an OMS that could not build it would not have started. An empty list is
// therefore a FACT about this deployment ("it can trade nothing"), never a
// symptom of a read that did not work.
func (s *Server) ListTradeableInstruments(context.Context, *venuepb.ListTradeableInstrumentsRequest) (*venuepb.ListTradeableInstrumentsResponse, error) {
	return &venuepb.ListTradeableInstrumentsResponse{
		Instruments: s.catalogue,
		OwnerTenant: s.ownerTenant,
	}, nil
}
