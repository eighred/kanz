package compliance

import (
	"context"

	compliancepb "github.com/kanz-eng/kanz-schemas-go/compliance/v1"
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
}

// DecisionRecorder persists a compliance decision. Implementations SHOULD be
// non-blocking: the pre-trade recorder runs on the order hot path. The engine
// ships no recorder; the bus/audit-backed recorder lives in the compliance
// service (COMP-01e), and a nil recorder disables logging.
type DecisionRecorder interface {
	Record(ctx context.Context, rec DecisionRecord) error
}
