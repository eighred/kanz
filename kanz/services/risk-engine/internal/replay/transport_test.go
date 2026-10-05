package replay

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/publish"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func TestPublishedRiskManifestSurvivesRestart(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required for real artifact transport")
	}
	a, _ := testRepositories(t)
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
	bustest.EnsureSubjects(t, ctx, js, name, []string{publish.EventTypeMeasuresComputed})
	stream, err := bustest.StreamFor(ctx, js, publish.EventTypeMeasuresComputed)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: publish.EventTypeMeasuresComputed, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
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

	source := new(datedSource)
	registry := compute.DefaultRegistry()
	varmodel.Register(ctx, registry, Returns{Source: source}, varmodel.Config{Confidence: .975, Window: 17})
	evaluator, err := New(registry, a, "transport-proof")
	if err != nil {
		t.Fatal(err)
	}
	p := testPortfolio()
	result, err := evaluator.Compute(ctx, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.EmitMeasures(ctx, result.Measures, nil); err != nil {
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
	wire := new(domainpb.RiskMeasureSet)
	if err := proto.Unmarshal(body, wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Measures) == 0 {
		t.Fatal("empty published calculation")
	}
	retained, err := NewPostgres(a.pool).Load(ctx, p.ID(), evaluationKnowledge(t, a))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range wire.Measures {
		if m.GetProvenance().GetInputManifestDigest() != retained.Digest {
			t.Fatal("published result lost its retained input identity")
		}
	}
	source.changed = true
	restarted, err := New(compute.DefaultRegistry(), NewPostgres(a.pool), "new-build")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Replay(ctx, p.ID(), evaluationKnowledge(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(wire, publish.ToProtoMeasureSet(replayed.Measures, nil)) {
		t.Fatal("restarted calculation differs from published FACT")
	}
	if err := msg.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
