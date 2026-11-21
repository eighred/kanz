package publish_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/filing"
	"github.com/eighred/kanz/internal/outbox"
	regulatorypublish "github.com/eighred/kanz/internal/regulatory/publish"
	"github.com/eighred/kanz/internal/sustainability"
	sustainabilitypublish "github.com/eighred/kanz/internal/sustainability/publish"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	sustainabilitypb "github.com/eighred/kanz/kanz-schemas-go/sustainability/v1"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/google/uuid"
	ns "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

func publishBroker(t *testing.T) (context.Context, *regulatorypublish.Publisher, jetstream.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	server, err := ns.NewServer(&ns.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true, JetStream: true, StoreDir: t.TempDir(), MaxPayload: 1 << 20, MaxConn: 8, JetStreamMaxMemory: 4 << 20, JetStreamMaxStore: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(func() { server.Shutdown(); server.WaitForShutdown() })
	if !server.ReadyForConnections(3 * time.Second) {
		t.Fatal("owned loopback broker did not become ready")
	}
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "SUSTAINABILITY_PUBLISH", Subjects: []string{sustainabilitypublish.ObservedSubject, sustainabilitypublish.AggregatedSubject}, Storage: jetstream.MemoryStorage, MaxBytes: 2 << 20, MaxMsgs: 32, Duplicates: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.DialNATS(ctx, bus.NATSConfig{URL: server.ClientURL(), Name: "sustainability-publish-test", DisableReconnect: true, PublishTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error("publish client cleanup failed")
		}
	})
	producer, err := bus.NewProducer(client, bus.ProducerConfig{Source: "regulatory", ProducerVersion: "synthetic-test", Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := regulatorypublish.New(producer, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, publisher, stream
}

func syntheticInput(t *testing.T) (filing.PublicationScope, []sustainabilitypublish.Input) {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	scope := filing.PublicationScope{Tenant: "acme", Portfolio: "pf1", ReportID: id.String(), InputDigest: strings.Repeat("a", 64), AsOf: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), RecordedAt: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	money := func(text string) *commonpb.Decimal {
		d, ok := dec.ParseProtoExact(text)
		if !ok {
			t.Fatal("invalid synthetic decimal")
		}
		return d
	}
	inputs := []sustainabilitypublish.Input{{Holding: sustainability.Holding{InstrumentID: "synthetic-issuer", MarketValue: 100, ESG: sustainability.ESGScore{Overall: 70, Environmental: 60, Social: 80, Governance: 90}, Carbon: sustainability.CarbonMetrics{Scope1: 1, Scope2: 1, Scope3: 2, Revenue: 10, EVIC: 50}}, MarketValue: money("100"), Revenue: money("10"), Enterprise: money("50"), ESGPresent: true}}
	return scope, inputs
}

func TestPublishSustainabilityFACTsReachTheBroker(t *testing.T) {
	ctx, producer, stream := publishBroker(t)
	scope, inputs := syntheticInput(t)
	observations, aggregate, err := sustainabilitypublish.Build(scope, inputs, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || aggregate.Esg == nil || aggregate.Esg.Overall != 70 || aggregate.WeightedAverageCarbonIntensity != 0.2 || aggregate.FinancedEmissions != 8 || aggregate.IntensityCoverage.Fraction != 1 || aggregate.AttributionCoverage.Fraction != 1 {
		t.Fatal("synthetic independent ESG/WACI/PCAF results changed")
	}
	binding := strings.Repeat("b", 64)
	payloads := []proto.Message{&sustainabilitypb.InstrumentObserved{Observation: observations[0], ContextBindingHash: binding}, &sustainabilitypb.InstrumentObserved{Observation: observations[1], ContextBindingHash: binding}, &sustainabilitypb.PortfolioAggregated{Observation: aggregate, ContextBindingHash: binding}}
	ids := map[string]bool{scope.ReportID: true}
	send := func(payload proto.Message) error {
		event, err := sustainabilitypublish.Event(payload)
		if err != nil {
			return err
		}
		record, err := outbox.From(ctx, event)
		if err != nil {
			return err
		}
		stored, err := record.Event()
		if err != nil {
			return err
		}
		return producer.Publish(ctx, stored)
	}
	for i, payload := range payloads {
		if err := send(payload); err != nil {
			t.Fatal(err)
		}
		message, err := stream.GetMsg(ctx, uint64(i+1))
		if err != nil {
			t.Fatalf("actual publication %d missing: %v", i, err)
		}
		envelope, raw, err := bus.Unframe(message.Data)
		if err != nil || bus.Validate(envelope) != nil {
			t.Fatal("actual FACT envelope invalid")
		}
		if ids[envelope.EventId] || envelope.TenantId != "acme" || envelope.PartitionKey != "acme/pf1" || envelope.CorrelationId != scope.ReportID || !envelope.EventTime.AsTime().Equal(scope.RecordedAt) {
			t.Fatal("actual FACT lost distinct identity/scope/record time")
		}
		ids[envelope.EventId] = true
		observed := payload.ProtoReflect().New().Interface()
		if proto.Unmarshal(raw, observed) != nil || !proto.Equal(payload, observed) {
			t.Fatal("actual broker payload differs from computed observation")
		}
		// Broker dedup sees the same persisted domain identity on retry.
		if err := send(payload); err != nil {
			t.Fatal(err)
		}
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 3 {
		t.Fatalf("retry minted duplicate FACT identities: count=%d", info.State.Msgs)
	}
}

func TestPublishSustainabilityRetainsMissingCoverageAndRefusesInvalidInput(t *testing.T) {
	scope, inputs := syntheticInput(t)
	unknown := inputs[0]
	unknown.Holding.InstrumentID = "unmeasured"
	unknown.Holding.Carbon.Revenue = 0
	unknown.Holding.Carbon.EVIC = 0
	unknown.Revenue = nil
	unknown.Enterprise = nil
	unknown.ESGPresent = false
	observations, aggregate, err := sustainabilitypublish.Build(scope, append(inputs, unknown), func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
	if err != nil || len(observations) != 3 || aggregate.Esg != nil || aggregate.EsgObservedHoldings != 1 || aggregate.IntensityCoverage.Fraction != 0.5 || aggregate.AttributionCoverage.Fraction != 0.5 || aggregate.AttributionCoverage.UncoveredCount != 1 {
		t.Fatalf("unknowns became a healthy default: %v", err)
	}
	for _, mutation := range []string{"duplicate", "source-money", "nonfinite", "reused-domain-id", "inventory-limit"} {
		t.Run(mutation, func(t *testing.T) {
			_, in := syntheticInput(t)
			next := func() (string, error) { id, err := uuid.NewV7(); return id.String(), err }
			switch mutation {
			case "duplicate":
				in = append(in, in[0])
			case "source-money":
				in[0].Holding.MarketValue = 101
			case "nonfinite":
				in[0].Holding.Carbon.Scope1 = math.NaN()
			case "reused-domain-id":
				next = func() (string, error) { return scope.ReportID, nil }
			case "inventory-limit":
				in = make([]sustainabilitypublish.Input, sustainabilitypublish.MaxHoldings+1)
			}
			if _, _, err := sustainabilitypublish.Build(scope, in, next); err == nil {
				t.Fatal("invalid publication input accepted")
			}
		})
	}
}
