package bus

import (
	"errors"
	"math"
	"testing"
	"time"

	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestPreflightNATSFactCountsFrameAndHeadersWithoutPublishing(t *testing.T) {
	const id = "00000000-0000-7000-8000-000000000001"
	const traceparent = "00-1234567890abcdef1234567890abcdef-1234567890abcdef-01"
	at := time.Date(2020, 2, 4, 12, 0, 0, 0, time.UTC)
	payload := wrapperspb.String("quarterly comparison")
	e := Event{Subject: "accounting.custody.reconciled", EventType: "accounting.custody.reconciled",
		EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, Domain: "accounting", SchemaVersion: 1, EventTime: at,
		PartitionKey: "PF1|CUST-A", TenantID: "synthetic", PayloadSchemaRef: "google.protobuf.StringValue:1",
		CorrelationID: "run", CausationID: "operator-request", Payload: payload}
	cfg := ProducerConfig{Source: "accounting", ProducerVersion: "test"}

	// Independently encode the expected envelope and NATS header block. Router
	// subject and request/reply inbox are outside the server's MaxPayload.
	bookend := &timestamppb.Timestamp{Seconds: -1, Nanos: 999999999}
	expected := &envelopepb.Envelope{EventId: id, IdempotencyKey: id, EnvelopeVersion: 2,
		EventType: e.EventType, EventClass: e.EventClass, Domain: e.Domain, SchemaVersion: 1, EventTime: timestamppb.New(at),
		PublishTime: bookend, IngestionTime: bookend, Source: cfg.Source, ProducerVersion: cfg.ProducerVersion,
		TenantId: e.TenantID, PayloadSchemaRef: e.PayloadSchemaRef, PartitionKey: e.PartitionKey, ProducerSequence: math.MaxUint64,
		CorrelationId: e.CorrelationID, CausationId: e.CausationID, TraceContext: traceparent}
	raw, err := proto.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := proto.Marshal(&envelopepb.EventFrame{Envelope: expected, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	headers := "NATS/1.0\r\nNats-Msg-Id: " + id + "\r\nKanz-Partition-Key: " + e.PartitionKey + "\r\n\r\n"
	total := len(frame) + len(headers)

	for _, tc := range []struct {
		name     string
		limit    int
		tooLarge bool
	}{{"exact", total, false}, {"one_byte_less", total - 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, MaxPayload: int32(tc.limit)})
			if err != nil {
				t.Fatal(err)
			}
			srv.Start()
			t.Cleanup(func() { srv.Shutdown(); srv.WaitForShutdown() })
			if !srv.ReadyForConnections(5 * time.Second) {
				t.Fatal("loopback broker unavailable")
			}
			nc, err := nats.Connect(srv.ClientURL(), nats.NoReconnect())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			p, err := NewProducer(&NATSClient{conn: nc}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, persistedID := range []string{"", id} {
				fact := e
				fact.EventID = persistedID
				err := p.PreflightNATSFact(t.Context(), fact)
				if tc.tooLarge && !errors.Is(err, ErrPublishFrameTooLarge) || !tc.tooLarge && err != nil {
					t.Fatal("preflight", err)
				}
			}
			if len(p.sequence) != 0 || nc.Stats().OutMsgs != 0 {
				t.Fatal("preflight consumed a sequence or published a message")
			}
			unbound := e
			unbound.IdempotencyKey = "00000000-0000-7000-8000-000000000000"
			if err := p.PreflightNATSFact(t.Context(), unbound); err == nil || errors.Is(err, ErrPublishFrameTooLarge) {
				t.Fatal("accepted an unbound FACT idempotency key", err)
			}
			nc.Close()
			if err := p.PreflightNATSFact(t.Context(), e); !errors.Is(err, ErrPublishCapacityUnknown) {
				t.Fatal("disconnected capacity", err)
			}
		})
	}
}

func TestPreflightNATSFactPreservesOtherTransport(t *testing.T) {
	p, err := NewProducer(&KafkaClient{}, ProducerConfig{Source: "accounting", ProducerVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PreflightNATSFact(t.Context(), Event{}); err != nil {
		t.Fatal(err)
	}
}
