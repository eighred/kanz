package cdc

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the lake-sink landing counters (OBS-01c convention). nil-safe so
// tests and the no-metrics path can pass nil. The bus RED metrics already cover
// delivery throughput/latency; these add sink-specific landing + decode outcomes.
type Metrics struct {
	rows    *prometheus.CounterVec // domain, entity, result
	decodes *prometheus.CounterVec // result: ok|transient|permanent
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_lake_sink_rows_total",
			Help: "Rows landed into the lakehouse, by domain/entity and result.",
		}, []string{"domain", "entity", "result"}),
		decodes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_lake_sink_decode_errors_total",
			Help: "Payload decode failures, by class (transient vs permanent).",
		}, []string{"result"}),
	}
	if reg != nil {
		reg.MustRegister(m.rows, m.decodes)
	}
	return m
}

func (m *Metrics) incRow(domain, entity, result string) {
	if m == nil {
		return
	}
	m.rows.WithLabelValues(domain, entity, result).Inc()
}

func (m *Metrics) incDecode(result string) {
	if m == nil {
		return
	}
	m.decodes.WithLabelValues(result).Inc()
}
