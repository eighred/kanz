// Package kafkatest provisions Kafka topics for integration tests.
//
// IT EXISTS BECAUSE CreateTopics RETURNS BEFORE THE TOPIC IS USABLE, and because
// eight places in this module had learned that separately or not at all. Seven
// created a topic and published immediately; one — tools/natsrebuild's
// tenant_rebuild_integration_test.go — had worked out the correct wait, written
// down the measurements, and kept them where nothing else could find them. That
// is CLAUDE.md's secret() shape with the polarity reversed: the right
// implementation existed and did not spread, so the wrong one kept being
// rediscovered as a flaky test.
//
// THE FAILURE IT PREVENTS. Auto-creation is off estate-wide
// (infra/kafka/topics-job.yaml, and KAFKA_AUTO_CREATE_TOPICS_ENABLE=false on the
// CI and local rigs), which is deliberate: it makes a missing topic declaration
// loud instead of silently conjuring one. The consequence is that a produce
// arriving before metadata has settled fails with UNKNOWN_TOPIC_OR_PARTITION —
// an error that reads like "the topic was never created" and sends whoever is
// debugging it to the provisioning code rather than to a race. #311 is that
// failure on pkg/bus's halt test.
//
// THE CONDITION IS "PRESENT AND LED", NOT MERELY "PRESENT". A partition can
// appear in metadata before a leader is elected, and a produce to a leaderless
// partition fails with the SAME error code — so a check that only asked whether
// the topic existed would pass and the produce would still fail.
//
// WHAT THIS DOES NOT FIX, stated because the measurement already exists and
// contradicts the obvious guess. On a local single-node broker the leaderless
// window was never observed: partitions came back already led in 2.6–11.8ms,
// and 7 of 8 fresh topics STILL failed their first produce. That cause was
// different — kafka-go's package-level DefaultTransport, whose 6s metadata TTL
// is shared by every Writer in the process, so a topic created after some
// earlier produce populated the cache stays invisible for up to 6s. This wait
// does nothing about that. A caller publishing through a Writer with a nil
// Transport needs a private one as well; see writeFrame in
// tools/natsrebuild/tenant_rebuild_integration_test.go, which carries the
// measurement. bus.DialKafka is already safe here — it builds a private
// kafka.Transport per client (pkg/bus/kafka.go).
//
// So this is the correct condition on a multi-broker cluster, where leader
// election is not instant, and it is cheap. It is not a universal cure for
// UNKNOWN_TOPIC_OR_PARTITION, and a test that still sees one should suspect the
// transport cache before widening this timeout.
package kafkatest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// DefaultReadyTimeout bounds the wait for a freshly created topic to become
// writable.
const DefaultReadyTimeout = 30 * time.Second

// CreateTopic creates topic and does not return until every partition is present
// AND led — or until ctx or DefaultReadyTimeout expires, whichever comes first.
//
// An already-existing topic is not an error: tests reuse rigs, and a create that
// lost a race to an identical create has still produced the state the caller
// wanted. The readiness wait runs either way, because "it already existed" is not
// evidence that it is led right now.
func CreateTopic(ctx context.Context, brokers []string, topic string, partitions int) error {
	if len(brokers) == 0 {
		return errors.New("kafkatest: no brokers")
	}
	if partitions <= 0 {
		return fmt.Errorf("kafkatest: %q needs at least one partition, got %d", topic, partitions)
	}
	conn, err := kafka.Dial("tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafkatest: dial %s: %w", brokers[0], err)
	}
	defer func() { _ = conn.Close() }()

	err = conn.CreateTopics(kafka.TopicConfig{
		Topic:             topic,
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	})
	if err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return fmt.Errorf("kafkatest: create %q: %w", topic, err)
	}
	return WaitReady(ctx, brokers, topic, partitions)
}

// WaitReady blocks until topic has at least partitions partitions and every one
// of them is led, or the deadline passes.
//
// The error names what was actually observed rather than saying "timed out",
// because the two shapes it can end in have different causes and different
// fixes: no partitions at all means the create never landed, and partitions
// without leaders means election is slow or a broker is down.
func WaitReady(ctx context.Context, brokers []string, topic string, partitions int) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultReadyTimeout)
	defer cancel()

	var lastErr error
	var lastParts []kafka.Partition
	for {
		conn, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			lastErr = err
		} else {
			parts, perr := conn.ReadPartitions(topic)
			_ = conn.Close()
			if perr == nil && len(parts) >= partitions && allLed(parts) {
				return nil
			}
			lastErr, lastParts = perr, parts
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("kafkatest: %q was not writable within %s: %d partition(s) visible, "+
				"all-led=%v, last metadata error=%v. No partitions at all means the create never "+
				"landed; partitions without leaders means election is slow or a broker is down",
				topic, DefaultReadyTimeout, len(lastParts), allLed(lastParts), lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// allLed reports whether every partition has a leader backed by a real host. A
// partition carrying its own Error, or whose Leader has a zero Host, is present
// in metadata but cannot accept a produce yet — the doc on kafka.Partition.Leader
// is the authority that a zero Host means no broker is known to be serving it.
//
// An EMPTY slice is not led. Returning true for it would turn "the topic is not
// there" into "the topic is ready", which is the exact confusion this package
// exists to remove.
func allLed(parts []kafka.Partition) bool {
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p.Error != nil || p.Leader.Host == "" {
			return false
		}
	}
	return true
}
