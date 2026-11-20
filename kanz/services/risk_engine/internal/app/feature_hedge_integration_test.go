package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/prediction"
	"github.com/eighred/kanz/internal/risk"
	"github.com/eighred/kanz/internal/risk/domain"
	"github.com/eighred/kanz/internal/risk/engine"
	"github.com/eighred/kanz/internal/risk/publish"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	inferencepb "github.com/eighred/kanz/kanz-schemas-go/inference/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/risk_engine/internal/app"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRecomputedHedgeFeatureOnJetStream(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL required for persisted inference features")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id := "hedge-" + uuid.NewString()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := bustest.StreamFor(ctx, js, prediction.EventTypeFeatureComputed)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: id, FilterSubject: prediction.EventTypeFeatureComputed, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteConsumer(ctx, stream, id) }()
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: url, Name: id})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "risk-engine", ProducerVersion: "test", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	measures, err := publish.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}
	features, err := prediction.NewPublisher(producer)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	book, reg := hedgedFeatureBook(t, id, at)
	st := state.NewStore()
	observe := app.PublishFeaturesOn(features, discardLogger())
	done := make(chan struct{}, 1)
	worker := engine.NewRecomputer(ctx, st, reg, risk.NewCache(), measures, time.Millisecond, discardLogger(), engine.WithMeasureObserver(func(ctx context.Context, ms *domain.MeasureSet) {
		observe(ctx, ms)
		select {
		case done <- struct{}{}:
		case <-ctx.Done():
		}
	}))
	defer worker.Close()
	// A final measured zero marks the end of the two unpublished snapshots.
	cent := &commonpb.Decimal{Coefficient: 2, Exponent: -2}
	for i, input := range []struct {
		currency string
		mark     *commonpb.Decimal
	}{{"USD", cent}, {"USD", nil}, {"", cent}, {"USD", &commonpb.Decimal{}}} {
		ts := timestamppb.New(at.Add(time.Duration(i) * time.Second))
		snapshot := &domainpb.PortfolioSnapshot{Portfolio: &domainpb.PortfolioState{PortfolioId: id, BaseCurrency: input.currency, AsOf: ts}}
		for _, pos := range book.Positions() {
			value := &commonpb.Money{CurrencyCode: input.currency, Amount: pos.MarketValue.Amount}
			if pos.InstrumentID == "B" {
				value.Amount = input.mark
			}
			snapshot.Positions = append(snapshot.Positions, &domainpb.PositionState{PortfolioId: id, InstrumentId: string(pos.InstrumentID), MarketValue: value, AsOf: ts})
		}
		if err := st.ApplyPortfolioSnapshot(ctx, &envelopepb.Envelope{IdempotencyKey: uuid.NewString()}, snapshot); err != nil {
			t.Fatal(err)
		}
		worker.Trigger(book.ID())
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for _, expected := range []struct {
		offset time.Duration
		loss   float64
	}{{0, .01}, {3 * time.Second, 0}} {
		for {
			msg, err := sub.Next(jetstream.FetchContext(ctx))
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
			var fv inferencepb.FeatureVector
			if err := proto.Unmarshal(payload, &fv); err != nil {
				t.Fatal(err)
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			if fv.SubjectId != id {
				continue
			}
			want := expectedHedgeFeature(id, at.Add(expected.offset), expected.loss)
			if env.GetSource() != "risk-engine" || env.GetTenantId() != "acme" || !proto.Equal(&fv, want) {
				t.Fatalf("incorrect persisted hedge feature: %v", &fv)
			}
			break
		}
	}
}
