package cashmove_test

// THE ENVELOPE, NOT THE ENCODER (#245).
//
// cashmove had seven tests and all seven called the unexported encode(). Not one
// of them published, so nothing in this package had ever seen bus.Validate — the
// exact shape pkg/bus/producer.go's header describes: "TWELVE publish sites …
// validated fine in unit tests (which inject fake Publishers that never
// validate) and failed on the first real broker."
//
// These tests are Tier-B (the internal/risk/publish pattern): a REAL bus.Producer
// over a fake bus.Client. The fake is at the TRANSPORT level — it receives wire
// bytes — so stamping, Validate and framing all really run, and the only thing
// mocked out is the socket. A double at the Event level would have proven
// nothing, because it is precisely Validate that it skips.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/cashmove"
)

// captureClient is a bus.Client that records the framed wire bytes. It is the
// same helper shape internal/risk/publish uses; it deliberately sits BELOW the
// Producer so Validate is on the path.
type captureClient struct {
	mu   sync.Mutex
	sent []bus.Message
}

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func (c *captureClient) messages() []bus.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Message(nil), c.sent...)
}

const testTenant = "acme"

var knowledge = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// newPublisher builds the Tier-B rig: a real Producer, configured the way the
// composition root configures it (buildCashPublisher in
// services/accounting/cmd/accounting/main.go), over a capturing transport.
func newPublisher(t *testing.T, cfg bus.ProducerConfig) (*cashmove.Publisher, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, cfg)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	pub, err := cashmove.NewPublisher(prod, func() time.Time { return knowledge })
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	return pub, cc
}

func movement(kind cashmove.Kind, amount string) cashmove.CashMovement {
	return cashmove.CashMovement{
		MovementID:  "MV-1",
		PortfolioID: "PORT-1",
		Kind:        kind,
		Amount:      dec.Rat(amount),
		Currency:    "USD",
		Effective:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		SourceRef:   "transfer-agent/42",
	}
}

// A SUBSCRIPTION MUST PRODUCE AN ENVELOPE A REAL BROKER ACCEPTS.
//
// Every field asserted here is one Validate (pkg/bus/validate.go) rejects when
// absent. A missing payload_schema_ref or partition_key is not a degraded FACT,
// it is a dropped one: the ledger's input never arrives and the book is short by
// the amount an investor actually wired.
func TestPublishSubscriptionEmitsAValidFactEnvelope(t *testing.T) {
	pub, cc := newPublisher(t, bus.ProducerConfig{
		Source:          "accounting/test",
		ProducerVersion: "test",
		Tenant:          testTenant,
	})

	if err := pub.Publish(context.Background(), movement(cashmove.Subscription, "100000")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.Subject != "accounting.cash.subscription" {
		t.Errorf("Subject = %q, want accounting.cash.subscription", msg.Subject)
	}
	if string(msg.Key) != "PORT-1" {
		t.Errorf("transport Key = %q, want PORT-1", msg.Key)
	}

	env, payload, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	// The producer already ran this; re-running it here is what makes a future
	// regression in the envelope this package builds fail HERE and not on a broker.
	if err := bus.Validate(env); err != nil {
		t.Errorf("emitted envelope fails Validate: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("EventClass = %v, want FACT", env.GetEventClass())
	}
	if env.GetDomain() != "accounting" {
		t.Errorf("Domain = %q, want accounting", env.GetDomain())
	}
	if env.GetEventType() != "accounting.cash.subscription" {
		t.Errorf("EventType = %q, want accounting.cash.subscription", env.GetEventType())
	}
	if env.GetSchemaVersion() != 1 {
		t.Errorf("SchemaVersion = %d, want 1", env.GetSchemaVersion())
	}
	if env.GetPartitionKey() != "PORT-1" {
		t.Errorf("PartitionKey = %q, want PORT-1 — a book's cash movements must stay ordered", env.GetPartitionKey())
	}
	if env.GetPayloadSchemaRef() != "accounting.v1.LedgerEntry:1" {
		t.Errorf("PayloadSchemaRef = %q, want accounting.v1.LedgerEntry:1", env.GetPayloadSchemaRef())
	}
	// FACT contract (pkg/bus/validate.go).
	if env.GetIdempotencyKey() != env.GetEventId() {
		t.Errorf("IdempotencyKey = %q, want event_id %q", env.GetIdempotencyKey(), env.GetEventId())
	}
	if env.GetTenantId() != testTenant {
		t.Errorf("TenantId = %q, want %q", env.GetTenantId(), testTenant)
	}
	// NATS keys its broker-side dedup window on this header, and a redelivered
	// subscription posted twice is the fund's cash wrong by the amount.
	if got := msg.Headers["Nats-Msg-Id"]; got != env.GetIdempotencyKey() {
		t.Errorf("Nats-Msg-Id = %q, want idempotency_key %q", got, env.GetIdempotencyKey())
	}

	var le accountingpb.LedgerEntry
	if err := proto.Unmarshal(payload, &le); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if le.GetEntryId() != "cash:MV-1" {
		t.Errorf("payload EntryId = %q, want cash:MV-1", le.GetEntryId())
	}
	if le.GetPortfolioId() != "PORT-1" {
		t.Errorf("payload PortfolioId = %q", le.GetPortfolioId())
	}
	if le.GetEntryType() != accountingpb.EntryType_ENTRY_TYPE_CASH {
		t.Errorf("payload EntryType = %v, want CASH", le.GetEntryType())
	}
	if got := dec.Str(dec.FromProto(le.GetCash())); got != "100000" {
		t.Errorf("payload cash = %s, want +100000", got)
	}
	// The declared schema ref must actually describe the payload, or a consumer
	// that dispatches on it decodes the wrong message.
	if want := string(le.ProtoReflect().Descriptor().FullName()) + ":1"; env.GetPayloadSchemaRef() != want {
		t.Errorf("PayloadSchemaRef = %q does not describe the payload (%q)", env.GetPayloadSchemaRef(), want)
	}
}

// Every kind routes to its own subject, keeps its sign, and still validates —
// the subject is what binds the FACT to the ACCOUNTING stream, so a wrong one is
// a hard publish failure on a live spine (infra/nats/bootstrap-job.yaml).
func TestPublishEveryKindValidatesAndKeepsItsSign(t *testing.T) {
	cases := []struct {
		kind     cashmove.Kind
		subject  string
		entry    accountingpb.EntryType
		wantCash string
	}{
		{cashmove.Subscription, "accounting.cash.subscription", accountingpb.EntryType_ENTRY_TYPE_CASH, "100000"},
		{cashmove.Redemption, "accounting.cash.redemption", accountingpb.EntryType_ENTRY_TYPE_CASH, "-5000"},
		{cashmove.Fee, "accounting.cash.fee", accountingpb.EntryType_ENTRY_TYPE_FEE, "-250"},
	}
	amounts := []string{"100000", "5000", "250"}

	for i, c := range cases {
		pub, cc := newPublisher(t, bus.ProducerConfig{
			Source:          "accounting/test",
			ProducerVersion: "test",
			Tenant:          testTenant,
		})
		if err := pub.Publish(context.Background(), movement(c.kind, amounts[i])); err != nil {
			t.Fatalf("%s: Publish: %v", c.subject, err)
		}
		msgs := cc.messages()
		if len(msgs) != 1 {
			t.Fatalf("%s: published %d messages, want 1", c.subject, len(msgs))
		}
		if msgs[0].Subject != c.subject {
			t.Errorf("subject = %q, want %q", msgs[0].Subject, c.subject)
		}
		env, payload, err := bus.Unframe(msgs[0].Body)
		if err != nil {
			t.Fatalf("%s: Unframe: %v", c.subject, err)
		}
		if err := bus.Validate(env); err != nil {
			t.Errorf("%s: envelope fails Validate: %v", c.subject, err)
		}
		var le accountingpb.LedgerEntry
		if err := proto.Unmarshal(payload, &le); err != nil {
			t.Fatalf("%s: payload unmarshal: %v", c.subject, err)
		}
		if le.GetEntryType() != c.entry {
			t.Errorf("%s: EntryType = %v, want %v", c.subject, le.GetEntryType(), c.entry)
		}
		if got := dec.Str(dec.FromProto(le.GetCash())); got != c.wantCash {
			t.Errorf("%s: cash = %s, want %s", c.subject, got, c.wantCash)
		}
	}
}

// A REJECTED MOVEMENT MUST NOT REACH THE TRANSPORT.
//
// The HTTP handler answers 400 on a Publish error, so the caller learns. What
// must not happen is a half-emission: bytes on the wire for a movement the
// producer refused.
func TestPublishRefusesAnInvalidMovementWithoutTouchingTheTransport(t *testing.T) {
	pub, cc := newPublisher(t, bus.ProducerConfig{
		Source:          "accounting/test",
		ProducerVersion: "test",
		Tenant:          testTenant,
	})
	bad := movement(cashmove.Subscription, "100")
	bad.PortfolioID = "" // also the partition key: Validate would not catch this
	if err := pub.Publish(context.Background(), bad); err == nil {
		t.Fatal("a movement with no portfolio was published")
	}
	if n := len(cc.messages()); n != 0 {
		t.Fatalf("a refused movement put %d messages on the transport, want 0", n)
	}
}

// THE PRODUCER'S TENANT IS NOT OPTIONAL ON THIS PATH (#245, MT-01b).
//
// A cash movement is raised by an HTTP request, not by an inbound delivery, so
// there is no ctx tenant for the producer to inherit — the fallback in
// ProducerConfig.Tenant is the ONLY source. Configured without it, every
// subscription, redemption and fee is refused by Validate before it reaches the
// broker.
//
// This is the failure mode market-ingest's composition root carries a comment
// about and the OMS crash-looped on. It is asserted here as a property of the
// PACKAGE (a Publisher over a tenant-less producer emits nothing) because the
// composition root itself is not unit-testable; the wiring is held by
// TestCashPublisherIsConfiguredWithATenant in the accounting main package.
func TestPublishWithoutAProducerTenantEmitsNothing(t *testing.T) {
	pub, cc := newPublisher(t, bus.ProducerConfig{
		Source:          "accounting/test",
		ProducerVersion: "test",
		// Tenant deliberately unset — the defect this test pins.
	})
	err := pub.Publish(context.Background(), movement(cashmove.Subscription, "100000"))
	if err == nil {
		t.Fatal("expected a tenant-less producer to refuse the movement")
	}
	if !strings.Contains(err.Error(), "tenant_id") {
		t.Errorf("error = %v, want a tenant_id validation failure", err)
	}
	if n := len(cc.messages()); n != 0 {
		t.Fatalf("a refused movement put %d messages on the transport, want 0", n)
	}
}

// …and the ctx tenant satisfies it, so a caller that DOES carry one (a consumer
// re-emitting inside a delivery) is not forced onto the config fallback. This is
// the non-vacuity half of the test above: it proves the refusal is about the
// tenant and not about the movement.
func TestPublishAcceptsATenantFromTheContext(t *testing.T) {
	pub, cc := newPublisher(t, bus.ProducerConfig{
		Source:          "accounting/test",
		ProducerVersion: "test",
	})
	ctx := bus.WithTenantID(context.Background(), "beta-fund")
	if err := pub.Publish(ctx, movement(cashmove.Subscription, "100000")); err != nil {
		t.Fatalf("Publish with a ctx tenant: %v", err)
	}
	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	env, _, err := bus.Unframe(msgs[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if env.GetTenantId() != "beta-fund" {
		t.Errorf("TenantId = %q, want beta-fund", env.GetTenantId())
	}
}

// producer_sequence is per-(event_type, partition_key) and a book's cash
// movements must arrive in order. Two subscriptions on one portfolio must carry
// increasing sequences and distinct event ids — a repeated event id would be
// deduplicated by the broker and the second subscription would vanish.
func TestPublishSequencesPerPortfolio(t *testing.T) {
	pub, cc := newPublisher(t, bus.ProducerConfig{
		Source:          "accounting/test",
		ProducerVersion: "test",
		Tenant:          testTenant,
	})
	first := movement(cashmove.Subscription, "100")
	second := movement(cashmove.Subscription, "200")
	second.MovementID = "MV-2"
	for _, m := range []cashmove.CashMovement{first, second} {
		if err := pub.Publish(context.Background(), m); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	msgs := cc.messages()
	if len(msgs) != 2 {
		t.Fatalf("published %d messages, want 2", len(msgs))
	}
	e1, _, err := bus.Unframe(msgs[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	e2, _, err := bus.Unframe(msgs[1].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if e1.GetProducerSequence() != 1 || e2.GetProducerSequence() != 2 {
		t.Errorf("producer sequences = %d,%d, want 1,2", e1.GetProducerSequence(), e2.GetProducerSequence())
	}
	if e1.GetEventId() == e2.GetEventId() {
		t.Errorf("two movements share event id %q — the broker would drop the second", e1.GetEventId())
	}
}
