package compliance

import (
	"context"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
)

// Decision phases — the enforcement point a decision was made at.
const (
	PhasePreTrade  = "pre_trade"
	PhasePostTrade = "post_trade"
)

// DecisionRecord is one compliance decision to be audited (COMP-01e) — the
// verdict plus the context that correlates it. Both enforcement points (the
// pre-trade gate and the post-trade monitor) produce these; the service-level
// recorder maps them to observation.v1.DecisionLog and the AUDIT-01 hash chain.
type DecisionRecord struct {
	// Phase is PhasePreTrade or PhasePostTrade.
	Phase string
	// Result is the full evaluation verdict.
	Result *compliancepb.ComplianceResult
	// Allowed is the gate outcome (pre-trade); always false for a post-trade
	// breach.
	Allowed bool
	// OrderID correlates a pre-trade decision to the order it gated. Empty
	// post-trade.
	OrderID string
	// Issuer is the principal that triggered the decision (the command issuer
	// pre-trade; the monitor decider post-trade).
	Issuer string
	// Trigger is the BreachTrigger label for a post-trade breach. Empty
	// pre-trade.
	Trigger string
	// WorkedSlices is how many child orders this decision authorises, when the
	// order is worked as a schedule rather than sent whole (#435). Zero for an
	// ordinary order.
	//
	// WITHOUT IT THE AUDIT LOG CANNOT BE RECONSTRUCTED BACKWARDS. A scheduled
	// parent is checked ONCE, for the whole notional, and its children are
	// admitted without re-checking — deliberately, because every limit is
	// evaluated against a book that only moves on fills, so N children inside one
	// window each see the same unchanged book and the sum is never tested
	// (#483). The consequence is that the fills land on N order ids and only the
	// PARENT has a decision record.
	//
	// A regulator asking "why was this trade allowed" holds a child's id. The
	// link back is parent_order_id, which travels on the child's own state and
	// every FACT carrying it — but nothing on the DECISION said it authorised
	// more than the one order it names. This is that statement: one decision,
	// this many orders.
	WorkedSlices uint32
}

// DecisionRecorder persists a compliance decision. Implementations SHOULD be
// non-blocking: the pre-trade recorder runs on the order hot path. The engine
// ships no recorder; the bus/audit-backed recorder lives in the compliance
// service (COMP-01e), and a nil recorder disables logging.
type DecisionRecorder interface {
	Record(ctx context.Context, rec DecisionRecord) error
}
