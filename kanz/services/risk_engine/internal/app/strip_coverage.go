package app

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/eighred/kanz/internal/risk/pricing/curve"
)

// WHAT THE CURVE WAS ACTUALLY BUILT FROM (#908).
//
// The sibling file states which calibrations are RUNNING. This one states, for
// the one that runs, whether its input was whole.
//
// A rate curve is calibrated from a strip of instruments the deployment names
// in RISK_ENGINE_CALIBRATION_RATES. The source skips any instrument it has no
// usable price for, and the calibrator refuses only an EMPTY set — so a
// nine-point strip that lost its four longest tenors calibrated, published and
// served a five-pillar curve that is indistinguishable, in the store and on
// every read, from one whose strip is five points long and complete. Past the
// last surviving pillar the curve extrapolates FLAT, so every discount factor
// and every DV01 beyond that tenor was derived from a curve nobody could tell
// was short. A typo in the reference spec, or one instrument the market-data
// spine stops quoting, was enough — permanently, from the first refresh, with
// no error, no warning and no metric.
//
// THE GAUGE IS THE DURABLE HALF, for the reason CalibrationPosture states: a
// log line is gone at the next rollout, and the question "how much of the strip
// is this desk's discount curve actually built from" is asked at 3am. The log
// is the half that carries the instrument IDS, which is the part an operator
// acts on and which must not become metric labels.
//
// IT REPORTS EVERY REFRESH, INCLUDING THE HEALTHY ONES. A signal that appears
// only when something is wrong makes "fine" and "not reporting" the same
// series, which is the defect this exists to end.

// CalibrationCoverage records, per currency, how much of the configured
// calibration strip reached the last curve calibration. Safe for concurrent
// use: the intraday and nightly jobs for one currency are separate goroutines
// driving the same observer.
type CalibrationCoverage struct {
	configured *prometheus.GaugeVec
	quoted     *prometheus.GaugeVec
	missing    *prometheus.GaugeVec
	logger     *slog.Logger

	mu   sync.Mutex
	last map[string]string // currency → the last state that was logged
}

// missingReasons is the closed vocabulary curve reports, listed here so every
// series exists from the first observation.
//
// A GaugeVec exports NO series for a label value never set, so a reason that
// has not yet occurred would be an EMPTY VECTOR rather than a zero — and a
// query for "instruments missing because their venue quotes garbage" would come
// back empty both when none are and when nothing reports. Every reason is set
// on every observation for that one reason; it is the same seeding the OMS
// attribution counter needs and for the same failure.
var missingReasons = []string{curve.MissingNoQuote, curve.MissingUnusableMid}

// NewCalibrationCoverage registers the strip-coverage gauges and returns the
// observer to hand to curve.Calibrator.OnCoverage.
func NewCalibrationCoverage(reg prometheus.Registerer, logger *slog.Logger) *CalibrationCoverage {
	c := &CalibrationCoverage{
		configured: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_risk_calibration_strip_configured",
			Help: "How many calibration instruments this pod's reference spec names for the " +
				"currency — the DENOMINATOR, read off configuration rather than off the feed (#908).",
		}, []string{"currency"}),
		quoted: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_risk_calibration_strip_quoted",
			Help: "How many of them had a usable price and reached the last calibration — the " +
				"number of pillars the published curve carries. Below _configured means the " +
				"curve is SHORT: it extrapolates flat past its last surviving pillar and is " +
				"otherwise indistinguishable from a complete one (#908).",
		}, []string{"currency"}),
		missing: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_risk_calibration_strip_missing",
			Help: "Configured calibration instruments that did NOT reach the last calibration, " +
				"by reason. no_quote is a question for the reference spec and the market-data " +
				"spine (is the id right, is anything publishing it); unusable_mid is a question " +
				"for the venue quoting it (#908).",
		}, []string{"currency", "reason"}),
		logger: logger,
		last:   map[string]string{},
	}
	reg.MustRegister(c.configured, c.quoted, c.missing)
	return c
}

// Observe records one refresh's coverage. It is the curve.Calibrator.OnCoverage
// shape, and it is called whether or not that refresh went on to calibrate.
func (c *CalibrationCoverage) Observe(currency string, cov curve.StripCoverage) {
	if !cov.Reported() {
		// UNKNOWN, and it must not be written as a zero. A source that declares no
		// strip cannot be short of one, and exporting Configured = 0 here would
		// publish an active claim ("this curve needs nothing") for what is really a
		// wiring defect: every currency the scheduler runs a job for came from the
		// parsed reference spec, so it has at least one instrument by construction.
		c.logOnce(currency, "unreported", func() {
			c.logger.Error("curve calibration reported no configured strip — this pod cannot say "+
				"what its discount curve was built from, and every valuation off it inherits that. "+
				"The quote source is not the one the composition root parses",
				"currency", currency)
		})
		return
	}

	c.configured.WithLabelValues(currency).Set(float64(cov.Configured))
	c.quoted.WithLabelValues(currency).Set(float64(cov.Quoted))
	byReason := map[string]int{}
	for _, m := range cov.Missing {
		byReason[m.Reason]++
	}
	for _, reason := range missingReasons {
		c.missing.WithLabelValues(currency, reason).Set(float64(byReason[reason]))
	}
	// A reason the vocabulary does not list is still exported rather than
	// dropped: a new one added to curve and not added here must show up as a
	// series, not vanish.
	for reason, n := range byReason {
		if !known(reason) {
			c.missing.WithLabelValues(currency, reason).Set(float64(n))
		}
	}

	if cov.Complete() {
		c.logOnce(currency, "complete", func() {
			c.logger.Info("curve calibration strip is complete",
				"currency", currency, "quoted", cov.Quoted, "configured", cov.Configured)
		})
		return
	}
	c.logOnce(currency, shortState(cov), func() {
		// WARN, not Info. "This currency's discount curve is built from six of the
		// nine instruments this deployment says it needs" is the sentence an
		// operator must have read before trusting a DV01, and Info is where it
		// would be filtered out.
		//
		// LOGGED ON CHANGE, NOT EVERY REFRESH. The intraday cadence is minutes and
		// a dead instrument stays dead, so an unconditional line would repeat
		// forever and teach its readers to skip the calibration log — the failure
		// mode alerts/README.md records for absent(). The gauges carry the standing
		// state; this carries the transition and the identities.
		c.logger.Warn("curve calibration strip is SHORT — this currency's discount curve is "+
			"calibrated from fewer instruments than the reference spec names, it extrapolates "+
			"flat past its last surviving pillar, and it is otherwise indistinguishable from a "+
			"complete curve. Every valuation and DV01 priced off it inherits that",
			"currency", currency, "quoted", cov.Quoted, "configured", cov.Configured,
			"missing", missingIDs(cov))
	})
}

// logOnce runs emit only when state differs from the last state logged for this
// currency, and records it. The dedupe key is the whole state, so a strip that
// loses a SECOND instrument logs again.
func (c *CalibrationCoverage) logOnce(currency, state string, emit func()) {
	c.mu.Lock()
	changed := c.last[currency] != state
	c.last[currency] = state
	c.mu.Unlock()
	if changed {
		emit()
	}
}

// shortState is the dedupe key for a short strip: the counts plus the exact set
// of missing instruments and reasons, sorted so map/iteration order cannot make
// an unchanged strip look like a new event.
func shortState(cov curve.StripCoverage) string {
	ids := make([]string, 0, len(cov.Missing))
	for _, m := range cov.Missing {
		ids = append(ids, m.InstrumentID+"="+m.Reason)
	}
	sort.Strings(ids)
	return fmt.Sprintf("short %d/%d %s", cov.Quoted, cov.Configured, strings.Join(ids, ","))
}

// missingIDs renders the missing instruments for the log: the ids an operator
// greps the deployment for, each with why it is absent. Sorted, and COMPLETE —
// the strip is bounded by this pod's configuration, so there is nothing to
// sample and the one that matters is never the one truncated away.
func missingIDs(cov curve.StripCoverage) []string {
	out := make([]string, 0, len(cov.Missing))
	for _, m := range cov.Missing {
		out = append(out, m.InstrumentID+" ("+m.Reason+")")
	}
	sort.Strings(out)
	return out
}

func known(reason string) bool {
	for _, r := range missingReasons {
		if r == reason {
			return true
		}
	}
	return false
}
