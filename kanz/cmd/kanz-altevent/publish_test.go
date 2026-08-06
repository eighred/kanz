// THE ENVELOPE THIS CLI PUBLISHES, PINNED AGAINST A REAL PRODUCER (#245).
//
// Before this file, nothing in the package had ever seen bus.Validate. run()
// dials a broker before it builds anything, so every field of the envelope was
// unreachable without one, and the only tests covered flag parsing and payload
// loading. That is the shape pkg/bus/producer.go records in past tense: twelve
// publish sites "validated fine in unit tests … and failed on the first real
// broker."
//
// TIER-B, not an Event-level double: a REAL bus.Producer over a FAKE bus.Client.
// The Producer is what stamps event_id / publish_time / producer_sequence and
// then runs Validate, so a double at the Event level would remove exactly the
// thing under test. Faking only the socket keeps all of it and costs a struct.
//
// WHAT IT CANNOT SHOW, so a green run is not read for more than it says: that a
// stream is bound to these subjects. That needs a real broker —
// test/arch's TestEverySubjectIsCarriedByAStream and cmd/kanz-halt's
// integration test are the two shapes that do.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	altpb "github.com/eighred/kanz/kanz-schemas-go/alternatives/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"

	alt "github.com/eighred/kanz/internal/alternatives"
	"github.com/eighred/kanz/pkg/bus"
)

// captureClient is a bus.Client that records the framed wire bytes — the Tier-B
// helper from internal/risk/publish and services/accounting/internal/cashmove.
type captureClient struct{ sent []bus.Message }

func (c *captureClient) Publish(_ context.Context, m bus.Message) error {
	c.sent = append(c.sent, m)
	return nil
}
func (c *captureClient) Subscribe(context.Context, string, string, bus.Handler) error {
	return errors.New("not implemented")
}
func (c *captureClient) Close() error { return nil }

func newTestProducer(t *testing.T, tenant string) (*bus.Producer, *captureClient) {
	t.Helper()
	cc := &captureClient{}
	prod, err := bus.NewProducer(cc, bus.ProducerConfig{
		Source:          "kanz-altevent",
		ProducerVersion: "1",
		Tenant:          tenant,
	})
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return prod, cc
}

var altEventTime = time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

func loaded(subject, schemaRef string, payload proto.Message) *loadedEvent {
	return &loadedEvent{
		subject:          subject,
		payloadSchemaRef: schemaRef,
		payload:          payload,
		eventID:          "evt-1",
		commitmentID:     "cmt-1",
		eventTime:        altEventTime,
	}
}

func commitmentEvent() *loadedEvent {
	return loaded(alt.SubjectCommitted, "alternatives.v1.Commitment:1",
		&altpb.Commitment{CommitmentId: "cmt-1"})
}

// THE ONE THAT MATTERS. Every field asserted here is one Validate rejects when
// it is wrong or absent, so this is the test that would fail if this CLI carried
// cashmove's defect.
func TestAltEventPublishesAValidFactEnvelope(t *testing.T) {
	prod, cc := newTestProducer(t, "eighred")
	opt := options{tenant: "eighred", kind: "commit"}

	if err := prod.Publish(context.Background(), altEvent(opt, commitmentEvent())); err != nil {
		t.Fatalf("Publish: %v — this CLI's envelope does not survive bus.Validate, which is the "+
			"failure an operator would meet the first time they used it", err)
	}
	if len(cc.sent) != 1 {
		t.Fatalf("published %d messages, want 1", len(cc.sent))
	}

	env, _, err := bus.Unframe(cc.sent[0].Body)
	if err != nil {
		t.Fatalf("Unframe: %v", err)
	}
	// Re-run Validate on the UNFRAMED envelope. Publish already ran it; asserting
	// here is what makes a future change to either side visible.
	if err := bus.Validate(env); err != nil {
		t.Errorf("the emitted envelope fails Validate: %v", err)
	}

	if got := env.GetTenantId(); got != "eighred" {
		t.Errorf("tenant_id = %q, want eighred — a missing tenant is what the bus rejects first, and "+
			"it is how the accounting cash producer was broken in production", got)
	}
	if got := env.GetEventClass(); got != envelopepb.EventClass_EVENT_CLASS_FACT {
		t.Errorf("event_class = %v, want FACT", got)
	}
	if got := env.GetPayloadSchemaRef(); got != "alternatives.v1.Commitment:1" {
		t.Errorf("payload_schema_ref = %q", got)
	}
	if got := env.GetDomain(); got != alt.Domain {
		t.Errorf("domain = %q, want %q", got, alt.Domain)
	}
	// FACT rule (pkg/bus/validate.go): idempotency_key == event_id.
	if env.GetIdempotencyKey() != env.GetEventId() {
		t.Errorf("idempotency_key %q != event_id %q — the FACT rule Validate enforces",
			env.GetIdempotencyKey(), env.GetEventId())
	}
	// The event's OWN date, never ingest time: IRR/TVPI are computed from it.
	if got := env.GetEventTime().AsTime(); !got.Equal(altEventTime) {
		t.Errorf("event_time = %s, want the event's own dated timestamp %s — stamping ingest time "+
			"corrupts the return silently rather than failing loudly", got, altEventTime)
	}
	// The commitment_id orders a capital call against the distribution and NAV
	// mark that follow it.
	if got := string(cc.sent[0].Key); got != "cmt-1" {
		t.Errorf("partition key = %q, want the commitment_id", got)
	}
}

// A TENANT-LESS RUN IS REFUSED BY THE BUS, and this pins that it is refused
// rather than published blank. That is exactly the defect which reached
// production in services/accounting: the producer had no tenant, every publish
// was rejected, and no test noticed because none of them published.
func TestAltEventWithNoTenantIsRefused(t *testing.T) {
	prod, cc := newTestProducer(t, "")

	err := prod.Publish(context.Background(), altEvent(options{tenant: ""}, commitmentEvent()))
	if err == nil {
		t.Fatal("a tenant-less alt event was published. On the live spine bus.Validate refuses it, so " +
			"this CLI would report success while the FACT never left")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("the refusal must name the tenant; got %v", err)
	}
	if len(cc.sent) != 0 {
		t.Errorf("a refused envelope still reached the transport (%d message(s))", len(cc.sent))
	}
}

// EVERY KIND, not just the first. The four differ in subject and
// payload_schema_ref — the two fields a wrong -kind corrupts — and a per-kind
// envelope defect would otherwise hide behind the commitment case. This is an
// append-only journal: a bad envelope that reaches the wire stays there.
func TestEveryAltEventKindProducesAValidEnvelope(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		subject   string
		schemaRef string
		payload   proto.Message
	}{
		{"commit", alt.SubjectCommitted, "alternatives.v1.Commitment:1",
			&altpb.Commitment{CommitmentId: "cmt-1"}},
		{"call", alt.SubjectCalled, "alternatives.v1.CapitalCall:1",
			&altpb.CapitalCall{CommitmentId: "cmt-1", CallId: "call-1"}},
		{"distribution", alt.SubjectDistributed, "alternatives.v1.Distribution:1",
			&altpb.Distribution{CommitmentId: "cmt-1", DistributionId: "dist-1"}},
		{"navmark", alt.SubjectMarked, "alternatives.v1.NAVMark:1",
			&altpb.NAVMark{CommitmentId: "cmt-1", MarkId: "mark-1"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			prod, cc := newTestProducer(t, "eighred")
			ev := loaded(tc.subject, tc.schemaRef, tc.payload)

			if err := prod.Publish(context.Background(), altEvent(options{tenant: "eighred"}, ev)); err != nil {
				t.Fatalf("Publish %s: %v", tc.kind, err)
			}
			env, _, err := bus.Unframe(cc.sent[0].Body)
			if err != nil {
				t.Fatalf("Unframe: %v", err)
			}
			if err := bus.Validate(env); err != nil {
				t.Errorf("%s envelope fails Validate: %v", tc.kind, err)
			}
			if got := env.GetEventType(); got != tc.subject {
				t.Errorf("event_type = %q, want %q", got, tc.subject)
			}
			if got := env.GetPayloadSchemaRef(); got != tc.schemaRef {
				t.Errorf("payload_schema_ref = %q, want %q — a wrong ref sends the consumer to the "+
					"wrong decoder for bytes it will then misread rather than reject", got, tc.schemaRef)
			}
			// All four key on the commitment, never on their own id: keying on the
			// event's id would let the bus interleave one commitment's events with
			// another's, and the fold cannot detect that after the fact.
			if got := string(cc.sent[0].Key); got != "cmt-1" {
				t.Errorf("%s partition key = %q, want the commitment_id", tc.kind, got)
			}
		})
	}
}
