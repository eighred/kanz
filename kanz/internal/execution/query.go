package execution

import (
	"context"
	"fmt"

	orderpb "github.com/kanz-eng/kanz-schemas-go/order/v1"
)

// OrderViewState is what a venue says it knows about one order.
//
// THE ZERO VALUE IS INDETERMINATE, DELIBERATELY — the same stance AccountProof
// takes. The dangerous answer is "I have no such order", because that is the one
// that authorizes sending the order to the exchange again. It must never be
// something a caller can produce by forgetting to set a field.
type OrderViewState int

const (
	// OrderViewIndeterminate means nothing was established. The venue did not
	// answer, answered in a way this adapter cannot map, or was never asked.
	// It resolves to quarantine, never to an action.
	OrderViewIndeterminate OrderViewState = iota
	// OrderViewUnknown means the venue AFFIRMATIVELY has no record of this
	// order. This is a positive statement by the venue, not the absence of one.
	OrderViewUnknown
	// OrderViewWorking means the venue holds the order and it is live.
	OrderViewWorking
	// OrderViewPartiallyFilled means the venue holds the order and it has
	// partially traded. Fills carries what traded.
	OrderViewPartiallyFilled
	// OrderViewFilled means the order is fully executed. Fills carries it.
	OrderViewFilled
	// OrderViewRejected means the venue terminally refused the order.
	OrderViewRejected
)

func (s OrderViewState) String() string {
	switch s {
	case OrderViewIndeterminate:
		return "INDETERMINATE"
	case OrderViewUnknown:
		return "UNKNOWN"
	case OrderViewWorking:
		return "WORKING"
	case OrderViewPartiallyFilled:
		return "PARTIALLY_FILLED"
	case OrderViewFilled:
		return "FILLED"
	case OrderViewRejected:
		return "REJECTED"
	default:
		return fmt.Sprintf("OrderViewState(%d)", int(s))
	}
}

// OrderView is a venue's answer about one order.
//
// Fills MUST carry the venue's OWN fill identities — the same fill_id the
// original Execute reported, not freshly minted ones. The position book dedups
// folds on fill_id (position_fills), so a venue that renames its fills when
// re-queried turns exactly-once into double-counting.
type OrderView struct {
	// State is the venue's verdict. Zero value quarantines.
	State OrderViewState
	// Fills are the executions the venue attributes to this order, for the
	// PARTIALLY_FILLED and FILLED states. Empty otherwise.
	Fills []*orderpb.Fill
	// Reason is the venue's own words for a REJECTED or INDETERMINATE answer,
	// carried into the quarantine record an operator reads.
	Reason string
}

// Querier is the optional Venue capability to ask "do you hold this order, and
// what did you do with it?".
//
// It is separate from Venue for the same reason Closer is: not every venue can
// answer. SimVenue can, because it remembers what it executed. An out-of-process
// GRPCVenue cannot, because venue.v1.VenueAdapterService exposes only Execute,
// CancelOrder and Describe — the Binance and OKX REST clients each have a
// private queryOrder their own reconcilers call, and nothing surfaces it across
// the process boundary. Until that RPC exists, those venues are not Queriers and
// the OMS quarantines rather than guessing on their behalf.
//
// QueryOrder addresses the order by st.order_id — the same deterministic
// clOrdId the submit used — so it is safe to call repeatedly.
//
// ERROR DISCIPLINE: a returned error means THE QUESTION COULD NOT BE ASKED (the
// venue was unreachable, rate-limited, timed out). That is transient: the caller
// backs off and asks again. It is NOT the same as an OrderViewUnknown answer,
// which is the venue positively stating it has no such order. Collapsing those
// two turns a network blip into a re-driven order.
type Querier interface {
	QueryOrder(ctx context.Context, st *orderpb.OrderState) (OrderView, error)
}
