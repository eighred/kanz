package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"github.com/kanz-eng/kanz/pkg/bus"
)

// oneShotSub fires Subscribe's handler exactly once with the canned message.
type oneShotSub struct {
	msg bus.Message
}

func (s *oneShotSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	return h(ctx, s.msg)
}

// multiShotSub simulates broker redelivery: invokes h with the canned
// message `times` times. Errors are recorded but do not short-circuit
// (mirroring a broker that keeps redelivering on nak).
type multiShotSub struct {
	msg   bus.Message
	times int
}

func (s *multiShotSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	var lastErr error
	for i := 0; i < s.times; i++ {
		if err := h(ctx, s.msg); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func frame(t *testing.T, env *envelopepb.Envelope, payload []byte) []byte {
	t.Helper()
	body, err := proto.Marshal(&envelopepb.EventFrame{Envelope: env, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestConsumerStashesPropagationOnContext(t *testing.T) {
	inbound := validEnvelope()
	inbound.EventId = "evt-A"
	inbound.CorrelationId = "corr-1"
	inbound.IdempotencyKey = "evt-A"
	inbound.TraceContext = "00-trace-span-01"

	sub := &oneShotSub{msg: bus.Message{Subject: "x", Body: frame(t, inbound, []byte("p"))}}
	c, err := bus.NewConsumer(sub)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	var gotCtx context.Context
	var gotEnv *envelopepb.Envelope
	var gotPayload []byte
	err = c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, env *envelopepb.Envelope, payload []byte) error {
		gotCtx, gotEnv, gotPayload = ctx, env, payload
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if gotEnv.EventId != "evt-A" {
		t.Errorf("EventId=%q", gotEnv.EventId)
	}
	if string(gotPayload) != "p" {
		t.Errorf("Payload=%q want p", gotPayload)
	}
	if bus.CorrelationIDFromContext(gotCtx) != "corr-1" {
		t.Errorf("ctx correlation=%q want corr-1", bus.CorrelationIDFromContext(gotCtx))
	}
	if bus.CausationIDFromContext(gotCtx) != "evt-A" {
		t.Errorf("ctx causation=%q want evt-A", bus.CausationIDFromContext(gotCtx))
	}
	if bus.TraceContextFromContext(gotCtx) != "00-trace-span-01" {
		t.Errorf("ctx trace=%q want 00-trace-span-01", bus.TraceContextFromContext(gotCtx))
	}
}

func TestConsumerOmitsEmptyTraceContext(t *testing.T) {
	inbound := validEnvelope()
	inbound.TraceContext = "" // hot-path sample skip

	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	c, _ := bus.NewConsumer(sub)

	var gotCtx context.Context
	_ = c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
		gotCtx = ctx
		return nil
	})
	if bus.TraceContextFromContext(gotCtx) != "" {
		t.Errorf("expected empty trace, got %q", bus.TraceContextFromContext(gotCtx))
	}
}

func TestConsumerReturnsUnframeError(t *testing.T) {
	sub := &oneShotSub{msg: bus.Message{Body: []byte("garbage")}}
	c, _ := bus.NewConsumer(sub)
	err := c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		t.Fatal("handler should not be called on unframe failure")
		return nil
	})
	if err == nil {
		t.Error("expected unframe error to surface")
	}
}

func TestConsumerReturnsValidationError(t *testing.T) {
	inbound := validEnvelope()
	inbound.EventId = "" // makes the envelope invalid

	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	c, _ := bus.NewConsumer(sub)
	err := c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		t.Fatal("handler should not be called on validation failure")
		return nil
	})
	if err == nil {
		t.Error("expected validation error to surface")
	}
}

func TestConsumerSurfacesHandlerError(t *testing.T) {
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
	c, _ := bus.NewConsumer(sub)
	sentinel := errors.New("handler failed")
	err := c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want %v", err, sentinel)
	}
}

func TestConsumerToProducerChainsLineage(t *testing.T) {
	// Inbound event A
	inbound := validEnvelope()
	inbound.EventId = "evt-A"
	inbound.CorrelationId = "corr-root"
	inbound.IdempotencyKey = "evt-A"
	inbound.TraceContext = "00-trace-span-01"

	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, []byte("a-payload"))}}
	c, _ := bus.NewConsumer(sub)

	// Producer for the handler's outbound event B
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{Source: "test/inst", ProducerVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}

	et := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	err = c.Subscribe(context.Background(), "x", "g", func(ctx context.Context, _ *envelopepb.Envelope, _ []byte) error {
		return prod.Publish(ctx, bus.Event{
			Subject:          "y",
			EventType:        "downstream.event",
			EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
			SchemaVersion:    1,
			Domain:           "market",
			EventTime:        et,
			PartitionKey:     "k",
			PayloadSchemaRef: "downstream.v1:1",
			Payload:          timestamppb.New(et),
		})
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("sent=%d want 1", len(cc.sent))
	}
	out, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if out.CorrelationId != "corr-root" {
		t.Errorf("CorrelationId=%q want corr-root (inherited)", out.CorrelationId)
	}
	if out.CausationId != "evt-A" {
		t.Errorf("CausationId=%q want evt-A (= inbound event_id)", out.CausationId)
	}
	if out.TraceContext != "00-trace-span-01" {
		t.Errorf("TraceContext=%q want 00-trace-span-01 (inherited)", out.TraceContext)
	}
}

func TestNewConsumerRejectsNil(t *testing.T) {
	if _, err := bus.NewConsumer(nil); err == nil {
		t.Error("expected error for nil subscriber")
	}
}

func TestConsumerDedupsRepeatedDelivery(t *testing.T) {
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-A"
	sub := &multiShotSub{msg: bus.Message{Body: frame(t, inbound, []byte("p"))}, times: 3}
	c, _ := bus.NewConsumer(sub)

	calls := 0
	err := c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if calls != 1 {
		t.Errorf("handler called %d times, want 1 (dedup)", calls)
	}
}

func TestConsumerDoesNotDedupOnHandlerFailure(t *testing.T) {
	// Dedup window records only after a successful dispatch, so a failing
	// handler must remain retry-able (broker keeps redelivering).
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-A"
	sub := &multiShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}, times: 3}
	c, _ := bus.NewConsumer(sub)

	calls := 0
	sentinel := errors.New("transient")
	_ = c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		calls++
		return sentinel
	})
	if calls != 3 {
		t.Errorf("handler called %d times, want 3 (no Record on failure)", calls)
	}
}

func TestConsumerWithDedupDisabled(t *testing.T) {
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-A"
	sub := &multiShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}, times: 3}
	c, _ := bus.NewConsumer(sub, bus.WithDedupWindow(0, 0))

	calls := 0
	_ = c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		calls++
		return nil
	})
	if calls != 3 {
		t.Errorf("handler called %d times, want 3 (dedup disabled)", calls)
	}
}

func TestConsumerDedupHonorsDistinctKeys(t *testing.T) {
	// Two different idempotency_keys should both dispatch; same key
	// repeated should dedup. Build two envelopes with distinct ids.
	a := validEnvelope()
	a.EventId = "evt-A"
	a.IdempotencyKey = "evt-A"
	b := validEnvelope()
	b.EventId = "evt-B"
	b.IdempotencyKey = "evt-B"

	// Deliver A, A, B in that order.
	deliveries := []bus.Message{
		{Body: frame(t, a, nil)},
		{Body: frame(t, a, nil)},
		{Body: frame(t, b, nil)},
	}
	sub := &queueSub{queue: deliveries}
	c, _ := bus.NewConsumer(sub)

	var seen []string
	err := c.Subscribe(context.Background(), "x", "g", func(_ context.Context, env *envelopepb.Envelope, _ []byte) error {
		seen = append(seen, env.EventId)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := seen, []string{"evt-A", "evt-B"}; !equalSlices(got, want) {
		t.Errorf("dispatched=%v want %v (A dedup'd, B distinct)", got, want)
	}
}

// queueSub drains a slice of messages through the handler.
type queueSub struct {
	queue []bus.Message
}

func (s *queueSub) Subscribe(ctx context.Context, _, _ string, h bus.Handler) error {
	for _, m := range s.queue {
		if err := h(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// flakyHandler fails its first `failures` calls, succeeds after.
type flakyHandler struct {
	failures int
	calls    int
}

func (f *flakyHandler) handle(_ context.Context, _ *envelopepb.Envelope, _ []byte) error {
	f.calls++
	if f.calls <= f.failures {
		return errors.New("transient")
	}
	return nil
}

func fastRetry(maxAttempts int) bus.ConsumerOption {
	return bus.WithRetry(bus.RetryConfig{
		MaxAttempts:    maxAttempts,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
}

func TestConsumerRetriesUntilSuccess(t *testing.T) {
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-A"
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	c, _ := bus.NewConsumer(sub, fastRetry(3))

	fh := &flakyHandler{failures: 2}
	if err := c.Subscribe(context.Background(), "x", "g", fh.handle); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if fh.calls != 3 {
		t.Errorf("calls=%d want 3 (fail, fail, succeed)", fh.calls)
	}
}

func TestConsumerSurfacesAfterRetryWithoutDLQ(t *testing.T) {
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
	c, _ := bus.NewConsumer(sub, fastRetry(3))

	fh := &flakyHandler{failures: 999} // always fail
	err := c.Subscribe(context.Background(), "x", "g", fh.handle)
	if err == nil {
		t.Error("expected error after retry exhaustion")
	}
	if fh.calls != 3 {
		t.Errorf("calls=%d want 3", fh.calls)
	}
}

func TestConsumerRoutesToDLQAfterRetries(t *testing.T) {
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-A"
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	dlq := &captureClient{}
	c, _ := bus.NewConsumer(sub, fastRetry(3), bus.WithDLQ(dlq))

	fh := &flakyHandler{failures: 999}
	err := c.Subscribe(context.Background(), "market.equity.trade", "g", fh.handle)
	if err != nil {
		t.Errorf("expected nil (DLQ'd), got %v", err)
	}
	if fh.calls != 3 {
		t.Errorf("calls=%d want 3", fh.calls)
	}
	if len(dlq.sent) != 1 {
		t.Fatalf("dlq.sent=%d want 1", len(dlq.sent))
	}
	dq := dlq.sent[0]
	if dq.Subject != "dlq.market.equity.trade" {
		t.Errorf("DLQ subject=%q want dlq.market.equity.trade", dq.Subject)
	}
	if dq.Headers["Kanz-DLQ-Original-Subject"] != "market.equity.trade" {
		t.Errorf("orig subject header=%q", dq.Headers["Kanz-DLQ-Original-Subject"])
	}
	if dq.Headers["Kanz-DLQ-Attempts"] != "3" {
		t.Errorf("attempts header=%q want 3", dq.Headers["Kanz-DLQ-Attempts"])
	}
	if dq.Headers["Kanz-DLQ-Error"] == "" {
		t.Error("error header empty")
	}
	// Original wire headers (incl. Nats-Msg-Id if present) are preserved.
	if _, err := bus.Unframe(dq.Body); err != nil {
		t.Errorf("DLQ body not framed: %v", err)
	}
}

func TestConsumerRoutesUnframeFailureToDLQ(t *testing.T) {
	sub := &oneShotSub{msg: bus.Message{Body: []byte("garbage")}}
	dlq := &captureClient{}
	c, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq))
	err := c.Subscribe(context.Background(), "x.y.z", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		t.Fatal("handler should not run on unframe failure")
		return nil
	})
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if len(dlq.sent) != 1 {
		t.Fatalf("dlq.sent=%d", len(dlq.sent))
	}
	if dlq.sent[0].Subject != "dlq.x.y.z" {
		t.Errorf("DLQ subject=%q", dlq.sent[0].Subject)
	}
	if dlq.sent[0].Headers["Kanz-DLQ-Attempts"] != "0" {
		t.Errorf("attempts=%q want 0 (no dispatch)", dlq.sent[0].Headers["Kanz-DLQ-Attempts"])
	}
}

func TestConsumerRoutesValidationFailureToDLQ(t *testing.T) {
	inbound := validEnvelope()
	inbound.EventId = "" // makes it invalid
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}}
	dlq := &captureClient{}
	c, _ := bus.NewConsumer(sub, bus.WithDLQ(dlq))
	err := c.Subscribe(context.Background(), "x", "g", func(context.Context, *envelopepb.Envelope, []byte) error {
		t.Fatal("handler should not run on validation failure")
		return nil
	})
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if len(dlq.sent) != 1 {
		t.Fatalf("dlq.sent=%d", len(dlq.sent))
	}
}

func TestConsumerDLQRecordsDedupOnTerminal(t *testing.T) {
	// After DLQ routing, future deliveries of the same idempotency_key
	// dedup — the message is terminally handled.
	inbound := validEnvelope()
	inbound.IdempotencyKey = "evt-poison"
	sub := &multiShotSub{msg: bus.Message{Body: frame(t, inbound, nil)}, times: 3}
	dlq := &captureClient{}
	c, _ := bus.NewConsumer(sub, fastRetry(1), bus.WithDLQ(dlq))

	fh := &flakyHandler{failures: 999}
	_ = c.Subscribe(context.Background(), "x", "g", fh.handle)

	if fh.calls != 1 {
		t.Errorf("handler calls=%d want 1 (DLQ records dedup)", fh.calls)
	}
	if len(dlq.sent) != 1 {
		t.Errorf("dlq.sent=%d want 1 (rest deduped)", len(dlq.sent))
	}
}

func TestConsumerRetryRespectsCtxCancel(t *testing.T) {
	// Long backoff with quick ctx cancel — Subscribe returns ctx.Err()
	// without finishing the retry loop.
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
	c, _ := bus.NewConsumer(sub, bus.WithRetry(bus.RetryConfig{
		MaxAttempts:    5,
		InitialBackoff: 10 * time.Second, // long
		MaxBackoff:     10 * time.Second,
	}))
	fh := &flakyHandler{failures: 999}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := c.Subscribe(ctx, "x", "g", fh.handle)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v want context.Canceled", err)
	}
	if fh.calls != 1 {
		t.Errorf("calls=%d want 1 (cancel during first backoff)", fh.calls)
	}
}

func TestConsumerDLQPublishFailureSurfaces(t *testing.T) {
	// When the DLQ publish itself fails, the error must surface so the
	// broker keeps the message (and ops can investigate).
	sub := &oneShotSub{msg: bus.Message{Body: frame(t, validEnvelope(), nil)}}
	failingDLQ := &errPublisher{err: errors.New("dlq down")}
	c, _ := bus.NewConsumer(sub, fastRetry(1), bus.WithDLQ(failingDLQ))
	fh := &flakyHandler{failures: 999}

	err := c.Subscribe(context.Background(), "x", "g", fh.handle)
	if err == nil {
		t.Error("expected error when DLQ publish fails")
	}
}

type errPublisher struct {
	err error
}

func (p *errPublisher) Publish(context.Context, bus.Message) error { return p.err }

func TestConsumerDefaultValidatorRejectsReplayed(t *testing.T) {
	// Default (live-mode) consumer treats a REPLAYED event as poison and
	// hard-rejects: with DLQ wired it routes there, without DLQ it surfaces.
	env := validEnvelope()
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	sub := &oneShotSub{msg: bus.Message{Subject: "market.equity", Body: frame(t, env, nil)}}
	c, _ := bus.NewConsumer(sub)

	dispatched := false
	err := c.Subscribe(context.Background(), "market.equity", "g",
		func(context.Context, *envelopepb.Envelope, []byte) error {
			dispatched = true
			return nil
		})
	if err == nil {
		t.Error("expected validation error for REPLAYED event on default consumer")
	}
	if dispatched {
		t.Error("REPLAYED event must not reach handler on the live path")
	}
}

func TestConsumerWithValidatorOverrideAcceptsReplayed(t *testing.T) {
	// Replay-scoped consumer wires bus.ValidateReplay; the same REPLAYED
	// event that the live path rejects now reaches the handler.
	env := validEnvelope()
	env.QualityFlags = []envelopepb.QualityFlag{envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED}
	sub := &oneShotSub{msg: bus.Message{Subject: "replay.X.market.equity.trade", Body: frame(t, env, nil)}}
	c, _ := bus.NewConsumer(sub, bus.WithValidator(bus.ValidateReplay))

	dispatched := false
	err := c.Subscribe(context.Background(), "replay.X.market.equity.trade", "g",
		func(context.Context, *envelopepb.Envelope, []byte) error {
			dispatched = true
			return nil
		})
	if err != nil {
		t.Errorf("Subscribe: %v", err)
	}
	if !dispatched {
		t.Error("replay-scoped consumer did not dispatch flagged event")
	}
}

func TestConsumerWithValidatorReplayRejectsUnflagged(t *testing.T) {
	// Replay-scoped consumer must also defend the inverse: a non-flagged
	// event on a replay subject means a broken publisher.
	sub := &oneShotSub{msg: bus.Message{Subject: "replay.X.foo", Body: frame(t, validEnvelope(), nil)}}
	c, _ := bus.NewConsumer(sub, bus.WithValidator(bus.ValidateReplay))

	dispatched := false
	err := c.Subscribe(context.Background(), "replay.X.foo", "g",
		func(context.Context, *envelopepb.Envelope, []byte) error {
			dispatched = true
			return nil
		})
	if err == nil {
		t.Error("ValidateReplay accepted an un-flagged event")
	}
	if dispatched {
		t.Error("un-flagged event must not reach a replay-scoped handler")
	}
}
