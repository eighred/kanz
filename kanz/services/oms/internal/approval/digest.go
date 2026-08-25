// Package approval is the ORDER half of maker-checker (#410, act three).
//
// internal/dualcontrol owns the rule — the approver is not the proposer, the
// approval is bound to what was proposed, a pending proposal expires — and says
// in its own doc that it does NOT decide which acts need dual control or above
// what threshold, because that belongs to the caller and its configuration.
// This package is that caller's half for ORDER_SUBMISSION: the threshold
// (gate.go) and the digest an approval covers (this file).
//
// # What this package does NOT hold, and where that landed
//
// It does not hold a pending order. THE PLACEMENT RULING IS: a proposal in the
// OMS, in its own table, which never becomes an admitted order until a second
// person signs it — NOT a ninth ORDER_STATUS_PENDING_APPROVAL, because
// order_status_exhaustive_test.go pins one status by a hardcoded string and would
// pass green on a new value while the gate at service.go's work() went uncovered,
// and kanz-web is outside every arch guard in the repo (it is already missing
// ORDER_STATUS_WORKING_SCHEDULED from #435). The template for that table is
// services/datamaster/internal/store/proposals.go, whose Claim is the
// serialisation point that stops two approvers double-applying one decision.
//
// NONE OF THAT CHANGES WHAT IS HERE, and that is the point rather than a
// coincidence. Terms is derived from a SubmitOrder — the command a proposal
// holds — OR from an OrderState, the approved order once it is admitted, and
// TestTheTwoPlacementsAgreeOnTheDigest proves the two derivations hash
// identically. The second derivation is what lets anybody LATER prove the order
// that traded is the order that was signed for, from the store alone.
package approval

import (
	"fmt"
	"strconv"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/dualcontrol"
)

// Terms are the fields of an order that a dual-control signature COVERS.
//
// # Why this is the load-bearing decision in the whole control
//
// dualcontrol.Approve takes a digest and Approval.Covers refuses a mismatch —
// that pair is what stops the payload changing between propose and approve. A
// field OUTSIDE the digest is therefore a field that can be EDITED AFTER
// APPROVAL and still apply under the approver's signature. The audit trail shows
// two names and the order that trades is not the order that was shown.
//
// So the rule for adding a field to SubmitOrder is: it goes in here, or it goes
// in the exclusion list in test/arch/order_digest_covers_submit_test.go with an
// argument. The guard fails the build on a third option.
//
// # What is covered, and why each one
//
//   - ORDER_ID. Not economically material; load-bearing anyway. Approval.Covers
//     checks the ACT and the DIGEST and nothing else — it never sees the
//     proposal's Subject — so without the id in the digest an approval for order
//     A covers any order with identical terms. That is not hypothetical here:
//     the slices of one TWAP parent have identical terms by construction and
//     differ only by id (#435).
//   - PORTFOLIO_ID. Whose capital is spent and whose mandate is checked.
//   - INSTRUMENT_ID. What is bought.
//   - SIDE. Flipping it inverts the position under the same signature.
//   - QUANTITY. Size.
//   - ORDER_TYPE. It decides whether limit_price BINDS AT ALL. Approving a LIMIT
//     and applying it as MARKET removes the price protection entirely while
//     leaving limit_price untouched — so covering the price and not the type
//     covers nothing.
//   - LIMIT_PRICE, STOP_PRICE. The committed price and the trigger. stop_price
//     is on this list explicitly because it is the field this platform has
//     already validated-and-then-dropped once (#405).
//   - TIME_IN_FORCE, EXPIRE_AT. An IOC re-signed as GTC RESTS an order the
//     trader asked to be gone — the #486 defect, arrived at by editing rather
//     than by a connector.
//   - VENUE. It selects the exchange ACCOUNT and therefore the collateral pool
//     (OMS_VENUE_ACCOUNTS): an exchange margins and liquidates per account, so
//     re-routing an approved order moves it onto a different pool without
//     changing a single price or quantity.
//   - EXECUTION_SCHEDULE, all five fields. Narrowing the window from an hour to
//     a second turns an order somebody asked to be worked carefully into a
//     market sweep, at the same quantity and the same price. ExecutionSchedule's
//     own doc says the schedule is a pure function of exactly these fields, so
//     covering all five covers the schedule.
//   - PARENT_ORDER_ID. It decides whether the order is checked at all: a child
//     is admitted WITHOUT re-running the compliance gate, because its parent was
//     checked once for the whole notional (service.go). Re-parenting an order
//     after approval therefore moves it under somebody else's clearance.
//
// # What is EXCLUDED, and why — read this before adding a field
//
// CommandMetadata IN ITS ENTIRETY (issuer, target_id, valid_until, reason,
// principal_portfolios). The approve step is a DIFFERENT command with its own
// metadata — necessarily a different issuer, since the approver is by definition
// not the proposer — so hashing it would make every approval fail as a payload
// change, for a reason that has nothing to do with the payload. That is #511's
// defect generalised: effective_at defaulting to now() made propose and approve
// hash different payloads and every approval failed. A field that legitimately
// differs between the two steps must not be in the digest. Field by field:
//
//   - target_id MUST equal order_id (order.proto), and order_id is covered.
//   - issuer is not dropped, it is covered by something STRONGER than a hash:
//     dualcontrol.Proposal.Proposer is the authenticated proposer and Approve
//     refuses an approver equal to it. A digest can prove a name did not change;
//     it cannot enforce that two names differ.
//   - principal_portfolios is the gateway's entitlement snapshot for whoever
//     issued THIS delivery. Freezing the proposer's snapshot into the signature
//     would preserve an entitlement that may since have been revoked, which is
//     the opposite of what a control wants.
//   - valid_until is a staleness bound the command path already enforces.
//   - reason is free text with no economic effect. STATED RISK, not an
//     oversight: an approver who was shown a reason cannot prove from the digest
//     that it was not edited afterwards. Covering it would tie the signature to
//     a human note that the approve step has no reason to resend verbatim.
//
// EVERY DERIVED OR LIFECYCLE FIELD OF OrderState — status, filled_quantity,
// leaves_quantity, average_fill_price, venue_account_id, arrival_price,
// arrival_at, accepted_at, as_of, venue_order_id and the two announcement
// markers. They are not terms of the order; they are what happened to it. They
// also differ BY CONSTRUCTION between a proposal (nothing has happened yet) and
// a stored order, which is exactly what would make the digest un-recomputable at
// approve time — #511 a third time.
type Terms struct {
	OrderID       string
	PortfolioID   string
	InstrumentID  string
	Side          orderpb.Side
	OrderType     orderpb.OrderType
	TimeInForce   orderpb.TimeInForce
	Quantity      *commonpb.Decimal
	LimitPrice    *commonpb.Decimal
	StopPrice     *commonpb.Decimal
	ExpireAt      *timestamppb.Timestamp
	Venue         string
	ParentOrderID string
	Schedule      *orderpb.ExecutionSchedule
	Leverage      *commonpb.Decimal
	MarginMode    orderpb.MarginMode
}

// TermsOfSubmit reads the covered terms off the COMMAND — the shape the
// proposal placement holds, where the order never becomes a SubmitOrder until it
// is approved.
func TermsOfSubmit(cmd *orderpb.SubmitOrder) Terms {
	return Terms{
		OrderID:       cmd.GetOrderId(),
		PortfolioID:   cmd.GetPortfolioId(),
		InstrumentID:  cmd.GetInstrumentId(),
		Side:          cmd.GetSide(),
		OrderType:     cmd.GetOrderType(),
		TimeInForce:   cmd.GetTimeInForce(),
		Quantity:      cmd.GetQuantity(),
		LimitPrice:    cmd.GetLimitPrice(),
		StopPrice:     cmd.GetStopPrice(),
		ExpireAt:      cmd.GetExpireAt(),
		Venue:         cmd.GetVenue(),
		ParentOrderID: cmd.GetParentOrderId(),
		Schedule:      cmd.GetExecutionSchedule(),
		Leverage:      cmd.GetLeverage(),
		MarginMode:    cmd.GetMarginMode(),
	}
}

// TermsOfState reads the covered terms off the STORED ORDER — the shape the
// OMS placement holds, where the order is admitted at a pending status and
// approved from the store.
//
// IT MUST AGREE WITH TermsOfSubmit FIELD FOR FIELD. OrderState carries every one
// of them, and it does so because test/arch/submit_fields_reach_state_test.go
// exists: stop_price (#405), expire_at (#405) and leverage (#240) were each
// accepted at the perimeter and dropped before the wire, and a digest computed
// over a state missing a field would have silently covered less than the
// proposal did.
func TermsOfState(st *orderpb.OrderState) Terms {
	return Terms{
		OrderID:       st.GetOrderId(),
		PortfolioID:   st.GetPortfolioId(),
		InstrumentID:  st.GetInstrumentId(),
		Side:          st.GetSide(),
		OrderType:     st.GetOrderType(),
		TimeInForce:   st.GetTimeInForce(),
		Quantity:      st.GetOrderedQuantity(),
		LimitPrice:    st.GetLimitPrice(),
		StopPrice:     st.GetStopPrice(),
		ExpireAt:      st.GetExpireAt(),
		Venue:         st.GetVenue(),
		ParentOrderID: st.GetParentOrderId(),
		Schedule:      st.GetExecutionSchedule(),
		Leverage:      st.GetLeverage(),
		MarginMode:    st.GetMarginMode(),
	}
}

// digestParts is how many strings Digest hashes. It is FIXED, and that is what
// makes the encoding unambiguous: an unset schedule contributes five empty parts
// rather than none, so an order with no schedule cannot produce the same part
// list as one whose schedule happens to sit where the next field would.
//
// 18 → 20 WHEN LEVERAGE AND MARGIN MODE JOINED THE COVERED TERMS (#417), and
// bumping it INVALIDATES EVERY DIGEST ALREADY SIGNED. That is the intended
// consequence, not a cost paid around it: a held proposal signed before those
// terms existed was signed over an order that could not express them, so
// honouring that signature after they can is exactly the substitution the digest
// is for. An in-flight proposal must be re-proposed and re-approved.
//
// The gap was found by test/arch's TestTheOrderDigestCoversEverySubmitOrderField
// rather than by review — the fields were added to SubmitOrder and OrderState,
// carried through translate, gated at admission, and still sat outside the
// signature. Uncovered, an order approved as spot could be submitted as 10x
// cross under the approver's signature: two people named on an order neither of
// them saw.
const digestParts = 20

// Digest is the value dualcontrol.Approve is given and Approval.Covers
// re-checks.
//
// # It hashes VALUES, not encodings
//
// common.v1.Decimal has more than one encoding per value: {1,0} and {10,-1} are
// the same price. Hashing the wire bytes would mean a store that normalises an
// exponent on the way through — or a client that sends the same price two ways —
// invalidates a signature over an order nobody changed. Every decimal is
// therefore canonicalised through dec.FromProtoChecked to an exact rational and
// rendered as its RatString.
//
// UNSET AND ZERO ARE DIFFERENT. A MARKET order has no limit price; an order with
// a limit price of zero is a different (and invalid) instruction. Unset renders
// as the empty string and zero as "0/1", and dualcontrol.Digest length-prefixes
// every part, so no combination of field contents can make the two collide.
//
// # It REFUSES rather than hashing what it could not read
//
// An out-of-domain exponent is not converted to a rational (dec.FromProto does
// not return within seconds on {1, 2e9}), so a decimal outside the domain yields
// an error and no digest. Substituting zero, or the raw coefficient, would sign
// a value nobody can reproduce — and this is a signature, so an unreproducible
// value means every later approval fails for a reason no operator can see. The
// OMS refuses such a command at the perimeter already (dec.InDomainDeep in
// handleSubmit); this is the second line, because a constructor callable from
// elsewhere is not a guarantee.
func (t Terms) Digest() (string, error) {
	parts := make([]string, 0, digestParts)
	parts = append(parts,
		t.OrderID,
		t.PortfolioID,
		t.InstrumentID,
		strconv.FormatInt(int64(t.Side), 10),
		strconv.FormatInt(int64(t.OrderType), 10),
		strconv.FormatInt(int64(t.TimeInForce), 10),
		strconv.FormatInt(int64(t.MarginMode), 10),
	)
	for _, d := range []struct {
		field string
		value *commonpb.Decimal
	}{
		{"quantity", t.Quantity},
		{"limit_price", t.LimitPrice},
		{"stop_price", t.StopPrice},
		{"leverage", t.Leverage},
	} {
		s, err := decimalPart(d.field, d.value)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	parts = append(parts,
		timestampPart(t.ExpireAt),
		t.Venue,
		t.ParentOrderID,
		// The schedule, flattened. SIX parts always — see digestParts — and the
		// first of them is PRESENCE. Without it an order with no schedule and one
		// carrying an all-zero schedule hash identically, and "worked over time"
		// versus "sent whole" is the largest behavioural difference on this
		// message. Admission refuses an all-zero schedule today (algo
		// UNSPECIFIED), so the collision is currently unreachable — which is
		// exactly the kind of reachability a signature must not depend on.
		schedulePresence(t.Schedule),
		strconv.FormatInt(int64(t.Schedule.GetAlgo()), 10),
		timestampPart(t.Schedule.GetWindowStart()),
		timestampPart(t.Schedule.GetWindowEnd()),
		strconv.FormatUint(uint64(t.Schedule.GetSliceCount()), 10),
	)
	maxSlice, err := decimalPart("execution_schedule.max_slice_quantity", t.Schedule.GetMaxSliceQuantity())
	if err != nil {
		return "", err
	}
	parts = append(parts, maxSlice)

	// A part list of the wrong length means a field was added to the append
	// chain and not to digestParts, which would silently change every digest the
	// estate has already signed. Refuse rather than hash it.
	if len(parts) != digestParts {
		return "", fmt.Errorf("approval: digest built %d parts, expected %d — the covered field set "+
			"changed without digestParts being updated, which invalidates every signature already collected",
			len(parts), digestParts)
	}
	return dualcontrol.Digest(parts...), nil
}

// Propose builds the pending proposal for an order submission — the single
// PRODUCER of dualcontrol.ActOrderSubmission, which was declared unconstructed
// from #495 until this landed.
//
// IT IS HERE RATHER THAN AT A PLACEMENT because both placements build exactly
// this object and only differ in where they put it: one writes it beside the
// order row, the other into a proposals table. Retyping dualcontrol.Propose at
// each of them is how the act constant, the subject and the digest drift apart —
// the failure internal/dualcontrol's own header describes.
//
// The subject is the ORDER ID, matching the pricing path's use of the exception
// id: it is what an approver is shown and what a pending list is keyed on.
func Propose(id string, t Terms, proposer string, now time.Time, ttl time.Duration) (dualcontrol.Proposal, error) {
	digest, err := t.Digest()
	if err != nil {
		return dualcontrol.Proposal{}, err
	}
	return dualcontrol.Propose(id, dualcontrol.ActOrderSubmission, t.OrderID, proposer, digest, now, ttl)
}

// Covers is the approve-side check: does this approval authorise submitting
// THESE terms?
//
// A CALLER HOLDING AN Approval MUST STILL CALL THIS, and the reason is in
// Approval.Covers' own doc: holding one proves some approval happened, not that
// it covers what is about to be applied — and Go permits dualcontrol.Approval{}
// anywhere, which would otherwise read as "nobody approved this" being approved.
// Re-deriving the digest from the terms IN HAND, rather than from whatever the
// proposal recorded, is what makes the check cover the order that will actually
// trade.
func Covers(a dualcontrol.Approval, t Terms) error {
	digest, err := t.Digest()
	if err != nil {
		return err
	}
	return a.Covers(dualcontrol.ActOrderSubmission, digest)
}

// decimalPart renders a decimal by value, or refuses.
func decimalPart(field string, d *commonpb.Decimal) (string, error) {
	if d == nil {
		return "", nil
	}
	r, ok := dec.FromProtoChecked(d)
	if !ok {
		return "", fmt.Errorf("approval: %s carries an out-of-domain exponent (%d) and cannot be "+
			"signed for — an approval must cover a value the approver and the applier both compute the same way",
			field, d.GetExponent())
	}
	return r.RatString(), nil
}

// schedulePresence distinguishes "no schedule" from "a schedule whose fields are
// all zero". See the call site.
func schedulePresence(s *orderpb.ExecutionSchedule) string {
	if s == nil {
		return ""
	}
	return "scheduled"
}

// timestampPart renders an instant as seconds and nanoseconds rather than a
// formatted time: a layout has a timezone and a variable-width fractional part,
// and a signature must not depend on either.
func timestampPart(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return strconv.FormatInt(ts.GetSeconds(), 10) + "." + fmt.Sprintf("%09d", ts.GetNanos())
}
