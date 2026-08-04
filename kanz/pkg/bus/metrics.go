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

	backlogPollFailures *prometheus.CounterVec // subject, group, transport
	backlogPollSuccess  *prometheus.GaugeVec   // subject, group, transport — unix seconds
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
		// when #230 wrote this, this package had already shipped two poller-fed
		// gauges whose poller was never written — consumerLag and pending below,
		// which exported no series at all and are why alerts/operational.rules.yaml
		// had no rule over either. Adding a third of the same shape would have
		// looked like DLQ depth was now measured while measuring nothing.
		//
		// #283 has since written that poller (backlog.go), so the premise of the
		// paragraph above no longer holds and a DLQ depth gauge is now a
		// reasonable thing to add — through pollBacklog, with its failure policy,
		// and not as a fourth bespoke loop. Inflow stays regardless: it is on the
		// message path, so it is real the moment this line runs, and paired with
		// dlqRedrive (outflow) it makes the dead-letter queue a two-sided,
		// observable thing without depending on a broker answering.
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
		// THE TWO BACKLOG GAUGES, AND THE ONE THING THAT MUST STAY TRUE OF THEM
		// (#283). Both are written by exactly one thing — pollBacklog in
		// backlog.go, started by Consumer.Subscribe — which is also what DELETES
		// them when it cannot reach the broker. Read that file's failure policy
		// before adding a second writer: a caller that sets either of these from
		// somewhere else can publish a zero the poller would have withheld, and
		// "no backlog" reading the same as "no answer" is the defect these two
		// spent their first years being.
		//
		// EMITTED PER POD, so both carry the scrape's own instance/pod labels and
		// every replica of a group reports the SAME group-wide number. Anything
		// aggregating them must collapse that duplication before summing —
		// `sum(max by (subject) (...))`, not `sum(...)`, which is why both KEDA
		// queries in infra/deploy/*-scaledobject.yaml are written that way.
		consumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_bus_consumer_lag",
			Help: "Kafka consumer lag (partition high-water offset - committed group offset) — the KEDA scale signal. Absent while the broker is unreachable; never zero-filled.",
		}, []string{"subject", "group", "partition"}),
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_bus_pending_messages",
			Help: "NATS JetStream consumer pending messages (ConsumerInfo.NumPending) — the KEDA scale signal. Absent while the broker is unreachable; never zero-filled.",
		}, []string{"subject", "group"}),
		// THE POLLER'S OWN HEALTH, AND THE REASON THE DELETION ABOVE IS SAFE. An
		// absent backlog gauge is not self-describing: "the broker is not
		// answering" and "nobody subscribes to that subject" look identical from
		// outside. These two stay PRESENT through an outage and are what tells
		// them apart — the poller is running, it is failing, and here is how long
		// it has been since it last had a real answer.
		backlogPollFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kanz_bus_backlog_poll_failures_total",
			Help: "Backlog polls that did not get an answer from the broker, by subject, consumer group and transport (nats|kafka). Each one also DELETED the backlog gauge for that subscription rather than publishing a zero.",
		}, []string{"subject", "group", "transport"}),
		backlogPollSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kanz_bus_backlog_poll_last_success_timestamp_seconds",
			Help: "Unix time of the last backlog poll that the broker answered. `time() - this` is the age of the newest real reading; it is the series that makes a MISSING kanz_bus_consumer_lag / kanz_bus_pending_messages attributable to a broker that stopped answering rather than to a subject nobody consumes.",
		}, []string{"subject", "group", "transport"}),
	}
	reg.MustRegister(
		m.publishTotal, m.publishLatency,
		m.consumeTotal, m.consumeLatency, m.consumeHalted,
		m.dlqParked, m.dlqRedrive,
		m.consumerLag, m.pending,
		m.backlogPollFailures, m.backlogPollSuccess,
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
//
// FED BY pollBacklog (backlog.go) VIA KafkaClient.Backlog, which computes the
// lag from the broker's own ListOffsets high-watermark minus the group's
// committed offset from OffsetFetch.
//
// IT IS NOT FED BY kafka-go's Reader.Stats().Lag, which is what this comment
// claimed for the years it described a poller nobody had written. That field
// cannot carry this signal, for three independent reasons, each fatal on its
// own (segmentio/kafka-go v0.4.51):
//
//   - It is written only from (*reader).read, i.e. ONLY WHEN A MESSAGE IS
//     ACTUALLY READ. A subscription that has stopped — which is exactly the
//     state worth measuring, and the one KafkaClient.Subscribe deliberately
//     enters rather than skip an event — freezes its last value forever while
//     the real backlog grows.
//   - Every per-partition reader a group reader spawns shares ONE *readerStats
//     (reader.go: `stats: r.stats`), so the gauge is last-writer-wins across
//     partitions. On a multi-partition topic it is neither the sum nor the max
//     of the lags; it is whichever partition delivered a batch most recently.
//   - kafka-go's own active lag poller (Reader.ReadLag / readLag) REFUSES to run
//     for a consumer-group reader — errNotAvailableWithGroup, and
//     activateReadLag skips group readers outright. Reader.Lag() likewise
//     returns -1 for one. Every reader this package creates has a GroupID.
//
// Reader.Stats() would also have been the wrong call to make on an interval:
// its counter fields are read-and-reset (`atomic.SwapInt64`), so a lag poller
// calling it would silently zero Fetches/Messages/Bytes/Errors for anyone else.
func (m *BusMetrics) SetConsumerLag(subject, group, partition string, lag float64) {
	if m == nil {
		return
	}
	m.consumerLag.WithLabelValues(subject, group, partition).Set(lag)
}

// SetPending publishes the NATS JetStream pending count for one (subject,
// group). Fed by pollBacklog (backlog.go) via NATSClient.Backlog, which reads
// ConsumerInfo.NumPending off the same durable Subscribe binds.
func (m *BusMetrics) SetPending(subject, group string, pending float64) {
	if m == nil {
		return
	}
	m.pending.WithLabelValues(subject, group).Set(pending)
}

// setBacklog writes one poll's readings into whichever gauge family the
// transport owns, then removes any partition that was present in the PREVIOUS
// poll and is not in this one.
//
// That second half is not tidiness. A Kafka rebalance moves partitions between
// pods, and a lag reading left behind by a partition this pod no longer owns is
// a number nothing will ever update again — double-counted by any aggregation
// and immune to the deletion the failure path performs, because the poll that
// would have deleted it is succeeding.
func (m *BusMetrics) setBacklog(kind BacklogKind, subject, group string, readings []PartitionBacklog, prev map[string]bool) {
	if m == nil {
		return
	}
	current := make(map[string]bool, len(readings))
	for _, r := range readings {
		current[r.Partition] = true
		switch kind {
		case BacklogPending:
			m.SetPending(subject, group, float64(r.Messages))
		case BacklogLag:
			m.SetConsumerLag(subject, group, r.Partition, float64(r.Messages))
		}
	}
	if kind != BacklogLag {
		return
	}
	for p := range prev {
		if !current[p] {
			m.consumerLag.DeleteLabelValues(subject, group, p)
		}
	}
}

// forgetBacklog removes every backlog series for (subject, group). Called when
// a poll fails and when the subscription ends — the two states in which this pod
// is no longer measuring this backlog, and must therefore not be reporting one.
func (m *BusMetrics) forgetBacklog(kind BacklogKind, subject, group string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"subject": subject, "group": group}
	switch kind {
	case BacklogPending:
		m.pending.Delete(labels)
	case BacklogLag:
		// Partial match: the partition set is the broker's to decide and may have
		// changed under us, so deletion cannot enumerate it.
		m.consumerLag.DeletePartialMatch(labels)
	}
}

// observeBacklogPollFailure records a poll the broker did not answer. It is
// paired with the deletion in forgetBacklog, never a substitute for it: the
// counter says the poller is in trouble, the deletion is what stops a stale or
// invented backlog reaching a KEDA trigger.
func (m *BusMetrics) observeBacklogPollFailure(kind BacklogKind, subject, group string) {
	if m == nil {
		return
	}
	m.backlogPollFailures.WithLabelValues(subject, group, kind.transport()).Inc()
	// Touch the failure counter's success partner so the pair is complete from
	// the first poll. Without this a subscription whose very first poll fails
	// exports a failure count and NO timestamp, and `time() - <absent>` is not a
	// staleness an alert can express.
	m.backlogPollSuccess.WithLabelValues(subject, group, kind.transport())
}

// observeBacklogPollSuccess stamps the wall-clock time of a poll the broker
// answered. `time() - kanz_bus_backlog_poll_last_success_timestamp_seconds` is
// the age of the newest real backlog reading, which is the only thing that makes
// an ABSENT backlog gauge attributable.
func (m *BusMetrics) observeBacklogPollSuccess(kind BacklogKind, subject, group string) {
	if m == nil {
		return
	}
	m.backlogPollSuccess.WithLabelValues(subject, group, kind.transport()).SetToCurrentTime()
}

// forgetBacklogPollHealth drops the poller's own series when the subscription
// ends. Kept separate from forgetBacklog because the failure path calls that one
// and MUST NOT call this one — deleting the staleness evidence at the moment it
// becomes load-bearing is how the outage goes back to looking like an idle bus.
func (m *BusMetrics) forgetBacklogPollHealth(kind BacklogKind, subject, group string) {
	if m == nil {
		return
	}
	m.backlogPollFailures.DeleteLabelValues(subject, group, kind.transport())
	m.backlogPollSuccess.DeleteLabelValues(subject, group, kind.transport())
}
