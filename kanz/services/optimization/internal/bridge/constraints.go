package bridge

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/optimization"
)

// A PROPOSAL SAID WHAT TO TRADE AND NEVER WHAT IT MAY COST (#972).
//
// bridge.ToOrders emitted every child as ORDER_TYPE_MARKET / TIME_IN_FORCE_DAY,
// so an approved rebalance was an instruction to cross the spread for whatever
// quantity the delta implied, at whatever price the book offered, for the rest of
// the day. A rebalance is sized in NOTIONAL and converted to quantity at the
// price known when it was built; between that instant and the fill nothing
// expressed the most slippage the thesis tolerates, the largest notional it may
// move, or how long the instruction stays valid.
//
// This is the sibling of #970 and they are independent: that one bounds a
// proposal's AGE, this one bounds its EFFECT. A perfectly fresh proposal can
// still be enormous, and a small one can still be stale.

var (
	// ErrConstraintsUnstated: the proposal carries no envelope at all.
	//
	// PRESENCE IS THE STATEMENT. proto3 cannot tell an unset scalar from a zero
	// one, so the envelope is a MESSAGE and its absence means nobody bounded this
	// proposal — which is not the same as bounding it at zero. Refused for the
	// reason MandateUnchecked is refused: an absent decision must not read as a
	// permissive one.
	ErrConstraintsUnstated = errors.New("this proposal states no constraint envelope")

	// ErrNotionalUnbounded: the envelope is present and names no notional ceiling.
	//
	// There is no legitimate unbounded rebalance. max_notional is the circuit
	// breaker between an optimizer bug — a weight error, a NAV read from the wrong
	// instant — and the market.
	ErrNotionalUnbounded = errors.New("this proposal's envelope sets no max_notional")

	// ErrNotionalExceeded: the trade list's aggregate notional is over the ceiling.
	ErrNotionalExceeded = errors.New("this proposal moves more notional than its envelope permits")

	// ErrUnpriceableSlippageBound: a slippage bound was set and a trade carries no
	// usable reference price to apply it to.
	//
	// REFUSED RATHER THAN DEGRADED TO A MARKET ORDER. Emitting an unprotected
	// child for a proposal that ASKED for price protection is the worst available
	// outcome: the constraint is recorded on the proposal, an auditor reads that
	// the rebalance was bounded at 20bp, and the order that filled had no limit at
	// all.
	ErrUnpriceableSlippageBound = errors.New("a slippage bound was set and this trade has no reference price")
)

// Constraints is the working shape of optimization.ProposalConstraints.
type Constraints = optimization.ProposalConstraints

// checkEnvelope refuses a proposal whose envelope is missing or unusable, and
// returns the aggregate notional it validated.
//
// THE AGGREGATE IS CHECKED, NOT EACH CHILD. A per-child ceiling is evaded by
// splitting one trade into two, and the risk being bounded is the rebalance's
// whole footprint — a bad NAV read scales every leg at once.
//
// AND THE WHOLE PROPOSAL IS REFUSED WHEN IT EXCEEDS, never truncated to fit.
// Dropping legs until the total fits leaves a partially rebalanced book: one side
// of a pair trade on, weights further from target than before the run, and no
// record of which legs went. Half a rebalance is not a smaller rebalance.
func checkEnvelope(p optimization.RebalanceProposal) (gross float64, err error) {
	c := p.Constraints
	if c == nil {
		return 0, ErrConstraintsUnstated
	}
	if !dec.IsPositive(c.MaxNotional) {
		return 0, ErrNotionalUnbounded
	}
	for _, tr := range p.Trades {
		gross += math.Abs(tr.Notional)
	}
	limit, ok := dec.FromProtoChecked(c.MaxNotional)
	if !ok {
		return gross, fmt.Errorf("%w: max_notional is not a usable Decimal", ErrNotionalUnbounded)
	}
	if new(big.Rat).SetFloat64(gross).Cmp(limit) > 0 {
		return gross, fmt.Errorf("%w: the trade list moves %.2f against a ceiling of %s",
			ErrNotionalExceeded, gross, dec.Str(limit))
	}
	return gross, nil
}

// referencePrice is the price the optimizer sized a trade at: notional / quantity.
//
// IT COMES FROM THE TRADE ITSELF and not from a price map supplied at
// materialization, deliberately. The slippage bound is measured from the price
// the PROPOSAL was built at — that is what "40bp of slippage" means to whoever
// approved it — and reading a fresher price here would silently re-anchor the
// bound to a book the approver never saw.
func referencePrice(tr optimization.ProposedTrade) (float64, bool) {
	if tr.Quantity == 0 || math.IsNaN(tr.Quantity) || math.IsInf(tr.Quantity, 0) {
		return 0, false
	}
	px := math.Abs(tr.Notional) / math.Abs(tr.Quantity)
	if px <= 0 || math.IsNaN(px) || math.IsInf(px, 0) {
		return 0, false
	}
	return px, true
}

// limitPrice renders the worst acceptable price for a trade under a slippage
// bound, in the direction that costs money: a buy may pay up, a sell may take
// less.
func limitPrice(tr optimization.ProposedTrade, bps uint32) (*commonpb.Decimal, error) {
	ref, ok := referencePrice(tr)
	if !ok {
		return nil, fmt.Errorf("%w: %s (quantity %g, notional %g)",
			ErrUnpriceableSlippageBound, tr.InstrumentID, tr.Quantity, tr.Notional)
	}
	adj := float64(bps) / 10_000
	worst := ref * (1 + adj)
	if tr.Side == optimization.Sell {
		worst = ref * (1 - adj)
	}
	d, ok := dec.ToProtoScaled(new(big.Rat).SetFloat64(worst))
	if !ok {
		// SCALED, NOT WRAPPED (#94). A limit price that wrapped would be a
		// fabricated number the venue would act on — and on a BUY a wrapped
		// negative reads as a far better price than asked for.
		return nil, fmt.Errorf("%w: %s limit price %g is not representable as a Decimal",
			ErrUnpriceableSlippageBound, tr.InstrumentID, worst)
	}
	return d, nil
}

// applyConstraints stamps the envelope onto a built child.
//
// A SLIPPAGE BOUND BECOMES A LIMIT PRICE, because that is what actually stops a
// fill; a bound checked only at proposal time bounds nothing, since the proposal
// is not where the money moves. An execution window becomes GTD + expire_at, so
// an instruction cannot outlive the thesis that produced it — TIME_IN_FORCE_DAY
// let a child rejected, requeued or slow to route still fill hours later against
// a book the proposal never saw.
//
// ZERO ON EITHER FIELD KEEPS TODAY'S BEHAVIOUR and is an explicit choice rather
// than an absence: max_slippage_bps = 0 says "cross whatever the book offers",
// which is legitimate for an unwind that must complete. The envelope's PRESENCE
// is what distinguishes that from nobody having decided.
func applyConstraints(cmd *orderpb.SubmitOrder, tr optimization.ProposedTrade, c *optimization.ProposalConstraints, now time.Time) error {
	if c == nil {
		return ErrConstraintsUnstated
	}
	if c.MaxSlippageBPS > 0 {
		px, err := limitPrice(tr, c.MaxSlippageBPS)
		if err != nil {
			return err
		}
		cmd.OrderType = orderpb.OrderType_ORDER_TYPE_LIMIT
		cmd.LimitPrice = px
	}
	if c.ExecutionWindow > 0 {
		cmd.TimeInForce = orderpb.TimeInForce_TIME_IN_FORCE_GTD
		cmd.ExpireAt = timestampOf(now.Add(c.ExecutionWindow))
	}
	return nil
}

// timestampOf renders an instant for the wire.
func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t.UTC()) }

// EnvelopeRefusalCode renders an envelope refusal as a stable, closed-vocabulary
// code, or "" for an error that is not one.
//
// FOUR ERRORS, THREE CODES. "No envelope" and "an envelope with no ceiling" are
// one operator problem — the proposal is unbounded and somebody must bound it —
// while EXCEEDED means the bound exists and this proposal is too big for it, and
// UNPRICEABLE means the bound exists and cannot be turned into a limit price.
// Three different next actions.
//
// A CODE RATHER THAN THE SENTENCE, for the reason pkg/auth's deny.code exists:
// counting refusals by grepping English is how a reworded message empties a
// dashboard.
func EnvelopeRefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrConstraintsUnstated), errors.Is(err, ErrNotionalUnbounded):
		return "PROPOSAL_UNBOUNDED"
	case errors.Is(err, ErrNotionalExceeded):
		return "NOTIONAL_EXCEEDED"
	case errors.Is(err, ErrUnpriceableSlippageBound):
		return "SLIPPAGE_BOUND_UNPRICEABLE"
	default:
		return ""
	}
}

// IsEnvelopeRefusal reports whether err is one of this file's refusals.
func IsEnvelopeRefusal(err error) bool { return EnvelopeRefusalCode(err) != "" }
