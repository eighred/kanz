package archive_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	envelopepb "github.com/kanz-eng/kanz-schemas-go/envelope/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kanz-eng/kanz/pkg/bus"
	"github.com/kanz-eng/kanz/services/archiver/internal/archive"
)

type fakeKafka struct {
	got  []bus.Message
	fail error
}

func (f *fakeKafka) Publish(_ context.Context, m bus.Message) error {
	if f.fail != nil {
		return f.fail
	}
	f.got = append(f.got, m)
	return nil
}

// body frames e the same way bus.Producer does on publish — an EventFrame
// wrapping the envelope — since that is what bus.Unframe expects to decode.
func body(t *testing.T, e *envelopepb.Envelope) []byte {
	t.Helper()
	b, err := proto.Marshal(&envelopepb.EventFrame{Envelope: e})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newArchiver(k archive.Publisher) *archive.Archiver {
	return archive.New(archive.Config{
		Tenant: "acme",
		Group:  "archiver",
		Kafka:  k,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestHandle_ProducesVerbatimToTheMappedTopic(t *testing.T) {
	e := &envelopepb.Envelope{
		EventType:    "order.order.submitted",
		TenantId:     "acme",
		EventId:      "evt-1",
		PartitionKey: "portfolio-7",
		EventClass:   envelopepb.EventClass_EVENT_CLASS_FACT,
	}
	raw := body(t, e)
	k := &fakeKafka{}

	if err := newArchiver(k).Handle(context.Background(), bus.Message{Subject: "order.order.submitted", Body: raw}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(k.got) != 1 {
		t.Fatalf("published %d messages, want 1", len(k.got))
	}
	m := k.got[0]
	if m.Subject != "acme.order.order" {
		t.Errorf("topic = %q, want %q", m.Subject, "acme.order.order")
	}
	// The KEY is what preserves per-entity order across partitions.
	if string(m.Key) != "portfolio-7" {
		t.Errorf("key = %q, want %q", m.Key, "portfolio-7")
	}
	// VERBATIM: the archiver is a transport, not a producer.
	if string(m.Body) != string(raw) {
		t.Error("body was rewritten — the archiver must never re-stamp the log of record")
	}
	if m.Headers["Kanz-Event-Id"] != "evt-1" {
		t.Errorf("Kanz-Event-Id = %q, want evt-1 (the downstream dedup key)", m.Headers["Kanz-Event-Id"])
	}
}

// THE CENTRAL GUARANTEE. A failed produce must NACK, so NATS redelivers. Acking a
// message we failed to archive is silent, permanent data loss — the exact bug this
// service exists to end.
func TestHandle_KafkaFailureNacks(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "acme", EventId: "evt-1"}
	k := &fakeKafka{fail: errors.New("kafka down")}

	err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)})
	if err == nil {
		t.Fatal("Handle returned nil on a failed produce — NATS would ACK and the event would be LOST FOREVER")
	}
}

// An unmappable event must NACK too, never fall back to a default topic.
func TestHandle_UnmappableNacksAndPublishesNothing(t *testing.T) {
	e := &envelopepb.Envelope{EventType: "order.order.submitted", TenantId: "someone-else", EventId: "evt-1"}
	k := &fakeKafka{}

	if err := newArchiver(k).Handle(context.Background(), bus.Message{Body: body(t, e)}); err == nil {
		t.Fatal("cross-tenant event was accepted — expected a refusal")
	}
	if len(k.got) != 0 {
		t.Fatalf("published %d messages for an unmappable event, want 0", len(k.got))
	}
}

func TestHandle_UndecodableBodyNacks(t *testing.T) {
	k := &fakeKafka{}
	if err := newArchiver(k).Handle(context.Background(), bus.Message{Body: []byte("not a protobuf envelope")}); err == nil {
		t.Fatal("garbage body was accepted — expected a refusal")
	}
}
