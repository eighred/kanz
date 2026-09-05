package orderview

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/execution"
)

// The two series a venue adapter exports about resolving an exchange execution
// report to an order it holds (#1047).
const (
	// MetricReadFailures is incremented for every failure this adapter's own
	// order view reports through NewSeam's onErr.
	MetricReadFailures = "kanz_venue_orderview_read_failures_total"
	// MetricReportsDropped is incremented for every execution report an ingester
	// did not publish as a fill FACT because it could not resolve it to an order
	// this adapter holds.
	MetricReportsDropped = "kanz_venue_fill_reports_dropped_total"
)

// Observability is the alertable half of the order view (#1047).
//
// # What was missing, and what each counter answers
//
// A store failure inside a venue adapter used to leave exactly one trace: a log
// line, in one pod, saying "reconciliation is degraded". Nothing was countable,
// so nothing was alertable, and nobody could answer the question an operator
// actually asks afterwards — "how many fills did we lose during that outage?" —
// because a log line is not a threshold and a fill that was never published
// leaves no other evidence anywhere in the estate.
//
// The two counters answer two different questions and neither substitutes for
// the other. ReadFailures says the view was unreadable, which is what an alert
// fires on. ReportsDropped says how many executions went past while it was,
// which is what an incident is reconstructed from.
//
// # Why they are constructed here rather than in each composition root
//
// Both venue adapters export the same two series about the same seam, and a
// counter declared twice is a counter whose help text, label set and — the one
// that actually bites — SEEDING drift apart. The first thing that happens to a
// per-root copy is that one root's seeding loop is forgotten.
//
// # Seeding is not optional, so it is not a separate step
//
// NewObservability seeds every label combination before it returns, which makes
// "registered but exporting nothing" unrepresentable. An un-incremented
// CounterVec label exports NO series at all, so an alert written
// `... {reason="store_error"} == 0` evaluates against an empty vector on an
// adapter that has never dropped a report — which is every adapter, right up
// until the outage the alert exists for. This estate has shipped that exact
// silence more than once (#963, #973, #1036).
//
// The collectors are returned UNREGISTERED, and Collectors is what a root passes
// to MustRegister — unconditionally, beside its other package-level collectors,
// never inside a branch that depends on the store, the broker or the exchange
// being reachable.
type Observability struct {
	// ReadFailures is labelled by MIC. Every increment is one failed order-view
	// operation: the Lookup a fill report is enriched from, the Open the healing
	// watchdog enumerates, or one of the two writes that cannot return an error
	// at all.
	ReadFailures *prometheus.CounterVec
	// ReportsDropped is labelled by MIC and reason —
	// execution.DropUnknownOrder or execution.DropStoreError.
	//
	// THE unknown_order ARM IS NOT NOISE. It is the baseline store_error is read
	// against: a shared exchange account produces a steady trickle of reports
	// that are not ours, and an operator who cannot see that trickle cannot tell
	// an ordinary adapter from one that has just gone blind.
	ReportsDropped *prometheus.CounterVec
}

// NewObservability builds and SEEDS both counters for one adapter. venue is the
// const label (the adapter's own name); mic is the value every increment carries
// and the value seeded here, so a seeded series and an incremented one are the
// same series rather than two.
func NewObservability(venue, mic string) Observability {
	o := Observability{
		ReadFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricReadFailures,
			Help: "Failures of this adapter's own order view, by MIC. Non-zero means the adapter " +
				"cannot resolve execution reports to the orders it holds: fills are being dropped " +
				"from the FACT stream, and the healing watchdog is blind to what is open at the venue.",
			ConstLabels: prometheus.Labels{"venue": venue},
		}, []string{"mic"}),
		ReportsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricReportsDropped,
			Help: "Venue execution reports this adapter did not publish as a fill FACT because it " +
				"could not resolve them to an order it holds, by MIC and reason. store_error means " +
				"an execution this adapter probably owns was lost from the fill stream — no position " +
				"booked, no cash journalled; unknown_order is the routine skip of a report belonging " +
				"to another account or replica.",
			ConstLabels: prometheus.Labels{"venue": venue},
		}, []string{"mic", "reason"}),
	}
	o.ReadFailures.WithLabelValues(mic)
	for _, reason := range []string{execution.DropUnknownOrder, execution.DropStoreError} {
		o.ReportsDropped.WithLabelValues(mic, reason)
	}
	return o
}

// Collectors is what the composition root hands MustRegister.
func (o Observability) Collectors() []prometheus.Collector {
	return []prometheus.Collector{o.ReadFailures, o.ReportsDropped}
}

// NewObservedSeam is the Seam a venue adapter actually wants: the store, wrapped,
// with every failure it reports counted AND logged (#1047).
//
// # Why this exists rather than four lines in each composition root
//
// It WAS four lines in each composition root, and both of them wrote the same
// wrong sentence. onErr logged "order view read failed — reconciliation is
// degraded", which names the healing watchdog going blind on a view it cannot
// enumerate. True, and the smaller half. The larger half is on the fill path: an
// execution report the ingester cannot resolve is a trade with NO
// order.order.filled FACT, so the position book has not booked it and the
// accounting ledger has not journalled its cash, while risk, compliance, margin
// and NAV all read a book missing a real trade. A message that names one
// consequence sends the operator to the wrong system.
//
// Everything in a composition root is unreachable by any test — the estate has
// twice shipped a crashing root with a green suite (#643) — so the wiring that
// decides whether an outage is visible does not belong there. Here it is one
// call, and the behaviour it wires is testable.
//
// # The counter and the log are both required
//
// The counter is the alertable half: a log line is not a threshold, and after
// the fact nobody can answer "was the view readable during that window?" from
// one. The ERROR is what an operator reads when the alert fires. Neither
// replaces the other, which is why this takes both and nils neither.
//
// o must be REGISTERED by the caller — unconditionally, beside its other
// package-level collectors — and it is a parameter rather than built here so
// that registration cannot end up downstream of a store that failed to open.
func NewObservedSeam(store Store, o Observability, mic string, logger *slog.Logger) *Seam {
	if logger == nil {
		logger = slog.Default()
	}
	return NewSeam(store, func(err error) {
		if o.ReadFailures != nil {
			o.ReadFailures.WithLabelValues(mic).Inc()
		}
		logger.Error("THE ADAPTER'S OWN ORDER VIEW COULD NOT BE READ — execution reports cannot be "+
			"resolved to the orders this adapter holds, so fills are being DROPPED from the FACT "+
			"stream (no position booked, no cash journalled) and the healing watchdog is blind to "+
			"what is open at the venue. The OMS sweep can adopt a missed execution only while its "+
			"order is still working",
			"mic", mic, "err", err)
	})
}
