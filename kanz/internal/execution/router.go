package execution

import (
	"errors"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
)

// ErrNoVenue is returned when the router has NO venue at all. An OMS with no
// execution wired is a deliberate configuration (a paper/observation deployment),
// and its orders rest.
var ErrNoVenue = errors.New("execution: no venue available")

// ErrUnpriced is returned when a venue cannot resolve an execution price for an
// order and never will — it has no price source for that order type. It is a
// PERMANENT condition, deliberately distinct from a transient venue fault: a
// caller must REFUSE the order (terminal) rather than return the error for
// retry, because every retry resolves the same way. The order used to rest
// silently instead, forever, looking exactly like a working limit order.
var ErrUnpriced = errors.New("execution: no price source for this order type")

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
// THE ACCOUNT IS PART OF THE ROUTE, AND IT IS THE PART THAT SPENDS THE MONEY.
//
// A venue (MIC) can hold many exchange accounts, and each adapter deployment holds
// exactly one credential — so an adapter IS an account. Routing on the MIC alone means
// every portfolio's orders reach whichever adapter happens to be configured for that
// venue, and they all margin against ITS collateral. That is cross-collateralization,
// and it happens in the router, silently, one line above the exchange.
//
// So when an order names an account (the OMS stamps it at admission from the
// portfolio's binding), the route must match BOTH: the venue AND the account. An order
// for Basket Alpha cannot reach Basket Beta's credential, because no venue in this
// router holds both.
func (r *Router) Route(st *orderpb.OrderState) (Venue, error) {
	if len(r.venues) == 0 {
		return nil, ErrNoVenue
	}
	target, account := st.GetVenue(), st.GetVenueAccountId()
	if target != "" {
		for _, v := range r.venues {
			if v.MIC() != target {
				continue
			}
			if account != "" && v.Account() != account {
				continue // right venue, WRONG COLLATERAL — keep looking
			}
			return v, nil
		}
		if account != "" {
			return nil, fmt.Errorf("%w: no adapter at %q holds account %q", ErrVenueNotConfigured, target, account)
		}
		return nil, fmt.Errorf("%w: %q", ErrVenueNotConfigured, target)
	}
	return r.venues[0], nil
}

// Supports reports whether an adapter for this MIC — and, when account is non-empty,
// for that specific exchange account — is configured. The OMS checks it at ADMISSION,
// so an order naming a venue that does not exist is refused before it is ever admitted
// — the same treatment a compliance breach or a malformed order gets, and for the same
// reason: it can never be executed.
//
// An empty account asks only "can this OMS reach that venue at all".
func (r *Router) Supports(mic, account string) bool {
	for _, v := range r.venues {
		if v.MIC() != mic {
			continue
		}
		if account != "" && v.Account() != account {
			continue
		}
		return true
	}
	return false
}

// AccountFor returns the account the configured adapter at this MIC actually trades.
//
// It is what the OMS stamps when no binding governs the portfolio: the account the
// order WILL hit is a fact whether or not anybody decided it should, and recording the
// real one is what makes shared collateral visible in the ledger instead of hidden by
// it.
func (r *Router) AccountFor(mic string) (string, bool) {
	for _, v := range r.venues {
		if v.MIC() == mic {
			return v.Account(), true
		}
	}
	return "", false
}
