package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

var _ Client = (*KafkaClient)(nil)

type KafkaConfig struct {
	Brokers        []string
	ClientID       string
	PublishTimeout time.Duration
}

type KafkaClient struct {
	cfg     KafkaConfig
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
		cfg: cfg,
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(cfg.Brokers...),
			Balancer:               &kafka.Hash{}, // partition by Message.Key
			RequiredAcks:           kafka.RequireAll,
			Async:                  false,
			Compression:            kafka.Snappy,
			AllowAutoTopicCreation: false, // EVT-09: topics provisioned explicitly
		},
	}, nil
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

func (k *KafkaClient) Subscribe(ctx context.Context, topic, group string, h Handler) error {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        k.cfg.Brokers,
		GroupID:        group,
		Topic:          topic,
		MinBytes:       1,
		MaxBytes:       10 << 20, // 10 MiB
		CommitInterval: 0,        // manual per-message commit; lets EVT-17e own retry
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
			// No commit — message is redelivered on next fetch. Bounded retry
			// + DLQ routing land in EVT-17e.
			continue
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
