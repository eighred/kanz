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
	dlqParked      *prometheus.CounterVec   // subject, group, class
	dlqRedrive     *prometheus.CounterVec   // subject, result
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
		// THE INFLOW SIDE OF THE DEAD-LETTER QUEUE (#230). Incremented ONLY when
		// Consumer.publishDLQ has actually parked the message — the DLQ publish
		// returned nil, so a recoverable copy exists on `dlq.<subject>` for
		// cmd/kanz-redrive to find.
		//
		// WHY NOT kanz_bus_consume_total{result="error"}, WHICH ALREADY MOVES ON
		// EVERY ONE OF THESE. Because it moves on more than these, and the
		// difference is the whole question a responder is asking:
		//
		//   - It counts a dispatch failure whether or not a DLQ is configured. A
		//     consumer built without WithDLQ returns the error to the broker and
		//     the event is redelivered or dropped; a consumer with one has a
		//     durable copy. "Parked" and "gone" must not share a series.
		//   - It counts the failure BEFORE the park is attempted. When the park
		//     itself fails, consume_total{result="error"} still increments while
		//     nothing was parked at all — on Kafka that is the halt case
		//     (kanz_bus_consume_halted_total), and on NATS the event goes back to
		//     the broker. Counting it as a park would report a recoverable message
		//     that does not exist.
		//   - It carries no CLASS, and class is what decides whether the message is
		//     recoverable. A redrive returns a `transient` park to its subject; it
		//     REFUSES a `terminal` one, because re-running unframeable bytes just
		//     parks them again (see IsTerminal). Those are two different incidents
		//     with two different responses and they must be separable in a rule.
		//
		// THIS IS INFLOW, NOT DEPTH, AND THAT IS DELIBERATE. A depth gauge would
		// have to be fed by a poller over JetStream ConsumerInfo/StreamInfo, and
		// this package has already shipped two poller-fed gauges whose poller was
		// never written — consumerLag and pending below, both of which export no
		// series at all and are why alerts/operational.rules.yaml has no rule over
		// either. Adding a third of the same shape would look like DLQ depth was
		// now measured while measuring nothing. Inflow is on the message path, so
		// it is real the moment this line runs; paired with dlqRedrive (outflow) it
		// makes the dead-letter queue a two-sided, observable thing.
		dlqParked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_dlq_parked_total",
			Help: "messages parked on dlq.<subject> after a delivery failed, by ORIGINAL subject, consumer group and failure class (transient|terminal). Counted only once the DLQ publish succeeded, so every increment is a message a redrive can still find.",
		}, []string{"subject", "group", "class"}),
		// THE DRAIN'S OWN RED SIGNAL (#220). Labelled by DESTINATION subject and
		// result (ok|refused|error), because the three outcomes need different
		// human responses: `ok` is recovery, `error` is a broken drain, and
		// `refused` is the drain WORKING — a loop bound or an age gate holding on
		// a specific message that now needs a decision. Folding refusals into
		// errors would make a functioning safety limit look like an outage; the
		// same split archiver makes between a routing failure and a failed DLQ
		// produce.
		dlqRedrive: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_dlq_redrive_total",
			Help: "parked messages a redrive run moved back onto their original subject, by destination subject and result (ok|refused|error).",
		}, []string{"subject", "result"}),
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
		m.dlqParked, m.dlqRedrive,
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

// observeDLQPark records one message that reached the dead-letter queue. class
// is ClassTransient or ClassTerminal — the same value written to
// HeaderDLQClass, taken from the same classOf() call, so the series and the
// parked message's own header can never disagree about what happened.
//
// Call it AFTER the DLQ publish succeeds and never before: a park that failed
// left no message anywhere, and counting it here would tell a responder to go
// looking for a copy that was never written.
func (m *BusMetrics) observeDLQPark(subject, group, class string) {
	if m == nil {
		return
	}
	m.dlqParked.WithLabelValues(subject, group, class).Inc()
}

// observeRedrive records one parked message the drain acted on. result is
// "ok", "refused" or "error"; see the collector's comment for why those three
// are not collapsed.
func (m *BusMetrics) observeRedrive(subject, result string) {
	if m == nil {
		return
	}
	m.dlqRedrive.WithLabelValues(subject, result).Inc()
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
