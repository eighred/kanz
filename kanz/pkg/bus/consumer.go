package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"

	"github.com/kanz-eng/kanz/pkg/observability"
)

// EventHandler processes one inbound envelope-framed event. The ctx carries
// the inbound envelope's correlation_id, the inbound event_id stashed as the
// *next* event's causation_id, and the inbound trace_context — so any
// Producer.Publish inside the handler auto-propagates lineage.
type EventHandler func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error

// Consumer wraps a Subscriber and turns its byte-level Handler into an
// envelope-aware EventHandler. Pipeline per delivery:
//
//	Unframe → Validate → dedup-check → retry-loop(handler) → dedup-record
//	             ↘                          ↘ exhausted ↘
//	               DLQ or surface              DLQ or surface
//
// DLQ (`WithDLQ`) and retry (`WithRetry`) are independent — either can be
// configured on its own. Without DLQ, terminal failures surface to the
// underlying Subscribe loop (broker redelivery). Without retry, the first
// failure is terminal.
type Consumer struct {
	subscriber Subscriber
	dlq        Publisher
	dedup      Deduper
	retry      RetryConfig
	validate   func(*envelopepb.Envelope) error
	metrics    *BusMetrics
}

type consumerOptions struct {
	dedupTTL  time.Duration
	dedupMax  int
	deduper   Deduper
	retry     RetryConfig
	dlq       Publisher
	validator func(*envelopepb.Envelope) error
	metrics   *BusMetrics
}

// ConsumerOption customizes Consumer construction.
type ConsumerOption func(*consumerOptions)

// WithDedupWindow sets the in-memory consumer-side dedup window. Default is
// 2m / 10_000 entries — the TTL matches the NATS JetStream broker-side dedup
// window (EVT-08) so the two layers reinforce. Pass ttl=0 or max=0 to
// disable dedup entirely (tests, or consumers that rely solely on
// handler-side idempotency).
func WithDedupWindow(ttl time.Duration, max int) ConsumerOption {
	return func(o *consumerOptions) {
		o.dedupTTL = ttl
		o.dedupMax = max
	}
}

// WithDeduper overrides the default in-memory dedup window with a caller-
// supplied Deduper — notably RedisDedup (DEBT-02a) for cross-pod dedup. Takes
// precedence over WithDedupWindow's ttl/max. A nil deduper is ignored (the
// in-memory default stands). The Consumer's pipeline is unchanged: it only ever
// calls Seen/Record on the Deduper, so the in-memory vs distributed choice is a
// pure wiring decision at the composition root.
func WithDeduper(d Deduper) ConsumerOption {
	return func(o *consumerOptions) {
		if d != nil {
			o.deduper = d
		}
	}
}

// WithRetry configures bounded in-handler retry. Default MaxAttempts=1 (no
// retry). Failures during the retry loop block the partition for that
// subject — long backoffs or large MaxAttempts trade throughput for
// resilience.
func WithRetry(cfg RetryConfig) ConsumerOption {
	return func(o *consumerOptions) { o.retry = cfg }
}

// WithValidator overrides the per-message envelope validator. Default is
// Validate (live-mode: hard-rejects QUALITY_FLAG_REPLAYED so a replay event
// that leaked past namespace isolation lands in the DLQ instead of being
// dispatched). Replay-scoped consumers pass WithValidator(ValidateReplay)
// so the same path accepts REPLAYED events — that is the consumer-side
// switch for EVT-20c.
func WithValidator(fn func(*envelopepb.Envelope) error) ConsumerOption {
	return func(o *consumerOptions) { o.validator = fn }
}

// WithDLQ enables DLQ routing. Terminal failures (retries exhausted, or
// unframe / validate failure before dispatch) republish the original
// `bus.Message` to `dlq.<original-subject>` with failure metadata in
// `Kanz-DLQ-*` headers, then ack the original delivery. If the DLQ publish
// itself fails, the error surfaces and the broker redelivers.
func WithDLQ(p Publisher) ConsumerOption {
	return func(o *consumerOptions) { o.dlq = p }
}

// WithBusMetrics wires the RED exporter (OBS-01c) so each delivery records
// consume rate/errors/duration. Nil ⇒ no instrumentation.
func WithBusMetrics(m *BusMetrics) ConsumerOption {
	return func(o *consumerOptions) { o.metrics = m }
}

func NewConsumer(s Subscriber, opts ...ConsumerOption) (*Consumer, error) {
	if s == nil {
		return nil, errors.New("bus: subscriber is nil")
	}
	o := consumerOptions{
		dedupTTL:  2 * time.Minute,
		dedupMax:  10_000,
		validator: Validate,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.validator == nil {
		o.validator = Validate
	}
	// A caller-supplied Deduper (WithDeduper, e.g. RedisDedup) wins; otherwise
	// the in-memory window from ttl/max. Either may be a nil-receiver no-op
	// ("disabled") — boxed in the Deduper interface it stays nil-safe, so the
	// Subscribe path needs no nil-check.
	dedup := o.deduper
	if dedup == nil {
		dedup = NewDedupWindow(o.dedupTTL, o.dedupMax)
	}
	return &Consumer{
		subscriber: s,
		dlq:        o.dlq,
		dedup:      dedup,
		retry:      o.retry.withDefaults(),
		validate:   o.validator,
		metrics:    o.metrics,
	}, nil
}

// Subscribe binds an EventHandler to (subject, group). Blocks until ctx is
// canceled.
func (c *Consumer) Subscribe(ctx context.Context, subject, group string, h EventHandler) error {
	return c.subscriber.Subscribe(ctx, subject, group, func(ctx context.Context, msg Message) error {
		env, payload, err := Unframe(msg.Body)
		if err != nil {
			c.metrics.observeConsume(subject, group, 0, err)
			return c.routeToDLQ(ctx, subject, msg, 0, fmt.Errorf("unframe: %w", err))
		}
		if err := c.validate(env); err != nil {
			c.metrics.observeConsume(subject, group, 0, err)
			return c.routeToDLQ(ctx, subject, msg, 0, fmt.Errorf("envelope validation: %w", err))
		}
		if c.dedup.Seen(env.IdempotencyKey) {
			return nil // duplicate within window — ack and skip (not a dispatch)
		}
		ctx = WithCorrelationID(ctx, env.CorrelationId)
		ctx = WithCausationID(ctx, env.EventId)
		// Stash the inbound tenant so a derived publish inside the handler
		// inherits it (MT-01b). A pre-tenancy event read on the replay path
		// carries no tenant_id; map it to SystemTenant rather than propagating
		// the empty string (MT-01a replay nuance).
		tenant := env.TenantId
		if tenant == "" {
			tenant = SystemTenant
		}
		ctx = WithTenantID(ctx, tenant)
		if env.TraceContext != "" {
			ctx = WithTraceContext(ctx, env.TraceContext)
			// Extract the inbound trace onto ctx so the consumer span (and any
			// handler-side span / log) joins the producer's trace (OBS-01d).
			ctx = observability.ContextWithTraceparent(ctx, env.TraceContext)
		}
		ctx, span := startConsumerSpan(ctx, env.EventType)
		start := time.Now()

		var lastErr error
		for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
			if err := h(ctx, env, payload); err == nil {
				c.dedup.Record(env.IdempotencyKey)
				endSpan(span, nil)
				c.metrics.observeConsume(subject, group, time.Since(start), nil)
				return nil
			} else {
				lastErr = err
			}
			if attempt < c.retry.MaxAttempts {
				if err := sleepWithCtx(ctx, c.retry.Backoff(attempt)); err != nil {
					endSpan(span, err)
					c.metrics.observeConsume(subject, group, time.Since(start), err)
					return err // ctx canceled during backoff
				}
			}
		}
		// Retries exhausted — the dispatch failed regardless of DLQ routing.
		endSpan(span, lastErr)
		c.metrics.observeConsume(subject, group, time.Since(start), lastErr)
		if c.dlq != nil {
			if err := c.publishDLQ(ctx, subject, msg, c.retry.MaxAttempts, lastErr); err != nil {
				return err
			}
			c.dedup.Record(env.IdempotencyKey) // DLQ is terminal — dedup future duplicates
			return nil
		}
		return lastErr
	})
}

// routeToDLQ is the pre-dispatch failure path (unframe/validate). attempts
// is 0 because the handler never ran. Falls through to publishDLQ when DLQ
// is configured; otherwise surfaces the error.
func (c *Consumer) routeToDLQ(ctx context.Context, origSubject string, msg Message, attempts int, dispatchErr error) error {
	if c.dlq == nil {
		return dispatchErr
	}
	return c.publishDLQ(ctx, origSubject, msg, attempts, dispatchErr)
}

func (c *Consumer) publishDLQ(ctx context.Context, origSubject string, msg Message, attempts int, dispatchErr error) error {
	dlqMsg := Message{
		Subject: dlqSubject(origSubject),
		Key:     msg.Key,
		Body:    msg.Body,
		Headers: dlqHeaders(msg.Headers, origSubject, attempts, dispatchErr),
	}
	if err := c.dlq.Publish(ctx, dlqMsg); err != nil {
		return fmt.Errorf("dlq publish: %w (original: %v)", err, dispatchErr)
	}
	return nil
}
