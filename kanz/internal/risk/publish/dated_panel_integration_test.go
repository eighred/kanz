package publish_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
	varmodel "github.com/eighred/kanz/internal/risk/compute/var"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
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
	// The portfolio's audit digest and excluded money also cross the real
	// boundary in dedicated typed fields, never the grouping-parameter map.
	bustest.EnsureSubjects(t, ctx, js, "DATED_MEASURES_"+suffix, []string{publish.EventTypeMeasuresComputed})
	measureStream, err := bustest.StreamFor(ctx, js, publish.EventTypeMeasuresComputed)
	if err != nil {
		t.Fatal(err)
	}
	measureConsumer, err := js.CreateConsumer(ctx, measureStream, jetstream.ConsumerConfig{Name: name + "-measures", FilterSubject: publish.EventTypeMeasuresComputed, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(context.Background(), measureStream, name+"-measures") }()
	prices := store.NewMemory()
	for i, value := range []int64{100, 90, 100} {
		at := baseTime.Add(time.Duration(i-2) * 24 * time.Hour)
		if err := prices.Put(ctx, []store.Observation{{InstrumentID: "A", ObservationTime: at, KnowledgeTime: at, Kind: store.PriceKindClose, Price: &commonpb.Decimal{Coefficient: value}}}); err != nil {
			t.Fatal(err)
		}
	}
	portfolio := domain.NewPortfolio("dated", "USD")
	portfolio.SetAggregate(domain.AggregateUpdate{AsOf: baseTime, BaseCurrency: "USD"})
	for id, value := range map[string]int64{"A": 1000, "MISSING": -500} {
		portfolio.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: value}}})
	}
	measure := varmodel.Historical(varmodel.Config{})(ctx, portfolio, returns.NewStoreReturnsProvider(prices, returns.ReturnsConfig{}))
	set := domain.NewMeasureSet("dated", baseTime, map[v1.MeasureName]v1.Measure{compute.MeasureVaR99: measure})
	if err := publisher.EmitMeasures(ctx, set, nil); err != nil {
		t.Fatal(err)
	}
	message, err := measureConsumer.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := bus.Unframe(message.Data())
	if err != nil {
		t.Fatal(err)
	}
	var record domainpb.RiskMeasureSet
	if err := proto.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Measures) != 1 {
		t.Fatal("missing measure")
	}
	provenance := record.Measures[0].GetProvenance()
	if provenance.GetInputDigest() == "" || provenance.GetInputDigest() != measure.Provenance.InputDigest || dec.Float64Or(provenance.GetExcludedGross(), -1) != 500 || provenance.GetParams()["panel_digest"] != "" {
		t.Fatalf("typed provenance was lost: %v", provenance)
	}
	if err := message.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}
