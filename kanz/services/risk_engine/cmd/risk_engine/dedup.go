package main

import "github.com/prometheus/client_golang/prometheus"

// These describe selection and failed operations, never backend readiness or
// financial admission. Labels contain neither endpoint nor tenant credentials.
func sharedDedupMetrics(reg prometheus.Registerer, configured bool) func(string) {
	selected := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kanz_risk_shared_dedup_configured",
		Help: "Whether shared risk dedup is configured; not backend readiness.",
	})
	if configured {
		selected.Set(1)
	}
	errors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kanz_risk_shared_dedup_errors_total",
		Help: "Shared risk dedup failures; processing continues under handler idempotency.",
	}, []string{"operation"})
	for _, op := range []string{"claim", "commit", "release"} {
		errors.WithLabelValues(op).Add(0)
	}
	reg.MustRegister(selected, errors)
	return func(op string) {
		switch op {
		case "claim", "commit", "release":
			errors.WithLabelValues(op).Inc()
		}
	}
}
