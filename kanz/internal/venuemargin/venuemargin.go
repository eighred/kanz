// Package venuemargin makes a venue account's margin state a FIRST-CLASS READ
// rather than a derived guess (#408, control 1).
//
// # The failure this is bought against
//
// #408's ruling states it in one line: margin turns "we lost money on a trade"
// into "THE EXCHANGE SOLD OUR COLLATERAL WHILE WE WERE READING STALE BOOKS".
// The second is not a larger version of the first. It is loss caused by our own
// ignorance of a position's state, executed by a counterparty on their schedule,
// and it cannot be traded out of.
//
// So every number here is the EXCHANGE'S. internal/collateral can compute what a
// margin requirement should be under an agreement we hold, and that computation
// is sound and unconsumed and STILL not the right input for this control: the
// exchange margins on its own maintenance tiers, its own mark price and its own
// index. Reconstructing its arithmetic from our own positions would produce
// precisely the stale book the control exists to replace. Nothing in this
// package computes a margin figure; it carries, dates and refuses them.
//
// # Three states, and only one of them is a number
//
//	REPORTED  the venue answered, recently enough to act on
//	UNKNOWN   the venue did not answer, has never answered, or answered too
//	          long ago — all one answer, deliberately
//	nothing   this adapter has no margin source at all — the posture gauge
//
// The first two are the View's business, the third is Announce's. They are kept
// apart because "nothing configured" and "checked, and fine" must never look the
// same, and on a margin feed they would: a portfolio with no leveraged position
// and a portfolio whose margin nobody has ever read both show no margin call.
//
// # Why UNKNOWN and not zero, restated where it costs the most
//
// A measure that resolves inputs through a provider skips what does not resolve
// and returns its accumulator anyway; when nothing resolved the accumulator is
// zero, and that zero crosses the wire as an ACTIVE CLAIM. This estate has fixed
// that defect five times in two days — twice in FRTB, in the liquidity measures,
// in the alpha grading loop, in the bar readers. Here the claim would be "this
// account needs no collateral", and the cost of it being wrong is the fund's
// collateral. So the quantities are POINTERS on the wire and on the seam, absent
// means absent, and every lookup returns an explicit ok.
package venuemargin

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/execution"
)

// Subject is where a venue adapter announces the exchange's margin state for the
// account its credential spends from.
//
// ON THE ACCOUNTING SPINE, NOT A NEW ONE. The venue adapters already publish
// accounting.balance.reconciled — venue truth about what an account holds — and
// this is venue truth about what the same account must post to keep holding it.
// One stream (ACCOUNTING, accounting.>) already carries, archives and tenants
// that traffic, and a second plane for one subject would be a stream, a Kafka
// topic, an archiver subscription and a DR restore set that all exist to be
// forgotten separately.
//
// DECLARED ONCE. Unlike balancerecon.Subject and riskview.Subject — which are
// duplicated because Go's internal-package rule forbids one service importing
// another's internals, and which need an arch guard to keep the copies equal —
// this package is under the module's own internal/ and every publisher and
// consumer imports this constant. There is nothing to drift.
const Subject = "accounting.margin.observed"

// DefaultMaxAge bounds how old an observation may be and still be read as this
// account's margin state.
//
// FAR TIGHTER THAN THE CASH AND RISK BOUNDS, AND THE DIFFERENCE IS THE POINT.
// balancerecon and riskview both allow 15 minutes, which suits state that moves
// when a trade settles or an engine recomputes. Margin does not move on our
// schedule: it moves with the mark, and the whole scenario #408 guards against
// is a fast move. A margin ratio from ten minutes ago during one is not a margin
// ratio — it is the number that was true before the move that is about to
// liquidate the account, and reading it as current is the "stale book" in the
// sentence the issue is built on.
//
// Aging out yields UNKNOWN, which every control in the #408 set fails closed on.
// That is a real cost: a poll that misses two ticks refuses trading on the
// account. It is the correct cost — refusing an order is recoverable, and being
// liquidated against a number we could not date is not.
const DefaultMaxAge = 2 * time.Minute

// DefaultInterval is how often a reporter asks the venue.
//
// It must divide DefaultMaxAge with room for a retry: one missed poll must not
// age the account out, or an ordinary REST hiccup becomes a trading halt, and an
// operator who has watched a control refuse for no reason starts routing around
// it. Two polls of headroom is the same shape as the reconciler's own cadence.
const DefaultInterval = 30 * time.Second

// The closed vocabulary of reasons a quantity is missing from an observation.
//
// CONSTANTS AND NOT FREE TEXT, because these become domain.v1.InputExclusion
// reasons and a metric label, and a label must not be whatever a future edit
// writes. Same stance as compute.SkipNoTerms and its siblings.
const (
	// SkipNoMaintenanceMargin: the venue reported no maintenance margin for the
	// account — an empty field, a field this API version does not carry, or a
	// value that would not parse. Never rendered as zero.
	SkipNoMaintenanceMargin = "no_maintenance_margin"
	// SkipNoMarginRatio: the venue reported no margin ratio for the account.
	SkipNoMarginRatio = "no_margin_ratio"
	// SkipNoLiquidationPrice: the venue reported an open position but no
	// liquidation price for it. The position is named in the exclusion so an
	// operator can see WHICH position's liquidation distance is unknowable —
	// dropping it silently would hide exactly the one nobody is watching.
	SkipNoLiquidationPrice = "no_liquidation_price"
	// SkipNotRepresentable: the venue's own figure will not survive conversion to
	// a common.v1.Decimal (#94). Publishing a wrapped number would not report a
	// smaller margin requirement, it would report a DIFFERENT one, so the
	// quantity is dropped and excluded rather than mangled.
	SkipNotRepresentable = "not_representable"
)

// MetricName is the gauge the adapters export. One spelling, named as a constant
// so the observability guard and any dashboard refer to the same series.
const MetricName = "kanz_venue_margin_source_configured"

// NewGauge builds the per-venue posture gauge.
//
// A GAUGE AND NOT A LOG LINE, for the reason balancerecon states one degraded
// posture over: the question is asked of a dashboard during an incident. "No
// margin call has fired" and "nothing has ever looked" are indistinguishable
// from the event stream, because a healthy account and an unwatched one both
// produce silence.
func NewGauge(venue string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Name: MetricName,
		Help: "1 if this venue adapter can read the exchange's own margin state for its account, 0 if not. " +
			"0 means " + Subject + " is silent because NOTHING IS OBSERVING MARGIN on this venue, " +
			"not because the account is unlevered or safe.",
		ConstLabels: prometheus.Labels{"venue": venue},
	})
}

// Announce records the posture and returns the source unchanged, so a
// composition root wires it in the same expression that fills the field:
//
//	Margin: venuemargin.Announce(g, logger, "okx", src),
//
// Returning the seam rather than taking a pointer is what stops the gauge and
// the wiring drifting apart — there is no way to set one without the other,
// because the value the workers receive IS this function's return. Copied in
// shape from balancerecon.Announce on purpose: an operator reading two venue
// dashboards should not have to learn two idioms for the same question.
//
// nil is not an error and does not refuse startup. An adapter that cannot see
// margin can still route and fill unleveraged orders, and taking a pod down over
// a reporting seam would be a self-inflicted trading outage. What it must never
// be is quiet: the warning below names the venue and the metric, so the state is
// reachable from a log search as well as from a dashboard.
func Announce(g prometheus.Gauge, logger *slog.Logger, venue string, src execution.VenueMarginSource) execution.VenueMarginSource {
	if src != nil {
		if g != nil {
			g.Set(1)
		}
		return src
	}
	if g != nil {
		g.Set(0)
	}
	if logger != nil {
		logger.Warn("NO MARGIN SOURCE ON THIS VENUE — nothing reads the exchange's own maintenance margin, "+
			"margin ratio or liquidation price for this account, so every #408 margin control is UNKNOWN "+
			"here and fails closed. Zero events on "+Subject+" means NOTHING IS OBSERVING, not that the "+
			"account is safe.",
			"venue", venue,
			"metric", MetricName,
			"subject", Subject,
		)
	}
	return nil
}
