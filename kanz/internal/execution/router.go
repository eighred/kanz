package execution

import (
	"errors"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// ErrNoVenue is returned when the router has NO venue at all. An OMS with no
// execution wired is a deliberate configuration (a paper/observation deployment),
// and its orders rest.
var ErrNoVenue = errors.New("execution: no venue available")

// ErrVenueNotConfigured is returned when an order NAMES a venue this OMS has no
// adapter for. It is deliberately NOT wrapped in ErrNoVenue, because the two are
// opposite situations and the caller must not confuse them:
//
//   - ErrNoVenue          — nothing is wired; resting the order is correct.
//   - ErrVenueNotConfigured — the order asked for a specific venue by name and
//     this OMS has no way to reach it. It can never be executed: not now, not on a
//     retry, not ever. Resting it tells the strategy its order is working while
//     nothing on this platform will ever send it anywhere.
//
// It used to wrap ErrNoVenue, so `errors.Is(err, ErrNoVenue)` was true for both and
// the OMS rested an order it could never fill (EXEC-M8).
var ErrVenueNotConfigured = errors.New("execution: target venue is not configured")

// Router is the smart-order-router + multi-venue allocation matrix (M4). When an
// order carries a target venue (OrderState.venue, stamped by the allocation
// fan-out), the router sends it to the venue whose MIC matches — so a signal
// fanned across Binance and OKX reaches each specific venue. With no target it
// falls back to the smart-order-routing default (first configured / best venue).
type Router struct {
	venues []Venue
}

// NewRouter builds a router over the given venues, in preference order.
func NewRouter(venues ...Venue) *Router { return &Router{venues: venues} }

// Route selects the venue to work st. A non-empty OrderState.venue routes to
// that specific venue by MIC (the allocation-matrix path); empty falls back to
// the first-configured venue (the SOR default; st is available so a richer
// policy can rank by price/liquidity/cost).
func (r *Router) Route(st *orderpb.OrderState) (Venue, error) {
	if len(r.venues) == 0 {
		return nil, ErrNoVenue
	}
	if target := st.GetVenue(); target != "" {
		for _, v := range r.venues {
			if v.MIC() == target {
				return v, nil
			}
		}
		return nil, fmt.Errorf("%w: %q", ErrVenueNotConfigured, target)
	}
	return r.venues[0], nil
}

// Supports reports whether an adapter for this MIC is configured. The OMS checks
// it at ADMISSION, so an order naming a venue that does not exist is refused
// before it is ever admitted — the same treatment a compliance breach or a
// malformed order gets, and for the same reason: it can never be executed.
func (r *Router) Supports(mic string) bool {
	for _, v := range r.venues {
		if v.MIC() == mic {
			return true
		}
	}
	return false
}
