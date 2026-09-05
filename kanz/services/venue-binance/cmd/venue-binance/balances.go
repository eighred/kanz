package main

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/venueadapter/balancerecon"
)

// balanceSeams is the balance-reconciliation half of this composition root: what
// the reconciler compares against, and what makes an asset it could NOT compare
// visible.
//
// A NAMED BUILDER THAT RETURNS WHAT IT BUILT, rather than twenty-seven lines in
// the middle of serve() (#643's shape, applied because #1063's wiring needed the
// room). Everything constructed inside serve is a local in package main and
// therefore unreachable by any test — and the defect this file exists to repair
// is precisely a seam nobody wired, on a path where a nil argument and a
// deliberate one look identical. Handing the pair back is what lets a test assert
// that the observer exists, is bound to a registered collector, and starts
// seeded.
type balanceSeams struct {
	// View is what Kanz believes this exchange account holds, folded from the
	// book of record's announcements (#450). It is also the bus handler for the
	// cash spine.
	View *balancerecon.View
	// Unknown is the alertable half (#1063): every asset a reconciliation pass
	// could not check, counted by reason and named in an ERROR log. Without it a
	// skipped asset and a cleanly reconciled one produce the same silence.
	//
	// A COUNTER AND A LOG, NOT A BREAK, and that is a decision rather than the
	// cheaper option.
	//
	// accounting.v1.BalanceReconciled carries expected, actual and delta. The
	// whole condition here is that EXPECTED DOES NOT EXIST, so publishing one per
	// unknown asset means inventing expected=0 — the exact fabrication #418
	// removed from both reconcilers. That is not a dashboard mistake: the FACT is
	// folded bitemporally, so the invented figure becomes durable book state, and
	// on a cold adapter it fires for every asset the exchange holds, which is the
	// break storm that teaches an operator to ignore this layer.
	//
	// A break also answers a different question. It means the two sides DISAGREE,
	// which sends an operator to the exchange's own history; this means one side
	// is MISSING, which sends them to the cash spine. Collapsing them loses the
	// only thing that tells those apart.
	//
	// #1073 IS THE NEIGHBOURING PRECEDENT AND IT POINTS THE SAME WAY: an empty
	// derived custody basis now fails the RUN rather than breaking every holding.
	// Breaking every holding is precisely what a break FACT per unknown asset
	// would be here. The run-level half of that rule is what this counter and its
	// alert supply — nothing on this path claims a pass SUCCEEDED, so the silence
	// was the claim, and this is what replaces it.
	Unknown *balancerecon.UnknownObserver
}

// newBalanceSeams builds both halves and registers their collectors on reg.
//
// REGISTRATION IS UNCONDITIONAL AND EARLY — before the exchange handshake, the
// broker dial, or any posture decision. A collector registered on one arm of an
// `if` exports no series at all on the other, so an alert over it is silent in
// exactly the deployment state it was written for; that has shipped twice on
// this estate (#973, #963) and was caught both times only by running the binary.
// Both counters here are seeded at zero for every reason as part of construction
// rather than by a loop the caller has to remember, because an un-incremented
// CounterVec label exports no series and "nothing was dropped" then reads
// identically to "nobody wired the metric" (#622).
func newBalanceSeams(reg prometheus.Registerer, logger *slog.Logger, venue, account string) balanceSeams {
	for _, reason := range []string{balancerecon.DropUndecodable, balancerecon.DropOutOfDomain} {
		balanceAnnouncementsDropped.WithLabelValues(reason)
	}
	unknown := balancerecon.NewUnknownCounter(venue)
	reg.MustRegister(balanceAnnouncementsDropped, unknown)

	// WHAT KANZ BELIEVES THIS EXCHANGE ACCOUNT HOLDS (#418), folded from the book
	// of record's announcements (#450).
	//
	// BROADCAST, NOT A WORK QUEUE: every adapter replica needs the whole picture
	// for its own account, and a consumer group would give each replica a subset —
	// so one pod would reconcile against a balance the other did not have. A
	// balance is replicated STATE, the same argument the OMS makes for its price
	// spine and mandate registry.
	view := balancerecon.NewView(account,
		balancerecon.WithViewLogger(logger),
		balancerecon.WithViewDropObserver(func(reason string) {
			balanceAnnouncementsDropped.WithLabelValues(reason).Inc()
		}),
		balancerecon.WithViewOnStale(func(age time.Duration) {
			logger.Warn("venue-"+venue+": expected balances are too old to reconcile against — "+
				"reconciliation is SKIPPING assets rather than reporting false breaks",
				"account", account, "age", age.String(), "subject", balancerecon.Subject)
		}),
	)
	return balanceSeams{
		View:    view,
		Unknown: balancerecon.NewUnknownObserver(unknown, logger, venue),
	}
}
