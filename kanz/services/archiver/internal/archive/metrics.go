package archive

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the archiver's own counters. Lag (how far behind the spine it is)
// comes from a periodic poll of the durable consumer's pending count via
// bus.NATSClient.Pending — see cmd/archiver/main.go — the same NumPending
// JetStream reports that feeds bus.BusMetrics.SetPending's kanz_bus_pending_messages
// gauge (the risk-engine's KEDA scale signal). The archiver has no bus.Consumer of
// its own to wire a BusMetrics through (it implements archive.Subscriber directly
// against *bus.NATSClient), so it exports the same signal under its own name,
// kanz_archiver_pending_messages, scoped to its own durable.
//
// AN ARCHIVER SILENTLY FALLING BEHIND ITS STREAM'S MAX-AGE IS DATA LOSS WITH A
// DELAY ON IT. Being down is survivable and self-healing; being slow is not, and it
// is invisible without this.
type Metrics struct {
	Archived *prometheus.CounterVec // by topic
	Failed   *prometheus.CounterVec // by topic, reason
	Lag      *prometheus.GaugeVec   // by subject: messages pending on the spine
}

// NewMetrics registers the archiver's metrics.
//
// Failed's "reason" label names every way Handle can fail to land an event on
// its real topic:
//
//   - "unframe"     — the body never decoded. Terminal: dead-lettered and acked.
//   - "route"       — event_type/tenant never mapped to a topic. Terminal:
//     dead-lettered and acked.
//   - "publish"     — the real-topic Kafka produce failed. Transient: NACKed.
//   - "dlq_publish" — the DLQ produce ITSELF failed, on top of an "unframe" or
//     "route" cause. The event reached NEITHER its real topic nor the DLQ —
//     the most severe outcome — so it is counted separately from (in addition
//     to) the underlying cause, and Transient: NACKed.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Archived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_archiver_archived_total",
			Help: "Envelopes durably written to the Kafka log of record.",
		}, []string{"topic"}),
		Failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_archiver_failed_total",
			Help: "Envelopes the archiver did not durably write to their real topic, by reason (unframe|route|publish|dlq_publish).",
		}, []string{"topic", "reason"}),
		Lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_archiver_pending_messages",
			Help: "Messages pending on the spine for this archiver's durable consumer.",
		}, []string{"subject"}),
	}
	reg.MustRegister(m.Archived, m.Failed, m.Lag)
	return m
}
