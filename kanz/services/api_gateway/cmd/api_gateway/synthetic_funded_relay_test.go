package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/outbox"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func syntheticFactRelay(t *testing.T, ctx context.Context, s syntheticSession, tenant string) (jetstream.Stream, *outbox.Relay) {
	t.Helper()
	facts, err := s.js.CreateStream(ctx, jetstream.StreamConfig{Name: "SYNTHETIC_FACTS", Subjects: []string{"order.>"}, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{Name: "oms", URL: s.broker})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	publisher, err := bus.NewProducer(client, bus.ProducerConfig{Source: "oms", ProducerVersion: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(outbox.NewPostgres(syntheticPool(t, ctx, "RELAY", tenant), outbox.ServiceOMS), publisher, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return facts, relay
}

func checkCancelledFactDelivery(t *testing.T, ctx context.Context, s syntheticSession, tenant, orderID string, cancelled *envelopepb.EventFrame) {
	t.Helper()
	facts, relay := syntheticFactRelay(t, ctx, s, tenant)
	if n, err := relay.Flush(ctx, orderID); err != nil || n != 2 {
		t.Fatalf("relay of accepted and cancelled facts published %d: %v", n, err)
	}
	if n, err := relay.Flush(ctx, orderID); err != nil || n != 0 {
		t.Fatalf("repeated relay published %d: %v", n, err)
	}
	consumer, err := facts.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: "terminal-reader", AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	proof := syntheticPool(t, ctx, "PROOF", tenant)
	for _, subject := range []string{syntheticvenue.AcceptedSubject, "order.order.cancelled"} {
		var payload []byte
		var correlation, cause, trace string
		err := proof.QueryRow(ctx, `SELECT payload,correlation_id,causation_id,trace_context FROM outbox
			WHERE partition_key=$1 AND subject=$2 AND published_at IS NOT NULL`, orderID, subject).Scan(&payload, &correlation, &cause, &trace)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := consumer.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		var frame envelopepb.EventFrame
		if err := proto.Unmarshal(msg.Data(), &frame); err != nil {
			t.Fatal(err)
		}
		env := frame.GetEnvelope()
		if err := bus.Validate(env); err != nil {
			t.Fatal(err)
		}
		if msg.Subject() != subject || env.GetEventType() != subject || env.GetEventClass() != envelopepb.EventClass_EVENT_CLASS_FACT ||
			env.GetSource() != "oms" || env.GetTenantId() != tenant || env.GetPartitionKey() != orderID ||
			env.GetCorrelationId() != correlation || env.GetCausationId() != cause || env.GetTraceContext() != trace || !bytes.Equal(frame.Payload, payload) {
			t.Fatalf("%s delivery differs from the committed OMS fact", subject)
		}
		if subject == "order.order.cancelled" && (cause != cancelled.GetEnvelope().GetEventId() || trace != cancelled.GetEnvelope().GetTraceContext()) {
			t.Fatal("terminal delivery lost its gateway cancellation identity")
		}
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.NumPending != 0 || info.NumAckPending != 0 || info.AckFloor.Consumer != 2 {
		t.Fatalf("terminal reader pending=%d unacknowledged=%d ack floor=%d", info.NumPending, info.NumAckPending, info.AckFloor.Consumer)
	}
}
