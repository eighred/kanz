package app

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
)

// WHICH CALIBRATIONS ARE ACTUALLY RUNNING (#113, corrected by #912).
//
// This platform has three calibrators, all built, all tested, all carrying the
// same Refresh(ctx, key, asOf) seam the generic scheduler drives:
//
//	internal/risk/pricing/curve      rates
//	internal/risk/pricing/volsurface implied vol (SVI)
//	internal/risk/pricing/credit     CDS / hazard rates
//
// THE CODE PATH CAN SCHEDULE ONE OF THEM. EVERY DEPLOYMENT IN THIS REPOSITORY
// SCHEDULES NONE. The composition root builds a curve calibrator and starts it
// only when RISK_ENGINE_CALIBRATION_INTERVAL and RISK_ENGINE_CALIBRATION_RATES
// are BOTH set, and no manifest, overlay, ConfigMap, compose file or CI job in
// this repository sets either — so the gauge below reads three zeroes in every
// running pod, not one and two.
//
// THIS COMMENT USED TO SAY "ONE of them is scheduled … Two thirds of it is not."
// That was true of the code path and false of the estate, and it under-sized the
// gap by a third for anyone reading it to decide what this platform prices. It is
// the failure this repository names elsewhere — a comment justifying a trade-off
// is dated evidence, and its premise has to be re-verified rather than relied on.
// #912 is the issue that measured it;
// test/arch/calibration_alert_follows_its_producer_test.go is what keeps the
// claim true, because it DERIVES the deployment side of this sentence rather
// than trusting it.
//
// THE CURVE'S ABSENCE HAS A DIFFERENT CAUSE FROM THE OTHER TWO, which is why the
// reasons below are per-kind rather than one sentence covering all three. Vol and
// credit have no live quote source (#203 for the provider, #345 for the vol
// quote-to-terms join). The curve calibrator's seam is complete and its quote
// source is written — what it lacks is an estate that publishes rate instruments
// at all: the whole market spine is two crypto spot pairs
// (MARKET_INGEST_INSTRUMENTS), so no deposit, rate future or par swap is quoted
// anywhere and there is no defensible value to put in
// RISK_ENGINE_CALIBRATION_RATES (#912; #104 for the vendor Source bindings and
// #105 for the licensed rate data, both blocked-external). Attributing all three
// to the vol/credit quote-source gap — which the single shared "why" on this log
// line used to do — sends an operator to the wrong issue.
//
// WHAT THIS DOES NOT DO IS START THEM, and for the curve that argument is now the
// stronger of the two rather than the weaker. Scheduling a calibration whose
// strip nothing quotes produces a job that fails every tick — noise that teaches
// operators to ignore the calibration log, which is worse than the silence it
// replaced — and in this service it does more than that: the same two variables
// gate registration of the seven FI and structured measures, so setting them
// without a rate producer would put DV01, Duration, Convexity, SpreadDuration,
// StructDuration, StructConvexity and StructWAL on the wire flagged unmeasured on
// EVERY response, and move kanz_risk_calibration_scheduled{kind="curve"} to 1 for
// a curve that can never calibrate. The posture is stated instead: unscheduled,
// with the reason that belongs to each kind, once at startup and continuously as
// a gauge.

// CalibrationKinds is every calibration this platform implements. Listed
// explicitly rather than derived, so ADDING a calibrator and forgetting to state
// its posture is a visible omission in this slice rather than an invisible one at
// the composition root.
var CalibrationKinds = []string{"curve", "volsurface", "credit"}

// calibrationUnscheduledReason is why each kind is idle when it is idle, keyed by
// the names CalibrationKinds carries.
//
// PER-KIND, BECAUSE THE THREE ARE BLOCKED ON DIFFERENT THINGS. The single shared
// sentence this replaced named only the vol/credit quote-source gap, so an
// operator asking why there is no discount curve was pointed at #203 and #345,
// neither of which is the curve's blocker. A reason nobody can act on costs the
// same as no reason at all.
//
// EVERY KIND MUST HAVE ONE. CalibrationPosture will not invent a reason for a
// kind missing from this map — it says so at WARN in place of the reason, so a
// fourth calibrator added without an explanation is visible in the log rather
// than silently unexplained, and
// TestEveryCalibrationKindHasAnUnscheduledReason derives the check from
// CalibrationKinds so it is caught before it ships.
var calibrationUnscheduledReason = map[string]string{
	"curve": "the seam and its quote source are complete; nothing in this estate QUOTES a rate " +
		"instrument, so RISK_ENGINE_CALIBRATION_RATES has no defensible value and no manifest " +
		"sets it (#912; #104 vendor Source bindings, #105 licensed rate data)",
	"volsurface": "no live vol quote source is configured in this estate (#203 provider, " +
		"#345 quote-to-terms join)",
	"credit": "no live credit quote source is configured in this estate, and the credit " +
		"Refresh/QuoteSource seam is still missing (#203 provider, #113 seam)",
}

// unscheduledReason answers for any kind, including one nobody has explained.
//
// A MISSING REASON IS ITSELF REPORTED rather than dropped. Returning "" and
// omitting the attribute would make "this calibration is idle for a stated
// reason" and "nobody wrote down why this calibration is idle" the same log
// line, which is the distinction this whole file exists to keep. A separate
// function rather than an inline lookup so the unexplained case is reachable
// from a test without mutating CalibrationKinds — a package-level var a test
// writes to is a data race waiting for the first parallel test in this package.
func unscheduledReason(kind string) string {
	if reason, ok := calibrationUnscheduledReason[kind]; ok {
		return reason
	}
	return "NO REASON RECORDED — this kind is in CalibrationKinds and not in " +
		"calibrationUnscheduledReason, so nobody stated what blocks it"
}

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

	var on, off, why []string
	for _, kind := range CalibrationKinds {
		if _, ok := scheduled[kind]; ok {
			g.WithLabelValues(kind).Set(1)
			on = append(on, kind)
			continue
		}
		g.WithLabelValues(kind).Set(0)
		off = append(off, kind)
		why = append(why, fmt.Sprintf("%s: %s", kind, unscheduledReason(kind)))
	}
	sort.Strings(on)
	sort.Strings(off)
	sort.Strings(why)

	if len(off) == 0 {
		logger.Info("risk calibration: every implemented calibration is scheduled", "kinds", on)
		return
	}
	// WARN, not Info. "Every one of the three calibrations this service
	// implements is unscheduled" is the sentence an operator needs to have read
	// before they trust a vol number or ask where DV01 went, and Info is where it
	// would be filtered out.
	logger.Warn("risk calibration: NOT every implemented calibration is running — the unscheduled "+
		"ones price nothing new, and a stale surface looks exactly like a fresh one",
		"scheduled", on, "not_scheduled", off, "why", why)
}
