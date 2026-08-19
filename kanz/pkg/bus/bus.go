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
	// SubscribeBroadcastReady is SubscribeBroadcast, plus a signal for WHEN THE INITIAL
	// REPLAY HAS DRAINED — the moment the subscriber has seen the current state of every
	// entity on the subject and not merely started listening for the next change.
	//
	// A consumer that ARMS ITSELF from a compacted stream is empty until the replay
	// lands, and an empty state is not a neutral state: webhook-ingest's CLOSE path sizes
	// each leg from the position it believes the fund holds, so a signal arriving before
	// the replay finishes would find NO POSITION, emit ZERO orders, and answer 202
	// Accepted — the very failure the arming exists to end, just in a narrower window.
	//
	// So the arming has to be observable, and readiness has to wait for it: a pod that
	// has not learned the book must not be handed traffic. ready is called ONCE, after
	// the backlog that existed at subscribe time has been delivered AND acked.
	SubscribeBroadcastReady(ctx context.Context, subject string, h Handler, ready func()) error
}

// ReplaySubscriber delivers EVERY message a subject's stream still holds, from the
// oldest, to EVERY subscriber — and then continues live. An OPTIONAL capability, like
// BroadcastSubscriber (NATS/JetStream has it; the Kafka client does not implement it).
//
// It is the transport an APPEND LOG needs, and it is NOT BroadcastSubscriber with a
// different flag. A broadcast answers "what is the current state", so
// DeliverLastPerSubject is exactly right for it. A log answers "what happened", and its
// mutations share one subject: the model registry publishes a record_validation and the
// promotion that depends on it both on platform.model.registered, so last-per-subject
// delivers the promotion, drops the evidence, and the reader rebuilds into a state the
// MLOPS-01a gate refuses to reach.
//
// AN EMPTY REPLAY IS NOT PROOF OF AN EMPTY HISTORY. Streams have finite retention (the
// PLATFORM stream is 168h), so a fold that started from the oldest message the broker
// still holds has seen the window, not the world. A consumer that reports on what it
// replayed must say which of the two it means.
type ReplaySubscriber interface {
	// SubscribeReplay folds the subject from the oldest retained message and stays
	// subscribed. ready is called ONCE, after the backlog that existed at subscribe
	// time has been delivered AND acked — so a caller can tell "I have folded the log"
	// apart from "I have not started yet", which are the same empty registry otherwise.
	SubscribeReplay(ctx context.Context, subject string, h Handler, ready func()) error
}

// Client is a transport that does both. NATSClient and KafkaClient each
// satisfy it; higher layers (EVT-17b–e) hold a Client rather than caring
// which transport is underneath.
type Client interface {
	Publisher
	Subscriber
	Close() error
}
