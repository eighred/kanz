// Package bus is the Go bus client for the Kanz event backbone — uniform
// publish/subscribe over NATS JetStream (the live spine, EVT-08) and Kafka
// KRaft (the durable log of record, EVT-09).
//
// EVT-17a is the wire-level layer: connect, publish bytes, subscribe to
// bytes. EVT-17b adds envelope stamping + pre-publish validation
// (Producer / Validate / Unframe). EVT-17c adds trace / correlation /
// causation auto-propagation via ctx (Consumer + With*/From* helpers).
// EVT-17d adds dedup: Producer stamps Nats-Msg-Id for broker-side dedup,
// and Consumer holds a bounded sliding DedupWindow for in-instance
// idempotency. EVT-17e adds bounded retry (RetryConfig, exponential
// backoff, ctx-cancelable) and DLQ routing (WithDLQ — terminal failures
// republish to `dlq.<subject>` with Kanz-DLQ-* metadata, then ack).
package bus

import "context"

// Message is one wire-level event: opaque body plus transport-agnostic
// metadata. Body is typically a serialized envelope; later EVT-17 layers
// stamp the envelope before calling Publish.
type Message struct {
	// Subject is the NATS subject or Kafka topic per
	// kanz-schemas/docs/subject-taxonomy.md.
	Subject string
	// Key is the partition key. Used directly as the Kafka message key.
	// NATS has no native partition key — wrappers serialize this onto a
	// transport header and reconstruct it on the receive side.
	Key []byte
	// Body is the opaque payload — typically a serialized envelope.
	Body []byte
	// Headers is free-form transport metadata (trace ids, schema refs, …).
	Headers map[string]string
}

// Publisher publishes one message at a time, synchronously. Errors are
// transport-level (broker rejection, timeout); they do not indicate consumer
// outcomes — durability guarantees are the broker's.
type Publisher interface {
	Publish(ctx context.Context, msg Message) error
}

// Handler processes one inbound Message. Returning nil acks/commits the
// message; returning non-nil nacks (NATS) or skips commit (Kafka). The
// transport-specific redelivery semantics apply — bounded retry + DLQ
// routing layer on top in EVT-17e.
type Handler func(ctx context.Context, msg Message) error

// Subscriber binds Handler to a (subject, group). Subscribe blocks until
// ctx is canceled; one call per (subject, group). `group` is the durable
// consumer name (NATS) / consumer-group id (Kafka).
type Subscriber interface {
	Subscribe(ctx context.Context, subject, group string, h Handler) error
}

// Client is a transport that does both. NATSClient and KafkaClient each
// satisfy it; higher layers (EVT-17b–e) hold a Client rather than caring
// which transport is underneath.
type Client interface {
	Publisher
	Subscriber
	Close() error
}
