package publish_test

import (
	"context"
	"fmt"
	"github.com/eighred/kanz/internal/risk/factormodel"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketdata/returns"
	"github.com/eighred/kanz/internal/marketdata/store"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/compute"
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

func TestDescriptorCoverageThroughPostgresAndJetStream(t *testing.T) {
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
	bustest.EnsureSubjects(t, ctx, js, "DESCRIPTOR_FACTOR_"+suffix, []string{publish.EventTypeFactorModelFitted})
	stream, err := bustest.StreamFor(ctx, js, publish.EventTypeFactorModelFitted)
	if err != nil {
		t.Fatal(err)
	}
	name := "descriptor-" + suffix
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
	model := descriptorPostgresModel(t, ctx)
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
	portfolio := domain.NewPortfolio("dated", "USD")
	portfolio.SetAggregate(domain.AggregateUpdate{AsOf: baseTime, BaseCurrency: "USD"})
	for id, value := range map[string]int64{"A": 1000, "MISSING": -500} {
		portfolio.SetPosition(domain.Position{InstrumentID: domain.InstrumentID(id), MarketValue: &commonpb.Money{CurrencyCode: "USD", Amount: &commonpb.Decimal{Coefficient: value}}})
	}
	registry := compute.DefaultRegistry()
	compute.RegisterFactorRisk(ctx, registry, compute.FactorProviders{Model: descriptorModelProvider{model}})
	measures := compute.ComputeMeasures(portfolio, registry, nil)
	measure, ok := measures.Lookup(compute.MeasureFactorVaR99)
	if !ok {
		t.Fatal("factor measure missing")
	}
	if measure.Coverage.ExcludedCount != 1 {
		t.Fatalf("missing descriptor lost exposure coverage: %+v", measure.Coverage)
	}
	set := domain.NewMeasureSet("dated", baseTime, map[v1.MeasureName]v1.Measure{compute.MeasureFactorVaR99: measure})
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
	if provenance.GetInputDigest() == "" || provenance.GetInputDigest() != measure.Provenance.InputDigest || dec.Float64Or(provenance.GetExcludedGross(), -1) != 500 || provenance.GetParams()["descriptor_exclusions"] != "" || provenance.GetParams()["descriptor_digest"] != "" {
		t.Fatalf("typed provenance was lost: %v", provenance)
	}
	if err := message.DoubleAck(ctx); err != nil {
		t.Fatal(err)
	}
}

type descriptorModelProvider struct{ model *factormodel.Model }

func (p descriptorModelProvider) Model(context.Context, time.Time) (*factormodel.Model, bool) {
	return p.model, true
}

func descriptorPostgresModel(t *testing.T, ctx context.Context) *factormodel.Model {
	t.Helper()
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL required")
	}
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var super, bypass bool
	if err := admin.QueryRow(ctx, "SELECT rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&super, &bypass); err != nil || super || bypass {
		t.Fatalf("ordinary PG role required: %v", err)
	}
	schema := pgx.Identifier{fmt.Sprintf("descriptor_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migration, err := os.ReadFile(filepath.Join("..", "..", "..", "services", "market-data", "migrations", "0001_price_history.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	st := store.NewPostgres(db)
	var observations []store.Observation
	for j, id := range []string{"A", "B", "C", "MISSING"} {
		for d := 0; d <= 20; d++ {
			if id == "MISSING" && d < 18 {
				continue
			}
			at := baseTime.Add(time.Duration(d-20) * 24 * time.Hour)
			observations = append(observations, store.Observation{InstrumentID: id, ObservationTime: at, KnowledgeTime: at, Kind: store.PriceKindClose, CurrencyCode: "USD", Price: &commonpb.Decimal{Coefficient: int64(10000 + d*(j+1)*10 + d*d*(j+1))}})
		}
	}
	if err := st.Put(ctx, observations); err != nil {
		t.Fatal(err)
	}
	rp := returns.NewStoreReturnsProvider(st, returns.ReturnsConfig{})
	chars := compute.NewStoreCharacteristicProvider(rp, nil, compute.CharacteristicConfig{Window: 15, MomentumSkip: 3})
	config := factormodel.Config{StyleFactors: []string{compute.StyleMomentum}, Window: 15}
	fit := func(at time.Time) *factormodel.Model {
		m, err := factormodel.Fit(ctx, config, []string{"A", "B", "C", "MISSING"}, at, factormodel.Providers{Returns: rp, Characteristics: chars})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	model := fit(baseTime)
	if len(model.Instruments) != 3 || !strings.Contains(model.InputProvenance["descriptor_exclusions"], "missing_style:Momentum") {
		t.Fatalf("missing descriptor treated as zero: %+v", model.InputProvenance)
	}
	futureBefore := fit(baseTime.Add(24 * time.Hour))
	correction := observations[10]
	correction.KnowledgeTime = baseTime.Add(24 * time.Hour)
	correction.Price = &commonpb.Decimal{Coefficient: 20000}
	if err := st.Put(ctx, []store.Observation{correction}); err != nil {
		t.Fatal(err)
	}
	if before := fit(baseTime); before.InputProvenance["input_digest"] != model.InputProvenance["input_digest"] {
		t.Fatal("future revision leaked into descriptors")
	}
	if after := fit(baseTime.Add(24 * time.Hour)); after.InputProvenance["input_digest"] == futureBefore.InputProvenance["input_digest"] {
		t.Fatal("descriptor revision identity unchanged")
	}
	return model
}
