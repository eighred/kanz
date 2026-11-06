package natsrebuild

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"

	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/tools/replay"
)

// KafkaTopicChecker probes topic existence against the broker metadata.
//
// It reads metadata only — no consumer group, no offsets, nothing the replay
// path would disturb. Auto-creation is disabled estate-wide
// (infra/kafka/topics-job.yaml), so an unknown topic really is a topic that was
// never provisioned, not one this probe is about to create.
type KafkaTopicChecker struct {
	Brokers     []string
	DialTimeout time.Duration
	TLSConfig   *tls.Config
	SASL        sasl.Mechanism
}

// Exists reports whether topic has any partition on the cluster.
//
// The three results are kept distinct on purpose:
//
//	(true,  nil) the topic is provisioned
//	(false, nil) the broker answered, and the topic is NOT there
//	(false, err) the broker could not be asked
//
// Collapsing the last two would be the same defect one level down: a DR run
// against an unreachable broker would report every tenant's topics "missing"
// and send an operator to re-provision topics that exist.
func (c KafkaTopicChecker) Exists(ctx context.Context, topic string) (bool, error) {
	if len(c.Brokers) == 0 {
		return false, errors.New("natsrebuild: no Kafka brokers configured for the topic probe")
	}
	timeout := c.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := bus.KafkaReaderDialer(bus.KafkaConfig{TLSConfig: c.TLSConfig, SASL: c.SASL})
	if dialer == nil {
		dialer = &kafka.Dialer{}
	}
	dialer.Timeout = timeout
	conn, err := dialer.DialContext(ctx, "tcp", c.Brokers[0])
	if err != nil {
		return false, fmt.Errorf("dial broker %s: %w", c.Brokers[0], err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return false, err
	}

	parts, err := conn.ReadPartitions(topic)
	if err != nil {
		if isUnknownTopic(err) {
			return false, nil
		}
		return false, fmt.Errorf("read metadata for %q: %w", topic, err)
	}
	return len(parts) > 0, nil
}

// isUnknownTopic distinguishes "the broker says this topic does not exist" from
// every other metadata failure. Both the sentinel comparison and the errors.As
// form are checked because kafka-go returns its Error value directly on some
// paths and wrapped on others.
func isUnknownTopic(err error) bool {
	if errors.Is(err, kafka.UnknownTopicOrPartition) {
		return true
	}
	var kerr kafka.Error
	return errors.As(err, &kerr) && kerr == kafka.UnknownTopicOrPartition
}

// KafkaOpen returns an OpenFunc backed by the read-only replay.Reader (EVT-20),
// the same bounded, consumer-group-free source the single-tenant path used.
func KafkaOpen(brokers []string) OpenFunc {
	return (KafkaTopicChecker{Brokers: brokers}).Open
}

// Open uses the same identity as the metadata check. Neither path changes a
// consumer group or falls back to an unauthenticated connection after failure.
func (c KafkaTopicChecker) Open(_ context.Context, topic string, rng replay.Range) (Source, error) {
	return replay.NewReader(replay.Config{Brokers: c.Brokers, Topic: topic, Range: rng,
		TLSConfig: c.TLSConfig, SASL: c.SASL, PartitionDialTimeout: c.DialTimeout})
}
