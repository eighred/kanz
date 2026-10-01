package artifacts

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/risk/pricing/curve"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestJetStreamFactMaterializesBeforeAcknowledgement(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required for real artifact transport")
	}
	a, b := testStores(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("artifact-%d", time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, name, []string{publish.EventTypeCurveCalibrated})
	stream, err := bustest.StreamFor(ctx, js, publish.EventTypeCurveCalibrated)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: publish.EventTypeCurveCalibrated, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, name) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "risk-engine", ProducerVersion: "test", Tenant: "artifact-a"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := publish.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := curve.NewZeroCurve([]float64{1, 30}, []float64{.01234567890123456, .04345678901234567}, curve.Continuous, curve.LogLinearDF)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := publisher.EmitCalibratedCurve(ctx, "USD", at, c); err != nil {
		t.Fatal(err)
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env, body, err := bus.Unframe(msg.Data())
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	if err := b.Handler("artifact-b")(ctx, env, body); err == nil {
		t.Fatal("foreign tenant FACT accepted")
	}
	for range 2 {
		if err := a.Handler("artifact-a")(ctx, env, body); err != nil {
			t.Fatal(err)
		}
	}
	got, err := New(a.pool).CurveAt(ctx, "USD", at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Discount(7) != c.Discount(7) {
		t.Fatal("durable FACT changed the price")
	}
	if err := msg.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
