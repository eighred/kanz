package compliance

import (
	"context"
	"log/slog"

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
	// TenantID is WHOSE decision this is, carried on the record rather than left
	// on the context.
	//
	// AN ASYNCHRONOUS RECORDER CANNOT RECOVER IT (#713). pkg/bus stashes the
	// inbound delivery's tenant on ctx, and every synchronous publisher reads it
	// from there — but a recorder that queues the decision and publishes it from a
	// background worker has left that context behind by the time it runs, and the
	// OMS's producer carries no tenant fallback: the broker refuses an event with
	// an empty tenant_id, which crash-looped that service once already.
	//
	// It is also the more correct shape for the synchronous path. A decision about
	// acme's portfolio filed under whatever tenant the deployment was configured
	// with would be a value that is valid, not theirs, and undetectable
	// downstream — the argument authbus.WithFallbackTenant makes for authorization
	// decisions, applied to compliance ones.
	TenantID string
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

// SlogRecorder records compliance decisions as structured log lines.
//
// IT IS A REAL AUDIT SINK, NOT A STUB, and it exists because the alternative
// shipped for months: services/oms/cmd/oms passed a bare nil for this seam, so
// the pre-trade gate — the enforcement point that decides whether capital
// MOVES — recorded nothing anywhere, while this file's own type doc claimed
// "every decision is recorded (COMP-01e), pass or reject, so the audit trail is
// complete". The platform could say a breach had been OBSERVED and could not say
// an order had been CHECKED.
//
// kanz logs are stdout JSON shipped by the platform (OBS-01a), so a decision
// logged here lands in the same pipeline as everything else. That is weaker than
// the bus-backed recorder — it is not on the AUDIT-01 hash chain and cannot be
// queried beside the FACTs it justified — and it is the difference between a
// weaker sink and NO sink, which is the difference this estate designs against.
//
// WHY THE OMS DOES NOT SIMPLY TAKE THE BUS RECORDER. The one that exists
// publishes SYNCHRONOUSLY, and the OMS order path is deliberately outbox-backed:
// FACTs are written in the same transaction as the order and drained by a relay,
// so nothing on the admission path waits for a broker. Putting a synchronous
// publish in front of every order would add broker latency to admission, which
// is a different decision from this one and is tracked separately. Copied in
// shape from pkg/auth.SlogRecorder on purpose — an operator reading two decision
// logs should not have to learn two idioms for the same question.
type SlogRecorder struct{ logger *slog.Logger }

// NewSlogRecorder returns a recorder writing to logger (slog.Default() if nil).
func NewSlogRecorder(logger *slog.Logger) *SlogRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogRecorder{logger: logger}
}

var _ DecisionRecorder = (*SlogRecorder)(nil)

// Record emits one decision as a structured log record.
//
// EVERY DECISION, PASS OR REJECT, AT INFO. A recorder that logged only refusals
// would answer "why was this order stopped" and not "why was this one allowed",
// and the second is the question a regulator asks about the trade that lost the
// money. It never returns an error: there is nothing here that can fail, and a
// caller treating this as best-effort must not be given a reason to branch.
func (s *SlogRecorder) Record(ctx context.Context, rec DecisionRecord) error {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "compliance decision",
		slog.String("phase", rec.Phase),
		slog.Bool("allowed", rec.Allowed),
		slog.String("order_id", rec.OrderID),
		slog.String("issuer", rec.Issuer),
		slog.String("trigger", rec.Trigger),
		slog.Uint64("worked_slices", uint64(rec.WorkedSlices)),
		slog.String("portfolio_id", rec.Result.GetPortfolioId()),
		slog.String("mandate_id", rec.Result.GetMandateId()),
		slog.Uint64("mandate_version", rec.Result.GetMandateVersion()),
		slog.String("status", rec.Result.GetStatus().String()),
		slog.Int("violations", len(rec.Result.GetViolations())),
	)
	return nil
}

// GatePosture reports which of the pre-trade gate's seams are actually wired.
//
// # Why a gate has to be able to answer this about itself
//
// #643: the OMS composition root is one 1,400-line function, and the two seams
// it left nil sat as bare `nil` arguments on a single line inside it. One was the
// instrument classifier, which made every sector and issuer mandate limit pass
// silently for months (#640); the other was this package's DecisionRecorder. The
// estate's compensating control is 52 arch guards that parse cmd/ as source
// text, and they cannot cover "this seam should not have been nil" — a nil
// argument is syntactically identical to a deliberate one.
//
// A posture the gate reports about itself CAN be covered: the composition root
// logs it at startup and exports it, and a test can assert the gate a builder
// returns has the controls it claims. That is the difference between a control
// that is present and one that is merely constructed.
type GatePosture struct {
	// Books, Mandates: the gate cannot evaluate anything without these, and it
	// admits — see Check's own refusals — so their absence is a total loss of
	// the control rather than a degraded one.
	Books    bool
	Mandates bool
	// Classifier resolves an instrument's sector, issuer and asset class. FALSE
	// MEANS EVERY SECTOR AND ISSUER LIMIT PASSES (#640), because the dimension
	// they are written against cannot be resolved.
	Classifier bool
	// Recorder is the COMP-01e audit sink. False means no pre-trade decision is
	// recorded anywhere, so the platform cannot say an order was checked.
	Recorder bool
	// Margin is the #408 venue-margin source. False means a mandate declaring
	// margin trading cannot be gated at all — distinct from the source answering
	// UNKNOWN, which refuses.
	Margin bool
}

// Complete reports whether every seam above is wired. A gate that is not
// complete still evaluates; it evaluates LESS than its mandates claim, and
// which part is missing is the operator's business.
func (p GatePosture) Complete() bool {
	return p.Books && p.Mandates && p.Classifier && p.Recorder && p.Margin
}

// Posture reports which seams this gate holds. Safe on a nil gate, which reports
// nothing wired — the honest answer for a gate that does not exist.
func (g *PreTradeGate) Posture() GatePosture {
	if g == nil {
		return GatePosture{}
	}
	return GatePosture{
		Books:      g.books != nil,
		Mandates:   g.mandates != nil,
		Classifier: g.classifier != nil,
		Recorder:   g.recorder != nil,
		Margin:     g.margins != nil,
	}
}
