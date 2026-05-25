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
		m.consumeTotal, m.consumeLatency,
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
