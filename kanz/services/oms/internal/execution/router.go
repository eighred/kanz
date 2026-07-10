package execution

import (
	"errors"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// ErrNoVenue is returned when the router has no venue to work an order.
var ErrNoVenue = errors.New("execution: no venue available")

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
		return nil, fmt.Errorf("%w: target venue %q is not configured", ErrNoVenue, target)
	}
	return r.venues[0], nil
}
