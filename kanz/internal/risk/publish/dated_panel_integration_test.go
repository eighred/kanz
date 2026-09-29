package publish_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/risk/publish"
	factorpb "github.com/eighred/kanz/kanz-schemas-go/factor/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestDatedPanelProvenanceSurvivesRealJetStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	suffix := fmt.Sprint(time.Now().UnixNano())
	bustest.EnsureSubjects(t, ctx, js, "DATED_FACTOR_"+suffix, []string{publish.EventTypeFactorModelFitted})
	stream, err := bustest.StreamFor(ctx, js, publish.EventTypeFactorModelFitted)
	if err != nil {
		t.Fatal(err)
	}
	name := "dated-" + suffix
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: publish.EventTypeFactorModelFitted, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), stream, name) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "risk-engine", ProducerVersion: "test", Tenant: "dated-panel"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := publish.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}
	model := fittedModel(t, baseTime)
	if model.InputProvenance["panel_digest"] == "" {
		t.Fatal("fit produced no dated provenance")
	}
	if err := publisher.EmitFactorModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	msg, err := consumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env, payload, err := bus.Unframe(msg.Data())
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Validate(env); err != nil {
		t.Fatal(err)
	}
	var snapshot factorpb.FactorModelSnapshot
	if err := proto.Unmarshal(payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(snapshot.InputProvenance, model.InputProvenance) {
		t.Fatalf("durable artifact lost panel provenance: %v", snapshot.InputProvenance)
	}
	if err := msg.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
