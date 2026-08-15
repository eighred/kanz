// THE OUTBOX'S RELAYED FACTs, DRIVEN THROUGH A REAL PRODUCER (#245).
//
// relay_test.go's recorder is an EVENT-LEVEL double that accepts any Event. It is
// the right tool for what those tests assert — SEQUENCE, head-of-line blocking,
// two drainers racing one key — and it means no test in this package had ever
// seen bus.Validate.
//
// WHAT MAKES THIS ONE DIFFERENT FROM THE OTHER #245 ENTRIES. Everywhere else the
// failure mode was an envelope the broker REFUSES. Here the dangerous one is an
// envelope the broker ACCEPTS. Record.Event()'s own comment says it:
//
//	"Leaving one empty here would not error; it would publish a FACT that
//	 validates and is simply DETACHED from the command that caused it."
//
// correlation_id, causation_id and trace_context are not validated by bus. A
// relay that dropped them would publish a stream of perfectly legal FACTs that
// no longer join back to the commands that produced them — no error, no DLQ, no
// alert, and an audit trail that cannot answer "what caused this". So Validate is
// necessary here and nowhere near sufficient, and the lineage fields are asserted
// on the wire bytes individually.
//
// Tier-B: a real bus.Producer over a fake bus.Client.
package outbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	orderpb "github.com/eighred/kanz/kanz-schemas-go/order/v1"

	"github.com/eighred/kanz/pkg/bus"
)

// captureClient records the framed wire bytes — the Tier-B helper from
// internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

// realRelayProducer mirrors the OMS's producer. NO ProducerConfig.Tenant: the
// relay's ctx belongs to the relay and carries no tenant of its own, so every
// FACT it sends must supply one from the stored Record. That is the whole reason
// Record.TenantID exists separately from the row's RLS tenant.
func realRelayProducer(t *testing.T) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "oms",
		ProducerVersion: "test",
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

// lineageFact builds a Record carrying every lineage field, the way an inbound
// command's envelope would have.
func lineageFact(t *testing.T) Record {
	t.Helper()
	ctx := bus.WithTenantID(context.Background(), testTenant)
	rec, err := From(ctx, bus.Event{
		Subject:          "order.order.cancelled",
		EventType:        "order.order.cancelled",
		EventClass:       envelopepb.EventClass_EVENT_CLASS_FACT,
		SchemaVersion:    1,
		Domain:           "order",
		EventTime:        time.Unix(1700000000, 0).UTC(),
		PartitionKey:     "o1",
		PayloadSchemaRef: "order.v1.OrderCancelled:1",
		CorrelationID:    "corr-1",
		CausationID:      "cause-1",
		Payload: &orderpb.OrderCancelled{
			OrderId:           "o1",
			CancelledQuantity: &commonpb.Decimal{Coefficient: 1, Exponent: 0},
		},
	})
	if err != nil {
		t.Fatalf("From: %v", err)
	}
	return rec
}

func drainOne(t *testing.T, rec Record) *captureClient {
	t.Helper()
	q := NewMemory()
	if err := q.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
	prod, cc := realRelayProducer(t)
	relay, err := NewRelay(q, prod, quietLogger())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	n, err := relay.DrainOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("DrainOnce = (%d, %v), want (1, nil) — the relayed FACT did not survive a real "+
			"producer, so nothing the OMS records would reach the bus", n, err)
	}
	return cc
}

// The envelope the relay actually puts on the wire survives Validate.
func TestRelayedFactIsAValidEnvelope(t *testing.T) {
	cc := drainOne(t, lineageFact(t))
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatalf("the relayed envelope fails Validate: %v", err)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetPayloadSchemaRef(); got != "order.v1.OrderCancelled:1" {
		t.Errorf("payload_schema_ref = %q", got)
	}
	if got := string(cc.sent[0].Key); got != "o1" {
		t.Errorf("partition key = %q, want the order id — consumers FOLD these, so a key that moved "+
			"reorders one order's history", got)
	}
}

// THE ONE VALIDATE CANNOT CATCH, AND IT IS WORSE THAN IT LOOKS.
//
// A FACT missing its correlation/causation is perfectly legal on the wire, and
// permanently detached from the command that caused it. Nothing reports that: no
// error, no DLQ, no alert.
//
// It does not even LOOK wrong. Measured by mutating Record.Event() to drop
// CorrelationID: the producer does not publish an empty field, it MINTS A FRESH
// UUID (stamp() generates one when the caller supplies none). So the detached
// FACT carries a plausible correlation id that joins to nothing — an empty field
// might eventually be noticed by a human reading a trace, a fabricated one never
// will.
//
// This is the assertion that would fail if Record.Event() ever stopped copying a
// lineage field, and it is the only thing that would.
func TestRelayedFactKeepsItsLineage(t *testing.T) {
	cc := drainOne(t, lineageFact(t))
	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}

	// Sanity: the envelope IS valid, so Validate cannot be what protects these.
	if err := bus.Validate(env); err != nil {
		t.Fatalf("setup: expected a valid envelope, got %v", err)
	}

	for _, c := range []struct{ field, got, want string }{
		{"correlation_id", env.GetCorrelationId(), "corr-1"},
		{"causation_id", env.GetCausationId(), "cause-1"},
		{"tenant_id", env.GetTenantId(), testTenant},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q. The envelope still VALIDATES without it — a relay that dropped "+
				"this field would publish legal FACTs that no longer join back to the commands that "+
				"caused them, with nothing reporting it", c.field, c.got, c.want)
		}
	}
}

// THE TENANT IS GUARDED AT ENQUEUE, NOT AT PUBLISH — which is EARLIER than the
// bus would catch it, and that is the point.
//
// The relay drains on its own goroutine with its own ctx, and the OMS producer
// has no Tenant fallback, so a stored record whose TenantID went missing has no
// other source and bus.Validate would refuse it. But it never gets that far: the
// queue refuses to accept it at all.
//
// That ordering matters. A tenant-less record that reached the queue would be
// durable, undeliverable, and HEAD-OF-LINE BLOCKING for its partition key — the
// relay would retry it forever and every later FACT for that order would sit
// behind it. Refusing at Append keeps the failure in the caller's hands, where
// there is still a request to fail.
//
// Record.TenantID is deliberately not the row's RLS tenant (the OMS serves
// __system__ while commands carry the caller's), which is exactly why it can go
// missing independently and why this guard is worth pinning.
func TestATenantLessRecordCannotEvenBeEnqueued(t *testing.T) {
	rec := lineageFact(t)
	rec.TenantID = "" // the field the record carries specifically for this

	q := NewMemory()
	err := q.Append(rec)
	if err == nil {
		t.Fatal("a tenant-less record was enqueued. It would be durable and undeliverable: the bus " +
			"refuses an envelope with no tenant, so the relay would retry it forever and every later " +
			"FACT for that order would block behind it")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the refusal must name the tenant; got %v", err)
	}
	if q.PendingCount() != 0 {
		t.Errorf("%d record(s) pending after a refused Append", q.PendingCount())
	}
}

// AN UNRESOLVABLE payload_schema_ref IS REFUSED, NOT SKIPPED.
//
// Record.Event() resolves the payload type from the ref via protoregistry. An
// unknown one means this relay is older than the FACTs it is being asked to
// send, and publishing the events AROUND it would hand a consumer a history with
// a hole in the middle — so it stops that partition key instead.
func TestAnUnresolvablePayloadRefIsRefusedNotSkipped(t *testing.T) {
	rec := lineageFact(t)
	rec.PayloadSchemaRef = "order.v1.ThisMessageDoesNotExist:1"

	_, err := rec.Event()
	if err == nil {
		t.Fatal("an unresolvable payload_schema_ref produced an Event. A relay older than the FACTs " +
			"it drains must STOP, not publish around the record it cannot decode")
	}
	if !errors.Is(err, ErrUnknownPayloadType) {
		t.Errorf("refused, but not as an unknown payload type: %v", err)
	}
}
