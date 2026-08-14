package app

import (
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

// WHICH CALIBRATIONS ARE ACTUALLY RUNNING (#113).
//
// This platform has three calibrators, all built, all tested, all carrying the
// same Refresh(ctx, key, asOf) seam the generic scheduler drives:
//
//	internal/risk/pricing/curve      rates
//	internal/risk/pricing/volsurface implied vol (SVI)
//	internal/risk/pricing/credit     CDS / hazard rates
//
// ONE of them is scheduled. The composition root builds a curve calibrator,
// starts it, and logs "calibration scheduler enabled" — and says nothing at all
// about the other two. An operator reading that line concludes calibration is
// running. Two thirds of it is not, and until now nothing anywhere said so.
//
// That is this repository's own rule broken: "Nothing configured" and "checked,
// and fine" must never look the same. A missing vol surface does not announce
// itself — it shows up as an option position priced off a surface nobody
// refreshed, which looks exactly like one priced off a fresh one.
//
// WHAT THIS DOES NOT DO IS START THEM. Vol and credit have no live quote source
// in this estate (#203 for the provider, #345 for the vol quote-to-terms join),
// so scheduling them would produce a job that fails every tick — noise that
// teaches operators to ignore the calibration log, which is worse than the
// silence it replaced. The posture is stated instead: unscheduled, with the
// reason, once at startup and continuously as a gauge.

// CalibrationKinds is every calibration this platform implements. Listed
// explicitly rather than derived, so ADDING a calibrator and forgetting to state
// its posture is a visible omission in this slice rather than an invisible one at
// the composition root.
var CalibrationKinds = []string{"curve", "volsurface", "credit"}

// CalibrationPosture records, for every calibration this platform implements,
// whether it is scheduled — and says why not, once, for the ones that are not.
//
// THE GAUGE IS THE DURABLE HALF. A startup log line is gone at the next rollout;
// kanz_risk_calibration_scheduled is queryable at 3am when a desk asks why a
// surface is stale. It carries a series for EVERY kind, including the zeroes:
// a metric that appeared only for running calibrations would make an absent one
// indistinguishable from a service that never had it.
func CalibrationPosture(reg prometheus.Registerer, logger *slog.Logger, scheduled map[string]string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kanz_risk_calibration_scheduled",
		Help: "1 when this calibration is on the scheduler, 0 when it is implemented but not " +
			"running. A zero is a posture, not an error — but a surface nobody refreshes prices " +
			"positions exactly like a fresh one, so it must be visible (#113).",
	}, []string{"kind"})
	reg.MustRegister(g)

	var on, off []string
	for _, kind := range CalibrationKinds {
		if _, ok := scheduled[kind]; ok {
			g.WithLabelValues(kind).Set(1)
			on = append(on, kind)
			continue
		}
		g.WithLabelValues(kind).Set(0)
		off = append(off, kind)
	}
	sort.Strings(on)
	sort.Strings(off)

	if len(off) == 0 {
		logger.Info("risk calibration: every implemented calibration is scheduled", "kinds", on)
		return
	}
	// WARN, not Info. "Two of the three calibrations this service implements are
	// not running" is the sentence an operator needs to have read before they
	// trust a vol number, and Info is where it would be filtered out.
	logger.Warn("risk calibration: NOT every implemented calibration is running — the unscheduled "+
		"ones price nothing new, and a stale surface looks exactly like a fresh one",
		"scheduled", on, "not_scheduled", off,
		"why", "no live quote source is configured for them in this estate (#203 provider, #345 vol terms join)")
}
