package store_test

// THE ENVELOPE, NOT THE ENCODER (#410).
//
// The override FACT's other tests build a bus.Event and inspect it. Not one of
// them publishes, so none has ever seen bus.Validate — the exact shape
// pkg/bus/producer.go's header describes: "TWELVE publish sites … validated fine
// in unit tests (which inject fake Publishers that never validate) and failed on
// the first real broker."
//
// That matters more here than it usually would. The enqueue happens INSIDE the
// override transaction, so a FACT the producer refuses does not merely fail to
// publish — it fails the override, and every override, on the durable path. And
// it fails at the relay rather than at the enqueue, so the first symptom would
// be a backlog that climbs with no bad input to point at.
//
// This is Tier-B: a REAL bus.Producer over a fake bus.Client. The fake is at the
// TRANSPORT level — it receives wire bytes — so stamping, Validate and framing
// all really run, and the only thing mocked out is the socket.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	masterpb "github.com/eighred/kanz/kanz-schemas-go/master/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/outbox"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/datamaster/internal/pricing"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

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

const pubTenant = "acme"

var pubAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// publishOverride runs the FACT through the SAME path production uses: the event
// builder, the outbox record, and a real Producer.
func publishOverride(t *testing.T, o pricing.Override) (*captureClient, error) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source: "datamaster/test", ProducerVersion: "test", Tenant: pubTenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	// Built by the store's own exported surface so this test cannot drift from
	// what PostgresExceptions.Override enqueues.
	ex := pricing.Exception{
		ID: "INST1:PRICE_TOLERANCE:ICE", InstrumentID: "INST1", Status: pricing.StatusOverridden,
	}
	ev, err := store.OverrideEventForTest(ex, o)
	if err != nil {
		return cc, err
	}
	ev.TenantID = pubTenant
	rec, err := outbox.From(context.Background(), ev)
	if err != nil {
		return cc, err
	}
	// THE RELAY PUBLISHES FROM THE RECORD, not from the event. Rebuilding here is
	// what the relay does, so a payload that survives the struct but not the
	// bytes is caught.
	replay, err := rec.Event()
	if err != nil {
		return cc, err
	}
	return cc, prod.Publish(context.Background(), replay)
}

func dualSignedOverride() pricing.Override {
	return pricing.Override{
		Actor: "alice@kanz", Approver: "bob@kanz", Reason: "vendor confirmed after corp action",
		ChosenPrice: dec.Rat("130.25"), At: pubAt,
	}
}

// THE OVERRIDE FACT MUST PRODUCE AN ENVELOPE A REAL BROKER ACCEPTS.
//
// Every field asserted here is one bus.Validate rejects when absent. A missing
// payload_schema_ref or partition_key is not a degraded FACT — it is a publish
// error inside the relay, and the audit trail silently stops advancing.
func TestPublishOverrideEmitsAValidFactEnvelope(t *testing.T) {
	cc, err := publishOverride(t, dualSignedOverride())
	if err != nil {
		t.Fatalf("a real Producer REFUSED the override FACT: %v", err)
	}
	msgs := cc.messages()
	if len(msgs) != 1 {
		t.Fatalf("published %d messages, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.Subject != "data.exception.overridden" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	// The transport key is what preserves per-exception order on a partitioned log.
	if string(msg.Key) != "INST1:PRICE_TOLERANCE:ICE" {
		t.Errorf("transport Key = %q, want the exception id", msg.Key)
	}

	// Unframe is what a consumer does, so this asserts the shape a consumer sees
	// rather than the shape the producer meant.
	env, raw, err := bus.Unframe(msg.Body)
	if err != nil {
		t.Fatalf("the framed body does not unframe: %v", err)
	}
	if env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event class = %v, want FACT", env.GetEventClass())
	}
	if env.GetTenantId() != pubTenant {
		t.Errorf("tenant = %q, want %q — an untenanted FACT is refused by the broker", env.GetTenantId(), pubTenant)
	}
	if env.GetPayloadSchemaRef() == "" {
		t.Error("payload_schema_ref is empty")
	}
	if env.GetEventId() == "" {
		t.Error("event_id is empty")
	}

	// AND BOTH IDENTITIES SURVIVE ALL THE WAY TO THE WIRE BYTES. Everything above
	// can be right while the payload says one person made the decision.
	var payload masterpb.ExceptionOverridden
	if err := proto.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload does not unmarshal: %v", err)
	}
	if payload.GetOverride().GetActor() != "alice@kanz" {
		t.Errorf("actor on the wire = %q", payload.GetOverride().GetActor())
	}
	if payload.GetOverride().GetApprover() != "bob@kanz" {
		t.Errorf("approver on the wire = %q — #410 requires the FACT to carry BOTH identities",
			payload.GetOverride().GetApprover())
	}
	if got := dec.FromProto(payload.GetOverride().GetChosenPrice()); got.Cmp(dec.Rat("130.25")) != 0 {
		t.Errorf("price on the wire = %s, want 130.25 exactly", got.RatString())
	}
}

// A SINGLE-SIGNED OVERRIDE ALSO PRODUCES A VALID ENVELOPE.
//
// It is the common case today, and an empty approver must not make the FACT
// unpublishable — that would mean the outbox filled with records the relay could
// never drain, on exactly the overrides that get the least scrutiny.
func TestPublishSingleSignedOverrideIsAlsoValid(t *testing.T) {
	o := dualSignedOverride()
	o.Approver = ""
	cc, err := publishOverride(t, o)
	if err != nil {
		t.Fatalf("a real Producer refused a single-signed override FACT: %v", err)
	}
	if len(cc.messages()) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.messages()))
	}
}
