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
}

// NewMetrics builds and registers the risk-engine collectors on reg (the
// OBS-01a Provider.Registry).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		recomputeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_risk_recompute_total",
			Help: "portfolio recomputes, by status (ok|error).",
		}, []string{"status"}),
		recomputeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kanz_risk_recompute_duration_seconds",
			Help: "exposure+measures recompute + emit latency (the ORCH-01f p99 budget).",
			// Sub-ms to the 500ms budget and beyond, so the p99 query resolves
			// around the gate.
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}),
	}
	reg.MustRegister(m.recomputeTotal, m.recomputeDuration)
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
