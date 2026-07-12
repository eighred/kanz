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

// BroadcastSubscriber delivers a subject's LATEST message to EVERY subscriber on
// startup, and every subsequent one — an OPTIONAL capability (NATS/JetStream has it;
// Kafka's client does not implement it, and callers that need it say so).
//
// It exists because a CONTROL-PLANE BROADCAST is not a work queue, and Subscribe is
// a work queue. The halt gate is the example that cost us: it is constructed CLOSED
// and is opened only by a ModeChanged FACT, consumed through Subscribe — so
//
//   - a RESTARTED pod resumed its durable at the last ack and NEVER SAW the
//     operator's resume. It came back halted, reported /readyz 200, and answered
//     423 to every signal until a human noticed. Every rolling update was a silent
//     trading outage.
//   - and with more than one pod, the consumer GROUP load-balanced the halt: the
//     resume reached ONE replica while the other stayed closed.
//
// A broadcast consumer is ephemeral (no durable — every pod gets its own) and starts
// at DeliverLastPerSubject, so a pod that boots learns the CURRENT mode at once.
// Deny-by-default survives: if no ModeChanged FACT was ever published, nothing is
// delivered and the gate stays closed.
type BroadcastSubscriber interface {
	SubscribeBroadcast(ctx context.Context, subject string, h Handler) error
}

// Client is a transport that does both. NATSClient and KafkaClient each
// satisfy it; higher layers (EVT-17b–e) hold a Client rather than caring
// which transport is underneath.
type Client interface {
	Publisher
	Subscriber
	Close() error
}
