package bus

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
)

var _ Client = (*KafkaClient)(nil)

type KafkaConfig struct {
	Brokers        []string
	ClientID       string
	PublishTimeout time.Duration

	// TLSConfig enables TLS on the broker connection (SEC-01c). For the
	// mesh the caller builds it from the workload SVID via
	// transport.ClientTLSConfig (SEC-01b). nil ⇒ plaintext (local/dev).
	// Applied to both the writer (Transport) and every reader (Dialer).
	TLSConfig *tls.Config
	// SASL is the optional SASL mechanism (e.g. SCRAM-SHA-512 via
	// kafka-go/sasl/scram, or PLAIN). nil ⇒ no SASL. Independent of TLS:
	// mTLS alone authenticates in the SPIFFE mesh, SASL is for brokers that
	// require credential-based auth (managed Kafka, cross-trust-domain).
	SASL sasl.Mechanism

	// Metrics is the optional RED/USE exporter (OBS-01c). Subscribe uses it for
	// kanz_bus_consume_halted_total — the only series that distinguishes a
	// subscription that STOPPED to avoid skipping an event from one with nothing
	// to do. Nil ⇒ no instrumentation, and the halt is then visible only as the
	// error Subscribe returns.
	Metrics *BusMetrics
}

type KafkaClient struct {
	cfg     KafkaConfig
	dialer  *kafka.Dialer // shared by readers; carries TLS + SASL
	writer  *kafka.Writer
	mu      sync.Mutex
	readers []*kafka.Reader
}

func DialKafka(cfg KafkaConfig) (*KafkaClient, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafka: at least one broker required")
	}
	if cfg.PublishTimeout == 0 {
		cfg.PublishTimeout = 5 * time.Second
	}
	return &KafkaClient{
		cfg:    cfg,
		dialer: readerDialer(cfg),
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(cfg.Brokers...),
			Balancer:               &kafka.Hash{}, // partition by Message.Key
			RequiredAcks:           kafka.RequireAll,
			Async:                  false,
			Compression:            kafka.Snappy,
			AllowAutoTopicCreation: false, // EVT-09: topics provisioned explicitly
			Transport: &kafka.Transport{
				TLS:  cfg.TLSConfig, // nil ⇒ plaintext, kafka-go's default
				SASL: cfg.SASL,
			},
		},
	}, nil
}

// readerDialer builds the dialer readers share. Returns nil when neither TLS
// nor SASL is configured so kafka-go falls back to its DefaultDialer.
func readerDialer(cfg KafkaConfig) *kafka.Dialer {
	if cfg.TLSConfig == nil && cfg.SASL == nil {
		return nil
	}
	return &kafka.Dialer{
		Timeout:       10 * time.Second,
		DualStack:     true,
		TLS:           cfg.TLSConfig,
		SASLMechanism: cfg.SASL,
	}
}

func (k *KafkaClient) Publish(ctx context.Context, msg Message) error {
	var headers []kafka.Header
	if len(msg.Headers) > 0 {
		headers = make([]kafka.Header, 0, len(msg.Headers))
		for hk, hv := range msg.Headers {
			headers = append(headers, kafka.Header{Key: hk, Value: []byte(hv)})
		}
	}
	ctx, cancel := context.WithTimeout(ctx, k.cfg.PublishTimeout)
	defer cancel()
	if err := k.writer.WriteMessages(ctx, kafka.Message{
		Topic:   msg.Subject,
		Key:     msg.Key,
		Value:   msg.Body,
		Headers: headers,
	}); err != nil {
		return fmt.Errorf("kafka publish: %w", err)
	}
	return nil
}

// Subscribe drains topic into h on the consumer group, committing each offset
// only after h has returned nil.
//
// A HANDLER ERROR ENDS THE SUBSCRIPTION. It does not skip the message and does
// not read past it — see the handler-error branch below for why the alternative
// loses events permanently. The caller gets a non-nil error naming the topic,
// partition and offset it stopped on; both Kafka callers already treat that as
// fatal to the process (services/lake-sink/cmd/lake-sink/main.go runSink,
// which cancels its sibling subscriptions and returns).
func (k *KafkaClient) Subscribe(ctx context.Context, topic, group string, h Handler) error {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        k.cfg.Brokers,
		GroupID:        group,
		Topic:          topic,
		MinBytes:       1,
		MaxBytes:       10 << 20, // 10 MiB
		CommitInterval: 0,        // manual per-message commit; lets EVT-17e own retry
		Dialer:         k.dialer, // nil ⇒ kafka-go DefaultDialer; carries TLS + SASL otherwise
	})
	k.mu.Lock()
	k.readers = append(k.readers, r)
	k.mu.Unlock()
	defer r.Close()

	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("kafka fetch: %w", err)
		}
		out := Message{Subject: m.Topic, Key: m.Key, Body: m.Value}
		if len(m.Headers) > 0 {
			headers := make(map[string]string, len(m.Headers))
			for _, hh := range m.Headers {
				headers[hh.Key] = string(hh.Value)
			}
			out.Headers = headers
		}
		if err := h(ctx, out); err != nil {
			// STOP. Do not commit, and do not read past this message.
			//
			// This branch used to `continue` on the claim that the message would be
			// "redelivered on next fetch". It is not, and #219 reproduced the loss
			// against a real broker: FetchMessage has ALREADY advanced the reader's
			// own position, so `continue` skips the message in-process, and the very
			// next successful CommitMessages writes a group offset PAST it. A
			// committed at offset 0, B skipped, C committed at offset 2 ⇒ group
			// offset 3. Re-subscribing on the same group saw only later messages; B
			// was unreachable forever — not on the next fetch, not after a rebalance,
			// not after a restart. On the archival/CDC path that is silent permanent
			// loss, and it is invisible: nothing logged, and lag reads normal because
			// the offset advanced.
			//
			// Kafka has no per-message nack. The only way to keep a message reachable
			// is to leave the group offset below it and stop consuming forward, which
			// is what returning here does — r.Close() (deferred) leaves the group, and
			// the next subscription resumes from the last COMMITTED offset, which is
			// this message. That is the same answer the NATS side already gives
			// (nats.go Nak) and the same one the archiver gives when its dead-letter
			// produce fails (archive.deadLetter NAKs rather than acking an event that
			// reached neither its topic nor the DLQ).
			//
			// WHAT ACTUALLY GETS HERE. Every bus.Consumer in the estate wires WithDLQ
			// — test/arch/bus_dlq_test.go makes that true rather than merely available
			// — so an ordinary handler failure resolves to a dead-letter publish and
			// returns nil. An error reaching this line therefore means the DEAD-LETTER
			// PATH ITSELF failed. There is nowhere left to put the event.
			//
			// CHOSEN OVER RETRYING THE DLQ PUBLISH HERE, deliberately. A bounded retry
			// only postpones this decision — when the bound is spent the same choice
			// returns — and it would be the THIRD retry layer over one publish:
			// kafka-go's Writer already retries internally, and bus.WithRetry already
			// bounds the handler above. Its cost is worse than the delay: a partition
			// stalled behind a retry loop is stalled either way, but it stalls SILENTLY
			// (no error returned, no restart, no counter) against a broker that is not
			// answering. The trade this makes is throughput for reachability — a
			// sustained dead-letter outage stops the sink, loudly, instead of quietly
			// deleting whatever it could not park.
			k.cfg.Metrics.observeConsumeHalt(m.Topic, group)
			return fmt.Errorf(
				"kafka: halting %s/%s at partition %d offset %d without committing — the handler failed and the dead-letter path did not accept it; the offset is held so the message stays reachable: %w",
				m.Topic, group, m.Partition, m.Offset, err)
		}
		if err := r.CommitMessages(ctx, m); err != nil {
			return fmt.Errorf("kafka commit: %w", err)
		}
	}
}

func (k *KafkaClient) Close() error {
	var firstErr error
	if err := k.writer.Close(); err != nil {
		firstErr = err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, r := range k.readers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
