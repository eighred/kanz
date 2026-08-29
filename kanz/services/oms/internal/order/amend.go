package order

import (
	"context"

	commandpb "github.com/eighred/kanz/kanz-schemas-go/command/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/platform/halt"
	"github.com/eighred/kanz/services/oms/internal/approval"
)

// ReasonAmendRequiresApproval is the outcome code for an amend whose new terms
// are at or above the dual-control threshold on a platform where the control is
// ARMED (#799).
//
// IT IS A REFUSAL AND NOT A HOLD, and the distinction is the whole design of the
// answer. Holding requires a proposal: hold() writes an OrderPendingApproval for
// a SubmitOrder, handleApprove proves a second signature against the digest of
// THAT command, and submit replays it. An amend has no such proposal and no such
// replay — there is nowhere for a second signature to land, and inventing one
// would put a second admission path beside the one submit already is.
//
// The caller is not blocked, and is told the route that works: CANCEL the order
// and submit a replacement. That path already carries maker-checker end to end,
// withdraws at the venue before recording, and is the path a size increase
// belongs on anyway — an amend that raises quantity is a new order wearing an
// old id.
const ReasonAmendRequiresApproval = "DUAL_CONTROL_AMEND_UNSUPPORTED"

// submitFromState renders an order's STORED terms as the SubmitOrder a fresh
// submission of those same terms would carry.
//
// WHY IT EXISTS AS ONE FUNCTION. Two callers need this mapping and they needed
// it for opposite reasons: the schedule driver derives a child command from its
// parent's state, and the amend path derives a candidate command from the state
// an amend would leave behind so the admission controls can be asked about it.
// The driver's copy was written first and hand-listed its fields — and it
// already had the defect a hand-listed copy always gets, silently dropping
// leverage and margin_mode, so a levered parent's slices reached admission as
// spot. That is exactly the failure cloneState's own comment describes about the
// field-by-field state copy that dropped venue_account_id, one layer out.
//
// It is deliberately NOT proto.Clone-shaped: SubmitOrder and OrderState are
// different messages, and the fields that exist on the state but not on the
// command (status, filled/leaves quantity, venue_ack_at, the announcement
// markers) are outcomes rather than instructions. What this copies is every
// field of the ORDER ITSELF, so a field added to both messages is the only thing
// a future edit has to remember.
//
// md is the command metadata of whoever is asking, which the state does not
// carry: the issuer is recorded on the compliance decision and digested by the
// dual-control gate, so it must be the person making THIS request rather than
// the one who submitted the order.
func submitFromState(st *orderpb.OrderState, md *commandpb.CommandMetadata) *orderpb.SubmitOrder {
	return &orderpb.SubmitOrder{
		Metadata:     md,
		OrderId:      st.GetOrderId(),
		PortfolioId:  st.GetPortfolioId(),
		InstrumentId: st.GetInstrumentId(),
		Side:         st.GetSide(),
		Quantity:     st.GetOrderedQuantity(),
		OrderType:    st.GetOrderType(),
		LimitPrice:   st.GetLimitPrice(),
		StopPrice:    st.GetStopPrice(),
		TimeInForce:  st.GetTimeInForce(),
		ExpireAt:     st.GetExpireAt(),
		Venue:        st.GetVenue(),
		// The schedule and the parent relation are part of what the order IS, not
		// of how it turned out, so they belong to any command that would recreate
		// it. emitChild clears the schedule for its slices — a child is not itself
		// worked as a schedule — and that override is stated where it happens.
		ExecutionSchedule: st.GetExecutionSchedule(),
		ParentOrderId:     st.GetParentOrderId(),
		Leverage:          st.GetLeverage(),
		MarginMode:        st.GetMarginMode(),
	}
}

// amendReducesExposure reports whether the amended terms commit the fund to no
// more than the terms the controls already admitted.
//
// THE TEST IS MONOTONE AND NEEDS NO PRICE ORACLE. Every control on the admission
// path sizes an order as quantity x price, and both factors are non-negative
// (side carries direction, not sign). So an amend that raises NEITHER cannot
// raise the number any control would compute, whatever price source is asked and
// however the market has moved since — which is what lets this be decided from
// the two states alone rather than from a mark this package does not have.
//
// It compares the STATES rather than the command's optional fields, because a
// field the amend left alone is equal on both sides and compares as no change.
// A nil decimal compares as zero (dec.Cmp), so an amend that puts a limit price
// on an order that had none reads as an INCREASE — the fail-closed direction,
// and the right one: a price appearing where there was none is a term no control
// has ever seen.
//
// WHY REDUCTIONS ARE LET THROUGH AT ALL, rather than re-checked like everything
// else. This platform has already answered that question twice, in writing, on
// the two paths either side of this one. submit does not re-check a schedule's
// children because "IT BREAKS ORDERS THE MANDATE ALLOWED" — a later refusal
// strands a decision half-taken. handleCancel is deliberately not halt-gated
// because "an operator halting on a risk breach must still be able to get out of
// the book." A de-risking amend is the same shape as both: refusing it leaves
// the LARGER order resting, which is strictly the worse book. The control plane
// decides whether exposure may GROW; nothing on this platform stops it shrinking.
func amendReducesExposure(prev, next *orderpb.OrderState) bool {
	return dec.Cmp(next.GetOrderedQuantity(), prev.GetOrderedQuantity()) <= 0 &&
		dec.Cmp(next.GetLimitPrice(), prev.GetLimitPrice()) <= 0
}

// admitAmendment runs the admission controls over the terms an amend would leave
// on the order, and is what stops handleAmend being a way around them (#799).
//
// # What it is repairing
//
// handleAmend called none of them. Not s.gate.Check, not halt.Refusal, not
// s.dualControl — every one of those appeared exactly once in this service, all
// three inside submit. The aggregate bounds an amended quantity only BELOW,
// against filled_quantity, so the size could go anywhere upward; and the only
// brake in front of it, venueMayBeWorking, is a venue-divergence guard (#740)
// that says nothing about compliance and does not cover an order resting in
// ACCEPTED or PENDING_NEW — which is where work() leaves every order in a paper
// deployment (s.router == nil) and every order whose route nacked awaiting
// redelivery (execution.ErrNoVenue).
//
// So: an order for 100 clears the mandate and rests. An amend raises it to
// 1,000,000. A venue is configured later, the startup sweep routes the amended
// size, and no mandate rule, no second signature and no kill switch ever saw the
// number that traded. CLAUDE.md's order path says no step is skippable; this was
// the one mutation that skipped all of them at once.
//
// # Why it is a re-entry and not a new control
//
// The approve path already establishes the shape: a held order replays through
// submit "so the approved path and the submit path are one path", because the
// clearance an order got when it was proposed is not the clearance it deserves
// today. An amend is the same problem stated over a longer interval — the order
// may have rested for hours — so it asks the same three questions of the same
// three seams, over a candidate command built from the amended state.
//
// # Why the answer to dual control is a refusal
//
// See ReasonAmendRequiresApproval. There is no proposal for an amend and nowhere
// for a second signature to land, so the fail-closed answer is to refuse and name
// the path that works.
//
// A returned error is TRANSIENT (the gate could not answer) and must redeliver;
// a returned *RejectError is terminal and the caller answers the client with it.
func (s *Service) admitAmendment(ctx context.Context, env *envelopepb.Envelope, md *commandpb.CommandMetadata, prev, next *orderpb.OrderState) (*RejectError, error) {
	if amendReducesExposure(prev, next) {
		return nil, nil
	}

	// THE PLATFORM KILL-SWITCH (#635), reaching the mutation it never reached.
	// The halt's own rule is that no new exposure comes into existence while the
	// platform is stopped, and an amend that raises an order's size creates
	// exposure exactly as a submission does — it simply does it to an order that
	// already exists. A cancel stays ungated for the reason handleCancel gives,
	// and a reducing amend is let through above on the same reasoning.
	if reason, halted := halt.Refusal(s.halted); halted {
		s.logger.Warn("oms: amend refused — platform halted",
			"order_id", next.GetOrderId(), "portfolio_id", next.GetPortfolioId(), "reason", reason)
		return &RejectError{Code: ReasonPlatformHalted, Msg: reason}, nil
	}

	cand := submitFromState(next, md)

	// MAKER-CHECKER OVER THE NEW TERMS.
	//
	// A CHILD IS CLASSIFIED HERE, THOUGH SUBMIT DOES NOT CLASSIFY ONE. The
	// carve-out on the submit path exists because a slice's size was decided once,
	// for the whole notional, at the parent — counting each slice would report one
	// decision as slice_count single-signed orders. That argument does not survive
	// an amend: an amend that raises a slice above what the parent's schedule
	// derived is not part of the parent's decision, it is a term nobody decided.
	// The parent's clearance covers the plan it produced, not an edit to it.
	dual := s.dualControl.Decide(cand)
	if dual.Required {
		return &RejectError{Code: ReasonAmendRequiresApproval, Msg: "the amended terms are at or " +
			"above the dual-control threshold, and an amend cannot carry a second signature — " +
			"there is no proposal for it to be bound to. Cancel this order and submit a " +
			"replacement, which withdraws at the venue before recording and goes through " +
			"maker-checker for the new size"}, nil
	}
	if dual.Posture == approval.PostureAtOrAbove {
		// UNARMED IS NOT UNRECORDED — submit's stance on the same posture, and for
		// the same reason. The act and digest a second signature would have had to
		// cover are recorded now, so arming the control later does not leave
		// today's large amendments ambiguous.
		s.logger.Warn("an amend at or above the dual-control threshold is being APPLIED ON ONE "+
			"SIGNATURE — the control is not armed (#410, #799)",
			"order_id", cand.GetOrderId(), "portfolio_id", cand.GetPortfolioId(),
			"act", string(dual.Act), "digest", dual.Digest, "digest_err", dual.DigestErr)
	}

	// PRE-TRADE COMPLIANCE OVER THE NEW TERMS, SCOPED TO THE ENVELOPE'S TENANT.
	//
	// The gate projects the post-trade book as holdings plus this order's whole
	// delta, so the candidate carries the AMENDED ordered quantity — the position
	// the fund would hold if the amended order filled, which is the question a
	// mandate is there to answer. s.tenant is the wrong tenant to ask under: the
	// shipped OMS serves __system__, and RequireTenantScope lets every tenant's
	// command through on that setting (#223, #243).
	//
	// THE ENVELOPE IS TAKEN WHOLE RATHER THAN AS A PRE-EXTRACTED STRING, and that
	// is not a style choice. test/arch's mandate tenant-scope guard is default-deny
	// over the EXPRESSION at this call site: env.GetTenantId() is on its allow-list
	// because the api-gateway stamps it from the authenticated principal, and a
	// `tenant string` parameter would have to be exempted instead — which is the
	// guard being talked out of the one question it exists to ask.
	breach, err := s.gate.Check(ctx, env.GetTenantId(), cand)
	if err != nil {
		return nil, err // transient gate failure ⇒ redeliver, never refuse
	}
	if breach != nil {
		return &RejectError{Code: "COMPLIANCE_" + breach.Code, Msg: breach.Reason}, nil
	}
	return nil, nil
}
