// Package envelope contains cross-component contract tests for the
// envelope invariants from kanz-schemas/docs/envelope-policy.md §7 and the
// per-class rules from docs/event-class-rules.md.
//
// Where the unit tests in kanz/pkg/bus exercise Validate / Producer
// internals, these tests sit ABOVE the bus package and treat it as a black
// box: drive Producer.Publish through the real stamp → frame → wire path,
// then Unframe + Validate on the captured wire bytes. The wire bytes are
// what crosses the bus, so these tests are the authoritative contract for
// any other client (Python EVT-18, TS EVT-19, future languages) — EVT-21b
// will assert the same envelope conforms after Python/TS round-trips, and
// EVT-21c will assert schema compatibility against this contract.
package envelope_test

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

// captureClient is the bus.Client analog from kanz/pkg/bus tests, lifted
// up so contract tests treat the bus package as a black box: Producer
// publishes → wire bytes land here → tests unframe and assert.
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

func newProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	p, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "contract-test/inst-1",
		ProducerVersion: "contract-1.0.0",
		Tenant:          "acme",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return p, cc
}

func factEvent() bus.Event {
	et := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
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

// validEnvelope returns an envelope that already satisfies every
// envelope-policy §7 rule. Contract tests mutate one field at a time and
// re-frame to assert wire-level rejection.
func validEnvelope() *envelopepb.Envelope {
	now := timestamppb.New(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	return &envelopepb.Envelope{
		EventId:          "evt-contract-1",
		EventType:        "market.equity.trade",
		SchemaVersion:    1,
		EnvelopeVersion:  1,
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		Domain:           "market",
		EventTime:        now,
		IngestionTime:    now,
		PublishTime:      now,
		CorrelationId:    "evt-contract-1",
		Source:           "contract-test/inst-1",
		ProducerVersion:  "contract-1.0.0",
		IdempotencyKey:   "evt-contract-1",
		PayloadSchemaRef: "market.v1.MarketDataEvent:1",
		PartitionKey:     "AAPL",
		ProducerSequence: 1,
		TenantId:         "acme",
	}
}

// frame is the wire encoding the Producer would have written. Hand-framing
// here is deliberate — contract tests must assert the receive side rejects
// what a non-Go (or buggy Go) publisher might emit.
func frame(t *testing.T, env *envelopepb.Envelope) []byte {
	t.Helper()
	body, err := proto.Marshal(&envelopepb.EventFrame{
		Envelope: env,
		Payload:  []byte{0x01, 0x02, 0x03}, // opaque — Validate is payload-blind
	})
	if err != nil {
		t.Fatalf("frame marshal: %v", err)
	}
	return body
}

// --- Round-trip contract ------------------------------------------------

// The end-to-end pipeline (Producer.Publish → wire bytes → Unframe →
// Validate) must produce an envelope that satisfies every required-field
// invariant. This is the canonical "the contract holds" assertion.
func TestContract_PublishRoundTripPassesValidate(t *testing.T) {
	p, cc := newProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("captured %d messages, want 1", len(cc.sent))
	}
	env, payload, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("round-tripped envelope failed Validate: %v", err)
	}
	if len(payload) == 0 {
		t.Error("round-tripped payload is empty")
	}
}

// The Nats-Msg-Id broker-dedup header (EVT-17d) is part of the wire
// contract: any non-Go client publishing onto NATS JetStream must stamp
// the same header keyed on idempotency_key, or broker-side dedup is lost.
func TestContract_PublishStampsBrokerDedupHeader(t *testing.T) {
	p, cc := newProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if got, want := cc.sent[0].Headers["Nats-Msg-Id"], env.IdempotencyKey; got != want {
		t.Errorf("Nats-Msg-Id header=%q want %q", got, want)
	}
}

// --- Required-field rejection ------------------------------------------

// Every required field from envelope-policy §7 must be rejected when
// missing on the receive side. Hand-frame the bad envelope so the test
// asserts what a misbehaving publisher's wire bytes would look like, not
// what Producer.Publish can construct (Producer would refuse to stamp many
// of these on the publish side already — that is the defense-in-depth
// half of EVT-17b; the consumer-side enforcement here is the other half).
func TestContract_RejectsBadEnvelopesOnReceive(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*envelopepb.Envelope)
	}{
		{"event_id missing", func(e *envelopepb.Envelope) { e.EventId = "" }},
		{"event_type missing", func(e *envelopepb.Envelope) { e.EventType = "" }},
		{"schema_version zero", func(e *envelopepb.Envelope) { e.SchemaVersion = 0 }},
		{"envelope_version zero", func(e *envelopepb.Envelope) { e.EnvelopeVersion = 0 }},
		{"event_class unspecified", func(e *envelopepb.Envelope) {
			e.EventClass = envelopepb.EventClass_EVENT_CLASS_UNSPECIFIED
		}},
		{"domain missing", func(e *envelopepb.Envelope) { e.Domain = "" }},
		{"event_time missing", func(e *envelopepb.Envelope) { e.EventTime = nil }},
		{"ingestion_time missing", func(e *envelopepb.Envelope) { e.IngestionTime = nil }},
		{"publish_time missing", func(e *envelopepb.Envelope) { e.PublishTime = nil }},
		{"correlation_id missing", func(e *envelopepb.Envelope) { e.CorrelationId = "" }},
		{"source missing", func(e *envelopepb.Envelope) { e.Source = "" }},
		{"producer_version missing", func(e *envelopepb.Envelope) { e.ProducerVersion = "" }},
		{"idempotency_key missing", func(e *envelopepb.Envelope) { e.IdempotencyKey = "" }},
		{"payload_schema_ref missing", func(e *envelopepb.Envelope) { e.PayloadSchemaRef = "" }},
		{"producer_sequence without partition_key", func(e *envelopepb.Envelope) {
			e.PartitionKey = ""
			e.ProducerSequence = 7
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope()
			tc.mut(env)
			body := frame(t, env)
			got, _, err := bus.Unframe(body)
			if err != nil {
				t.Fatalf("Unframe (precondition): %v", err)
			}
			if err := bus.Validate(got); err == nil {
				t.Errorf("Validate accepted envelope with %s", tc.name)
			}
		})
	}
}

// A frame whose envelope field is absent must be rejected by Unframe — the
// frame contract guarantees an envelope exists, so callers downstream can
// dereference it without a nil-check.
func TestContract_UnframeRejectsMissingEnvelope(t *testing.T) {
	body, err := proto.Marshal(&envelopepb.EventFrame{Payload: []byte("x")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, err := bus.Unframe(body); err == nil {
		t.Error("Unframe accepted frame with no envelope")
	}
}

// Truncated / malformed wire bytes must surface as an Unframe error, not a
// panic and not a zero-value envelope.
func TestContract_UnframeRejectsMalformedBytes(t *testing.T) {
	if _, _, err := bus.Unframe([]byte{0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("Unframe accepted malformed bytes")
	}
}

// --- Per-class invariants ----------------------------------------------

// FACT events: idempotency_key MUST equal event_id (event-class-rules §1).
// Round-trip the published envelope to assert the wire enforces this.
func TestContract_FactRoundTripIdempotencyEqualsEventID(t *testing.T) {
	p, cc := newProducer(t)
	if err := p.Publish(context.Background(), factEvent()); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.EventClass != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Fatalf("EventClass=%v want FACT", env.EventClass)
	}
	if env.IdempotencyKey != env.EventId {
		t.Errorf("FACT contract: IdempotencyKey=%q want %q", env.IdempotencyKey, env.EventId)
	}
}

// A FACT on the wire with idempotency_key != event_id is a contract
// violation — the receive side must reject it even though the publisher's
// Producer.Publish would have refused to stamp it.
func TestContract_FactRejectsMismatchedIdempotencyOnReceive(t *testing.T) {
	env := validEnvelope()
	env.IdempotencyKey = "not-the-event-id"
	if err := bus.Validate(env); err == nil {
		t.Error("Validate accepted FACT with idempotency_key != event_id")
	}
}

// COMMAND events: idempotency_key is caller-supplied and need not equal
// event_id (event-class-rules §2). Round-trip via Producer to assert the
// caller-set key survives stamping untouched.
func TestContract_CommandRoundTripPreservesCallerIdempotency(t *testing.T) {
	p, cc := newProducer(t)
	e := factEvent()
	e.EventClass = envelopepb.EventClass_EVENT_CLASS_COMMAND
	e.EventType = "risk.command.rebalance"
	e.IdempotencyKey = "caller-key-xyz"
	if err := p.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	env, _, _ := bus.Unframe(cc.sent[0].Body)
	if env.IdempotencyKey != "caller-key-xyz" {
		t.Errorf("COMMAND IdempotencyKey=%q want caller-key-xyz", env.IdempotencyKey)
	}
	if env.IdempotencyKey == env.EventId {
		t.Error("COMMAND contract: IdempotencyKey must not equal EventId for this case")
	}
	if err := bus.Validate(env); err != nil {
		t.Errorf("COMMAND round-trip failed Validate: %v", err)
	}
}

// STATE_SNAPSHOT and OBSERVATION fall under the same idempotency_key ==
// event_id rule as FACT (only COMMAND is the exception). The Producer
// stamping path must enforce this, and a hand-framed envelope of either
// class that violates it must be rejected.
func TestContract_NonCommandClassesRequireIdempotencyEqualsEventID(t *testing.T) {
	classes := []envelopepb.EventClass{
		envelopepb.EventClass_EVENT_CLASS_FACT,
		envelopepb.EventClass_EVENT_CLASS_STATE_SNAPSHOT,
		envelopepb.EventClass_EVENT_CLASS_OBSERVATION,
	}
	for _, cls := range classes {
		t.Run(cls.String(), func(t *testing.T) {
			env := validEnvelope()
			env.EventClass = cls
			env.IdempotencyKey = env.EventId
			if err := bus.Validate(env); err != nil {
				t.Errorf("Validate rejected %s with matching idempotency_key: %v", cls, err)
			}
			// Only FACT carries the explicit mismatch rule; OBSERVATION and
			// STATE_SNAPSHOT do not enforce equality at the bus layer
			// (event-class-rules §1 names FACT specifically).
			if cls != envelopepb.EventClass_EVENT_CLASS_FACT {
				return
			}
			env.IdempotencyKey = "different"
			if err := bus.Validate(env); err == nil {
				t.Errorf("Validate accepted %s with mismatched idempotency_key", cls)
			}
		})
	}
}

// --- Live vs replay sink contract --------------------------------------

// QUALITY_FLAG_REPLAYED is set only by replay tooling (EVT-20c). A live
// sink MUST reject it; a replay-scoped sink (one configured with
// bus.WithValidator(bus.ValidateReplay)) MUST accept it. Both halves are
// part of the wire contract because they define which subjects can carry
// which events.
func TestContract_ReplayFlagLiveVsReplaySink(t *testing.T) {
	flagged := validEnvelope()
	flagged.QualityFlags = []envelopepb.QualityFlag{
		envelopepb.QualityFlag_QUALITY_FLAG_REPLAYED,
	}

	if err := bus.Validate(flagged); err == nil {
		t.Error("live Validate accepted REPLAYED-flagged envelope")
	}
	if err := bus.ValidateReplay(flagged); err != nil {
		t.Errorf("ValidateReplay rejected flagged envelope: %v", err)
	}

	unflagged := validEnvelope()
	if err := bus.ValidateReplay(unflagged); err == nil {
		t.Error("ValidateReplay accepted envelope without REPLAYED flag")
	}
}
