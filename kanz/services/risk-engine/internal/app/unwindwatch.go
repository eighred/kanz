package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	compliancepb "github.com/eighred/kanz/kanz-schemas-go/compliance/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/risk/unwind"
	"github.com/eighred/kanz/pkg/bus"
)

// A BREACH NOW TRIGGERS THE UNWIND DECISION PATH — AND STILL EXECUTES NOTHING
// (#74).
//
// internal/risk/unwind has been able to answer "what would this portfolio have to
// shed" since M4, and nothing ever asked it. `grep` for a caller outside its own
// tests returned nothing: the arithmetic was built, tested, and unreachable. A
// skeleton nobody calls is indistinguishable from a skeleton that does not work,
// which is what #74's "Verified when" was actually asking about.
//
// WHY THE RISK-ENGINE AND NOT COMPLIANCE, which is where the breach is detected
// and would have been the shorter wire: test/arch/risk_boundary_test.go permits
// only services/risk-engine/ to import the risk module's impl packages, and
// unwind is one. That is the RISK-02 boundary working as designed rather than an
// obstacle — the alternative was punching a hole in it for convenience.
//
// IT DECIDES. IT DOES NOT ACT. The proposal is logged and counted, and goes
// nowhere else. That restraint is the feature, not an unfinished edge:
// auto-deleveraging that ACTS is long-term work gated on an operator's judgement,
// because a breach is often just a price moving and a book that liquidates into a
// falling market is how a risk control becomes the accelerant. This handler is
// structurally incapable of placing an order — it imports no venue adapter, no
// order schema and no producer, and test/arch/unwind_cannot_execute_test.go keeps
// the unwind package that way.

// UnwindWatch folds ComplianceBreach FACTs into unwind proposals.
//
// Stateless by construction: each breach is decided from its own evidence, so
// there is nothing to keep between deliveries and no ordering requirement. That
// is what lets it run under an ordinary load-balanced consumer group.
type UnwindWatch struct {
	// tenant is the tenant this risk-engine serves. A breach from another one
	// must not be decided here — see Handle.
	tenant string
	now    func() time.Time
	logger *slog.Logger

	decided     *prometheus.CounterVec
	breachAge   prometheus.Histogram
	undecidable prometheus.Counter
}

// UnwindWatchOption customizes an UnwindWatch.
type UnwindWatchOption func(*UnwindWatch)

// WithUnwindClock injects the clock (tests).
func WithUnwindClock(now func() time.Time) UnwindWatchOption {
	return func(w *UnwindWatch) {
		if now != nil {
			w.now = now
		}
	}
}

// NewUnwindWatch registers the proposal metrics and returns the handler.
//
// THE COUNTERS ARE THE POINT, not decoration. A proposal that is only logged is
// invisible the moment stdout rotates, and the two numbers an operator needs are
// different questions: how many breaches the platform could size a reduction for,
// and how many it could NOT. The second is the one that must never read as zero
// by omission — a rule nobody can unwind is a rule a human has to handle, and it
// looks exactly like a quiet system if it is not counted.
func NewUnwindWatch(reg prometheus.Registerer, tenant string, logger *slog.Logger, opts ...UnwindWatchOption) *UnwindWatch {
	w := &UnwindWatch{
		tenant: tenant,
		now:    time.Now,
		logger: logger,
		decided: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_risk_unwind_proposals_total",
			Help: "Unwind proposals derived from compliance breaches. actionable=\"true\" means a " +
				"reduction was sized; \"false\" means the breach was understood and nothing could be " +
				"derived for any of its rules. NOTHING IS EXECUTED either way (#74).",
		}, []string{"actionable"}),
		breachAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kanz_risk_unwind_breach_age_seconds",
			Help: "Seconds between a breach being DETECTED by the compliance monitor and this " +
				"engine sizing a reduction for it. A proposal is arithmetic against the book as " +
				"it was; a large lag means the answer may already be wrong, and DecidedAt alone " +
				"cannot show that because it is the time this handler ran.",
			Buckets: []float64{0.1, 0.5, 1, 5, 15, 60, 300, 1800},
		}),
		undecidable: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_risk_unwind_undecidable_rules_total",
			Help: "Violated rules the unwind decider would not size a reduction for — each one is a " +
				"breach a human has to resolve. Counted separately from proposals because a proposal " +
				"can be actionable overall while still leaving individual rules unsolved.",
		}),
	}
	for _, opt := range opts {
		opt(w)
	}
	if w.logger == nil {
		w.logger = slog.Default()
	}
	reg.MustRegister(w.decided, w.breachAge, w.undecidable)
	return w
}

// Handle decodes a ComplianceBreach and records what would clear it. It is a
// bus.EventHandler.
//
// EVERY RETURN IS NIL — every delivery is ACKED, deliberately.
//
// This handler observes; it does not own the breach. A nack would redeliver
// indefinitely, and the redelivery would decide the same undecidable breach the
// same way forever while occupying the consumer that real breaches arrive on. The
// breach FACT itself is durable and is not lost by this handler declining to
// re-read it — the compliance service owns that record, and an operator alerted
// by the counters can read the proposal off the FACT at any time.
func (w *UnwindWatch) Handle(_ context.Context, env *envelopepb.Envelope, payload []byte) error {
	// A BREACH BELONGS TO ONE TENANT'S BOOK (#223). The proposal names a
	// portfolio and is read by whoever operates this deployment, so deciding
	// another tenant's breach here would put their portfolio and their sector
	// weights in front of the wrong operator. Inert while this engine serves
	// __system__, which is what makes adopting it safe today and correct the day
	// per-tenant compute lands (#97).
	if err := bus.RequireTenantScope(env.GetTenantId(), w.tenant); err != nil {
		return err
	}

	var breach compliancepb.ComplianceBreach
	if err := proto.Unmarshal(payload, &breach); err != nil {
		// A permanent defect in these bytes. Redelivering them changes nothing.
		w.logger.Error("unwind: undecodable compliance breach — no proposal derived for it",
			"event_id", env.GetEventId(), "err", err)
		return nil
	}

	now := w.now().UTC()
	p := unwind.Decide(&breach, now)
	w.decided.WithLabelValues(boolLabel(p.Actionable())).Inc()
	w.undecidable.Add(float64(len(p.Undecidable)))

	// HOW OLD IS THE BREACH THIS ANSWERS?
	//
	// ComplianceBreach.detected_at is documented Required and the compliance
	// monitor sets it — and until now NOTHING in the module read it back. So a
	// proposal for a breach detected one second ago and one for a breach detected
	// an hour ago were the same log line.
	//
	// The difference matters precisely because the proposal is a REDUCTION SIZED
	// AGAINST THE BOOK AS IT WAS. A backlogged consumer, a redelivery after a
	// restart, or a replayed stream all produce a proposal whose arithmetic was
	// correct when the breach was observed and may be wrong now — and an operator
	// reading "shed 55% of TECH" needs to know which of those they are looking at
	// before acting on it. DecidedAt alone cannot say: it is the time this handler
	// ran, which is exactly the clock that hides the lag.
	var ageSeconds float64
	detectedAt := breach.GetDetectedAt().AsTime().UTC()
	if breach.GetDetectedAt() != nil && !detectedAt.IsZero() {
		ageSeconds = now.Sub(detectedAt).Seconds()
		w.breachAge.Observe(ageSeconds)
	}

	// LOGGED AT WARN EVEN WHEN NOTHING IS ACTIONABLE. "This portfolio is in breach
	// and the platform cannot size a way out of it" is the more alarming of the two
	// outcomes, and logging it below the level operators watch would bury exactly
	// the case that needs a person.
	w.logger.Warn("unwind: breach decided — PROPOSAL ONLY, nothing has been executed",
		"portfolio_id", p.PortfolioID,
		"mandate_id", p.MandateID,
		"mandate_version", p.MandateVersion,
		"actionable", p.Actionable(),
		"detected_at", detectedAt, "breach_age_seconds", ageSeconds,
		"reductions", describeReductions(p.Reductions),
		"undecidable", describeUndecidable(p.Undecidable),
		"decided_at", p.DecidedAt,
	)
	return nil
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// describeReductions renders the proposal for the operator reading the log. The
// FRACTION AND ITS SCOPE TRAVEL TOGETHER: "shed 0.25" is meaningless without
// knowing whether that is a quarter of one bucket or a quarter of the book.
func describeReductions(rs []unwind.Reduction) string {
	if len(rs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		scope := r.Bucket
		if scope == "" {
			scope = "whole book"
		}
		parts = append(parts, r.RuleID+": shed "+r.Fraction.FloatString(6)+" of "+scope+
			" ("+r.Dimension+") — "+r.Why)
	}
	return strings.Join(parts, "; ")
}

// describeUndecidable names each rule nothing could be derived for, WITH its
// reason. A count alone would tell an operator that something is unresolved
// without telling them what to look at.
func describeUndecidable(us []unwind.Undecidable) string {
	if len(us) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(us))
	for _, u := range us {
		parts = append(parts, u.RuleID+": "+u.Reason)
	}
	return strings.Join(parts, "; ")
}
