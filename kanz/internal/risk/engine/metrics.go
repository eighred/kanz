package engine

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the risk-engine RED exporter (OBS-01c). The series names + the
// `status` label are the contract the CICD-01e canary analysis
// (kanz/infra/deploy/analysis-template.yaml) queries to gate a rollout:
// kanz_risk_recompute_total{status="ok"} for the success rate and
// kanz_risk_recompute_duration_seconds for the p99 budget — renaming either
// silently breaks the automated rollback. Optional: a nil *Metrics is safe on
// every method.
type Metrics struct {
	recomputeTotal    *prometheus.CounterVec // status (ok|error)
	recomputeDuration prometheus.Histogram

	// emitSkipped and awaitingEmit are SEPARATE SERIES, NOT NEW `status` VALUES
	// ON recomputeTotal, and that is load-bearing (#620).
	//
	// The canary query above is ok / TOTAL with an UNLABELLED denominator
	// (analysis-template.yaml). Any third status value therefore dilutes the
	// success ratio without any recompute having failed — so counting a normal
	// shutdown skip on that counter would make every rolling deploy look like a
	// canary regression and roll itself back. The reason the label set is a
	// contract cuts both ways: it cannot be renamed, and it cannot be EXTENDED.
	emitSkipped  prometheus.Counter // recomputes whose emit was skipped at shutdown
	awaitingEmit prometheus.Gauge   // portfolios whose last emit failed and has not since succeeded
}

// NewMetrics builds and registers the risk-engine collectors on reg (the
// OBS-01a Provider.Registry).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		recomputeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_risk_recompute_total",
			Help: "portfolio recomputes, by status (ok|error).",
		}, []string{"status"}),
		emitSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kanz_risk_emit_skipped_total",
			Help: "recomputes that updated the cache but skipped their emit because the process was shutting down.",
		}),
		awaitingEmit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kanz_risk_portfolios_awaiting_emit",
			Help: "portfolios whose last emit failed and has not since succeeded; each is serving a stale exposure FACT as current.",
		}),
		recomputeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kanz_risk_recompute_duration_seconds",
			Help: "exposure+measures recompute + emit latency (the ORCH-01f p99 budget).",
			// Sub-ms to the 500ms budget and beyond, so the p99 query resolves
			// around the gate.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}),
	}
	reg.MustRegister(m.recomputeTotal, m.recomputeDuration, m.emitSkipped, m.awaitingEmit)
	return m
}

// observeRecompute records one fired recompute's outcome and latency. status
// is "error" when an emit failed, "ok" otherwise.
func (m *Metrics) observeRecompute(d time.Duration, err error) {
	if m == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.recomputeTotal.WithLabelValues(status).Inc()
	m.recomputeDuration.Observe(d.Seconds())
}

// observeEmitSkipped records a recompute that updated the cache and did not
// emit because the process was shutting down (#620).
//
// It exists because that path used to `return` BEFORE observeRecompute, so the
// one signal that a recompute never reached the spine was skipped on the path
// most likely to skip it — the comment on that line said so and nothing counted
// it. On its own series, never as a recomputeTotal status: see the struct.
func (m *Metrics) observeEmitSkipped() {
	if m == nil {
		return
	}
	m.emitSkipped.Inc()
}

// setAwaitingEmit publishes how many portfolios are currently serving a stale
// exposure FACT because their last emit failed (#620).
//
// THIS IS THE ALERTABLE ONE. kanz_risk_recompute_total{status="error"} already
// counted the failure, but a counter cannot say whether the condition is STILL
// TRUE: a portfolio that fails one emit and then stops trading has no next
// apply to re-trigger it, so the last successfully-published exposure stands as
// the portfolio's live risk indefinitely and the error counter stays flat. A
// gauge above zero means at least one book's published risk is not its real
// risk, right now.
func (m *Metrics) setAwaitingEmit(n int) {
	if m == nil {
		return
	}
	m.awaitingEmit.Set(float64(n))
}
