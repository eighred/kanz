package order

import (
	"fmt"

	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/execution"
)

// Action is what to do with an order whose work was interrupted.
//
// THE ZERO VALUE FREEZES. Every other value causes something to happen to
// somebody's capital, so the value a caller gets by forgetting to assign one
// must be the value that does nothing.
type Action int

const (
	// ActionQuarantine freezes the order: no re-drive, no cancel, no further
	// automatic handling. It is the answer whenever venue truth and our own
	// record cannot be reconciled, and it is deliberately the zero value.
	ActionQuarantine Action = iota
	// ActionRedrive works the order at the venue now. Valid ONLY when the venue
	// has affirmatively stated it has no such order.
	ActionRedrive
	// ActionAdopt takes the venue's truth — its fills, or its rejection — as
	// ours. The venue is the authority on what it did.
	ActionAdopt
	// ActionLeave does nothing: the venue holds a live order and is working it.
	ActionLeave
)

func (a Action) String() string {
	switch a {
	case ActionQuarantine:
		return "QUARANTINE"
	case ActionRedrive:
		return "REDRIVE"
	case ActionAdopt:
		return "ADOPT"
	case ActionLeave:
		return "LEAVE"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// Reconcile decides what to do with an interrupted order, given what the venue
// says about it. It is pure: same inputs, same decision, on the live path and in
// a test.
//
// The policy, in full:
//
//	venue answer      | venue_ack_at unset | venue_ack_at set
//	------------------|--------------------|------------------
//	UNKNOWN           | REDRIVE            | QUARANTINE
//	WORKING           | LEAVE              | LEAVE
//	PARTIALLY_FILLED  | ADOPT              | ADOPT
//	FILLED            | ADOPT              | ADOPT
//	REJECTED          | ADOPT              | ADOPT
//	INDETERMINATE     | QUARANTINE         | QUARANTINE
//
// Only ONE cell reads venue_ack_at, and it is the only cell where it could
// possibly matter. Everywhere else the venue has made a positive statement about
// an order it holds, and the venue is the authority on that — our record of
// whether we heard an acknowledgement adds nothing. Where the venue says it has
// NO such order, our record is the entire question: never acknowledged means it
// truly never arrived, so work it; acknowledged-then-denied means two
// authorities contradict each other, and the correct action is to stop.
//
// A returned reason is populated for ActionQuarantine and empty otherwise.
func Reconcile(st *orderpb.OrderState, view execution.OrderView) (Action, string) {
	// A terminal order has nothing left to work. Reaching here with one is a
	// caller bug, and any answer other than "stop" would re-trade it.
	if IsTerminal(st) {
		return ActionQuarantine, fmt.Sprintf(
			"reconciliation was asked about a %s order, which is terminal and has nothing to resume; "+
				"this is a caller defect, not a venue disagreement", st.GetStatus())
	}

	switch view.State {
	case execution.OrderViewWorking:
		return ActionLeave, ""

	case execution.OrderViewFilled, execution.OrderViewPartiallyFilled, execution.OrderViewRejected:
		return ActionAdopt, ""

	case execution.OrderViewUnknown:
		if st.GetVenueAckAt() == nil {
			// The venue never confirmed it, and the venue says it does not have
			// it. Both records agree: it never arrived. Working it now is the
			// whole point of resuming.
			return ActionRedrive, ""
		}
		return ActionQuarantine, fmt.Sprintf(
			"venue acknowledged this order at %s and now reports UNKNOWN. Two authorities "+
				"contradict each other: our record says the venue held it, the venue says it "+
				"never did. Re-driving would trade the fund twice if our record is right; "+
				"abandoning would strand a live exchange order if the venue is wrong. "+
				"Resolve against the venue's own order history before clearing this",
			st.GetVenueAckAt().AsTime().UTC().Format("2006-01-02T15:04:05Z"))

	default:
		// OrderViewIndeterminate and anything a future venue adapter returns that
		// this policy has not been taught. Both freeze: an answer we cannot map
		// is not an answer.
		reason := fmt.Sprintf("venue returned %s — nothing was established about this order", view.State)
		if view.Reason != "" {
			reason += ": " + view.Reason
		}
		return ActionQuarantine, reason
	}
}
