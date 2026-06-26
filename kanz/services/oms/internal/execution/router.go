package execution

import (
	"errors"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// ErrNoVenue is returned when the router has no venue to work an order.
var ErrNoVenue = errors.New("execution: no venue available")

// Router is the smart-order-router seam. The default selects the first
// configured venue; a real SOR ranks venues by price/liquidity/cost per the
// order. Keeping it an explicit type (not a bare slice) means the selection
// policy can grow without touching callers.
type Router struct {
	venues []Venue
}

// NewRouter builds a router over the given venues, in preference order.
func NewRouter(venues ...Venue) *Router { return &Router{venues: venues} }

// Route selects the venue to work st. The default policy is first-configured;
// st is passed so a richer policy can route by instrument/venue eligibility.
func (r *Router) Route(_ *orderpb.OrderState) (Venue, error) {
	if len(r.venues) == 0 {
		return nil, ErrNoVenue
	}
	return r.venues[0], nil
}
