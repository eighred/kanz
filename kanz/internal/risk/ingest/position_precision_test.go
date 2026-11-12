package ingest_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/bustest"
	"github.com/eighred/kanz/internal/platform/subject"
	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/ingest"
	"github.com/eighred/kanz/internal/risk/state"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	envelopepb "github.com/eighred/kanz/kanz-schemas-go/envelope/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The OMS SQL/broker proof lives in its own module. This separate wire-contract
// test exercises the production risk handler/fold without an OMS import of risk
// internals or a new production API solely for testing.
func TestPositionPrecisionFACTOverJetStream(t *testing.T) {
	config := bus.ProducerConfig{Source: "oms/precision-fixture", ProducerVersion: "0.0.0-test"}
	// NewProducer validates metadata without touching its client. Check this
	// before dependency skipping, so a missing fixture version fails locally too.
	if _, err := bus.NewProducer(new(bus.NATSClient), config); err != nil {
		t.Fatal(err)
	}
	address := os.Getenv("TEST_NATS_URL")
	if address == "" {
		t.Skip("TEST_NATS_URL unavailable: actual risk precision broker required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nc, err := nats.Connect(address, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: address, Name: "risk-precision-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	producer, err := bus.NewProducer(client, config)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sign int64
	}{{"long", 1}, {"short", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			portfolio := "precision-" + uuid.NewString()
			key := subject.PositionFor("precision", portfolio, "instrument")
			stream, err := bustest.StreamFor(ctx, js, key)
			if err != nil {
				t.Fatal("canonical position stream unavailable", err)
			}
			name := "risk-" + uuid.NewString()
			consumer, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{Name: name, FilterSubject: key, DeliverPolicy: jetstream.DeliverNewPolicy, AckPolicy: jetstream.AckExplicitPolicy})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := js.DeleteConsumer(ctx, stream, name); err != nil {
					t.Error(err)
				}
			}()
			money := func(c int64) *commonpb.Money {
				return &commonpb.Money{Amount: &commonpb.Decimal{Coefficient: c, Exponent: -18}, CurrencyCode: "USD"}
			}
			position := &domainpb.PositionState{PortfolioId: portfolio, InstrumentId: "instrument", Quantity: &commonpb.Decimal{Coefficient: tc.sign, Exponent: -9}, MarketValue: money(tc.sign), RealizedPnl: money(3), UnrealizedPnl: money(-2), AsOf: timestamppb.Now()}
			if err := producer.Publish(bus.WithTenantID(ctx, "precision"), bus.Event{Subject: key, EventType: ingest.EventTypePositionChanged, EventClass: envelopepb.EventClass_EVENT_CLASS_FACT, SchemaVersion: 1, Domain: "risk", EventTime: position.AsOf.AsTime(), PartitionKey: key, PayloadSchemaRef: "domain.v1.PositionState:1", Payload: position, CorrelationID: uuid.Must(uuid.NewV7()).String(), CausationID: uuid.Must(uuid.NewV7()).String(), TraceContext: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}); err != nil {
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
			store := state.NewStore()
			handler, err := ingest.NewIngestor(store)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := handler.Handler(ctx, env, payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := msg.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			view, ok := store.Snapshot(v1.PortfolioID(portfolio))
			if !ok || len(view.Positions()) != 1 {
				t.Fatal("risk lost or duplicated the holding")
			}
			got := view.Positions()[0]
			if !proto.Equal(got.Quantity, position.Quantity) || !proto.Equal(got.MarketValue, position.MarketValue) || !proto.Equal(got.RealizedPnL, position.RealizedPnl) || !proto.Equal(got.UnrealizedPnL, position.UnrealizedPnl) {
				t.Fatal("actual risk handler/fold changed precise FACT values")
			}
		})
	}
}
