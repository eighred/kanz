package bus

import (
	"context"
	"log/slog"
	"time"
)

// BACKLOG POLLING — the broker-side half of the bus's observability (#283).
//
// kanz_bus_consumer_lag and kanz_bus_pending_messages shipped as registered
// GaugeVecs with a doc comment describing "a periodic poller" and no poller. A
// GaugeVec with no WithLabelValues call exports the metric FAMILY and no
// SERIES, so `/metrics` carried the names and Prometheus never held a sample.
// Two KEDA ScaledObjects (infra/deploy/{risk-engine,market-data}-scaledobject.yaml)
// scale on those series, so both sat at minReplicaCount under any load — and an
// empty vector reads as an idle, healthy system rather than as an error. This
// file is the poller, and the failure policy below is the reason it is more
// than fifty lines.
//
// WHY A POLLER AND NOT A prometheus.GaugeFunc. This estate already prefers
// GaugeFunc where it fits — services/lineage/cmd/lineage/main.go (#244) and
// services/oms/cmd/oms/main.go read their gauges THROUGH the structure they
// describe precisely so the metric cannot drift from it. Both of those read an
// in-process map: nanoseconds, and it cannot fail. Backlog is BROKER state, and
// a GaugeFunc over it would put a $JS.API.CONSUMER.INFO / Kafka
// OffsetFetch+ListOffsets round-trip INSIDE the Prometheus scrape. The failure
// that matters is not the latency, it is the blast radius: a scrape that blocks
// on an unresponsive broker times out and drops THE WHOLE ENDPOINT, so
// kanz_bus_consume_total, kanz_bus_consume_halted_total and every other series
// this process exports disappear at exactly the moment an operator needs them —
// the observability of the incident is taken out by the metric reporting it.
// #230's BusConsumerStalled is written over one of those counters. A poller pays
// staleness (bounded below by backlogPollInterval) to keep the scrape local and
// unconditional, and staleness is the cheaper of the two.
//
// The GaugeFunc's real virtue — one writer, no drift — is kept by construction
// instead: pollBacklog is the ONLY caller of SetPending and SetConsumerLag, and
// it owns the series' whole lifecycle including deletion.

// metricsScrapeInterval is Prometheus's global scrape_interval for this estate
// (infra/observability/prometheus.yaml). It is restated here because
// backlogPollInterval is only meaningful RELATIVE to it, and
// test/arch/backlog_poll_interval_test.go reads the YAML and fails the build if
// this constant and that file ever disagree — a number copied out of a config
// file is the dated-evidence trap CLAUDE.md names, so it is checked rather than
// trusted.
const metricsScrapeInterval = 30 * time.Second

// backlogPollInterval is how often each subscription asks its broker how far
// behind it is.
//
// IT MUST NOT EXCEED THE SCRAPE INTERVAL. If it did, consecutive scrapes would
// read the same poll — the series would be a staircase, and every sample
// carrying a value up to (poll − scrape) older than it appears. KEDA then acts
// on it a third time removed: the trigger's own pollingInterval (default 30s)
// queries Prometheus, so end-to-end age is backlogPollInterval + scrape + KEDA
// polling. 15s keeps the term this file controls the SMALLEST of the three.
//
// Going lower is not free: this poll runs per (pod, subject), so a 12-replica
// risk-engine over five subjects is 60 consumer-info round-trips per interval.
// 15s is the largest value that satisfies the ordering above, which is the
// right side to err on.
const backlogPollInterval = 15 * time.Second

// backlogPollTimeout bounds ONE poll. Without it a broker that accepts the
// connection and never answers would hold the poll goroutine past its own
// interval, and the series would go stale while the poller reported no failure
// at all — "no answer" wearing "checked, and fine"'s clothes, one layer down
// from the defect this file exists to remove.
const backlogPollTimeout = 5 * time.Second

// Compile-time assertions, in the shape dedup.go uses for the claim lease: the
// two orderings this file depends on are checked BY THE COMPILER, so inverting
// either constant stops the package building instead of quietly degrading a
// scale signal in production.
//
//	backlogPollInterval <= metricsScrapeInterval — every scrape sees a fresh poll.
//	backlogPollTimeout  <  backlogPollInterval   — a poll cannot outlive its slot.
const (
	_ = uint(metricsScrapeInterval - backlogPollInterval)
	_ = uint(backlogPollInterval - backlogPollTimeout - 1)
)

// BacklogKind says WHICH gauge family a transport's readings belong to. The two
// are not interchangeable and must never be summed together: NATS pending is one
// number for a whole durable consumer, Kafka lag is one number per partition,
// and the KEDA ScaledObjects query them by name.
type BacklogKind int

const (
	// BacklogPending is JetStream ConsumerInfo.NumPending ⇒ kanz_bus_pending_messages.
	BacklogPending BacklogKind = iota + 1
	// BacklogLag is Kafka high-watermark minus committed offset ⇒ kanz_bus_consumer_lag.
	BacklogLag
)

// transport is the value of the `transport` label on the poller's own health
// series. It exists so those series stay attributable AFTER the backlog gauge
// has been deleted — which is exactly the state they are there to describe.
func (k BacklogKind) transport() string {
	switch k {
	case BacklogPending:
		return "nats"
	case BacklogLag:
		return "kafka"
	default:
		return "unknown"
	}
}

// PartitionBacklog is one broker-side reading: how many messages a consumer
// group has not yet consumed. Partition is "" for a transport that has no
// partitions (NATS JetStream), and the decimal partition id for Kafka.
type PartitionBacklog struct {
	Partition string
	Messages  int64
}

// BacklogSource is a transport that can be asked how far behind a consumer
// group is. *NATSClient and *KafkaClient both implement it; the assertions live
// next to each type.
//
// Backlog MUST return an error rather than a zero reading when the broker did
// not answer. That distinction is the entire point of this interface: a poller
// that cannot tell "no backlog" from "no answer" reinstates #283 on a 15-second
// timescale instead of a permanent one, and the KEDA trigger it feeds would
// scale DOWN during a broker incident.
type BacklogSource interface {
	BacklogKind() BacklogKind
	Backlog(ctx context.Context, subject, group string) ([]PartitionBacklog, error)
}

// EphemeralBacklogSource is a transport that can report how far behind ONE
// ephemeral consumer is, addressed by the name the BROKER generated for it
// (#1009).
//
// It exists because BacklogSource cannot serve the broadcast path at all:
// Backlog resolves the consumer through durableName(group, subject), and an
// ephemeral consumer has no durable name to resolve. That is why
// kanz_bus_pending_messages was structurally unavailable for every broadcast
// subscription in the estate — the compliance position book, both mandate
// registries, the OMS's mark and cash folds, the halt gate — rather than merely
// unwired.
//
// It is a SEPARATE interface, asserted the way BroadcastSubscriber is, because
// broadcast is a NATS-only concept here: KafkaClient implements BacklogSource
// and must not be forced to answer a question its transport does not have.
type EphemeralBacklogSource interface {
	BacklogKind() BacklogKind
	BacklogForConsumer(ctx context.Context, stream, consumer string) ([]PartitionBacklog, error)
}

// The `delivery` label's two values. A queue-group durable is SHARED across the
// replicas of a service and its backlog is a queue of work; a broadcast
// consumer is PER POD and its backlog is a control reading stale state. Summing
// them would be meaningless, so they are told apart on the series rather than
// inferred from an empty group — and the estate's three KEDA queries all select
// an explicit group=, so neither is picked up by a scaler it was not meant for.
const (
	deliveryLabelGroup     = "group"
	deliveryLabelBroadcast = "broadcast"
)

// backlogRead is ONE poll, however the consumer is addressed: the durable path
// asks by (subject, group), the broadcast path by the ephemeral consumer's
// generated name. Injecting the read is what keeps a single loop and a single
// failure policy — the deletion, the two health series and the transition
// logging below are the part that took #283 fifty lines to get right, and a
// second copy of them for the broadcast path is how one of the two would come to
// zero-fill during an outage.
type backlogRead func(ctx context.Context) ([]PartitionBacklog, error)

// startBacklogPoll launches the backlog poller for one subscription and returns
// a function that stops it and waits for it to finish clearing up.
//
// It is started FROM Subscribe rather than wired at each composition root for
// the reason tuningForSubject gives for living in tuning.go: twenty-odd binaries
// build a bus client, and twenty copies of the same three lines is how a fix
// stops spreading (CLAUDE.md's `secret()` lesson). A service gets the scale
// signal by subscribing, with nothing to wire and nothing to forget.
//
// Two ways it declines, and neither is silent in a way that matters:
//
//   - c.metrics == nil. The consumer asked for no instrumentation at all; a
//     backlog gauge with no BusMetrics to put it in is not a thing that can exist.
//   - the Subscriber is not a BacklogSource. In THIS module that is only ever a
//     test fake — both real transports implement it and the compile-time
//     assertions in nats.go and kafka.go keep it that way, so a new transport
//     that forgets fails to build rather than shipping a dead scale signal.
func (c *Consumer) startBacklogPoll(ctx context.Context, subject, group string) func() {
	src, ok := c.subscriber.(BacklogSource)
	if !ok || c.metrics == nil {
		return func() {}
	}
	return c.pollInBackground(ctx, src.BacklogKind(), subject, group, deliveryLabelGroup,
		func(pollCtx context.Context) ([]PartitionBacklog, error) {
			return src.Backlog(pollCtx, subject, group)
		})
}

// startEphemeralBacklogPoll is startBacklogPoll for a BROADCAST subscription
// (#1009).
//
// The only difference is how the consumer is addressed. subscribeEphemeral hands
// back the stream and the server-generated consumer name as soon as the consumer
// exists, and that pair is the durable name's equivalent — everything after it,
// including the failure policy that makes an unanswered poll DELETE the series
// rather than zero-fill it, is the same loop.
//
// The group label is empty because a broadcast consumer HAS no group: it is one
// consumer per pod, not one shared across replicas. delivery="broadcast" is what
// says so out loud, so an empty group reads as a fact about the subscription
// rather than as a value somebody forgot to set.
func (c *Consumer) startEphemeralBacklogPoll(ctx context.Context, subject, stream, consumer string) func() {
	src, ok := c.subscriber.(EphemeralBacklogSource)
	if !ok || c.metrics == nil || stream == "" || consumer == "" {
		return func() {}
	}
	return c.pollInBackground(ctx, src.BacklogKind(), subject, "", deliveryLabelBroadcast,
		func(pollCtx context.Context) ([]PartitionBacklog, error) {
			return src.BacklogForConsumer(pollCtx, stream, consumer)
		})
}

// pollInBackground runs one poller and returns the stop-and-join both callers use.
func (c *Consumer) pollInBackground(
	ctx context.Context, kind BacklogKind, subject, group, delivery string, read backlogRead,
) func() {
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pollBacklog(pollCtx, read, kind, c.metrics, subject, group, delivery)
	}()
	// Joined, not abandoned: the poller DELETES its series on the way out (see
	// pollBacklog), and returning from Subscribe before that has happened would
	// leave this pod exporting a backlog for a subscription it no longer has.
	return func() {
		cancel()
		<-done
	}
}

// pollBacklog is the loop, and the failure policy is the part worth reading.
//
// WHAT AN UNREACHABLE BROKER PUBLISHES: NOTHING. On a failed poll the backlog
// series for (subject, group) is DELETED, not set to zero and not left at its
// last value.
//
//   - Zero is the original defect at a shorter timescale. "No backlog" and "no
//     answer" would again be the same reading, and the KEDA trigger would scale
//     the service IN during a broker incident — the one direction that makes the
//     incident worse.
//
//   - Last-value-held is what the archiver's own lag poller does
//     (services/archiver/cmd/archiver/main.go: log a warning and `continue`).
//     It is better than zero and still wrong: the number is a fiction with no
//     expiry, KEDA keeps scaling on a backlog measured before the outage, and
//     nothing in the series says how old it is.
//
//   - Deleted leaves an EMPTY vector, which is a state a consumer can act on —
//     provided it is configured to. It is not self-describing, so it never
//     travels alone; the two health series below are published alongside and
//     stay present through the outage:
//
//     kanz_bus_backlog_poll_failures_total          — rising ⇒ this pod is trying and failing.
//     kanz_bus_backlog_poll_last_success_timestamp_seconds — `time() - x` is the age of the last real answer.
//
//     Present-but-old is what separates "no answer" from "nobody subscribes to
//     this subject", which a bare absence cannot do.
//
// WHAT KEDA DOES WITH AN EMPTY VECTOR, WHICH IS WHY THE MANIFESTS CHANGED TOO.
// The Prometheus scaler defaults to ignoreNullValues: true, under which an empty
// result is reported as the value 0 with NO error — deleting the series would
// then be indistinguishable from zero backlog and this policy would buy nothing.
// Both ScaledObjects now set ignoreNullValues: "false", under which the scaler
// returns an error: KEDA marks ScalingActive=False, surfaces the failure on the
// ScaledObject and in keda_scaler_errors, and the HPA — having no value for the
// metric — HOLDS the current replica count rather than scaling in. Hold-and-shout
// is the correct behaviour for "I cannot see the backlog"; scale-in is not.
//
// LOGGED ON TRANSITION ONLY. Every pod of every consumer runs this loop, so a
// line per failed poll is a log flood during precisely the outage someone is
// reading the log through. The series above are the durable signal; the log is
// the pointer to which pod and which subject.
func pollBacklog(
	ctx context.Context, read backlogRead, kind BacklogKind, m *BusMetrics, subject, group, delivery string,
) {
	logger := slog.Default()
	// seen tracks the partitions this loop has published, so a Kafka rebalance
	// that moves a partition away does not leave its last lag reading behind as
	// a permanent series. NATS contributes the single "" entry.
	seen := map[string]bool{}
	failing := false

	defer func() {
		// This subscription is over. Whatever this pod last measured is no longer
		// something it is measuring, and a series left behind here is the same
		// stale fiction the failure path above refuses to publish.
		m.forgetBacklog(kind, subject, group, delivery)
		m.forgetBacklogPollHealth(kind, subject, group, delivery)
	}()

	ticker := time.NewTicker(backlogPollInterval)
	defer ticker.Stop()
	for {
		// Polled BEFORE the first tick: a pod that starts into an existing backlog
		// must not spend a whole interval exporting no series, because during that
		// interval it is indistinguishable from the bug this replaces.
		pollCtx, cancel := context.WithTimeout(ctx, backlogPollTimeout)
		readings, err := read(pollCtx)
		cancel()

		switch {
		case err != nil:
			m.observeBacklogPollFailure(kind, subject, group, delivery)
			m.forgetBacklog(kind, subject, group, delivery)
			seen = map[string]bool{}
			if !failing {
				failing = true
				logger.Warn("bus: backlog poll failed — the consumer-backlog series for this subscription "+
					"has been DELETED rather than reported as zero, so any KEDA trigger over it now errors and "+
					"holds replicas instead of scaling in; kanz_bus_backlog_poll_last_success_timestamp_seconds "+
					"carries the age of the last real answer",
					"subject", subject, "group", group, "delivery", delivery, "transport", kind.transport(), "err", err)
			}
		default:
			m.setBacklog(kind, subject, group, delivery, readings, seen)
			next := make(map[string]bool, len(readings))
			for _, r := range readings {
				next[r.Partition] = true
			}
			seen = next
			m.observeBacklogPollSuccess(kind, subject, group, delivery)
			if failing {
				failing = false
				logger.Info("bus: backlog poll recovered",
					"subject", subject, "group", group, "delivery", delivery, "transport", kind.transport())
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
