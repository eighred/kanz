package controller

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the autopilot control-loop counters (OBS-01c). nil-safe so tests
// can pass nil. The closed-loop SLI a reviewer wants is here: how often a
// condition was auto-remediated vs escalated to a human.
type Metrics struct {
	outcomes *prometheus.CounterVec // condition, outcome
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_autopilot_outcomes_total",
			Help: "Control-loop outcomes by condition and outcome (remediated|escalated).",
		}, []string{"condition", "outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.outcomes)
	}
	return m
}

func (m *Metrics) incOutcome(condition string, o Outcome) {
	if m == nil {
		return
	}
	m.outcomes.WithLabelValues(condition, string(o)).Inc()
}
