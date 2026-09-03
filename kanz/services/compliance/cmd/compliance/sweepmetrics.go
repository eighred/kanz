package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// THE PASSIVE-BREACH SWEEP COULD STOP AND NOTHING WOULD SAY SO (#983).
//
// reevaluateBooks re-runs every book the monitor holds on an interval, and that
// loop is not an optimisation: it is the ONLY thing that detects a breach with
// no FACT behind it. A financed book that falls 20% raises its gross leverage
// with no order placed anywhere, so nothing wakes the monitor and nothing else
// in the estate is watching. #787 built the cash and mark folds precisely so
// that breach becomes detectable after the trade as well as before it.
//
// # Why a log was not enough
//
// The only signal was an ERROR log, and only on a sweep that RAN AND FAILED:
//
//	case <-ticker.C:
//	    if err := mon.ReevaluateAll(ctx); err != nil && ctx.Err() == nil {
//	        logger.Error("...post-trade re-evaluation sweep did not complete...")
//	    }
//
// A stopped ticker, a goroutine that never started, and a deployment with no
// broker all produce total silence — and a log cannot express "this did not
// happen". The loop also deliberately logs and continues rather than
// terminating, because a monitor that stops is a fund nothing is watching at
// all, which means a persistently failing sweep never surfaces as a crash
// either. Pods up, probes green, control dead.
//
// # Three series, and why the third exists
//
// A last-success TIMESTAMP rather than a rate: a rate over a sweep that runs
// every COMPLIANCE_REEVALUATE_INTERVAL cannot separate "slow" from "stopped"
// without encoding the interval in the rule. A timestamp compares against
// time() and needs no such knowledge.
//
// A FAILURE COUNTER beside it, because a sweep that runs and errors every cycle
// is a different fault from one that is not running: the first is usually one
// bad book, the second is the loop. They route to different people.
//
// And THE INTERVAL ITSELF, exported as a gauge. Without it the alert has to
// hard-code a staleness bound, which silently becomes wrong the moment somebody
// sets COMPLIANCE_REEVALUATE_INTERVAL — a rule that is correct only for the
// default is a rule that stops being correct without anybody editing it. With
// it, the alert reads "more than three intervals since the last success" and
// adapts to whatever the deployment is configured for.
type sweepMetrics struct {
	lastSuccess prometheus.Gauge
	failures    prometheus.Counter
	interval    prometheus.Gauge

	// now is the clock the timestamp is read from, injectable for tests.
	now func() time.Time
}

// newSweepMetrics builds the sweep-liveness set for a configured interval.
func newSweepMetrics(interval time.Duration) *sweepMetrics {
	return &sweepMetrics{
		// THE ZERO VALUE IS "NEVER SUCCEEDED", AND IT IS LOAD-BEARING. A gauge
		// registered and never set reads 0, so time() - 0 is ~55 years and the
		// staleness rule fires — which is the correct answer for a process whose
		// sweep has not run once. Initialising it to time.Now() at startup would
		// mean a deployment whose loop never started looked healthy for the first
		// three intervals and then, only then, began to page; and a deployment
		// with no broker would look healthy for three intervals every restart.
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_compliance_reevaluate_last_success_timestamp_seconds",
			Help: "Unix time of the last post-trade re-evaluation sweep that completed without error. " +
				"0 means no sweep has ever succeeded in this process. The sweep is the only detector " +
				"of a breach with no FACT behind it — a financed book that falls in value (#787/#983).",
		}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_compliance_reevaluate_failures_total",
			Help: "Post-trade re-evaluation sweeps that ran and did not complete. Distinct from a sweep " +
				"that is not running at all, which the last-success timestamp reports: this one is " +
				"usually one book the monitor cannot evaluate, that one is the loop (#983).",
		}),
		// A CONFIGURATION VALUE ON A DASHBOARD, deliberately. It is what makes the
		// staleness rule interval-agnostic, and it also answers "how quickly would
		// this estate notice a passive breach" without reading a deployment's
		// environment.
		interval: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_compliance_reevaluate_interval_seconds",
			Help: "The configured COMPLIANCE_REEVALUATE_INTERVAL. Exported so the staleness alert can " +
				"be written in intervals rather than in seconds, and so a deployment that widens the " +
				"cadence widens the alert with it rather than silently outrunning it (#983).",
		}),
		now: time.Now,
	}
}

// register registers the sweep-liveness set and records the configured cadence.
//
// CALLED FROM run(), NEVER FROM runConsumers. A compliance with no
// COMPLIANCE_NATS_URL starts no sweep at all — which is exactly the state the
// staleness rule exists to page on — and registering inside the consumer path
// would leave that deployment exporting no series, over which the rule
// evaluates to nothing. This is the third time this estate has hit that wiring
// defect (#973's answer metrics, #963's cash-drag series), so it is asserted
// structurally by an arch guard rather than left to review.
func (m *sweepMetrics) register(r prometheus.Registerer, interval time.Duration) {
	r.MustRegister(m.lastSuccess, m.failures, m.interval)
	m.interval.Set(interval.Seconds())
}

// succeeded records a sweep that completed.
func (m *sweepMetrics) succeeded() {
	m.lastSuccess.Set(float64(m.clock().Unix()))
}

// failed records a sweep that ran and did not complete.
//
// IT DOES NOT ADVANCE THE TIMESTAMP, and that is the whole discrimination. A
// sweep that errors has not established that every book was looked at, so
// treating it as liveness would let a loop failing on its first book for a week
// report itself as current — the failure counter would rise beside a timestamp
// that says everything is fine, and the staleness rule would never fire.
func (m *sweepMetrics) failed() {
	m.failures.Inc()
}

// clock resolves the timestamp source.
func (m *sweepMetrics) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}
