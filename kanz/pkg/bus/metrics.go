package bus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BusMetrics is the bus RED/USE exporter (OBS-01c): publish/consume rate +
// errors + duration, plus the consumer-lag and pending gauges that are the
// KEDA autoscale signal (INFRA-01c). It is optional — a nil *BusMetrics is
// safe on every method, so a Producer/Consumer with no metrics wired is a
// no-op, not a panic. Register once on the OBS-01a Provider.Registry and share
// the instance across the producers/consumers in a service.
type BusMetrics struct {
	publishTotal   *prometheus.CounterVec   // subject, result
	publishLatency *prometheus.HistogramVec // subject
	consumeTotal   *prometheus.CounterVec   // subject, group, result
	consumeLatency *prometheus.HistogramVec // subject, group
	consumeHalted  *prometheus.CounterVec   // subject, group
	consumerLag    *prometheus.GaugeVec     // subject, group, partition
	pending        *prometheus.GaugeVec     // subject, group
}

// NewBusMetrics builds and registers the bus collectors on reg.
func NewBusMetrics(reg prometheus.Registerer) *BusMetrics {
	m := &BusMetrics{
		publishTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_publish_total",
			Help: "messages published, by subject and result (ok|error).",
		}, []string{"subject", "result"}),
		publishLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kanz_bus_publish_duration_seconds",
			Help:    "publish call latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"subject"}),
		consumeTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_consume_total",
			Help: "messages dispatched to handlers, by subject, group and result (ok|error).",
		}, []string{"subject", "group", "result"}),
		consumeLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kanz_bus_consume_duration_seconds",
			Help:    "handler dispatch latency (including retries).",
			Buckets: prometheus.DefBuckets,
		}, []string{"subject", "group"}),
		// THE ONLY SIGNAL THAT A KAFKA SUBSCRIPTION STOPPED TO AVOID LOSING AN
		// EVENT. Incremented by KafkaClient.Subscribe when a delivery could
		// neither be handled nor dead-lettered, so the loop returns without
		// committing (EVT-17e / #219). Every other bus series reads NORMAL in
		// that state — kanz_bus_consume_total records the failed dispatch and
		// then stops moving, and kanz_bus_consumer_lag is computed from the
		// COMMITTED offset, which by construction did not advance, so lag grows
		// the way an idle topic's does. This counter is what distinguishes
		// "quiet" from "stopped, on purpose, with a message it refused to skip".
		consumeHalted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_consume_halted_total",
			Help: "subscriptions halted without committing because a delivery could neither be handled nor dead-lettered — the dead-letter path is down and the consumer is holding the offset rather than skipping the event.",
		}, []string{"subject", "group"}),
		consumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_bus_consumer_lag",
			Help: "Kafka consumer lag (high-water offset - committed) — the KEDA scale signal.",
		}, []string{"subject", "group", "partition"}),
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_bus_pending_messages",
			Help: "NATS JetStream consumer pending messages — the KEDA scale signal.",
		}, []string{"subject", "group"}),
	}
	reg.MustRegister(
		m.publishTotal, m.publishLatency,
		m.consumeTotal, m.consumeLatency, m.consumeHalted,
		m.consumerLag, m.pending,
	)
	return m
}

func result(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func (m *BusMetrics) observePublish(subject string, d time.Duration, err error) {
	if m == nil {
		return
	}
	m.publishTotal.WithLabelValues(subject, result(err)).Inc()
	m.publishLatency.WithLabelValues(subject).Observe(d.Seconds())
}

func (m *BusMetrics) observeConsume(subject, group string, d time.Duration, err error) {
	if m == nil {
		return
	}
	m.consumeTotal.WithLabelValues(subject, group, result(err)).Inc()
	m.consumeLatency.WithLabelValues(subject, group).Observe(d.Seconds())
}

// observeConsumeHalt records that a subscription stopped rather than commit
// past a delivery it could neither handle nor dead-letter. It is NOT paired
// with observeConsume: that one has already counted the dispatch failure, and
// this one counts the separate, more severe outcome — the same split the
// archiver makes between a routing failure and a failed DLQ produce
// (services/archiver/internal/archive/archiver.go, observeFail(DLQSubject,
// "dlq_publish")).
func (m *BusMetrics) observeConsumeHalt(subject, group string) {
	if m == nil {
		return
	}
	m.consumeHalted.WithLabelValues(subject, group).Inc()
}

// SetConsumerLag publishes the Kafka lag for one (subject, group, partition).
// A periodic poller over kafka-go's Reader.Stats().Lag feeds this — the lag
// is broker state, not on the per-message path.
func (m *BusMetrics) SetConsumerLag(subject, group, partition string, lag float64) {
	if m == nil {
		return
	}
	m.consumerLag.WithLabelValues(subject, group, partition).Set(lag)
}

// SetPending publishes the NATS JetStream pending count for one (subject,
// group), fed by a poller over the consumer's ConsumerInfo.NumPending.
func (m *BusMetrics) SetPending(subject, group string, pending float64) {
	if m == nil {
		return
	}
	m.pending.WithLabelValues(subject, group).Set(pending)
}
