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

type captureClient struct {
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newTestProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	p, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "test-svc/inst-1",
		ProducerVersion: "test-1.0.0",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p, cc
}

func factEvent() bus.Event {
	et := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	return bus.Event{
		Subject:          "market.equity.trade",
		EventType:        "market.equity.trade",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "market",
		EventTime:        et,
		PartitionKey:     "AAPL",
		PayloadSchemaRef: "market.v1.MarketDataEvent:1",
		Payload:          timestamppb.New(et),
	}
}

func TestProducerStampsBrokerDedupHeader(t *testing.T) {
	p, cc := newTestProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	sent := cc.sent[0]
	env, _, _ := bus.Unframe(sent.Body)
	if env.IdempotencyKey == "" {
		t.Fatal("envelope has empty IdempotencyKey")
	}
	if got := sent.Headers["Nats-Msg-Id"]; got != env.IdempotencyKey {
		t.Errorf("Nats-Msg-Id header=%q want %q (broker-side dedup key)", got, env.IdempotencyKey)
	}
}

func TestProducerStampsFactEvent(t *testing.T) {
	p, cc := newTestProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := len(cc.sent); got != 1 {
		t.Fatalf("client received %d messages, want 1", got)
	}
	sent := cc.sent[0]
	if sent.Subject != "market.equity.trade" {
		t.Errorf("Subject=%q", sent.Subject)
	}
	if string(sent.Key) != "AAPL" {
		t.Errorf("Key=%q", sent.Key)
	}
	env, payloadBytes, err := bus.Unframe(sent.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if env.EventId == "" {
		t.Error("event_id not stamped")
	}
	if env.CorrelationId != env.EventId {
		t.Errorf("CorrelationId=%q want %q (root event)", env.CorrelationId, env.EventId)
	}
	if env.IdempotencyKey != env.EventId {
		t.Errorf("IdempotencyKey=%q want %q (FACT)", env.IdempotencyKey, env.EventId)
	}
	if env.EnvelopeVersion != 1 {
		t.Errorf("EnvelopeVersion=%d want 1", env.EnvelopeVersion)
	}
	if env.Source != "test-svc/inst-1" {
		t.Errorf("Source=%q", env.Source)
	}
	if env.ProducerVersion != "test-1.0.0" {
		t.Errorf("ProducerVersion=%q", env.ProducerVersion)
	}
	if env.ProducerSequence != 1 {
		t.Errorf("ProducerSequence=%d want 1", env.ProducerSequence)
	}
	if env.PublishTime == nil || env.IngestionTime == nil {
		t.Error("PublishTime / IngestionTime not stamped")
	}
	want := timestamppb.New(time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	got := &timestamppb.Timestamp{}
	if err := proto.Unmarshal(payloadBytes, got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if !proto.Equal(got, want) {
		t.Errorf("payload mismatch: %v vs %v", got, want)
	}
}

func TestProducerPropagatesCausation(t *testing.T) {
	p, cc := newTestProducer(t)
	e := factEvent()
	e.CorrelationID = "root-corr-id"
	e.CausationID = "parent-event-id"
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.CorrelationId != "root-corr-id" {
		t.Errorf("CorrelationId=%q want root-corr-id", env.CorrelationId)
	}
	if env.CausationId != "parent-event-id" {
		t.Errorf("CausationId=%q want parent-event-id", env.CausationId)
	}
}

func TestProducerSequenceIncrementsPerPartitionKey(t *testing.T) {
	p, cc := newTestProducer(t)
	for _, pk := range []string{"AAPL", "AAPL", "MSFT", "AAPL"} {
		e := factEvent()
		e.PartitionKey = pk
		if err := p.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	want := []uint64{1, 2, 1, 3} // AAPL,AAPL,MSFT,AAPL
	for i, m := range cc.sent {
		env, _, _ := bus.Unframe(m.Body)
		if env.ProducerSequence != want[i] {
			t.Errorf("seq[%d]=%d want %d (key=%s)", i, env.ProducerSequence, want[i], env.PartitionKey)
		}
	}
}

func TestProducerSequenceZeroWithoutPartitionKey(t *testing.T) {
	p, cc := newTestProducer(t)
	e := factEvent()
	e.PartitionKey = ""
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.ProducerSequence != 0 {
		t.Errorf("ProducerSequence=%d want 0 (no partition_key)", env.ProducerSequence)
	}
}

func TestProducerCommandRequiresIdempotencyKey(t *testing.T) {
	p, _ := newTestProducer(t)
	e := factEvent()
	e.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	e.EventType = "risk.command.rebalance"
	e.IdempotencyKey = "" // missing on purpose
	if err := p.Publish(context.Background(), e); err == nil {
		t.Fatal("expected error for COMMAND without idempotency_key")
	}
}

func TestProducerCommandPreservesIdempotencyKey(t *testing.T) {
	p, cc := newTestProducer(t)
	e := factEvent()
	e.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	e.IdempotencyKey = "caller-key-123"
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.IdempotencyKey != "caller-key-123" {
		t.Errorf("IdempotencyKey=%q want caller-key-123", env.IdempotencyKey)
	}
	if env.IdempotencyKey == env.EventId {
		t.Error("COMMAND idempotency_key should not equal event_id")
	}
}

func TestProducerRejectsMismatchedIdempotencyKeyForFact(t *testing.T) {
	p, _ := newTestProducer(t)
	e := factEvent()
	e.IdempotencyKey = "not-the-event-id"
	if err := p.Publish(context.Background(), e); err == nil {
		t.Fatal("expected error for FACT with mismatched idempotency_key")
	}
}

func TestProducerRejectsZeroEventTime(t *testing.T) {
	p, _ := newTestProducer(t)
	e := factEvent()
	e.EventTime = time.Time{}
	if err := p.Publish(context.Background(), e); err == nil {
		t.Fatal("expected error for zero EventTime")
	}
}

func TestProducerRejectsNilPayload(t *testing.T) {
	p, _ := newTestProducer(t)
	e := factEvent()
	e.Payload = nil
	if err := p.Publish(context.Background(), e); err == nil {
		t.Fatal("expected error for nil Payload")
	}
}

func TestProducerInheritsLineageFromContext(t *testing.T) {
	p, cc := newTestProducer(t)
	ctx := context.Background()
	ctx = bus.WithCorrelationID(ctx, "corr-from-ctx")
	ctx = bus.WithCausationID(ctx, "parent-evt")
	ctx = bus.WithTraceContext(ctx, "00-ctx-trace")

	if err := p.Publish(ctx, factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.CorrelationId != "corr-from-ctx" {
		t.Errorf("CorrelationId=%q want corr-from-ctx", env.CorrelationId)
	}
	if env.CausationId != "parent-evt" {
		t.Errorf("CausationId=%q want parent-evt", env.CausationId)
	}
	if env.TraceContext != "00-ctx-trace" {
		t.Errorf("TraceContext=%q want 00-ctx-trace", env.TraceContext)
	}
}

func TestProducerExplicitFieldsBeatContext(t *testing.T) {
	p, cc := newTestProducer(t)
	ctx := bus.WithCorrelationID(context.Background(), "corr-from-ctx")
	ctx = bus.WithCausationID(ctx, "ctx-causation")
	ctx = bus.WithTraceContext(ctx, "ctx-trace")

	e := factEvent()
	e.CorrelationID = "explicit-corr"
	e.CausationID = "explicit-causation"
	e.TraceContext = "explicit-trace"

	if err := p.Publish(ctx, e); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.CorrelationId != "explicit-corr" {
		t.Errorf("CorrelationId=%q want explicit-corr (explicit beats ctx)", env.CorrelationId)
	}
	if env.CausationId != "explicit-causation" {
		t.Errorf("CausationId=%q want explicit-causation", env.CausationId)
	}
	if env.TraceContext != "explicit-trace" {
		t.Errorf("TraceContext=%q want explicit-trace", env.TraceContext)
	}
}

func TestProducerRootEventCorrelationDefaultsToEventID(t *testing.T) {
	// No ctx fields, no Event.CorrelationID — root event, correlation = self.
	p, cc := newTestProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.CorrelationId != env.EventId {
		t.Errorf("root CorrelationId=%q want %q", env.CorrelationId, env.EventId)
	}
	if env.CausationId != "" {
		t.Errorf("root CausationId=%q want empty", env.CausationId)
	}
}

func TestNewProducerRequiresSourceAndVersion(t *testing.T) {
	if _, err := bus.NewProducer(&captureClient{}, bus.ProducerConfig{}); err == nil {
		t.Error("expected error for empty config")
	}
	if _, err := bus.NewProducer(&captureClient{}, bus.ProducerConfig{Source: "x"}); err == nil {
		t.Error("expected error for missing ProducerVersion")
	}
	if _, err := bus.NewProducer(nil, bus.ProducerConfig{Source: "x", ProducerVersion: "v"}); err == nil {
		t.Error("expected error for nil client")
	}
}
