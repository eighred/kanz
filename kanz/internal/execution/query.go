package execution

import (
	"context"
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
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
	// OrderViewCancelled means the VENUE WITHDREW the order: it is gone from
	// the book and it will not trade again. A cancel this platform issued and
	// lost the answer to, an operator acting at the exchange, self-trade
	// prevention, market-maker protection.
	//
	// IT IS TERMINAL AND IT MAY HAVE TRADED FIRST. Fills carries whatever
	// executed before the withdrawal — see the OrderView doc.
	OrderViewCancelled
	// OrderViewExpired means the order's time in force ELAPSED at the venue.
	//
	// IT IS THE ORDINARY TERMINAL STATE OF AN UNFILLED IOC OR FOK ORDER, not an
	// exceptional condition, and that is why it is a verdict rather than a
	// freeze: both time-in-force values are declared supported (#486), so every
	// IOC order interrupted between Save(ROUTED) and its venue ack used to
	// quarantine for a human on an answer the venue had given in full (#924).
	//
	// DISTINCT FROM OrderViewCancelled BECAUSE THE BOOK OF RECORD IS. The
	// aggregate has two terminal statuses here, carrying two different FACTs —
	// ORDER_CANCELLED with a cancelled quantity, ORDER_EXPIRED with an unfilled
	// one — and recording an IOC that simply did not fill as "cancelled" would
	// tell an operator somebody pulled it. Fills carries what traded before the
	// window closed, under the same rule as OrderViewCancelled.
	OrderViewExpired
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
	case OrderViewCancelled:
		return "CANCELLED"
	case OrderViewExpired:
		return "EXPIRED"
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
	// PARTIALLY_FILLED, FILLED, CANCELLED and EXPIRED states. Empty otherwise.
	//
	// EMPTY MEANS SOMETHING DIFFERENT ON EACH OF THOSE, AND THAT IS WHY THE TWO
	// WITHDRAWN VERDICTS NEED NO "AND IT DID NOT TRADE" COMPANION. For
	// PARTIALLY_FILLED and FILLED an empty slice is a CONTRADICTION — the venue
	// says it traded and produced no trade — and the OMS freezes the order
	// rather than record a traded order as untraded. For CANCELLED and EXPIRED
	// it is the ORDINARY case: an unfilled IOC that expired traded nothing, and
	// there is nothing to carry. The fills already say which case this is, so a
	// third state would be a second name for a fact the payload states, and a
	// connector could then contradict itself by setting one and supplying the
	// other.
	Fills []*orderpb.Fill
	// Reason is the venue's own words for a REJECTED or INDETERMINATE answer,
	// carried into the quarantine record an operator reads.
	Reason string
}

// Querier is the optional Venue capability to ask "do you hold this order, and
// what did you do with it?".
//
// It is separate from Venue for the same reason Closer is: not every venue can
// answer. SimVenue can, because it remembers what it executed. GRPCVenue can as
// of #920: venue.v1.VenueAdapterService now carries a QueryOrder RPC, served by
// each connector over the private queryOrder its own reconciler has always
// called. A venue that still cannot — one whose adapter predates the RPC, or a
// connector that implements no Querier — reports INDETERMINATE, and the OMS
// quarantines rather than guessing on its behalf.
//
// THAT ABSENCE USED TO BE THE NORMAL CASE AND IT COST THE PLATFORM ITS CRASH
// RECOVERY. Every deployment that actually traded reached its venues through
// GRPCVenue, so Service.resume's type assertion failed for all of them and every
// interrupted ROUTED order froze for a human — and order.Reconcile's policy
// table was exercised only against the simulator.
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
