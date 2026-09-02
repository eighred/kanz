package custody

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/services/accounting/internal/recon"
)

// Metrics is the observability for the custody reconciliation control.
//
// # Why the series are pre-seeded
//
// A CounterVec exports nothing for a label combination it has never incremented,
// so a rule over `...{outcome="failed"} > 0` is a rule over an EMPTY VECTOR until
// the first failure — and a rule over an empty vector never fires. The estate
// then reads as alerted while the alert is structurally incapable of firing,
// which is #62's ten deleted data-quality rules exactly. Every outcome and every
// break kind is therefore registered at zero when the metrics are built, from
// Outcomes() and recon's own kind set rather than from a list retyped here.
//
// # Why the staleness gauge is a timestamp and not a duration
//
// kanz_accounting_reconciliation_last_success_timestamp_seconds is set to the
// completion instant, and the ALERT does the subtraction (`time() - metric`). A
// gauge holding a duration is computed at scrape time by the process that owns
// it, so a process that has hung — the single most important thing a staleness
// alert must catch — publishes a duration frozen at whatever it last managed to
// compute, and the alert reads it as healthy forever. A timestamp cannot lie that
// way: it stops advancing, and `time()` keeps moving.
type Metrics struct {
	runs         *prometheus.CounterVec
	lastSuccess  *prometheus.GaugeVec
	openBreaks   *prometheus.GaugeVec
	oldestBreak  *prometheus.GaugeVec
	publishFails *prometheus.CounterVec
}

// NewMetrics registers the custody reconciliation series on reg. A nil reg
// returns nil, and every method below is nil-safe — the reconciler is usable in a
// test without a registry, and no caller has to guard each call site.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_accounting_reconciliation_runs_total",
			Help: "Custody reconciliation runs, by custodian and outcome. outcome=no_statement is a run that came due while the custodian had sent nothing (#962).",
		}, []string{"custodian", "outcome"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_accounting_reconciliation_last_success_timestamp_seconds",
			Help: "Unix time of the last reconciliation run that completed AND was published, per portfolio and custodian. The alert subtracts it from time(); it is not a duration, so a hung process cannot report a frozen-but-healthy age.",
		}, []string{"portfolio", "custodian"}),
		openBreaks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_accounting_breaks_open",
			Help: "Outstanding reconciliation breaks by custodian and kind. EXPLAINED counts as outstanding: an explanation is a claim about the future, not evidence the difference is gone.",
		}, []string{"custodian", "kind"}),
		oldestBreak: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_accounting_break_oldest_age_seconds",
			Help: "Age of the oldest outstanding break per custodian, measured from when it was FIRST seen and never reset by a redetection.",
		}, []string{"custodian"}),
		publishFails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_accounting_reconciliation_publish_failures_total",
			Help: "Runs that completed locally and could not be published. The run happened; the estate did not hear it, so the staleness gauge deliberately does not advance.",
		}, []string{"custodian"}),
	}
	reg.MustRegister(m.runs, m.lastSuccess, m.openBreaks, m.oldestBreak, m.publishFails)
	return m
}

// SeedCustodian registers the zero series for one custodian.
//
// IT IS CALLED FROM THE COMPOSITION ROOT FOR EVERY CONFIGURED PAIR, before any
// run happens. Without it the first alert evaluation of a freshly deployed estate
// queries an empty vector for every rule, so a deployment whose reconciliation
// never starts at all is the one case the alerting cannot see — the failure mode
// is worst exactly where the metric is most absent.
func (m *Metrics) SeedCustodian(custodianID string) {
	if m == nil {
		return
	}
	for _, o := range Outcomes() {
		m.runs.WithLabelValues(custodianID, o.String()).Add(0)
	}
	for _, k := range recon.BreakKinds() {
		m.openBreaks.WithLabelValues(custodianID, k.String()).Set(0)
	}
	m.oldestBreak.WithLabelValues(custodianID).Set(0)
	m.publishFails.WithLabelValues(custodianID).Add(0)
}

// SeedPair registers the zero staleness series for a (portfolio, custodian).
//
// IT SEEDS TO ZERO, WHICH READS AS 1970 — an infinite age to the alert, and
// deliberately so. A pair that has never reconciled is a worse state than one
// whose last run is old, so it must alert rather than sit absent from the vector
// and be silently skipped by the rule.
func (m *Metrics) SeedPair(portfolioID, custodianID string) {
	if m == nil {
		return
	}
	m.lastSuccess.WithLabelValues(portfolioID, custodianID).Set(0)
	m.SeedCustodian(custodianID)
}

// observeRun counts a run and, for one that was published, advances the pair's
// staleness gauge.
//
// A FAILED RUN DOES NOT ADVANCE THE GAUGE. It happened, so it is counted; it did
// not establish that the book agrees with anything, so treating it as recent
// evidence of a working control would be the false green the whole issue is
// about. The same holds for NO_STATEMENT: the control ran, and it reconciled
// nothing.
func (m *Metrics) observeRun(run Run) {
	if m == nil {
		return
	}
	m.runs.WithLabelValues(run.Subject.CustodianID, run.Outcome.String()).Inc()
	switch run.Outcome {
	case OutcomeClean, OutcomeBreaks:
		m.lastSuccess.
			WithLabelValues(run.Subject.PortfolioID, run.Subject.CustodianID).
			Set(float64(run.CompletedAt.UTC().Unix()))
	}
}

// runFailed counts a run that completed locally and could not be published.
func (m *Metrics) runFailed(custodianID string) {
	if m == nil {
		return
	}
	m.publishFails.WithLabelValues(custodianID).Inc()
}

// observeBreaks re-states the outstanding-break gauges from the full stored set.
//
// IT SETS FROM A COMPLETE SNAPSHOT rather than incrementing per detection, which
// is what makes the gauge self-correcting: a resolved break disappears from the
// set and the gauge follows, with no delta bookkeeping that could drift from the
// store. Every seeded series is zeroed first, so a kind that USED to have breaks
// and now has none reads 0 rather than keeping its last non-zero value forever.
func (m *Metrics) observeBreaks(outstanding []Break, now time.Time) {
	if m == nil {
		return
	}
	counts := map[[2]string]int{}
	oldest := map[string]time.Duration{}
	custodians := map[string]struct{}{}

	for _, b := range outstanding {
		custodian := custodianOf(b.BreakID)
		custodians[custodian] = struct{}{}
		counts[[2]string{custodian, b.Kind.String()}]++
		if age := b.Age(now); age > oldest[custodian] {
			oldest[custodian] = age
		}
	}
	for custodian := range custodians {
		for _, k := range recon.BreakKinds() {
			m.openBreaks.WithLabelValues(custodian, k.String()).Set(float64(counts[[2]string{custodian, k.String()}]))
		}
		m.oldestBreak.WithLabelValues(custodian).Set(oldest[custodian].Seconds())
	}
}

// custodianOf extracts the custodian from a break id (portfolio|custodian|kind|key).
func custodianOf(breakID string) string {
	parts := splitN(breakID, '|', 4)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// splitN splits on sep into at most n parts. It exists rather than strings.SplitN
// only to keep the key parsing beside the id construction it mirrors.
func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
